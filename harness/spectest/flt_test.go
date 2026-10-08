//go:build rltest

package spectest

import (
	"fmt"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/limiter"
)

// stopStore stops d's store and registers a cleanup that restarts it (data kept) unless the test
// did; the returned func marks it restarted. The cleanup is registered after the driver's
// teardown, so it runs first: a test that failed while the store was down starts it again for
// the teardown audit.
func stopStore(t *testing.T, d *driver.Driver) (restarted func()) {
	t.Helper()
	down := true
	t.Cleanup(func() {
		if down {
			d.RestartStore()
		}
	})
	d.Store().Stop()
	return func() { down = false }
}

// TestT_FLT_1 (SPEC T-FLT-1; API-20a, API-22, API-11; HARNESS FLT-1; plan S9 Tests (b)), closed
// mode, D = 500 ms, each part on its own driver.
//   - (a) A real closed store port, no wrapper (the production adapter's classification): the
//     store is stopped, and once rl_store_connections{kind="call"} is 0 (commands in flight at a
//     Stop may be outcome_unknown, so the test waits for the connections to drop), 10 acquires of
//     a new key are each unavailable with nothing sent; no acquire is written. After the restart
//     (data kept) inspect shows no charge: the key has no bucket.
//   - (b) The store wrapper's refuse fault (FLT-1 b): unavailable, no acquire written, the key's
//     inspect unchanged.
func TestT_FLT_1(t *testing.T) {
	t.Run("a", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, nil)
		const key = "flt1a"
		d.Done(allowed(t, d, 0, "flt1a-warm"))
		mark := len(d.Sent(0))
		restarted := stopStore(t, d)
		waitMetric(t, d, 0, "rl_store_connections", "call", waitMax, "(a) after Stop", func(v float64) bool { return v == 0 })
		for n := range 10 {
			if op := d.Acquire(0, key); !notSentRefusal(op, limiter.Closed) {
				t.Errorf("(a) acquire %d with the store's port closed: %v %+v (SentCommands %d), want unavailable with nothing sent",
					n+1, op.Kind, op.Diag, op.SentCommands)
			}
		}
		if c := driver.Count(d.Sent(0)[mark:], "acquire"); c != 0 {
			t.Errorf("(a) %d acquire commands written while the store was down, want none", c)
		}
		d.RestartStore()
		restarted()
		if in := d.Inspect(key); in.Code != 40 || in.Present != 0 {
			t.Errorf("(a) inspect after the restart: %+v, want no bucket (no charge)", in)
		}
	})
	t.Run("b", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, nil)
		const key = "flt1b"
		d.Done(allowed(t, d, 0, key))
		before := d.Inspect(key)
		arm(t, d, 0, fixtures.Fault{Mode: fixtures.Refuse, Match: "acquire", Count: 1})
		mark := len(d.Sent(0))
		if op := d.Acquire(0, key); !notSentRefusal(op, limiter.Closed) {
			t.Errorf("(b) acquire refused by the wrapper: %v %+v (SentCommands %d), want unavailable with nothing sent", op.Kind, op.Diag, op.SentCommands)
		}
		if c := driver.Count(d.Sent(0)[mark:], "acquire"); c != 0 {
			t.Errorf("(b) %d acquire commands written, want none", c)
		}
		if after := d.Inspect(key); !sameBucket(after, before) {
			t.Errorf("(b) inspect %+v, want unchanged %+v", after, before)
		}
	})
}

// TestT_FLT_2 (SPEC T-FLT-2; API-20b, API-5, API-17, API-23; HARNESS FLT-1; plan S9 Tests (b)),
// open mode, D = 500 ms: 5 permits are held when the store stops for 2 s. Once
// rl_store_connections{kind="call"} is 0 they are released, and 3 new acquires are each degraded:
// a permit whose DeductionID() is "", nothing sent, counted under
// rl_acquire_results_total{result="degraded"} (+3). Until the restart no applied or
// already_applied event arrives (the refunds are retried, never reported successful); after the
// restart (data kept) all 5 are applied.
func TestT_FLT_2(t *testing.T) {
	d := s9Driver(t, 1, limiter.Open, nil)
	held := make([]driver.Op, 5)
	for k := range held {
		held[k] = allowed(t, d, 0, fmt.Sprintf("flt2-%d", k))
	}
	stopped := time.Now()
	restarted := stopStore(t, d)
	waitMetric(t, d, 0, "rl_store_connections", "call", waitMax, "after Stop", func(v float64) bool { return v == 0 })
	for _, op := range held {
		d.Release(op)
	}
	degraded := metric(t, d, 0, "rl_acquire_results_total", "degraded")
	const n = 3
	for k := range n {
		op := d.Acquire(0, "flt2-new")
		finishDegraded(op)
		if !notSentRefusal(op, limiter.Open) {
			t.Errorf("acquire %d during the outage: %v %+v (SentCommands %d), want degraded with no deduction_id, nothing sent",
				k+1, op.Kind, op.Diag, op.SentCommands)
		}
	}
	if got := metric(t, d, 0, "rl_acquire_results_total", "degraded"); got != degraded+n {
		t.Errorf(`rl_acquire_results_total{result="degraded"} = %v after %d degraded acquires, want %v`, got, n, degraded+n)
	}
	time.Sleep(time.Until(stopped.Add(2 * time.Second)))
	for _, ev := range d.RefundEvents(0) {
		if confirmed(ev) {
			t.Errorf("during the outage: event %+v, want no applied or already_applied", ev)
		}
	}
	d.RestartStore()
	restarted()
	for _, op := range held {
		if ev := event(t, d, op); ev.Result != limiter.Applied {
			t.Errorf("refund of %s after the restart: %+v, want applied", op.Key, ev)
		}
	}
}

// TestT_FLT_3 (SPEC T-FLT-3; API-12, API-22; HARNESS FLT-2; plan S9 Tests (b)) in both modes,
// each on its own driver and epoch, D = 500 ms, quiet (only the test's acquires are written):
// the store is paused (SIGSTOP) for 1.5 s. During the first 0.85 s an acquire of a new key
// starts every 25 ms on its own goroutine, so every call has ended (within D) before the store
// resumes. Commands written to the call connections wait; each connection closes once its
// oldest command has waited D, and the redials' AUTH cannot complete while the store is paused,
// so later calls send nothing. Every call whose command was sent (SentCommands > 0: the
// adapter's byte ranges; their total equals the acquire sends the observer heard) is
// outcome_unknown, never degraded; every unsent call is unavailable (closed) or degraded (open).
// Both kinds of call occur.
func TestT_FLT_3(t *testing.T) {
	s9BothModes(t, 1, quiet, nil, func(t *testing.T, d *driver.Driver, mode limiter.FailureMode) {
		d.Done(allowed(t, d, 0, "flt3-warm"))
		waitMetric(t, d, 0, "rl_store_connections", "call", waitMax, "warm-up", func(v float64) bool { return v == 8 })
		st := d.Store()
		t.Cleanup(func() { _ = st.Resume() }) // runs before the driver's teardown
		mark := len(d.Sent(0))
		if err := st.Pause(); err != nil {
			t.Fatal(err)
		}
		paused := time.Now()
		var calls []<-chan driver.Op
		for k := 0; k*25 <= 850; k++ {
			time.Sleep(time.Until(paused.Add(time.Duration(k) * 25 * time.Millisecond)))
			calls = append(calls, acquireAsync(d, 0, fmt.Sprintf("flt3-%d", k)))
		}
		ops := make([]driver.Op, len(calls))
		for k, ch := range calls { // each call ends within D of its start, so by 1.35 s
			ops[k] = opBy(t, ch, paused.Add(1490*time.Millisecond), fmt.Sprintf("call %d (started at %d ms)", k, k*25))
		}
		written := driver.Count(d.Sent(0)[mark:], "acquire")
		time.Sleep(time.Until(paused.Add(1500 * time.Millisecond)))
		if err := st.Resume(); err != nil {
			t.Fatal(err)
		}
		sent, unsent, sentCommands := 0, 0, 0
		for k, op := range ops {
			finishDegraded(op)
			switch {
			case op.SentCommands > 0:
				sent++
				sentCommands += op.SentCommands
				if op.Kind != limiter.OutcomeUnknown || op.Permit != nil {
					t.Errorf("call %d, its command sent: %v %+v (permit %v), want outcome_unknown, never degraded", k, op.Kind, op.Diag, op.Permit != nil)
				}
			case notSentRefusal(op, mode):
				unsent++
			default:
				t.Errorf("call %d, nothing sent: %v %+v, want %s", k, op.Kind, op.Diag, refusalName(mode))
			}
		}
		t.Logf("%d calls: %d sent, %d unsent", len(ops), sent, unsent)
		if written != sentCommands {
			t.Errorf("the observer heard %d acquire sends, the calls report %d sent commands", written, sentCommands)
		}
		if sent == 0 || unsent == 0 {
			t.Errorf("%d sent and %d unsent calls, want both kinds", sent, unsent)
		}
	})
}

// TestT_FLT_4 (SPEC T-FLT-4; API-12, API-24, REF-7; HARNESS FLT-3; plan S9 Tests (b)) in both
// modes, each on its own driver and epoch, D = 500 ms: a first allowed acquire loads the script
// and creates the key's bucket; with drop_reply armed on acquire (count 1), the next acquire of
// the key is outcome_unknown with no permit and exactly one acquire written (no second acquire),
// and inspect shows the store applied it: s + 1.
func TestT_FLT_4(t *testing.T) {
	s9BothModes(t, 1, nil, nil, func(t *testing.T, d *driver.Driver, _ limiter.FailureMode) {
		const key = "flt4"
		d.Done(allowed(t, d, 0, key))
		before := d.Inspect(key)
		arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "acquire", Count: 1})
		mark := len(d.Sent(0))
		op := d.Acquire(0, key)
		if op.Kind != limiter.OutcomeUnknown || op.Permit != nil {
			t.Errorf("acquire with its reply dropped: %v %+v (permit %v), want outcome_unknown, no permit", op.Kind, op.Diag, op.Permit != nil)
		}
		if n := driver.Count(d.Sent(0)[mark:], "acquire"); n != 1 {
			t.Errorf("%d acquire commands written, want exactly 1", n)
		}
		if after := d.Inspect(key); after.Present != 1 || after.S != before.S+1 {
			t.Errorf("inspect after the lost reply: present %d, s %d; want s %d (the deduction applied)", after.Present, after.S, before.S+1)
		}
	})
}

// TestT_FLT_5 (SPEC T-FLT-5; STA-6a, API-10; HARNESS FLT-8; plan S9 Tests (b)), each limit on its
// own driver (the limits are fingerprinted), closed mode, D = 500 ms. A refusal deducts, credits
// and marks nothing, and the earlier buckets and markers stay as they were (inspect):
//   - tracked_buckets: max_tracked_buckets = 3; three keys are allowed, the 4th key's acquire is
//     capacity_error tracked_buckets and leaves it with no bucket.
//   - live_markers: max_live_markers = max_markers_per_key = 2; one refund on each of 3 keys: the
//     first two are applied, the 3rd's refund reply (per-attempt capture) is {24, t,
//     'live_markers'}.
//   - key_markers: max_markers_per_key = 2, max_live_markers = 10; 3 refunds on one key: the
//     first two are applied, the 3rd's reply is {24, t, 'key_markers'}.
//
// The refused refunds are retried until their retention, so the instance is killed at the end.
func TestT_FLT_5(t *testing.T) {
	t.Run("tracked_buckets", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, func(p *limiter.Policy, _ *limiter.Instance) { p.MaxTrackedBuckets = 3 })
		ks := []string{"flt5-a", "flt5-b", "flt5-c"}
		var before []driver.Inspect
		for _, k := range ks {
			d.Done(allowed(t, d, 0, k))
			before = append(before, d.Inspect(k))
		}
		op := d.Acquire(0, "flt5-d")
		if op.Kind != limiter.CapacityError || op.Diag.Reason != "tracked_buckets" || op.Permit != nil {
			t.Errorf("4th key: %v %+v (permit %v), want capacity_error tracked_buckets", op.Kind, op.Diag, op.Permit != nil)
		}
		if in := d.Inspect("flt5-d"); in.Code != 40 || in.Present != 0 {
			t.Errorf("4th key's inspect: %+v, want no bucket (no deduction)", in)
		}
		for i, k := range ks {
			if in := d.Inspect(k); !sameBucket(in, before[i]) {
				t.Errorf("%s after the refusal: %+v, want unchanged %+v", k, in, before[i])
			}
		}
		if st := d.Status(); st.NB != 3 {
			t.Errorf("status: tracked buckets %d, want 3", st.NB)
		}
	})
	t.Run("live_markers", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, func(p *limiter.Policy, _ *limiter.Instance) { p.MaxLiveMarkers, p.MaxMarkersPerKey = 2, 2 })
		ks := []string{"flt5-a", "flt5-b", "flt5-c"}
		ops := make([]driver.Op, len(ks))
		for i, k := range ks {
			ops[i] = allowed(t, d, 0, k)
		}
		for _, op := range ops[:2] {
			releaseApplied(t, d, op)
		}
		flt5Refused(t, d, ks, ops[2], "live_markers")
	})
	t.Run("key_markers", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, func(p *limiter.Policy, _ *limiter.Instance) { p.MaxLiveMarkers, p.MaxMarkersPerKey = 10, 2 })
		const key = "flt5-k"
		ops := []driver.Op{allowed(t, d, 0, key), allowed(t, d, 0, key), allowed(t, d, 0, key)}
		for _, op := range ops[:2] {
			releaseApplied(t, d, op)
		}
		flt5Refused(t, d, []string{key}, ops[2], "key_markers")
	})
}

// flt5Refused releases op, whose refund the marker limit named reason refuses: its first
// attempt's store reply must be {24, t, reason}, and the keys ks (op's among them) must show
// in inspect exactly what they showed before, with the status's marker count unchanged. Then
// it kills instance 0, whose refund would be retried until its retention.
func flt5Refused(t *testing.T, d *driver.Driver, ks []string, op driver.Op, reason string) {
	t.Helper()
	before := make([]driver.Inspect, len(ks))
	for i, k := range ks {
		before[i] = d.Inspect(k)
	}
	nm := d.Status().NM
	d.Release(op)
	a := firstAttempt(t, d, op)
	storeReply(t, "refund past the "+reason+" limit", a, 24, reason)
	if a.Store.Reply.TUs <= 0 {
		t.Errorf("refund past the %s limit: t %d, want the budget time", reason, a.Store.Reply.TUs)
	}
	for i, k := range ks {
		if in := d.Inspect(k); !sameBucket(in, before[i]) {
			t.Errorf("%s after the %s refusal: %+v, want unchanged %+v (no credit, no marker)", k, reason, in, before[i])
		}
	}
	if got := d.Status().NM; got != nm {
		t.Errorf("status after the %s refusal: %d markers, want %d", reason, got, nm)
	}
	d.Kill(0)
}

// TestT_FLT_6 (SPEC T-FLT-6 [D]; API-7b, RCP-9, STA-6b, REF-10) on the fast profile, two
// instances with their own slot limits (instance settings, not fingerprinted). Held 2 /
// pending 4: with 2 permits open the third acquire is busy and writes no acquire byte, and
// after done() the next is allowed. Held 4 / pending 2: two released permits whose refunds are
// held at a before_send barrier keep both refund slots, so the next acquire is busy with no
// acquire byte written; once the barrier is let go and both final events arrive, it is
// allowed.
func TestT_FLT_6(t *testing.T) {
	p := driver.FastPolicy()
	small := func(held, pending int) *limiter.Instance {
		in := driver.FastInstance(p)
		in.MaxHeldReceipts, in.MaxPendingRefunds = held, pending
		return &in
	}
	d := driver.New(t, driver.Options{
		Instances: 2, Policy: p, Instance: driver.FastInstance(p),
		PerInstance: []driver.InstanceSpec{{Instance: small(2, 4)}, {Instance: small(4, 2)}},
	})
	busy := func(i int, what string) {
		t.Helper()
		mark := len(d.Sent(i))
		if op := d.Acquire(i, "flt6"); op.Kind != limiter.Busy {
			t.Errorf("%s: %v (%+v), want busy", what, op.Kind, op.Diag)
		}
		if n := driver.Count(d.Sent(i)[mark:], "acquire"); n != 0 {
			t.Errorf("%s: %d acquire commands written, want none", what, n)
		}
	}

	a, b := allowed(t, d, 0, "flt6"), allowed(t, d, 0, "flt6")
	busy(0, "held 2 of 2")
	d.Done(a)
	d.Done(allowed(t, d, 0, "flt6"))
	d.Done(b)

	c, e := allowed(t, d, 1, "flt6"), allowed(t, d, 1, "flt6")
	arm(t, d, 1, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 2, At: fixtures.BeforeSend, Name: "flt6"})
	d.Release(c)
	d.Release(e)
	reached(t, d, 1, "flt6")
	busy(1, "pending 2 of 2")
	d.StoreWrapper(1).Release("flt6")
	for _, op := range []driver.Op{c, e} {
		if ev := event(t, d, op); ev.Result != limiter.Applied {
			t.Errorf("held refund: event %+v, want applied", ev)
		}
	}
	d.Done(allowed(t, d, 1, "flt6"))
}
