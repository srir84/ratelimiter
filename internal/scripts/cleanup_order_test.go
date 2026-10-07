//go:build scripttest

package scripts_test

import (
	"testing"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/storestate"
)

// unattributable is the ix member TestCleanupEntryCheckOrderPairs uses for the is_enc check:
// "YR" is not canonical base64url, so it fails is_enc (§A3.2).
const unattributable = "YR"

// cleanupBreaker breaks one cleanup_entry check (plan S7 Tests (a)), in the §A4.5 order.
// apply breaks the fresh state (through the rl_fixture connection) or the request for the
// step's member; damaged names the keys it breaks; want is the reply it alone causes and
// writes applies the writes it alone causes to a snapshot. member marks the is_enc breaker:
// the step's member is then unattributable, due in ix, with the placeholder KEYS Go passes.
type cleanupBreaker struct {
	name    string
	member  bool
	apply   func(e *env, member string, req *store.Request)
	damaged func(member string) []string
	want    func(member string) []any
	writes  func(st storestate.State, member string)
}

// cleanupBreakers lists one breaker per cleanup_entry check, in check order (§A4.2, §A4.5):
// each load_epoch reason, id/fp, the floors, the clock, ix of the wrong type, gone, not_due, a
// member that fails is_enc, the KEYS, q of the wrong type, quarantined, and a §A3.3 row.
func cleanupBreakers() []cleanupBreaker {
	epoch := func(string) []string { return []string{keys.EpochKey} }
	bucket := func(member string) []string { return []string{kKey(member)} }
	refused := func(code int, reason string) func(string) []any {
		return func(string) []any { return rep(code, 0, reason) }
	}
	held := func(reason string) func(storestate.State, string) {
		return func(st storestate.State, _ string) { setField(st, keys.EpochKey, "hold", reason) }
	}
	pushed := func(reason string) func(storestate.State, string) {
		return func(st storestate.State, member string) {
			if reason != "" {
				setField(st, qKey, member, reason+"|"+num(pairNow))
			}
			setMember(st, ixKey, member, num(pairNow+lifetimeUs))
			setField(st, keys.EpochKey, "hwm", num(pairNow))
		}
	}
	return []cleanupBreaker{
		{name: "epoch_record_missing", apply: func(e *env, _ string, _ *store.Request) { e.do("DEL", keys.EpochKey) },
			want: refused(22, "epoch_record_missing")},
		{name: "epoch_record_malformed", apply: func(e *env, _ string, _ *store.Request) { e.do("HSET", keys.EpochKey, "schema", "2") },
			damaged: epoch, want: refused(22, "epoch_record_malformed")},
		{name: "hwm_missing", apply: func(e *env, _ string, _ *store.Request) { e.do("HDEL", keys.EpochKey, "hwm") },
			damaged: epoch, want: refused(22, "hwm_missing")},
		{name: "hwm_malformed", apply: func(e *env, _ string, _ *store.Request) { e.do("HSET", keys.EpochKey, "hwm", "x") },
			damaged: epoch, want: refused(22, "hwm_malformed")},
		{name: "integrity_hold", apply: func(e *env, _ string, _ *store.Request) { e.do("HSET", keys.EpochKey, "hold", "operator") },
			want: refused(22, "integrity_hold")},
		{name: "epoch_or_fingerprint", apply: func(_ *env, _ string, r *store.Request) { r.Args[cArgEpochID] = otherID },
			want: refused(20, "epoch_or_fingerprint")},
		{name: "store_rollback", apply: func(_ *env, _ string, r *store.Request) { r.Args[cArgFloorGen] = num(gen0 + 1) },
			want: refused(22, "integrity_hold"), writes: held("store_rollback")},
		{name: "clock", apply: func(e *env, _ string, _ *store.Request) { e.do("DEL", keys.CtlTimeKey) },
			want: refused(21, "clock")},
		{name: "ix wrong type", apply: func(e *env, _ string, _ *store.Request) { e.do("DEL", ixKey); e.do("SET", ixKey, "x") },
			damaged: func(string) []string { return []string{ixKey} }, want: refused(22, "integrity_hold"), writes: held("shared_key_malformed")},
		{name: "gone", apply: func(e *env, member string, _ *store.Request) { e.do("ZREM", ixKey, member) },
			damaged: bucket, want: func(string) []any { return rep(33, pairNow, "gone", 0) }},
		{name: "not_due", apply: func(e *env, member string, _ *store.Request) { e.do("ZADD", ixKey, "XX", num(pairNow+1), member) },
			damaged: bucket, want: func(string) []any { return rep(33, pairNow, "not_due", 0) }},
		{name: "unattributable_entry", member: true, apply: func(*env, string, *store.Request) {},
			want: refused(22, "integrity_hold"), writes: held("unattributable_entry")},
		{name: "keys_mismatch", apply: func(_ *env, _ string, r *store.Request) { r.Keys[4] = kKey(encD) },
			want: refused(25, "keys_mismatch")},
		{name: "q wrong type", apply: func(e *env, _ string, _ *store.Request) { e.do("SET", qKey, "x") },
			damaged: func(string) []string { return []string{qKey} }, want: refused(22, "integrity_hold"), writes: held("shared_key_malformed")},
		{name: "quarantined", apply: func(e *env, member string, _ *store.Request) {
			e.do("HSET", qKey, member, "malformed_bucket|"+num(hwm0))
		},
			damaged: bucket, want: func(string) []any { return rep(31, pairNow, "quarantined", 0) }, writes: pushed("")},
		{name: "row 4", apply: func(e *env, member string, _ *store.Request) { e.do("HSET", kKey(member), "zz", "1") },
			damaged: bucket, want: func(string) []any { return rep(32, pairNow, "malformed_bucket", 0) }, writes: pushed("malformed_bucket")},
	}
}

// cleanupCannotCoexist lists the ordered pairs (earlier check, later check) whose breakers
// cannot hold together, with the reason; every other pair runs.
func cleanupCannotCoexist() map[[2]string]string {
	const noRecord = "the record is missing, so none of its fields can be broken"
	readableIx := "the later breaker needs a readable ix, which ix of the wrong type makes a string"
	return map[[2]string]string{
		{"epoch_record_missing", "epoch_record_malformed"}: noRecord,
		{"epoch_record_missing", "hwm_missing"}:            noRecord,
		{"epoch_record_missing", "hwm_malformed"}:          noRecord,
		{"epoch_record_missing", "integrity_hold"}:         noRecord,
		{"hwm_missing", "hwm_malformed"}:                   "hwm cannot be both absent and present",
		{"ix wrong type", "gone"}:                          readableIx,
		{"ix wrong type", "not_due"}:                       readableIx,
		{"gone", "not_due"}:                                "the member cannot be both absent from ix and scored after t",
		{"unattributable_entry", "row 4"}:                  "a §A3.3 row breaks the member's own bucket, which a member failing is_enc cannot name (its k key would lie outside the §A3.2 layout)",
		{"q wrong type", "quarantined"}:                    "q cannot be a string and hold the key's flag",
	}
}

// TestCleanupEntryCheckOrderPairs (plan S7, L2; DESIGN §C3, extends T-ORD-1): each breaker of
// a cleanup_entry check alone gives its reply and writes; then every jointly reachable ordered
// pair, applied together on a fresh store (earlier breaker first), gives the earlier check's
// reply and writes. Pairs that cannot coexist are listed in cleanupCannotCoexist with the
// reason. The fresh state is encA's eligible bucket, due, at pairNow.
func TestCleanupEntryCheckOrderPairs(t *testing.T) {
	p := testPolicy()
	breakers := cleanupBreakers()
	order := map[string]int{}
	for i, b := range breakers {
		order[b.name] = i
	}
	skip := cleanupCannotCoexist()
	for pair, reason := range skip {
		if i, ok := order[pair[0]]; !ok || order[pair[1]] <= i || reason == "" {
			t.Errorf("cleanupCannotCoexist lists %v (%q), which is not an ordered pair of breakers", pair, reason)
		}
	}

	// run applies bs in order on a fresh store and checks the first one's reply and writes.
	run := func(t *testing.T, bs ...cleanupBreaker) {
		t.Parallel()
		e := newEnv(t, p)
		e.record(healthy(e.fp))
		e.clock(pairNow)
		e.fullKey(encA, pastX)
		member := encA
		for _, b := range bs {
			if b.member {
				member = unattributable
				e.do("ZADD", ixKey, num(pastX), member)
				e.h.Damaged(kKey(member)) // exempts the ix member and its q field
			}
		}
		req := cleanupReq(p, e.fp, member)
		for _, b := range bs {
			b.apply(e, member, &req)
			if b.damaged != nil {
				e.h.Damaged(b.damaged(member)...)
			}
		}
		before := e.snap()
		checkReply(t, "cleanup_entry", e.run(e.la, req), bs[0].want(member))
		want := cloneState(before)
		if bs[0].writes != nil {
			bs[0].writes(want, member)
		}
		checkState(t, "cleanup_entry", e.snap(), want)
	}

	t.Run("alone", func(t *testing.T) {
		for _, b := range breakers {
			t.Run(b.name, func(t *testing.T) { run(t, b) })
		}
	})
	pairs := 0
	t.Run("pairs", func(t *testing.T) {
		for i, a := range breakers {
			for _, b := range breakers[i+1:] {
				if _, ok := skip[[2]string{a.name, b.name}]; ok {
					continue
				}
				pairs++
				t.Run(a.name+" then "+b.name, func(t *testing.T) { run(t, a, b) })
			}
		}
	})
	if n := len(breakers); pairs+len(skip) != n*(n-1)/2 {
		t.Errorf("%d pairs ran and %d cannot coexist, want %d in all", pairs, len(skip), n*(n-1)/2)
	}
}
