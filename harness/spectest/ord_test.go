//go:build rltest

package spectest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/driver"
	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/limiter"
)

// TestT_ORD_1 (SPEC T-ORD-1 [D]; API-1a, API-2, API-2a, API-19, CLK-4; plan S9 Tests (b)): two
// failing conditions at once, each pair on its own driver (closed mode, D = 500 ms); the earlier
// check in the documented order decides.
//   - Epoch record wiped + clock missing (rl:epoch and rl:ctl:time deleted as rl_fixture), with
//     the epoch re-read every hour so the acquire script decides, not the epoch watch: epoch-wide
//     state_integrity_error epoch_record_missing (step 1), not clock_error (step 2).
//   - Fingerprint mismatch + quarantined key: an instance with failure_mode open (another
//     fingerprint, as in T-CFG-1) reports not_ready (fingerprint_mismatch); key K is quarantined
//     in the joined epoch through the tamper tool (its index entry deleted, so the joined
//     instance's next acquire quarantines it, bucket_without_index); Acquire(K) on the mismatched
//     instance is config_error, not state_integrity_error.
//   - Expired + already refunded: the first refund's reply is dropped (applied at the store) and
//     attempt 2 is held at a before_send barrier; the clock moves past the receipt's expiry; once
//     released, attempt 2 ends receipt_expired (step 5) though the marker exists (step 8).
//   - Full marker pool + existing marker: max_live_markers = max_markers_per_key = 1; the first
//     refund fills the pool with its marker, its reply dropped; the retry is already_applied (step
//     8), not capacity_error (step 9).
//   - Unusable signer + empty bucket (the key depleted at the frozen clock, so the store would
//     answer limited): config_error signer_unusable, nothing sent, inspect unchanged.
//   - ctx already past its deadline + empty bucket: cancelled, nothing sent, inspect unchanged.
func TestT_ORD_1(t *testing.T) {
	t.Run("wiped record + missing clock", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, func(_ *limiter.Policy, in *limiter.Instance) { in.EpochRereadIntervalNs = hourNs })
		const key = "ord1-a"
		d.Done(allowed(t, d, 0, key))
		if r, err := d.Fixture().Do("DEL", keys.EpochKey, keys.CtlTimeKey); err != nil || r.Kind != ':' || r.Int != 2 {
			t.Fatalf("DEL rl:epoch rl:ctl:time: %v %v, want 2 keys deleted", r, err)
		}
		refusedIntegrity(t, "acquire with the record wiped and the clock missing", d.Acquire(0, key), "epoch", "epoch_record_missing")
	})

	t.Run("config mismatch + quarantined key", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, nil)
		const key = "ord1-b"
		open := d.Policy()
		open.FailureMode = limiter.Open
		start := time.Now()
		i, err := d.AddInstance(driver.InstanceSpec{Policy: &open})
		if err != nil {
			t.Fatal(err)
		}
		if err := waitHealth(d.Limiter(i), "not_ready", "fingerprint_mismatch", start.Add(joinTolerance(d))); err != nil {
			t.Fatalf("instance with another fingerprint: %v", err)
		}
		d.Done(allowed(t, d, 0, key))
		if err := d.Tamper().DeleteIndexMember(key); err != nil {
			t.Fatal(err)
		}
		refusedIntegrity(t, "acquire of K without its index entry on the joined instance", d.Acquire(0, key), "key", "bucket_without_index")
		if in := d.Inspect(key); !strings.HasPrefix(in.Quarantine, "bucket_without_index|") {
			t.Fatalf("K: %+v, want quarantined (bucket_without_index)", in)
		}
		if op := d.Acquire(i, key); op.Kind != limiter.ConfigError || op.Permit != nil {
			t.Errorf("Acquire(K) on the mismatched instance: %v %+v, want config_error, not state_integrity_error", op.Kind, op.Diag)
		}
	})

	t.Run("expired + already refunded", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, nil)
		op := allowed(t, d, 0, "ord1-c")
		dropThenHold(t, d, "ord1c")
		d.Release(op)
		reached(t, d, 0, "ord1c")
		appliedUnseen(t, d, "first refund", op)
		setClock(t, d, (expiryOf(op)+1)*1000) // 1 µs past the receipt's expiry
		d.StoreWrapper(0).Release("ord1c")
		if ev := event(t, d, op); ev.Result != limiter.ReceiptExpired {
			t.Errorf("attempt 2 after expiry, its marker present: %+v, want receipt_expired", ev)
		}
		if as := d.RefundAttempts(op); len(as) != 2 || !as[1].Reached || as[1].Store.Outcome != store.OK || as[1].Store.Reply.Code != 11 {
			t.Errorf("refund attempts %+v, want a second one answered by the store with code 11 (receipt_expired)", as)
		}
	})

	t.Run("full marker pool + existing marker", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, func(p *limiter.Policy, _ *limiter.Instance) { p.MaxLiveMarkers, p.MaxMarkersPerKey = 1, 1 })
		op := allowed(t, d, 0, "ord1-d")
		arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
		d.Release(op)
		if ev := event(t, d, op); ev.Result != limiter.AlreadyApplied {
			t.Errorf("retry with the pool full of its own marker: %+v, want already_applied, not capacity_error", ev)
		}
		if as := d.RefundAttempts(op); len(as) < 1 || as[0].Store.Outcome != store.OK || as[0].Store.Reply.Code != 3 {
			t.Errorf("refund attempts %+v, want the first applied at the store (code 3) with its reply lost", as)
		}
		if st := d.Status(); st.NM != 1 {
			t.Errorf("status: %d markers, want 1 (the pool full)", st.NM)
		}
	})

	t.Run("unusable signer + empty bucket", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, nil)
		const key = "ord1-e"
		ord1Empty(t, d, key)
		before := d.Inspect(key)
		if err := d.SignerWrapper(0).Arm(fixtures.SignerFault{Mode: fixtures.SignUnusable, Count: 1}); err != nil {
			t.Fatal(err)
		}
		mark := len(d.Sent(0))
		if op := d.Acquire(0, key); op.Kind != limiter.ConfigError || op.Diag.Reason != "signer_unusable" || op.Permit != nil {
			t.Errorf("unusable signer, empty bucket: %v %+v, want config_error signer_unusable, not limited", op.Kind, op.Diag)
		}
		ord1Untouched(t, d, key, mark, before)
	})

	t.Run("deadline passed + empty bucket", func(t *testing.T) {
		d := s9Driver(t, 1, limiter.Closed, nil)
		const key = "ord1-f"
		ord1Empty(t, d, key)
		before := d.Inspect(key)
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		mark := len(d.Sent(0))
		if op := d.AcquireContext(ctx, 0, key); op.Kind != limiter.Cancelled || op.Permit != nil {
			t.Errorf("ctx past its deadline, empty bucket: %v %+v, want cancelled, not limited", op.Kind, op.Diag)
		}
		ord1Untouched(t, d, key, mark, before)
	})
}

// ord1Empty empties key's bucket at the frozen clock: exactly 20 allows (the capacity), each
// finished with done(), then limited.
func ord1Empty(t *testing.T, d *driver.Driver, key string) {
	t.Helper()
	for _, op := range burst(t, d, key, 20) {
		d.Done(op)
	}
}

// ord1Untouched fails the test if instance 0 wrote an acquire since mark or key's inspect
// differs from before.
func ord1Untouched(t *testing.T, d *driver.Driver, key string, mark int, before driver.Inspect) {
	t.Helper()
	if n := driver.Count(d.Sent(0)[mark:], "acquire"); n != 0 {
		t.Errorf("%d acquire commands written, want none (a local check refuses first)", n)
	}
	if after := d.Inspect(key); !sameBucket(after, before) {
		t.Errorf("inspect %+v, want unchanged %+v", after, before)
	}
}
