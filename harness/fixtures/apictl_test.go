//go:build rltest

package fixtures_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/internal/testutil/golden"
)

// KillAPI test values (IMPLEMENTATION_PLAN H4 step 2 and tests).
const (
	// goldenAPIFile is the hand-written golden api.jsonl (DESIGN §C5).
	goldenAPIFile = "../runlog/testdata/golden/api.jsonl"
	// logBefore is what an instance's run log holds before the kill.
	logBefore = `{"kind":"lifecycle","event":"start"}` + "\n" + `{"kind":"lifecycle","event":"ready"}` + "\n"
	// preKillHold is how long the test keeps the pre_kill line from being written (KillAPI
	// blocks opening the FIFO log), so a kill sent before the line would show.
	preKillHold = 300 * time.Millisecond
	// killWait bounds the waits for KillAPI's steps to show: the pre-kill call, the line, the
	// helper's exit.
	killWait = 10 * time.Second
	// helperSleep is how long a helper process would live unkilled, in seconds.
	helperSleep = "60"
	// shortPrekill is the PrekillTimeout of a pre-kill call that is never answered;
	// prekillCutWithin bounds KillAPI's return then: the timeout, the line and the kill, far
	// below the client's own 10 s Timeout (killWait).
	shortPrekill     = 300 * time.Millisecond
	prekillCutWithin = 2 * time.Second
	// exitedWithin bounds KillAPI when the restarted process has exited at once: far below the
	// 30 s it may wait for /health ready.
	exitedWithin = 5 * time.Second
	// prekillOK is a pre-kill reply that drained the run log with no drops.
	prekillOK = `{"listener_drops":0,"log_drops":0,"log_queue_len":0}`
)

// helper is a child process standing in for a hitapi instance's process: sleep, recorded in
// .out/procs while it runs. A waiter goroutine reaps it; the test's cleanup kills it if it
// still runs and waits for the waiter.
type helper struct {
	cmd    *exec.Cmd
	pid    int              // cmd.Process.Pid
	remove func() error     // removes its record
	done   chan struct{}    // closed once the waiter has reaped it and removed its record
	state  *os.ProcessState // set before done closes
	recErr error            // removing its record; set before done closes
}

func startHelper(t *testing.T) *helper {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	// Ended explicitly (killed, then reaped), never by the test's context (AGENTS.md §2).
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), sleep, helperSleep)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd, done: make(chan struct{}), pid: cmd.Process.Pid}
	h.remove, err = recordProc(h.pid, sleep+" "+helperSleep)
	if err != nil {
		_ = cmd.Process.Kill() // unrecorded: it must not outlive this call
		_ = cmd.Wait()
		t.Fatalf("recording process %d: %v", h.pid, err)
	}
	go func() {
		_ = cmd.Wait() // a killed sleep's Wait error is its signal, read from ProcessState
		h.state = cmd.ProcessState
		h.recErr = h.remove()
		close(h.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // already gone after a KillAPI: nothing to do then
		<-h.done
		if h.recErr != nil {
			t.Errorf("removing the record of process %d: %v", h.pid, h.recErr)
		}
	})
	return h
}

// alive reports whether the helper has not exited yet.
func (h *helper) alive() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// checkKilled waits up to killWait for the helper to exit and checks that SIGKILL ended it.
func (h *helper) checkKilled(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(killWait)
	defer timer.Stop()
	select {
	case <-h.done:
	case <-timer.C:
		t.Fatalf("process %d still ran %v after KillAPI returned", h.pid, killWait)
	}
	ws, ok := h.state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("process %d ended with %v, want SIGKILL", h.pid, h.state)
	}
}

// fakeAPI serves an instance's /_test/prekill and /health (httptest): the pre-kill reply is
// fixed, or with status 0 never sent (the call is held until its client gives up or the test
// ends); /health answers health[i] to its i-th GET, the last one from then on, after calling
// the onHealth function, if any, with i.
type fakeAPI struct {
	srv       *httptest.Server
	proc      *helper // the process to be killed; nil when none
	status    int
	body      string
	health    []string
	prekilled chan struct{} // closed at the first pre-kill call
	release   chan struct{} // closed when the test ends: a held pre-kill call returns

	mu             sync.Mutex
	prekills       int
	aliveAtPrekill bool
	healthGets     int
	beforeHealth   func(i int) // set by onHealth
}

func newFakeAPI(t *testing.T, proc *helper, status int, body string, health ...string) *fakeAPI {
	t.Helper()
	a := &fakeAPI{proc: proc, status: status, body: body, health: health, prekilled: make(chan struct{}), release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/_test/prekill", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		a.mu.Lock()
		a.prekills++
		if a.prekills == 1 {
			a.aliveAtPrekill = a.proc != nil && a.proc.alive()
			close(a.prekilled)
		}
		a.mu.Unlock()
		if a.status == 0 { // never answered
			select {
			case <-r.Context().Done(): // the client gave up and closed the connection
			case <-a.release:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body) // a failed write shows as a failed pre-kill read
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || len(a.health) == 0 {
			http.Error(w, "no /health here", http.StatusMethodNotAllowed)
			return
		}
		a.mu.Lock()
		i := a.healthGets
		body := a.health[min(i, len(a.health)-1)]
		a.healthGets++
		before := a.beforeHealth
		a.mu.Unlock()
		if before != nil {
			before(i)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body) // a failed write shows as an instance not ready
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	t.Cleanup(func() { close(a.release) }) // runs first: Close waits for a held call to return
	return a
}

// onHealth makes /health call fn with the index of each GET (0 for the first) before it
// answers.
func (a *fakeAPI) onHealth(fn func(i int)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.beforeHealth = fn
}

// counts returns the pre-kill calls, whether the process was alive at the first, and the
// /health reads so far.
func (a *fakeAPI) counts() (prekills int, aliveAtPrekill bool, healthGets int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prekills, a.aliveAtPrekill, a.healthGets
}

// apiTarget is the target for an instance served by a, running as process pid, with its run
// log at logPath.
func apiTarget(t *testing.T, a *fakeAPI, pid int, logPath string) fixtures.APITarget {
	t.Helper()
	client := &http.Client{Timeout: killWait}
	t.Cleanup(client.CloseIdleConnections)
	return fixtures.APITarget{BaseURL: a.srv.URL, PID: pid, LogPath: logPath, PrekillTimeout: 2 * time.Second, Client: client}
}

// runLog writes an instance's run log as it stands before the kill and returns its path.
func runLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api-2.jsonl")
	if err := os.WriteFile(path, []byte(logBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkLog checks that the run log holds its earlier lines, then exactly the line want.
func checkLog(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != logBefore+want+"\n" {
		t.Errorf("the run log after KillAPI:\n%s\nwant its earlier lines, then:\n%s", got, want)
	}
}

// checkLogUnchanged checks that the run log holds only its earlier lines.
func checkLogUnchanged(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != logBefore {
		t.Errorf("the run log after KillAPI:\n%s\nwant only its earlier lines", got)
	}
}

// startExited starts true(1) as a child of the test and waits, at most killWait, until it has
// exited and ps shows it as a zombie: nobody reaps it before the test's cleanup, so its PID
// names an exited process that no other process can take meanwhile. It is recorded in
// .out/procs until then.
func startExited(t *testing.T) (int, error) {
	t.Helper()
	bin, err := exec.LookPath("true")
	if err != nil {
		return 0, err
	}
	// Reaped by the cleanup, never ended by the test's context (AGENTS.md §2).
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), bin)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	remove, recErr := recordProc(pid, bin)
	t.Cleanup(func() {
		_ = cmd.Wait() // reaps it: true's exit status does not matter
		if recErr == nil {
			recErr = remove()
		}
		if recErr != nil {
			t.Errorf("the record of process %d: %v", pid, recErr)
		}
	})
	if recErr != nil {
		return 0, fmt.Errorf("recording process %d: %w", pid, recErr)
	}
	for limit := time.Now().Add(killWait); ; time.Sleep(time.Millisecond) {
		if strings.HasPrefix(psStat(t, pid), "Z") {
			return pid, nil
		}
		if !time.Now().Before(limit) {
			return 0, fmt.Errorf("process %d did not exit within %v", pid, killWait)
		}
	}
}

// readFIFOLine opens the FIFO at path for reading (O_NONBLOCK, so it never waits for a writer)
// and reads until a whole line has arrived, at most killWait. Before the writer opens its end,
// a read finds no writer (EOF), and before it writes, nothing to read (EAGAIN): it reads again.
func readFIFOLine(t *testing.T, path string) string {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // read only
	var got []byte
	buf := make([]byte, 4096)
	limit := time.Now().Add(killWait)
	for {
		n, err := f.Read(buf)
		got = append(got, buf[:n]...)
		if i := strings.IndexByte(string(got), '\n'); i >= 0 {
			return string(got[:i])
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("reading the FIFO run log: %v", err)
		}
		if !time.Now().Before(limit) {
			t.Fatalf("no whole line on the FIFO run log within %v (got %q)", killWait, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestKillAPIPreKillRecord: KillAPI (IMPLEMENTATION_PLAN H4 step 2; DESIGN §B6 FLT-6) against a
// helper process plus an httptest server for /_test/prekill and /health. The pre-kill call
// comes first, while the process runs, and the pre_kill line with the reply's drop counts and
// read_ok:true is written before the kill; it matches the golden pre_kill line. With the
// pre-kill call unreachable, never answered (cut at PrekillTimeout), or a reply that leaves
// log_queue_len above 0, is not 200, or lacks a field, the line says read_ok:false and the
// process is still killed. A PID checkPID refuses, or one that names no process, is refused
// before the pre-kill call: nothing is called, written, killed or started. With restart, the
// instance starts again (through Start after the kill, or from its command line as a detached
// child with exactly its environment and output file) and KillAPI waits for /health ready; a
// restarted process that has exited fails that wait at once, ready answer or not, and so does
// one that exits between two reads, at the second, though it answers ready.
func TestKillAPIPreKillRecord(t *testing.T) {
	t.Run("read_ok", func(t *testing.T) {
		proc := startHelper(t)
		api := newFakeAPI(t, proc, http.StatusOK, `{"listener_drops":3,"log_drops":5,"log_queue_len":0}`)
		// A FIFO run log: KillAPI's open(2) for the line blocks until the test opens the other
		// end, so the line provably comes before the kill when the process outlives the hold.
		logPath := filepath.Join(t.TempDir(), "api-2.jsonl")
		if err := syscall.Mkfifo(logPath, 0o600); err != nil {
			t.Fatal(err)
		}
		target := apiTarget(t, api, proc.pid, logPath)
		type killResult struct {
			pid int
			err error
		}
		res := make(chan killResult, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			pid, err := fixtures.KillAPI(t.Context(), target, false)
			res <- killResult{pid, err}
		}()
		t.Cleanup(func() {
			// On a failure before the read below, KillAPI may still block opening the FIFO: a
			// reader, open until it returns, lets it go on.
			f, err := os.OpenFile(logPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			wg.Wait()
			if err == nil {
				_ = f.Close() // read only
			}
		})

		timer := time.NewTimer(killWait)
		defer timer.Stop()
		select {
		case <-api.prekilled:
		case <-timer.C:
			t.Fatalf("no pre-kill call within %v", killWait)
		}
		hold := time.NewTimer(preKillHold)
		defer hold.Stop()
		select {
		case <-proc.done:
			t.Fatal("the process was killed before its pre_kill line was written")
		case <-hold.C:
		}
		line := readFIFOLine(t, logPath)
		const want = `{"kind":"lifecycle","event":"pre_kill","listener_drops":3,"log_drops":5,"read_ok":true}`
		if line != want {
			t.Errorf("pre_kill line %s, want %s", line, want)
		}
		golden.Match(t, [][]byte{[]byte(line)}, goldenAPIFile, "kind", "event")
		var got killResult
		select {
		case got = <-res:
		case <-time.After(killWait):
			t.Fatalf("KillAPI did not return within %v of writing its line", killWait)
		}
		if got.err != nil || got.pid != 0 {
			t.Errorf("KillAPI without restart: %d, %v; want 0, nil", got.pid, got.err)
		}
		proc.checkKilled(t)
		if prekills, alive, gets := api.counts(); prekills != 1 || !alive || gets != 0 {
			t.Errorf("pre-kill calls %d (process alive at the first: %v), /health reads %d; want 1 (alive), 0", prekills, alive, gets)
		}
	})

	t.Run("prekill_unreachable", func(t *testing.T) {
		proc := startHelper(t)
		api := newFakeAPI(t, proc, http.StatusOK, `{}`)
		target := apiTarget(t, api, proc.pid, runLog(t))
		api.srv.Close() // nothing answers at BaseURL any more: connection refused
		pid, err := fixtures.KillAPI(t.Context(), target, false)
		if !errors.Is(err, fixtures.ErrPrekillUnread) || pid != 0 {
			t.Errorf("KillAPI with the pre-kill call unreachable: %d, %v; want 0 and an error wrapping ErrPrekillUnread", pid, err)
		}
		proc.checkKilled(t)
		checkLog(t, target.LogPath, `{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":0,"read_ok":false}`)
	})

	t.Run("reply_unread", func(t *testing.T) {
		cases := []struct {
			name   string
			status int
			body   string
			want   string
		}{
			{"log_queue_not_drained", http.StatusOK, `{"listener_drops":1,"log_drops":2,"log_queue_len":4}`,
				`{"kind":"lifecycle","event":"pre_kill","listener_drops":1,"log_drops":2,"read_ok":false}`},
			{"status_503", http.StatusServiceUnavailable, `{"listener_drops":0,"log_drops":7,"log_queue_len":0}`,
				`{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":7,"read_ok":false}`},
			{"field_missing", http.StatusOK, `{"listener_drops":1,"log_drops":2}`,
				`{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":0,"read_ok":false}`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				proc := startHelper(t)
				api := newFakeAPI(t, proc, c.status, c.body)
				target := apiTarget(t, api, proc.pid, runLog(t))
				pid, err := fixtures.KillAPI(t.Context(), target, false)
				if !errors.Is(err, fixtures.ErrPrekillUnread) || pid != 0 {
					t.Errorf("KillAPI: %d, %v; want 0 and an error wrapping ErrPrekillUnread", pid, err)
				}
				proc.checkKilled(t)
				checkLog(t, target.LogPath, c.want)
				if prekills, alive, _ := api.counts(); prekills != 1 || !alive {
					t.Errorf("pre-kill calls %d (process alive at the first: %v), want 1 (alive)", prekills, alive)
				}
			})
		}
	})

	t.Run("restart_start_hook", func(t *testing.T) {
		old := startHelper(t)
		api := newFakeAPI(t, old, http.StatusOK, `{"listener_drops":0,"log_drops":0,"log_queue_len":0}`,
			`{"status":"not_ready","reason":"no_epoch"}`, `{"status":"not_ready","reason":"no_epoch"}`, `{"status":"ready","reason":"ready"}`)
		target := apiTarget(t, api, old.pid, runLog(t))
		var started *helper
		target.Start = func(ctx context.Context) (int, error) {
			// The killed process's waiter closes done after it has reaped it and removed its
			// record, a moment after the kill(2) check KillAPI's gone-wait ends on: wait for it,
			// bounded. Start called before the kill finds the process still running.
			select {
			case <-old.done:
			case <-time.After(killWait):
				return 0, errors.New("Start ran while the killed process was still there")
			}
			if _, _, gets := api.counts(); gets != 0 {
				return 0, errors.New("/health was read before the instance was started again")
			}
			if _, ok := ctx.Deadline(); !ok {
				return 0, errors.New("Start got a context without a deadline")
			}
			started = startHelper(t)
			return started.pid, nil
		}
		newPID, err := fixtures.KillAPI(t.Context(), target, true)
		if err != nil {
			t.Fatalf("KillAPI with restart: %v", err)
		}
		if started == nil || newPID != started.pid {
			t.Fatalf("KillAPI returned PID %d, want the one Start started", newPID)
		}
		old.checkKilled(t)
		if !started.alive() {
			t.Error("the restarted process is not running")
		}
		if _, _, gets := api.counts(); gets != 3 {
			t.Errorf("/health read %d times, want 3: until the third answered status ready", gets)
		}
		checkLog(t, target.LogPath, `{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":0,"read_ok":true}`)
	})

	t.Run("restart_from_args", func(t *testing.T) {
		old := startHelper(t)
		api := newFakeAPI(t, old, http.StatusOK, `{"listener_drops":0,"log_drops":0,"log_queue_len":0}`, `{"status":"ready"}`)
		target := apiTarget(t, api, old.pid, runLog(t))
		t.Setenv("RL_FIXTURES_CALLER_ONLY", "1") // in the caller's environment only
		const marker = "RL_FIXTURES_RESTARTED=1"
		outPath := filepath.Join(t.TempDir(), "hitapi-2.stderr")
		if err := os.WriteFile(outPath, []byte("earlier output\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		target.Args = []string{"/bin/sh", "-c", "/usr/bin/env; exec /bin/sleep " + helperSleep}
		target.Env = []string{marker}
		target.OutPath = outPath
		newPID, err := fixtures.KillAPI(t.Context(), target, true)
		if newPID > 1 {
			remove, recErr := recordProc(newPID, strings.Join(target.Args, " "))
			t.Cleanup(func() {
				reapDetached(t, newPID, syscall.SIGKILL)
				if recErr == nil {
					recErr = remove()
				}
				if recErr != nil {
					t.Errorf("the record of process %d: %v", newPID, recErr)
				}
			})
		}
		if err != nil {
			t.Fatalf("KillAPI with restart from the command line: %v", err)
		}
		if newPID <= 1 || newPID == old.pid {
			t.Fatalf("KillAPI returned PID %d, want the restarted process", newPID)
		}
		old.checkKilled(t)
		if pgid, err := syscall.Getpgid(newPID); err != nil || pgid != newPID {
			t.Errorf("the restarted process's group: %d (%v), want its own, %d", pgid, err, newPID)
		}
		var out string
		for limit := time.Now().Add(killWait); ; time.Sleep(10 * time.Millisecond) {
			b, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			if out = string(b); strings.Contains(out, marker+"\n") || !time.Now().Before(limit) {
				break
			}
		}
		if !strings.HasPrefix(out, "earlier output\n") || !strings.Contains(out, marker+"\n") || strings.Contains(out, "RL_FIXTURES_CALLER_ONLY") {
			t.Errorf("the restarted process's output file:\n%s\nwant the earlier output, then its environment: %s and nothing of the caller's", out, marker)
		}
		if err := syscall.Kill(newPID, 0); err != nil {
			t.Errorf("the restarted process %d: %v, want it running", newPID, err)
		}
		checkLog(t, target.LogPath, `{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":0,"read_ok":true}`)
	})

	t.Run("prekill_timeout", func(t *testing.T) {
		proc := startHelper(t)
		api := newFakeAPI(t, proc, 0, "") // the pre-kill call is never answered
		target := apiTarget(t, api, proc.pid, runLog(t))
		target.PrekillTimeout = shortPrekill // the client's own Timeout stays killWait, 10 s
		began := time.Now()
		pid, err := fixtures.KillAPI(t.Context(), target, false)
		took := time.Since(began)
		if !errors.Is(err, fixtures.ErrPrekillUnread) || pid != 0 {
			t.Errorf("KillAPI with the pre-kill call never answered: %d, %v; want 0 and an error wrapping ErrPrekillUnread", pid, err)
		}
		if took < shortPrekill || took > prekillCutWithin {
			t.Errorf("KillAPI returned after %v, want from the %v PrekillTimeout to %v: the call is cut at PrekillTimeout, not the client's %v Timeout",
				took, shortPrekill, prekillCutWithin, killWait)
		}
		proc.checkKilled(t)
		checkLog(t, target.LogPath, `{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":0,"read_ok":false}`)
		if prekills, alive, _ := api.counts(); prekills != 1 || !alive {
			t.Errorf("pre-kill calls %d (process alive at the first: %v), want 1 (alive)", prekills, alive)
		}
	})

	t.Run("pid_refused", func(t *testing.T) {
		// PID 1, init or launchd, which checkPID refuses: KillAPI must stop before any call. Never
		// 0 or -1 here: without the check, kill(2) would reach this test's process group or every
		// process of the user. As root, PID 1 would not even refuse the signal.
		if os.Geteuid() == 0 {
			t.Fatal("this test sends signals that are safe only without root: run it as another user")
		}
		api := newFakeAPI(t, nil, http.StatusOK, prekillOK)
		target := apiTarget(t, api, 1, runLog(t))
		pid, err := fixtures.KillAPI(t.Context(), target, false)
		if err == nil || pid != 0 {
			t.Errorf("KillAPI with PID 1: %d, %v; want 0 and an error", pid, err)
		}
		if prekills, _, _ := api.counts(); prekills != 0 {
			t.Errorf("pre-kill calls %d for PID 1, want 0", prekills)
		}
		checkLogUnchanged(t, target.LogPath)
	})

	t.Run("pid_not_running", func(t *testing.T) {
		// MaxInt32 passes checkPID but lies above every system's largest PID (99,999 on macOS, at
		// most 2^22 on Linux), so it names no process: the pre-kill call, which cannot be undone,
		// must not be made, and nothing is written, killed or started.
		const pid = math.MaxInt32
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("kill(%d, 0): %v, want ESRCH (no such process)", pid, err)
		}
		api := newFakeAPI(t, nil, http.StatusOK, prekillOK, `{"status":"ready"}`)
		target := apiTarget(t, api, pid, runLog(t))
		var starts atomic.Int32
		target.Start = func(context.Context) (int, error) {
			starts.Add(1)
			return 0, errors.New("Start ran for a PID that names no process")
		}
		newPID, err := fixtures.KillAPI(t.Context(), target, true)
		if err == nil || newPID != 0 {
			t.Errorf("KillAPI with a PID that names no process: %d, %v; want 0 and an error", newPID, err)
		}
		if prekills, _, gets := api.counts(); prekills != 0 || gets != 0 || starts.Load() != 0 {
			t.Errorf("pre-kill calls %d, /health reads %d, starts %d; want none", prekills, gets, starts.Load())
		}
		checkLogUnchanged(t, target.LogPath)
	})

	t.Run("restart_exited", func(t *testing.T) {
		cases := []struct{ name, health string }{
			// Another listener answering ready at BaseURL must not count for a dead restart.
			{"health_ready", `{"status":"ready","reason":"ready"}`},
			// Nor may a dead restart be waited on for the 30 s.
			{"health_not_ready", `{"status":"not_ready","reason":"no_epoch"}`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				old := startHelper(t)
				api := newFakeAPI(t, old, http.StatusOK, prekillOK, c.health)
				target := apiTarget(t, api, old.pid, runLog(t))
				exited := 0
				target.Start = func(context.Context) (int, error) {
					pid, err := startExited(t)
					exited = pid
					return pid, err
				}
				began := time.Now()
				newPID, err := fixtures.KillAPI(t.Context(), target, true)
				took := time.Since(began)
				if err == nil {
					t.Errorf("KillAPI whose restarted process %d had exited: nil error, want one", exited)
				}
				if exited == 0 || newPID != exited {
					t.Errorf("KillAPI returned PID %d, want the restarted process %d", newPID, exited)
				}
				if took > exitedWithin {
					t.Errorf("KillAPI returned after %v, want within %v: the exited process ends the wait for ready", took, exitedWithin)
				}
				old.checkKilled(t)
				checkLog(t, target.LogPath, `{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":0,"read_ok":true}`)
			})
		}
	})

	t.Run("restart_exits_while_waiting", func(t *testing.T) {
		// The restarted process runs at the first /health read, which answers not ready, and is
		// killed and reaped while the second read answers ready: the wait checks the process
		// after every read, so it fails there, as the ready answer may be another listener's.
		old := startHelper(t)
		api := newFakeAPI(t, old, http.StatusOK, prekillOK, `{"status":"not_ready","reason":"no_epoch"}`, `{"status":"ready","reason":"ready"}`)
		target := apiTarget(t, api, old.pid, runLog(t))
		var restarted atomic.Pointer[helper]
		target.Start = func(context.Context) (int, error) {
			h := startHelper(t)
			restarted.Store(h)
			return h.pid, nil
		}
		var reaped atomic.Bool
		api.onHealth(func(i int) {
			h := restarted.Load()
			if i != 1 || h == nil {
				return
			}
			_ = h.cmd.Process.Kill() // reaped by its waiter, which closes done
			select {
			case <-h.done:
				reaped.Store(true)
			case <-time.After(killWait):
			}
		})
		newPID, err := fixtures.KillAPI(t.Context(), target, true)
		h := restarted.Load()
		if h == nil || newPID != h.pid {
			t.Fatalf("KillAPI returned PID %d, want the one Start started", newPID)
		}
		_, _, gets := api.counts()
		if !reaped.Load() {
			t.Fatalf("the restarted process %d was not killed and reaped at the second /health read (/health read %d times; KillAPI: %v)",
				h.pid, gets, err)
		}
		if want := fmt.Sprintf("the restarted process %d has exited", h.pid); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("KillAPI whose restarted process exited after the first /health read: %v, want an error saying %q", err, want)
		}
		if gets != 2 {
			t.Errorf("/health read %d times, want 2: the second one's ready answer came after the process had exited", gets)
		}
		old.checkKilled(t)
		checkLog(t, target.LogPath, `{"kind":"lifecycle","event":"pre_kill","listener_drops":0,"log_drops":0,"read_ok":true}`)
	})
}
