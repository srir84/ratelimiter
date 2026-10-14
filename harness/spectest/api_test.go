//go:build rltest

package spectest

import (
	"context"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// TestT_API_1 (SPEC T-API-1 [D]; API-1, API-7c, API-12, API-21, API-22, REF-7; plan S9 Tests
// (b)) in both modes, each on its own driver and epoch: instance 0 at D = 500 ms; instance 1
// behind a respproxy (ViaProxy) at D = 2 s. A first allowed acquire creates the key's bucket and
// loads the script.
//   - (a) A before_send barrier holds an acquire past its 10 ms host deadline: cancelled, refused
//     in both modes, no acquire written (Sent), the key's inspect unchanged.
//   - (b) An allowed permit of another key is held open through (b), so the earlier
//     rl_slots_in_use is 1, 1 and a slot freed too often shows as well as one kept. A 50 ms host
//     deadline while the store is paused for 300 ms (under D, so the connection stays up):
//     outcome_unknown, its command sent; rl_slots_in_use is back to its earlier value when
//     Acquire returns. Once the store resumes, inspect shows the late allow charged (s + 1, u one
//     token lower at the frozen clock) and, the late reply drained, the slot count is still the
//     earlier value.
//   - (c) A 1 h host deadline, later than D: allowed (capped, not rejected).
//   - (d) Hang() on instance 1's proxy once its 8 call connections are up; when
//     rl_store_connections{kind="call"} is 0, an acquire with a 10 s host deadline ends at D as
//     unavailable (closed) or degraded (open), with nothing sent, returning [1.9 s, 2.3 s] after
//     the hang began.
func TestT_API_1(t *testing.T) {
	p := driver.DefaultPolicy()
	slow := driver.DefaultInstance(p)
	slow.DependencyDeadlineNs = 2_000_000_000
	specs := []driver.InstanceSpec{{}, {ViaProxy: true, Instance: &slow}}
	s9BothModes(t, 2, nil, specs, func(t *testing.T, d *driver.Driver, mode limiter.FailureMode) {
		const key = "api1"
		d.Done(allowed(t, d, 0, key))

		// (a)
		before := d.Inspect(key)
		arm(t, d, 0, fixtures.Fault{Mode: fixtures.Barrier, Match: "acquire", Count: 1, At: fixtures.BeforeSend, Name: "api1a"})
		mark := len(d.Sent(0))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		op := d.AcquireContext(ctx, 0, key)
		cancel()
		d.StoreWrapper(0).Release("api1a")
		if op.Kind != limiter.Cancelled || op.Permit != nil {
			t.Errorf("(a) held past a 10 ms deadline before the send: %v %+v (permit %v), want cancelled", op.Kind, op.Diag, op.Permit != nil)
		}
		if n := driver.Count(d.Sent(0)[mark:], "acquire"); n != 0 {
			t.Errorf("(a) %d acquire commands written, want none", n)
		}
		if after := d.Inspect(key); !sameBucket(after, before) {
			t.Errorf("(a) inspect %+v, want unchanged %+v", after, before)
		}

		// (b)
		held := allowed(t, d, 0, "api1-held") // done after (b)'s last slot check
		before = d.Inspect(key)
		r0, f0 := slotsInUse(t, d, 0)
		if r0 != 1 || f0 != 1 {
			t.Errorf("(b) rl_slots_in_use with one permit held: receipt %v, refund %v; want 1, 1 (one slot of each kind, DESIGN §A5.5)", r0, f0)
		}
		st := d.Store()
		// Registered after the driver's teardown, so it runs first: a test that failed while the
		// store was paused resumes it for the teardown (Resume of a store not paused does nothing).
		t.Cleanup(func() { _ = st.Resume() })
		if err := st.Pause(); err != nil {
			t.Fatal(err)
		}
		paused := time.Now()
		ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
		op = d.AcquireContext(ctx, 0, key)
		cancel()
		r1, f1 := slotsInUse(t, d, 0)
		if op.Kind != limiter.OutcomeUnknown || op.Permit != nil || op.SentCommands != 1 {
			t.Errorf("(b) 50 ms deadline, store paused: %v %+v (permit %v, SentCommands %d), want outcome_unknown, sent",
				op.Kind, op.Diag, op.Permit != nil, op.SentCommands)
		}
		if r1 != r0 || f1 != f0 {
			t.Errorf("(b) rl_slots_in_use when Acquire returned: receipt %v, refund %v; want the earlier %v, %v", r1, f1, r0, f0)
		}
		time.Sleep(time.Until(paused.Add(300 * time.Millisecond)))
		if err := st.Resume(); err != nil {
			t.Fatal(err)
		}
		var after driver.Inspect
		charged := func() bool {
			after = d.Inspect(key)
			return after.S == before.S+1 && after.U == before.U-token(d.Policy())
		}
		if !waitUntil(time.Now().Add(waitMax), charged) {
			t.Errorf("(b) inspect after the resume: s %d, u %d; want the late allow charged: s %d, u %d", after.S, after.U, before.S+1, before.U-token(d.Policy()))
		}
		time.Sleep(100 * time.Millisecond) // the store has written the late reply; let the reader drain it
		if r2, f2 := slotsInUse(t, d, 0); r2 != r0 || f2 != f0 {
			t.Errorf("(b) rl_slots_in_use after the late allow: receipt %v, refund %v; want the earlier %v, %v (unchanged)", r2, f2, r0, f0)
		}
		d.Done(held)

		// (c)
		ctx, cancel = context.WithTimeout(t.Context(), time.Hour)
		op = d.AcquireContext(ctx, 0, key)
		cancel()
		if op.Kind != limiter.Allowed {
			t.Errorf("(c) 1 h deadline: %v %+v, want allowed (capped at D, not rejected)", op.Kind, op.Diag)
		}
		d.Done(op)

		// (d)
		waitMetric(t, d, 1, "rl_store_connections", "call", waitMax, "(d) instance 1's call connections", func(v float64) bool { return v == 8 })
		mark = len(d.Sent(1))
		// Before the hang, so every call slot redials after start: a redial's AUTH timeout, which
		// wakes the waiting acquire to fail fast (DESIGN §A5.4), comes D after start or later.
		start := time.Now()
		d.Proxy(1).Hang()
		waitMetric(t, d, 1, "rl_store_connections", "call", time.Second, "(d) after Hang", func(v float64) bool { return v == 0 })
		ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
		op = d.AcquireContext(ctx, 1, key)
		took := time.Since(start)
		cancel()
		finishDegraded(op)
		if !notSentRefusal(op, mode) {
			t.Errorf("(d) connect hangs, 10 s deadline: %v %+v (SentCommands %d), want %s with nothing sent", op.Kind, op.Diag, op.SentCommands, refusalName(mode))
		}
		if took < 1900*time.Millisecond || took > 2300*time.Millisecond {
			t.Errorf("(d) the acquire returned %v after the hang began, want within [1.9 s, 2.3 s] (D = 2 s)", took)
		}
		if n := driver.Count(d.Sent(1)[mark:], "acquire"); n != 0 {
			t.Errorf("(d) %d acquire commands written, want none", n)
		}
	})
}

// refusalName names notSentRefusal's result in mode.
func refusalName(mode limiter.FailureMode) string {
	if mode == limiter.Open {
		return "degraded (no deduction_id)"
	}
	return "unavailable"
}

// apiErrorLines are T-API-2's injected error replies (plan S9 Tests (b)): read-only, unknown
// command, a script runtime error and BUSYKEY, worded as the store words them.
func apiErrorLines() []string {
	return []string{
		"READONLY You can't write against a read only replica.",
		"ERR unknown command 'EVALSHA', with args beginning with: ",
		"ERR user_script:1: Script attempted to access nonexistent global variable 'x' script: " + scripts.Acquire.SHA + ", on @user_script:1.",
		"BUSYKEY Target key name already exists.",
	}
}

// TestT_API_2 (SPEC T-API-2 [D]; API-22, API-12, API-11, API-21; plan S9 Tests (b)) in both
// modes, each on its own driver and epoch with one instance behind a respproxy (ViaProxy), D =
// 500 ms, its epoch read once at startup and timed cleanup off (quiet), so it writes only the
// test's acquires. A first allowed acquire creates the key's bucket and loads the script.
//   - Error replies injected at the proxy for acquire (READONLY, ERR unknown command, a script
//     runtime ERR, BUSYKEY): each acquire is outcome_unknown, never degraded, its command written
//     once, the reply in Diag.StoreError; inspect unchanged (the store never ran it).
//   - BUSY: rl_ops sets busy-reply-threshold to 100 ms and rl_fixture runs a 1.5 s Lua loop on its
//     own connection; once the store answers BUSY (an rl_ops PING shows it), an acquire is
//     unavailable (closed) or degraded (open) with nothing sent and BUSY as its StoreError's
//     first word; inspect unchanged.
//   - The store stopped and restarted with its data kept (empty script cache): an acquire every
//     100 ms until one is allowed (≤ 5 s); the earlier ones are unavailable or degraded with
//     nothing sent, and the allowed one wrote, AUTH and PING left out, acquire, SCRIPT LOAD,
//     acquire (one reload, then the call succeeds).
//   - LOADING: LoadPadding to the §A7 size, then Store().Restart(false), and an acquire every 2 ms
//     until one comes back refused with LOADING as its StoreError's first word: unavailable or
//     degraded with nothing sent; inspect unchanged once the store has loaded.
func TestT_API_2(t *testing.T) {
	s9BothModes(t, 1, quiet, []driver.InstanceSpec{{ViaProxy: true}}, func(t *testing.T, d *driver.Driver, mode limiter.FailureMode) {
		const key = "api2"
		d.Done(allowed(t, d, 0, key))

		for _, line := range apiErrorLines() {
			if err := d.Proxy(0).InjectError(scripts.Acquire.Name, line, 1); err != nil {
				t.Fatal(err)
			}
			before := d.Inspect(key)
			mark := len(d.Sent(0))
			op := d.Acquire(0, key)
			finishDegraded(op)
			if op.Kind != limiter.OutcomeUnknown || op.Permit != nil || op.SentCommands != 1 || op.Diag.StoreError != line {
				t.Errorf("error reply %q: %v %+v (permit %v, SentCommands %d), want outcome_unknown, sent, with the reply in StoreError",
					line, op.Kind, op.Diag, op.Permit != nil, op.SentCommands)
			}
			if n := driver.Count(d.Sent(0)[mark:], "acquire"); n != 1 {
				t.Errorf("error reply %q: %d acquire commands written, want 1 (no retry)", line, n)
			}
			if after := d.Inspect(key); !sameBucket(after, before) {
				t.Errorf("error reply %q: inspect %+v, want unchanged %+v", line, after, before)
			}
		}

		api2Busy(t, d, mode, key)
		api2NoScript(t, d, mode, key)
		api2Loading(t, d, mode, key)
	})
}

// api2Busy is T-API-2's BUSY step (plan S9 Tests (b): DESIGN §C2's technique).
func api2Busy(t *testing.T, d *driver.Driver, mode limiter.FailureMode, key string) {
	t.Helper()
	st := d.Store()
	ops, err := st.Conn("rl_ops")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ops.Close() }()
	setThreshold := func(ms string) bool {
		t.Helper()
		if r, err := ops.Do("CONFIG", "SET", "busy-reply-threshold", ms); err != nil || r.Kind != '+' {
			t.Errorf("CONFIG SET busy-reply-threshold %s as rl_ops: %v %v", ms, r, err)
			return false
		}
		return true
	}
	if !setThreshold("100") {
		t.FailNow()
	}
	defer setThreshold("5000") // the store's default
	fx, err := st.Conn("rl_fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fx.Close() }()
	before := d.Inspect(key)
	type evalResult struct {
		r   storeproc.Reply
		err error
	}
	eval := make(chan evalResult, 1)
	go func() {
		r, err := fx.Do("EVAL", "local s=os.clock() while os.clock()-s<1.5 do end return 0", "0")
		eval <- evalResult{r, err}
	}()
	var probe storeproc.Reply
	busy := func() bool {
		r, err := ops.Do("PING")
		probe = r
		return err == nil && r.Kind == '-' && firstWord(r.Str) == "BUSY"
	}
	if !waitUntil(time.Now().Add(time.Second), busy) {
		t.Fatalf("BUSY: the store still answers %v a second after the 1.5 s script was sent", probe)
	}
	op := d.Acquire(0, key)
	finishDegraded(op)
	if !notSentRefusal(op, mode) || firstWord(op.Diag.StoreError) != "BUSY" {
		t.Errorf("BUSY: acquire %v %+v (SentCommands %d), want %s with nothing sent and BUSY in StoreError", op.Kind, op.Diag, op.SentCommands, refusalName(mode))
	}
	select {
	case res := <-eval:
		if res.err != nil || res.r.Kind != ':' || res.r.Int != 0 {
			t.Errorf("BUSY: the 1.5 s script replied %v %v, want 0", res.r, res.err)
		}
	case <-time.After(waitMax):
		t.Fatal("BUSY: the 1.5 s script did not reply")
	}
	if after := d.Inspect(key); !sameBucket(after, before) {
		t.Errorf("BUSY: inspect %+v, want unchanged %+v", after, before)
	}
}

// api2NoScript is T-API-2's empty-script-cache step: the store restarted with its data kept.
func api2NoScript(t *testing.T, d *driver.Driver, mode limiter.FailureMode, key string) {
	t.Helper()
	d.Store().Stop()
	d.RestartStore()
	start := time.Now()
	for n := 1; ; n++ {
		mark := len(d.Sent(0))
		op := d.Acquire(0, key)
		if op.Kind == limiter.Allowed {
			var cmds []string
			for _, s := range d.Sent(0)[mark:] {
				if s.Bytes > 0 && s.Cmd != "AUTH" && s.Cmd != "PING" {
					cmds = append(cmds, s.Cmd)
				}
			}
			if len(cmds) != 3 || cmds[0] != "acquire" || cmds[1] != "SCRIPT LOAD" || cmds[2] != "acquire" || !op.NoScript || op.SentCommands != 2 {
				t.Errorf("the first allowed acquire after the restart wrote %q (NoScript %v, SentCommands %d), want acquire, SCRIPT LOAD, acquire",
					cmds, op.NoScript, op.SentCommands)
			}
			d.Done(op)
			return
		}
		finishDegraded(op)
		if !notSentRefusal(op, mode) {
			t.Fatalf("acquire %d after the restart: %v %+v (SentCommands %d), want %s with nothing sent until one is allowed",
				n, op.Kind, op.Diag, op.SentCommands, refusalName(mode))
		}
		next := start.Add(time.Duration(n) * 100 * time.Millisecond)
		if next.After(start.Add(5 * time.Second)) {
			t.Fatalf("no allowed acquire within 5 s of the restart (%d refused)", n)
		}
		time.Sleep(time.Until(next))
	}
}

// loadingTries bounds the restarts of T-API-2's LOADING step (plan choice): a restart whose
// loading ends before the instance has reconnected shows no LOADING reply, and the step restarts
// the store again, at most this many times in all.
const loadingTries = 3

// api2Loading is T-API-2's LOADING step (plan S9 Tests (b): LoadPadding, then a restart that does
// not wait for the data; the S1 technique, DESIGN §C2).
func api2Loading(t *testing.T, d *driver.Driver, mode limiter.FailureMode, key string) {
	t.Helper()
	st := d.Store()
	if err := st.LoadPadding(storeproc.PaddingA7); err != nil {
		t.Fatal(err)
	}
	for try := 1; try <= loadingTries; try++ {
		before := d.Inspect(key)
		if err := st.Restart(false); err != nil {
			t.Fatal(err)
		}
		restarted := time.Now()
		op, seen := pollLoading(t, d, mode, key)
		answered := time.Since(restarted)
		d.ReconnectStore()
		t.Logf("restart %d: the store answered %v after Restart(false) returned (LOADING %v); loaded %v after", try, answered, seen, time.Since(restarted))
		if seen {
			if after := d.Inspect(key); !sameBucket(after, before) {
				t.Errorf("LOADING: inspect %+v, want unchanged %+v", after, before)
			}
			return
		}
		d.Done(op) // the store loaded before the instance reconnected: restart again
	}
	t.Fatalf("no acquire came back with LOADING in %d restarts after LoadPadding", loadingTries)
}

// pollLoading acquires key every 2 ms, right after a restart that did not wait for the data, until a
// result shows the store answered: LOADING (reported true), or an allowed acquire once the data
// was loaded (false). Results with nothing sent and no error reply (the redial backoff) go on;
// anything else fails the test, as does no answer within waitMax.
func pollLoading(t *testing.T, d *driver.Driver, mode limiter.FailureMode, key string) (driver.Op, bool) {
	t.Helper()
	deadline := time.Now().Add(waitMax)
	for {
		op := d.Acquire(0, key)
		finishDegraded(op)
		switch {
		case op.Kind == limiter.Allowed:
			return op, false
		case notSentRefusal(op, mode) && firstWord(op.Diag.StoreError) == "LOADING":
			return op, true
		case !notSentRefusal(op, mode) || op.Diag.StoreError != "":
			t.Fatalf("acquire while the store restarts: %v %+v (SentCommands %d), want %s with nothing sent, LOADING, or allowed",
				op.Kind, op.Diag, op.SentCommands, refusalName(mode))
		case !time.Now().Before(deadline):
			t.Fatalf("no acquire reached the restarted store within %v", waitMax)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
