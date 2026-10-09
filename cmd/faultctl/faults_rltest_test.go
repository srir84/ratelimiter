//go:build rltest

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/fixtures"
	"ratelimiter/harness/runlog"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
	"ratelimiter/internal/testutil/storeproc"
)

// The child processes these tests start from their own binary (TestMain), named by helperEnv
// in their environment: a stand-in for a hitapi instance's process, which api kill and the
// epoch lookup find through pids.json, and faultctl's own main, which a test signals.
const (
	helperEnv      = "FAULTCTL_TEST_HELPER"
	helperHitAPI   = "hitapi"   // standInHitAPI
	helperFaultctl = "faultctl" // main, on the child's arguments
	// helperLinesEnv names the file a hitapi stand-in appends its line to, and helperLibEnv the
	// RL_LIB_PASSWORD it is to get (standInHitAPI). Neither starts with RL_, so a restart keeps
	// both (plainEnv).
	helperLinesEnv = "FAULTCTL_TEST_HELPER_LINES"
	helperLibEnv   = "FAULTCTL_TEST_HELPER_LIB"
	// helperLife is how long a hitapi stand-in that nobody kills lives.
	helperLife = time.Minute
	// procWait bounds each wait on a child process: for its line, its state, its exit;
	// storeReady bounds a store's start until it has loaded; storeStarts is how many free ports
	// a store start tries, since another process can take one between the probe and the bind.
	procWait    = 10 * time.Second
	storeReady  = 10 * time.Second
	storeStarts = 3
	// A store fault of 1 s (IMPLEMENTATION_PLAN H4 tests) measures an outage of more than its
	// 1,000 ms (checkOutageLine) and at most 5,000 ms.
	faultFor    = time.Second
	maxOutageNs = int64(5 * time.Second)
	// clockPad is how long a paddedClock waits before and after each of its readings.
	clockPad = 100 * time.Millisecond
	// opsPassword is a fake store's rl_ops password; libPassword the RL_LIB_PASSWORD api kill
	// --restart hands the restarted instance.
	opsPassword = "0ps-test-password"
	libPassword = "l1b-test-password"
	// logBefore is what an instance's run log holds before the kill.
	logBefore = `{"kind":"lifecycle","event":"start"}` + "\n" + `{"kind":"lifecycle","event":"ready"}` + "\n"
	// fileLimitShell is the orchestrator's file-limit shell (plan H3 step 4): a run's store and
	// instances start through it, and faultctl restarts them through it.
	fileLimitShell = `ulimit -S -n 8192; exec "$0" "$@"`
	// quitIgnoringShell starts its arguments with SIGQUIT ignored, as a shell starts a
	// background job (TestStopSignals).
	quitIgnoringShell = `trap "" QUIT; exec "$0" "$@"`
)

// TestMain runs the tests or, in a child process a test started from this binary, what
// helperEnv names: faultctl's main on the child's arguments, or a hitapi stand-in.
func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case helperFaultctl:
		main()
	case helperHitAPI:
		os.Exit(standInHitAPI())
	}
	os.Exit(m.Run())
}

// standInHitAPI stands in for a hitapi instance's process (TestMain): it appends one JSON line
// (standInLine: its PID, its command line, its RL_ variables' names and whether
// RL_LIB_PASSWORD is $FAULTCTL_TEST_HELPER_LIB) to $FAULTCTL_TEST_HELPER_LINES with one
// O_APPEND write, then sleeps until a signal ends it, at most helperLife. faultctl reads only
// its command line (hitapi --config … --listen … --log …); the test's own server answers its
// routes.
func standInHitAPI() int {
	l := standInLine{PID: os.Getpid(), Argv: os.Args, RL: []string{}}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "RL_") {
			l.RL = append(l.RL, name)
		}
	}
	slices.Sort(l.RL)
	lib, ok := os.LookupEnv(config.LibPasswordEnv)
	l.Lib = ok && lib != "" && lib == os.Getenv(helperLibEnv)
	data, err := json.Marshal(l)
	if err != nil {
		return 1
	}
	f, err := os.OpenFile(os.Getenv(helperLinesEnv), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 1
	}
	_, werr := f.Write(append(data, '\n'))
	if cerr := f.Close(); werr != nil || cerr != nil {
		return 1
	}
	time.Sleep(helperLife)
	return 0
}

// standInLine is the line a hitapi stand-in writes (standInHitAPI).
type standInLine struct {
	PID  int      `json:"pid"`
	Argv []string `json:"argv"` // its command line, os.Args
	RL   []string `json:"rl"`   // its RL_ variables' names, sorted
	Lib  bool     `json:"lib"`  // its RL_LIB_PASSWORD was the one the test gave
}

// readLines reads the stand-in lines in the file at path; a missing file holds none.
func readLines(t *testing.T, path string) []standInLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []standInLine
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var l standInLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("stand-in line %q: %v", line, err)
		}
		out = append(out, l)
	}
	return out
}

// waitLines waits up to procWait until the stand-in lines file at path holds n lines and
// returns them; fewer when the wait ran out.
func waitLines(t *testing.T, path string, n int) []standInLine {
	t.Helper()
	limit := time.Now().Add(procWait)
	for {
		lines := readLines(t, path)
		if len(lines) >= n || !time.Now().Before(limit) {
			return lines
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// child is a process the test started: recorded in .out/procs while it runs (bin/trace procs)
// and reaped by a waiter goroutine; the cleanup kills it if it still runs and waits for the
// waiter.
type child struct {
	pid   int
	done  chan struct{}    // closed once the waiter has reaped it and removed its record
	state *os.ProcessState // set before done closes
}

// startChild starts cmd as a child the test owns (child). cmd is made with a context that
// never ends (the cleanup ends the child, AGENTS.md §2).
func startChild(t *testing.T, cmd *exec.Cmd) *child {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{pid: cmd.Process.Pid, done: make(chan struct{})}
	remove, err := recordProc(c.pid, strings.Join(cmd.Args, " "))
	if err != nil {
		_ = cmd.Process.Kill() // unrecorded: it must not outlive this call
		_ = cmd.Wait()
		t.Fatalf("recording process %d: %v", c.pid, err)
	}
	var recErr error // set before done closes
	go func() {
		_ = cmd.Wait() // a killed child's error is its signal, read from ProcessState
		c.state = cmd.ProcessState
		recErr = remove()
		close(c.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // gone already when the test ended it: nothing to do then
		<-c.done
		if recErr != nil {
			t.Errorf("removing the record of process %d: %v", c.pid, recErr)
		}
	})
	return c
}

// exited reports whether the child has exited and been reaped.
func (c *child) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// wait waits up to procWait for the child to exit and returns how it ended.
func (c *child) wait(t *testing.T) *os.ProcessState {
	t.Helper()
	timer := time.NewTimer(procWait)
	defer timer.Stop()
	select {
	case <-c.done:
		return c.state
	case <-timer.C:
		t.Fatalf("process %d still ran %v later", c.pid, procWait)
		return nil
	}
}

// checkKilled waits for the child to exit and checks that SIGKILL ended it.
func checkKilled(t *testing.T, c *child) {
	t.Helper()
	st := c.wait(t)
	if ws, ok := st.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("process %d ended with %v, want SIGKILL", c.pid, st)
	}
}

// recordPath is the process record of pid, <module root>/.out/procs/<pid>, which bin/trace
// procs reads.
func recordPath(t *testing.T, pid int) string {
	t.Helper()
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, ".out", "procs", strconv.Itoa(pid))
}

// recordProc writes the process record .out/procs/<pid> (bin/trace procs) for a child the test
// started, and returns the function that removes it once the child is gone (it also removes a
// record that failed half-written).
func recordProc(pid int, command string) (func() error, error) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, ".out", "procs", strconv.Itoa(pid))
	remove := func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(command+"\n"), 0o644); err != nil {
		return nil, errors.Join(err, remove())
	}
	return remove, nil
}

// adoptRestarted records pid, a process faultctl restarted inside this test process from its
// command line (a released child nobody else waits for), in .out/procs, and registers the
// cleanup that kills it, reaps it (wait4) and removes the record.
func adoptRestarted(t *testing.T, pid int, command string) {
	t.Helper()
	remove, recErr := recordProc(pid, command)
	t.Cleanup(func() {
		reapDetached(t, pid)
		if recErr == nil {
			recErr = remove()
		}
		if recErr != nil {
			t.Errorf("the record of process %d: %v", pid, recErr)
		}
	})
}

// reapDetached kills a released child of this test process and reaps its exit with wait4, at
// most procWait: no exec.Cmd waits for it. A process reaped already is left alone.
func reapDetached(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("killing process %d: %v", pid, err)
	}
	limit := time.Now().Add(procWait)
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.ECHILD):
			return // reaped already
		case err != nil:
			t.Errorf("reaping process %d: %v", pid, err)
			return
		case got == pid:
			return
		case !time.Now().Before(limit):
			t.Errorf("process %d did not exit within %v of SIGKILL", pid, procWait)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writePIDs writes the run directory's pids.json naming procs.
func writePIDs(t *testing.T, dir string, procs ...runlog.Proc) {
	t.Helper()
	if err := runlog.WritePIDs(filepath.Join(dir, runlog.PIDsFile), runlog.PIDs{Procs: procs}); err != nil {
		t.Fatal(err)
	}
}

// checkPIDs checks that the run directory's pids.json names want, in that order.
func checkPIDs(t *testing.T, dir string, want ...runlog.Proc) {
	t.Helper()
	p, err := runlog.ReadPIDs(filepath.Join(dir, runlog.PIDsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Procs, want) {
		t.Errorf("pids.json names %+v, want %+v", p.Procs, want)
	}
}

// decodeLine decodes faultctl's one stdout line and returns it and its keys, sorted.
func decodeLine(t *testing.T, out string) (map[string]any, []string) {
	t.Helper()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("stdout %q: want one line", out)
	}
	var line map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&line); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	keys := make([]string, 0, len(line))
	for k := range line {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return line, keys
}

// outageTimes are the times a store fault's line gives (checkOutageLine): start_ns, end_ns,
// outage_start_ns and outage_ns.
type outageTimes struct {
	start, end, outageStart, outage int64
}

// checkOutageLine checks a store fault's line and returns its times: its cmd; start_ns before
// end_ns, the command's own times; outage_ns more than lo ns and at most hi ns, lo the
// commanded d (or 0 for a fault cut short): a real outage ends at the first PING answered after
// SIGCONT or the restart, d after it began, so an outage_ns that was not measured, d itself
// among them, fails; the outage it measured, from outage_start_ns (just before SHUTDOWN or
// SIGSTOP) for outage_ns, inside the command's times: start_ns <= outage_start_ns and
// outage_start_ns + outage_ns <= end_ns, each within |adj|, the wall clock's adjustment the
// test saw around the command (clockAdjustment: outage_ns is a monotonic length and the
// others wall-clock readings); and, with failed, an error.
func checkOutageLine(t *testing.T, out, cmd string, lo, hi int64, failed bool, adj int64) outageTimes {
	t.Helper()
	line, keys := decodeLine(t, out)
	want := []string{"cmd", "end_ns", "outage_ns", "outage_start_ns", "start_ns"}
	if failed {
		want = []string{"cmd", "end_ns", "error", "outage_ns", "outage_start_ns", "start_ns"}
	}
	if !slices.Equal(keys, want) {
		t.Errorf("line %s: keys %v, want %v", out, keys, want)
	}
	if line["cmd"] != cmd {
		t.Errorf("line %s: cmd %v, want %q", out, line["cmd"], cmd)
	}
	if msg, _ := line["error"].(string); failed && msg == "" {
		t.Errorf("line %s: no error, want the failure's message", out)
	}
	num := func(k string) int64 {
		n, ok := line[k].(json.Number)
		if !ok {
			return -1
		}
		v, err := n.Int64()
		if err != nil {
			return -1
		}
		return v
	}
	got := outageTimes{start: num("start_ns"), end: num("end_ns"), outageStart: num("outage_start_ns"), outage: num("outage_ns")}
	if got.start <= 0 || got.end <= got.start {
		t.Errorf("line %s: start_ns %d, end_ns %d: want 0 < start_ns < end_ns", out, got.start, got.end)
	}
	if got.outage <= lo || got.outage > hi {
		t.Errorf("line %s: outage_ns %d (%v), want more than %d and at most %d", out, got.outage, time.Duration(got.outage), lo, hi)
	}
	if slack := max(adj, -adj); got.outageStart < got.start-slack || got.outageStart+got.outage > got.end+slack {
		t.Errorf("line %s: an outage of outage_ns %d from outage_start_ns %d, start_ns %d, end_ns %d (the wall clock adjusted by %d ns meanwhile): "+
			"want it inside the command's times, start_ns <= outage_start_ns and outage_start_ns + outage_ns <= end_ns, within that adjustment",
			out, got.outage, got.outageStart, got.start, got.end, adj)
	}
	return got
}

// clockAdjustment is how far the wall clock moved beyond the monotonic clock between before
// and after, two time.Now readings: the system clock's steps and slews in between (timed on
// macOS slews it by up to a few ms a second and sometimes steps it by tens of ms).
func clockAdjustment(before, after time.Time) int64 {
	return (after.UnixNano() - before.UnixNano()) - after.Sub(before).Nanoseconds()
}

// paddedClock is a clock for one command (its now, run's now) that records each reading and
// waits pad before and after taking it: whatever the command does between its first and last
// readings starts at least pad after the first and ends at least pad before the last.
type paddedClock struct {
	pad time.Duration

	mu    sync.Mutex
	reads []time.Time
}

func (c *paddedClock) now() time.Time {
	time.Sleep(c.pad)
	now := time.Now()
	c.mu.Lock()
	c.reads = append(c.reads, now)
	c.mu.Unlock()
	time.Sleep(c.pad)
	return now
}

// checkOutageTimes checks a store fault's times (DESIGN §B5 "Faults": a fault's actual start
// and end; §B6) against the paddedClock that timed its command: start_ns and end_ns are the
// command's own, the clock's first and last readings, and the outage, outage_ns long from
// outage_start_ns, lies at least pad inside them, within |adj| (clockAdjustment). A start or
// an end taken from anything else, the outage's own bounds among them, or an outage placed at
// the command's start or end fails.
func (c *paddedClock) checkOutageTimes(t *testing.T, cmd string, got outageTimes, adj int64) {
	t.Helper()
	c.mu.Lock()
	reads := slices.Clone(c.reads)
	c.mu.Unlock()
	if len(reads) < 2 || got.start != reads[0].UnixNano() || got.end != reads[len(reads)-1].UnixNano() {
		t.Errorf("%s: start_ns %d, end_ns %d; want the command's own start and end, its clock's first and last readings %v", cmd, got.start, got.end, reads)
	}
	pad, slack := c.pad.Nanoseconds(), max(adj, -adj)
	if head, tail := got.outageStart-got.start, got.end-(got.outageStart+got.outage); head < pad-slack || tail < pad-slack {
		t.Errorf("%s: the outage starts %v after start_ns and ends %v before end_ns (the wall clock adjusted by %d ns meanwhile): "+
			"want each at least %v, the clock's wait after its first reading and before its last", cmd, time.Duration(head), time.Duration(tail), adj, c.pad)
	}
}

// The store's log lines that time its AOF load after a store down (Valkey 9.1, the lines
// TestStoreCtlDownAndPause reads in store.log; a run's store logs to its output file): SHUTDOWN
// arriving, then the restarted store's load ("…: 0.165 seconds", from Valkey's wall clock,
// rounded to the ms).
const (
	shutdownLogLine = "User requested shutdown"
	loadLogLine     = "DB loaded from append only file: "
	// loadLogWait bounds the wait for the restarted store to log its load.
	loadLogWait = 10 * time.Second
	// loadResolution is the resolution of the logged load time.
	loadResolution = time.Millisecond
)

// checkLoadInOutage checks the outage, outage ns long, of a store down commanded for d against
// the store's log at path (the run's valkey-server.stderr, which the restarted store appends
// to) from offset, its size before the fault: the outage ends at the first PING answered after
// the restarted store loaded its AOF (DESIGN §B6 FLT-1 (a): LOADING replies count inside it),
// so it holds d and the load time the log shows (aofLoadTime), within the log's resolution and
// |adj| (clockAdjustment: the load time is a wall-clock length, outage_ns a monotonic one). On a
// store whose load takes long (storeproc.PaddingA7), an outage_ns that was not measured, d or a
// little more, fails.
func checkLoadInOutage(t *testing.T, cmd, path string, offset int, d time.Duration, outage, adj int64) {
	t.Helper()
	load := aofLoadTime(t, path, offset)
	t.Logf("%s: an outage of %v, the AOF load %v", cmd, time.Duration(outage), load)
	if least := int64(d+load-loadResolution) - max(adj, -adj); outage < least {
		t.Errorf("%s: outage_ns %d (%v), want at least %d: the commanded %v plus the AOF load of %v that %s shows (the wall clock adjusted by %d ns meanwhile)",
			cmd, outage, time.Duration(outage), least, d, load, path, adj)
	}
}

// aofLoadTime reads the store's log at path from offset (the lines of one store down) until
// the restarted store has logged its AOF load after the SHUTDOWN line, at most loadLogWait,
// and returns that load time.
func aofLoadTime(t *testing.T, path string, offset int) time.Duration {
	t.Helper()
	limit := time.Now().Add(loadLogWait)
	for {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < offset {
			t.Fatalf("%s has %d bytes, fewer than the %d it had before the fault", path, len(data), offset)
		}
		shutdown := false
		for _, line := range strings.SplitAfter(string(data[offset:]), "\n") {
			_, rest, found := strings.Cut(line, loadLogLine)
			switch {
			case !strings.HasSuffix(line, "\n"): // the last one: empty, or still being written
			case !shutdown:
				shutdown = strings.Contains(line, shutdownLogLine)
			case found:
				secs, ok := strings.CutSuffix(strings.TrimSuffix(rest, "\n"), " seconds")
				load, err := time.ParseDuration(secs + "s")
				if !ok || err != nil || load < 0 {
					t.Fatalf("%s: no load time in %q", path, line)
				}
				return load
			}
		}
		if !time.Now().Before(limit) {
			t.Fatalf("%s: no %q line after %q within %v", path, loadLogLine, shutdownLogLine, loadLogWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fileSize returns the size of the file at path, in bytes.
func fileSize(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}

// psState returns ps's state letters for pid ("" when ps finds no such process).
func psState(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("ps -o stat= -p %d: %v", pid, err)
	}
	return strings.TrimSpace(string(out))
}

// waitState waits up to procWait until ps shows pid stopped (state T) or, with stopped false,
// not stopped.
func waitState(t *testing.T, pid int, stopped bool) {
	t.Helper()
	limit := time.Now().Add(procWait)
	for {
		st := psState(t, pid)
		if strings.HasPrefix(st, "T") == stopped {
			return
		}
		if !time.Now().Before(limit) {
			t.Fatalf("process %d: ps state %q after %v, want stopped %v", pid, st, procWait, stopped)
		}
		time.Sleep(time.Millisecond)
	}
}

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if err := l.Close(); err != nil || !ok {
		t.Fatalf("probing a free port: %v (%v)", err, l.Addr())
	}
	return addr.Port
}

// runStore is a durable store started as the orchestrator starts a run's store
// (startRunStore).
type runStore struct {
	*child
	addr string
	port int
	pw   storeproc.Passwords
	args []string // its command line
}

// startRunStore starts a durable store in the run directory dir with the orchestrator's exact
// command line (plan H3 step 3; runlog.FileLimitCommand of runlog.StoreArgs): the file-limit
// shell, valkey-server, the durable profile, a free loopback port, the run's store directory
// with a freshly rendered ACL file in it, and --auto-aof-rewrite-percentage 0 unless forced. Its
// output goes to the run's valkey-server.stderr, where faultctl's restart appends too, and its
// environment is PATH alone. It waits until the store has loaded (INFO persistence loading:0 as
// rl_ops); the cleanup kills and reaps it (startChild).
func startRunStore(t *testing.T, dir string, forced bool) *runStore {
	t.Helper()
	bin, err := storeproc.Bin()
	if err != nil {
		t.Fatal(err)
	}
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	acl := filepath.Join(storeDir, "acl.conf")
	pw := storeproc.NewPasswords()
	if err := storeproc.RenderACL(filepath.Join(root, "deploy", "acl.conf.tmpl"), acl, pw); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(root, "deploy", "valkey-durable.conf")
	outPath := filepath.Join(dir, runlog.RoleValkey+".stderr")
	for attempt := 1; ; attempt++ {
		port := freePort(t)
		args := runlog.FileLimitCommand(bin, runlog.StoreArgs(conf, port, storeDir, acl, forced)...)
		out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(context.WithoutCancel(t.Context()), args[0], args[1:]...)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		cmd.Stdout, cmd.Stderr = out, out
		st := &runStore{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), port: port, pw: pw, args: args}
		st.child = startChild(t, cmd)
		_ = out.Close() // the child holds its own descriptor
		err = st.waitLoaded()
		if err == nil {
			return st
		}
		if !st.exited() || attempt == storeStarts {
			t.Fatalf("starting the store on port %d: %v (its output: %s)", port, err, outPath)
		}
	}
}

// waitLoaded waits up to storeReady until INFO persistence, read as rl_ops, shows loading:0;
// it fails at once once the process has exited (its port was taken, say).
func (st *runStore) waitLoaded() error {
	limit := time.Now().Add(storeReady)
	for {
		if st.exited() {
			return errors.New("the store process exited")
		}
		err := st.loaded()
		if err == nil {
			return nil
		}
		if !time.Now().Before(limit) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// loaded returns nil when INFO persistence, read as rl_ops, shows loading:0.
func (st *runStore) loaded() error {
	c, err := storeproc.Dial(st.addr, userOps, st.pw.Ops)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }() // a read-only probe
	r, err := c.Do("INFO", "persistence")
	if err != nil {
		return err
	}
	if v := storeproc.ParseInfo(r.Str)["loading"]; r.Kind != '$' || v != "0" {
		return fmt.Errorf("INFO persistence: %s", r)
	}
	return nil
}

// do runs one command as rl_ops on a fresh connection to the store's address.
func (st *runStore) do(t *testing.T, args ...string) storeproc.Reply {
	t.Helper()
	c, err := storeproc.Dial(st.addr, userOps, st.pw.Ops)
	if err != nil {
		t.Fatalf("connecting to the store at %s as rl_ops: %v", st.addr, err)
	}
	defer func() { _ = c.Close() }() // one command, read in full
	r, err := c.Do(args...)
	if err != nil {
		t.Fatalf("%s: %v", args[0], err)
	}
	return r
}

// settings reads what the store at the address runs with, as rl_ops: CONFIG GET port, dir,
// aclfile and auto-aof-rewrite-percentage, and INFO server's process_id.
func (st *runStore) settings(t *testing.T) map[string]string {
	t.Helper()
	names := []string{"port", "dir", "aclfile", "auto-aof-rewrite-percentage"}
	r := st.do(t, append([]string{"CONFIG", "GET"}, names...)...)
	if r.Kind != '*' || len(r.Arr)%2 != 0 {
		t.Fatalf("CONFIG GET %s: the store replied %s", strings.Join(names, " "), r)
	}
	m := map[string]string{}
	for i := 0; i < len(r.Arr); i += 2 {
		m[r.Arr[i].Str] = r.Arr[i+1].Str
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			t.Fatalf("CONFIG GET %s: no %s in %s", strings.Join(names, " "), n, r)
		}
	}
	m["process_id"] = storeproc.ParseInfo(st.do(t, "INFO", "server").Str)["process_id"]
	return m
}

// reapChildStores registers the cleanup that kills and reaps every valkey-server process whose
// parent is this test process, other than the store st the test started: a store faultctl's
// store down restarted here, from its command line, is a released child nobody else waits for,
// wherever it listens. Each one's .out/procs record goes too.
func reapChildStores(t *testing.T, st *runStore) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), procWait)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,command=").Output()
		if err != nil {
			t.Errorf("listing the processes: %v", err)
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) < 3 {
				continue
			}
			pid, err1 := strconv.Atoi(f[0])
			ppid, err2 := strconv.Atoi(f[1])
			if err1 != nil || err2 != nil || ppid != os.Getpid() || pid == st.pid || !strings.Contains(strings.Join(f[2:], " "), "valkey-server") {
				continue
			}
			reapDetached(t, pid)
			if err := os.Remove(recordPath(t, pid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("removing the record of process %d: %v", pid, err)
			}
		}
	})
}

// TestStoreCommandLineFromStore (IMPLEMENTATION_PLAN H4 step 3; fake store): store down's
// restart line is the orchestrator's: the file-limit shell, valkey-server, the profile's conf,
// --port, --dir and --aclfile, and --auto-aof-rewrite-percentage 0 unless the store runs with
// another value (forced rewrites keep the profile's), built from INFO server's executable,
// config_file, process_id and tcp_port and from CONFIG GET dir, aclfile and
// auto-aof-rewrite-percentage, each read as rl_ops. A store that is another process than the
// one pids.json names, or listens on another port, is refused before CONFIG GET; a relative
// path, an error reply or a CONFIG GET reply that is not name and value pairs of bulk strings
// for every setting is refused too.
func TestStoreCommandLineFromStore(t *testing.T) {
	const (
		exe  = "/opt/valkey/bin/valkey-server"
		conf = "/repo/deploy/valkey-durable.conf"
		dir  = "/repo/runs/20261009-120000-s-1/store"
		acl  = "/repo/runs/20261009-120000-s-1/store/acl.conf"
		pid  = 4242
	)
	fake := fakeresp.Start(t, userOps, opsPassword)
	_, port, err := net.SplitHostPort(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	otherPort := strconv.Itoa(portNum + 1)
	info := func(exe, pid, port string) resp.Value {
		return resp.Value{Kind: resp.Bulk, Str: "# Server\r\nexecutable:" + exe + "\r\nconfig_file:" + conf +
			"\r\nprocess_id:" + pid + "\r\ntcp_port:" + port + "\r\n"}
	}
	bulk := func(s string) resp.Value { return resp.Value{Kind: resp.Bulk, Str: s} }
	pairs := func(kv ...string) resp.Value {
		v := resp.Value{Kind: resp.Array}
		for _, s := range kv {
			v.Elems = append(v.Elems, bulk(s))
		}
		return v
	}
	target := fixtures.StoreTarget{Addr: fake.Addr(), OpsUser: userOps, OpsPassword: opsPassword}
	head := []string{"/bin/sh", "-c", fileLimitShell, exe, conf, "--port", port, "--dir", dir, "--aclfile", acl}
	sent := func(n int) [][]string {
		var out [][]string
		for _, c := range fake.Commands()[n:] {
			out = append(out, c.Args)
		}
		return out
	}
	ptr := func(v resp.Value) *resp.Value { return &v }
	for _, c := range []struct {
		rewrite string
		want    []string
	}{
		{"0", append(slices.Clone(head), "--auto-aof-rewrite-percentage", "0")},
		{"100", head},
	} {
		fake.ReplyValue("INFO", info(exe, strconv.Itoa(pid), port), 1)
		fake.ReplyValue("CONFIG", pairs("auto-aof-rewrite-percentage", c.rewrite, "dir", dir, "aclfile", acl), 1)
		n := len(fake.Commands())
		got, err := storeCommandLine(t.Context(), target, pid)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("auto-aof-rewrite-percentage %s: the restart line\n  %q (%v)\nwant\n  %q", c.rewrite, got, err, c.want)
		}
		want := [][]string{{"AUTH", userOps, opsPassword}, {"INFO", "server"}, {"CONFIG", "GET", "dir", "aclfile", "auto-aof-rewrite-percentage"}}
		if got := sent(n); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("auto-aof-rewrite-percentage %s: the store got %q, want %q", c.rewrite, got, want)
		}
	}

	good := pairs("dir", dir, "aclfile", acl, "auto-aof-rewrite-percentage", "0")
	for _, c := range []struct {
		name   string
		info   resp.Value
		config *resp.Value // nil: no CONFIG GET may be sent
	}{
		{"another process", info(exe, "4243", port), nil},
		{"another port", info(exe, strconv.Itoa(pid), otherPort), nil},
		{"relative executable", info("valkey-server", strconv.Itoa(pid), port), &good},
		{"relative dir", info(exe, strconv.Itoa(pid), port), ptr(pairs("dir", "store", "aclfile", acl, "auto-aof-rewrite-percentage", "0"))},
		{"relative aclfile", info(exe, strconv.Itoa(pid), port), ptr(pairs("dir", dir, "aclfile", "acl.conf", "auto-aof-rewrite-percentage", "0"))},
		{"no aclfile", info(exe, strconv.Itoa(pid), port), ptr(pairs("dir", dir, "auto-aof-rewrite-percentage", "0"))},
		{"no rewrite setting", info(exe, strconv.Itoa(pid), port), ptr(pairs("dir", dir, "aclfile", acl))},
		{"an odd reply", info(exe, strconv.Itoa(pid), port), ptr(pairs("dir", dir, "aclfile", acl, "auto-aof-rewrite-percentage"))},
		{"a number in the reply", info(exe, strconv.Itoa(pid), port),
			&resp.Value{Kind: resp.Array, Elems: []resp.Value{bulk("dir"), {Kind: resp.Int, Int: 7}}}},
		{"an error reply", info(exe, strconv.Itoa(pid), port), &resp.Value{Kind: resp.Error, Str: "NOPERM this user has no permissions"}},
	} {
		fake.ReplyValue("INFO", c.info, 1)
		if c.config != nil {
			fake.ReplyValue("CONFIG", *c.config, 1)
		}
		n := len(fake.Commands())
		if got, err := storeCommandLine(t.Context(), target, pid); err == nil {
			t.Errorf("%s: the restart line %q, want a refusal", c.name, got)
		}
		configs := 0
		for _, args := range sent(n) {
			if fakeresp.Label(args) == "CONFIG" {
				configs++
			}
		}
		if want := map[bool]int{true: 0, false: 1}[c.config == nil]; configs != want {
			t.Errorf("%s: %d CONFIG GET sent, want %d", c.name, configs, want)
		}
	}
}

// TestRunPIDs (IMPLEMENTATION_PLAN H4 step 3): procPID reads the PID pids.json names for a role
// (and instance); setPID rewrites that one entry, the others as they were, and refuses a role
// or instance pids.json does not name, changing nothing; a run without pids.json is an error
// for both.
func TestRunPIDs(t *testing.T) {
	dir := t.TempDir()
	procs := []runlog.Proc{
		{Role: runlog.RoleValkey, PID: 1001},
		{Role: runlog.RoleHitAPI, Instance: 1, PID: 1002},
		{Role: runlog.RoleHitAPI, Instance: 3, PID: 1003},
		{Role: runlog.RoleSampler, PID: 1004},
	}
	writePIDs(t, dir, procs...)
	for _, c := range []struct {
		role     string
		instance int
		want     int // 0: an error
	}{
		{runlog.RoleValkey, 0, 1001},
		{runlog.RoleHitAPI, 1, 1002},
		{runlog.RoleHitAPI, 3, 1003},
		{runlog.RoleHitAPI, 2, 0},
		{runlog.RoleHitgen, 0, 0},
		{runlog.RoleValkey, 1, 0},
	} {
		got, err := procPID(dir, c.role, c.instance)
		if (err != nil) != (c.want == 0) || got != c.want {
			t.Errorf("procPID(%s, %d) = %d, %v; want %d", c.role, c.instance, got, err, c.want)
		}
	}

	if err := setPID(dir, runlog.RoleHitAPI, 3, 2003); err != nil {
		t.Fatal(err)
	}
	if err := setPID(dir, runlog.RoleValkey, 0, 2001); err != nil {
		t.Fatal(err)
	}
	checkPIDs(t, dir,
		runlog.Proc{Role: runlog.RoleValkey, PID: 2001},
		runlog.Proc{Role: runlog.RoleHitAPI, Instance: 1, PID: 1002},
		runlog.Proc{Role: runlog.RoleHitAPI, Instance: 3, PID: 2003},
		runlog.Proc{Role: runlog.RoleSampler, PID: 1004})
	before, err := os.ReadFile(filepath.Join(dir, runlog.PIDsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		role     string
		instance int
	}{{runlog.RoleHitAPI, 2}, {runlog.RoleHitgen, 0}} {
		if err := setPID(dir, c.role, c.instance, 3000); err == nil {
			t.Errorf("setPID(%s, %d) of a process pids.json does not name succeeded, want an error", c.role, c.instance)
		}
	}
	if after, err := os.ReadFile(filepath.Join(dir, runlog.PIDsFile)); err != nil || !bytes.Equal(after, before) {
		t.Errorf("pids.json after the refused setPID calls:\n%s (%v)\nwant it unchanged:\n%s", after, err, before)
	}
	empty := t.TempDir()
	if _, err := procPID(empty, runlog.RoleValkey, 0); err == nil {
		t.Error("procPID without pids.json succeeded, want an error")
	}
	if err := setPID(empty, runlog.RoleValkey, 0, 2001); err == nil {
		t.Error("setPID without pids.json succeeded, want an error")
	}
	if _, err := os.Stat(filepath.Join(empty, runlog.PIDsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("setPID without pids.json made one (%v), want none", err)
	}
}

// TestStoreFaults (IMPLEMENTATION_PLAN H4 step 3; DESIGN §B5 "Faults", §B6 FLT-1 (a), FLT-2):
// store down and store pause through faultctl's run, on a durable store started with the
// orchestrator's exact command line (startRunStore), in a standalone stack's run directory:
// config.toml and pids.json, no context.json. store down 1s, on the store grown to the §A7 size
// (storeproc.PaddingA7, as TestStoreCtlDownAndPause's down_padded), prints outage_ns of more
// than 1,000 ms, at most 5,000 ms, that holds the 1 s and the AOF load the store's log shows
// (checkLoadInOutage), and the store comes back as a new process on the same port with the
// dir, aclfile and auto-aof-rewrite-percentage it ran with (0, or the profile's 100 when the
// scenario forces rewrites); pids.json names that process, its other entries unchanged. store
// pause 1s prints outage_ns of more than 1,000 ms, at most 5,000 ms, and leaves the same
// process running, pids.json unchanged. Each line's start_ns and end_ns are the command's own
// times, the first and last readings of the clock run gets (a paddedClock, which waits 100 ms
// around each reading), and the outage, outage_ns long from outage_start_ns, lies inside them,
// at least those 100 ms from each end (checkOutageLine, checkOutageTimes).
func TestStoreFaults(t *testing.T) {
	for _, c := range []struct {
		name    string
		forced  bool
		rewrite string // the store's auto-aof-rewrite-percentage: StoreArgs' 0, or the durable profile's 100
	}{
		{"down", false, "0"},
		{"down_forced_rewrites", true, "100"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := newRunDir(t)
			st := startRunStore(t, dir, c.forced)
			// The store grows to the §A7 size, so the restarted store's AOF load takes long enough
			// for the store's log (the run's valkey-server.stderr, which the restart appends to;
			// offset is its size before the fault) to time it.
			if err := storeproc.LoadPaddingAt(st.addr, st.pw.Fixture, storeproc.PaddingA7); err != nil {
				t.Fatal(err)
			}
			writeConfig(t, dir, st.addr, nil)
			writePIDs(t, dir, runlog.Proc{Role: runlog.RoleValkey, PID: st.pid}, runlog.Proc{Role: runlog.RoleSampler, PID: os.Getpid()})
			before := st.settings(t)
			if before["port"] != strconv.Itoa(st.port) || before["auto-aof-rewrite-percentage"] != c.rewrite || before["process_id"] != strconv.Itoa(st.pid) {
				t.Fatalf("the store started as %v, want port %d, auto-aof-rewrite-percentage %s and process %d", before, st.port, c.rewrite, st.pid)
			}
			reapChildStores(t, st)
			logPath := filepath.Join(dir, runlog.RoleValkey+outputSuffix)
			offset := fileSize(t, logPath)
			clk := &paddedClock{pad: clockPad}
			ran := time.Now()
			code, out, errOut := runCLIClock(t, []string{"--run", dir, "store", "down", "1s"}, map[string]string{"RL_OPS_PASSWORD": st.pw.Ops},
				[]string{"PATH=/usr/bin:/bin"}, clk.now)
			adj := clockAdjustment(ran, time.Now())
			if code != 0 {
				t.Fatalf("faultctl store down 1s: exit %d, stdout %q, stderr %s; want 0", code, out, errOut)
			}
			after := st.settings(t)
			newPID, err := strconv.Atoi(after["process_id"])
			if err != nil {
				t.Fatalf("INFO server process_id %q after store down: %v", after["process_id"], err)
			}
			adoptRestarted(t, newPID, strings.Join(st.args, " "))
			got := checkOutageLine(t, out, "store down 1s", int64(faultFor), maxOutageNs, false, adj)
			clk.checkOutageTimes(t, "store down 1s", got, adj)
			checkLoadInOutage(t, "store down 1s", logPath, offset, faultFor, got.outage, adj)
			if newPID == st.pid {
				t.Errorf("the store at %s is still process %d: store down never restarted it", st.addr, st.pid)
			}
			if !st.exited() {
				t.Errorf("the store's first process %d still runs after store down", st.pid)
			}
			for _, k := range []string{"port", "dir", "aclfile", "auto-aof-rewrite-percentage"} {
				if after[k] != before[k] {
					t.Errorf("the restarted store runs with %s %q, want %q, the setting it ran with before store down", k, after[k], before[k])
				}
			}
			checkPIDs(t, dir, runlog.Proc{Role: runlog.RoleValkey, PID: newPID}, runlog.Proc{Role: runlog.RoleSampler, PID: os.Getpid()})
		})
	}

	t.Run("pause", func(t *testing.T) {
		dir := newRunDir(t)
		st := startRunStore(t, dir, false)
		writeConfig(t, dir, st.addr, nil)
		procs := []runlog.Proc{{Role: runlog.RoleValkey, PID: st.pid}, {Role: runlog.RoleSampler, PID: os.Getpid()}}
		writePIDs(t, dir, procs...)
		clk := &paddedClock{pad: clockPad}
		before := time.Now()
		code, out, errOut := runCLIClock(t, []string{"--run", dir, "store", "pause", "1s"}, map[string]string{"RL_OPS_PASSWORD": st.pw.Ops},
			[]string{"PATH=/usr/bin:/bin"}, clk.now)
		adj := clockAdjustment(before, time.Now())
		if code != 0 {
			t.Fatalf("faultctl store pause 1s: exit %d, stdout %q, stderr %s; want 0", code, out, errOut)
		}
		clk.checkOutageTimes(t, "store pause 1s", checkOutageLine(t, out, "store pause 1s", int64(faultFor), maxOutageNs, false, adj), adj)
		waitState(t, st.pid, false)
		if got := st.settings(t)["process_id"]; got != strconv.Itoa(st.pid) || st.exited() {
			t.Errorf("after store pause the store at %s is process %s, want the same process %d", st.addr, got, st.pid)
		}
		checkPIDs(t, dir, procs...)
	})
}

// fakeAPI serves an instance's /_test/prekill and /health (httptest): the pre-kill reply is
// prekill with status 200 or, with prekill "", never sent (the call is held until its client
// gives up or the test ends); /health answers status ready. It records the pre-kill calls,
// whether the instance's process still ran at the first, and how long a held call was held.
type fakeAPI struct {
	srv *httptest.Server

	mu             sync.Mutex
	proc           *child // the instance's process, once started
	prekills       int
	aliveAtPrekill bool
	held           time.Duration
	healthGets     int
}

func newFakeAPI(t *testing.T, prekill string) *fakeAPI {
	t.Helper()
	a := &fakeAPI{}
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/_test/prekill", func(w http.ResponseWriter, r *http.Request) {
		arrived := time.Now()
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		a.mu.Lock()
		a.prekills++
		if a.prekills == 1 {
			a.aliveAtPrekill = a.proc != nil && !a.proc.exited()
		}
		a.mu.Unlock()
		if prekill == "" {
			select {
			case <-r.Context().Done(): // the client gave up and closed the connection
			case <-release:
			}
			a.mu.Lock()
			a.held = time.Since(arrived)
			a.mu.Unlock()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, prekill) // a failed write shows as a failed pre-kill read
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.healthGets++
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready"}`) // a failed write shows as an instance not ready
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: Close waits for a held call to return
	return a
}

// addr is the server's host:port, an instance's --listen.
func (a *fakeAPI) addr() string { return a.srv.Listener.Addr().String() }

// setProc names the instance's process, once started.
func (a *fakeAPI) setProc(c *child) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.proc = c
}

// counts returns the pre-kill calls, whether the process ran at the first, the time a held
// call was held (0 until it ended) and the /health reads.
func (a *fakeAPI) counts() (prekills int, aliveAtPrekill bool, held time.Duration, healthGets int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prekills, a.aliveAtPrekill, a.held, a.healthGets
}

// standInEnv is the environment of a hitapi stand-in that appends its line to lines.
func standInEnv(lines string) []string {
	return []string{"PATH=/usr/bin:/bin", helperEnv + "=" + helperHitAPI, helperLinesEnv + "=" + lines, helperLibEnv + "=" + libPassword}
}

// startStandIn starts this test binary as a hitapi stand-in (TestMain), through a link to it
// named hitapi, with args and env as its whole environment (standInEnv(lines)), as a child the
// test owns, and returns once it has written its line to lines.
func startStandIn(t *testing.T, args []string, lines string) *child {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), runlog.RoleHitAPI)
	if err := os.Symlink(exe, bin); err != nil {
		t.Fatal(err)
	}
	n := len(readLines(t, lines))
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), bin, args...)
	cmd.Env = standInEnv(lines)
	c := startChild(t, cmd)
	got := waitLines(t, lines, n+1)
	if len(got) != n+1 || got[n].PID != c.pid {
		t.Fatalf("the hitapi stand-in %d wrote no line of its own within %v (lines %+v)", c.pid, procWait, got)
	}
	return c
}

// writeAPILog writes instance 2's run log as it stands before the kill and returns its path.
func writeAPILog(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "api-2.jsonl")
	if err := os.WriteFile(path, []byte(logBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkAPILog checks that the run log holds its earlier lines, then exactly the line want.
func checkAPILog(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != logBefore+want+"\n" {
		t.Errorf("the run log after api kill:\n%s\nwant its earlier lines, then:\n%s", got, want)
	}
}

// TestAPIKill (IMPLEMENTATION_PLAN H4 steps 2 and 3; DESIGN §B6 FLT-6): api kill through
// faultctl's run, on a hitapi stand-in (this test binary run as hitapi, TestMain) that
// pids.json names as instance 2, with an httptest server for its /_test/prekill and /health at
// its --listen. api kill 2 --restart calls the pre-kill route while the process runs, appends
// the pre_kill line with the reply's drop counts and read_ok:true to its --log, SIGKILLs it,
// runs its command line again with RL_LIB_PASSWORD (the value faultctl was given) as its only
// RL_ variable, waits for /health ready, and pids.json names the restarted process, its other
// entries unchanged. A pre-kill call never answered is cut at P + 2 s (P from the instance's
// --config, here 200 ms): the line says read_ok:false, the process is still killed, and the
// command fails (exit 1, error set).
func TestAPIKill(t *testing.T) {
	t.Run("restart", func(t *testing.T) {
		dir := writeRun(t, "127.0.0.1:1")
		api := newFakeAPI(t, `{"listener_drops":3,"log_drops":2,"log_queue_len":0}`)
		logPath := writeAPILog(t, dir)
		lines := filepath.Join(t.TempDir(), "stand-in.lines")
		proc := startStandIn(t, []string{"--config", filepath.Join(dir, "config.toml"), "--listen", api.addr(), "--log", logPath, "--instance", "2"}, lines)
		first := readLines(t, lines)[0]
		if len(first.RL) != 0 || first.Lib {
			t.Fatalf("the stand-in started with RL_ variables %q (lib %v), want none", first.RL, first.Lib)
		}
		api.setProc(proc)
		writePIDs(t, dir, runlog.Proc{Role: runlog.RoleHitAPI, Instance: 2, PID: proc.pid}, runlog.Proc{Role: runlog.RoleSampler, PID: os.Getpid()})
		environ := append(standInEnv(lines), "RL_OPS_PASSWORD=ops-not-for-hitapi", "RL_FIXTURE_PASSWORD=fixture-not-for-hitapi")
		code, out, errOut := runCLIWith(t, []string{"--run", dir, "api", "kill", "2", "--restart"}, map[string]string{config.LibPasswordEnv: libPassword}, environ)
		got := waitLines(t, lines, 2)
		for _, l := range got[1:] {
			adoptRestarted(t, l.PID, strings.Join(l.Argv, " "))
		}
		if code != 0 {
			t.Fatalf("faultctl api kill 2 --restart: exit %d, stdout %q, stderr %s; want 0", code, out, errOut)
		}
		checkLine(t, out, "api kill 2 --restart")
		checkAPILog(t, logPath, `{"kind":"lifecycle","event":"pre_kill","listener_drops":3,"log_drops":2,"read_ok":true}`)
		checkKilled(t, proc)
		prekills, alive, _, healthGets := api.counts()
		if prekills != 1 || !alive {
			t.Errorf("%d pre-kill calls, the process running at the first: %v; want 1, while it ran", prekills, alive)
		}
		if healthGets < 1 {
			t.Error("no /health read: api kill --restart did not wait for the restarted instance to be ready")
		}
		if len(got) != 2 {
			t.Fatalf("stand-in lines %+v, want 2: the first process's and the restarted one's", got)
		}
		re := got[1]
		if re.PID == proc.pid {
			t.Fatalf("the restarted stand-in has the killed process's PID %d", re.PID)
		}
		if !slices.Equal(re.Argv, first.Argv) {
			t.Errorf("the restarted instance's command line %q, want the killed one's %q", re.Argv, first.Argv)
		}
		if !slices.Equal(re.RL, []string{config.LibPasswordEnv}) || !re.Lib {
			t.Errorf("the restarted instance's RL_ variables %q, RL_LIB_PASSWORD the one faultctl got: %v; want %s alone, with that value",
				re.RL, re.Lib, config.LibPasswordEnv)
		}
		checkPIDs(t, dir, runlog.Proc{Role: runlog.RoleHitAPI, Instance: 2, PID: re.PID}, runlog.Proc{Role: runlog.RoleSampler, PID: os.Getpid()})
	})

	t.Run("prekill_cut_at_p_plus_2s", func(t *testing.T) {
		const p = 200 * time.Millisecond
		dir := writeRun(t, "127.0.0.1:1")
		cfg := writeConfigAs(t, filepath.Join(dir, "config-2.toml"), "127.0.0.1:1", func(f *config.File) { f.Instance.MaxReleaseDelayNs = int64(p) })
		api := newFakeAPI(t, "")
		logPath := writeAPILog(t, dir)
		lines := filepath.Join(t.TempDir(), "stand-in.lines")
		proc := startStandIn(t, []string{"--config", cfg, "--listen", api.addr(), "--log", logPath, "--instance", "2"}, lines)
		api.setProc(proc)
		procs := []runlog.Proc{{Role: runlog.RoleHitAPI, Instance: 2, PID: proc.pid}}
		writePIDs(t, dir, procs...)
		code, out, errOut := runCLI(t, []string{"--run", dir, "api", "kill", "2"}, nil)
		if code != 1 {
			t.Errorf("faultctl api kill 2 with the pre-kill call unanswered: exit %d, stderr %s; want 1", code, errOut)
		}
		checkErrorLine(t, out, "api kill 2")
		checkAPILog(t, logPath, `{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":0,"read_ok":false}`)
		checkKilled(t, proc)
		var held time.Duration
		for limit := time.Now().Add(procWait); held == 0 && time.Now().Before(limit); time.Sleep(10 * time.Millisecond) {
			_, _, held, _ = api.counts()
		}
		// The server sees the call end a little after the client's P + 2 s deadline, minus the
		// time the request took to arrive: 100 ms of slack below, 1 s above.
		if lo, hi := p+2*time.Second-100*time.Millisecond, p+3*time.Second; held < lo || held > hi {
			t.Errorf("the unanswered pre-kill call was held %v, want P + 2 s = %v (%v to %v)", held, p+2*time.Second, lo, hi)
		}
		checkPIDs(t, dir, procs...)
	})
}

// healthServer serves an instance's GET /health with a body and status the test sets, and
// counts the reads.
type healthServer struct {
	srv *httptest.Server

	mu     sync.Mutex
	status int
	body   string
	gets   int
}

func newHealthServer(t *testing.T) *healthServer {
	t.Helper()
	h := &healthServer{status: http.StatusOK, body: "{}"}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		status, body := h.status, h.body
		if r.URL.Path == "/health" && r.Method == http.MethodGet {
			h.gets++
		} else {
			status, body = http.StatusNotFound, `{"error":"not_found"}`
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body) // a failed write shows as a failed read
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *healthServer) set(status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.body = status, body
}

func (h *healthServer) reads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gets
}

// TestTamperEpochFromHealth (IMPLEMENTATION_PLAN H4 step 3; DESIGN §B6 FLT-7 `faultctl tamper
// <op> --key K`; fake store): a tamper operation on the epoch's namespace without --epoch uses
// the epoch_id the run's instance serves on GET /health, the instance found through pids.json
// and its --listen (a hitapi stand-in); --epoch overrides it, with no /health read, and an
// epoch-wide operation reads none either. An instance that serves no usable epoch_id (empty,
// not 32 lowercase hex characters, absent, or not a 200 JSON reply) refuses the command: exit
// 2, nothing sent to the store.
func TestTamperEpochFromHealth(t *testing.T) {
	fake := fakeresp.Start(t, userFixture, testPassword)
	dir := writeRun(t, fake.Addr())
	h := newHealthServer(t)
	lines := filepath.Join(t.TempDir(), "stand-in.lines")
	proc := startStandIn(t, []string{"--config", filepath.Join(dir, "config.toml"), "--listen", h.srv.Listener.Addr().String(),
		"--log", writeAPILog(t, dir), "--instance", "2"}, lines)
	writePIDs(t, dir, runlog.Proc{Role: runlog.RoleHitAPI, Instance: 2, PID: proc.pid})
	env := map[string]string{"RL_FIXTURE_PASSWORD": testPassword}
	const (
		served = "00112233445566778899aabbccddeeff"
		given  = "ffeeddccbbaa99887766554433221100"
	)
	ready := `{"status":"ready","reason":"ready","epoch_id":"` + served + `"}`
	one := resp.Value{Kind: resp.Int, Int: 1}
	for _, c := range []struct {
		args  []string
		label string // the command the store answers with 1
		want  [][]string
		reads int // /health reads
	}{
		{[]string{"del-k", "--key", "k1"}, "DEL", [][]string{{"DEL", "rl:e:" + served + ":k:azE"}}, 1},
		{[]string{"del-ix", "--key", "k1"}, "ZREM", [][]string{{"ZREM", "rl:e:" + served + ":ix", "azE"}}, 1},
		{[]string{"del-k", "--key", "k1", "--epoch", given}, "DEL", [][]string{{"DEL", "rl:e:" + given + ":k:azE"}}, 0},
		{[]string{"del-hwm"}, "HDEL", [][]string{{"HDEL", "rl:epoch", "hwm"}}, 0},
	} {
		h.set(http.StatusOK, ready)
		fake.ReplyValue(c.label, one, 1)
		n, reads := len(fake.Commands()), h.reads()
		code, out, errOut := runCLI(t, append([]string{"--run", dir, "tamper"}, c.args...), env)
		if code != 0 {
			t.Errorf("faultctl tamper %s: exit %d, stderr %s; want 0", strings.Join(c.args, " "), code, errOut)
			continue
		}
		checkLine(t, out, "tamper "+strings.Join(c.args, " "))
		if got := sentSince(t, fake, n, "tamper "+c.args[0]); !slices.EqualFunc(got, c.want, slices.Equal) {
			t.Errorf("faultctl tamper %s sent %q, want %q", strings.Join(c.args, " "), got, c.want)
		}
		if got := h.reads() - reads; got != c.reads {
			t.Errorf("faultctl tamper %s read /health %d times, want %d", strings.Join(c.args, " "), got, c.reads)
		}
	}

	accepted := fake.Accepted()
	for _, c := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"status":"not_ready","reason":"no_epoch","epoch_id":""}`},
		{http.StatusOK, `{"status":"ready","epoch_id":"0123"}`},
		{http.StatusOK, `{"status":"ready","epoch_id":"00112233445566778899AABBCCDDEEFF"}`},
		{http.StatusOK, `{"status":"ready"}`},
		{http.StatusOK, `not json`},
		{http.StatusServiceUnavailable, ready},
	} {
		h.set(c.status, c.body)
		code, out, _ := runCLI(t, []string{"--run", dir, "tamper", "del-k", "--key", "k1"}, env)
		if code != 2 || out != "" {
			t.Errorf("faultctl tamper del-k --key k1 with /health %d %s: exit %d, stdout %q; want exit 2 and no line", c.status, c.body, code, out)
		}
	}
	if n := fake.Accepted(); n != accepted {
		t.Errorf("the refused tamper commands opened %d store connections, want none", n-accepted)
	}
}

// TestRefusesUnderOrchestrator (IMPLEMENTATION_PLAN H4 step 3, plan choice; DESIGN §B4, §B5
// "Faults"): every command changes the store or a process, so each one, store down, store
// pause, api kill with and without --restart, tamper (an epoch-wide and a per-key operation)
// and clock set, advance and back, exits 2 with nothing done (no line, no store connection)
// and points to [[faults]], which context.json records, while the run's orchestrator is live:
// .data/orchestrator.pid in the repository root (the run directory's grandparent) names a live
// process. A standalone stack's run directory, which has no context.json, refuses nothing when
// there is no pid file, or one naming no live process or holding no PID: the store and api
// commands fail (exit 1 with their line and its error, no store connection: the run has no
// pids.json), and the tamper and clock commands run (exit 0, their commands sent to the fake
// store); and store pause 1s, in a standalone run directory whose config.toml and pids.json
// name a durable store (startRunStore), pauses that store: exit 0, its line with outage_ns of
// more than 1,000 ms (checkOutageLine), the same process answering afterwards. The usage says
// faultctl is for stacks without an orchestrator and every command is refused while the run's
// orchestrator is live.
func TestRefusesUnderOrchestrator(t *testing.T) {
	fake := fakeresp.Start(t, userFixture, testPassword)
	dir := writeRun(t, fake.Addr())
	pidFile := filepath.Join(filepath.Dir(filepath.Dir(dir)), ".data", "orchestrator.pid")
	env := map[string]string{"RL_OPS_PASSWORD": opsPassword, "RL_FIXTURE_PASSWORD": testPassword, config.LibPasswordEnv: libPassword}
	const epoch = "0123456789abcdef0123456789abcdef"
	type reply struct {
		label string
		v     resp.Value
	}
	ok := resp.Value{Kind: resp.Simple, Str: "OK"}
	stored := reply{"GET", resp.Value{Kind: resp.Bulk, Str: "5"}} // rl:ctl:time, µs
	cmds := []struct {
		args    []string
		replies []reply    // the fake store's replies to the commands it sends when it runs
		sent    [][]string // what it sends when it runs; nil: it fails before reaching the store
	}{
		{args: []string{"store", "down", "1s"}},
		{args: []string{"store", "pause", "1s"}},
		{args: []string{"api", "kill", "1", "--restart"}},
		{args: []string{"api", "kill", "1"}},
		{args: []string{"tamper", "wipe"}, replies: []reply{{"FLUSHALL", ok}}, sent: [][]string{{"FLUSHALL"}}},
		{args: []string{"tamper", "del-k", "--key", "k1", "--epoch", epoch}, replies: []reply{{"DEL", resp.Value{Kind: resp.Int, Int: 1}}},
			sent: [][]string{{"DEL", "rl:e:" + epoch + ":k:azE"}}},
		{args: []string{"clock", "set", "1000"}, replies: []reply{{"SET", ok}}, sent: [][]string{{"SET", "rl:ctl:time", "1"}}},
		{args: []string{"clock", "advance", "1000"}, replies: []reply{stored, {"SET", ok}},
			sent: [][]string{{"GET", "rl:ctl:time"}, {"SET", "rl:ctl:time", "6"}}},
		{args: []string{"clock", "back", "1000"}, replies: []reply{stored, {"SET", ok}},
			sent: [][]string{{"GET", "rl:ctl:time"}, {"SET", "rl:ctl:time", "4"}}},
	}
	check := func(state string, refused bool) {
		t.Helper()
		for _, c := range cmds {
			line := strings.Join(c.args, " ")
			if !refused {
				for _, r := range c.replies {
					fake.ReplyValue(r.label, r.v, 1)
				}
			}
			n, accepted := len(fake.Commands()), fake.Accepted()
			code, out, errOut := runCLI(t, append([]string{"--run", dir}, c.args...), env)
			conns := fake.Accepted() - accepted
			switch {
			case refused:
				if code != 2 || out != "" || conns != 0 || !strings.Contains(errOut, "[[faults]]") {
					t.Errorf("%s: faultctl %s: exit %d, stdout %q, %d store connections, stderr %s; want exit 2, no line, none, and [[faults]] named",
						state, line, code, out, conns, errOut)
				}
			case c.sent == nil:
				if code != 1 || conns != 0 {
					t.Errorf("%s: faultctl %s: exit %d, %d store connections, stderr %s; want 1 and none (not refused: the run has no pids.json)",
						state, line, code, conns, errOut)
				}
				checkErrorLine(t, out, line)
			default:
				if code != 0 {
					t.Errorf("%s: faultctl %s: exit %d, stderr %s; want 0 (not refused)", state, line, code, errOut)
				}
				checkLine(t, out, line)
				if got := sentSince(t, fake, n, line); !slices.EqualFunc(got, c.sent, slices.Equal) {
					t.Errorf("%s: faultctl %s sent %q, want %q", state, line, got, c.sent)
				}
			}
		}
	}
	check("a standalone run (no context.json), no pid file", false)

	// store pause on a standalone stack's own store: a run directory whose config.toml and
	// pids.json name a durable store, with no context.json and no orchestrator.
	sdir := newRunDir(t)
	st := startRunStore(t, sdir, false)
	writeConfig(t, sdir, st.addr, nil)
	writePIDs(t, sdir, runlog.Proc{Role: runlog.RoleValkey, PID: st.pid})
	before := time.Now()
	code, out, errOut := runCLI(t, []string{"--run", sdir, "store", "pause", "1s"}, map[string]string{"RL_OPS_PASSWORD": st.pw.Ops})
	adj := clockAdjustment(before, time.Now())
	if code != 0 {
		t.Fatalf("a standalone run with its store, no pid file: faultctl store pause 1s: exit %d, stdout %q, stderr %s; want 0 (not refused)", code, out, errOut)
	}
	checkOutageLine(t, out, "store pause 1s", int64(faultFor), maxOutageNs, false, adj)
	if got := st.settings(t)["process_id"]; got != strconv.Itoa(st.pid) || st.exited() {
		t.Errorf("after store pause the store at %s is process %s, want the same process %d", st.addr, got, st.pid)
	}

	if err := os.MkdirAll(filepath.Dir(pidFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		content string
		refused bool
	}{
		{strconv.Itoa(os.Getpid()) + "\n", true}, // this test process: live
		{"2147483647\n", false},                  // math.MaxInt32: no such process
		{"not a pid\n", false},
	} {
		if err := os.WriteFile(pidFile, []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprintf("a standalone run (no context.json), .data/orchestrator.pid %q", c.content), c.refused)
	}
	code, _, usage := runCLI(t, []string{"-h"}, nil)
	for _, want := range []string{"faultctl is for stacks without an orchestrator: every command changes the store or a process",
		"every command is refused while the run's orchestrator is"} {
		if code != 0 || !strings.Contains(usage, want) {
			t.Errorf("faultctl -h: exit %d, usage %q; want 0 and %q", code, usage, want)
		}
	}
}

// TestStopSignals (DESIGN §B6; IMPLEMENTATION_PLAN H4 step 3): faultctl, run as its own process
// (this test binary running faultctl's main, TestMain), ends store pause 30s early on SIGHUP (a
// closed terminal) and on SIGQUIT as it does on SIGTERM: the store gets SIGCONT (ps state no
// longer T) and answers again, and faultctl exits 1 with its line, outage_start_ns, outage_ns
// and error set (the pause was cut short; checkOutageLine). It does the same on a SIGQUIT when
// started with SIGQUIT ignored, under sh -c 'trap "" QUIT; exec "$0" "$@"' (quitIgnoringShell,
// which first shows a shell it starts surviving its own SIGQUIT): Go's runtime replaces that
// ignore with its own handler. As cmd/orchestrator's, stopSignals always catches SIGTERM and
// SIGQUIT and leaves out only an ignored SIGINT or SIGHUP (the rows; signal.Ignored never
// reports SIGQUIT). A SIGHUP ignored at startup stays ignored: faultctl started under
// /usr/bin/nohup (SIGHUP ignored) runs store pause 2s to its end through a SIGHUP: exit 0, its
// line without an error, outage_ns of more than 2,000 ms, at most 5,000 ms, the store
// answering PING again.
func TestStopSignals(t *testing.T) {
	for _, c := range []struct {
		ignored []os.Signal
		want    []os.Signal
	}{
		{nil, []os.Signal{syscall.SIGTERM, syscall.SIGQUIT, os.Interrupt, syscall.SIGHUP}},
		{[]os.Signal{syscall.SIGHUP}, []os.Signal{syscall.SIGTERM, syscall.SIGQUIT, os.Interrupt}},
		{[]os.Signal{os.Interrupt}, []os.Signal{syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP}},
		{[]os.Signal{os.Interrupt, syscall.SIGHUP}, []os.Signal{syscall.SIGTERM, syscall.SIGQUIT}},
		{[]os.Signal{syscall.SIGTERM}, []os.Signal{syscall.SIGTERM, syscall.SIGQUIT, os.Interrupt, syscall.SIGHUP}},
	} {
		if got := stopSignals(func(sig os.Signal) bool { return slices.Contains(c.ignored, sig) }); !slices.Equal(got, c.want) {
			t.Errorf("stopSignals with %v ignored at startup = %v, want %v", c.ignored, got, c.want)
		}
	}

	// The wrapper's control: a shell it starts survives a SIGQUIT it sends itself, which only an
	// ignored SIGQUIT allows (a shell that is not interactive neither ignores nor traps it).
	ctx, cancel := context.WithTimeout(t.Context(), procWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c", quitIgnoringShell, "/bin/sh", "-c", `kill -QUIT $$ && echo survived`).Output()
	if err != nil || string(out) != "survived\n" {
		t.Fatalf("a shell started through %q, after its own SIGQUIT: %v, stdout %q; want exit 0 and \"survived\": the wrapper does not start it with SIGQUIT ignored",
			quitIgnoringShell, err, out)
	}

	dir := newRunDir(t)
	st := startRunStore(t, dir, false)
	writeConfig(t, dir, st.addr, nil)
	writePIDs(t, dir, runlog.Proc{Role: runlog.RoleValkey, PID: st.pid})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Each child must start with SIGHUP at its default action, whatever this process inherited
	// (go test under nohup ignores it, and an ignored signal stays ignored across exec): while
	// this process catches SIGHUP, exec gives a child the default action. SIGQUIT needs no such
	// step: Go's runtime always installs its own handler for it, which exec resets as well.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for _, c := range []struct {
		name string
		wrap []string // what starts faultctl: nothing, or quitIgnoringShell
		sig  syscall.Signal
	}{
		{"SIGHUP", nil, syscall.SIGHUP},
		{"SIGQUIT", nil, syscall.SIGQUIT},
		{"SIGQUIT, faultctl started with SIGQUIT ignored", []string{"/bin/sh", "-c", quitIgnoringShell}, syscall.SIGQUIT},
	} {
		var stdout, stderr bytes.Buffer
		args := append(slices.Clone(c.wrap), exe, "--run", dir, "store", "pause", "30s")
		cmd := exec.CommandContext(context.WithoutCancel(t.Context()), args[0], args[1:]...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", helperEnv + "=" + helperFaultctl, "RL_OPS_PASSWORD=" + st.pw.Ops}
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		before := time.Now()
		fc := startChild(t, cmd)
		waitState(t, st.pid, true)
		signalled := time.Now()
		if err := syscall.Kill(fc.pid, c.sig); err != nil {
			t.Fatalf("%s: sending %v to faultctl: %v", c.name, c.sig, err)
		}
		state := fc.wait(t)
		adj := clockAdjustment(before, time.Now())
		if took := time.Since(signalled); state.ExitCode() != 1 || took > procWait {
			t.Fatalf("%s: faultctl store pause 30s after %v: %v after %v, stderr %s; want exit 1 at once", c.name, c.sig, state, took, stderr.String())
		}
		checkOutageLine(t, stdout.String(), "store pause 30s", 0, int64(30*time.Second), true, adj)
		waitState(t, st.pid, false)
		if r := st.do(t, "PING"); r.Kind != '+' || r.Str != "PONG" {
			t.Errorf("%s: PING after faultctl ended on %v: %s, want PONG", c.name, c.sig, r)
		}
	}

	// Under nohup, which ignores SIGHUP and execs faultctl in its own process, a hangup changes
	// nothing: the pause runs its 2 s.
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/usr/bin/nohup", exe, "--run", dir, "store", "pause", "2s")
	cmd.Env = []string{"PATH=/usr/bin:/bin", helperEnv + "=" + helperFaultctl, "RL_OPS_PASSWORD=" + st.pw.Ops}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	before := time.Now()
	fc := startChild(t, cmd)
	waitState(t, st.pid, true)
	if err := syscall.Kill(fc.pid, syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP to faultctl under nohup: %v", err)
	}
	state := fc.wait(t)
	adj := clockAdjustment(before, time.Now())
	if state.ExitCode() != 0 {
		t.Fatalf("faultctl store pause 2s under nohup after SIGHUP: %v, stderr %s; want exit 0, the pause run to its end", state, stderr.String())
	}
	checkOutageLine(t, stdout.String(), "store pause 2s", int64(2*time.Second), maxOutageNs, false, adj)
	waitState(t, st.pid, false)
	if r := st.do(t, "PING"); r.Kind != '+' || r.Str != "PONG" {
		t.Errorf("PING after the pause under nohup: %s, want PONG", r)
	}
}
