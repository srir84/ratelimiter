package storeproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Profile is a store deployment profile (DESIGN §A2): one deploy/valkey-<profile>.conf.
type Profile int

const (
	// Fast is appendfsync no: logic tests only. It is the zero value.
	Fast Profile = iota
	// Durable is appendfsync always: production, benchmarks and durability tests.
	Durable
)

// String returns the profile's name, as in deploy/valkey-<name>.conf.
func (p Profile) String() string {
	switch p {
	case Fast:
		return "fast"
	case Durable:
		return "durable"
	}
	return "Profile(" + strconv.Itoa(int(p)) + ")"
}

// Options selects how Start runs a store.
type Options struct {
	Profile Profile
	// Dir holds the AOF (appendonlydir/), acl.conf, store.log, store.pid and store.out.
	// Default: t.TempDir().
	Dir string
}

const (
	startReady       = 10 * time.Second // Start waits this long for loading:0
	restartReady     = 30 * time.Second // Restart(true) waits this long for loading:0
	restartListening = 10 * time.Second // Restart(false) waits this long for the port
	stopGrace        = 10 * time.Second // SIGTERM, then this long, then SIGKILL
	killWait         = 10 * time.Second // the longest wait for an exit after SIGKILL
	listenPoll       = 2 * time.Millisecond
	readyPoll        = 10 * time.Millisecond
	tailLines        = 50 // store.log lines logged when a test fails
	startAttempts    = 3  // fresh ports tried when another process takes the picked one
)

// spawnScript sets the file limit to 8,192, then execs the store ("$0" "$@"). Go gives a
// child the original soft RLIMIT_NOFILE (256 by default on macOS), so without the wrapper
// the store's limit depends on the caller. /bin/sh's ulimit -n sets the hard limit as
// well as the soft one, and the hard limit is what the code relies on: maxclients can then
// go up to 8,160 (8,192 less Valkey's 32 reserved descriptors) and no further, which
// TestC2_DurableProfile checks (CONFIG SET maxclients 8000 accepted, 8161 refused with
// "try with 8160"). DESIGN §B1 expects Valkey to lower maxclients when its limit is too
// low; on macOS, without the wrapper, it was seen instead to raise its own limit to 4,128
// when the hard limit allowed and keep maxclients 4,096.
const spawnScript = `ulimit -n 8192 && exec "$0" "$@"`

// errPortTaken marks a start that failed because the picked port was taken meanwhile.
var errPortTaken = errors.New("port taken")

// Store is a store process a test owns. Start registers Stop as a cleanup. Conn, Env and
// RecordPath are safe from any goroutine; the other methods are for one goroutine at a
// time, normally the test's.
type Store struct {
	Addr    string // 127.0.0.1:<Port>
	Port    int
	Dir     string
	Bin     string
	Product string // from --version, e.g. "Valkey"
	Version string // from --version, e.g. "9.1.2"
	PW      Passwords
	PID     int // the running process; Restart changes it
	// Args is the exact command line Start ran (the /bin/sh file-limit wrapper, then the
	// binary and its arguments), so a restart keeps the port, directory and ACL file.
	Args []string

	tb     testing.TB
	root   string
	conf   string
	mu     sync.Mutex
	proc   *proc // nil once stopped
	paused bool  // Pause stopped proc with SIGSTOP, and no Resume or halt continued it
	tailed bool
}

// proc is one child process; reap closes done once Wait returned and the record is gone.
type proc struct {
	cmd     *exec.Cmd
	pid     int
	done    chan struct{}
	waitErr error
	recErr  error
}

// Start starts a store with o's profile on a free loopback port, with a freshly rendered
// ACL file in o.Dir, and waits until it has loaded its data (WaitReady, 10 s). It
// registers Stop with t.Cleanup.
func Start(t testing.TB, o Options) *Store {
	t.Helper()
	s, err := newStore(t, o)
	if err != nil {
		t.Fatalf("storeproc: %v", err)
	}
	t.Cleanup(s.Stop)
	for attempt := 1; ; attempt++ {
		err := s.launch()
		if err == nil {
			return s
		}
		if attempt < startAttempts && errors.Is(err, errPortTaken) {
			if err := s.pickPort(); err != nil {
				t.Fatalf("storeproc: %v", err)
			}
			continue
		}
		t.Fatalf("storeproc: starting the %s store: %v", o.Profile, err)
	}
}

func newStore(t testing.TB, o Options) (*Store, error) {
	root, err := ModuleRoot()
	if err != nil {
		return nil, err
	}
	conf := filepath.Join(root, "deploy", "valkey-"+o.Profile.String()+".conf")
	if _, err := os.Stat(conf); err != nil {
		return nil, fmt.Errorf("profile %s: %w", o.Profile, err)
	}
	bin, err := Bin()
	if err != nil {
		return nil, err
	}
	product, version, err := Version(t.Context(), bin)
	if err != nil {
		return nil, err
	}
	dir := o.Dir
	if dir == "" {
		dir = t.TempDir()
	} else {
		if dir, err = filepath.Abs(dir); err != nil {
			return nil, fmt.Errorf("store directory: %w", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store directory: %w", err)
		}
	}
	pw := NewPasswords()
	if err := RenderACL(filepath.Join(root, "deploy", "acl.conf.tmpl"), filepath.Join(dir, "acl.conf"), pw); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, Bin: bin, Product: product, Version: version, PW: pw, tb: t, root: root, conf: conf}
	if err := s.pickPort(); err != nil {
		return nil, err
	}
	return s, nil
}

// pickPort picks a free loopback port and builds the command line for it.
func (s *Store) pickPort() error {
	var lc net.ListenConfig
	l, err := lc.Listen(s.ctx(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("picking a free port: %w", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if err := l.Close(); err != nil {
		return fmt.Errorf("picking a free port: %w", err)
	}
	if !ok {
		return fmt.Errorf("picking a free port: unexpected address %v", l.Addr())
	}
	s.Port = addr.Port
	s.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port))
	s.Args = []string{
		"/bin/sh", "-c", spawnScript, s.Bin, s.conf,
		"--port", strconv.Itoa(s.Port),
		"--dir", s.Dir,
		"--aclfile", filepath.Join(s.Dir, "acl.conf"),
		"--logfile", filepath.Join(s.Dir, "store.log"),
		"--pidfile", filepath.Join(s.Dir, "store.pid"),
	}
	return nil
}

// launch spawns the store and waits until it is ready; on failure the process is gone.
func (s *Store) launch() error {
	logPath := filepath.Join(s.Dir, "store.log")
	var offset int64
	if info, err := os.Stat(logPath); err == nil {
		offset = info.Size()
	}
	if err := s.spawn(); err != nil {
		return err
	}
	err := s.WaitReady(startReady)
	if err == nil {
		return nil
	}
	if herr := s.halt(syscall.SIGKILL, killWait); herr != nil {
		return errors.Join(err, herr)
	}
	if b, rerr := os.ReadFile(logPath); rerr == nil && int64(len(b)) >= offset {
		for _, line := range strings.Split(string(b[offset:]), "\n") {
			if strings.Contains(line, "Address already in use") && strings.Contains(line, ":"+strconv.Itoa(s.Port)) {
				return fmt.Errorf("%w: %w", errPortTaken, err)
			}
		}
	}
	return err
}

// spawn starts the process from Args, records it in .out/procs/<pid> and starts its reaper.
func (s *Store) spawn() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != nil {
		return errors.New("the store is already running")
	}
	out, err := os.OpenFile(filepath.Join(s.Dir, "store.out"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("store output file: %w", err)
	}
	// The store must outlive the test's context until Stop ends it (AGENTS.md §2).
	cmd := exec.CommandContext(s.ctx(), s.Args[0], s.Args[1:]...)
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Start()
	_ = out.Close() // the child holds its own descriptor
	if err != nil {
		return fmt.Errorf("starting %s: %w", s.Bin, err)
	}
	p := &proc{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{})}
	if err := s.writeRecord(p.pid); err != nil {
		_ = cmd.Process.Kill() // the record failed, so the process must not outlive this call
		_ = cmd.Wait()
		// A failed write can leave the file it created, empty or partial: remove it too.
		return errors.Join(err, s.removeRecord(p.pid))
	}
	go s.reap(p)
	s.proc, s.PID = p, p.pid
	return nil
}

// reap waits for the process, then deletes its PID record: on every path (Stop, Kill,
// Restart, or a store that exited by itself) the record goes once Wait returns.
func (s *Store) reap(p *proc) {
	p.waitErr = p.cmd.Wait()
	p.recErr = s.removeRecord(p.pid)
	close(p.done)
}

// RecordPath is the PID record of a process this package started: <module root>/.out/procs/<pid>
// holding its command line, which bin/trace procs reads (never process names: Valkey
// rewrites its title, and the owner's own stores must not match).
func (s *Store) RecordPath(pid int) string {
	return filepath.Join(s.root, ".out", "procs", strconv.Itoa(pid))
}

func (s *Store) writeRecord(pid int) error {
	path := s.RecordPath(pid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("process record: %w", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(s.Args, " ")+"\n"), 0o644); err != nil {
		return fmt.Errorf("process record: %w", err)
	}
	return nil
}

func (s *Store) removeRecord(pid int) error {
	if err := os.Remove(s.RecordPath(pid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the process record: %w", err)
	}
	return nil
}

// halt sends sig to the running process, waits up to grace for it to exit, then
// SIGKILLs it. It returns once the process has exited and its record is gone. A process
// that Pause stopped gets SIGCONT right after sig, which a stopped process holds until it
// continues, so it exits within grace as a running one would.
func (s *Store) halt(sig syscall.Signal, grace time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.proc
	if p == nil {
		return nil
	}
	s.proc = nil
	if err := p.signal(sig); err != nil {
		return err
	}
	if s.paused {
		s.paused = false
		if err := p.signal(syscall.SIGCONT); err != nil {
			return err
		}
	}
	if !p.exitedWithin(grace) {
		if err := p.signal(syscall.SIGKILL); err != nil {
			return err
		}
		if !p.exitedWithin(killWait) {
			return fmt.Errorf("store process %d still running %v after SIGKILL", p.pid, killWait)
		}
	}
	return p.recErr
}

func (p *proc) signal(sig syscall.Signal) error {
	if err := p.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("sending %v to store process %d: %w", sig, p.pid, err)
	}
	return nil
}

func (p *proc) exitedWithin(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	}
}

// Stop ends the store gracefully: SIGTERM, up to 10 s, then SIGKILL. It does nothing
// when the store is not running. When the test has failed, it logs the last 50 lines
// of store.log (once). A panicking test counts as failed: the testing package runs its
// cleanups while the panic unwinds and marks it failed only afterwards.
func (s *Store) Stop() {
	s.tb.Helper()
	if err := s.halt(syscall.SIGTERM, stopGrace); err != nil {
		s.tb.Errorf("storeproc: stopping the store: %v", err)
	}
	if (s.tb.Failed() || panicking()) && !s.tailed {
		s.tailed = true
		s.logTail()
	}
}

// panicking reports whether a panic is unwinding through its caller: the runtime runs a
// panicking goroutine's deferred calls, and so the testing package's cleanups, from
// runtime.gopanic. A recovered panic stops counting once the call that recovered it has
// returned, and t.FailNow's runtime.Goexit never counts (it marks the test failed first).
func panicking() bool {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	for n == len(pcs) { // the stack may go deeper: read all of it
		pcs = make([]uintptr, 2*len(pcs))
		n = runtime.Callers(2, pcs)
	}
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function == "runtime.gopanic" {
			return true
		}
		if !more {
			return false
		}
	}
}

// Kill ends the store with SIGKILL and waits until it has exited.
func (s *Store) Kill() {
	s.tb.Helper()
	if err := s.halt(syscall.SIGKILL, killWait); err != nil {
		s.tb.Errorf("storeproc: killing the store: %v", err)
	}
}

// Restart stops the store if it still runs (as Stop does), then starts it again with the
// same command line: same port, directory, data and ACL file. With waitReady it waits
// for loading:0 (30 s); without, only until the port accepts connections (10 s), so a
// caller can watch the store load.
func (s *Store) Restart(waitReady bool) error {
	if err := s.halt(syscall.SIGTERM, stopGrace); err != nil {
		return fmt.Errorf("restarting the store: %w", err)
	}
	if err := s.spawn(); err != nil {
		return fmt.Errorf("restarting the store: %w", err)
	}
	if waitReady {
		return s.WaitReady(restartReady)
	}
	return s.WaitListening(restartListening)
}

// running returns an error unless the store process is running.
func (s *Store) running() error {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p == nil {
		return errors.New("the store is not running")
	}
	select {
	case <-p.done:
		if p.waitErr != nil {
			return fmt.Errorf("store process %d exited: %w", p.pid, p.waitErr)
		}
		return fmt.Errorf("store process %d exited with status 0", p.pid)
	default:
		return nil
	}
}

// WaitListening waits up to d until a TCP connection to the store succeeds.
func (s *Store) WaitListening(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if err := s.running(); err != nil {
			return err
		}
		ctx, cancel := context.WithDeadline(s.ctx(), deadline)
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", s.Addr)
		cancel()
		if err == nil {
			_ = conn.Close() // only the connect mattered
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("store %s not listening within %v: %w", s.Addr, d, err)
		}
		time.Sleep(listenPoll)
	}
}

// WaitReady waits up to d until INFO persistence, read as rl_ops, shows loading:0. The
// store accepts connections while it still loads its data (DESIGN §A3.0 row 16).
func (s *Store) WaitReady(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if err := s.running(); err != nil {
			return err
		}
		err := s.loaded(deadline)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("store %s not ready within %v: %w", s.Addr, d, err)
		}
		time.Sleep(readyPoll)
	}
}

// loaded returns nil when INFO persistence shows loading:0; no I/O runs past deadline.
func (s *Store) loaded(deadline time.Time) error {
	c, err := dial(s.Addr, "rl_ops", s.PW.Ops, deadline)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }() // a read-only probe: nothing to flush
	c.limit = deadline
	rep, err := c.Do("INFO", "persistence")
	if err != nil {
		return err
	}
	if rep.Kind != '$' || rep.Nil {
		return fmt.Errorf("INFO persistence: unexpected reply %s", rep)
	}
	switch v, ok := ParseInfo(rep.Str)["loading"]; {
	case !ok:
		return errors.New("INFO persistence has no loading field")
	case v != "0":
		return fmt.Errorf("still loading (loading:%s)", v)
	}
	return nil
}

// Conn opens a connection authenticated as an ACL user of DESIGN §A2: rl_lib, rl_admin,
// rl_fixture or rl_ops. The caller closes it.
func (s *Store) Conn(user string) (*Client, error) {
	var pw string
	switch user {
	case "rl_lib":
		pw = s.PW.Lib
	case "rl_admin":
		pw = s.PW.Admin
	case "rl_fixture":
		pw = s.PW.Fixture
	case "rl_ops":
		pw = s.PW.Ops
	default:
		return nil, fmt.Errorf("unknown ACL user %q: want rl_lib, rl_admin, rl_fixture or rl_ops", user)
	}
	return Dial(s.Addr, user, pw)
}

// ctx is the test's context without its cancellation: Stop and later slices' audit hooks
// run in t.Cleanup, after the test's context is done, and still need the store.
func (s *Store) ctx() context.Context {
	return context.WithoutCancel(s.tb.Context())
}

// logTail logs the last lines of store.log and of the process output.
func (s *Store) logTail() {
	for _, name := range [...]string{"store.log", "store.out"} {
		path := filepath.Join(s.Dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			s.tb.Logf("storeproc: reading %s: %v", path, err)
			continue
		}
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > tailLines {
			lines = lines[len(lines)-tailLines:]
		}
		s.tb.Logf("storeproc: last %d lines of %s:\n%s", len(lines), path, strings.Join(lines, "\n"))
	}
}
