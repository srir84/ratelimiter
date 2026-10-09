//go:build rltest

package fixtures_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
	"ratelimiter/internal/testutil/storeproc"
)

// Store fault test values (IMPLEMENTATION_PLAN H4 tests): StoreDown(1s) measures an outage of
// 1,000–5,000 ms; StorePause(1s) at least 1,000 ms (and within the 5 s limit of §B6).
const (
	faultFor    = time.Second
	minOutageNs = int64(1_000) * int64(time.Millisecond)
	maxOutageNs = int64(5_000) * int64(time.Millisecond)
	// longFault is a fault the test cuts short by ending its ctx.
	longFault = 30 * time.Second
	// survivorKey is the key written before StoreDown, which the AOF must bring back.
	survivorKey   = "rl:test:storectl"
	survivorValue = "written-before-the-outage"
	// stateWait bounds a wait for ps to show a process stopped or running again, and for the
	// port to refuse connections.
	stateWait = 5 * time.Second
	// faultWait bounds a wait for a fault running in another goroutine to return.
	faultWait = 40 * time.Second
	// reapWait bounds the wait for a child the test stops to exit.
	reapWait = 10 * time.Second
	// logWait bounds the wait for the restarted store to log its AOF load in store.log.
	logWait = 10 * time.Second
	// logStamp is the time stamp of a Valkey log line, local time to the ms (truncated), as in
	// "1234:M 09 Oct 2026 05:43:15.596 * Background AOF rewrite finished successfully".
	logStamp = "02 Jan 2006 15:04:05.000"
	// logResolution is the resolution of store.log's time stamps and load times.
	logResolution = time.Millisecond
	// The store.log lines that place a StoreDown (Valkey 9.1): the rewrite before SHUTDOWN ended,
	// SHUTDOWN arrived, and the restarted store loaded its AOF ("…: 0.165 seconds").
	rewriteDoneLine = "Background AOF rewrite finished successfully"
	shutdownLine    = "User requested shutdown"
	loadLine        = "DB loaded from append only file: "
)

// faultResult is a store fault's return values, from the goroutine that ran it.
type faultResult struct {
	out fixtures.Outage
	err error
}

// storeTarget is the StoreTarget of a storeproc store: its command line, the rl_ops password,
// a PID file only the fault writes (fault.pid in a test directory, seeded with the store's
// PID; Valkey's own --pidfile, which a restarted store rewrites itself, stays in the store
// directory) and, unless fromArgs, its Restart, so the store keeps one owner and one
// .out/procs record (plan H4 step 1).
func storeTarget(t *testing.T, st *storeproc.Store, fromArgs bool) fixtures.StoreTarget {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "fault.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(st.PID)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tg := fixtures.StoreTarget{
		Addr: st.Addr, OpsUser: "rl_ops", OpsPassword: st.PW.Ops, Args: st.Args, PIDFile: pidFile,
	}
	if !fromArgs {
		tg.Restart = func(context.Context) error { return st.Restart(false) }
	}
	return tg
}

// do runs one command on a fresh connection as user and returns the reply.
func do(t *testing.T, st *storeproc.Store, user string, args ...string) storeproc.Reply {
	t.Helper()
	c, err := st.Conn(user)
	if err != nil {
		t.Fatalf("connecting as %s: %v", user, err)
	}
	defer func() { _ = c.Close() }() // one command, read in full
	r, err := c.Do(args...)
	if err != nil {
		t.Fatalf("%s as %s: %v", args[0], user, err)
	}
	return r
}

// writeSurvivor writes survivorKey as rl_fixture.
func writeSurvivor(t *testing.T, st *storeproc.Store) {
	t.Helper()
	if r := do(t, st, "rl_fixture", "SET", survivorKey, survivorValue); r.Kind != '+' || r.Str != "OK" {
		t.Fatalf("SET %s: %s", survivorKey, r)
	}
}

// checkSurvivor checks that survivorKey still holds its value.
func checkSurvivor(t *testing.T, st *storeproc.Store) {
	t.Helper()
	if r := do(t, st, "rl_fixture", "GET", survivorKey); r.Kind != '$' || r.Nil || r.Str != survivorValue {
		t.Errorf("GET %s after the outage: %s, want %q (the AOF lost it)", survivorKey, r, survivorValue)
	}
}

// pidInFile reads the PID in the file at path.
func pidInFile(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the PID file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("the PID file %s holds %q: %v", path, b, err)
	}
	return pid
}

// checkPIDFile checks that the fault's PID file names the store answering at st.Addr: INFO
// server's process_id, read as rl_ops.
func checkPIDFile(t *testing.T, st *storeproc.Store, path string) {
	t.Helper()
	running := storeproc.ParseInfo(do(t, st, "rl_ops", "INFO", "server").Str)["process_id"]
	if got := pidInFile(t, path); strconv.Itoa(got) != running {
		t.Errorf("the PID file %s names process %d, want the store at %s: INFO server process_id %q", path, got, st.Addr, running)
	}
}

// fileSize returns the size of the file at path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// aofBase returns the base file the store's AOF manifest names, from its line "file <name> seq
// <n> type b" (appendonlydir/appendonly.aof.manifest: Valkey's default appenddirname and
// appendfilename, which the profiles keep). Each finished rewrite makes a new base.
func aofBase(t *testing.T, st *storeproc.Store) string {
	t.Helper()
	path := filepath.Join(st.Dir, "appendonlydir", "appendonly.aof.manifest")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 6 && f[0] == "file" && f[2] == "seq" && f[4] == "type" && f[5] == "b" {
			return f[1]
		}
	}
	t.Fatalf("%s names no base file:\n%s", path, b)
	return ""
}

// storeLogEvents is what store.log shows of one StoreDown: when the last AOF rewrite before
// SHUTDOWN finished, when the store logged SHUTDOWN, and how long the restarted store took to
// load its AOF.
type storeLogEvents struct {
	rewriteDone, shutdown time.Time
	load                  time.Duration
}

// readStoreLog reads store.log from offset (the lines of one StoreDown) until the restarted
// store has logged its AOF load, at most logWait: a StoreDown that returned before the load
// ended would find no load line yet.
func readStoreLog(t *testing.T, path string, offset int64) storeLogEvents {
	t.Helper()
	limit := time.Now().Add(logWait)
	for {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(b)) < offset {
			t.Fatalf("%s has %d bytes, fewer than the %d before StoreDown", path, len(b), offset)
		}
		ev, loaded, err := parseStoreLog(string(b[offset:]))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if loaded {
			return ev
		}
		if !time.Now().Before(limit) {
			t.Fatalf("%s: no %q line after SHUTDOWN within %v", path, loadLine, logWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// parseStoreLog reads the whole lines of Valkey's log in text ("<pid>:<role> <logStamp> <level>
// <message>"): the last rewriteDoneLine before the first shutdownLine, and the first loadLine
// after it; loaded is false until that line is there. Lines of another form are left out.
func parseStoreLog(text string) (ev storeLogEvents, loaded bool, err error) {
	lines := strings.Split(text, "\n")
	lines = lines[:len(lines)-1] // the last one is empty, or a line still being written
	shutdownSeen := false
	for _, line := range lines {
		_, rest, ok := strings.Cut(line, " ")
		if !ok || len(rest) < len(logStamp)+3 {
			continue
		}
		at, perr := time.ParseInLocation(logStamp, rest[:len(logStamp)], time.Local)
		if perr != nil {
			continue
		}
		msg := rest[len(logStamp)+3:]
		switch {
		case !shutdownSeen && msg == rewriteDoneLine:
			ev.rewriteDone = at
		case !shutdownSeen && strings.HasPrefix(msg, shutdownLine):
			shutdownSeen, ev.shutdown = true, at
		case shutdownSeen && strings.HasPrefix(msg, loadLine):
			secs, ok := strings.CutSuffix(strings.TrimPrefix(msg, loadLine), " seconds")
			load, derr := time.ParseDuration(secs + "s")
			if !ok || derr != nil {
				return ev, false, fmt.Errorf("no load time in %q", line)
			}
			if ev.rewriteDone.IsZero() {
				return ev, false, fmt.Errorf("no %q line before SHUTDOWN: the AOF was not rewritten first", rewriteDoneLine)
			}
			ev.load = load
			return ev, true, nil
		}
	}
	return ev, false, nil
}

// psStat returns ps's state letters for pid ("" when ps finds no such process).
func psStat(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("ps -o stat= -p %d: %v", pid, err)
	}
	return strings.TrimSpace(string(out))
}

// waitStopped waits until ps shows pid stopped (state T) or, with stopped false, not stopped.
func waitStopped(t *testing.T, pid int, stopped bool) {
	t.Helper()
	limit := time.Now().Add(stateWait)
	for {
		st := psStat(t, pid)
		if strings.HasPrefix(st, "T") == stopped {
			return
		}
		if !time.Now().Before(limit) {
			t.Fatalf("process %d: ps state %q after %v, want stopped %v", pid, st, stateWait, stopped)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitRefused waits until a connection to addr is refused: the store is down.
func waitRefused(t *testing.T, addr string) {
	t.Helper()
	limit := time.Now().Add(stateWait)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		var d net.Dialer
		nc, err := d.DialContext(ctx, "tcp", addr)
		cancel()
		if errors.Is(err, syscall.ECONNREFUSED) {
			return
		}
		if err == nil {
			_ = nc.Close() // only the connect mattered
		}
		if !time.Now().Before(limit) {
			t.Fatalf("%s still accepted connections after %v (last: %v)", addr, stateWait, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// runFault runs fault in a goroutine the test owns (its cleanup waits for it) and returns the
// channel its result arrives on.
func runFault(t *testing.T, fault func() (fixtures.Outage, error)) <-chan faultResult {
	t.Helper()
	res := make(chan faultResult, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out, err := fault()
		res <- faultResult{out, err}
	}()
	t.Cleanup(wg.Wait)
	return res
}

// awaitFault waits for the fault's result, at most faultWait.
func awaitFault(t *testing.T, res <-chan faultResult) faultResult {
	t.Helper()
	timer := time.NewTimer(faultWait)
	defer timer.Stop()
	select {
	case r := <-res:
		return r
	case <-timer.C:
		t.Fatalf("the store fault did not return within %v", faultWait)
		return faultResult{}
	}
}

// recordProc writes the process record .out/procs/<pid> (bin/trace procs) for a child the
// test started outside storeproc, and returns the function that removes it once the child is
// gone (it also removes a record that failed half-written).
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

// reapDetached stops a released child of the test process (a fault restarted it from its
// command line): sig, up to reapWait for it to exit, then SIGKILL; each exit is reaped with
// wait4, since no exec.Cmd waits for it.
func reapDetached(t *testing.T, pid int, sig syscall.Signal) {
	t.Helper()
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("signalling process %d: %v", pid, err)
	}
	limit := time.Now().Add(reapWait)
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err != nil {
			t.Errorf("reaping process %d: %v", pid, err)
			return
		}
		if got == pid {
			return
		}
		if !time.Now().Before(limit) {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("killing process %d: %v", pid, err)
			}
			if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
				t.Errorf("reaping process %d: %v", pid, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkOutage checks an outage against [lo, hi] ns, and its wall-clock ends against each other.
func checkOutage(t *testing.T, what string, out fixtures.Outage, lo, hi int64) {
	t.Helper()
	t.Logf("%s: outage %v", what, time.Duration(out.OutageNs))
	if out.OutageNs < lo || out.OutageNs > hi {
		t.Errorf("%s: outage %d ns (%v), want %d to %d ns", what, out.OutageNs, time.Duration(out.OutageNs), lo, hi)
	}
	if out.StartNs <= 0 || out.EndNs <= out.StartNs {
		t.Errorf("%s: outage from %d to %d ns, want a wall-clock start before its end", what, out.StartNs, out.EndNs)
	}
}

// TestStoreCtlDownAndPause: StoreDown and StorePause against real stores (storeproc, durable
// profile; IMPLEMENTATION_PLAN H4 step 1). StoreDown(1s) measures an outage of 1,000–5,000 ms,
// restarts the store through its owner's Restart or from its command line, and a key written
// before it survives (AOF); the PID file, which only the fault writes, names the restarted
// store as INFO server shows it. On a store of the §A7 size, store.log places the outage: it
// starts after the AOF rewrite finished and before SHUTDOWN arrived, its length holds d and
// the AOF load, and the manifest names a new base file. StorePause(1s) blocks a PING written
// while the store is stopped until the pause ends and measures ≥ 1,000 ms; a PID file naming
// another process stops nothing. A ctx that ends during either fault cuts it short, and the
// store still comes back.
func TestStoreCtlDownAndPause(t *testing.T) {
	t.Run("down", func(t *testing.T) {
		st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Durable})
		writeSurvivor(t, st)
		oldPID, target := st.PID, storeTarget(t, st, false)
		out, err := fixtures.StoreDown(t.Context(), target, faultFor)
		if err != nil {
			t.Fatalf("StoreDown: %v", err)
		}
		checkOutage(t, "StoreDown(1s)", out, minOutageNs, maxOutageNs)
		if st.PID == oldPID {
			t.Errorf("the store still runs as process %d: StoreDown never restarted it", oldPID)
		}
		if got := pidInFile(t, target.PIDFile); got != st.PID {
			t.Errorf("the PID file names process %d, want the restarted store %d", got, st.PID)
		}
		checkPIDFile(t, st, target.PIDFile)
		checkSurvivor(t, st)
	})

	t.Run("down_from_args", func(t *testing.T) {
		st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Durable})
		writeSurvivor(t, st)
		oldPID, target := st.PID, storeTarget(t, st, true)
		target.OutPath = filepath.Join(t.TempDir(), "restarted.out")
		out, err := fixtures.StoreDown(t.Context(), target, faultFor)
		// The store StoreDown started is the test process's child, outside storeproc: stop it
		// before the store directory goes, whatever else failed. Its PID comes from Valkey's own
		// PID file, which the store removes at SHUTDOWN and the restarted one writes before it
		// loads, never from the file under test.
		if newPID, rerr := os.ReadFile(filepath.Join(st.Dir, "store.pid")); rerr == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(newPID))); perr == nil && pid != oldPID && pid > 1 {
				remove, recErr := recordProc(pid, strings.Join(target.Args, " "))
				t.Cleanup(func() {
					reapDetached(t, pid, syscall.SIGTERM)
					if recErr == nil {
						recErr = remove()
					}
					if recErr != nil {
						t.Errorf("the record of process %d: %v", pid, recErr)
					}
				})
			}
		}
		if err != nil {
			t.Fatalf("StoreDown: %v", err)
		}
		checkOutage(t, "StoreDown(1s) from the command line", out, minOutageNs, maxOutageNs)
		newPID := pidInFile(t, target.PIDFile)
		if newPID == oldPID {
			t.Fatalf("the PID file still names process %d: StoreDown never wrote the restarted store's PID", oldPID)
		}
		if pgid, err := syscall.Getpgid(newPID); err != nil || pgid != newPID {
			t.Errorf("the restarted store's process group: %d (%v), want its own, %d", pgid, err, newPID)
		}
		if r := do(t, st, "rl_ops", "INFO", "server"); storeproc.ParseInfo(r.Str)["process_id"] != strconv.Itoa(newPID) {
			t.Errorf("INFO server of the store at %s: process_id %q, want the PID file's %d",
				st.Addr, storeproc.ParseInfo(r.Str)["process_id"], newPID)
		}
		checkSurvivor(t, st)
	})

	t.Run("pause", func(t *testing.T) {
		st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Durable})
		pid, target := st.PID, storeTarget(t, st, false)
		c, err := st.Conn("rl_ops") // authenticated before the pause
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }() // its one PING was read in full
		began := time.Now()
		res := runFault(t, func() (fixtures.Outage, error) { return fixtures.StorePause(t.Context(), target, faultFor) })
		waitStopped(t, pid, true)
		r, err := c.Do("PING") // sent while the store is stopped: no answer until SIGCONT
		answered := time.Now()
		if err != nil || r.Kind != '+' || r.Str != "PONG" {
			t.Errorf("PING during the pause: %s, %v; want +PONG once the pause ended", r, err)
		}
		if answered.Sub(began) < faultFor {
			t.Errorf("a PING sent while the store was stopped was answered %v after StorePause began, want ≥ %v", answered.Sub(began), faultFor)
		}
		got := awaitFault(t, res)
		if got.err != nil {
			t.Fatalf("StorePause: %v", got.err)
		}
		checkOutage(t, "StorePause(1s)", got.out, minOutageNs, maxOutageNs)
		waitStopped(t, pid, false)
		if got := pidInFile(t, target.PIDFile); got != pid || st.PID != pid {
			t.Errorf("after the pause the PID file names %d and the store is %d, want the same process %d", got, st.PID, pid)
		}
	})

	t.Run("pause_cut_short", func(t *testing.T) {
		st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Durable})
		pid, target := st.PID, storeTarget(t, st, false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		res := runFault(t, func() (fixtures.Outage, error) { return fixtures.StorePause(ctx, target, longFault) })
		waitStopped(t, pid, true)
		cancel()
		got := awaitFault(t, res)
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("StorePause with its ctx ended during the pause: %v, want an error wrapping context.Canceled", got.err)
		}
		checkOutage(t, "StorePause cut short", got.out, 1, int64(longFault))
		waitStopped(t, pid, false)
		if r := do(t, st, "rl_ops", "PING"); r.Kind != '+' || r.Str != "PONG" {
			t.Errorf("PING after the cut-short pause: %s, want +PONG", r)
		}
	})

	t.Run("down_cut_short", func(t *testing.T) {
		st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Durable})
		writeSurvivor(t, st)
		oldPID, target := st.PID, storeTarget(t, st, false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		res := runFault(t, func() (fixtures.Outage, error) { return fixtures.StoreDown(ctx, target, longFault) })
		waitRefused(t, st.Addr) // SHUTDOWN took it down
		cancel()
		got := awaitFault(t, res)
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("StoreDown with its ctx ended during the outage: %v, want an error wrapping context.Canceled", got.err)
		}
		checkOutage(t, "StoreDown cut short", got.out, 1, int64(longFault))
		if st.PID == oldPID {
			t.Fatalf("the store still runs as process %d: StoreDown never restarted it", oldPID)
		}
		if got := pidInFile(t, target.PIDFile); got != st.PID {
			t.Errorf("the PID file names process %d, want the restarted store %d", got, st.PID)
		}
		checkPIDFile(t, st, target.PIDFile)
		checkSurvivor(t, st)
	})

	t.Run("down_padded", func(t *testing.T) {
		// A store of the §A7 size (storeproc.PaddingA7): its rewrite and its AOF load take long
		// enough for store.log to show where the outage starts and ends.
		st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Durable})
		if err := st.LoadPadding(storeproc.PaddingA7); err != nil {
			t.Fatal(err)
		}
		writeSurvivor(t, st)
		oldPID, target := st.PID, storeTarget(t, st, false)
		logPath := filepath.Join(st.Dir, "store.log")
		offset, baseBefore := fileSize(t, logPath), aofBase(t, st)
		out, err := fixtures.StoreDown(t.Context(), target, faultFor)
		if err != nil {
			t.Fatalf("StoreDown: %v", err)
		}
		checkOutage(t, "StoreDown(1s) of a padded store", out, minOutageNs, maxOutageNs)
		// The rewrite finished before SHUTDOWN, which would have killed a running one: the
		// manifest names a new base file.
		if base := aofBase(t, st); base == baseBefore {
			t.Errorf("the AOF manifest still names the base file %s: no rewrite finished before SHUTDOWN", base)
		}
		ev := readStoreLog(t, logPath, offset)
		start := time.Unix(0, out.StartNs)
		t.Logf("store.log: rewrite finished %s, SHUTDOWN %s, AOF loaded in %v; outage from %s",
			ev.rewriteDone.Format(logStamp), ev.shutdown.Format(logStamp), ev.load, start.Format("15:04:05.000000"))
		// The outage ends at the first PING after the AOF load (§B6 FLT-1 (a)), so it holds d and
		// the load; store.log rounds the load time to the ms.
		if least := faultFor + ev.load - logResolution; out.OutageNs < int64(least) {
			t.Errorf("outage %v, want at least %v: d %v plus the AOF load of %v that store.log shows", time.Duration(out.OutageNs), least, faultFor, ev.load)
		}
		// It starts just before SHUTDOWN is sent: after the rewrite finished, before the store
		// logged SHUTDOWN (time stamps truncated to the ms).
		if start.Before(ev.rewriteDone) || !start.Before(ev.shutdown.Add(logResolution)) {
			t.Errorf("outage start %s, want from the rewrite's end %s to SHUTDOWN %s (+%v)",
				start.Format("15:04:05.000000"), ev.rewriteDone.Format(logStamp), ev.shutdown.Format(logStamp), logResolution)
		}
		if st.PID == oldPID {
			t.Errorf("the store still runs as process %d: StoreDown never restarted it", oldPID)
		}
		checkPIDFile(t, st, target.PIDFile)
		checkSurvivor(t, st)
	})

	t.Run("pause_stale_pid", func(t *testing.T) {
		st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Durable})
		other := startHelper(t) // a process that is not the store
		target := storeTarget(t, st, false)
		if err := os.WriteFile(target.PIDFile, []byte(strconv.Itoa(other.pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := runFault(t, func() (fixtures.Outage, error) { return fixtures.StorePause(t.Context(), target, faultFor) })
		// Until StorePause returns, the process the stale file names must never show stopped.
		var got faultResult
		stoppedState := ""
		limit := time.Now().Add(faultWait)
	poll:
		for {
			select {
			case got = <-res:
				break poll
			default:
			}
			if s := psStat(t, other.pid); strings.HasPrefix(s, "T") && stoppedState == "" {
				stoppedState = s
			}
			if !time.Now().Before(limit) {
				t.Fatalf("StorePause did not return within %v", faultWait)
			}
			time.Sleep(time.Millisecond)
		}
		if got.err == nil || got.out != (fixtures.Outage{}) {
			t.Errorf("StorePause with a PID file naming process %d, not the store %d: %+v, %v; want a zero Outage and an error",
				other.pid, st.PID, got.out, got.err)
		}
		if stoppedState != "" {
			t.Errorf("process %d showed ps state %q while StorePause ran: the stale PID file stopped it", other.pid, stoppedState)
		}
		if s := psStat(t, other.pid); !other.alive() || strings.HasPrefix(s, "T") {
			t.Errorf("process %d after StorePause: ps state %q (alive %v), want it running", other.pid, s, other.alive())
		}
		if s := psStat(t, st.PID); strings.HasPrefix(s, "T") {
			t.Errorf("the store, process %d: ps state %q, want it running", st.PID, s)
		}
	})
}

// shutDownOnArrival plays a store that shuts down: once SHUTDOWN has reached srv, srv stops,
// closing the connection with no reply and refusing new ones. Then it waits for the fault's
// result, at most faultWait; a fault that returns before it sends SHUTDOWN returns at once.
func shutDownOnArrival(t *testing.T, srv *fakeresp.Server, res <-chan faultResult) faultResult {
	t.Helper()
	limit := time.Now().Add(faultWait)
	for srv.Count("SHUTDOWN") == 0 {
		select {
		case r := <-res:
			return r
		default:
		}
		if !time.Now().Before(limit) {
			t.Fatalf("no SHUTDOWN within %v; commands: %v", faultWait, srv.Commands())
		}
		time.Sleep(time.Millisecond)
	}
	srv.Stop()
	return awaitFault(t, res)
}

// TestStoreCtlDownSequence: StoreDown's commands against a fake store (internal/testutil/fakeresp,
// L1; IMPLEMENTATION_PLAN H4 step 1). It sends AUTH as rl_ops, BGREWRITEAOF, INFO persistence
// every 50 ms until aof_rewrite_in_progress:0, then on a second connection AUTH and SHUTDOWN,
// and restarts the store only after SHUTDOWN closed the connection with no reply. A
// BGREWRITEAOF refused because a rewrite already runs is waited out the same way. A SHUTDOWN
// that gets a reply, an error or +OK, leaves the store running: StoreDown fails and never
// restarts it.
func TestStoreCtlDownSequence(t *testing.T) {
	const opsPassword = "fake-ops-password"
	// Valkey 9.1's replies to BGREWRITEAOF: started, and refused while a rewrite runs.
	started := resp.Value{Kind: resp.Simple, Str: "Background append only file rewriting started"}
	const inProgress = "ERR Background append only file rewriting already in progress"
	persistence := func(rewriting int) resp.Value {
		return resp.Value{Kind: resp.Bulk, Str: "# Persistence\r\nloading:0\r\naof_rewrite_scheduled:0\r\naof_rewrite_in_progress:" +
			strconv.Itoa(rewriting) + "\r\naof_last_bgrewrite_status:ok\r\n"}
	}
	errNoRestart := errors.New("test: the fake store is not restarted")
	// fakeStore is a fake store whose INFO persistence shows a rewrite on its first two reads
	// and none on the third, and a target whose Restart counts its calls, notes how many SHUTDOWN
	// commands had arrived, and fails, so StoreDown ends there.
	fakeStore := func(t *testing.T) (srv *fakeresp.Server, target fixtures.StoreTarget, restarts, shutdownsSeen *atomic.Int32) {
		t.Helper()
		srv = fakeresp.Start(t, "rl_ops", opsPassword)
		srv.ReplyValue("INFO", persistence(1), 2)
		srv.ReplyValue("INFO", persistence(0), 1)
		restarts, shutdownsSeen = new(atomic.Int32), new(atomic.Int32)
		target = fixtures.StoreTarget{
			Addr: srv.Addr(), OpsUser: "rl_ops", OpsPassword: opsPassword,
			PIDFile: filepath.Join(t.TempDir(), "fault.pid"),
			Restart: func(context.Context) error {
				restarts.Add(1)
				shutdownsSeen.Store(int32(srv.Count("SHUTDOWN")))
				return errNoRestart
			},
		}
		return srv, target, restarts, shutdownsSeen
	}
	// Every command StoreDown sends, in order (AUTH without its password): the rewrite on one
	// connection, SHUTDOWN on a second.
	want := []string{"AUTH rl_ops", "BGREWRITEAOF", "INFO persistence", "INFO persistence", "INFO persistence", "AUTH rl_ops", "SHUTDOWN"}
	checkSequence := func(t *testing.T, srv *fakeresp.Server) {
		t.Helper()
		var got []string
		var conns []int
		for _, c := range srv.Commands() {
			args := c.Args
			if fakeresp.Label(args) == "AUTH" && len(args) > 2 {
				args = args[:2]
			}
			got = append(got, strings.Join(args, " "))
			conns = append(conns, c.Conn)
		}
		if !slices.Equal(got, want) {
			t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			return
		}
		if c := conns[0]; conns[1] != c || conns[2] != c || conns[3] != c || conns[4] != c || conns[5] == c || conns[6] != conns[5] {
			t.Errorf("connections of the commands: %v, want the first five on one, AUTH and SHUTDOWN on another", conns)
		}
	}

	for _, c := range []struct {
		name    string
		rewrite func(*fakeresp.Server)
	}{
		{"rewrite_started", func(srv *fakeresp.Server) { srv.ReplyValue("BGREWRITEAOF", started, 1) }},
		{"rewrite_in_progress", func(srv *fakeresp.Server) { srv.ReplyError("BGREWRITEAOF", inProgress, 1) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, target, restarts, shutdownsSeen := fakeStore(t)
			c.rewrite(srv)
			srv.Delay("SHUTDOWN", faultWait) // never answered: shutDownOnArrival closes the connection
			res := runFault(t, func() (fixtures.Outage, error) { return fixtures.StoreDown(t.Context(), target, time.Millisecond) })
			got := shutDownOnArrival(t, srv, res)
			if !errors.Is(got.err, errNoRestart) {
				t.Errorf("StoreDown: %v; want it to reach the restart (an error wrapping the test Restart's)", got.err)
			}
			if n, seen := restarts.Load(), shutdownsSeen.Load(); n != 1 || seen != 1 {
				t.Errorf("Restart called %d times, with %d SHUTDOWN commands sent; want once, after the SHUTDOWN", n, seen)
			}
			checkSequence(t, srv)
		})
	}

	for _, c := range []struct {
		name  string
		reply resp.Value
	}{
		{"shutdown_refused", resp.Value{Kind: resp.Error, Str: "ERR Errors trying to SHUTDOWN. Check logs."}},
		{"shutdown_answered_ok", resp.Value{Kind: resp.Simple, Str: "OK"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, target, restarts, _ := fakeStore(t)
			srv.ReplyValue("BGREWRITEAOF", started, 1)
			srv.ReplyValue("SHUTDOWN", c.reply, 1)
			got := awaitFault(t, runFault(t, func() (fixtures.Outage, error) { return fixtures.StoreDown(t.Context(), target, time.Millisecond) }))
			if got.err == nil || errors.Is(got.err, errNoRestart) || got.out != (fixtures.Outage{}) {
				t.Errorf("StoreDown with SHUTDOWN answered %s: %+v, %v; want a zero Outage and an error before any restart", c.reply, got.out, got.err)
			}
			if n := restarts.Load(); n != 0 {
				t.Errorf("Restart called %d times, want 0: a store that answers SHUTDOWN still runs", n)
			}
			checkSequence(t, srv)
		})
	}
}
