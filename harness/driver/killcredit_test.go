//go:build rltest

package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/fixtures"
	"ratelimiter/limiter"
)

// TestDriverKilledReleaseCredit (F1; the unknown-time credit of HARNESS §8 T-HFLT-4 through
// the op log, REF-8): a released permit whose instance is killed before its final event is a
// possible credit anywhere in [acquire decision_ns, + receipt lifetime), and here the verdict
// needs it. On two instances at the driver's frozen start, instance 0 takes one token of key k
// and releases it. Its refund call applies in the store (admin_inspect: u back to 20 tokens,
// the receipt's marker kept), but an after_reply barrier holds the call's return, so no final
// event comes before Kill(0). Instance 1 then takes the 20 tokens at the same instant, and its
// next acquire is limited. check_bud11 on the op log exits 0, as the teardown check does too;
// on the op log without the release record it exits 1 for a window at the frozen start that
// is one token (rate_period_ns units) over.
func TestDriverKilledReleaseCredit(t *testing.T) {
	const wait = 10 * time.Second // bounds the wait for the refund call to reach the barrier
	d := New(t, Options{Instances: 2})
	if p := d.Policy(); p.Capacity != 20 || p.RatePeriodNs != 1_000_000_000 {
		t.Fatalf("the driver's policy has capacity %d and rate_period_ns %d; this test counts on 20 and 10^9", p.Capacity, p.RatePeriodNs)
	}
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Fatalf("instance 0's acquire: %v, want allowed", op.Kind)
	}
	id := op.Permit.DeductionID()
	sw := d.StoreWrapper(0)
	if err := sw.Arm(fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", At: fixtures.AfterReply, Name: "held"}); err != nil {
		t.Fatal(err)
	}
	d.Release(op)
	select {
	case <-sw.Reached("held"):
	case <-time.After(wait):
		t.Fatal("instance 0's refund call did not reach the after_reply barrier")
	}
	if in := d.Inspect("k"); in.U != 20_000_000_000 || in.MC != 1 {
		t.Fatalf("admin_inspect k while the refund's return is held: u %d with %d markers, want 20000000000 (the refund applied) with 1", in.U, in.MC)
	}
	d.Kill(0)
	for n := range 20 {
		o := d.Acquire(1, "k")
		if o.Kind != limiter.Allowed {
			t.Fatalf("instance 1's acquire %d of 20 at the frozen start: %v, want allowed", n+1, o.Kind)
		}
		d.Done(o)
	}
	if o := d.Acquire(1, "k"); o.Kind != limiter.Limited {
		t.Fatalf("instance 1's 21st acquire at the frozen start: %v, want limited", o.Kind)
	}

	lines := d.OpLog()
	release := `{"type":"release","instance":"0","deduction_id":"` + id + `"}`
	var bare []string
	for _, l := range lines {
		if strings.Contains(l, `"type":"refund"`) {
			t.Fatalf("op log refund record %s, want none: instance 0 was killed before any final event", l)
		}
		if l != release {
			bare = append(bare, l)
		}
	}
	if n := len(lines) - len(bare); n != 1 {
		t.Fatalf("the op log has %d records %s, want 1", n, release)
	}
	for _, c := range []struct {
		name string
		log  []string
		code int
		want string // in check_bud11's output
	}{
		{"the op log", lines, 0, ""},
		{"the op log without the release record", bare, 1,
			"window [1760000000000000000, 1760000000000000000] exceeds capacity by at least 1000000000 units"},
	} {
		path := filepath.Join(t.TempDir(), "oplog.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(c.log, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, err := RunRefModel(t.Context(), "refmodel.check_bud11", path)
		if err != nil {
			t.Fatalf("check_bud11 on %s: %v (a failure, never a skip)", c.name, err)
		}
		if code != c.code || !strings.Contains(string(out), c.want) {
			t.Errorf("check_bud11 on %s: exit %d, output %q; want exit %d with %q", c.name, code, out, c.code, c.want)
		}
	}
}
