//go:build scripttest

package scripts_test

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/srir84/ratelimiter/internal/fingerprint"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/settings"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/testutil/scriptrun"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
	"github.com/srir84/ratelimiter/internal/testutil/storestate"
)

// Receipts of the bucket bucketA writes (generation gen0, sequence 3, x = bucketX): the
// default receipt is (gen0, 2), expiring at rExp2; (gen0, 1) expires at rExp1 and (gen0, 3),
// the latest, at bucketX. mCredit is a stored marker's credit time, before hwm0. All are µs
// at least 10^15 and not round (AGENTS.md §3).
const (
	rExp1   = hwm0 + 39_000_000
	rExp2   = hwm0 + 49_000_000
	mCredit = hwm0 - 5_000_000
)

// Positions in refund's ARGV (§A4.1): epoch_id, fp, enc, receipt epoch_id, g, s, expiry_us,
// cap_units, rate_period_ns, rate_tokens, max_markers, max_markers_per_key, floor_hwm,
// floor_gen.
const (
	rArgEpochID    = 0
	rArgFP         = 1
	rArgREpoch     = 3
	rArgG          = 4
	rArgS          = 5
	rArgExpiry     = 6
	rArgMaxMarkers = 10
	rArgMaxPerKey  = 11
	rArgFloorHwm   = 12
	rArgFloorGen   = 13
)

// refundReq is the refund of the receipt (epochID, g, s, expiry) for enc in epochID with
// fingerprint fp and policy p's limits, floors 0, in the §A4.1 KEYS and ARGV order; it carries
// the receipt's expiry for the decoder's E_ALREADY_APPLIED check (§A4.9).
func refundReq(p settings.Policy, fp, enc string, g, s, expiry int64) store.Request {
	return store.Request{
		Kind:   store.Refund,
		Script: scripts.Refund,
		Keys:   []string{keys.EpochKey, keys.CtlTimeKey, ixKey, qKey, kKey(enc), miKey(enc)},
		Args: []string{
			epochID, fp, enc, epochID, num(g), num(s), num(expiry),
			num(p.Capacity * p.RatePeriodNs), num(p.RatePeriodNs), num(p.RateTokens),
			num(p.MaxLiveMarkers), num(p.MaxMarkersPerKey), "0", "0",
		},
		ReceiptExpiryUs: expiry,
	}
}

// setExpiry sets a refund request's receipt expiry, in ARGV and for the decoder.
func setExpiry(r *store.Request, expiry int64) {
	r.Args[rArgExpiry] = num(expiry)
	r.ReceiptExpiryUs = expiry
}

// refundOf runs, as rl_lib, the refund of the receipt (gen0, s) expiring at exp for enc.
func (e *env) refundOf(enc string, s, exp int64) []any {
	e.t.Helper()
	return e.run(e.la, refundReq(e.p, e.fp, enc, gen0, s, exp))
}

// mustInt parses a stored integer or fails the test.
func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("stored value %q is not an integer: %v", s, err)
	}
	return n
}

// setMember sets a sorted-set member's score in st, creating the set when the key is missing.
func setMember(st storestate.State, key, member, score string) {
	v, ok := st[key]
	if !ok {
		v = storestate.Value{Type: storestate.ZSet, Fields: map[string]string{}}
	}
	v.Fields[member] = score
	st[key] = v
}

// applied is the state an applied refund leaves after before (DESIGN §A4.4) for enc's bucket
// (generation gen0) and the receipt (gen0, s) expiring at exp, decided at budget time at with
// u units after refill and credit: HSET u, t, m:<gen0>:<s> = <exp>:<at> and mc + 1; the mi
// member <gen0>:<s> scored exp; the ix score lowered to exp when it was higher (ZADD LT);
// nm + 1; and hwm = at when later.
func applied(t *testing.T, before storestate.State, enc string, s, exp, at, u int64) storestate.State {
	t.Helper()
	want := cloneState(before)
	gs := num(gen0) + ":" + num(s)
	mc := mustInt(t, want[kKey(enc)].Fields["mc"])
	for f, v := range map[string]string{"u": num(u), "t": num(at), "mc": num(mc + 1), "m:" + gs: num(exp) + ":" + num(at)} {
		setField(want, kKey(enc), f, v)
	}
	setMember(want, miKey(enc), gs, num(exp))
	if exp < mustInt(t, want[ixKey].Fields[enc]) {
		setMember(want, ixKey, enc, num(exp))
	}
	setField(want, keys.EpochKey, "nm", num(mustInt(t, want[keys.EpochKey].Fields["nm"])+1))
	if at > mustInt(t, want[keys.EpochKey].Fields["hwm"]) {
		setField(want, keys.EpochKey, "hwm", num(at))
	}
	return want
}

// addMarker stores, through rl_fixture, the marker of the receipt (gen0, s) for enc with its
// expiry exp and credit time credit, consistently: the field and mc + 1 on k:<enc>, the mi
// member, the ix score lowered to exp when the member exists and is higher (ZADD XX LT, and
// only when ix is a sorted set), and nm + 1. mc and nm are read from the store first, so an
// earlier break of either stays broken.
func (e *env) addMarker(enc string, s, exp, credit int64) {
	e.t.Helper()
	st := e.snap()
	mc := int64(0)
	if v, ok := st[kKey(enc)].Fields["mc"]; ok {
		mc = mustInt(e.t, v)
	}
	gs := num(gen0) + ":" + num(s)
	e.do("HSET", kKey(enc), "m:"+gs, num(exp)+":"+num(credit), "mc", num(mc+1))
	e.do("ZADD", miKey(enc), num(exp), gs)
	if st[ixKey].Type == storestate.ZSet {
		e.do("ZADD", ixKey, "XX", "LT", num(exp), enc)
	}
	e.do("HSET", keys.EpochKey, "nm", num(mustInt(e.t, st[keys.EpochKey].Fields["nm"])+1))
}

// otherKeyMarker adds a second tracked bucket, encB's (nb 2), that holds the epoch's one marker
// (the receipt (gen0, 1) expiring at rExp1, nm 1), while encA's bucket keeps none (mc 0).
func otherKeyMarker(e *env) {
	e.t.Helper()
	e.bucketA(encB)
	e.do("HSET", keys.EpochKey, "nb", "2")
	e.addMarker(encB, 1, rExp1, mCredit)
}

// TestRefundScript_Prelude (plan S6, L2; DESIGN §A4.1, §A4.2): refund's prelude checks in the
// §A4.2 order (API-2a steps 2 and 3), each refusal with its exact reply: the record checks, the
// id and fingerprint, the KEYS assertion right after the epoch check (before the floors), the
// floors, then the clock. Every refused case leaves the store unchanged, except the rollback
// hold, which writes only the hold; a receipt that the budget time has reached is expired
// (step 5) and writes only hwm.
func TestRefundScript_Prelude(t *testing.T) {
	p := testPolicy()
	fp := fingerprint.Of(p)
	base := healthy(fp)
	malformed := rep(22, 0, "epoch_record_malformed")
	clockErr := rep(21, 0, "clock")
	held := rep(22, 0, "integrity_hold")
	cases := []struct {
		name    string
		record  rec                  // nil: no record
		str     bool                 // rl:epoch is a string instead
		ctl     string               // the controlled time; "" = now0, "<del>" = none
		req     func(*store.Request) // request changes
		damaged bool                 // rl:epoch is broken on purpose
		want    []any
		hold    string // the hold the reply sets ("" for none)
		hwm     int64  // the hwm a finished refusal writes (0: none)
		at, u   int64  // an applied refund's budget time (0: refused) and the bucket's units after it
	}{
		{name: "record missing", want: rep(22, 0, "epoch_record_missing")},
		{name: "id upper case", record: base.with("id", "9C56CC51B374C3BA189210D5B6D4BF57"), damaged: true, want: malformed},
		{name: "schema 2", record: base.with("schema", "2"), damaged: true, want: malformed},
		{name: "nm malformed", record: base.with("nm", "1.5"), damaged: true, want: malformed},
		{name: "hold absent", record: base.with("hold", "<del>"), damaged: true, want: malformed},
		{name: "rl:epoch a string", str: true, damaged: true, want: malformed},
		{name: "hwm absent", record: base.with("hwm", "<del>"), damaged: true, want: rep(22, 0, "hwm_missing")},
		{name: "hwm x", record: base.with("hwm", "x"), damaged: true, want: rep(22, 0, "hwm_malformed")},
		{name: "hwm TBOUND", record: base.with("hwm", num(tBound)), damaged: true, want: rep(22, 0, "hwm_malformed")},
		{name: "hold set", record: base.with("hold", "operator"), want: held},
		{name: "wrong id", record: base, req: func(r *store.Request) { r.Args[rArgEpochID] = otherID },
			want: rep(20, 0, "epoch_or_fingerprint")},
		{name: "wrong fp", record: base, req: func(r *store.Request) { r.Args[rArgFP] = fingerprint.Of(storeClockPolicy()) },
			want: rep(20, 0, "epoch_or_fingerprint")},
		{name: "wrong k KEYS, floors above the record", record: base, req: func(r *store.Request) {
			r.Keys[4] = kKey(encB)
			r.Args[rArgFloorHwm], r.Args[rArgFloorGen] = num(hwm0+1), num(gen0+1)
		}, want: rep(25, 0, "keys_mismatch")},
		{name: "wrong mi KEYS", record: base, req: func(r *store.Request) { r.Keys[5] = miKey(encB) }, want: rep(25, 0, "keys_mismatch")},
		{name: "wrong ix KEYS", record: base, req: func(r *store.Request) { r.Keys[2] = keys.IndexKey(otherID) }, want: rep(25, 0, "keys_mismatch")},
		{name: "wrong q KEYS", record: base, req: func(r *store.Request) { r.Keys[3] = keys.QuarantineKey(otherID) }, want: rep(25, 0, "keys_mismatch")},
		{name: "hwm below floor_hwm", record: base, req: func(r *store.Request) { r.Args[rArgFloorHwm] = num(hwm0 + 1) },
			want: held, hold: "store_rollback"},
		{name: "gen below floor_gen", record: base, req: func(r *store.Request) { r.Args[rArgFloorGen] = num(gen0 + 1) },
			want: held, hold: "store_rollback"},
		// refill: 5 tokens + 1.1 s × 10 tokens/s = 16 tokens, plus the credited one (§A3.1).
		{name: "floors equal to the record", record: base, req: func(r *store.Request) {
			r.Args[rArgFloorHwm], r.Args[rArgFloorGen] = num(hwm0), num(gen0)
		}, want: rep(3, now0, "", ratePeriod), at: now0, u: 17_000_000_000},
		{name: "controlled time missing", record: base, ctl: "<del>", want: clockErr},
		{name: "controlled time -5", record: base, ctl: "-5", want: clockErr},
		{name: "controlled time 1.5", record: base, ctl: "1.5", want: clockErr},
		{name: "controlled time TBOUND", record: base, ctl: num(tBound), want: clockErr},
		{name: "controlled time TBOUND - 1", record: base, ctl: num(tBound - 1),
			want: rep(11, tBound-1, "expired"), hwm: tBound - 1},
		// The budget time is the high-water mark: 5 tokens + 1 s × 10 tokens/s, plus one;
		// hwm stays.
		{name: "clock behind the high-water mark", record: base, ctl: num(hwm0 - 1000),
			want: rep(3, hwm0, "", ratePeriod), at: hwm0, u: 16_000_000_000},
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
			e.bucketA(encA)
			switch c.ctl {
			case "":
				e.clock(now0)
			case "<del>":
			default:
				e.do("SET", keys.CtlTimeKey, c.ctl)
			}
			req := refundReq(p, fp, encA, gen0, 2, rExp2)
			if c.req != nil {
				c.req(&req)
			}
			before := e.snap()
			checkReply(t, c.name, e.run(e.la, req), c.want)
			want := cloneState(before)
			switch {
			case c.hold != "":
				setField(want, keys.EpochKey, "hold", c.hold)
			case c.hwm != 0:
				setField(want, keys.EpochKey, "hwm", num(c.hwm))
			case c.at != 0:
				want = applied(t, before, encA, 2, rExp2, c.at, c.u)
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// TestRefundScript_Steps (plan S6, L2; SPEC API-2a steps 4-9, RCP-5, RCP-6, API-16; kills mutant
// PB5): each check with both sides of its boundary. A refusal from step 4 on writes only hwm
// (§A4.8 rule 8); a refund at exactly the receipt's expiry is expired and 1 µs before it is
// applied (PB5); the sequence must lie in 1 … s and the generation equal g; an existing marker
// answers already_applied with its credit time; the per-key cap is checked before the
// epoch-wide one, and with markers on another key the per-key cap counts this key's markers
// (mc) and the epoch-wide cap every key's (nm), not the buckets (nb).
func TestRefundScript_Steps(t *testing.T) {
	p := testPolicy()
	type result int
	const (
		finished result = iota // the reply, and hwm = its t
		applies                // the reply, and applied's state
	)
	cases := []struct {
		name    string
		setup   func(e *env)
		req     func(r *store.Request)
		clock   int64 // the controlled time; 0 = now0
		damaged []string
		want    []any
		res     result
		s, exp  int64 // the receipt an applied case refunds (s 0: the default (gen0, 2), rExp2)
		u       int64 // the applied bucket's units
	}{
		{name: "step 4 a receipt of another epoch", req: func(r *store.Request) { r.Args[rArgREpoch] = otherID },
			want: rep(10, now0, "epoch")},
		// 5 tokens + 1.1 s × 10 tokens/s = 16 tokens, plus one.
		{name: "step 4 a receipt of this epoch", want: rep(3, now0, "", ratePeriod), res: applies, u: 17_000_000_000},
		{name: "step 5 at exactly the expiry", clock: rExp2, want: rep(11, rExp2, "expired")},
		// About 50 s after the bucket's t: full, so the credit is 0.
		{name: "step 5 1 µs before the expiry", clock: rExp2 - 1, want: rep(3, rExp2-1, "", 0), res: applies, u: capUnits},
		{name: "step 5 the high-water mark past the expiry", setup: func(e *env) { e.do("HSET", keys.EpochKey, "hwm", num(rExp2)) },
			want: rep(11, rExp2, "expired")},
		{name: "step 6 quarantined", setup: func(e *env) { e.do("HSET", qKey, encA, "malformed_bucket|"+num(hwm0)) },
			damaged: []string{kKey(encA)}, want: rep(23, now0, "quarantined")},
		{name: "step 7 generation above", req: func(r *store.Request) { r.Args[rArgG] = num(gen0 + 1) },
			want: rep(10, now0, "generation_or_sequence")},
		{name: "step 7 generation below", req: func(r *store.Request) { r.Args[rArgG] = num(gen0 - 1) },
			want: rep(10, now0, "generation_or_sequence")},
		{name: "step 7 sequence 0", req: func(r *store.Request) { r.Args[rArgS] = "0" },
			want: rep(10, now0, "generation_or_sequence")},
		{name: "step 7 sequence above the bucket's", req: func(r *store.Request) { r.Args[rArgS] = num(bucketS + 1) },
			want: rep(10, now0, "generation_or_sequence")},
		{name: "step 7 sequence 1", want: rep(3, now0, "", ratePeriod), res: applies, s: 1, exp: rExp1, u: 17_000_000_000},
		{name: "step 7 sequence equal to the bucket's", want: rep(3, now0, "", ratePeriod), res: applies, s: bucketS, exp: bucketX, u: 17_000_000_000},
		{name: "step 8 the marker exists", setup: func(e *env) { e.addMarker(encA, 2, rExp2, mCredit) },
			want: rep(4, now0, "", mCredit)},
		{name: "step 9 per-key cap reached", setup: func(e *env) { e.addMarker(encA, 1, rExp1, mCredit) },
			req: func(r *store.Request) { r.Args[rArgMaxPerKey] = "1" }, want: rep(24, now0, "key_markers")},
		{name: "step 9 per-key cap one above the markers", setup: func(e *env) { e.addMarker(encA, 1, rExp1, mCredit) },
			req: func(r *store.Request) { r.Args[rArgMaxPerKey] = "2" }, want: rep(3, now0, "", ratePeriod), res: applies, u: 17_000_000_000},
		{name: "step 9 epoch-wide cap reached", setup: func(e *env) { e.addMarker(encA, 1, rExp1, mCredit) },
			req: func(r *store.Request) { r.Args[rArgMaxMarkers] = "1" }, want: rep(24, now0, "live_markers")},
		{name: "step 9 epoch-wide cap one above the markers", setup: func(e *env) { e.addMarker(encA, 1, rExp1, mCredit) },
			req: func(r *store.Request) { r.Args[rArgMaxMarkers] = "2" }, want: rep(3, now0, "", ratePeriod), res: applies, u: 17_000_000_000},
		{name: "step 9 the per-key cap before the epoch-wide cap", setup: func(e *env) { e.addMarker(encA, 1, rExp1, mCredit) },
			req: func(r *store.Request) { r.Args[rArgMaxPerKey], r.Args[rArgMaxMarkers] = "1", "1" }, want: rep(24, now0, "key_markers")},
		// With otherKeyMarker, encA holds no marker (mc 0), the epoch one (nm 1) and two buckets
		// (nb 2): the per-key cap counts mc, the epoch-wide cap nm.
		{name: "step 9 per-key cap 1 with the epoch's marker on another key", setup: otherKeyMarker,
			req: func(r *store.Request) { r.Args[rArgMaxPerKey] = "1" }, want: rep(3, now0, "", ratePeriod), res: applies, u: 17_000_000_000},
		{name: "step 9 epoch-wide cap 1 with the epoch's marker on another key", setup: otherKeyMarker,
			req: func(r *store.Request) { r.Args[rArgMaxMarkers] = "1" }, want: rep(24, now0, "live_markers")},
		{name: "step 9 epoch-wide cap 2 with two buckets and one marker", setup: otherKeyMarker,
			req: func(r *store.Request) { r.Args[rArgMaxMarkers] = "2" }, want: rep(3, now0, "", ratePeriod), res: applies, u: 17_000_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.base(encA)
			at := int64(now0)
			if c.clock != 0 {
				at = c.clock
				e.clock(at)
			}
			if c.setup != nil {
				c.setup(e)
			}
			e.h.Damaged(c.damaged...)
			s, exp := c.s, c.exp
			if s == 0 {
				s, exp = 2, rExp2
			}
			req := refundReq(p, e.fp, encA, gen0, s, exp)
			if c.req != nil {
				c.req(&req)
			}
			before := e.snap()
			checkReply(t, c.name, e.run(e.la, req), c.want)
			var want storestate.State
			switch c.res {
			case finished:
				want = cloneState(before)
				setField(want, keys.EpochKey, "hwm", num(c.want[1].(int64)))
			case applies:
				want = applied(t, before, encA, s, exp, at, c.u)
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// TestRefundScript_CheckKey (plan S6, L2; DESIGN §A3.3, §C3; SPEC STA-9, STA-10, API-2b): one
// case per check_key row reached through refund, plus ix scores inf, 1.5, -1 and 2^53, and q
// and ix stored as strings. A per-key row quarantines the key: {23, t, reason}, q[enc] =
// reason|t and hwm = t; an absent bucket is quarantined refund_bucket_missing and no bucket is
// created. A shared row sets the hold shared_key_malformed with hwm unchanged.
func TestRefundScript_CheckKey(t *testing.T) {
	p := testPolicy()
	const (
		mExp    = hwm0 + 30_000_000 // a marker's expiry
		mVal    = "1760000030123456:1759999995123456"
		storedQ = "malformed_bucket|1760000000123456"
	)
	cases := []struct {
		name    string
		setup   func(e *env)
		damaged []string
		reason  string // the quarantine reason; "" for the cases below
		shared  bool   // the hold shared_key_malformed
	}{
		{name: "row 1 absent: refund_bucket_missing", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
			e.do("HSET", keys.EpochKey, "nb", "0")
		}, damaged: []string{kKey(encA)}, reason: "refund_bucket_missing"},
		{name: "row 2 marker_index_without_bucket", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
			e.do("ZADD", miKey(encA), num(mExp), "7:1")
		}, damaged: []string{miKey(encA)}, reason: "marker_index_without_bucket"},
		{name: "row 2 mi a string", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
			e.do("SET", miKey(encA), "x")
		}, damaged: []string{miKey(encA)}, reason: "marker_index_without_bucket"},
		{name: "row 3 index_without_bucket", setup: func(e *env) { e.do("DEL", kKey(encA)) },
			damaged: []string{kKey(encA)}, reason: "index_without_bucket"},
		{name: "row 4 k a string", setup: func(e *env) { e.do("DEL", kKey(encA)); e.do("SET", kKey(encA), "x") },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 4 some but not all bucket fields", setup: func(e *env) { e.do("HDEL", kKey(encA), "x", "mc") },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 4 u above cap_units", setup: func(e *env) { e.do("HSET", kKey(encA), "u", num(capUnits+1)) },
			damaged: []string{kKey(encA)}, reason: "malformed_bucket"},
		{name: "row 4 g 0", setup: func(e *env) { e.do("HSET", kKey(encA), "g", "0") },
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
		{name: "row 7 marker_count", setup: func(e *env) { e.do("HSET", kKey(encA), "mc", "1") },
			damaged: []string{kKey(encA)}, reason: "marker_count"},
		{name: "row 8 mi a string", setup: func(e *env) {
			e.do("HSET", kKey(encA), "m:7:1", mVal, "mc", "1")
			e.do("SET", miKey(encA), "x")
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.base(encA)
			c.setup(e)
			e.h.Damaged(c.damaged...)
			before := e.snap()
			got := e.refundOf(encA, 2, rExp2)
			want := cloneState(before)
			if c.shared {
				checkReply(t, c.name, got, rep(22, 0, "integrity_hold"))
				setField(want, keys.EpochKey, "hold", "shared_key_malformed")
			} else {
				checkReply(t, c.name, got, rep(23, now0, c.reason))
				setField(want, qKey, encA, c.reason+"|"+num(now0))
				setField(want, keys.EpochKey, "hwm", num(now0))
			}
			checkState(t, c.name, e.snap(), want)
			if _, ok := e.snap()[kKey(encA)]; c.reason == "refund_bucket_missing" && ok {
				t.Errorf("%s: a refund created a bucket (API-2b)", c.name)
			}
		})
	}
	// An already-quarantined key stays as it was apart from hwm (step 6, finish).
	t.Run("already quarantined", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, p)
		e.base(encA)
		e.do("HSET", qKey, encA, storedQ)
		e.h.Damaged(kKey(encA))
		before := e.snap()
		checkReply(t, "already quarantined", e.refundOf(encA, 2, rExp2), rep(23, now0, "quarantined"))
		want := cloneState(before)
		setField(want, keys.EpochKey, "hwm", num(now0))
		checkState(t, "already quarantined", e.snap(), want)
	})
}

// TestRefundScript_Credit (plan S6, L2; DESIGN §A4.4; SPEC API-13, API-14, API-16, BUD-13,
// REF-3, RCP-6; kills mutants PB6, PB7 and PB8): an applied refund writes exactly the §A4.4 set
// and lowers a higher ix score to the marker's expiry (PB8); a repeat answers already_applied
// with the first credit's t and writes only hwm (PB7); a later receipt's credit keeps a lower ix
// score; a repeat at the receipt's expiry is expired, not already_applied (step 5 before step
// 8); the credit is capped at capacity: 0 on a full bucket (PB6), the missing units on a nearly
// full one, computed after the refill.
func TestRefundScript_Credit(t *testing.T) {
	p := testPolicy()
	t.Run("applied, repeated, then expired", func(t *testing.T) {
		e := newEnv(t, p)
		e.base(encA)

		// 5 tokens + 1.1 s × 10 tokens/s = 16 tokens, plus the credited one.
		before := e.snap()
		checkReply(t, "applied", e.refundOf(encA, 2, rExp2), rep(3, now0, "", ratePeriod))
		want := cloneState(before)
		want[kKey(encA)] = bucketValue(17_000_000_000, now0, gen0, bucketS, bucketX, 1, "m:7:2", "1760000049123456:1760000000223456")
		want[miKey(encA)] = storestate.Value{Type: storestate.ZSet, Fields: map[string]string{"7:2": "1760000049123456"}}
		setMember(want, ixKey, encA, "1760000049123456") // lowered from bucketX by ZADD LT (PB8)
		setField(want, keys.EpochKey, "nm", "1")
		setField(want, keys.EpochKey, "hwm", "1760000000223456")
		checkState(t, "applied", e.snap(), want)

		// The same receipt 1 ms later: already applied, with the first credit's t; only hwm moves.
		e.clock(now0 + 1000)
		checkReply(t, "repeat", e.refundOf(encA, 2, rExp2), rep(4, now0+1000, "", now0))
		setField(want, keys.EpochKey, "hwm", num(now0+1000))
		want[keys.CtlTimeKey] = storestate.Value{Type: storestate.String, Str: num(now0 + 1000)}
		checkState(t, "repeat", e.snap(), want)

		// The latest receipt (gen0, 3), expiring at bucketX, 2 ms after the first credit: 2,000 µs ×
		// 1000 × 10 = 2 × 10^7 units refilled, then one token; the ix score stays at the lower
		// rExp2 (ZADD LT).
		e.clock(now0 + 2000)
		checkReply(t, "a later receipt", e.refundOf(encA, 3, bucketX), rep(3, now0+2000, "", ratePeriod))
		want[kKey(encA)] = bucketValue(18_020_000_000, now0+2000, gen0, bucketS, bucketX, 2,
			"m:7:2", "1760000049123456:1760000000223456", "m:7:3", "1760000059123456:1760000000225456")
		setMember(want, miKey(encA), "7:3", "1760000059123456")
		setField(want, keys.EpochKey, "nm", "2")
		setField(want, keys.EpochKey, "hwm", num(now0+2000))
		want[keys.CtlTimeKey] = storestate.Value{Type: storestate.String, Str: num(now0 + 2000)}
		checkState(t, "a later receipt", e.snap(), want)

		// The first receipt again, at its expiry: expired, though its marker exists (API-16).
		e.clock(rExp2)
		checkReply(t, "repeat at expiry", e.refundOf(encA, 2, rExp2), rep(11, rExp2, "expired"))
		setField(want, keys.EpochKey, "hwm", num(rExp2))
		want[keys.CtlTimeKey] = storestate.Value{Type: storestate.String, Str: num(rExp2)}
		checkState(t, "repeat at expiry", e.snap(), want)
	})

	for _, c := range []struct {
		name         string
		u, t         int64 // the bucket's units and last update
		credited, to int64 // the reply's credited units and the units stored
	}{
		// A full bucket stays full: credit 0 (PB6).
		{name: "a full bucket credits 0", u: capUnits, t: now0, credited: 0, to: capUnits},
		// 0.4 token missing: that is the credit.
		{name: "a nearly full bucket credits the missing units", u: 19_600_000_000, t: now0, credited: 400_000_000, to: capUnits},
		// Refill first: 18.5 tokens + 100 ms × 10 tokens/s = 19.5 tokens, so half a token is
		// credited; crediting before the refill would credit a whole token.
		{name: "the credit follows the refill", u: 18_500_000_000, t: now0 - 100_000, credited: 500_000_000, to: capUnits},
		// An empty bucket updated now: one whole token.
		{name: "an empty bucket credits one token", u: 0, t: now0, credited: ratePeriod, to: ratePeriod},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.base(encA)
			e.do("HSET", kKey(encA), "u", num(c.u), "t", num(c.t))
			before := e.snap()
			checkReply(t, c.name, e.refundOf(encA, 2, rExp2), rep(3, now0, "", c.credited))
			checkState(t, c.name, e.snap(), applied(t, before, encA, 2, rExp2, now0, c.to))
		})
	}
}

// TestRefundScript_Concurrent (plan S6, L2; SPEC REF-3, CMP-2): 16 identical refunds sent at
// once on one connection: exactly one applied, fifteen already_applied with its credit time,
// and the store as after one applied refund (nm + 1).
func TestRefundScript_Concurrent(t *testing.T) {
	p := testPolicy()
	e := newEnv(t, p)
	e.base(encA)
	before := e.snap()
	const n = 16
	replies := make([][]any, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := scriptrun.Run(t, e.la, refundReq(p, e.fp, encA, gen0, 2, rExp2))
			if r.Outcome != store.OK {
				t.Errorf("refund %d: outcome %v (store error %q, raw reply %s), want a decoded reply", i, r.Outcome, r.StoreError, r.Raw)
				return
			}
			replies[i] = values(r.Reply.Elems)
		}()
	}
	close(start)
	wg.Wait()
	appliedN, already := 0, 0
	for i, got := range replies {
		switch {
		case got == nil: // reported above
		case reflect.DeepEqual(got, rep(3, now0, "", ratePeriod)):
			appliedN++
		case reflect.DeepEqual(got, rep(4, now0, "", now0)):
			already++
		default:
			t.Errorf("refund %d: reply %v, want applied or already_applied", i, got)
		}
	}
	if appliedN != 1 || already != n-1 {
		t.Errorf("%d applied and %d already_applied, want 1 and %d", appliedN, already, n-1)
	}
	checkState(t, "16 concurrent refunds", e.snap(), applied(t, before, encA, 2, rExp2, now0, 17_000_000_000))
}

// TestRefundScript_OOM (plan S6, L2; DESIGN §A3.0 row 6, §A4.8 rule 1, §C2 bullet 2): with used
// memory above maxmemory under noeviction, the store refuses refund (plain #!lua) before it
// runs: capacity_error with an OOM store error, and the store unchanged. With maxmemory
// restored, the same refund applies; the restore also runs at cleanup, before the audit.
func TestRefundScript_OOM(t *testing.T) {
	p := testPolicy()
	e := newEnv(t, p)
	e.base(encA)

	// About 2 MB under the filler prefix the teardown audit accepts (as in TestOOMRefusal).
	value := strings.Repeat("f", 10_000)
	for i := range 200 {
		e.do("SET", fmt.Sprintf("%s%03d", scriptrun.FillerPrefix, i), value)
	}
	ops := dial(t, e.h, "rl_ops")
	old := mustDo(t, ops, "CONFIG", "GET", "maxmemory")
	if old.Kind != '*' || len(old.Arr) != 2 {
		t.Fatalf("CONFIG GET maxmemory: %s", old)
	}
	restore := func() {
		if _, err := ops.Do("CONFIG", "SET", "maxmemory", old.Arr[1].Str); err != nil {
			t.Errorf("restoring maxmemory: %v", err)
		}
	}
	t.Cleanup(restore) // before the teardown audit, which Store registered earlier
	used, err := strconv.ParseInt(storeproc.ParseInfo(mustDo(t, ops, "INFO", "memory").Str)["used_memory"], 10, 64)
	if err != nil || used < 2_000_000 {
		t.Fatalf("used_memory %d (%v), want at least the 2 MB filler", used, err)
	}
	mustDo(t, ops, "CONFIG", "SET", "maxmemory", strconv.FormatInt(used/2, 10))

	before := e.snap()
	r := scriptrun.Run(t, e.la, refundReq(p, e.fp, encA, gen0, 2, rExp2))
	if r.Outcome != store.CapacityError || !strings.HasPrefix(r.StoreError, "OOM") {
		t.Errorf("refund under OOM: outcome %v, store error %q; want capacity_error with an OOM store error", r.Outcome, r.StoreError)
	}
	e.checkUnchanged("refund under OOM", before)

	restore()
	checkReply(t, "refund after maxmemory is restored", e.refundOf(encA, 2, rExp2), rep(3, now0, "", ratePeriod))
	checkState(t, "refund after maxmemory is restored", e.snap(), applied(t, before, encA, 2, rExp2, now0, 17_000_000_000))
}

// refundOrderBreakers lists one breaker per refund check, in DESIGN order (plan S6 Tests (a)):
// the §A4.2 prelude (each load_epoch reason, id/fp, KEYS, floors, clock), then §A4.4: the
// receipt's epoch, its expiry, q of the wrong type, the quarantine flag, each check_key row
// (rows 2 to 11, as TestAcquireCheckOrderPairs breaks them; row 10 is ix of the wrong type), the
// absent bucket, the generation, the existing marker, the per-key cap and the epoch-wide cap.
func refundOrderBreakers() []orderBreaker {
	epoch := func(string) []string { return []string{keys.EpochKey} }
	bucket := func(enc string) []string { return []string{kKey(enc)} }
	refused := func(code int, reason string) func(string) []any {
		return func(string) []any { return rep(code, 0, reason) }
	}
	// finished is a refusal from step 4 on: the reply at pairNow, and hwm = pairNow.
	finished := func(code int, reason string, vals ...any) (func(string) []any, func(storestate.State, string)) {
		return func(string) []any { return rep(append([]any{code, pairNow, reason}, vals...)...) },
			func(st storestate.State, _ string) { setField(st, keys.EpochKey, "hwm", num(pairNow)) }
	}
	held := func(reason string) func(storestate.State, string) {
		return func(st storestate.State, _ string) { setField(st, keys.EpochKey, "hold", reason) }
	}
	step := func(name string, code int, reason string, apply func(e *env, enc string, r *store.Request), vals ...any) orderBreaker {
		want, writes := finished(code, reason, vals...)
		return orderBreaker{name: name, apply: apply, want: want, writes: writes}
	}
	bs := []orderBreaker{
		// Applied last, so a marker breaker updates the record before it is deleted.
		{name: "epoch_record_missing", last: true, apply: func(e *env, _ string, _ *store.Request) { e.do("DEL", keys.EpochKey) },
			want: refused(22, "epoch_record_missing")},
		{name: "epoch_record_malformed", apply: func(e *env, _ string, _ *store.Request) { e.do("HSET", keys.EpochKey, "schema", "2") },
			damaged: epoch, want: refused(22, "epoch_record_malformed")},
		{name: "hwm_missing", apply: func(e *env, _ string, _ *store.Request) { e.do("HDEL", keys.EpochKey, "hwm") },
			damaged: epoch, want: refused(22, "hwm_missing")},
		{name: "hwm_malformed", apply: func(e *env, _ string, _ *store.Request) { e.do("HSET", keys.EpochKey, "hwm", "x") },
			damaged: epoch, want: refused(22, "hwm_malformed")},
		{name: "integrity_hold", apply: func(e *env, _ string, _ *store.Request) { e.do("HSET", keys.EpochKey, "hold", "operator") },
			want: refused(22, "integrity_hold")},
		{name: "epoch_or_fingerprint", apply: func(_ *env, _ string, r *store.Request) { r.Args[rArgEpochID] = otherID },
			want: refused(20, "epoch_or_fingerprint")},
		{name: "keys_mismatch", apply: func(_ *env, _ string, r *store.Request) { r.Keys[4] = kKey(encD) },
			want: refused(25, "keys_mismatch")},
		{name: "store_rollback", apply: func(_ *env, _ string, r *store.Request) { r.Args[rArgFloorGen] = num(gen0 + 1) },
			want: refused(22, "integrity_hold"), writes: held("store_rollback")},
		{name: "clock", apply: func(e *env, _ string, _ *store.Request) { e.do("DEL", keys.CtlTimeKey) },
			want: refused(21, "clock")},
		step("receipt epoch", 10, "epoch", func(_ *env, _ string, r *store.Request) { r.Args[rArgREpoch] = otherID }),
		// The budget time pairNow is the receipt's expiry.
		step("expired", 11, "expired", func(_ *env, _ string, r *store.Request) { setExpiry(r, pairNow) }),
		{name: "q wrong type", apply: func(e *env, _ string, _ *store.Request) { e.do("SET", qKey, "x") },
			damaged: func(string) []string { return []string{qKey} }, want: refused(22, "integrity_hold"), writes: held("shared_key_malformed")},
	}
	quarantined := step("quarantined", 23, "quarantined", func(e *env, enc string, _ *store.Request) {
		e.do("HSET", qKey, enc, "malformed_bucket|"+num(hwm0))
	})
	quarantined.damaged = bucket
	bs = append(bs, quarantined)
	for _, b := range orderBreakers() { // check_key's rows, broken as for acquire
		if strings.HasPrefix(b.name, "row ") {
			bs = append(bs, b)
		}
	}
	return append(bs,
		orderBreaker{name: "refund_bucket_missing", absent: true, apply: func(*env, string, *store.Request) {},
			damaged: bucket,
			want:    func(string) []any { return rep(23, pairNow, "refund_bucket_missing") },
			writes: func(st storestate.State, enc string) {
				setField(st, qKey, enc, "refund_bucket_missing|"+num(pairNow))
				setField(st, keys.EpochKey, "hwm", num(pairNow))
			}},
		step("generation_or_sequence", 10, "generation_or_sequence", func(_ *env, _ string, r *store.Request) { r.Args[rArgG] = num(gen0 + 1) }),
		step("already_applied", 4, "", func(e *env, enc string, _ *store.Request) { e.addMarker(enc, 2, rExp2, mCredit) }, mCredit),
		step("key_markers", 24, "key_markers", func(e *env, enc string, r *store.Request) {
			e.addMarker(enc, 1, rExp1, mCredit)
			r.Args[rArgMaxPerKey] = "1"
		}),
		// Another marker (sequence 3), so the per-key cap's marker and this one compose.
		step("live_markers", 24, "live_markers", func(e *env, enc string, r *store.Request) {
			e.addMarker(enc, 3, bucketX, mCredit)
			r.Args[rArgMaxMarkers] = "1"
		}),
	)
}

// refundCannotCoexist lists the ordered pairs (earlier check, later check) of refund's breakers
// whose states cannot hold together, with the reason; every other pair runs.
func refundCannotCoexist() map[[2]string]string {
	const (
		noRecord     = "the record is missing, so none of its fields can be broken"
		nmInRecord   = "nm lives in rl:epoch: a missing record holds no marker count to reach the epoch-wide cap"
		absentTarget = "refund_bucket_missing needs an absent key, which the row's state rules out"
		needBucket   = "the later check needs the target's bucket, which the earlier breaker rules out (k absent, or no bucket field)"
	)
	m := map[[2]string]string{
		{"epoch_record_missing", "epoch_record_malformed"}: noRecord,
		{"epoch_record_missing", "hwm_missing"}:            noRecord,
		{"epoch_record_missing", "hwm_malformed"}:          noRecord,
		{"epoch_record_missing", "integrity_hold"}:         noRecord,
		{"hwm_missing", "hwm_malformed"}:                   "hwm cannot be both absent and present",
		{"q wrong type", "quarantined"}:                    "q cannot be a string and hold the key's flag",
	}
	for pair, reason := range cannotCoexist() { // the pairs of check_key's rows (TestAcquireCheckOrderPairs)
		if strings.HasPrefix(pair[0], "row ") && strings.HasPrefix(pair[1], "row ") {
			m[pair] = reason
		}
	}
	m[[2]string{"epoch_record_missing", "live_markers"}] = nmInRecord
	markers := []string{"already_applied", "key_markers", "live_markers"}
	// Row 10 (ix a string) leaves the absent key absent; every other row puts an entry there.
	for _, r := range []string{"row 2", "row 3", "row 4", "row 5", "row 6", "row 7", "row 8", "row 9", "row 11"} {
		m[[2]string{r, "refund_bucket_missing"}] = absentTarget
	}
	// Rows 2, 3 and 5 leave no bucket fields, and an absent key has none: the generation check
	// and the marker breakers (which write the target's marker and mc) need the bucket.
	for _, earlier := range []string{"row 2", "row 3", "row 5", "refund_bucket_missing"} {
		for _, later := range append([]string{"generation_or_sequence"}, markers...) {
			m[[2]string{earlier, later}] = needBucket
		}
	}
	return m
}

// TestRefundCheckOrderPairs (plan S6, L2; DESIGN §C3, extends T-ORD-1): each refund breaker
// alone gives its reply and writes; then every jointly reachable ordered pair, applied together
// on a fresh store (earlier breaker first, except that epoch_record_missing deletes the record
// last), gives the earlier check's reply and writes. Pairs that cannot coexist are listed in
// refundCannotCoexist with the reason.
func TestRefundCheckOrderPairs(t *testing.T) {
	p := testPolicy()
	breakers := refundOrderBreakers()
	order := map[string]int{}
	for i, b := range breakers {
		if _, dup := order[b.name]; dup {
			t.Fatalf("two breakers named %q", b.name)
		}
		order[b.name] = i
	}
	if len(breakers) != 28 {
		t.Fatalf("%d breakers, want 28: 9 in the prelude, 4 before check_key, 10 rows, then 5", len(breakers))
	}
	skip := refundCannotCoexist()
	for pair, reason := range skip {
		if i, ok := order[pair[0]]; !ok || order[pair[1]] <= i || reason == "" {
			t.Errorf("refundCannotCoexist lists %v (%q), which is not an ordered pair of breakers", pair, reason)
		}
	}

	// run applies bs in order (a breaker marked last after the other) on a fresh store and
	// checks the first one's reply and writes.
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
		req := refundReq(p, e.fp, enc, gen0, 2, rExp2)
		for _, last := range []bool{false, true} { // a breaker marked last writes its state after the other's
			for _, b := range bs {
				if b.last != last {
					continue
				}
				b.apply(e, enc, &req)
				if b.damaged != nil {
					e.h.Damaged(b.damaged(enc)...)
				}
			}
		}
		before := e.snap()
		checkReply(t, "refund", e.run(e.la, req), bs[0].want(enc))
		want := cloneState(before)
		if bs[0].writes != nil {
			bs[0].writes(want, enc)
		}
		checkState(t, "refund", e.snap(), want)
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
