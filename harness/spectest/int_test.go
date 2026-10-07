//go:build rltest

package spectest

import (
	"testing"

	"ratelimiter/harness/driver"
	"ratelimiter/limiter"
)

// burst acquires key at one instant, alternating over the driver's instances, until a limited
// reply, and returns the allows; it fails the test unless there are exactly want of them.
func burst(t *testing.T, d *driver.Driver, key string, want int) []driver.Op {
	t.Helper()
	var ops []driver.Op
	for n := 0; ; n++ {
		op := d.Acquire(n%d.Instances(), key)
		if op.Kind != limiter.Allowed {
			if op.Kind != limiter.Limited || n != want {
				t.Fatalf("acquire %d of %q: %v after %d allows, want limited after exactly %d", n+1, key, op.Kind, n, want)
			}
			return ops
		}
		ops = append(ops, op)
	}
}

// TestT_INT_1 (SPEC T-INT-1; STA-8): a new key gets a full bucket: at a frozen t0 exactly 20
// allows, then limited, on generation g1. Once eligible (t0 + 6 s: refilled, its receipts
// expired) a pass removes it, and the returning key again gets a full bucket, on a new
// generation: exactly 20 allows at one instant, g2 > g1, s = 1 after the first allow; the key
// is not quarantined (inspect, the status's quarantined count 0). The op log matches the
// reference model.
func TestT_INT_1(t *testing.T) {
	d := fastDriver(t, 2, nil)
	const key = "int1"
	t0 := d.Clock().NowNs()
	first := burst(t, d, key, 20)
	g1 := first[0].Reply.Elems[4].Int
	for k, op := range first {
		if g, s := op.Reply.Elems[4].Int, op.Reply.Elems[5].Int; g != g1 || s != int64(k+1) {
			t.Errorf("new key, allow %d: g %d, s %d; want g %d, s %d", k+1, g, s, g1, k+1)
		}
	}
	doneAll(first)
	setClock(t, d, t0+d.Policy().ReceiptLifetimeNs)
	if rep := passOK(t, d, 0); rep.Removed != 1 || rep.Moved+rep.Skipped+rep.Quarantined != 0 {
		t.Errorf("pass at eligibility: %+v, want Removed 1 alone", rep)
	}
	if in := d.Inspect(key); in.Present != 0 {
		t.Fatalf("key after the pass: %+v, want absent", in)
	}
	again := burst(t, d, key, 20)
	if g2, s := again[0].Reply.Elems[4].Int, again[0].Reply.Elems[5].Int; g2 <= g1 || s != 1 {
		t.Errorf("returning key, first allow: g %d, s %d; want g above %d and s 1", g2, s, g1)
	}
	doneAll(again)
	if in := d.Inspect(key); in.Present != 1 || in.Quarantine != "" || in.S != 20 {
		t.Errorf("returning key: %+v, want a bucket with s 20 and no quarantine", in)
	}
	if st := d.Status(); st.QuarantinedCount != 0 {
		t.Errorf("status %+v, want quarantined count 0", st)
	}
	d.CheckRefModel(t)
}
