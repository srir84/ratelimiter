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
