//go:build rltest

package spectest

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/store"
	"ratelimiter/limiter"
	"ratelimiter/limitertest"
)

// refundWindow is REF-5's bound on the fast profile: refund_deadline 3 s + outage_tolerance
// 0.5 s (plan S6 Pass when).
const refundWindow = 3_500 * time.Millisecond

// arm arms f on instance i's store wrapper.
func arm(t *testing.T, d *driver.Driver, i int, f fixtures.Fault) {
	t.Helper()
	if err := d.StoreWrapper(i).Arm(f); err != nil {
		t.Fatal(err)
	}
}

// armOn arms f on w, a store wrapper of the test's own.
func armOn(t *testing.T, w *fixtures.StoreWrapper, f fixtures.Fault) {
	t.Helper()
	if err := w.Arm(f); err != nil {
		t.Fatal(err)
	}
}

// metOutage checks that op's refund met a refuse outage: at least 2 refund calls, one of them
// refused (unavailable) before the last, the confirming one.
func metOutage(t *testing.T, d *driver.Driver, what string, op driver.Op) {
	t.Helper()
	as := d.RefundAttempts(op)
	refused := false
	for _, a := range as[:max(len(as)-1, 0)] {
		refused = refused || a.Response.Outcome == store.Unavailable
	}
	if len(as) < 2 || !refused {
		codes := make([]any, len(as))
		for i, a := range as {
			codes[i] = a.Code()
		}
		t.Errorf("%s: refund calls %v, want at least 2, one refused (unavailable) by the outage before the confirming one", what, codes)
	}
}

// reached waits up to waitMax for a call to reach barrier name of instance i.
func reached(t *testing.T, d *driver.Driver, i int, name string) {
	t.Helper()
	select {
	case <-d.StoreWrapper(i).Reached(name):
	case <-time.After(waitMax):
		t.Fatalf("no call reached barrier %s of instance %d within %v", name, i, waitMax)
	}
}

// confirmed reports whether ev is a confirmed refund (applied or already_applied).
func confirmed(ev limiter.RefundEvent) bool {
	return ev.Result == limiter.Applied || ev.Result == limiter.AlreadyApplied
}

// within checks REF-5 on ev: confirmed, and FinalMonoNs − ReleaseMonoNs ≤ 3.5 s.
func within(t *testing.T, what string, ev limiter.RefundEvent) {
	t.Helper()
	if !confirmed(ev) {
		t.Errorf("%s: event %+v, want applied or already_applied", what, ev)
	}
	if took := time.Duration(ev.FinalMonoNs - ev.ReleaseMonoNs); took > refundWindow || took < 0 {
		t.Errorf("%s: FinalMonoNs − ReleaseMonoNs = %v, want within %v (REF-5)", what, took, refundWindow)
	}
}

// TestT_REF_1 (SPEC T-REF-1; API-14, REF-4, API-1d, API-4; FLT-4): the first refund reply is
// dropped after the store applied it; the retry returns already_applied, and the one event
// carries the permit's deduction_id, CreditDecisionNs = the dropped reply's t × 1000 (captured
// behind the wrapper) and Attempts 2. One credit in all: of 5 permits taken at a frozen
// instant, the released one brings u from 15 to 16 tokens, with 1 marker. The op log replays in
// the reference model (the lost reply's credit, proven by the marker). Then the original credit
// time is told from the retry's: a second permit's first reply is dropped at the frozen start
// t0 = 1,760,000,000,000,000 µs and its retry held at a before_send barrier while the clock
// moves 1 s, so the retry answers already_applied at t0 + 1 s; its event's CreditDecisionNs is
// t0 × 1000, not the retry's t × 1000, and u is 17 tokens with 2 markers. That part comes
// after the replay check: replay is sequential and puts a lost reply's credit at the clock of
// its refund record, which comes after the step (the teardown's check_bud11 takes the record's
// credit_us).
func TestT_REF_1(t *testing.T) {
	d := fastDriver(t, 1, nil)
	p := d.Policy()
	const key = "ref1"
	warm := allowed(t, d, 0, "ref1-warm") // loads the refund script, so no NOSCRIPT adds a send below
	d.Release(warm)
	event(t, d, warm)
	var ops []driver.Op
	for range 5 {
		ops = append(ops, allowed(t, d, 0, key))
	}
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
	d.Release(ops[0])
	ev := event(t, d, ops[0])
	as := d.RefundAttempts(ops[0])
	if len(as) != 2 {
		t.Fatalf("%d refund calls %+v, want 2: the dropped one and the retry", len(as), as)
	}
	lost := as[0]
	if lost.Response.Outcome != store.OutcomeUnknown || !lost.Reached || lost.Store.Outcome != store.OK || lost.Store.Reply.Code != 3 {
		t.Fatalf("first call: the limiter got %v, the store replied %v code %d; want outcome_unknown over an E_APPLIED reply",
			lost.Response.Outcome, lost.Store.Outcome, lost.Store.Reply.Code)
	}
	if ev.Result != limiter.AlreadyApplied || ev.DeductionID != ops[0].Permit.DeductionID() || ev.Attempts != 2 ||
		ev.CreditDecisionNs != lost.Store.Reply.TUs*1000 {
		t.Errorf("event %+v, want already_applied, deduction_id %s, Attempts 2, CreditDecisionNs %d (the dropped reply's t × 1000)",
			ev, ops[0].Permit.DeductionID(), lost.Store.Reply.TUs*1000)
	}
	if in := d.Inspect(key); in.U != 16*token(p) || in.MC != 1 {
		t.Errorf("Inspect %+v, want u 16 tokens and 1 marker: one credit", in)
	}
	for _, op := range ops[2:] {
		d.Done(op)
	}
	d.CheckRefModel(t)

	const t0 = driver.DefaultStartNs / 1000 // the frozen budget time, in µs
	// take() gives the first refund call the drop and the second, the retry, the barrier.
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.BeforeSend, Name: "ref1-retry"})
	d.Release(ops[1])
	reached(t, d, 0, "ref1-retry")
	step(t, d, 1_000_000_000) // the retry runs 1 s after the credit
	d.StoreWrapper(0).Release("ref1-retry")
	ev = event(t, d, ops[1])
	as = d.RefundAttempts(ops[1])
	if len(as) != 2 {
		t.Fatalf("clock moved: %d refund calls %+v, want 2: the dropped one and the retry", len(as), as)
	}
	lost, retry := as[0], as[1]
	if lost.Response.Outcome != store.OutcomeUnknown || lost.Store.Outcome != store.OK || lost.Store.Reply.Code != 3 || lost.Store.Reply.TUs != t0 {
		t.Fatalf("clock moved: first call: the limiter got %v, the store replied %v code %d at t %d; want outcome_unknown over an E_APPLIED reply at t %d",
			lost.Response.Outcome, lost.Store.Outcome, lost.Store.Reply.Code, lost.Store.Reply.TUs, t0)
	}
	if retry.Response.Outcome != store.OK || retry.Response.Reply.Code != 4 || retry.Response.Reply.TUs != t0+1_000_000 {
		t.Fatalf("clock moved: retry: %v code %d at t %d, want E_ALREADY_APPLIED at t %d", retry.Response.Outcome, retry.Response.Reply.Code,
			retry.Response.Reply.TUs, t0+1_000_000)
	}
	if ev.Result != limiter.AlreadyApplied || ev.DeductionID != ops[1].Permit.DeductionID() || ev.Attempts != 2 ||
		ev.CreditDecisionNs != t0*1000 || ev.CreditDecisionNs == retry.Response.Reply.TUs*1000 {
		t.Errorf("clock moved: event %+v, want already_applied, deduction_id %s, Attempts 2, CreditDecisionNs %d (the dropped reply's t × 1000, not the retry's %d)",
			ev, ops[1].Permit.DeductionID(), int64(t0*1000), retry.Response.Reply.TUs*1000)
	}
	if in := d.Inspect(key); in.U != 17*token(p) || in.MC != 2 {
		t.Errorf("clock moved: Inspect %+v, want u 17 tokens and 2 markers: one more credit", in)
	}
}

// TestT_REF_2 (SPEC T-REF-2; REF-2, REF-5, RCP-8, API-13, STA-7, API-1b) on the fast profile,
// each case on its own instance, release() at max_release_delay (0.5 s after the instant just
// before the acquire): (a) the first refund attempt held at a before_send barrier until 1.5 s
// after release(), then let go with a 0.5 s refuse outage armed at that instant on a store
// wrapper of the test's, inside the driver's (nearer the store), so the attempt let go meets
// it; its drop-reply variant, whose held first attempt also loses its reply (on the same inner
// wrapper), so the retries meet the outage and the refund may end already_applied; each (a)
// refund makes at least 2 refund calls, one refused (unavailable) before the confirming one;
// (b) a 0.5 s refuse outage starting at release(); and a refund on a full bucket (credit 0,
// captured). Each ends confirmed with FinalMonoNs − ReleaseMonoNs ≤ 3.5 s.
func TestT_REF_2(t *testing.T) {
	inners := []*fixtures.StoreWrapper{fixtures.NewStoreWrapper(), fixtures.NewStoreWrapper()}
	p := driver.FastPolicy()
	in := driver.FastInstance(p)
	d := driver.New(t, driver.Options{
		Instances: 4, Policy: p, Instance: in,
		PerInstance: []driver.InstanceSpec{
			{Options: []limitertest.Option{limitertest.WithStoreWrapper(inners[0].Wrap)}},
			{Options: []limitertest.Option{limitertest.WithStoreWrapper(inners[1].Wrap)}},
		},
	})
	maxDelay := time.Duration(in.MaxReleaseDelayNs)
	late := func(i int, key string, dropFirst bool) {
		t.Helper()
		begun := time.Now()
		op := allowed(t, d, i, key)
		name := fmt.Sprintf("ref2-%d", i)
		arm(t, d, i, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.BeforeSend, Name: name})
		if dropFirst {
			armOn(t, inners[i], fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
		}
		time.Sleep(time.Until(begun.Add(maxDelay)))
		released := time.Now()
		d.Release(op)
		reached(t, d, i, name)
		time.Sleep(time.Until(released.Add(1500 * time.Millisecond)))
		armOn(t, inners[i], fixtures.Fault{Mode: fixtures.Refuse, Match: "refund", Duration: 500 * time.Millisecond})
		d.StoreWrapper(i).Release(name)
		ev := event(t, d, op)
		what := fmt.Sprintf("(a) instance %d, drop first reply %v", i, dropFirst)
		within(t, what, ev)
		if dropFirst && len(d.RefundAttempts(op)) < 2 {
			t.Errorf("(a) drop-reply variant: %d refund calls, want the lost one and a retry", len(d.RefundAttempts(op)))
		}
		metOutage(t, d, what, op)
	}
	late(0, "ref2-barrier", false)
	late(1, "ref2-drop", true)

	begun := time.Now()
	op := allowed(t, d, 2, "ref2-outage")
	time.Sleep(time.Until(begun.Add(maxDelay)))
	arm(t, d, 2, fixtures.Fault{Mode: fixtures.Refuse, Match: "refund", Duration: 500 * time.Millisecond})
	d.Release(op)
	within(t, "(b) outage at release()", event(t, d, op))

	begun = time.Now()
	op = allowed(t, d, 3, "ref2-full")
	step(t, d, d.Policy().RatePeriodNs/d.Policy().RateTokens) // one token's refill: full again
	time.Sleep(time.Until(begun.Add(maxDelay)))
	d.Release(op)
	ev := event(t, d, op)
	within(t, "full bucket", ev)
	if as := d.RefundAttempts(op); len(as) == 0 || as[len(as)-1].Credited() != 0 {
		t.Errorf("full bucket: refund calls %+v, want the last crediting 0 units", as)
	}
}

// TestT_REF_3 (SPEC T-REF-3; REF-4a, REF-6) on the fast profile: with refunds refused for
// 8 s, a release ends unresolved (retention) when the retention timer fires, with Attempts 0;
// separately, on another instance, a refund held at a before_send barrier while the clock
// moves past its receipt's expiry_us, then let go, ends receipt_expired. Neither counts as
// applied: rl_refund_results_total{result="applied"} is 0 on both instances.
func TestT_REF_3(t *testing.T) {
	d := fastDriver(t, 2, nil)
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Refuse, Match: "refund", Duration: 8 * time.Second})
	stuck := allowed(t, d, 0, "ref3-unreachable")
	d.Release(stuck)

	arm(t, d, 1, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.BeforeSend, Name: "ref3"})
	held := allowed(t, d, 1, "ref3-answers-late")
	d.Release(held)
	reached(t, d, 1, "ref3")
	if err := d.Clock().Set((expiryOf(held) + 1) * 1000); err != nil {
		t.Fatal(err)
	}
	d.StoreWrapper(1).Release("ref3")
	if ev := event(t, d, held); ev.Result != limiter.ReceiptExpired {
		t.Errorf("refund answered after expiry: event %+v, want receipt_expired", ev)
	}

	ev, err := d.WaitRefund(stuck, time.Duration(d.Policy().ReceiptLifetimeNs)+waitMax)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Result != limiter.Unresolved || ev.Reason != limiter.UnresolvedRetention || ev.Attempts != 0 {
		t.Errorf("store unreachable past the lifetime: event %+v, want unresolved (retention) with 0 attempts", ev)
	}
	for i := range 2 {
		if v := metric(t, d, i, "rl_refund_results_total", "applied"); v != 0 {
			t.Errorf(`instance %d: rl_refund_results_total{result="applied"} = %v, want 0`, i, v)
		}
	}
}

// TestT_REF_4 (SPEC T-REF-4; API-18, API-2a) with max_live_markers = max_markers_per_key = 2
// (fingerprinted: its own deployment) and three keys: R1 (K1) is applied; R2 (K2) is applied
// with its reply dropped, which fills the marker pool, and its retry is already_applied (the
// marker check comes before the capacity check); R3 (K3) gets the reply {24, t,
// 'live_markers'} (captured), retried until the instance is killed.
func TestT_REF_4(t *testing.T) {
	d := fastDriver(t, 1, func(p *limiter.Policy, _ *limiter.Instance) { p.MaxLiveMarkers, p.MaxMarkersPerKey = 2, 2 })
	r1 := allowed(t, d, 0, "ref4-k1")
	d.Release(r1)
	if ev := event(t, d, r1); ev.Result != limiter.Applied {
		t.Errorf("R1: event %+v, want applied", ev)
	}
	r2 := allowed(t, d, 0, "ref4-k2")
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
	d.Release(r2)
	if ev := event(t, d, r2); ev.Result != limiter.AlreadyApplied {
		t.Errorf("R2, its reply lost with the pool full: event %+v, want already_applied, not capacity_error", ev)
	}
	r3 := allowed(t, d, 0, "ref4-k3")
	d.Release(r3)
	if !waitUntil(time.Now().Add(waitMax), func() bool { return len(d.RefundAttempts(r3)) >= 1 }) {
		t.Fatal("no refund call for R3")
	}
	if r := d.RefundAttempts(r3)[0].Response; r.Outcome != store.OK || r.Reply.Code != 24 || r.Reply.Reason != "live_markers" || r.Reply.TUs <= 0 {
		t.Errorf("R3's reply: outcome %v, {%d, %d, %q}; want {24, t, 'live_markers'}", r.Outcome, r.Reply.Code, r.Reply.TUs, r.Reply.Reason)
	}
	d.Kill(0) // R3 retries until its retention; nothing more to see
}

// TestT_REF_5 (SPEC T-REF-5; REF-4): with every refund delayed 500 ms before its send, 100
// release() calls each return in under 5 ms, and the 100 applied events arrive afterwards:
// each event's FinalMonoNs is at least 500 ms after its ReleaseMonoNs, so every refund
// finished in the background.
func TestT_REF_5(t *testing.T) {
	d := fastDriver(t, 1, nil)
	var ops []driver.Op
	for n := range 100 {
		ops = append(ops, allowed(t, d, 0, fmt.Sprintf("ref5-%d", n%5)))
	}
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Delay, Match: "refund", Delay: 500 * time.Millisecond})
	var slowest time.Duration
	for _, op := range ops {
		start := time.Now()
		d.Release(op)
		slowest = max(slowest, time.Since(start))
	}
	if slowest >= 5*time.Millisecond {
		t.Errorf("the slowest of 100 release() calls took %v, want under 5 ms", slowest)
	}
	for _, op := range ops {
		ev := event(t, d, op)
		if ev.Result != limiter.Applied || time.Duration(ev.FinalMonoNs-ev.ReleaseMonoNs) < 500*time.Millisecond {
			t.Errorf("event %+v, want applied at least 500 ms after its release()", ev)
		}
	}
}

// TestT_REF_6 (SPEC T-REF-6; STA-15, API-1e, HOST-6, REF-9, API-8a, API-1g) on the fast
// profile. (a) Refunds delayed 300 ms, 20 released, Close(): all 20 applied events arrive
// before it returns; meanwhile an acquire on that instance is not_ready with no acquire byte
// sent, and health is not_ready (closing). (b) Refunds refused, 5 released,
// Close(WithDrainTimeout(500 ms)) returns in 500–800 ms, the 5 counted as unresolved in
// rl_refund_results_total, and all 5 received as unresolved (closed), none dropped, when it
// returns: the drain's end emits them into a queue with room for them, and API-1d drops an
// event only on overflow (T-REF-6 (b): "the rest emitted as unresolved (closed)"). Another
// instance keeps serving. (c) On a second deployment, the default drain (3.5 s) with T-REF-2 (a)'s
// outage (the refunds held at a barrier until 1.5 s after release, then a 0.5 s refuse outage
// on a store wrapper of the test's, nearer the store, which every refund let go meets): all
// confirmed within 3.5 s of release, each after at least 2 refund calls, one refused before
// the confirming one (retried during Close's drain); the other instance keeps serving, and
// the key's allows total 20 + the credits.
func TestT_REF_6(t *testing.T) {
	d := fastDriver(t, 3, nil)
	var ops []driver.Op
	for range 20 {
		ops = append(ops, allowed(t, d, 0, "ref6a"))
	}
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Delay, Match: "refund", Delay: 300 * time.Millisecond})
	for _, op := range ops {
		d.Release(op)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		d.Limiter(0).Close()
	}()
	if !waitUntil(time.Now().Add(waitMax), func() bool { return d.Limiter(0).Health().Reason == "closing" }) {
		t.Fatal("(a) Close did not begin")
	}
	mark := len(d.Sent(0))
	if op := d.Acquire(0, "ref6a-new"); op.Kind != limiter.NotReady {
		t.Errorf("(a) acquire while closing: %v, want not_ready", op.Kind)
	}
	if n := driver.Count(d.Sent(0)[mark:], "acquire"); n != 0 {
		t.Errorf("(a) acquire while closing wrote %d acquire commands, want none", n)
	}
	if h := d.Limiter(0).Health(); h.Status != "not_ready" || h.Reason != "closing" {
		t.Errorf("(a) health while closing: %s (%s), want not_ready (closing)", h.Status, h.Reason)
	}
	d.Done(allowed(t, d, 2, "ref6-other")) // another instance keeps serving
	select {
	case <-closed:
	case <-time.After(waitMax):
		t.Fatal("(a) Close did not return")
	}
	evs := d.RefundEvents(0)
	applied := 0
	for _, ev := range evs {
		if ev.Result == limiter.Applied {
			applied++
		}
	}
	if applied != 20 || len(evs) != 20 {
		t.Errorf("(a) %d events, %d applied, when Close returned; want 20 applied", len(evs), applied)
	}

	ops = ops[:0]
	for range 5 {
		ops = append(ops, allowed(t, d, 1, "ref6b"))
	}
	arm(t, d, 1, fixtures.Fault{Mode: fixtures.Refuse, Match: "refund", Duration: 10 * time.Second})
	for _, op := range ops {
		d.Release(op)
	}
	start := time.Now()
	d.Limiter(1).Close(limiter.WithDrainTimeout(500 * time.Millisecond))
	if took := time.Since(start); took < 500*time.Millisecond || took > 800*time.Millisecond {
		t.Errorf("(b) Close(WithDrainTimeout(500 ms)) took %v, want 500–800 ms", took)
	}
	if v := metric(t, d, 1, "rl_refund_results_total", "unresolved"); v != 5 {
		t.Errorf(`(b) rl_refund_results_total{result="unresolved"} = %v, want 5`, v)
	}
	// The drain used its whole timeout; Close then lets the dispatcher deliver the events its end
	// queued before it stops the dispatcher, and returns once the queue is empty, so the
	// driver's callback has taken all 5.
	received := 0
	for _, ev := range d.RefundEvents(1) {
		if ev.Result != limiter.Unresolved || ev.Reason != limiter.UnresolvedClosed {
			t.Errorf("(b) event %+v, want unresolved (closed)", ev)
		}
		received++
	}
	if drops := d.Limiter(1).Health().ListenerDrops; received != 5 || drops != 0 {
		t.Errorf("(b) %d events received and %d dropped when Close returned, want 5 and 0: the queue had room for them (API-1d)", received, drops)
	}
	d.Done(allowed(t, d, 2, "ref6-other"))

	inner := fixtures.NewStoreWrapper()
	pc := driver.FastPolicy()
	e := driver.New(t, driver.Options{
		Instances: 2, Policy: pc, Instance: driver.FastInstance(pc),
		PerInstance: []driver.InstanceSpec{{Options: []limitertest.Option{limitertest.WithStoreWrapper(inner.Wrap)}}},
	})
	const key = "ref6c"
	var mine []driver.Op
	for range 10 {
		mine = append(mine, allowed(t, e, 0, key))
	}
	for range 10 {
		e.Done(allowed(t, e, 1, key))
	}
	arm(t, e, 0, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 10, At: fixtures.BeforeSend, Name: "ref6c"})
	released := time.Now()
	for _, op := range mine {
		e.Release(op)
	}
	reached(t, e, 0, "ref6c")
	closed = make(chan struct{})
	go func() {
		defer close(closed)
		e.Limiter(0).Close()
	}()
	time.Sleep(time.Until(released.Add(1500 * time.Millisecond)))
	armOn(t, inner, fixtures.Fault{Mode: fixtures.Refuse, Match: "refund", Duration: 500 * time.Millisecond})
	e.StoreWrapper(0).Release("ref6c")
	e.Done(allowed(t, e, 1, "ref6c-other")) // the other instance keeps serving
	select {
	case <-closed:
	case <-time.After(waitMax):
		t.Fatal("(c) Close did not return")
	}
	if n := metric(t, e, 0, "rl_refund_results_total", "applied") + metric(t, e, 0, "rl_refund_results_total", "already_applied"); n != 10 {
		t.Fatalf("(c) %v refunds confirmed when Close returned (%v unresolved), want all 10", n, metric(t, e, 0, "rl_refund_results_total", "unresolved"))
	}
	credits := 0
	for _, op := range mine {
		ev := event(t, e, op)
		within(t, "(c)", ev)
		metOutage(t, e, "(c)", op)
		if confirmed(ev) {
			credits++
		}
	}
	more := 0
	for range 30 {
		op := e.Acquire(1, key)
		if op.Kind != limiter.Allowed {
			if op.Kind != limiter.Limited {
				t.Fatalf("(c) acquire on instance 2: %v (%+v), want allowed or limited", op.Kind, op.Diag)
			}
			break
		}
		e.Done(op)
		more++
	}
	if more != credits {
		t.Errorf("(c) %d more allowed after the refunds, want %d: allows = 20 + credits", more, credits)
	}
}

// TestT_REF_7 (SPEC T-REF-7 [D]; API-1d, HOST-8). (a) A listener that blocks 5 s and then
// panics on every event, and one that panics at once on every event (compared once it has
// been called for all 6 events), each leave refund results, retries, credits and slot release
// as on a run with only the driver's recording listener: after a warm-up refund (the script
// loaded), 5 permits of a key at a frozen instant, the first refund reply dropped, all
// released; the per-result counts, the refund sends (retries), Inspect's u and mc and the
// slots match. (b) With ListenerQueueSize 4 and a listener blocked in its first callback, 10
// events give at least 5 drops, and drops + delivered = 10 once it is let go.
func TestT_REF_7(t *testing.T) {
	type outcome struct {
		applied, already float64
		sends            int
		u, mc            int64
		receipts, refund float64
	}
	// run measures once the 6 refunds are final and heard, when set, reports true.
	run := func(listener func(limiter.RefundEvent), heard func() bool) outcome {
		t.Helper()
		p := driver.FastPolicy()
		d := driver.New(t, driver.Options{
			Instances: 1, Policy: p, Instance: driver.FastInstance(p),
			PerInstance: []driver.InstanceSpec{{Listener: listener}},
		})
		warm := allowed(t, d, 0, "ref7-warm") // loads the refund script, so no NOSCRIPT adds a send below
		d.Release(warm)
		event(t, d, warm)
		var ops []driver.Op
		for range 5 {
			ops = append(ops, allowed(t, d, 0, "ref7"))
		}
		arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
		for _, op := range ops {
			d.Release(op)
		}
		final := func() bool {
			return metric(t, d, 0, "rl_refund_results_total", "applied")+
				metric(t, d, 0, "rl_refund_results_total", "already_applied") == 6 &&
				metric(t, d, 0, "rl_pending_refunds", "") == 0 && (heard == nil || heard())
		}
		if !waitUntil(time.Now().Add(waitMax), final) {
			t.Fatalf("the 5 refunds were not final (and their events heard) within %v", waitMax)
		}
		in := d.Inspect("ref7")
		r, f := slotsInUse(t, d, 0)
		return outcome{
			applied: metric(t, d, 0, "rl_refund_results_total", "applied"),
			already: metric(t, d, 0, "rl_refund_results_total", "already_applied"),
			sends:   refundSends(d, 0), u: in.U, mc: in.MC, receipts: r, refund: f,
		}
	}
	plain := run(nil, nil)
	stop := make(chan struct{})
	var stopOnce sync.Once
	blocking := run(func(limiter.RefundEvent) {
		select {
		case <-time.After(5 * time.Second):
		case <-stop:
		}
		panic("T-REF-7 listener")
	}, nil)
	t.Cleanup(func() { stopOnce.Do(func() { close(stop) }) }) // before the drivers' teardown
	if blocking != plain {
		t.Errorf("with a blocking, panicking listener %+v, want as with none %+v", blocking, plain)
	}
	var calls atomic.Int64
	throwing := run(func(limiter.RefundEvent) {
		calls.Add(1)
		panic("T-REF-7 listener throws")
	}, func() bool { return calls.Load() == 6 }) // the warm-up's event and the 5 others
	if throwing != plain {
		t.Errorf("with a listener panicking on all %d events %+v, want as with none %+v", calls.Load(), throwing, plain)
	}
	// 8 refund sends: the warm-up's NOSCRIPT and resend, the 5 first attempts and 1 retry.
	if plain.applied != 5 || plain.already != 1 || plain.sends != 8 || plain.mc != 5 || plain.receipts != 0 || plain.refund != 0 {
		t.Errorf("the run without a listener %+v, want 5 applied (the warm-up's included), 1 already_applied, 8 refund sends, 5 markers, slots 0/0", plain)
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	var mu sync.Mutex
	delivered := 0
	p := driver.FastPolicy()
	in := driver.FastInstance(p)
	in.ListenerQueueSize = 4
	d := driver.New(t, driver.Options{
		Instances: 1, Policy: p, Instance: in,
		PerInstance: []driver.InstanceSpec{{Listener: func(limiter.RefundEvent) {
			mu.Lock()
			delivered++
			mu.Unlock()
			<-release
		}}},
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var ops []driver.Op
	for range 10 {
		ops = append(ops, allowed(t, d, 0, "ref7b"))
	}
	for _, op := range ops {
		d.Release(op)
	}
	if !waitUntil(time.Now().Add(waitMax), func() bool {
		return metric(t, d, 0, "rl_refund_results_total", "applied") == 10
	}) {
		t.Fatal("(b) the 10 refunds were not applied")
	}
	if drops := d.Limiter(0).Health().ListenerDrops; drops < 5 {
		t.Errorf("(b) %d listener drops, want at least 5 (1 in the callback, 4 queued)", drops)
	}
	releaseOnce.Do(func() { close(release) })
	if !waitUntil(time.Now().Add(waitMax), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return uint64(delivered)+d.Limiter(0).Health().ListenerDrops == 10
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Errorf("(b) %d delivered + %d dropped, want 10", delivered, d.Limiter(0).Health().ListenerDrops)
	}
}

// TestT_REF_8 (SPEC T-REF-8; REF-8, STA-2): instance 1 acquires 5 permits of K and releases
// them while its refunds are refused, then is killed, and the refusal is lifted: after twice
// the time a live instance needs to retry and refund (2 × (2 × RefundBackoffMax + D) =
// 1.2 s), Inspect shows K as after the 5 acquires (u, g, s, and no marker), and instance 1 wrote
// no refund command and emitted no refund event; instance 2 acquires and refunds K as usual;
// instance 1 still emitted no refund event.
func TestT_REF_8(t *testing.T) {
	d := fastDriver(t, 2, nil)
	const key = "ref8"
	var ops []driver.Op
	for range 5 {
		ops = append(ops, allowed(t, d, 0, key))
	}
	after := d.Inspect(key)
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Refuse, Match: "refund"})
	for _, op := range ops {
		d.Release(op)
	}
	time.Sleep(200 * time.Millisecond) // refused attempts and their retries
	d.Kill(0)
	d.StoreWrapper(0).Disarm() // the store answers refunds again: a live instance would now refund
	inst := d.Instance()
	retryWindow := 2*time.Duration(inst.RefundBackoffMaxNs) + time.Duration(inst.DependencyDeadlineNs)
	time.Sleep(2 * retryWindow)
	if in := d.Inspect(key); in.U != after.U || in.G != after.G || in.S != after.S || in.MC != 0 {
		t.Errorf("Inspect %v after the kill %+v, want u %d, g %d, s %d and mc 0 as after the acquires", 2*retryWindow, in, after.U, after.G, after.S)
	}
	if n := refundSends(d, 0); n != 0 {
		t.Errorf("the killed instance wrote %d refund commands, want none: every attempt before the kill was refused", n)
	}
	if evs := d.RefundEvents(0); len(evs) != 0 {
		t.Errorf("the killed instance emitted %+v %v after the kill, want no refund event", evs, 2*retryWindow)
	}
	op := allowed(t, d, 1, key)
	d.Release(op)
	if ev := event(t, d, op); ev.Result != limiter.Applied {
		t.Errorf("instance 2's refund: event %+v, want applied", ev)
	}
	if evs := d.RefundEvents(0); len(evs) != 0 {
		t.Errorf("the killed instance emitted %+v, want no refund event", evs)
	}
}
