//go:build scripttest

package scripts_test

import (
	"strconv"
	"testing"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/storestate"
)

// bucketValue is a bucket hash as the store holds it: u, t, g, s, x, mc and the given
// marker field/value pairs.
func bucketValue(u, t, g, s, x, mc int64, markers ...string) storestate.Value {
	f := map[string]string{"u": num(u), "t": num(t), "g": num(g), "s": num(s), "x": num(x), "mc": num(mc)}
	for i := 0; i+1 < len(markers); i += 2 {
		f[markers[i]] = markers[i+1]
	}
	return storestate.Value{Type: storestate.Hash, Fields: f}
}

// allowedNew is the state an allowed acquire of a new key enc leaves after before, at
// budget time t with generation g (DESIGN §A4.3): gen and nb up by one, the full bucket
// less one token, its ix member at t + lifetime, and hwm = t.
func allowedNew(before storestate.State, enc string, t, g int64) storestate.State {
	want := cloneState(before)
	ep := want[keys.EpochKey]
	ep.Fields["gen"] = num(g)
	nb, _ := strconv.ParseInt(ep.Fields["nb"], 10, 64)
	ep.Fields["nb"] = num(nb + 1)
	ep.Fields["hwm"] = num(t)
	want[kKey(enc)] = bucketValue(capUnits-ratePeriod, t, g, 1, t+lifetimeUs, 0)
	ix, ok := want[ixKey]
	if !ok {
		ix = storestate.Value{Type: storestate.ZSet, Fields: map[string]string{}}
	}
	ix.Fields[enc] = num(t + lifetimeUs)
	want[ixKey] = ix
	return want
}

// TestAcquirePrelude (plan S4, L2; kills mutant PB3): acquire's prelude checks in the
// §A4.2 order, each refusal with its exact reply. Every refused case but the rollback hold
// leaves the store unchanged; the rollback hold writes only the hold.
func TestAcquirePrelude(t *testing.T) {
	p := testPolicy()
	fp := fingerprint.Of(p)
	base := healthy(fp).with("nb", "0") // no bucket yet
	malformed := rep(22, 0, "epoch_record_malformed")
	clockErr := rep(21, 0, "clock")
	held := rep(22, 0, "integrity_hold")
	cases := []struct {
		name    string
		record  rec                  // nil: no record
		str     bool                 // rl:epoch is a string instead
		ctl     string               // the controlled time; "<del>": none
		req     func(*store.Request) // request changes
		damaged bool                 // rl:epoch is broken on purpose
		want    []any                // the reply
		hold    string               // the hold the reply sets ("" for none)
		allowed int64                // the budget time of an allowed new bucket (0: refused)
	}{
		{name: "record missing", want: rep(22, 0, "epoch_record_missing")},
		{name: "id upper case", record: base.with("id", "9C56CC51B374C3BA189210D5B6D4BF57"), damaged: true, want: malformed},
		{name: "fp malformed", record: base.with("fp", "xyz"), damaged: true, want: malformed},
		{name: "schema 2", record: base.with("schema", "2"), damaged: true, want: malformed},
		{name: "clock malformed", record: base.with("clock", "wall"), damaged: true, want: malformed},
		{name: "gen malformed", record: base.with("gen", "x"), damaged: true, want: malformed},
		{name: "nb malformed", record: base.with("nb", "-1"), damaged: true, want: malformed},
		{name: "nm malformed", record: base.with("nm", "1.5"), damaged: true, want: malformed},
		{name: "origin malformed", record: base.with("origin", ""), damaged: true, want: malformed},
		{name: "created 2^53", record: base.with("created", num(two53)), damaged: true, want: malformed},
		{name: "hold absent", record: base.with("hold", "<del>"), damaged: true, want: malformed},
		{name: "rl:epoch a string", str: true, damaged: true, want: malformed},
		{name: "hwm absent", record: base.with("hwm", "<del>"), damaged: true, want: rep(22, 0, "hwm_missing")},
		{name: "hwm x", record: base.with("hwm", "x"), damaged: true, want: rep(22, 0, "hwm_malformed")},
		{name: "hwm TBOUND", record: base.with("hwm", num(tBound)), damaged: true, want: rep(22, 0, "hwm_malformed")},
		{name: "hold set", record: base.with("hold", "operator"), want: held},
		{name: "wrong id", record: base, req: func(r *store.Request) { r.Args[argEpochID] = otherID },
			want: rep(20, 0, "epoch_or_fingerprint")},
		{name: "wrong fp", record: base, req: func(r *store.Request) { r.Args[argFP] = fingerprint.Of(storeClockPolicy()) },
			want: rep(20, 0, "epoch_or_fingerprint")},
		{name: "wrong k KEYS, floors above the record", record: base, req: func(r *store.Request) {
			r.Keys[4] = kKey(encB)
			r.Args[argFloorHwm], r.Args[argFloorGen] = num(hwm0+1), num(gen0+1)
		}, want: rep(25, 0, "keys_mismatch")},
		{name: "wrong mi KEYS", record: base, req: func(r *store.Request) { r.Keys[5] = miKey(encB) }, want: rep(25, 0, "keys_mismatch")},
		{name: "wrong ix KEYS", record: base, req: func(r *store.Request) { r.Keys[2] = keys.IndexKey(otherID) }, want: rep(25, 0, "keys_mismatch")},
		{name: "wrong q KEYS", record: base, req: func(r *store.Request) { r.Keys[3] = keys.QuarantineKey(otherID) }, want: rep(25, 0, "keys_mismatch")},
		{name: "hwm below floor_hwm", record: base, req: func(r *store.Request) { r.Args[argFloorHwm] = num(hwm0 + 1) },
			want: held, hold: "store_rollback"},
		{name: "gen below floor_gen", record: base, req: func(r *store.Request) { r.Args[argFloorGen] = num(gen0 + 1) },
			want: held, hold: "store_rollback"},
		{name: "floors equal to the record", record: base, req: func(r *store.Request) {
			r.Args[argFloorHwm], r.Args[argFloorGen] = num(hwm0), num(gen0)
		}, want: rep(1, now0, "", capUnits-ratePeriod, gen0+1, 1, now0+lifetimeUs), allowed: now0},
		{name: "controlled time missing", record: base, ctl: "<del>", want: clockErr},
		{name: "controlled time -5", record: base, ctl: "-5", want: clockErr},
		{name: "controlled time 1.5", record: base, ctl: "1.5", want: clockErr},
		{name: "controlled time TBOUND", record: base, ctl: num(tBound), want: clockErr},
		{name: "controlled time TBOUND - 1", record: base, ctl: num(tBound - 1),
			want: rep(1, tBound-1, "", capUnits-ratePeriod, gen0+1, 1, tBound-1+lifetimeUs), allowed: tBound - 1},
		{name: "clock behind the high-water mark", record: base, ctl: num(hwm0 - 1000),
			want: rep(1, hwm0, "", capUnits-ratePeriod, gen0+1, 1, hwm0+lifetimeUs), allowed: hwm0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			switch {
			case c.str:
				e.do("SET", keys.EpochKey, "x")
			case c.record != nil:
				e.record(c.record)
			}
			if c.damaged {
				e.h.Damaged(keys.EpochKey)
			}
			switch c.ctl {
			case "":
				e.clock(now0)
			case "<del>":
			default:
				e.do("SET", keys.CtlTimeKey, c.ctl)
			}
			req := acquireReq(p, fp, encA)
			if c.req != nil {
				c.req(&req)
			}
			before := e.snap()
			checkReply(t, c.name, e.run(e.la, req), c.want)
			want := cloneState(before)
			switch {
			case c.hold != "":
				setField(want, keys.EpochKey, "hold", c.hold)
			case c.allowed != 0:
				want = allowedNew(before, encA, c.allowed, gen0+1)
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// TestAcquireCheckKey (plan S4, L2; DESIGN §A3.3, §C3): one case per check_key row, plus ix
// scores inf, 1.5, -1 and 2^53, and q and ix stored as strings. A per-key row quarantines
// the key: {23, t, reason}, q[enc] = reason|t and hwm = t. A shared row sets the hold
// shared_key_malformed with hwm unchanged; an already-quarantined key gets {23, t,
// 'quarantined'} and the store stays as it was. Row 12 allows: u, t, s and x change and the
// ix score moves, while g, mc and any markers stay.
func TestAcquireCheckKey(t *testing.T) {
	p := testPolicy()
	const (
		mExp    = hwm0 + 30_000_000  // a marker's expiry
		mCredit = "1760000000000000" // a marker's credit time, before hwm0
		mVal    = "1760000030123456:" + mCredit
		e1      = hwm0 + 10_000_000 // the earliest of row 12's three marker expiries; mExp and bucketX are the others
	)
	cases := []struct {
		name    string
		setup   func(e *env)
		damaged []string
		reason  string // the quarantine reason; "" for the cases below
		shared  bool   // the hold shared_key_malformed
		quar    bool   // already quarantined
		ok      bool   // row 12: allowed, leaving u = okU and the ix score okIx; the reply carries generation okG
		okU     int64
		okIx    int64
		okG     int64
	}{
		{name: "row 1 absent: a new bucket", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
			e.do("HSET", keys.EpochKey, "nb", "0")
		}},
		{name: "row 2 marker_index_without_bucket", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
			e.do("ZADD", miKey(encA), num(mExp), "7:1")
		}, damaged: []string{miKey(encA)}, reason: "marker_index_without_bucket"},
		{name: "row 2 with the ix member still there", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZADD", miKey(encA), num(mExp), "7:1")
		}, damaged: []string{miKey(encA)}, reason: "marker_index_without_bucket"},
		// A wrong-type mi exists too, and row 2 needs no ix read, so it wins over a wrong-type ix.
		{name: "row 2 mi a string", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
			e.do("SET", miKey(encA), "x")
		}, damaged: []string{miKey(encA)}, reason: "marker_index_without_bucket"},
		{name: "row 2 mi a string, ix a string too", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("DEL", ixKey)
			e.do("SET", ixKey, "x")
			e.do("SET", miKey(encA), "x")
		}, damaged: []string{miKey(encA), ixKey}, reason: "marker_index_without_bucket"},
		{name: "row 3 index_without_bucket", setup: func(e *env) { e.do("DEL", kKey(encA)) },
			damaged: []string{kKey(encA)}, reason: "index_without_bucket"},
		{name: "row 4 k a string", setup: func(e *env) { e.do("DEL", kKey(encA)); e.do("SET", kKey(encA), "x") },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 4 some but not all bucket fields", setup: func(e *env) { e.do("HDEL", kKey(encA), "x", "mc") },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 4 u above cap_units", setup: func(e *env) { e.do("HSET", kKey(encA), "u", num(capUnits+1)) },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		// g < 1 is out of range too (owner decision at S4's §5 stop): quarantined, never charged.
		{name: "row 4 g 0, below 1", setup: func(e *env) { e.do("HSET", kKey(encA), "g", "0") },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 4 a non-integer field", setup: func(e *env) { e.do("HSET", kKey(encA), "t", "1.5") },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 4 a field with a leading zero", setup: func(e *env) { e.do("HSET", kKey(encA), "s", "03") },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 4 a field outside the layout", setup: func(e *env) { e.do("HSET", kKey(encA), "zz", "1") },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 5 marker_without_bucket", setup: func(e *env) { e.hset(kKey(encA), "m:7:1", mVal) },
			damaged: []string{kKey(encA)}, reason: "marker_without_bucket"},
		{name: "row 6 a marker name with bad grammar", setup: func(e *env) { e.do("HSET", kKey(encA), "m:7:x", mVal, "mc", "1") },
			damaged: []string{kKey(encA)}, reason: "malformed_marker"},
		{name: "row 6 a marker value with bad grammar", setup: func(e *env) { e.do("HSET", kKey(encA), "m:7:1", num(mExp), "mc", "1") },
			damaged: []string{kKey(encA)}, reason: "malformed_marker"},
		{name: "row 6 a marker of another generation", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:8:1", mVal, "mc", "1")
			e.do("ZADD", miKey(encA), num(mExp), "8:1")
		}, damaged: []string{kKey(encA)}, reason: "malformed_marker"},
		// One bad part each, a leading zero, the rest of the marker and its mi member 7:1 sound.
		{name: "row 6 a marker generation with a leading zero", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:07:1", mVal, "mc", "1")
			e.do("ZADD", miKey(encA), num(mExp), "7:1")
		}, damaged: []string{kKey(encA)}, reason: "malformed_marker"},
		{name: "row 6 a marker sequence with a leading zero", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:7:01", mVal, "mc", "1")
			e.do("ZADD", miKey(encA), num(mExp), "7:1")
		}, damaged: []string{kKey(encA)}, reason: "malformed_marker"},
		{name: "row 6 a marker expiry with a leading zero", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:7:1", "0"+mVal, "mc", "1")
			e.do("ZADD", miKey(encA), num(mExp), "7:1")
		}, damaged: []string{kKey(encA)}, reason: "malformed_marker"},
		{name: "row 6 a marker credit time with a leading zero", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:7:1", num(mExp)+":0"+mCredit, "mc", "1")
			e.do("ZADD", miKey(encA), num(mExp), "7:1")
		}, damaged: []string{kKey(encA)}, reason: "malformed_marker"},
		{name: "row 7 marker_count", setup: func(e *env) { e.do("HSET", kKey(encA), "mc", "1") },
			damaged: []string{kKey(encA)}, reason: "marker_count"},
		{name: "row 8 mi a string", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:7:1", mVal, "mc", "1")
			e.do("SET", miKey(encA), "x")
		}, damaged: []string{miKey(encA)}, reason: "marker_index_mismatch"},
		{name: "row 8 mi a string, no markers", setup: func(e *env) { e.do("SET", miKey(encA), "x") },
			damaged: []string{miKey(encA)}, reason: "marker_index_mismatch"},
		{name: "row 8 an mi member not <g>:<s>", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:7:1", mVal, "mc", "1")
			e.do("ZADD", miKey(encA), num(mExp), "7-1")
		}, damaged: []string{miKey(encA)}, reason: "marker_index_mismatch"},
		{name: "row 8 a marker without its mi member", setup: func(e *env) { e.do("HSET", kKey(encA), "m:7:1", mVal, "mc", "1") },
			damaged: []string{miKey(encA)}, reason: "marker_index_mismatch"},
		{name: "row 8 an mi member without its marker", setup: func(e *env) { e.do("ZADD", miKey(encA), num(mExp), "7:1") },
			damaged: []string{miKey(encA)}, reason: "marker_index_mismatch"},
		{name: "row 8 an mi score other than the marker's expiry", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:7:1", mVal, "mc", "1")
			e.do("ZADD", miKey(encA), num(mExp+1), "7:1")
		}, damaged: []string{miKey(encA)}, reason: "marker_index_mismatch"},
		{name: "row 9 bucket_without_index", setup: func(e *env) { e.do("ZREM", ixKey, encA) },
			damaged: []string{kKey(encA)}, reason: "bucket_without_index"},
		{name: "row 10 ix a string", setup: func(e *env) { e.do("DEL", ixKey); e.do("SET", ixKey, "x") },
			damaged: []string{ixKey}, shared: true},
		{name: "q a string", setup: func(e *env) { e.do("SET", qKey, "x") },
			damaged: []string{qKey}, shared: true},
		{name: "row 11 ix score inf", setup: func(e *env) { e.do("ZADD", ixKey, "inf", encA) },
			damaged: []string{kKey(encA)}, reason: "malformed_index"},
		{name: "row 11 ix score 1.5", setup: func(e *env) { e.do("ZADD", ixKey, "1.5", encA) },
			damaged: []string{kKey(encA)}, reason: "malformed_index"},
		{name: "row 11 ix score -1", setup: func(e *env) { e.do("ZADD", ixKey, "-1", encA) },
			damaged: []string{kKey(encA)}, reason: "malformed_index"},
		{name: "row 11 ix score 2^53", setup: func(e *env) { e.do("ZADD", ixKey, num(two53), encA) },
			damaged: []string{kKey(encA)}, reason: "malformed_index"},
		// refill: 5 tokens + 1.1 s × 10 tokens/s = 16 tokens, less one (§A3.1); x and the ix
		// score move to now0 + lifetime, which is later than the old expiry.
		{name: "row 12 ok", setup: func(*env) {}, ok: true, okU: 15_000_000_000, okIx: now0 + lifetimeUs, okG: gen0},
		// g = 1, the lowest generation acquire issues (a fresh epoch's first key), is in range:
		// the upper edge of row 4's g < 1. Allowed as above, with g unchanged.
		{name: "row 12 ok, g 1", setup: func(e *env) { e.do("HSET", kKey(encA), "g", "1") },
			ok: true, okU: 15_000_000_000, okIx: now0 + lifetimeUs, okG: 1},
		// Three consistent markers (mc 3, nm 3, each an mi member scored with its expiry, all
		// ≤ x), the earliest, e1, neither the first field nor the last, and the ix score 5 s
		// below e1: the markers and mc stay, and ZADD GT raises the ix score to min(x, earliest
		// marker expiry) = e1, not to x (§A4.3).
		{name: "row 12 ok, three markers", setup: func(e *env) {
			e.do("HSET", kKey(encA), "mc", "3",
				"m:7:1", num(mExp)+":"+mCredit, "m:7:2", num(e1)+":"+mCredit, "m:7:3", num(bucketX)+":"+mCredit)
			e.do("ZADD", miKey(encA), num(mExp), "7:1", num(e1), "7:2", num(bucketX), "7:3")
			e.do("ZADD", ixKey, num(e1-5_000_000), encA)
			e.do("HSET", keys.EpochKey, "nm", "3")
		}, ok: true, okU: 15_000_000_000, okIx: e1, okG: gen0},
		// A full bucket, u = cap_units, is in range (row 4 is u > cap_units): refill keeps
		// cap_units, less one token.
		{name: "row 12 ok, a full bucket", setup: func(e *env) { e.do("HSET", kKey(encA), "u", num(capUnits)) },
			ok: true, okU: capUnits - ratePeriod, okIx: now0 + lifetimeUs, okG: gen0},
		{name: "already quarantined", setup: func(e *env) { e.do("HSET", qKey, encA, "malformed_bucket|"+num(hwm0)) },
			damaged: []string{kKey(encA)}, quar: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.base(encA)
			c.setup(e)
			e.h.Damaged(c.damaged...)
			before := e.snap()
			got := e.acquire(encA)
			want := cloneState(before)
			switch {
			case c.reason != "":
				checkReply(t, c.name, got, rep(23, now0, c.reason))
				setField(want, qKey, encA, c.reason+"|"+num(now0))
				setField(want, keys.EpochKey, "hwm", num(now0))
			case c.shared:
				checkReply(t, c.name, got, rep(22, 0, "integrity_hold"))
				setField(want, keys.EpochKey, "hold", "shared_key_malformed")
			case c.quar:
				checkReply(t, c.name, got, rep(23, now0, "quarantined"))
			case c.ok:
				checkReply(t, c.name, got, rep(1, now0, "", c.okU, c.okG, bucketS+1, now0+lifetimeUs))
				for f, v := range map[string]int64{"u": c.okU, "t": now0, "s": bucketS + 1, "x": now0 + lifetimeUs} {
					setField(want, kKey(encA), f, num(v))
				}
				setField(want, ixKey, encA, num(c.okIx))
				setField(want, keys.EpochKey, "hwm", num(now0))
			default: // row 1: a new bucket
				checkReply(t, c.name, got, rep(1, now0, "", capUnits-ratePeriod, gen0+1, 1, now0+lifetimeUs))
				want = allowedNew(before, encA, now0, gen0+1)
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// bigPolicy has capacity 4,503,599 × rate_period_ns 10^9: cap_units just below 2^52, where
// a float computation would lose units (plan S4 Tests (a)).
func bigPolicy() settings.Policy {
	p := testPolicy()
	p.Capacity = 4_503_599
	return p
}

// TestAcquireDecisions (plan S4, L2; kills mutants PB1 and PB2): a new bucket's exact reply
// and writes; 20 acquires at one frozen time, then limited with the store unchanged; a
// limited acquire later than hwm leaves hwm as it was (CLK-4); exactly one token refilled is
// allowed (EX-2); the tracked-bucket cap refuses a new key with the store unchanged; and,
// with cap_units near 2^52, exact units and an ix score that only rises (ZADD GT).
func TestAcquireDecisions(t *testing.T) {
	t.Run("default policy", func(t *testing.T) {
		p := testPolicy()
		e := newEnv(t, p)
		e.record(healthy(e.fp).with("nb", "0"))
		e.clock(now0)

		// A new key: a full bucket less one token, generation gen0 + 1, sequence 1.
		before := e.snap()
		checkReply(t, "new key", e.acquire(encA), rep(1, now0, "", 19_000_000_000, gen0+1, 1, now0+lifetimeUs))
		state := allowedNew(before, encA, now0, gen0+1)
		checkState(t, "new key", e.snap(), state)

		// 19 more at the same instant: s = 2 … 20, one token less each time; t, x, the ix
		// score and hwm stay.
		for s := int64(2); s <= 20; s++ {
			checkReply(t, "frozen time", e.acquire(encA), rep(1, now0, "", (20-s)*ratePeriod, gen0+1, s, now0+lifetimeUs))
		}
		state[kKey(encA)] = bucketValue(0, now0, gen0+1, 20, now0+lifetimeUs, 0)
		checkState(t, "after 20 acquires", e.snap(), state)

		// The 21st is limited, with 0 units, and writes nothing.
		checkReply(t, "21st acquire", e.acquire(encA), rep(2, now0, "", 0))
		checkState(t, "21st acquire", e.snap(), state)

		// 1 ms later: 10^7 units, under one token; limited, and hwm stays at now0 although
		// the budget time is later (CLK-4; PB2).
		e.clock(now0 + 1000)
		state = e.snap()
		checkReply(t, "1 ms later", e.acquire(encA), rep(2, now0+1000, "", 10_000_000))
		checkState(t, "1 ms later", e.snap(), state)

		// 100 ms after the last allow: exactly one token (10^9 units) refilled, so allowed
		// with 0 left (EX-2; PB1).
		e.clock(now0 + 100_000)
		before = e.snap()
		checkReply(t, "100 ms later", e.acquire(encA), rep(1, now0+100_000, "", 0, gen0+1, 21, now0+100_000+lifetimeUs))
		state = cloneState(before)
		state[kKey(encA)] = bucketValue(0, now0+100_000, gen0+1, 21, now0+100_000+lifetimeUs, 0)
		setField(state, ixKey, encA, num(now0+100_000+lifetimeUs))
		setField(state, keys.EpochKey, "hwm", num(now0+100_000))
		checkState(t, "100 ms later", e.snap(), state)

		// nb = max_tracked: a new key is refused and nothing is written.
		req := acquireReq(p, e.fp, encB)
		req.Args[argMaxTracked] = "1"
		checkReply(t, "tracked buckets full", e.run(e.la, req), rep(24, now0+100_000, "tracked_buckets"))
		checkState(t, "tracked buckets full", e.snap(), state)
	})

	t.Run("cap_units near 2^52", func(t *testing.T) {
		p := bigPolicy()
		const bigCap = 4_503_599_000_000_000 // capacity × rate_period_ns, below 2^52
		e := newEnv(t, p)
		e.record(healthy(e.fp).with("nb", "0"))
		e.clock(now0)
		checkReply(t, "new key", e.acquire(encA), rep(1, now0, "", bigCap-ratePeriod, gen0+1, 1, now0+lifetimeUs))

		// 1 µs later: 1 µs × 1000 × 10 = 10^4 units refilled, exactly.
		e.clock(now0 + 1)
		checkReply(t, "1 µs later", e.acquire(encA), rep(1, now0+1, "", bigCap-2*ratePeriod+10_000, gen0+1, 2, now0+1+lifetimeUs))

		// A bucket with 2 tokens, whose eligibility (refilling 4.5 × 10^15 units at 10^4 per µs)
		// is far beyond its ix score now0 + 1000 s, itself beyond the expiry an allow sets:
		// ZADD GT keeps the score (§A4.3).
		e.hset(kKey(encB), "u", "2000000000", "t", num(now0), "g", num(gen0+1), "s", "1", "x", num(now0+lifetimeUs), "mc", "0")
		e.do("ZADD", ixKey, num(now0+1_000_000_000), encB)
		e.do("HSET", keys.EpochKey, "nb", "2")
		e.clock(now0 + 100_001)
		before := e.snap()
		// refill: 100,001 µs × 10^4 = 1,000,010,000 units, so 3,000,010,000 less one token.
		checkReply(t, "ix score only rises", e.acquire(encB), rep(1, now0+100_001, "", 2_000_010_000, gen0+1, 2, now0+100_001+lifetimeUs))
		want := cloneState(before)
		want[kKey(encB)] = bucketValue(2_000_010_000, now0+100_001, gen0+1, 2, now0+100_001+lifetimeUs, 0)
		setField(want, keys.EpochKey, "hwm", num(now0+100_001))
		checkState(t, "ix score only rises", e.snap(), want)
	})
}
