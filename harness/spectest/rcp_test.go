//go:build rltest

package spectest

import (
	"testing"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/limiter"
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
