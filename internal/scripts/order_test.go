//go:build scripttest

package scripts_test

import (
	"testing"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/storestate"
)

// pairNow is the controlled time of the check-order test: 50 ms after hwm0, so an empty
// bucket last updated at hwm0 refills half a token and stays limited.
const pairNow = hwm0 + 50_000

// orderBreaker breaks one acquire check (plan S4 Tests (a)), in the §A4.2/§A4.3 order.
// apply breaks the fresh state (through the rl_fixture connection) or the request for
// the target key enc; damaged names the keys it breaks; want is the reply it alone causes
// and writes applies the writes it alone causes to a snapshot.
type orderBreaker struct {
	name    string
	absent  bool // needs the target key absent: the request then targets encC
	last    bool // applied after the other breaker of a pair (TestRefundCheckOrderPairs): its state is written last
	apply   func(e *env, enc string, req *store.Request)
	damaged func(enc string) []string
	want    func(enc string) []any
	writes  func(st storestate.State, enc string)
}

// orderBreakers lists one breaker per acquire check, in check order: each load_epoch
// reason, id/fp, KEYS, floors, clock, q wrong type, quarantined, each check_key row
// (rows 2 to 11; row 10 is ix of the wrong type), tracked capacity and limited.
func orderBreakers() []orderBreaker {
	epoch := func(string) []string { return []string{keys.EpochKey} }
	bucket := func(enc string) []string { return []string{kKey(enc)} }
	index := func(enc string) []string { return []string{miKey(enc)} }
	refused := func(code int, reason string) func(string) []any {
		return func(string) []any { return rep(code, 0, reason) }
	}
	quarantined := func(reason string) (func(string) []any, func(storestate.State, string)) {
		return func(string) []any { return rep(23, pairNow, reason) },
			func(st storestate.State, enc string) {
				setField(st, qKey, enc, reason+"|"+num(pairNow))
				setField(st, keys.EpochKey, "hwm", num(pairNow))
			}
	}
	held := func(reason string) func(storestate.State, string) {
		return func(st storestate.State, _ string) { setField(st, keys.EpochKey, "hold", reason) }
	}
	const marker = "1760000030123456:1760000000000000"
	row := func(name, reason string, damaged func(string) []string, apply func(e *env, enc string)) orderBreaker {
		want, writes := quarantined(reason)
		return orderBreaker{name: name, damaged: damaged, want: want, writes: writes,
			apply: func(e *env, enc string, _ *store.Request) { apply(e, enc) }}
	}
	return []orderBreaker{
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
		{name: "epoch_or_fingerprint", apply: func(_ *env, _ string, r *store.Request) { r.Args[argEpochID] = otherID },
			want: refused(20, "epoch_or_fingerprint")},
		{name: "keys_mismatch", apply: func(_ *env, _ string, r *store.Request) { r.Keys[4] = kKey(encD) },
			want: refused(25, "keys_mismatch")},
		{name: "store_rollback", apply: func(_ *env, _ string, r *store.Request) { r.Args[argFloorGen] = num(gen0 + 1) },
			want: refused(22, "integrity_hold"), writes: held("store_rollback")},
		{name: "clock", apply: func(e *env, _ string, _ *store.Request) { e.do("DEL", keys.CtlTimeKey) },
			want: refused(21, "clock")},
		{name: "q wrong type", apply: func(e *env, _ string, _ *store.Request) { e.do("SET", qKey, "x") },
			damaged: func(string) []string { return []string{qKey} }, want: refused(22, "integrity_hold"), writes: held("shared_key_malformed")},
		{name: "quarantined", apply: func(e *env, enc string, _ *store.Request) { e.do("HSET", qKey, enc, "malformed_bucket|"+num(hwm0)) },
			damaged: bucket, want: func(string) []any { return rep(23, pairNow, "quarantined") }},
		row("row 2", "marker_index_without_bucket", index, func(e *env, enc string) {
			e.do("DEL", kKey(enc))
			e.do("ZADD", miKey(enc), num(hwm0+30_000_000), "7:1")
		}),
		row("row 3", "index_without_bucket", bucket, func(e *env, enc string) { e.do("DEL", kKey(enc)) }),
		// Rows 4 and 5 compose: row 4 adds a field outside the layout (plan S4 step 2), and row
		// 5 removes the six bucket fields and adds a marker, leaving any other field.
		row("row 4", "malformed_bucket", bucket, func(e *env, enc string) { e.do("HSET", kKey(enc), "zz", "1") }),
		row("row 5", "marker_without_bucket", bucket, func(e *env, enc string) {
			e.do("HDEL", kKey(enc), "u", "t", "g", "s", "x", "mc")
			e.do("HSET", kKey(enc), "m:7:1", marker)
		}),
		row("row 6", "malformed_marker", bucket, func(e *env, enc string) { e.do("HSET", kKey(enc), "m:7:x", marker) }),
		row("row 7", "marker_count", bucket, func(e *env, enc string) { e.do("HSET", kKey(enc), "mc", "5") }),
		row("row 8", "marker_index_mismatch", index, func(e *env, enc string) { e.do("ZADD", miKey(enc), num(hwm0+30_000_000), "7:9") }),
		row("row 9", "bucket_without_index", bucket, func(e *env, enc string) { e.do("ZREM", ixKey, enc) }),
		{name: "row 10", apply: func(e *env, _ string, _ *store.Request) { e.do("DEL", ixKey); e.do("SET", ixKey, "x") },
			damaged: func(string) []string { return []string{ixKey} }, want: refused(22, "integrity_hold"), writes: held("shared_key_malformed")},
		row("row 11", "malformed_index", bucket, func(e *env, enc string) { e.do("ZADD", ixKey, "1.5", enc) }),
		{name: "tracked_buckets", absent: true, apply: func(_ *env, _ string, r *store.Request) { r.Args[argMaxTracked] = "1" },
			want: func(string) []any { return rep(24, pairNow, "tracked_buckets") }},
		{name: "limited", apply: func(e *env, enc string, _ *store.Request) { e.do("HSET", kKey(enc), "u", "0", "t", num(hwm0)) },
			// 50 ms × 10 tokens/s = 5 × 10^8 units, under one token (§A3.1).
			want: func(string) []any { return rep(2, pairNow, "", 500_000_000) }},
	}
}

// cannotCoexist lists the ordered pairs (earlier check, later check) whose breakers cannot
// hold together, with the reason; every other pair runs.
func cannotCoexist() map[[2]string]string {
	const (
		noRecord   = "the record is missing, so none of its fields can be broken"
		kAbsent    = "the earlier row needs k absent, the later one a present k"
		absentKey  = "tracked_buckets needs an absent key, which the row's state rules out"
		needBucket = "limited needs the six bucket fields, which the row rules out (k absent, or no bucket field)"
		readableIx = "the row needs a readable ix, which row 10 makes a string"
	)
	m := map[[2]string]string{
		{"epoch_record_missing", "epoch_record_malformed"}: noRecord,
		{"epoch_record_missing", "hwm_missing"}:            noRecord,
		{"epoch_record_missing", "hwm_malformed"}:          noRecord,
		{"epoch_record_missing", "integrity_hold"}:         noRecord,
		{"hwm_missing", "hwm_malformed"}:                   "hwm cannot be both absent and present",
		{"q wrong type", "quarantined"}:                    "q cannot be a string and hold the key's flag",
		{"row 3", "row 10"}:                                readableIx,
		{"row 9", "row 10"}:                                readableIx,
		{"row 10", "row 11"}:                               readableIx,
		{"row 3", "row 8"}:                                 "row 8 needs an mi, and with k absent an mi makes row 2 match before row 3",
		{"row 5", "row 7"}:                                 "row 7 sets mc, a bucket field, which row 5 rules out",
		{"row 9", "row 11"}:                                "row 9 removes the ix member whose score row 11 breaks",
		{"tracked_buckets", "limited"}:                     "tracked_buckets needs an absent key, limited a bucket",
	}
	// Rows 8 and 11 need no k: (row 2, row 8), (row 2, row 11) and (row 3, row 11) run.
	for _, later := range []string{"row 4", "row 5", "row 6", "row 7", "row 9"} {
		m[[2]string{"row 2", later}] = kAbsent
		m[[2]string{"row 3", later}] = kAbsent
	}
	for _, r := range []string{"row 2", "row 3", "row 4", "row 5", "row 6", "row 7", "row 8", "row 9", "row 11"} {
		m[[2]string{r, "tracked_buckets"}] = absentKey
	}
	// The other rows leave the six bucket fields valid, so limited's u and t hold beside them.
	for _, r := range []string{"row 2", "row 3", "row 5"} {
		m[[2]string{r, "limited"}] = needBucket
	}
	return m
}

// TestAcquireCheckOrderPairs (plan S4, L2; DESIGN §C3, extends T-ORD-1): each breaker
// alone gives its reply and writes; then every jointly reachable ordered pair, applied
// together on a fresh store (earlier breaker first), gives the earlier check's reply and
// writes. Pairs that cannot coexist are listed in cannotCoexist with the reason.
func TestAcquireCheckOrderPairs(t *testing.T) {
	p := testPolicy()
	breakers := orderBreakers()
	order := map[string]int{}
	for i, b := range breakers {
		order[b.name] = i
	}
	skip := cannotCoexist()
	for pair, reason := range skip {
		if i, ok := order[pair[0]]; !ok || order[pair[1]] <= i || reason == "" {
			t.Errorf("cannotCoexist lists %v (%q), which is not an ordered pair of breakers", pair, reason)
		}
	}

	// run applies bs in order on a fresh store and checks the first one's reply and writes.
	run := func(t *testing.T, bs ...orderBreaker) {
		t.Parallel()
		e := newEnv(t, p)
		e.base(encA)
		e.clock(pairNow)
		enc := encA
		for _, b := range bs {
			if b.absent {
				enc = encC
			}
		}
		req := acquireReq(p, e.fp, enc)
		for _, b := range bs {
			b.apply(e, enc, &req)
			if b.damaged != nil {
				e.h.Damaged(b.damaged(enc)...)
			}
		}
		before := e.snap()
		checkReply(t, "acquire", e.run(e.la, req), bs[0].want(enc))
		want := cloneState(before)
		if bs[0].writes != nil {
			bs[0].writes(want, enc)
		}
		checkState(t, "acquire", e.snap(), want)
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
