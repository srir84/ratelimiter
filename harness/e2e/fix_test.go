//go:build rltest

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/limiter"
)

// T-FIX-1's bounds (IMPLEMENTATION_PLAN H4 Tests; DESIGN §B6; plan choices unless noted).
const (
	// downFor and pauseFor are the outages scenarios arm (DESIGN §B6): store down 4s and store
	// pause 4.5s, each of which must measure at most maxOutage (a longer one invalidates the
	// run's REF-5 evidence).
	downFor   = 4 * time.Second
	pauseFor  = 4500 * time.Millisecond
	maxOutage = 5 * time.Second
	// faultWait bounds a store fault from its start to its result: the AOF rewrite, the outage,
	// the restart and the PINGs.
	faultWait = 2 * time.Minute
	// stageWait bounds each wait for a fault to reach its stage, or for the stack to recover.
	stageWait = 10 * time.Second
	// holdFor is how long a request held at a barrier must stay unanswered (plan H4 Tests).
	holdFor = 200 * time.Millisecond
	// pauseSlack is the margin before the pause ends that a request sent under it keeps: its
	// whole acquire deadline (D) must pass while the store is stopped.
	pauseSlack = 400 * time.Millisecond
	// token is one token in rladmin inspect's balance units (u).
	token = int64(1_000_000_000)
)

// sendRecord is one record of an instance's send observer, GET /_test/sends (DESIGN §A5.4 test
// hook, §A5.12): cmd is the script name of an EVALSHA ("acquire"), and bytes_written above 0
// means the command was sent.
type sendRecord struct {
	Seq          uint64 `json:"seq"`
	Cmd          string `json:"cmd"`
	BytesWritten int    `json:"bytes_written"`
}

// sendsSince returns the instance's send-observer records after seq.
func sendsSince(t *testing.T, a *API, seq uint64) []sendRecord {
	t.Helper()
	r := a.Do(t, http.MethodGet, "/_test/sends?since="+strconv.FormatUint(seq, 10), nil, "")
	var recs []sendRecord
	if r.Status != http.StatusOK || json.Unmarshal(r.Body, &recs) != nil {
		t.Fatalf("GET /_test/sends on hitapi %d: %d %s, want 200 with the records", a.N, r.Status, r.Body)
	}
	return recs
}

// lastSend returns the sequence number of the instance's newest send record (0 before any).
func lastSend(t *testing.T, a *API) uint64 {
	t.Helper()
	recs := sendsSince(t, a, 0)
	if len(recs) == 0 {
		return 0
	}
	return recs[len(recs)-1].Seq
}

// acquiresSent counts the acquire records among recs whose bytes were written: acquires sent.
// One acquire call may send twice: a store's first acquire gets NOSCRIPT, and the adapter
// sends it once more after SCRIPT LOAD (AGENTS.md §6), so "sent" is a count above 0.
func acquiresSent(recs []sendRecord) int {
	n := 0
	for _, s := range recs {
		if s.Cmd == "acquire" && s.BytesWritten > 0 {
			n++
		}
	}
	return n
}

// controlHit is the positive control of every check that reads the send observer (an empty
// observer cannot pass it): a /hit that is allowed (200, its row created) and whose acquire the
// observer shows sent, bytes_written above 0.
func controlHit(t *testing.T, a *API, user, request string) {
	t.Helper()
	since := lastSend(t, a)
	wantCreated(t, "the control /hit "+request, a.Hit(t, user, request), user, request)
	if n := acquiresSent(sendsSince(t, a, since)); n == 0 {
		t.Errorf("the control /hit %s: the send observer shows no acquire sent, want it sent", request)
	}
}

// storeFault is a store fault's result (fixtures.StoreDown, fixtures.StorePause).
type storeFault struct {
	out fixtures.Outage
	err error
}

// startStoreFault runs fault on its own goroutine with t's context and returns where its result
// arrives. Its cleanup, which runs before the stack's, waits for it, at most faultWait: once
// t's context ends the fault stops waiting and brings the store back (StoreDown, StorePause).
func startStoreFault(t *testing.T, fault func(context.Context) (fixtures.Outage, error)) <-chan storeFault {
	ch := make(chan storeFault, 1)
	done := make(chan struct{})
	ctx := t.Context()
	go func() {
		defer close(done)
		out, err := fault(ctx)
		ch <- storeFault{out, err}
	}()
	t.Cleanup(func() {
		timer := time.NewTimer(faultWait)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			t.Errorf("the store fault still ran %v after its test", faultWait)
		}
	})
	return ch
}

// awaitStoreFault receives the fault's result, at most faultWait, failing the test on an error,
// and checks that its outage measured at least min and at most maxOutage.
func awaitStoreFault(t *testing.T, ch <-chan storeFault, what string, min time.Duration) fixtures.Outage {
	t.Helper()
	timer := time.NewTimer(faultWait)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("%s: %v", what, res.err)
		}
		if d := time.Duration(res.out.OutageNs); d < min || d > maxOutage || res.out.EndNs <= res.out.StartNs {
			t.Errorf("%s measured an outage of %v (%d to %d ns), want %v to %v", what, d, res.out.StartNs, res.out.EndNs, min, maxOutage)
		}
		t.Logf("%s measured an outage of %v", what, time.Duration(res.out.OutageNs))
		return res.out
	case <-timer.C:
		t.Fatalf("%s: no result within %v", what, faultWait)
		return fixtures.Outage{}
	}
}

// storeTarget is the stack's store as a store fault's target (DESIGN §B6; plan H4 step 1): its
// address, rl_ops, a PID file holding its PID, and the stack's own restart (storeproc's), so
// the restarted store keeps one owner and one .out/procs record. While the fault runs only it
// touches s.Store (storeproc's methods are for one goroutine at a time).
func storeTarget(t *testing.T, s *Stack) fixtures.StoreTarget {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "store.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(s.Store.PID)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixtures.StoreTarget{Addr: s.Store.Addr, OpsUser: "rl_ops", OpsPassword: s.Store.PW.Ops, PIDFile: pidFile,
		Restart: func(context.Context) error { return s.Store.Restart(false) }}
}

// waitRefused waits until a connection to addr is refused, at most stageWait: the store has
// closed its port.
func waitRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(stageWait)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		cancel()
		if errors.Is(err, syscall.ECONNREFUSED) {
			return
		}
		if err == nil {
			_ = c.Close() // only the connect mattered
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store at %s still accepted connections %v later", addr, stageWait)
		}
		time.Sleep(pollEvery)
	}
}

// series reads one series of the instance's /metrics, such as rl_store_connections{kind="call"}.
func series(t *testing.T, a *API, name string) float64 {
	t.Helper()
	r := a.Do(t, http.MethodGet, "/metrics", nil, "")
	for _, line := range strings.Split(string(r.Body), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("/metrics line %q: %v", line, err)
			}
			return f
		}
	}
	t.Fatalf("/metrics has no series %s:\n%s", name, r.Body)
	return 0
}

// waitSeries waits up to stageWait until the instance's /metrics shows name at want.
func waitSeries(t *testing.T, a *API, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(stageWait)
	for {
		got := series(t, a, name)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hitapi %d: /metrics %s = %v after %v, want %v", a.N, name, got, stageWait, want)
		}
		time.Sleep(pollEvery)
	}
}

// waitStopped waits up to stageWait until ps shows process pid stopped (state T).
func waitStopped(t *testing.T, pid int) time.Time {
	t.Helper()
	deadline := time.Now().Add(stageWait)
	for {
		out, err := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "T") {
			return time.Now()
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d: ps state %q (%v) after %v, want stopped", pid, out, err, stageWait)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitAllowed sends /hit for user, each try with a new request ID, until one is allowed (200,
// created), at most stageWait: the instance has reconnected to a store that kept its epoch.
func waitAllowed(t *testing.T, a *API, user string) {
	t.Helper()
	deadline := time.Now().Add(stageWait)
	for i := 1; ; i++ {
		request := "after-" + strconv.Itoa(i)
		r := a.Hit(t, user, request)
		if r.Status == http.StatusOK {
			wantCreated(t, "the /hit after the store came back", r, user, request)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hitapi %d: no /hit allowed within %v after the store came back; the last: %d %s", a.N, stageWait, r.Status, r.Body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tamper runs one faultctl tamper command (fixtures.ParseFaultCommand, the grammar faultctl and
// the orchestrator use, then Tamper.Apply) on the stack's store as rl_fixture, in the namespace
// of the epoch the instance serves (/health epoch_id).
func tamper(t *testing.T, s *Stack, a *API, args ...string) {
	t.Helper()
	cmd, err := fixtures.ParseFaultCommand(append([]string{"tamper"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	epoch, _ := a.Health(t)["epoch_id"].(string)
	conn, err := s.Store.Conn("rl_fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // its commands were read in full
	if err := fixtures.NewTamper(conn, epoch).Apply(cmd.Tamper); err != nil {
		t.Fatalf("tamper %s: %v", strings.Join(args, " "), err)
	}
}

// deductionOf returns the deduction_id of user's decision record on the instance, waiting for
// it (the run log is written a moment after the response).
func deductionOf(t *testing.T, a *API, user string) string {
	t.Helper()
	var id string
	a.WaitRecords(t, "decision", func(rs []map[string]any) bool {
		for _, r := range rs {
			if r["user_id"] == user {
				id, _ = r["deduction_id"].(string)
				return id != ""
			}
		}
		return false
	})
	return id
}

// refundOf waits for the refund record of deduction id on the instance and returns it.
func refundOf(t *testing.T, a *API, id string) map[string]any {
	t.Helper()
	var rec map[string]any
	a.WaitRecords(t, "refund", func(rs []map[string]any) bool {
		for _, r := range rs {
			if r["deduction_id"] == id {
				rec = r
				return true
			}
		}
		return false
	})
	return rec
}

// restartAPI starts hitapi a.N again with its own command line (address, config, run log, run
// ID) and environment, recorded in .out/procs, in place of a's process, and returns its PID:
// KillAPI's Start hook (plan H4 step 2), so the restarted instance has one owner, the stack,
// whose cleanup stops it.
func (s *Stack) restartAPI(t *testing.T, a *API) (int, error) {
	t.Helper()
	c, err := s.startChild(t, s.bin("hitapi"), a.cmd.Args[1:], a.cmd.Env, "hitapi-"+strconv.Itoa(a.N)+"-restarted")
	if err != nil {
		return 0, err
	}
	a.child = c
	return c.PID, nil
}

// TestT_FIX_1 (HARNESS §8 T-FIX-1: FLT-1, FLT-2, FLT-3, FLT-4, FLT-6, FLT-7, FLT-8, FLT-10,
// FLT-11, TRF-2; IMPLEMENTATION_PLAN H4 Tests; DESIGN §B6): each fault, armed through the code
// faultctl and the orchestrator run (harness/fixtures, the /_test routes), reaches its stage
// and gives its result through bin/test hitapi's /hit, on a stack of its own on a frozen
// controlled clock; tokens from bin/test/rladmin inspect, rows from dbclient.CountKey. Each
// check that reads the send observer has its positive control: a command sent shows up with
// bytes_written above 0 (controlHit, or the fault's own sent acquires). "Sent" is shown by the
// observer, never by timing.
func TestT_FIX_1(t *testing.T) {
	// FLT-1 (a): store down 4s closes the store's port. Once it is closed and the instance has
	// no ready call connection, a /hit is 503 limiter_unavailable (closed mode), its acquire not
	// sent; the outage measures 4,000 to 5,000 ms, and the store comes back with its epoch.
	t.Run("FLT-1a", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		controlHit(t, api, "f1a", "before")
		target := storeTarget(t, s)
		ch := startStoreFault(t, func(ctx context.Context) (fixtures.Outage, error) { return fixtures.StoreDown(ctx, target, downFor) })
		waitRefused(t, target.Addr)
		waitSeries(t, api, `rl_store_connections{kind="call"}`, 0)
		since := lastSend(t, api)
		r := api.Hit(t, "f1a", "down")
		wantError(t, "a /hit while the store is down", r, http.StatusServiceUnavailable, "limiter_unavailable")
		if n := acquiresSent(sendsSince(t, api, since)); n != 0 {
			t.Errorf("the /hit while the store is down: %d acquires sent, want 0 (a refused connection sends nothing)", n)
		}
		awaitStoreFault(t, ch, "store down 4s", downFor)
		if n := s.keyRows(t, "f1a", "down"); n != 0 {
			t.Errorf("rows for (f1a, down) = %d, want 0", n)
		}
		waitAllowed(t, api, "f1a")
	})

	// FLT-1 (b): the store wrapper's refuse returns unavailable without sending: the observer
	// shows no acquire sent; closed mode answers 503 limiter_unavailable with no row, open mode
	// 200 degraded with the row.
	t.Run("FLT-1b", func(t *testing.T) {
		for _, mode := range []limiter.FailureMode{limiter.Closed, limiter.Open} {
			s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.Policy.FailureMode = mode }})
			api := s.APIs[0]
			controlHit(t, api, "f1b", "before")
			api.Control(t, "/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":1}`)
			since := lastSend(t, api)
			r := api.Hit(t, "f1b", "refused")
			if n := acquiresSent(sendsSince(t, api, since)); n != 0 {
				t.Errorf("mode %v: the refused /hit: %d acquires sent, want 0", mode, n)
			}
			if mode == limiter.Closed {
				wantError(t, "the refused /hit in closed mode", r, http.StatusServiceUnavailable, "limiter_unavailable")
				if n := s.keyRows(t, "f1b", "refused"); n != 0 {
					t.Errorf("closed mode: rows for (f1b, refused) = %d, want 0", n)
				}
			} else {
				if r.Status != http.StatusOK || r.JSON["degraded"] != true || r.JSON["existed"] != false {
					t.Errorf("the refused /hit in open mode: %d %s, want 200 degraded, its row created", r.Status, r.Body)
				}
				if n := s.keyRows(t, "f1b", "refused"); n != 1 {
					t.Errorf("open mode: rows for (f1b, refused) = %d, want 1", n)
				}
			}
			controlHit(t, api, "f1b", "after") // count 1: the next acquire is sent again
		}
	})

	// FLT-2: store pause 4.5s stops the store (SIGSTOP). Requests one at a time, each started
	// while its whole acquire deadline (D = 2 s) ends before the pause does: each whose acquire
	// the observer shows sent gives 503 limiter_outcome_unknown, in both modes; one never sent
	// may only be unavailable (closed 503 limiter_unavailable, open 200 degraded). At least one
	// acquire is sent in each mode, and the outage measures 4,500 to 5,000 ms.
	t.Run("FLT-2", func(t *testing.T) {
		for _, mode := range []limiter.FailureMode{limiter.Closed, limiter.Open} {
			s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.Policy.FailureMode = mode }})
			api := s.APIs[0]
			controlHit(t, api, "f2", "before")
			d := time.Duration(s.Config.Instance.DependencyDeadlineNs)
			target, pid := storeTarget(t, s), s.Store.PID
			ch := startStoreFault(t, func(ctx context.Context) (fixtures.Outage, error) { return fixtures.StorePause(ctx, target, pauseFor) })
			stopped := waitStopped(t, pid)
			sent := 0
			for i := 1; time.Since(stopped)+d+pauseSlack < pauseFor; i++ {
				request := "paused-" + strconv.Itoa(i)
				since := lastSend(t, api)
				r := api.Hit(t, "f2", request)
				switch n := acquiresSent(sendsSince(t, api, since)); {
				case n > 0:
					sent++
					wantError(t, "mode "+mode.String()+": a /hit whose acquire was sent under the pause", r, http.StatusServiceUnavailable, "limiter_outcome_unknown")
				case mode == limiter.Closed:
					wantError(t, "closed mode: a /hit whose acquire was never sent", r, http.StatusServiceUnavailable, "limiter_unavailable")
				default:
					if r.Status != http.StatusOK || r.JSON["degraded"] != true {
						t.Errorf("open mode: a /hit whose acquire was never sent: %d %s, want 200 degraded", r.Status, r.Body)
					}
				}
			}
			if sent == 0 {
				t.Errorf("mode %v: no acquire sent under the pause, want at least one", mode)
			}
			t.Logf("mode %v: %d acquires sent under the pause", mode, sent)
			awaitStoreFault(t, ch, "store pause 4.5s ("+mode.String()+")", pauseFor)
		}
	})

	// FLT-3: drop_reply on acquire calls the real adapter (the acquire is sent and the store
	// deducts) and drops the reply: 503 limiter_outcome_unknown, 1 token charged, no row.
	t.Run("FLT-3", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		api.Control(t, "/_test/faults", `{"target":"store","mode":"drop_reply","match":"acquire","count":1}`)
		since := lastSend(t, api)
		wantError(t, "the /hit whose acquire reply was dropped", api.Hit(t, "f3", "r1"), http.StatusServiceUnavailable, "limiter_outcome_unknown")
		if n := acquiresSent(sendsSince(t, api, since)); n == 0 {
			t.Error("the /hit whose acquire reply was dropped: no acquire sent, want it sent (the store deducted)")
		}
		if u := s.Balance(t, "f3"); u != 19*token {
			t.Errorf("rladmin inspect --key f3: u = %d, want %d (1 token charged)", u, 19*token)
		}
		if n := s.keyRows(t, "f3", "r1"); n != 0 {
			t.Errorf("rows for (f3, r1) = %d, want 0", n)
		}
	})

	// FLT-4: with refuse_after_acquire_bp = 10000 every /hit is refused after its acquire
	// (ERR-15) and its permit released; drop_reply on refund drops the first refund's reply after
	// the store applied it, so the worker retries: the refund record is already_applied after at
	// least 2 attempts, and the bucket is back to its 20 tokens.
	t.Run("FLT-4", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.API.RefuseAfterAcquireBP = 10_000 }})
		api := s.APIs[0]
		api.Control(t, "/_test/faults", `{"target":"store","mode":"drop_reply","match":"refund","count":1}`)
		wantError(t, "the refused /hit", api.Hit(t, "f4", "r1"), http.StatusServiceUnavailable, "service_aborted_after_acquire")
		rec := refundOf(t, api, deductionOf(t, api, "f4"))
		if rec["result"] != "already_applied" || Int(t, rec, "attempts") < 2 {
			t.Errorf("the refund record %v, want result already_applied after at least 2 attempts", rec)
		}
		if u := s.Balance(t, "f4"); u != 20*token {
			t.Errorf("rladmin inspect --key f4: u = %d, want %d (the refund applied once)", u, 20*token)
		}
	})

	// FLT-6: api kill 2 --restart, as faultctl and the orchestrator run it (fixtures.KillAPI):
	// the pre-kill call, then a pre_kill line read_ok:true with both drop counts 0 in
	// api-2.jsonl, SIGKILL, and the instance restarted on its address. Instances 1 and 3 keep
	// serving (200), and the receipts they issued before the kill, whose refunds a barrier held
	// across it (after the kill and r2, before the release: 18 tokens, r1 and r2 both charged,
	// and no refund record for r1's deduction yet), still refund (applied); the restarted
	// instance serves too.
	t.Run("FLT-6", func(t *testing.T) {
		s := StartStack(t, StackOpts{Instances: 3, Clock: Frozen})
		api1, api2, api3 := s.APIs[0], s.APIs[1], s.APIs[2]
		for _, a := range []*API{api1, api3} {
			a.Control(t, "/_test/barrier", `{"name":"held","at":"before_send","match":"refund"}`)
			a.Control(t, "/_test/faults", `{"target":"db","mode":"fail_before","count":1}`)
			user := "f6-" + strconv.Itoa(a.N)
			wantError(t, "hitapi "+strconv.Itoa(a.N)+"'s refunded /hit", a.Hit(t, user, "r1"), http.StatusServiceUnavailable, "database_unavailable")
		}
		ids := map[*API]string{api1: deductionOf(t, api1, "f6-1"), api3: deductionOf(t, api3, "f6-3")}

		old := api2.child
		target := fixtures.APITarget{
			BaseURL: api2.URL, PID: old.PID, LogPath: api2.LogPath, Client: s.HTTP,
			PrekillTimeout: time.Duration(s.Config.Instance.MaxReleaseDelayNs) + 2*time.Second,
			Start:          func(context.Context) (int, error) { return s.restartAPI(t, api2) },
		}
		newPID, err := fixtures.KillAPI(t.Context(), target, true)
		if err != nil {
			t.Fatalf("api kill 2 --restart: %v", err)
		}
		if !old.exitedWithin(killWait) {
			t.Fatalf("hitapi 2's process %d still runs after the kill", old.PID)
		}
		var exit *exec.ExitError
		if !errors.As(old.waitErr, &exit) {
			t.Errorf("hitapi 2's process %d ended with %v, want SIGKILL", old.PID, old.waitErr)
		} else if ws, ok := exit.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("hitapi 2's process %d ended with %v, want SIGKILL", old.PID, old.waitErr)
		}
		old.checkRecord(t)
		if newPID == old.PID || newPID != api2.PID {
			t.Errorf("the restarted hitapi 2 is process %d (the stack's %d), want a new process (not %d)", newPID, api2.PID, old.PID)
		}
		var preKill map[string]any
		for _, rec := range api2.Records(t, "lifecycle") {
			if rec["event"] == "pre_kill" {
				preKill = rec
			}
		}
		if preKill == nil || preKill["read_ok"] != true || Int(t, preKill, "listener_drops") != 0 || Int(t, preKill, "log_drops") != 0 {
			t.Errorf("api-2.jsonl's pre_kill line %v, want read_ok true, listener_drops 0, log_drops 0", preKill)
		}

		for _, a := range []*API{api1, api3} {
			user := "f6-" + strconv.Itoa(a.N)
			wantCreated(t, "hitapi "+strconv.Itoa(a.N)+" after the kill", a.Hit(t, user, "r2"), user, "r2")
			if u := s.Balance(t, user); u != 18*token {
				t.Errorf("rladmin inspect --key %s before the barrier's release: u = %d, want %d (r1's refund held across the kill, r2 charged)",
					user, u, 18*token)
			}
			for _, rec := range a.Records(t, "refund") {
				if rec["deduction_id"] == ids[a] {
					t.Errorf("hitapi %d's refund record %v before the barrier's release, want none yet: the barrier holds r1's refund", a.N, rec)
				}
			}
			a.Control(t, "/_test/barrier/release", `{"name":"held"}`)
			if rec := refundOf(t, a, ids[a]); rec["result"] != "applied" {
				t.Errorf("hitapi %d's refund of its receipt issued before the kill: %v, want applied", a.N, rec)
			}
			if u := s.Balance(t, user); u != 19*token {
				t.Errorf("rladmin inspect --key %s: u = %d, want %d (r1 refunded, r2 charged)", user, u, 19*token)
			}
		}
		wantCreated(t, "the restarted hitapi 2", api2.Hit(t, "f6-2", "r1"), "f6-2", "r1")
	})

	// FLT-7: the tamper tool deletes the ix member, corrupts a bucket field, or wipes the store:
	// the key's next /hit is 503 limiter_state_integrity_error, with no row.
	t.Run("FLT-7", func(t *testing.T) {
		for _, op := range [][]string{
			{"del-ix", "--key", "f7"},
			{"corrupt-field", "--key", "f7", "--field", "u", "--value", "x"},
			{"wipe"},
		} {
			s := StartStack(t, StackOpts{Clock: Frozen})
			api := s.APIs[0]
			wantCreated(t, "the /hit before tamper "+op[0], api.Hit(t, "f7", "r1"), "f7", "r1")
			tamper(t, s, api, op...)
			wantError(t, "the /hit after tamper "+op[0], api.Hit(t, "f7", "r2"), http.StatusServiceUnavailable, "limiter_state_integrity_error")
			if n := s.keyRows(t, "f7", "r2"); n != 0 {
				t.Errorf("tamper %s: rows for (f7, r2) = %d, want 0", op[0], n)
			}
		}
	})

	// FLT-8: with max_tracked_buckets = 2 the third user's /hit is 503
	// limiter_capacity_exceeded; the first two users' buckets are still there, nothing evicted.
	t.Run("FLT-8", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.Policy.MaxTrackedBuckets = 2 }})
		api := s.APIs[0]
		wantCreated(t, "the first user", api.Hit(t, "f8-a", "r1"), "f8-a", "r1")
		wantCreated(t, "the second user", api.Hit(t, "f8-b", "r1"), "f8-b", "r1")
		wantError(t, "the third user", api.Hit(t, "f8-c", "r1"), http.StatusServiceUnavailable, "limiter_capacity_exceeded")
		for _, user := range []string{"f8-a", "f8-b"} {
			if u := s.Balance(t, user); u != 19*token {
				t.Errorf("rladmin inspect --key %s: u = %d, want %d: still tracked, one token charged", user, u, 19*token)
			}
		}
		if n := s.keyRows(t, "f8-c", "r1"); n != 0 {
			t.Errorf("rows for (f8-c, r1) = %d, want 0", n)
		}
	})

	// FLT-10: the signer wrapper fails after the confirmed deduction: 503 limiter_receipt_error,
	// 1 token charged (ERR-12c), no row.
	t.Run("FLT-10", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		api.Control(t, "/_test/faults", `{"target":"signer","mode":"fail","count":1}`)
		wantError(t, "the /hit whose signing failed", api.Hit(t, "f10", "r1"), http.StatusServiceUnavailable, "limiter_receipt_error")
		if u := s.Balance(t, "f10"); u != 19*token {
			t.Errorf("rladmin inspect --key f10: u = %d, want %d (1 token charged)", u, 19*token)
		}
		if n := s.keyRows(t, "f10", "r1"); n != 0 {
			t.Errorf("rows for (f10, r1) = %d, want 0", n)
		}
	})

	// FLT-11: the tamper tool deletes the high-water mark: every key's /hit is 503
	// limiter_state_integrity_error (epoch-wide), and /health shows reason state_lost.
	t.Run("FLT-11", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		wantCreated(t, "the /hit before del-hwm", api.Hit(t, "f11", "r1"), "f11", "r1")
		tamper(t, s, api, "del-hwm")
		wantError(t, "the same key after del-hwm", api.Hit(t, "f11", "r2"), http.StatusServiceUnavailable, "limiter_state_integrity_error")
		wantError(t, "another key after del-hwm", api.Hit(t, "f11-other", "r1"), http.StatusServiceUnavailable, "limiter_state_integrity_error")
		deadline := time.Now().Add(stageWait)
		for h := api.Health(t); h["reason"] != "state_lost"; h = api.Health(t) {
			if time.Now().After(deadline) {
				t.Fatalf("/health %v %v after del-hwm, want reason state_lost", stageWait, h)
			}
			time.Sleep(pollEvery)
		}
	})

	// A barrier before_send on acquire holds a /hit: inside the instance (in_flight 1), no
	// answer within 200 ms, its acquire not sent; released, it is sent and answers 200.
	t.Run("barrier", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		controlHit(t, api, "fb", "before")
		api.Control(t, "/_test/barrier", `{"name":"fix1","at":"before_send","match":"acquire"}`)
		since := lastSend(t, api)
		held := api.Go(t, http.MethodGet, "/hit", IDs("fb", "held"), "")
		deadline := time.Now().Add(stageWait)
		for Int(t, api.Health(t), "in_flight") != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("the held /hit never showed in /health in_flight within %v", stageWait)
			}
			time.Sleep(pollEvery)
		}
		select {
		case a := <-held:
			t.Fatalf("the /hit held at the barrier answered %d %s (%v) before its release", a.Status, a.Body, a.Err)
		case <-time.After(holdFor):
		}
		if n := acquiresSent(sendsSince(t, api, since)); n != 0 {
			t.Errorf("the held /hit: %d acquires sent while it was held, want 0", n)
		}
		api.Control(t, "/_test/barrier/release", `{"name":"fix1"}`)
		wantCreated(t, "the released /hit", Wait(t, held, stageWait, "the released /hit"), "fb", "held")
		if n := acquiresSent(sendsSince(t, api, since)); n == 0 {
			t.Error("the released /hit: no acquire sent, want it sent once released")
		}
	})
}
