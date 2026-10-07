//go:build rltest

package spectest

import (
	"fmt"
	"math/big"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/limiter"
)

// interval is the driver's epoch re-read interval (DESIGN §A5.10: 1 s at the harness values).
func interval(d *driver.Driver) time.Duration {
	return time.Duration(d.Instance().EpochRereadIntervalNs)
}

// waitHealth waits until l reports status and reason, by deadline.
func waitHealth(l *limiter.Limiter, status, reason string, deadline time.Time) error {
	for {
		h := l.Health()
		if h.Status == status && h.Reason == reason {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("health %s/%s, want %s/%s", h.Status, h.Reason, status, reason)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// retryAfter is SPEC API-6's exact wait, computed with math/big: ⌈(rp − u) / rt⌉ ns.
func retryAfter(u, rp, rt int64) int64 {
	n := new(big.Int).Sub(big.NewInt(rp), big.NewInt(u))
	q, m := new(big.Int).QuoRem(n, big.NewInt(rt), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// replyU is a limited or allowed acquire's u from its reply (§A4.9: element 3).
func replyU(t *testing.T, op driver.Op) int64 {
	t.Helper()
	if !op.Replied || len(op.Reply.Elems) < 4 {
		t.Fatalf("%s acquire of %q has no reply u", op.Kind, op.Key)
	}
	return op.Reply.Elems[3].Int
}

// doneAll finishes every permit of ops.
func doneAll(ops []driver.Op) {
	for _, op := range ops {
		if op.Permit != nil {
			op.Permit.Done()
		}
	}
}

// step advances d's controlled clock by ns, failing the test on a refusal.
func step(t *testing.T, d *driver.Driver, ns int64) {
	t.Helper()
	if err := d.Clock().Advance(ns); err != nil {
		t.Fatal(err)
	}
}

// waitMax bounds every wait for a refund event or a state the test drives to (plan choice).
const waitMax = 10 * time.Second

// fastDriver returns a driver of n instances on driver.FastPolicy and driver.FastInstance
// (plan S6 step 10), with edit, when not nil, applied to both first.
func fastDriver(t *testing.T, n int, edit func(*limiter.Policy, *limiter.Instance)) *driver.Driver {
	t.Helper()
	p := driver.FastPolicy()
	in := driver.FastInstance(p)
	if edit != nil {
		edit(&p, &in)
	}
	return driver.New(t, driver.Options{Instances: n, Policy: p, Instance: in})
}

// allowed acquires key on instance i and fails the test unless it is allowed.
func allowed(t *testing.T, d *driver.Driver, i int, key string) driver.Op {
	t.Helper()
	op := d.Acquire(i, key)
	if op.Kind != limiter.Allowed || op.Permit == nil {
		t.Fatalf("acquire of %q on instance %d: %v (%+v), want allowed", key, i, op.Kind, op.Diag)
	}
	return op
}

// event waits up to waitMax for the final refund event of op's permit.
func event(t *testing.T, d *driver.Driver, op driver.Op) limiter.RefundEvent {
	t.Helper()
	ev, err := d.WaitRefund(op, waitMax)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// refundSends is the number of refund commands instance i wrote bytes of.
func refundSends(d *driver.Driver, i int) int { return driver.Count(d.Sent(i), "refund") }

// metric returns instance i's value of the series of family name with label value label.
func metric(t *testing.T, d *driver.Driver, i int, name, label string) float64 {
	t.Helper()
	v, ok := driver.MetricValue(d.Limiter(i).Metrics(), name, label)
	if !ok {
		t.Fatalf("instance %d has no series %s{%q}", i, name, label)
	}
	return v
}

// slotsInUse returns instance i's rl_slots_in_use for receipts and refunds.
func slotsInUse(t *testing.T, d *driver.Driver, i int) (receipts, refunds float64) {
	t.Helper()
	return metric(t, d, i, "rl_slots_in_use", "receipt"), metric(t, d, i, "rl_slots_in_use", "refund")
}

// waitUntil polls cond every 2 ms until it holds or deadline passes, and reports whether it
// held.
func waitUntil(deadline time.Time, cond func() bool) bool {
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// token is one token of p in units: rate_period_ns (BUD-13).
func token(p limiter.Policy) int64 { return p.RatePeriodNs }
