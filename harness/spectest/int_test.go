//go:build rltest

package spectest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/driver"
	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
	"github.com/srir84/ratelimiter/internal/testutil/storestate"
	"github.com/srir84/ratelimiter/limiter"
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

// TestT_INT_2 (SPEC T-INT-2; STA-9, STA-10, API-9; FLT-7), in both modes: deleting the bucket of
// an indexed key before it is eligible (DeleteBucket: k:<enc> gone, its index entry kept) makes
// the next acquire state_integrity_error for that key, reason index_without_bucket, never
// degraded; inspect shows the quarantine flag, and the key stays refused (quarantined).
func TestT_INT_2(t *testing.T) {
	bothModes(t, 1, nil, func(t *testing.T, d *driver.Driver) {
		const key = "int2"
		d.Done(allowed(t, d, 0, key))
		if err := d.Tamper().DeleteBucket(key); err != nil {
			t.Fatal(err)
		}
		refusedIntegrity(t, "acquire after the delete", d.Acquire(0, key), "key", "index_without_bucket")
		if in := d.Inspect(key); !strings.HasPrefix(in.Quarantine, "index_without_bucket|") || in.Present != 0 {
			t.Errorf("inspect: %+v, want no bucket and the quarantine flag index_without_bucket|<t>", in)
		}
		refusedIntegrity(t, "acquire again", d.Acquire(0, key), "key", "quarantined")
	})
}

// int3Case is one corruption of SPEC T-INT-3 with the paths that can reach it.
type int3Case struct {
	name   string
	paths  []string
	scope  string // key: quarantine; epoch: epoch-wide, nothing written
	reason string
	// tamper damages key (encoded enc) in epoch id; g and s name its one live marker.
	tamper func(tm *fixtures.Tamper, key, enc, id string, g, s int64) error
}

// int3Cases are SPEC T-INT-3's corruptions, each with exactly the paths SPEC lists for it.
func int3Cases() []int3Case {
	all := []string{"acquire", "refund", "cleanup"}
	return []int3Case{
		{"malformed_bucket", all, "key", "malformed_bucket", func(tm *fixtures.Tamper, _, enc, id string, _, _ int64) error {
			return tm.CorruptField(keys.BucketKey(id, enc), "u", "x")
		}},
		{"malformed_marker", all, "key", "malformed_marker", func(tm *fixtures.Tamper, _, enc, id string, g, s int64) error {
			return tm.CorruptField(keys.BucketKey(id, enc), fmt.Sprintf("m:%d:%d", g, s), "x")
		}},
		{"malformed_index_entry", all, "key", "malformed_index", func(tm *fixtures.Tamper, _, enc, id string, _, _ int64) error {
			return tm.CorruptField(keys.IndexKey(id), enc, "1.5")
		}},
		{"malformed_epoch_record", all, "epoch", "epoch_record_malformed", func(tm *fixtures.Tamper, _, _, _ string, _, _ int64) error {
			return tm.CorruptRecord()
		}},
		{"malformed_high-water_mark", all, "epoch", "hwm_malformed", func(tm *fixtures.Tamper, _, _, _ string, _, _ int64) error {
			return tm.CorruptField(keys.EpochKey, "hwm", "x")
		}},
		{"bucket_without_index_entry", []string{"acquire", "refund"}, "key", "bucket_without_index", func(tm *fixtures.Tamper, key, _, _ string, _, _ int64) error {
			return tm.DeleteIndexMember(key)
		}},
		{"unexpired_marker_without_bucket", all, "key", "marker_without_bucket", func(tm *fixtures.Tamper, key, _, _ string, _, _ int64) error {
			return tm.DeleteBucketFields(key)
		}},
		{"marker_deleted_index_entry_kept", []string{"refund", "cleanup"}, "key", "marker_count", func(tm *fixtures.Tamper, key, _, _ string, g, s int64) error {
			return tm.DeleteMarkerField(key, g, s)
		}},
		{"bucket_and_index_deleted_live_marker_kept", []string{"acquire", "refund"}, "key", "marker_index_without_bucket", func(tm *fixtures.Tamper, key, _, _ string, _, _ int64) error {
			if err := tm.DeleteBucket(key); err != nil {
				return err
			}
			return tm.DeleteIndexMember(key)
		}},
	}
}

// TestT_INT_3 (SPEC T-INT-3; STA-8, STA-9, STA-10, API-2a, CMP-7; FLT-7, FLT-11): subtests
// <corruption>/<path>/<mode>, each on its own driver. At a frozen t0 the key gets two allows; the
// first is released and applied (one live marker, expiring with the receipts at t0 + 6 s), the
// second stays open. The tamper tool applies the corruption, then the path runs: an acquire, the
// open permit's release (its first refund call, captured), or a cleanup pass once the key is due
// (t0 + 6 s). Each gets state_integrity_error, never degraded: per key with the corruption's
// §A3.3 reason (code 23, or cleanup's 32 with Quarantined 1), epoch-wide for the record and the
// high-water mark (code 22, the pass stopped as state_integrity_error). After an epoch-wide
// refusal the instance reports not_ready (state_lost) once its epoch read shows the damage, and
// an acquire is still refused (epoch-wide, the same reason, never degraded). No credit and no
// fresh bucket: for a per-key case the key's inspect (bucket fields, marker count and expiries)
// and the status's nb and nm are unchanged and the quarantine flag carries the reason; an
// epoch-wide refusal writes nothing at all (the store is unchanged).
func TestT_INT_3(t *testing.T) {
	for _, c := range int3Cases() {
		t.Run(c.name, func(t *testing.T) {
			for _, path := range c.paths {
				t.Run(path, func(t *testing.T) {
					for _, m := range modes() {
						t.Run(m.String(), func(t *testing.T) { int3Leaf(t, c, path, m) })
					}
				})
			}
		})
	}
}

// int3Leaf runs one corruption through one path in one mode (TestT_INT_3).
func int3Leaf(t *testing.T, c int3Case, path string, mode limiter.FailureMode) {
	d := modeDriver(t, 1, mode, nil)
	const key = "int3"
	t0 := d.Clock().NowNs()
	r1, r2 := allowed(t, d, 0, key), allowed(t, d, 0, key)
	releaseApplied(t, d, r1)
	g, s := r1.Reply.Elems[4].Int, r1.Reply.Elems[5].Int
	if err := c.tamper(d.Tamper(), key, b64(key), d.ReadEpoch().ID, g, s); err != nil {
		t.Fatal(err)
	}
	tampered := time.Now()
	if path == "cleanup" { // the key is due at t0 + 6 s: its receipts' and its marker's expiry
		setClock(t, d, t0+d.Policy().ReceiptLifetimeNs)
	}
	var in0 driver.Inspect
	var st0 driver.Status
	var dump0 storestate.State
	if c.scope == "key" {
		in0, st0 = d.Inspect(key), d.Status()
		if in0.Code != 40 || st0.Code != 40 || in0.Quarantine != "" {
			t.Fatalf("before the %s: inspect %+v, status %+v; want both readable and no quarantine", path, in0, st0)
		}
	} else {
		dump0 = d.Dump()
	}
	code := int64(23)
	if c.scope == "epoch" {
		code = 22
	}
	switch path {
	case "acquire":
		refusedIntegrity(t, "acquire", d.Acquire(0, key), c.scope, c.reason)
	case "refund":
		d.Release(r2)
		storeReply(t, "the open permit's first refund call", firstAttempt(t, d, r2), code, c.reason)
	case "cleanup":
		rep := d.RunCleanupPass(0)
		switch c.scope {
		case "epoch":
			if rep.Status != "state_integrity_error" {
				t.Errorf("pass: %+v, want stopped as state_integrity_error", rep)
			}
		default:
			steps := d.StepsFor(key)
			if rep.Status != "complete" || rep.Quarantined != 1 || rep.Removed+rep.Moved != 0 || len(steps) != 1 || steps[0].Code != 32 || steps[0].Reason != c.reason {
				t.Errorf("pass: %+v, steps %+v; want complete with Quarantined 1 alone, one step 32 %s", rep, steps, c.reason)
			}
		}
	}
	if c.scope == "epoch" { // once its epoch read shows the damage: state_lost, and still refused (CFG-3)
		waitAllHealth(t, d, "not_ready", "state_lost", tampered.Add(epochReads(d)))
		refusedIntegrity(t, "acquire once the instance reports state_lost", d.Acquire(0, key), "epoch", c.reason)
	}
	if c.scope == "key" {
		in1, st1 := d.Inspect(key), d.Status()
		if !sameBucket(in0, in1) || !strings.HasPrefix(in1.Quarantine, c.reason+"|") {
			t.Errorf("after the %s: inspect %+v, want the bucket of %+v unchanged and the quarantine flag %s|<t>", path, in1, in0, c.reason)
		}
		if st1.NB != st0.NB || st1.NM != st0.NM || st1.QuarantinedCount != 1 {
			t.Errorf("after the %s: status %+v, want nb %d and nm %d unchanged and 1 key quarantined", path, st1, st0.NB, st0.NM)
		}
	} else if dump1 := d.Dump(); !reflect.DeepEqual(dump0, dump1) {
		t.Errorf("after the epoch-wide %s refusal the store changed:\nbefore %v\nafter  %v", path, dump0, dump1)
	}
	if path == "refund" {
		d.Kill(0) // the refund retries until its retention timer; nothing more to see
		return
	}
	d.Done(r2)
}

// TestT_INT_4 (SPEC T-INT-4; STA-9, CFG-3, CFG-6; FLT-7), in both modes: after a wipe mid-epoch
// (FLUSHALL), the joined instances refuse every acquire (epoch-wide state_integrity_error,
// epoch_record_missing, never degraded: right after the wipe, and again once their health is
// not_ready (state_lost)) and every refund (code 22, nothing credited: the store stays empty).
// An instance restarted after the wipe reports not_ready (no_epoch) within one re-read interval
// and refuses with not_ready.
func TestT_INT_4(t *testing.T) {
	bothModes(t, 2, nil, func(t *testing.T, d *driver.Driver) {
		const key = "int4"
		held := []driver.Op{allowed(t, d, 0, key), allowed(t, d, 1, key)}
		if err := d.Tamper().Wipe(); err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			refusedIntegrity(t, fmt.Sprintf("acquire on instance %d after the wipe", i), d.Acquire(i, key), "epoch", "epoch_record_missing")
		}
		waitAllHealth(t, d, "not_ready", "state_lost", time.Now().Add(epochReads(d)))
		for i := range 2 { // once state_lost is reported, still refused (CFG-3: all refuse in both modes)
			refusedIntegrity(t, fmt.Sprintf("acquire on instance %d in state_lost", i), d.Acquire(i, key), "epoch", "epoch_record_missing")
		}
		for i, op := range held {
			d.Release(op)
			storeReply(t, fmt.Sprintf("instance %d's refund after the wipe", i), firstAttempt(t, d, op), 22, "epoch_record_missing")
		}
		if left := d.StoreKeys("rl:*"); len(left) != 0 {
			t.Errorf("store after the refused calls: %v, want empty", left)
		}
		if err := d.Restart(1); err != nil {
			t.Fatal(err)
		}
		if err := waitHealth(d.Limiter(1), "not_ready", "no_epoch", time.Now().Add(joinTolerance(d))); err != nil {
			t.Errorf("restarted instance: %v", err)
		}
		if op := d.Acquire(1, key); op.Kind != limiter.NotReady || op.Permit != nil {
			t.Errorf("restarted instance: %v, want not_ready", op.Kind)
		}
		d.Kill(0) // its refunds retry until their retention timers
	})
}

// TestT_INT_5 (SPEC T-INT-5; STA-10): after T-INT-2's setup on two keys (each quarantined by an
// acquire after DeleteBucket), the clock passes their old eligibility and a pass keeps both
// entries (moved, quarantined); instance 0 restarts, a refuse fault makes one acquire
// unavailable and is disarmed, and both keys are still refused (quarantined). Restore of a full
// balance and clear bring the first back (allowed, on the restored generation) while the second
// stays refused; a new epoch (ReplaceEpoch) brings the second back.
func TestT_INT_5(t *testing.T) {
	d := fastDriver(t, 1, nil)
	ks := []string{"int5-a", "int5-b"}
	t0 := d.Clock().NowNs()
	for _, k := range ks {
		d.Done(allowed(t, d, 0, k))
		if err := d.Tamper().DeleteBucket(k); err != nil {
			t.Fatal(err)
		}
		refusedIntegrity(t, k, d.Acquire(0, k), "key", "index_without_bucket")
	}
	setClock(t, d, t0+d.Policy().ReceiptLifetimeNs+second)
	if rep := passOK(t, d, 0); rep.Moved != 2 || rep.Removed+rep.Quarantined != 0 {
		t.Errorf("pass past the old eligibility: %+v, want both entries moved and kept", rep)
	}
	for _, k := range ks {
		if steps := d.StepsFor(k); len(steps) != 1 || steps[0].Code != 31 || steps[0].Reason != "quarantined" {
			t.Errorf("%s: steps %+v, want one 31 quarantined", k, steps)
		}
	}
	if err := d.Restart(0); err != nil {
		t.Fatal(err)
	}
	if err := d.WaitReady(0, waitMax); err != nil {
		t.Fatal(err)
	}
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Refuse, Match: "acquire"})
	if op := d.Acquire(0, ks[0]); op.Kind != limiter.Unavailable {
		t.Errorf("acquire with the store refused: %v, want unavailable", op.Kind)
	}
	d.StoreWrapper(0).Disarm()
	for _, k := range ks {
		refusedIntegrity(t, k+" after the restart and the outage", d.Acquire(0, k), "key", "quarantined")
	}
	r := d.Restore(ks[0], d.Policy().Capacity*token(d.Policy()))
	g, ok := r.Int("g")
	if r.Code != 0 || !ok {
		t.Fatalf("restore: exit %d %s", r.Code, r.Stderr)
	}
	refusedIntegrity(t, ks[0]+" restored, not cleared", d.Acquire(0, ks[0]), "key", "quarantined")
	if c := d.Clear(ks[0]); c.Code != 0 {
		t.Fatalf("clear: exit %d %s", c.Code, c.Stderr)
	}
	if op := allowed(t, d, 0, ks[0]); op.Reply.Elems[4].Int != g {
		t.Errorf("%s after restore and clear: g %d, want the restored generation %d", ks[0], op.Reply.Elems[4].Int, g)
	} else {
		d.Done(op)
	}
	refusedIntegrity(t, ks[1]+" before the new epoch", d.Acquire(0, ks[1]), "key", "quarantined")
	start := time.Now()
	id := d.ReplaceEpoch()
	if err := d.WaitJoined(0, id, joinTolerance(d)-time.Since(start)); err != nil {
		t.Fatal(err)
	}
	d.Done(allowed(t, d, 0, ks[1]))
}

// TestT_INT_6 (SPEC T-INT-6; STA-5, STA-10; FLT-7): a key's bucket is deleted right after its
// allow (DeleteBucket), with no traffic for it until past its eligibility (t0 + 6 s + 1 s); the
// pass then quarantines the key instead of deleting the entry (Quarantined 1, one step 32
// index_without_bucket, the index entry kept), and an acquire gets state_integrity_error.
func TestT_INT_6(t *testing.T) {
	d := fastDriver(t, 1, nil)
	const key = "int6"
	t0 := d.Clock().NowNs()
	d.Done(allowed(t, d, 0, key))
	if err := d.Tamper().DeleteBucket(key); err != nil {
		t.Fatal(err)
	}
	setClock(t, d, t0+d.Policy().ReceiptLifetimeNs+second)
	rep := passOK(t, d, 0)
	steps := d.StepsFor(key)
	if rep.Quarantined != 1 || rep.Removed+rep.Moved != 0 || len(steps) != 1 || steps[0].Code != 32 || steps[0].Reason != "index_without_bucket" {
		t.Errorf("pass: %+v, steps %+v; want Quarantined 1 alone, one step 32 index_without_bucket", rep, steps)
	}
	if in := d.Inspect(key); in.IxScore < 0 || !strings.HasPrefix(in.Quarantine, "index_without_bucket|") {
		t.Errorf("inspect after the pass: %+v, want the index entry kept and the key quarantined", in)
	}
	refusedIntegrity(t, "acquire", d.Acquire(0, key), "key", "quarantined")
}

// quarantineDepleted depletes key at the frozen clock on instance 0 (exactly 20 allows, then
// limited), finishes all but the last two permits with done() and keeps those open as old
// receipts, then deletes the key's index entry (FLT-7): the next acquire finds a bucket without
// its index entry and quarantines the key (state_integrity_error bucket_without_index). It
// returns the two open permits.
func quarantineDepleted(t *testing.T, d *driver.Driver, key string) []driver.Op {
	t.Helper()
	ops := burst(t, d, key, 20)
	for _, op := range ops[:18] {
		d.Done(op)
	}
	if err := d.Tamper().DeleteIndexMember(key); err != nil {
		t.Fatal(err)
	}
	refusedIntegrity(t, "acquire of the depleted key without its index entry", d.Acquire(0, key), "key", "bucket_without_index")
	if in := d.Inspect(key); !strings.HasPrefix(in.Quarantine, "bucket_without_index|") {
		t.Fatalf("%s: %+v, want quarantined bucket_without_index", key, in)
	}
	return ops[18:]
}

// adminCode fails the test unless r exited with want.
func adminCode(t *testing.T, what string, r driver.AdminResult, want int) {
	t.Helper()
	if r.Code != want {
		t.Errorf("%s: rladmin %q exited %d (%s%s), want %d", what, r.Args, r.Code, r.Stdout, r.Stderr, want)
	}
}

// TestT_INT_7 (SPEC T-INT-7; STA-10, STA-4, ADM-2, ADM-3), relying on the controlled clock to pass
// a receipt's expiry before its retention timer fires; subtests (a)–(f), each on its own driver,
// on a key depleted at t0 and quarantined (quarantineDepleted, or for (e) by a marker deleted
// with its index entry kept): (a) Clear without a restore is refused (exit 2) and the key stays
// quarantined; so is Clear of a depleted key with only its quarantine flag left (bucket and
// index entry deleted, its open refund then refund_bucket_missing): exit 2 bucket_missing, the
// key unchanged and still refused (ADM-3). (b) A restore of 3 tokens writes exactly them
// (u = 3 × 10^9 at the restore's t, s = 0, x = t + 6 s); past the restored bucket's eligibility
// (t + 6 s) a pass keeps the quarantined bucket (one step 31 quarantined); Clear succeeds; the
// key resumes on the restored generation, no new bucket made (nb and gen unchanged), with 3
// tokens plus refill: by hand, min(20 × 10^9, 3 × 10^9 + 7,000,000 µs × 10^4) − 10^9 =
// 19 × 10^9 units left after the first allow, and 19 more allows; the old receipts end
// receipt_expired. (c) A restore of a key not quarantined is refused (exit 2). (d) After a
// replace, restore and clear naming the old epoch are refused (exit 2) and change nothing. (e)
// A key quarantined by "marker deleted, index entry kept" (its refund finds marker_count):
// Restore removes the orphaned marker index, then Clear succeeds, and the refund ends
// receipt_invalid. (f) Restore of a full balance and Clear at once; the refund of an unexpired
// receipt issued before the restore ends receipt_invalid and the key is not quarantined again;
// a pass after restore time + lifetime removes the bucket.
func TestT_INT_7(t *testing.T) {
	t.Run("a", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		const key = "int7a"
		held := quarantineDepleted(t, d, key)
		in0 := d.Inspect(key)
		adminCode(t, "clear without a restore", d.Clear(key), 2)
		if in := d.Inspect(key); !sameBucket(in, in0) || in.Quarantine != in0.Quarantine {
			t.Errorf("after the refused clear: %+v, want unchanged %+v", in, in0)
		}
		refusedIntegrity(t, "acquire after the refused clear", d.Acquire(0, key), "key", "quarantined")
		doneAll(held)

		// ADM-3: a depleted key with only its quarantine flag left (bucket and index entry
		// deleted, then its open permit's refund finds nothing: refund_bucket_missing) is
		// refused too (bucket_missing), so it never resumes as a fresh full bucket.
		const flagOnly = "int7a-flag-only"
		ops := burst(t, d, flagOnly, 20)
		doneAll(ops[:19])
		tm := d.Tamper()
		if err := tm.DeleteBucket(flagOnly); err != nil {
			t.Fatal(err)
		}
		if err := tm.DeleteIndexMember(flagOnly); err != nil {
			t.Fatal(err)
		}
		d.Release(ops[19])
		storeReply(t, "the refund after the bucket and its index entry were deleted", firstAttempt(t, d, ops[19]), 23, "refund_bucket_missing")
		fo := d.Inspect(flagOnly)
		if fo.Present != 0 || fo.IxScore >= 0 || !strings.HasPrefix(fo.Quarantine, "refund_bucket_missing|") {
			t.Fatalf("%s after the refund: %+v, want no bucket, no index entry, the quarantine flag alone", flagOnly, fo)
		}
		if c := d.Clear(flagOnly); c.Code != 2 || !strings.Contains(c.Stderr, "bucket_missing") {
			t.Errorf("clear of a key with only its flag left: exit %d %q, want 2 bucket_missing", c.Code, c.Stderr)
		}
		if in := d.Inspect(flagOnly); !reflect.DeepEqual(in, fo) {
			t.Errorf("%s after the refused clear: %+v, want unchanged %+v", flagOnly, in, fo)
		}
		refusedIntegrity(t, "acquire of the flag-only key after the refused clear", d.Acquire(0, flagOnly), "key", "quarantined")
		d.Kill(0) // its refund retries until its retention timer
	})
	t.Run("b", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		p := d.Policy()
		const key = "int7b"
		held := quarantineDepleted(t, d, key)
		r := d.Restore(key, 3*token(p))
		g, okG := r.Int("g")
		tR, okT := r.Int("t_us")
		if r.Code != 0 || !okG || !okT {
			t.Fatalf("restore: exit %d %s", r.Code, r.Stderr)
		}
		const lifetimeUs = 6_000_000 // FastPolicy's receipt lifetime
		if in := d.Inspect(key); in.U != 3_000_000_000 || in.T != tR || in.G != g || in.S != 0 || in.X != tR+lifetimeUs || in.MC != 0 || in.Quarantine == "" {
			t.Errorf("after the restore: %+v, want u 3e9, t %d, g %d, s 0, x %d, mc 0, still quarantined", in, tR, g, tR+lifetimeUs)
		}
		nb, gen := d.Status().NB, d.ReadEpoch().Gen
		setClock(t, d, (tR+lifetimeUs+1_000_000)*1000) // past the eligibility, t + 6 s
		passOK(t, d, 0)
		if steps := d.StepsFor(key); len(steps) != 1 || steps[0].Code != 31 || steps[0].Reason != "quarantined" {
			t.Errorf("pass past the restored bucket's eligibility: steps %+v, want one 31 quarantined (bucket kept)", steps)
		}
		if in := d.Inspect(key); in.Present != 1 || in.G != g {
			t.Errorf("after the pass: %+v, want the restored bucket kept", in)
		}
		adminCode(t, "clear", d.Clear(key), 0)
		ops := burst(t, d, key, 20)
		want := min(20_000_000_000, 3_000_000_000+7_000_000*10_000) - 1_000_000_000
		if first := ops[0]; first.Reply.Elems[4].Int != g || replyU(t, first) != int64(want) {
			t.Errorf("first allow after the clear: g %d, u %d; want the restored g %d and u %d", first.Reply.Elems[4].Int, replyU(t, first), g, want)
		}
		doneAll(ops)
		if st, e := d.Status(), d.ReadEpoch(); st.NB != nb || e.Gen != gen {
			t.Errorf("after the clear and the allows: nb %d, gen %d; want %d and %d (no new bucket)", st.NB, e.Gen, nb, gen)
		}
		for _, op := range held {
			releaseExpired(t, d, op)
		}
	})
	t.Run("c", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		const key = "int7c"
		d.Done(allowed(t, d, 0, key))
		in0 := d.Inspect(key)
		adminCode(t, "restore of a key not quarantined", d.Restore(key, 0), 2)
		if in := d.Inspect(key); !sameBucket(in, in0) || in.Quarantine != "" {
			t.Errorf("after the refused restore: %+v, want unchanged %+v", in, in0)
		}
	})
	t.Run("d", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		const key = "int7d"
		stale := d.ReadEpoch().ID
		start := time.Now()
		id := d.ReplaceEpoch()
		if err := d.WaitJoined(0, id, joinTolerance(d)-time.Since(start)); err != nil {
			t.Fatal(err)
		}
		held := quarantineDepleted(t, d, key)
		in0, dump0 := d.Inspect(key), d.Dump()
		adminCode(t, "restore with a stale --expect-epoch", d.RLAdmin("restore", "--key", key, "--balance-units", "0", "--expect-epoch", stale), 2)
		adminCode(t, "clear with a stale --expect-epoch", d.RLAdmin("clear", "--key", key, "--expect-epoch", stale), 2)
		if in := d.Inspect(key); !sameBucket(in, in0) || in.Quarantine != in0.Quarantine || !reflect.DeepEqual(d.Dump(), dump0) {
			t.Errorf("after the refused restore and clear: %+v, want the store unchanged (%+v)", in, in0)
		}
		doneAll(held)
	})
	t.Run("e", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		const key = "int7e"
		ops := burst(t, d, key, 20)
		for _, op := range ops[:18] {
			d.Done(op)
		}
		releaseApplied(t, d, ops[18]) // its marker m:<g>:19
		g, s := ops[18].Reply.Elems[4].Int, ops[18].Reply.Elems[5].Int
		if err := d.Tamper().DeleteMarkerField(key, g, s); err != nil {
			t.Fatal(err)
		}
		d.Release(ops[19])
		storeReply(t, "the refund after the marker's deletion", firstAttempt(t, d, ops[19]), 23, "marker_count")
		if in := d.Inspect(key); !strings.HasPrefix(in.Quarantine, "marker_count|") || len(in.Expiries) != 1 {
			t.Fatalf("after the refund: %+v, want quarantined marker_count with the orphaned marker index entry", in)
		}
		adminCode(t, "restore", d.Restore(key, 0), 0)
		if in := d.Inspect(key); in.MC != 0 || len(in.Expiries) != 0 || in.Quarantine == "" {
			t.Errorf("after the restore: %+v, want no marker and no marker index entry, still quarantined", in)
		}
		if mi := d.StoreKeys(keys.MarkerIndexKey(d.ReadEpoch().ID, b64(key))); len(mi) != 0 {
			t.Errorf("after the restore: %v still exists", mi)
		}
		adminCode(t, "clear", d.Clear(key), 0)
		if in := d.Inspect(key); in.Quarantine != "" {
			t.Errorf("after the clear: %+v, want no quarantine", in)
		}
		if ev := event(t, d, ops[19]); ev.Result != limiter.ReceiptInvalid {
			t.Errorf("the open refund after the restore and clear: %+v, want receipt_invalid", ev)
		}
	})
	t.Run("f", func(t *testing.T) {
		d := fastDriver(t, 1, nil)
		p := d.Policy()
		const key = "int7f"
		held := quarantineDepleted(t, d, key)
		r := d.Restore(key, p.Capacity*token(p))
		tR, ok := r.Int("t_us")
		if r.Code != 0 || !ok {
			t.Fatalf("restore: exit %d %s", r.Code, r.Stderr)
		}
		adminCode(t, "clear", d.Clear(key), 0)
		if ev := event(t, d, releaseOp(d, held[0])); ev.Result != limiter.ReceiptInvalid {
			t.Errorf("refund of a receipt from before the restore: %+v, want receipt_invalid", ev)
		}
		if in := d.Inspect(key); in.Quarantine != "" || in.Present != 1 {
			t.Errorf("after the refund: %+v, want the bucket, not quarantined again", in)
		}
		setClock(t, d, tR*1000+p.ReceiptLifetimeNs)
		if rep := passOK(t, d, 0); rep.Removed != 1 {
			t.Errorf("pass at restore time + lifetime: %+v, want the bucket removed", rep)
		}
		if in := d.Inspect(key); in.Present != 0 {
			t.Errorf("after the pass: %+v, want absent", in)
		}
		doneAll(held[1:])
	})
}

// releaseOp releases op's permit through the driver and returns op.
func releaseOp(d *driver.Driver, op driver.Op) driver.Op {
	d.Release(op)
	return op
}

// TestT_INT_8 (SPEC T-INT-8; STA-6a, STA-10; FLT-7): with max_tracked_buckets = 5 filled (a 6th
// key gets capacity_error tracked_buckets), deleting one key's bucket makes its next acquire
// state_integrity_error with the quarantine written, never capacity_error, and so does the one
// after.
func TestT_INT_8(t *testing.T) {
	d := fastDriver(t, 1, func(p *limiter.Policy, _ *limiter.Instance) { p.MaxTrackedBuckets = 5 })
	for k := range 5 {
		d.Done(allowed(t, d, 0, fmt.Sprintf("int8-%d", k)))
	}
	if op := d.Acquire(0, "int8-new"); op.Kind != limiter.CapacityError {
		t.Fatalf("a 6th key: %v, want capacity_error (the limit is filled)", op.Kind)
	}
	if err := d.Tamper().DeleteBucket("int8-0"); err != nil {
		t.Fatal(err)
	}
	refusedIntegrity(t, "acquire after the delete", d.Acquire(0, "int8-0"), "key", "index_without_bucket")
	if in := d.Inspect("int8-0"); !strings.HasPrefix(in.Quarantine, "index_without_bucket|") {
		t.Errorf("inspect: %+v, want quarantined", in)
	}
	refusedIntegrity(t, "acquire again", d.Acquire(0, "int8-0"), "key", "quarantined")
}

// TestT_INT_9 (SPEC T-INT-9; STA-9, API-9, ADM-1, CFG-6, ADM-4, API-1g; FLT-7), in both modes: a
// malformed index member (AddMalformedIndexMember, score 0) makes the pass on instance 1 set the
// integrity hold unattributable_entry (the pass stops, state_integrity_error). An unrelated key
// on instance 2 is refused (epoch-wide integrity_hold, never degraded), also after Restart(2);
// every instance reports not_ready (integrity_hold) and rl_integrity_hold = 1; status shows the
// hold. After ReplaceEpoch the hold is gone, rl_integrity_hold = 0, every instance is ready in
// the new epoch within the join tolerance, and the key is allowed.
func TestT_INT_9(t *testing.T) {
	bothModes(t, 3, nil, func(t *testing.T, d *driver.Driver) {
		const key = "int9-other"
		d.Done(allowed(t, d, 2, key))
		if err := d.Tamper().AddMalformedIndexMember("!bad", 0); err != nil {
			t.Fatal(err)
		}
		if rep := d.RunCleanupPass(1); rep.Status != "state_integrity_error" {
			t.Errorf("pass on instance 1: %+v, want stopped as state_integrity_error", rep)
		}
		if st := d.Status(); st.Hold != "unattributable_entry" {
			t.Errorf("status: %+v, want hold unattributable_entry", st)
		}
		refusedIntegrity(t, "unrelated key on instance 2", d.Acquire(2, key), "epoch", "integrity_hold")
		if err := d.Restart(2); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(epochReads(d))
		waitAllHealth(t, d, "not_ready", "integrity_hold", deadline)
		refusedIntegrity(t, "unrelated key on the restarted instance 2", d.Acquire(2, key), "epoch", "integrity_hold")
		for i := range 3 {
			waitMetric(t, d, i, "rl_integrity_hold", "", time.Until(deadline), "under the hold", func(v float64) bool { return v == 1 })
		}
		if st := d.Status(); st.Hold != "unattributable_entry" {
			t.Errorf("status before the replace: %+v, want the hold", st)
		}
		start := time.Now()
		id := d.ReplaceEpoch()
		waitAllJoined(t, d, id, start.Add(joinTolerance(d)))
		if st := d.Status(); st.Hold != "" || st.EpochID != id {
			t.Errorf("status after the replace: %+v, want epoch %s with no hold", st, id)
		}
		for i := range 3 {
			waitMetric(t, d, i, "rl_integrity_hold", "", epochReads(d), "after the replace", func(v float64) bool { return v == 0 })
		}
		d.Done(allowed(t, d, 2, key))
	})
}

// TestT_INT_10 (SPEC T-INT-10; API-19, STA-10; FLT-7): the refund of an unexpired, correctly
// signed receipt after (i) DeleteBucket (no marker: the index entry alone is left) gets
// state_integrity_error index_without_bucket, and after (ii) deleting k, mi (the key has one
// live marker) and the index entry, so nothing of the key is left, gets refund_bucket_missing.
// Both write the quarantine flag, and a later acquire is refused.
func TestT_INT_10(t *testing.T) {
	d := fastDriver(t, 1, nil)
	cases := []struct {
		key, reason string
		marker      bool // one released permit first: a live marker, so the key has an mi
		damage      func(tm *fixtures.Tamper, key string) error
	}{
		{"int10-i", "index_without_bucket", false, func(tm *fixtures.Tamper, key string) error { return tm.DeleteBucket(key) }},
		{"int10-ii", "refund_bucket_missing", true, func(tm *fixtures.Tamper, key string) error {
			for _, del := range []func(string) error{tm.DeleteBucket, tm.DeleteMarkerIndex, tm.DeleteIndexMember} {
				if err := del(key); err != nil {
					return err
				}
			}
			return nil
		}},
	}
	for _, c := range cases {
		if c.marker {
			releaseApplied(t, d, allowed(t, d, 0, c.key))
		}
		open := allowed(t, d, 0, c.key)
		if err := c.damage(d.Tamper(), c.key); err != nil {
			t.Fatal(err)
		}
		d.Release(open)
		storeReply(t, c.key+": refund", firstAttempt(t, d, open), 23, c.reason)
		if in := d.Inspect(c.key); !strings.HasPrefix(in.Quarantine, c.reason+"|") {
			t.Errorf("%s after the refund: %+v, want the quarantine flag %s|<t>", c.key, in, c.reason)
		}
		refusedIntegrity(t, c.key+": later acquire", d.Acquire(0, c.key), "key", "quarantined")
	}
	d.Kill(0) // the refunds retry until their retention timers
}

// int11Driver is T-INT-11's deployment (plan S8 Tests (b)): the durable profile (checked in
// the store: durableProfile) and the store clock, FastPolicy's other settings, two instances,
// and traffic on three keys: three allows each (allowedSettled), one released and confirmed (a
// marker), the others done.
func int11Driver(t *testing.T) (*driver.Driver, []string) {
	t.Helper()
	p := driver.FastPolicy()
	p.ClockSource = limiter.Store
	d := driver.New(t, driver.Options{Instances: 2, Policy: p, Instance: driver.FastInstance(p), Profile: storeproc.Durable})
	durableProfile(t, d)
	ks := []string{"int11-a", "int11-b", "int11-c"}
	for k, key := range ks {
		ops := []driver.Op{allowedSettled(t, d, k%2, key), allowedSettled(t, d, k%2, key), allowedSettled(t, d, k%2, key)}
		releaseConfirmed(t, d, ops[0])
		doneAll(ops[1:])
	}
	return d, ks
}

// epochWideUntilStart checks T-INT-11's recovery: every instance reports not_ready (state_lost)
// after its next epoch read, which also shows its connections are back after a store restart,
// and refuses with an epoch-wide state_integrity_error of reason; rladmin start-epoch without
// the replace flag exits 0; every instance is ready in the new epoch within the join tolerance
// and allows.
func epochWideUntilStart(t *testing.T, d *driver.Driver, key, reason string) {
	t.Helper()
	waitAllHealth(t, d, "not_ready", "state_lost", time.Now().Add(epochReads(d)))
	for i := range d.Instances() {
		refusedIntegrity(t, fmt.Sprintf("instance %d", i), d.Acquire(i, key), "epoch", reason)
	}
	start := time.Now()
	r := d.RLAdmin("start-epoch")
	adminCode(t, "start-epoch without --replace", r, 0)
	waitAllJoined(t, d, r.Str("epoch_id"), start.Add(joinTolerance(d)))
	for i := range d.Instances() {
		d.Done(allowedSettled(t, d, i, key))
	}
}

// TestT_INT_11 (SPEC T-INT-11; STA-16, CFG-6, ADM-1), durable profile, store clock: (persistence)
// Stop/Restart with the data kept leaves every key's inspect and the whole store identical, and
// no integrity error follows: the keys serve on, rl_acquire_results_total{state_integrity_error}
// is 0 and every instance is ready after an epoch read. (wipe) Stop, WipeDataDir, Restart: the
// joined instances refuse with epoch-wide state_integrity_error (epoch_record_missing, health
// state_lost) until rladmin start-epoch with no flag exits 0, then re-join within the join
// tolerance. (corrupt) CorruptRecord: the same with epoch_record_malformed.
func TestT_INT_11(t *testing.T) {
	t.Run("persistence", func(t *testing.T) {
		d, ks := int11Driver(t)
		before := map[string]driver.Inspect{}
		for _, k := range ks {
			before[k] = d.Inspect(k)
		}
		dump := d.Dump()
		d.Store().Stop()
		d.RestartStore()
		for i := range d.Instances() {
			if err := d.WaitReady(i, waitMax); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range ks {
			if in := d.Inspect(k); !reflect.DeepEqual(in, before[k]) {
				t.Errorf("%s after the restart: %+v, want %+v", k, in, before[k])
			}
		}
		if after := d.Dump(); !reflect.DeepEqual(after, dump) {
			t.Errorf("store after the restart:\n%v\nwant\n%v", after, dump)
		}
		for i := range d.Instances() {
			for _, k := range ks {
				if op := acquireSettled(t, d, i, k); op.Kind != limiter.Allowed && op.Kind != limiter.Limited {
					t.Errorf("instance %d, %s after the restart: %v %+v, want allowed or limited", i, k, op.Kind, op.Diag)
				} else {
					d.Done(op)
				}
			}
		}
		time.Sleep(interval(d) + 100*time.Millisecond)
		for i := range d.Instances() {
			if h := d.Limiter(i).Health(); h.Status != "ready" {
				t.Errorf("instance %d after an epoch read: %s/%s, want ready", i, h.Status, h.Reason)
			}
			if v := metric(t, d, i, "rl_acquire_results_total", "state_integrity_error"); v != 0 {
				t.Errorf("instance %d: %v state_integrity_error results, want 0", i, v)
			}
		}
	})
	t.Run("wipe", func(t *testing.T) {
		d, ks := int11Driver(t)
		d.Store().Stop()
		if err := d.Store().WipeDataDir(); err != nil {
			t.Fatal(err)
		}
		d.RestartStore()
		epochWideUntilStart(t, d, ks[0], "epoch_record_missing")
	})
	t.Run("corrupt", func(t *testing.T) {
		d, ks := int11Driver(t)
		if err := d.Tamper().CorruptRecord(); err != nil {
			t.Fatal(err)
		}
		epochWideUntilStart(t, d, ks[0], "epoch_record_malformed")
	})
}
