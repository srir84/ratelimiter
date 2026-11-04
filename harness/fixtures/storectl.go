//go:build rltest

package fixtures

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/srir84/ratelimiter/internal/store/resp"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// Store fault timing (DESIGN §B6; IMPLEMENTATION_PLAN H4 step 1).
const (
	// rewritePoll is the interval between two reads of INFO persistence while the rewrite
	// before SHUTDOWN runs (plan H4 step 1).
	rewritePoll = 50 * time.Millisecond
	// rewriteWait bounds that rewrite, BGREWRITEAOF and the polls (plan choice, as the
	// orchestrator's forced rewrites).
	rewriteWait = 5 * time.Minute
	// opsWait bounds one ops connection that sends a few quick commands: the dial, AUTH and
	// the commands (plan choice).
	opsWait = 10 * time.Second
	// shutdownWait bounds SHUTDOWN once it is sent: its reply (none, the connection closes)
	// and the port refusing connections afterwards (plan choice).
	shutdownWait = 10 * time.Second
	// portPoll is the interval between two connection attempts while SHUTDOWN closes the port.
	portPoll = 10 * time.Millisecond
	// pingEvery and pingTimeout pace the probes that end an outage: a fresh connection, AUTH
	// as the ops user and PING, one every 20 ms, each within 200 ms (plan H4 step 1).
	pingEvery   = 20 * time.Millisecond
	pingTimeout = 200 * time.Millisecond
	// backWait bounds bringing the store back once it is down or stopped: the restart, and
	// separately the probes until the first successful PING (plan choice).
	backWait = 30 * time.Second
	// maxPIDFile bounds a PID file read, in bytes.
	maxPIDFile = 64
	// rewriteInProgress is the store's error reply to BGREWRITEAOF while a rewrite runs
	// already: that rewrite is waited for instead.
	rewriteInProgress = "ERR Background append only file rewriting already in progress"
)

// StoreTarget is the store a store fault acts on (DESIGN §B6; IMPLEMENTATION_PLAN H4 step 1).
type StoreTarget struct {
	Addr        string // host:port
	OpsUser     string // the ops ACL user, rl_ops (DESIGN §A2)
	OpsPassword string // never in an error
	// Args is the exact command line that started the store. StoreDown runs it again when
	// Restart is nil, so the restarted store keeps its port, data directory and ACL file.
	Args []string
	// Env is the whole environment of a store started from Args (plan choice): nil or empty
	// gives it none, never the caller's, so no password reaches it that way.
	Env []string
	// OutPath, when not empty, is the file (created when missing, appended to) that takes the
	// output of a store started from Args (plan choice); empty discards it.
	OutPath string
	// PIDFile holds the store's PID as decimal text: StorePause stops that process, once INFO
	// server shows the store at Addr is it; StoreDown writes the restarted store's PID to it
	// (written to a temporary file and renamed over it, so a reader never sees half of it).
	PIDFile string
	// Restart, when not nil, starts the store again instead of Args (plan choice): its owner
	// keeps the process and its records, as a test passes its storeproc store's Restart. It
	// gets a context with its own deadline that the caller's ending does not cut short.
	Restart func(context.Context) error
}

// Outage is one store fault as measured (DESIGN §B6): from just before SHUTDOWN or SIGSTOP is
// sent to the first successful PING after the store is back, so AOF loading (LOADING
// replies) counts inside it.
type Outage struct {
	StartNs  int64 `json:"start_ns"`  // wall clock, just before SHUTDOWN or SIGSTOP was sent
	EndNs    int64 `json:"end_ns"`    // wall clock, when the first successful PING's reply was read
	OutageNs int64 `json:"outage_ns"` // from StartNs to EndNs on the monotonic clock
}

// newOutage measures the outage from start to end, both read by time.Now: their monotonic
// readings give the length, so a wall-clock step does not change it.
func newOutage(start, end time.Time) Outage {
	return Outage{StartNs: start.UnixNano(), EndNs: end.UnixNano(), OutageNs: end.Sub(start).Nanoseconds()}
}

// StoreDown takes the store down for d (FLT-1 (a), DESIGN §B6; plan H4 step 1): BGREWRITEAOF
// as the ops user, so the reload is short, then INFO persistence every 50 ms until
// aof_rewrite_in_progress:0 (and aof_rewrite_scheduled:0); SHUTDOWN, whose reply is the
// connection closing (EOF counts as success), and a wait until the port refuses connections;
// then d after SHUTDOWN, the restart (t.Restart, or t.Args with the new PID written to
// t.PIDFile at once); probes until the first successful PING; and the store's PID, read from
// INFO server, in t.PIDFile (checked against the PID already written after an Args restart).
//
// When ctx ends before SHUTDOWN is sent, nothing has changed. Once it is sent the store comes
// back whatever ctx does (AGENTS.md §2): ctx ending cuts the wait for d short, and the
// restart and the probes run on a context without ctx's cancellation, each within its own
// 30 s. StoreDown then returns the outage measured and an error wrapping ctx's. The Outage is
// zero whenever no end was measured.
func StoreDown(ctx context.Context, t StoreTarget, d time.Duration) (Outage, error) {
	if err := t.check(d, true); err != nil {
		return Outage{}, err
	}
	if err := rewriteAOF(ctx, t); err != nil {
		return Outage{}, err
	}
	start, err := shutdown(ctx, t)
	if err != nil {
		return Outage{}, err
	}
	back := context.WithoutCancel(ctx) // the store is down: it comes back whatever ctx does
	cut := cutShort(sleepUntil(ctx, start.Add(d)))
	pid, restartErr := restartStore(back, t)
	if restartErr != nil && pid == 0 { // not restarted (a store started from Args whose PID file failed is)
		return Outage{}, errors.Join(restartErr, cut)
	}
	end, err := waitPing(back, t)
	if err != nil {
		return Outage{}, errors.Join(restartErr, err, cut)
	}
	out := newOutage(start, end)
	if err := recordPID(back, t, pid); err != nil {
		return out, errors.Join(restartErr, err, cut)
	}
	return out, errors.Join(restartErr, cut)
}

// StorePause stops the store for d (FLT-2, a blackhole outage, DESIGN §B6; plan H4 step 1):
// SIGSTOP to the PID in t.PIDFile, once INFO server shows the store at t.Addr is that process
// (plan choice: a stale PID file never stops another process), then SIGCONT d later, then
// probes until the first successful PING. The kernel still completes TCP connects to a
// stopped store, but nothing is read or answered until SIGCONT (§A5.4).
//
// When ctx ends before SIGSTOP is sent, nothing has changed. Once it is sent the store always
// gets SIGCONT (AGENTS.md §2), a panic included: ctx ending cuts the wait for d short, and the
// probes run on a context without ctx's cancellation, within their own 30 s. StorePause then
// returns the outage measured and an error wrapping ctx's.
func StorePause(ctx context.Context, t StoreTarget, d time.Duration) (Outage, error) {
	if err := t.check(d, false); err != nil {
		return Outage{}, err
	}
	pid, err := readPIDFile(t.PIDFile)
	if err != nil {
		return Outage{}, fmt.Errorf("fixtures: store pause: %w", err)
	}
	running, err := storePID(ctx, t)
	if err != nil {
		return Outage{}, fmt.Errorf("fixtures: store pause: %w", err)
	}
	if running != pid {
		return Outage{}, fmt.Errorf("fixtures: store pause: %s names process %d, but the store at %s is process %d", t.PIDFile, pid, t.Addr, running)
	}
	if err := ctx.Err(); err != nil {
		return Outage{}, fmt.Errorf("fixtures: store pause: %w", err)
	}
	start := time.Now()
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		return Outage{}, fmt.Errorf("fixtures: store pause: SIGSTOP to process %d: %w", pid, err)
	}
	stopped := true
	defer func() {
		if stopped { // only on a panic, or after a SIGCONT that failed: try once more
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
	}()
	cut := cutShort(sleepUntil(ctx, start.Add(d)))
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		return Outage{}, errors.Join(fmt.Errorf("fixtures: store pause: SIGCONT to process %d: %w", pid, err), cut)
	}
	stopped = false
	end, err := waitPing(context.WithoutCancel(ctx), t)
	if err != nil {
		return Outage{}, errors.Join(err, cut)
	}
	return newOutage(start, end), cut
}

// check refuses a target or duration a fault cannot use; down adds StoreDown's restart.
func (t StoreTarget) check(d time.Duration, down bool) error {
	if _, _, err := net.SplitHostPort(t.Addr); err != nil {
		return fmt.Errorf("fixtures: store target address %q: %w", t.Addr, err)
	}
	switch {
	case t.OpsUser == "":
		return errors.New("fixtures: store target: no ops user")
	case t.PIDFile == "":
		return errors.New("fixtures: store target: no PID file")
	case d <= 0:
		return fmt.Errorf("fixtures: store fault duration %v: want more than 0", d)
	case down && t.Restart == nil && len(t.Args) == 0:
		return errors.New("fixtures: store target: no Restart and no command line to restart the store with")
	}
	return nil
}

// cutShort is StoreDown's and StorePause's error for a wait for d that ctx ended (err from
// sleepUntil), nil when it ran to its end.
func cutShort(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("fixtures: the store fault was cut short: %w", err)
}

// sleepUntil waits until at, or until ctx ends: then it returns ctx's error.
func sleepUntil(ctx context.Context, at time.Time) error {
	wait := time.Until(at)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// rewriteAOF rewrites the AOF before SHUTDOWN (§B6): BGREWRITEAOF, then INFO persistence at
// once and every rewritePoll until aof_rewrite_in_progress:0 and no rewrite is scheduled. A
// rewrite already running is waited for the same way. A rewrite that ends with an
// aof_last_bgrewrite_status other than ok fails: the reload would not be short.
func rewriteAOF(ctx context.Context, t StoreTarget) error {
	ctx, cancel := context.WithTimeout(ctx, rewriteWait)
	defer cancel()
	c, err := dialOps(ctx, t)
	if err != nil {
		return fmt.Errorf("fixtures: store down: %w", err)
	}
	defer c.close()
	v, err := c.do("BGREWRITEAOF")
	if err != nil {
		return fmt.Errorf("fixtures: store down: %w", err)
	}
	if v.Kind == resp.Error && v.Str != rewriteInProgress {
		return fmt.Errorf("fixtures: store down: BGREWRITEAOF: the store replied %s", v)
	}
	for {
		info, err := c.info("persistence")
		if err != nil {
			return fmt.Errorf("fixtures: store down: %w", err)
		}
		switch p := info["aof_rewrite_in_progress"]; {
		case p != "0" && p != "1":
			return fmt.Errorf("fixtures: store down: INFO persistence: aof_rewrite_in_progress %q, want 0 or 1", p)
		case p == "0" && info["aof_rewrite_scheduled"] != "1":
			if s := info["aof_last_bgrewrite_status"]; s != "ok" {
				return fmt.Errorf("fixtures: store down: the AOF rewrite ended with aof_last_bgrewrite_status %q", s)
			}
			return nil
		}
		if err := sleepUntil(ctx, time.Now().Add(rewritePoll)); err != nil {
			return fmt.Errorf("fixtures: store down: waiting for the AOF rewrite to end: %w", err)
		}
	}
}

// shutdown sends SHUTDOWN as the ops user and returns the instant just before it was sent,
// once the store has closed its port. A store that shuts down closes the connection without
// a reply: EOF (or a reset) is success. An error reply means it refused and still runs.
// Once SHUTDOWN is written its outcome is awaited within shutdownWait whatever ctx does: a
// lost or late reply leaves the outcome to the port, which must refuse connections by then.
func shutdown(ctx context.Context, t StoreTarget) (time.Time, error) {
	dctx, cancel := context.WithTimeout(ctx, opsWait)
	defer cancel()
	c, err := dialOps(dctx, t)
	if err != nil {
		return time.Time{}, fmt.Errorf("fixtures: store down: %w", err)
	}
	defer c.close()
	if err := ctx.Err(); err != nil {
		return time.Time{}, fmt.Errorf("fixtures: store down: %w", err)
	}
	start := time.Now()
	deadline := start.Add(shutdownWait)
	if err := c.nc.SetDeadline(deadline); err != nil {
		return time.Time{}, fmt.Errorf("fixtures: store down: SHUTDOWN: setting the deadline: %w", err)
	}
	if _, err := c.nc.Write(resp.AppendCommand(nil, "SHUTDOWN")); err != nil {
		return time.Time{}, fmt.Errorf("fixtures: store down: sending SHUTDOWN: %w", err)
	}
	v, rerr := c.r.ReadValue()
	switch {
	case rerr == nil && v.Kind == resp.Error:
		return time.Time{}, fmt.Errorf("fixtures: store down: SHUTDOWN: the store replied %s", v)
	case rerr == nil:
		return time.Time{}, fmt.Errorf("fixtures: store down: SHUTDOWN: the store replied %s and kept running", v)
	}
	if err := waitPortClosed(context.WithoutCancel(ctx), t.Addr, deadline); err != nil {
		if closedByPeer(rerr) {
			return time.Time{}, fmt.Errorf("fixtures: store down: the store closed the SHUTDOWN connection, but %w", err)
		}
		return time.Time{}, fmt.Errorf("fixtures: store down: SHUTDOWN: reading the reply: %w; and %w", rerr, err)
	}
	return start, nil
}

// closedByPeer reports whether err is the store closing the connection: the end of the
// stream, or a reset.
func closedByPeer(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

// waitPortClosed waits until a connection to addr is refused, at most until deadline: the
// store has closed its listening socket, so a store restarted on the same port can bind it.
func waitPortClosed(ctx context.Context, addr string, deadline time.Time) error {
	for {
		dctx, cancel := context.WithDeadline(ctx, deadline)
		var d net.Dialer
		nc, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		if err == nil {
			_ = nc.Close() // only the connect mattered
		}
		if !time.Now().Add(portPoll).Before(deadline) {
			return fmt.Errorf("the store still accepted connections at %s %v after SHUTDOWN", addr, shutdownWait)
		}
		time.Sleep(portPoll)
	}
}

// restartStore starts the store again within backWait: t.Restart, or t.Args as a detached
// child whose PID goes to t.PIDFile at once. It returns that PID, also with the error when
// only the PID file failed, or 0 after t.Restart and whenever nothing was started.
func restartStore(ctx context.Context, t StoreTarget) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, backWait)
	defer cancel()
	if t.Restart != nil {
		if err := t.Restart(ctx); err != nil {
			return 0, fmt.Errorf("fixtures: restarting the store: %w", err)
		}
		return 0, nil
	}
	pid, err := startDetached(ctx, t.Args, t.Env, t.OutPath)
	if err != nil {
		return 0, fmt.Errorf("fixtures: restarting the store: %w", err)
	}
	if err := writePIDFile(t.PIDFile, pid); err != nil {
		return pid, fmt.Errorf("fixtures: restarted the store as process %d: %w", pid, err)
	}
	return pid, nil
}

// recordPID reads the PID of the store answering at t.Addr from INFO server once it is back.
// After an Args restart (started is its PID, already in t.PIDFile) the two must be equal;
// after t.Restart (started is 0) it is written to t.PIDFile.
func recordPID(ctx context.Context, t StoreTarget, started int) error {
	pid, err := storePID(ctx, t)
	if err != nil {
		return fmt.Errorf("fixtures: the restarted store's PID: %w", err)
	}
	if started != 0 {
		if pid != started {
			return fmt.Errorf("fixtures: the store at %s is process %d, not the restarted process %d", t.Addr, pid, started)
		}
		return nil
	}
	if err := writePIDFile(t.PIDFile, pid); err != nil {
		return fmt.Errorf("fixtures: the restarted store's PID %d: %w", pid, err)
	}
	return nil
}

// waitPing probes the store until the first successful PING, one probe every pingEvery, each
// within pingTimeout, at most backWait in all, and returns the instant that PING's reply was
// read.
func waitPing(ctx context.Context, t StoreTarget) (time.Time, error) {
	limit := time.Now().Add(backWait)
	for {
		begin := time.Now()
		end, err := ping(ctx, t)
		if err == nil {
			return end, nil
		}
		next := begin.Add(pingEvery)
		if !next.Before(limit) {
			return time.Time{}, fmt.Errorf("fixtures: the store at %s did not answer PING within %v: %w", t.Addr, backWait, err)
		}
		if err := sleepUntil(ctx, next); err != nil {
			return time.Time{}, fmt.Errorf("fixtures: waiting for the store at %s to answer PING: %w", t.Addr, err)
		}
	}
}

// ping is one probe: a fresh connection, AUTH as the ops user and PING, within pingTimeout.
// It returns the instant +PONG was read. A loading store answers PING with LOADING.
func ping(ctx context.Context, t StoreTarget) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	c, err := dialOps(ctx, t)
	if err != nil {
		return time.Time{}, err
	}
	defer c.close()
	v, err := c.do("PING")
	if err != nil {
		return time.Time{}, err
	}
	if v.Kind != resp.Simple || v.Str != "PONG" {
		return time.Time{}, fmt.Errorf("PING: the store replied %s", v)
	}
	return time.Now(), nil
}

// storePID reads the PID of the store at t.Addr from INFO server (process_id).
func storePID(ctx context.Context, t StoreTarget) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, opsWait)
	defer cancel()
	c, err := dialOps(ctx, t)
	if err != nil {
		return 0, err
	}
	defer c.close()
	info, err := c.info("server")
	if err != nil {
		return 0, err
	}
	pid, err := parsePID(info["process_id"])
	if err != nil {
		return 0, fmt.Errorf("INFO server process_id: %w", err)
	}
	return pid, nil
}

// readPIDFile reads the PID in the file at path (decimal text, surrounding space ignored).
func readPIDFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("reading the PID file: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPIDFile+1))
	_ = f.Close() // read-only: the read's error is the one that matters
	if err != nil {
		return 0, fmt.Errorf("reading the PID file %s: %w", path, err)
	}
	if len(b) > maxPIDFile {
		return 0, fmt.Errorf("the PID file %s is longer than %d bytes", path, maxPIDFile)
	}
	pid, err := parsePID(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("the PID file %s: %w", path, err)
	}
	return pid, nil
}

// parsePID parses a PID that a signal may be sent to (checkPID).
func parsePID(s string) (int, error) {
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("PID %q is not a number", s)
	}
	if err := checkPID(pid); err != nil {
		return 0, err
	}
	return pid, nil
}

// checkPID refuses a PID no fault may signal: 0, 1 and negative values (kill(2) would reach a
// process group, init, or every process), values a pid_t cannot hold (they would wrap into
// those), and this process.
func checkPID(pid int) error {
	if pid <= 1 || pid > math.MaxInt32 || pid == os.Getpid() {
		return fmt.Errorf("PID %d: want a process other than this one, above 1 and at most %d", pid, math.MaxInt32)
	}
	return nil
}

// writePIDFile writes pid to the file at path: to a temporary file in its directory first,
// renamed over path, so a reader sees the old content or the new, never a part.
func writePIDFile(path string, pid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("writing the PID file: %w", err)
	}
	_, werr := f.WriteString(strconv.Itoa(pid) + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(f.Name()) // the half-written temporary file goes; the error says why
		return fmt.Errorf("writing the PID file: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("writing the PID file: %w", err)
	}
	return nil
}

// startDetached starts args as a child that outlives its starter (AGENTS.md §2 "Child
// processes"; plan choice): on a context that never ends; in its own process group, so a
// terminal's SIGINT to the caller never reaches it; with env as its whole environment, never
// the caller's, so no password reaches it that way; and its output appended to outPath (""
// discards it). The process is released and never waited for: its owner is whoever stops it
// by its PID (the run's orchestrator, or a test), and when a short-lived caller such as the
// faultctl command exits, launchd adopts and reaps it. A caller that lives on and must reap
// the process itself passes a Restart or Start hook instead.
func startDetached(ctx context.Context, args, env []string, outPath string) (int, error) {
	if len(args) == 0 {
		return 0, errors.New("no command line")
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), args[0], args[1:]...)
	cmd.Env = append([]string{}, env...) // never nil: nil would hand over the caller's environment
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if outPath != "" {
		out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return 0, fmt.Errorf("the output file: %w", err)
		}
		defer func() { _ = out.Close() }() // the child holds its own descriptor
		cmd.Stdout, cmd.Stderr = out, out
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", args[0], err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // handed over (above); releasing cannot fail in a way that matters here
	return pid, nil
}

// opsConn is one RESP2 connection to the store as the ops user (DESIGN §A2). It sends one
// command at a time; every exchange ends by the deadline of the ctx it was dialed with, and
// that ctx ending ends a wait at once. After an error the stream position is unknown: the
// caller closes it.
type opsConn struct {
	ctx context.Context
	nc  net.Conn
	r   *resp.Reader
}

// dialOps connects to t.Addr and authenticates as t.OpsUser within ctx, which must have a
// deadline; ctx ending ends the wait at once, and dialOps then reports ctx's error
// (authenticate). Errors never hold the password (resp.Handshake redacts the store's reply).
func dialOps(ctx context.Context, t StoreTarget) (*opsConn, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("connecting to the store: no deadline")
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to the store at %s: %w", t.Addr, err)
	}
	if err := authenticate(ctx, nc, t.OpsUser, t.OpsPassword); err != nil {
		return nil, fmt.Errorf("authenticating to the store at %s as %s: %w", t.Addr, t.OpsUser, err)
	}
	return &opsConn{ctx: ctx, nc: nc, r: resp.NewReader(nc)}, nil
}

// authenticate runs resp.Handshake on the freshly dialed nc as user, within ctx's deadline
// (ctx must have one). ctx ending closes nc, which ends any wait on it for good; a past
// deadline would not do, since resp.Handshake sets its own deadline first and that replaces
// one a cancel just before had set. On an error nc is closed, and the error is ctx's once ctx
// has ended (the closed connection's error is then only its effect).
func authenticate(ctx context.Context, nc net.Conn, user, pass string) error {
	deadline, _ := ctx.Deadline()
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	err := resp.Handshake(nc, user, pass, deadline, nil)
	stop()
	if cerr := ctx.Err(); cerr != nil {
		err = cerr
	}
	if err != nil {
		_ = nc.Close() // unusable either way; a second close changes nothing
		return err
	}
	return nil
}

// do sends one command and returns its reply; an error reply is a reply, not an error.
func (c *opsConn) do(args ...string) (resp.Value, error) {
	if err := c.ctx.Err(); err != nil {
		return resp.Value{}, fmt.Errorf("%s: %w", args[0], err)
	}
	deadline, _ := c.ctx.Deadline() // dialOps checked that there is one
	if err := c.nc.SetDeadline(deadline); err != nil {
		return resp.Value{}, fmt.Errorf("%s: setting the deadline: %w", args[0], err)
	}
	stop := context.AfterFunc(c.ctx, func() { _ = c.nc.SetDeadline(time.Now()) })
	defer stop()
	if _, err := c.nc.Write(resp.AppendCommand(nil, args...)); err != nil {
		return resp.Value{}, fmt.Errorf("%s: sending: %w", args[0], c.cause(err))
	}
	v, err := c.r.ReadValue()
	if err != nil {
		return resp.Value{}, fmt.Errorf("%s: reading the reply: %w", args[0], c.cause(err))
	}
	return v, nil
}

// info reads one INFO section as name:value fields.
func (c *opsConn) info(section string) (map[string]string, error) {
	v, err := c.do("INFO", section)
	if err != nil {
		return nil, err
	}
	if v.Kind != resp.Bulk || v.Null {
		return nil, fmt.Errorf("INFO %s: the store replied %s, want a bulk string", section, v)
	}
	return storeproc.ParseInfo(v.Str), nil
}

// cause is ctx's error when it ended (that made the I/O fail), else err.
func (c *opsConn) cause(err error) error {
	if cerr := c.ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

func (c *opsConn) close() { _ = c.nc.Close() } // nothing waits on the connection any more

// StoreConn is one RESP2 connection to the store as one ACL user, each exchange bounded by
// the deadline of the ctx it was dialed with, and that ctx ending ends a wait at once
// (IMPLEMENTATION_PLAN H4 step 3): faultctl's rl_fixture connection for the controlled clock
// and the tamper tool (a StoreConn is a ClockConn and a TamperConn), and its rl_ops connection
// for INFO and CONFIG GET (DESIGN §A2). It sends one command at a time and is not safe for
// concurrent use; after an error the stream position is unknown, so the caller closes it.
type StoreConn struct{ c *opsConn }

// DialStore connects to addr and authenticates as user within ctx, which must have a
// deadline. Errors never hold the password.
func DialStore(ctx context.Context, addr, user, password string) (*StoreConn, error) {
	c, err := dialOps(ctx, StoreTarget{Addr: addr, OpsUser: user, OpsPassword: password})
	if err != nil {
		return nil, fmt.Errorf("fixtures: %w", err)
	}
	return &StoreConn{c: c}, nil
}

// Do sends one command and returns its reply in the storeproc.Reply form the fixtures take;
// an error reply is a reply (Kind '-'), not an error.
func (s *StoreConn) Do(args ...string) (storeproc.Reply, error) {
	if len(args) == 0 {
		return storeproc.Reply{}, errors.New("fixtures: no command")
	}
	v, err := s.c.do(args...)
	if err != nil {
		return storeproc.Reply{}, err
	}
	return toReply(v), nil
}

// Info reads one INFO section as name:value fields.
func (s *StoreConn) Info(section string) (map[string]string, error) { return s.c.info(section) }

// Close closes the connection.
func (s *StoreConn) Close() { s.c.close() }

// toReply converts a RESP2 value to the storeproc.Reply form.
func toReply(v resp.Value) storeproc.Reply {
	r := storeproc.Reply{Str: v.Str, Int: v.Int, Nil: v.Null}
	switch v.Kind {
	case resp.Simple:
		r.Kind = '+'
	case resp.Error:
		r.Kind = '-'
	case resp.Int:
		r.Kind = ':'
	case resp.Bulk:
		r.Kind = '$'
	case resp.Array:
		r.Kind = '*'
		for _, e := range v.Elems {
			r.Arr = append(r.Arr, toReply(e))
		}
	}
	return r
}
