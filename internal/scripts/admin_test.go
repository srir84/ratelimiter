//go:build scripttest

package scripts_test

import (
	"testing"

	"github.com/srir84/ratelimiter/internal/fingerprint"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/store"
)

// statusReq is an admin_status call (§A4.1: KEYS epoch, q; ARGV expected_id).
func statusReq(expected string) store.Request {
	return store.Request{Kind: store.Call, Script: scripts.AdminStatus, Keys: []string{keys.EpochKey, qKey}, Args: []string{expected}}
}

// inspectReq is an admin_inspect call of enc (§A4.1: KEYS epoch, ix, q, k, mi; ARGV
// expected_id, enc).
func inspectReq(expected, enc string) store.Request {
	return store.Request{
		Kind: store.Call, Script: scripts.AdminInspect,
		Keys: []string{keys.EpochKey, ixKey, qKey, kKey(enc), miKey(enc)},
		Args: []string{expected, enc},
	}
}

// TestAdminStatus (plan S4, L2; SPEC ADM-4; DESIGN §A4.6): as rl_admin, the exact
// 10-element reply, also under a hold; hwm as ” when missing or malformed and as stored
// otherwise; the record refusals; a wrong-type q is a refusal, not a hold; the store never
// changes.
func TestAdminStatus(t *testing.T) {
	p := testPolicy()
	fp := fingerprint.Of(p)
	cases := []struct {
		name     string
		setup    func(e *env)
		expected string
		damaged  []string
		want     []any
	}{
		{name: "healthy", setup: func(*env) {}, want: rep(40, 0, "", epochID, fp, "", hwm0, 1, 0, 0)},
		{name: "two quarantined keys", setup: func(e *env) {
			e.do("HSET", qKey, encB, "malformed_bucket|"+num(hwm0), encC, "marker_count|"+num(hwm0))
		}, damaged: []string{kKey(encB), kKey(encC)}, want: rep(40, 0, "", epochID, fp, "", hwm0, 1, 0, 2)},
		{name: "under a hold", setup: func(e *env) { e.do("HSET", keys.EpochKey, "hold", "store_rollback") },
			want: rep(40, 0, "", epochID, fp, "store_rollback", hwm0, 1, 0, 0)},
		{name: "hwm missing", setup: func(e *env) { e.do("HDEL", keys.EpochKey, "hwm") }, damaged: []string{keys.EpochKey},
			want: rep(40, 0, "", epochID, fp, "", "", 1, 0, 0)},
		{name: "hwm x", setup: func(e *env) { e.do("HSET", keys.EpochKey, "hwm", "x") }, damaged: []string{keys.EpochKey},
			want: rep(40, 0, "", epochID, fp, "", "", 1, 0, 0)},
		{name: "hwm with a leading zero", setup: func(e *env) { e.do("HSET", keys.EpochKey, "hwm", "01") }, damaged: []string{keys.EpochKey},
			want: rep(40, 0, "", epochID, fp, "", "", 1, 0, 0)},
		{name: "hwm TBOUND, shown as stored", setup: func(e *env) { e.do("HSET", keys.EpochKey, "hwm", num(tBound)) }, damaged: []string{keys.EpochKey},
			want: rep(40, 0, "", epochID, fp, "", tBound, 1, 0, 0)},
		{name: "empty", setup: func(e *env) { e.do("DEL", keys.EpochKey) }, want: rep(41, 0, "epoch_record_missing")},
		{name: "malformed", setup: func(e *env) { e.do("HSET", keys.EpochKey, "clock", "wall") }, damaged: []string{keys.EpochKey},
			want: rep(41, 0, "epoch_record_malformed")},
		{name: "a string", setup: func(e *env) { e.do("DEL", keys.EpochKey); e.do("SET", keys.EpochKey, "x") }, damaged: []string{keys.EpochKey},
			want: rep(41, 0, "epoch_record_malformed")},
		{name: "stale id", setup: func(*env) {}, expected: otherID, want: rep(41, 0, "epoch_changed")},
		{name: "q a string", setup: func(e *env) { e.do("SET", qKey, "x") }, damaged: []string{qKey}, want: rep(41, 0, "shared_key_malformed")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.base(encA)
			c.setup(e)
			e.h.Damaged(c.damaged...)
			expected := c.expected
			if expected == "" {
				expected = epochID
			}
			before := e.snap()
			checkReply(t, c.name, e.run(e.aa, statusReq(expected)), c.want)
			e.checkUnchanged(c.name, before)
		})
	}
}

// TestAdminInspect (plan S4, L2; SPEC ADM-4; DESIGN §A4.6): as rl_admin, one key's
// state: absent; equal to an acquire's reply and the stored bucket; three markers' expiries
// ascending; present 2 for a wrong-type k and n 0 for a wrong-type mi; ” for a bucket
// field, ix score or expiry that fails validation; the quarantine flag; the record and
// KEYS refusals; a wrong-type ix or q is a refusal, not a hold; the store never changes.
func TestAdminInspect(t *testing.T) {
	p := testPolicy()
	const (
		e1, e2, e3 = hwm0 + 10_000_000, hwm0 + 30_000_000, hwm0 + 59_000_000 // marker expiries, ≤ x
		credit     = hwm0 - 500
	)
	none := rep(40, 0, "", 0, "", "", "", "", "", "", "", "", 0)
	bucket := func(vals ...any) []any { // the base bucket's reply, then vals from the ix score on
		return append(rep(40, 0, "", 1, bucketU, bucketT, gen0, bucketS, bucketX, 0), rep(vals...)...)
	}
	cases := []struct {
		name    string
		setup   func(e *env)
		req     func(r *store.Request)
		damaged []string
		want    []any
	}{
		{name: "absent key", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
			e.do("HSET", keys.EpochKey, "nb", "0")
		}, want: none},
		{name: "a bucket", setup: func(*env) {}, want: bucket(bucketX, "", 0)},
		{name: "three markers, expiries ascending", setup: func(e *env) {
			e.do("HSET", kKey(encA), "s", "5", "mc", "3",
				"m:7:2", num(e2)+":"+num(credit), "m:7:4", num(e3)+":"+num(credit), "m:7:5", num(e1)+":"+num(credit))
			e.do("ZADD", miKey(encA), num(e2), "7:2", num(e3), "7:4", num(e1), "7:5")
			e.do("ZADD", ixKey, num(e1), encA)
			e.do("HSET", keys.EpochKey, "nm", "3")
		}, want: rep(40, 0, "", 1, bucketU, bucketT, gen0, 5, bucketX, 3, e1, "", 3, e1, e2, e3)},
		{name: "k a string", setup: func(e *env) { e.do("DEL", kKey(encA)); e.do("SET", kKey(encA), "x") }, damaged: []string{kKey(encA)},
			want: rep(40, 0, "", 2, "", "", "", "", "", "", bucketX, "", 0)},
		{name: "mi a string", setup: func(e *env) { e.do("SET", miKey(encA), "x") }, damaged: []string{miKey(encA)},
			want: bucket(bucketX, "", 0)},
		{name: "u x", setup: func(e *env) { e.do("HSET", kKey(encA), "u", "x") }, damaged: []string{kKey(encA)},
			want: rep(40, 0, "", 1, "", bucketT, gen0, bucketS, bucketX, 0, bucketX, "", 0)},
		{name: "u -5", setup: func(e *env) { e.do("HSET", kKey(encA), "u", "-5") }, damaged: []string{kKey(encA)},
			want: rep(40, 0, "", 1, "", bucketT, gen0, bucketS, bucketX, 0, bucketX, "", 0)},
		{name: "mc missing", setup: func(e *env) { e.do("HDEL", kKey(encA), "mc") }, damaged: []string{kKey(encA)},
			want: rep(40, 0, "", 1, bucketU, bucketT, gen0, bucketS, bucketX, "", bucketX, "", 0)},
		{name: "ix score 1.5", setup: func(e *env) { e.do("ZADD", ixKey, "1.5", encA) }, damaged: []string{kKey(encA)},
			want: bucket("", "", 0)},
		{name: "an mi score inf", setup: func(e *env) {
			e.do("HSET", kKey(encA), "mc", "1", "m:7:1", num(e1)+":"+num(credit))
			e.do("ZADD", miKey(encA), "inf", "7:1")
		}, damaged: []string{miKey(encA)}, want: rep(40, 0, "", 1, bucketU, bucketT, gen0, bucketS, bucketX, 1, bucketX, "", 1, "")},
		{name: "ix a string", setup: func(e *env) { e.do("DEL", ixKey); e.do("SET", ixKey, "x") }, damaged: []string{ixKey},
			want: rep(41, 0, "shared_key_malformed")},
		{name: "q a string", setup: func(e *env) { e.do("SET", qKey, "x") }, damaged: []string{qKey}, want: rep(41, 0, "shared_key_malformed")},
		{name: "quarantined key", setup: func(e *env) { e.do("HSET", qKey, encA, "malformed_bucket|"+num(hwm0)) }, damaged: []string{kKey(encA)},
			want: bucket(bucketX, "malformed_bucket|"+num(hwm0), 0)},
		{name: "wrong k KEYS", setup: func(*env) {}, req: func(r *store.Request) { r.Keys[3] = kKey(encB) }, want: rep(25, 0, "keys_mismatch")},
		{name: "wrong mi KEYS", setup: func(*env) {}, req: func(r *store.Request) { r.Keys[4] = miKey(encB) }, want: rep(25, 0, "keys_mismatch")},
		{name: "wrong ix KEYS", setup: func(*env) {}, req: func(r *store.Request) { r.Keys[1] = keys.IndexKey(otherID) }, want: rep(25, 0, "keys_mismatch")},
		{name: "empty record", setup: func(e *env) { e.do("DEL", keys.EpochKey) }, want: rep(41, 0, "epoch_record_missing")},
		{name: "malformed record", setup: func(e *env) { e.do("HSET", keys.EpochKey, "origin", "01") }, damaged: []string{keys.EpochKey},
			want: rep(41, 0, "epoch_record_malformed")},
		{name: "stale id", setup: func(*env) {}, req: func(r *store.Request) { r.Args[0] = otherID }, want: rep(41, 0, "epoch_changed")},
		// rladmin builds KEYS from the id it read: the record checks come before assert_keys.
		{name: "stale id, KEYS built from it", setup: func(*env) {}, req: func(r *store.Request) {
			r.Args[0] = otherID
			r.Keys = []string{keys.EpochKey, keys.IndexKey(otherID), keys.QuarantineKey(otherID), keys.BucketKey(otherID, encA), keys.MarkerIndexKey(otherID, encA)}
		}, want: rep(41, 0, "epoch_changed")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.base(encA)
			c.setup(e)
			e.h.Damaged(c.damaged...)
			req := inspectReq(epochID, encA)
			if c.req != nil {
				c.req(&req)
			}
			before := e.snap()
			checkReply(t, c.name, e.run(e.aa, req), c.want)
			e.checkUnchanged(c.name, before)
		})
	}

	// After an acquire, inspect shows the fields the acquire returned and the stored bucket.
	t.Run("after an acquire", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, p)
		e.record(healthy(e.fp).with("nb", "0"))
		e.clock(now0)
		got := e.acquire(encA)
		checkReply(t, "acquire", got, rep(1, now0, "", capUnits-ratePeriod, gen0+1, 1, now0+lifetimeUs))
		u, g, s, exp := got[3], got[4], got[5], got[6]
		before := e.snap()
		checkReply(t, "inspect", e.run(e.aa, inspectReq(epochID, encA)), []any{int64(40), int64(0), "", int64(1), u, int64(now0), g, s, exp, int64(0), exp, "", int64(0)})
		k := before[kKey(encA)].Fields
		for f, v := range map[string]any{"u": u, "t": int64(now0), "g": g, "s": s, "x": exp, "mc": int64(0)} {
			if k[f] != num(v.(int64)) {
				t.Errorf("stored %s = %q, want %d as in the acquire reply", f, k[f], v)
			}
		}
		e.checkUnchanged("inspect", before)
	})
}
