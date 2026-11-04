//go:build rltest

package spectest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/driver"
	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/limiter"
	"github.com/srir84/ratelimiter/limitertest"
)

// TestT_RCP_6 (SPEC T-RCP-6; API-12a, RCP-3; FLT-10): with a frozen clock and the signer
// wrapper failing once, the confirmed deduction gives receipt_error with no permit and the
// receipt slot back at 0; the charge stays, so the key's next 19 acquires are allowed and the
// 20th limited, and Inspect shows 20 deductions.
func TestT_RCP_6(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 1})
	if err := d.SignerWrapper(0).Arm(fixtures.SignerFault{Mode: fixtures.SignFail, Count: 1}); err != nil {
		t.Fatal(err)
	}
	op := d.Acquire(0, "rcp6")
	if op.Kind != limiter.ReceiptError || op.Permit != nil {
		t.Fatalf("signing failed after the deduction: %v, permit %v; want receipt_error and no permit", op.Kind, op.Permit)
	}
	if v, ok := driver.MetricValue(d.Limiter(0).Metrics(), "rl_slots_in_use", "receipt"); !ok || v != 0 {
		t.Errorf(`rl_slots_in_use{kind="receipt"} = %v (found %v), want 0`, v, ok)
	}
	var ops []driver.Op
	for n := range 19 {
		op := d.Acquire(0, "rcp6")
		ops = append(ops, op)
		if op.Kind != limiter.Allowed {
			t.Fatalf("acquire %d after the receipt error: %v, want allowed", n+1, op.Kind)
		}
	}
	doneAll(ops)
	if op := d.Acquire(0, "rcp6"); op.Kind != limiter.Limited {
		t.Errorf("acquire 20 after the receipt error: %v, want limited (the charge stayed)", op.Kind)
	}
	if in := d.Inspect("rcp6"); in.Present != 1 || in.S != 20 || in.U != 0 {
		t.Errorf("Inspect %+v, want s 20 and u 0: 20 deductions, the failed signing's included", in)
	}
}

// TestT_RCP_1 (SPEC T-RCP-1; RCP-1, RCP-4, API-15; DESIGN §A5.9, §B6 FLT-10 corrupt): for each
// of the 7 authenticated receipt fields, the signer wrapper alters that field of the receipt
// after signing (CorruptField), and the permit's release ends receipt_invalid at local
// verification with Attempts 0; no refund byte is ever sent, and Inspect shows the key's u and
// mc as before the release (no credit, no marker). The op log replays in the reference model.
func TestT_RCP_1(t *testing.T) {
	d := fastDriver(t, 1, nil)
	fields := fixtures.ReceiptFields()
	if len(fields) != 7 {
		t.Fatalf("ReceiptFields() = %q, want the 7 MAC'd fields of §A5.9", fields)
	}
	for _, f := range fields {
		key := "rcp1-" + f
		op := allowed(t, d, 0, key)
		before := d.Inspect(key)
		if err := d.SignerWrapper(0).CorruptField(f); err != nil {
			t.Fatal(err)
		}
		d.Release(op)
		ev := event(t, d, op)
		d.SignerWrapper(0).Disarm()
		if ev.Result != limiter.ReceiptInvalid || ev.Attempts != 0 || ev.DeductionID != op.Permit.DeductionID() {
			t.Errorf("%s altered: event %+v, want receipt_invalid with 0 attempts", f, ev)
		}
		if as := d.RefundAttempts(op); len(as) != 0 {
			t.Errorf("%s altered: %d refund calls, want none (local verification)", f, len(as))
		}
		if after := d.Inspect(key); after.U != before.U || after.MC != before.MC || after.MC != 0 {
			t.Errorf("%s altered: Inspect %+v after the release, want u %d and mc 0 as before", f, after, before.U)
		}
	}
	if n := refundSends(d, 0); n != 0 {
		t.Errorf("%d refund commands written, want none", n)
	}
	d.CheckRefModel(t)
}

// TestT_RCP_7 (SPEC T-RCP-7 [D]; API-1b, API-1c, API-1h, RCP-9, REF-1) on the fast profile
// (6 s receipt lifetime): (a) done() frees both slots at once and keeps the charge; (b) a
// permit never finished frees its slots at 6 s ± 0.2 s after the acquire began, its charge
// kept; (e) after that, release() of an expired permit ends unresolved (expired) with nothing
// sent, and done() of another is a silent first finish; (c) under FailureMode Open with
// acquires refused, a degraded acquire leaves both slots at 0 and its permit's release() emits
// nothing and sends nothing; (d) release() then done() emits exactly one event, done() then
// release() none; (f) release() of an OPEN permit once Close began ends unresolved (closed)
// with nothing sent; (g) every acquire result other than allowed leaves both slots at 0 when
// it returns (busy aside: it comes only while other permits hold every slot, and takes none):
// limited, validation_error, cancelled, unavailable, outcome_unknown, receipt_error,
// config_error, clock_error (the controlled time at TBOUND) and not_ready; capacity_error
// (max_tracked_buckets 1) and state_integrity_error (a bucket damaged through Fixture) each on
// a driver of its own, so the test driver keeps its BUD-11 check; degraded in (c).
func TestT_RCP_7(t *testing.T) {
	t.Run("a", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		op := allowed(t, d, 0, "rcp7a")
		if r, f := slotsInUse(t, d, 0); r != 1 || f != 1 {
			t.Fatalf("slots %v/%v while OPEN, want 1/1", r, f)
		}
		d.Done(op)
		if r, f := slotsInUse(t, d, 0); r != 0 || f != 0 {
			t.Errorf("slots %v/%v right after done(), want 0/0", r, f)
		}
		if in := d.Inspect("rcp7a"); in.S != 1 || in.U != 19*token(d.Policy()) {
			t.Errorf("Inspect %+v after done(), want s 1 and u 19 tokens: the charge kept", in)
		}
	})
	t.Run("b_e", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		lifetime := time.Duration(d.Policy().ReceiptLifetimeNs)
		begun := time.Now()
		never := allowed(t, d, 0, "rcp7b")
		late := allowed(t, d, 0, "rcp7b")
		silent := allowed(t, d, 0, "rcp7b")
		if waitUntil(begun.Add(lifetime-200*time.Millisecond), func() bool {
			r, _ := slotsInUse(t, d, 0)
			return r == 0
		}) {
			t.Fatalf("slots 0 at %v after the acquires began, want 3 until 6 s − 0.2 s", time.Since(begun))
		}
		if !waitUntil(begun.Add(lifetime+200*time.Millisecond), func() bool {
			r, f := slotsInUse(t, d, 0)
			return r == 0 && f == 0
		}) {
			t.Fatalf("slots not 0 at %v after the acquires began, want 0 by 6 s + 0.2 s", time.Since(begun))
		}
		if in := d.Inspect("rcp7b"); in.S != 3 || in.U != 17*token(d.Policy()) {
			t.Errorf("Inspect %+v after expiry, want s 3 and u 17 tokens: the charges kept", in)
		}
		d.Release(late)
		ev := event(t, d, late)
		if ev.Result != limiter.Unresolved || ev.Reason != limiter.UnresolvedExpired || ev.Attempts != 0 {
			t.Errorf("release() after expiry: event %+v, want unresolved (expired) with 0 attempts", ev)
		}
		d.Done(silent)
		time.Sleep(300 * time.Millisecond)
		if evs := d.RefundEvents(0); len(evs) != 1 {
			t.Errorf("events %+v, want only the expired release's (done() after expiry is silent)", evs)
		}
		if n := refundSends(d, 0); n != 0 {
			t.Errorf("%d refund commands written, want none", n)
		}
		_ = never // never finished: its slots were freed at expiry
	})
	t.Run("c", func(t *testing.T) {
		d := fastDriver(t, 1, func(p *limiter.Policy, _ *limiter.Instance) { p.FailureMode = limiter.Open })
		if err := d.StoreWrapper(0).Arm(fixtures.Fault{Mode: fixtures.Refuse, Match: "acquire", Count: 1}); err != nil {
			t.Fatal(err)
		}
		op := d.Acquire(0, "rcp7c")
		if op.Kind != limiter.Degraded || op.Permit == nil || op.Permit.DeductionID() != "" {
			t.Fatalf("acquire refused in open mode: %v, want degraded with a permit and no deduction_id", op.Kind)
		}
		if r, f := slotsInUse(t, d, 0); r != 0 || f != 0 { // (g) for degraded
			t.Errorf("slots %v/%v after the degraded acquire returned, want 0/0", r, f)
		}
		d.Release(op)
		time.Sleep(300 * time.Millisecond)
		if evs := d.RefundEvents(0); len(evs) != 0 {
			t.Errorf("events %+v after a degraded permit's release(), want none", evs)
		}
		if n := refundSends(d, 0); n != 0 {
			t.Errorf("%d refund commands written, want none", n)
		}
	})
	t.Run("d", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		first := allowed(t, d, 0, "rcp7d")
		second := allowed(t, d, 0, "rcp7d")
		d.Release(first)
		d.Done(first)
		d.Done(second)
		d.Release(second)
		if ev := event(t, d, first); ev.Result != limiter.Applied {
			t.Errorf("release() then done(): event %+v, want applied", ev)
		}
		time.Sleep(300 * time.Millisecond)
		if evs := d.RefundEvents(0); len(evs) != 1 || evs[0].DeductionID != first.Permit.DeductionID() {
			t.Errorf("events %+v, want exactly one, the first permit's", evs)
		}
	})
	t.Run("f", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		open := allowed(t, d, 0, "rcp7f")
		pending := allowed(t, d, 0, "rcp7f")
		if err := d.StoreWrapper(0).Arm(fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.BeforeSend, Name: "rcp7f"}); err != nil {
			t.Fatal(err)
		}
		d.Release(pending)
		select {
		case <-d.StoreWrapper(0).Reached("rcp7f"):
		case <-time.After(waitMax):
			t.Fatal("the pending refund did not reach the barrier")
		}
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			d.Limiter(0).Close()
		}()
		if !waitUntil(time.Now().Add(waitMax), func() bool { return d.Limiter(0).Health().Reason == "closing" }) {
			t.Fatal("Close did not begin")
		}
		d.Release(open)
		ev := event(t, d, open)
		if ev.Result != limiter.Unresolved || ev.Reason != limiter.UnresolvedClosed || ev.Attempts != 0 {
			t.Errorf("release() after Close began: event %+v, want unresolved (closed) with 0 attempts", ev)
		}
		if n := refundSends(d, 0); n != 0 {
			t.Errorf("%d refund commands written while the only refund is held before its send, want none", n)
		}
		d.StoreWrapper(0).Release("rcp7f")
		select {
		case <-closed:
		case <-time.After(waitMax):
			t.Fatal("Close did not return")
		}
		if ev := event(t, d, pending); ev.Result != limiter.Applied {
			t.Errorf("the pending refund: event %+v, want applied", ev)
		}
		if n := refundSends(d, 0); n < 1 {
			t.Errorf("%d refund commands written, want the pending permit's", n)
		}
	})
	t.Run("g", func(t *testing.T) {
		d := fastDriver(t, 2, nil)
		check := func(on *driver.Driver, what string, op driver.Op, want limiter.ResultKind) {
			t.Helper()
			if op.Kind != want {
				t.Errorf("%s: %v (%+v), want %v", what, op.Kind, op.Diag, want)
			}
			if r, f := slotsInUse(t, on, op.Instance); r != 0 || f != 0 {
				t.Errorf("%s: slots %v/%v after the acquire returned, want 0/0", what, r, f)
			}
		}
		for range 20 {
			d.Done(allowed(t, d, 0, "rcp7g"))
		}
		check(d, "limited", d.Acquire(0, "rcp7g"), limiter.Limited)
		check(d, "validation_error", d.Acquire(0, ""), limiter.ValidationError)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		check(d, "cancelled", d.AcquireContext(ctx, 0, "rcp7g-c"), limiter.Cancelled)
		arm := func(f fixtures.Fault) {
			t.Helper()
			if err := d.StoreWrapper(0).Arm(f); err != nil {
				t.Fatal(err)
			}
		}
		arm(fixtures.Fault{Mode: fixtures.Refuse, Match: "acquire", Count: 1})
		check(d, "unavailable", d.Acquire(0, "rcp7g-u"), limiter.Unavailable)
		arm(fixtures.Fault{Mode: fixtures.DropReply, Match: "acquire", Count: 1})
		check(d, "outcome_unknown", d.Acquire(0, "rcp7g-o"), limiter.OutcomeUnknown)
		if err := d.SignerWrapper(0).Arm(fixtures.SignerFault{Mode: fixtures.SignFail, Count: 1}); err != nil {
			t.Fatal(err)
		}
		check(d, "receipt_error", d.Acquire(0, "rcp7g-r"), limiter.ReceiptError)
		if err := d.SignerWrapper(0).Arm(fixtures.SignerFault{Mode: fixtures.SignUnusable, Count: 1}); err != nil {
			t.Fatal(err)
		}
		check(d, "config_error", d.Acquire(0, "rcp7g-s"), limiter.ConfigError)
		const tBoundUs int64 = 1<<53 - 1<<43 // TBOUND (DESIGN §A3.1): no budget time reaches it
		frozen := d.Clock().NowNs()
		if err := d.Clock().Set(tBoundUs * 1000); err != nil {
			t.Fatal(err)
		}
		check(d, "clock_error", d.Acquire(0, "rcp7g-t"), limiter.ClockError)
		if err := d.Clock().Set(frozen); err != nil {
			t.Fatal(err)
		}
		d.Close(1)
		check(d, "not_ready", d.Acquire(1, "rcp7g-n"), limiter.NotReady)

		// A deployment that tracks one bucket refuses a second key's (no Fixture write: this
		// driver keeps its BUD-11 check too).
		one := fastDriver(t, 1, func(p *limiter.Policy, _ *limiter.Instance) { p.MaxTrackedBuckets = 1 })
		one.Done(allowed(t, one, 0, "rcp7g-1"))
		check(one, "capacity_error", one.Acquire(0, "rcp7g-2"), limiter.CapacityError)

		// A bucket without its u field, through Fixture on a driver of its own: the damage
		// exempts that driver's teardown from BUD-11, not d's.
		bad := fastDriver(t, 1, nil)
		bad.Done(allowed(t, bad, 0, "rcp7g-i"))
		enc, err := keys.Encode("rcp7g-i")
		if err != nil {
			t.Fatal(err)
		}
		if r, err := bad.Fixture().Do("HDEL", keys.BucketKey(bad.ReadEpoch().ID, enc), "u"); err != nil || r.Kind == '-' {
			t.Fatalf("Fixture HDEL of the bucket's u: %v %s", err, r)
		}
		check(bad, "state_integrity_error", bad.Acquire(0, "rcp7g-i"), limiter.StateIntegrityError)
	})
}

// dropThenHold arms, on instance 0's store wrapper, a drop of the next refund call's reply and
// a before_send barrier named name on the refund call after it: a permit released next has
// its first refund applied at the store with the reply lost, and its retry held.
func dropThenHold(t *testing.T, d *driver.Driver, name string) {
	t.Helper()
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.BeforeSend, Name: name})
}

// appliedUnseen checks that op's only refund attempt so far was applied at the store (E_APPLIED
// behind the wrapper) while the limiter got outcome_unknown.
func appliedUnseen(t *testing.T, d *driver.Driver, what string, op driver.Op) {
	t.Helper()
	as := d.RefundAttempts(op)
	if len(as) != 1 || as[0].Response.Outcome != store.OutcomeUnknown || !as[0].Reached ||
		as[0].Store.Outcome != store.OK || as[0].Store.Reply.Code != 3 {
		t.Fatalf("%s: refund attempts %+v, want one applied at the store with its reply lost", what, as)
	}
}

// TestT_RCP_3 (SPEC T-RCP-3; RCP-5, RCP-6, API-16): R1, R2 and R3 on K are issued at a frozen
// t0 and expire at E = t0 + 6 s. At E − 1 µs, R1's release is applied once (one attempt). R2's
// and R3's first refunds are applied too, their replies dropped, and each retry is held at a
// before_send barrier: K holds the 3 markers, all expiring at E. At E, R2's retry is
// receipt_expired though its marker exists (still 3 markers). At E + 1 s a pass removes K with
// its 3 markers (step 30, markers_removed 3); then R3's retry is receipt_expired too. With the
// scripts loaded first, R1's event counts 1 attempt and R2's and R3's 2.
func TestT_RCP_3(t *testing.T) {
	d := fastDriver(t, 1, nil)
	d.LoadScripts() // no NOSCRIPT round adds an attempt
	const key = "rcp3"
	r1, r2, r3 := allowed(t, d, 0, key), allowed(t, d, 0, key), allowed(t, d, 0, key)
	eUs := expiryOf(r1)
	if expiryOf(r2) != eUs || expiryOf(r3) != eUs {
		t.Fatalf("expiries %d, %d, %d; want one E", eUs, expiryOf(r2), expiryOf(r3))
	}
	setClock(t, d, eUs*1000-1000)
	if ev := releaseApplied(t, d, r1); ev.Attempts != 1 {
		t.Errorf("R1 at E − 1 µs: %+v, want applied once, 1 attempt", ev)
	}
	for _, c := range []struct {
		name string
		op   driver.Op
	}{{"r2", r2}, {"r3", r3}} {
		dropThenHold(t, d, c.name)
		d.Release(c.op)
		reached(t, d, 0, c.name)
		appliedUnseen(t, d, c.name, c.op)
	}
	if in := d.Inspect(key); in.MC != 3 || !slices.Equal(in.Expiries, []int64{eUs, eUs, eUs}) {
		t.Fatalf("K before E: %+v, want 3 markers expiring at %d", in, eUs)
	}

	setClock(t, d, eUs*1000)
	d.StoreWrapper(0).Release("r2")
	if ev := event(t, d, r2); ev.Result != limiter.ReceiptExpired || ev.Attempts != 2 {
		t.Errorf("R2's retry at E: %+v, want receipt_expired after 2 attempts", ev)
	}
	if in := d.Inspect(key); in.MC != 3 {
		t.Errorf("K after R2's retry: %+v, want its 3 markers kept", in)
	}

	setClock(t, d, eUs*1000+1_000_000_000)
	if rep := passOK(t, d, 0); rep.Removed != 1 {
		t.Errorf("pass at E + 1 s: %+v, want Removed 1", rep)
	}
	if steps := d.StepsFor(key); len(steps) != 1 || steps[0].Code != 30 || steps[0].Markers != 3 {
		t.Fatalf("K's steps %+v, want one 30 removing 3 markers", steps)
	}
	d.StoreWrapper(0).Release("r3")
	if ev := event(t, d, r3); ev.Result != limiter.ReceiptExpired || ev.Attempts != 2 {
		t.Errorf("R3's retry after the markers' removal: %+v, want receipt_expired after 2 attempts", ev)
	}
}

// TestT_RCP_4 (SPEC T-RCP-4; RCP-2, STA-4, API-2a): at a frozen t0, 5 allows on K, generation
// g1; P, the first, is kept OPEN and the rest done. Past eligibility (t0 + 6 s) a pass removes
// K, and K returns with 5 allows on a generation g2 ≠ g1 (s 1 … 5 again); release(P) is
// receipt_expired; the 10 deduction_ids across both generations are distinct. The op log
// matches the reference model.
func TestT_RCP_4(t *testing.T) {
	d := fastDriver(t, 1, nil)
	const key = "rcp4"
	t0 := d.Clock().NowNs()
	var gen1, gen2 []driver.Op
	for range 5 {
		gen1 = append(gen1, allowed(t, d, 0, key))
	}
	p := gen1[0]
	doneAll(gen1[1:])
	setClock(t, d, t0+d.Policy().ReceiptLifetimeNs)
	if rep := passOK(t, d, 0); rep.Removed != 1 {
		t.Errorf("pass past eligibility: %+v, want Removed 1", rep)
	}
	for k := range 5 {
		op := allowed(t, d, 0, key)
		if s := op.Reply.Elems[5].Int; s != int64(k+1) {
			t.Errorf("recreated K, allow %d: s %d, want %d", k+1, s, k+1)
		}
		gen2 = append(gen2, op)
	}
	if g1, g2 := p.Reply.Elems[4].Int, gen2[0].Reply.Elems[4].Int; g1 == g2 {
		t.Errorf("recreated K: generation %d, want one other than %d", g2, g1)
	}
	doneAll(gen2)
	releaseExpired(t, d, p)
	seen := map[string]bool{}
	for _, op := range append(gen1, gen2...) {
		id := op.Permit.DeductionID()
		if seen[id] {
			t.Errorf("deduction_id %s repeats", id)
		}
		seen[id] = true
	}
	if len(seen) != 10 {
		t.Errorf("%d distinct deduction_ids, want 10", len(seen))
	}
	d.CheckRefModel(t)
}

// TestT_RCP_5 (SPEC T-RCP-5; RCP-1, RCP-2, RCP-5, CFG-5): an instance holds an epoch-A permit,
// and its epoch read sees that traffic (rl_tracked_buckets 1: A's grown gen reaches its floors);
// ReplaceEpoch starts B with the same settings; once the instance's health shows B (and with
// the scripts cached, so no NOSCRIPT round adds a send), Release() ends with receipt_invalid
// (wrong epoch_id: the call names B in its KEYS and the receipt's A in its ARGV, so the store
// answers 10 epoch), Attempts 1, and credits nothing in B: the key has no bucket there and
// status shows no marker. The op log matches the reference model.
func TestT_RCP_5(t *testing.T) {
	d := fastDriver(t, 1, nil)
	const key = "rcp5"
	op := allowed(t, d, 0, key)
	allowedAt := time.Now()
	idA := limitertestEpoch(op)
	// an epoch read sees A's traffic (its new bucket), so A's grown gen reaches the floors (§A5.7)
	waitMetric(t, d, 0, "rl_tracked_buckets", "", time.Until(allowedAt.Add(epochReads(d))), "after the allow in A", func(v float64) bool { return v == 1 })
	start := time.Now()
	idB := d.ReplaceEpoch()
	if idB == idA {
		t.Fatalf("ReplaceEpoch kept epoch %s", idA)
	}
	if err := d.WaitJoined(0, idB, joinTolerance(d)-time.Since(start)); err != nil {
		t.Fatal(err)
	}
	d.LoadScripts() // the refund script is cached: no NOSCRIPT round adds a send (API-1d)
	d.Release(op)
	ev := event(t, d, op)
	if ev.Result != limiter.ReceiptInvalid || ev.Attempts != 1 {
		t.Errorf("release of the epoch-A permit in B: %+v, want receipt_invalid after 1 attempt", ev)
	}
	if as := d.RefundAttempts(op); len(as) != 1 {
		t.Errorf("refund calls %d, want 1", len(as))
	} else {
		storeReply(t, "the refund in B", as[0], 10, "epoch")
	}
	if in, st := d.Inspect(key), d.Status(); in.Present != 0 || st.EpochID != idB || st.NB != 0 || st.NM != 0 {
		t.Errorf("epoch B after the refund: inspect %+v, status %+v; want no bucket and no marker", in, st)
	}
	d.CheckRefModel(t)
}

// limitertestEpoch is the epoch_id an allowed op's permit carries, from its acquire reply's
// epoch (the driver's op log records it the same way).
func limitertestEpoch(op driver.Op) string { return limitertest.PermitEpoch(op.Permit) }
