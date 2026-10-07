//go:build rltest

package spectest

import (
	"testing"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/limiter"
)

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
