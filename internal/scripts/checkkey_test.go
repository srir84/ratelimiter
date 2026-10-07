//go:build scripttest

package scripts_test

import (
	"slices"
	"testing"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storestate"
)

// The check_key rows' start state (plan S8 Tests (a)): the healthy record, the controlled time
// now0 and encA's bucket as bucketA writes it, its ix member at ckDue, which is due at now0
// and below the bucket's eligibility (bucketX), so cleanup_entry reaches check_key and the
// state is consistent (§A3.2). ckExp is a marker expiry the rows write, before bucketX.
const (
	ckDue = hwm0 - 2_000_000
	ckExp = hwm0 + 30_000_000
)

// ckRow is one §A3.3 row as a break of encA's start state: the break, the shared keys it
// breaks, and what check_key finds: a quarantine reason, or absent, the hold
// shared_key_malformed, or ok. noIx marks a row that leaves encA no ix member and notDue one
// whose ix score is after now0: cleanup_entry skips both before check_key ('gone', 'not_due').
type ckRow struct {
	name               string
	setup              func(e *env)
	broken             []string
	reason             string
	absent, shared, ok bool
	noIx, notDue       bool
}

// start writes the row's start state for script, and declares Damaged the keys the row
// leaves broken or flagged through it (damaged). For admin_clear it sets encA's q field first,
// as admin_clear needs a flagged key (a row that rewrites q, such as q stored as a string,
// then replaces it).
func (r ckRow) start(e *env, script string) {
	e.t.Helper()
	e.base(encA)
	e.do("ZADD", ixKey, num(ckDue), encA)
	if script == "admin_clear" {
		e.do("HSET", qKey, encA, flag("malformed_bucket"))
	}
	if r.setup != nil {
		r.setup(e)
	}
	e.h.Damaged(r.damaged(script)...)
}

// damaged returns the keys a test of the row through script declares Damaged: exactly those
// the row leaves broken or flagged (AGENTS.md §5). They are the shared key a shared row breaks,
// and encA's k and mi when encA ends broken or flagged: on a per-key row through every script,
// since none repairs the break; on absent through refund, which flags the key
// (refund_bucket_missing); and through admin_clear unless it succeeds or the row replaced q,
// since its refusals and the hold keep the start flag. Row 12 ends consistent and unflagged
// through every script, as does absent through acquire (a new bucket) and cleanup_entry
// (skipped), so the teardown audit checks those states in full; a shared row breaks no key of
// encA's own.
func (r ckRow) damaged(script string) []string {
	perKey := !r.ok && !r.absent && !r.shared
	flagged := (r.absent && script == "refund") ||
		(script == "admin_clear" && !r.ok && !slices.Contains(r.broken, qKey))
	if !perKey && !flagged {
		return r.broken
	}
	return append([]string{kKey(encA), miKey(encA)}, r.broken...)
}

// checkKeyRows are the §A3.3 rows top to bottom, each as one or more breaks of the start state
// (the ix scores inf and 1.5 among them, and ix and q of the wrong type), and row 12, the
// consistent key.
func checkKeyRows() []ckRow {
	mVal := num(ckExp) + ":" + num(mCredit) // a marker of gen0, credited before hwm0
	delK := func(e *env) { e.do("DEL", kKey(encA)) }
	field := func(fv ...string) func(e *env) {
		return func(e *env) { e.do(append([]string{"HSET", kKey(encA)}, fv...)...) }
	}
	ixScore := func(score string) func(e *env) { return func(e *env) { e.do("ZADD", ixKey, score, encA) } }
	return []ckRow{
		{name: "row 1 absent", setup: func(e *env) {
			delK(e)
			e.do("ZREM", ixKey, encA)
			e.do("HSET", keys.EpochKey, "nb", "0")
		}, absent: true, noIx: true},
		{name: "row 2 marker_index_without_bucket, no ix member", setup: func(e *env) {
			delK(e)
			e.do("ZREM", ixKey, encA)
			e.do("ZADD", miKey(encA), num(ckExp), "7:1")
		}, reason: "marker_index_without_bucket", noIx: true},
		{name: "row 2 with the ix member", setup: func(e *env) {
			delK(e)
			e.do("ZADD", miKey(encA), num(ckExp), "7:1")
		}, reason: "marker_index_without_bucket"},
		{name: "row 2 mi a string", setup: func(e *env) {
			delK(e)
			e.do("SET", miKey(encA), "x")
		}, reason: "marker_index_without_bucket"},
		{name: "row 3 index_without_bucket", setup: delK, reason: "index_without_bucket"},
		{name: "row 4 k a string", setup: func(e *env) {
			delK(e)
			e.do("SET", kKey(encA), "x")
		}, reason: "malformed_bucket"},
		{name: "row 4 some but not all bucket fields", setup: func(e *env) { e.do("HDEL", kKey(encA), "x", "mc") }, reason: "malformed_bucket"},
		{name: "row 4 u above cap_units", setup: field("u", num(capUnits+1)), reason: "malformed_bucket"},
		{name: "row 4 g 0", setup: field("g", "0"), reason: "malformed_bucket"},
		{name: "row 4 a non-integer field", setup: field("t", "1.5"), reason: "malformed_bucket"},
		{name: "row 4 a field with a leading zero", setup: field("s", "03"), reason: "malformed_bucket"},
		{name: "row 4 a field outside the layout", setup: field("zz", "1"), reason: "malformed_bucket"},
		{name: "row 5 marker_without_bucket", setup: func(e *env) { e.hset(kKey(encA), "m:7:1", mVal) }, reason: "marker_without_bucket"},
		{name: "row 6 a marker name with bad grammar", setup: field("m:7:x", mVal, "mc", "1"), reason: "malformed_marker"},
		{name: "row 6 a marker value with bad grammar", setup: field("m:7:1", num(ckExp), "mc", "1"), reason: "malformed_marker"},
		{name: "row 6 a marker of another generation", setup: func(e *env) {
			field("m:8:1", mVal, "mc", "1")(e)
			e.do("ZADD", miKey(encA), num(ckExp), "8:1")
		}, reason: "malformed_marker"},
		{name: "row 7 marker_count", setup: field("mc", "1"), reason: "marker_count"},
		{name: "row 8 mi a string", setup: func(e *env) {
			field("m:7:1", mVal, "mc", "1")(e)
			e.do("SET", miKey(encA), "x")
		}, reason: "marker_index_mismatch"},
		{name: "row 8 mi a string, no markers", setup: func(e *env) { e.do("SET", miKey(encA), "x") }, reason: "marker_index_mismatch"},
		{name: "row 8 an mi member not <g>:<s>", setup: func(e *env) {
			field("m:7:1", mVal, "mc", "1")(e)
			e.do("ZADD", miKey(encA), num(ckExp), "7-1")
		}, reason: "marker_index_mismatch"},
		{name: "row 8 a marker without its mi member", setup: field("m:7:1", mVal, "mc", "1"), reason: "marker_index_mismatch"},
		{name: "row 8 an mi member without its marker", setup: func(e *env) { e.do("ZADD", miKey(encA), num(ckExp), "7:1") },
			reason: "marker_index_mismatch"},
		{name: "row 8 an mi score other than the marker's expiry", setup: func(e *env) {
			field("m:7:1", mVal, "mc", "1")(e)
			e.do("ZADD", miKey(encA), num(ckExp+1), "7:1")
		}, reason: "marker_index_mismatch"},
		{name: "row 9 bucket_without_index", setup: func(e *env) { e.do("ZREM", ixKey, encA) }, reason: "bucket_without_index", noIx: true},
		{name: "row 10 ix a string", setup: func(e *env) {
			e.do("DEL", ixKey)
			e.do("SET", ixKey, "x")
		}, broken: []string{ixKey}, shared: true},
		{name: "row 10 q a string", setup: func(e *env) { e.do("SET", qKey, "x") }, broken: []string{qKey}, shared: true},
		{name: "row 11 ix score inf", setup: ixScore("inf"), reason: "malformed_index", notDue: true},
		{name: "row 11 ix score 1.5", setup: ixScore("1.5"), reason: "malformed_index"},
		{name: "row 11 ix score -1", setup: ixScore("-1"), reason: "malformed_index"},
		{name: "row 11 ix score 2^53", setup: ixScore(num(two53)), reason: "malformed_index", notDue: true},
		{name: "row 12 ok", ok: true},
	}
}

// flaggedAt is the state a script that quarantines encA with reason at now0 leaves after
// before (§A4.2 quarantine): q[encA] = reason|now0 and hwm = now0.
func flaggedAt(before storestate.State, reason string) storestate.State {
	want := cloneState(before)
	setField(want, qKey, encA, reason+"|"+num(now0))
	setField(want, keys.EpochKey, "hwm", num(now0))
	return want
}

// expect returns the reply and the state script gives on the row's start state before, worked
// out from §A3.3, §A4.3–§A4.6 and the start state's numbers. Every script, check_key's caller
// or not, meets a shared row with the hold shared_key_malformed, hwm unchanged.
func (r ckRow) expect(t *testing.T, script string, before storestate.State) ([]any, storestate.State) {
	t.Helper()
	if r.shared {
		return rep(22, 0, "integrity_hold"), held(before)
	}
	switch script {
	case "acquire": // §A4.3
		switch {
		case r.ok: // 5 tokens at bucketT, refilled for 1.1 s at 10 tokens/s: 16, less one
			want := cloneState(before)
			for f, v := range map[string]int64{"u": 15_000_000_000, "t": now0, "s": bucketS + 1, "x": now0 + lifetimeUs} {
				setField(want, kKey(encA), f, num(v))
			}
			setMember(want, ixKey, encA, num(now0+lifetimeUs)) // ZADD GT min(x, no marker)
			setField(want, keys.EpochKey, "hwm", num(now0))
			return rep(1, now0, "", 15_000_000_000, gen0, bucketS+1, now0+lifetimeUs), want
		case r.absent: // expected absence: a new bucket
			return rep(1, now0, "", capUnits-ratePeriod, gen0+1, 1, now0+lifetimeUs), allowedNew(before, encA, now0, gen0+1)
		}
		return rep(23, now0, r.reason), flaggedAt(before, r.reason)
	case "refund": // §A4.4, the receipt (gen0, 2) expiring at rExp2
		switch {
		case r.ok: // refilled to 16 tokens, credited 1 token: 17
			return rep(3, now0, "", ratePeriod), applied(t, before, encA, 2, rExp2, now0, 17_000_000_000)
		case r.absent:
			return rep(23, now0, "refund_bucket_missing"), flaggedAt(before, "refund_bucket_missing")
		}
		return rep(23, now0, r.reason), flaggedAt(before, r.reason)
	case "cleanup_entry": // §A4.5
		switch {
		case r.noIx:
			return rep(33, now0, "gone", 0), cloneState(before)
		case r.notDue:
			return rep(33, now0, "not_due", 0), cloneState(before)
		case r.ok: // refilled to 16 tokens, not eligible before bucketX: moved there
			return rep(31, now0, "", 0), movedState(t, before, encA, nil, bucketX, now0)
		}
		return rep(32, now0, r.reason, 0), pushedState(t, before, encA, r.reason, now0)
	case "admin_clear": // §A4.6, on the flagged key
		switch {
		case r.ok:
			return rep(40, now0, "", 0), cleared(t, before, encA, now0)
		case r.absent:
			return rep(41, 0, "bucket_missing"), cloneState(before)
		}
		return rep(41, 0, r.reason), cloneState(before)
	}
	t.Fatalf("no expectation for script %q", script)
	return nil, nil
}

// TestCheckKey_Rows (plan S8, L2; DESIGN §A3.3, §C3; SPEC STA-8, STA-9, STA-10): every §A3.3
// row — the ix scores inf, 1.5, −1 and 2^53 and ix and q of the wrong type among them, and the
// consistent key — through each of check_key's four callers, acquire, refund (the receipt
// (gen0, 2)), cleanup_entry and admin_clear (on the flagged key), as rl_lib and rl_admin, each
// on its own store: a per-key row quarantines the key in the library scripts (cleanup_entry
// also pushes its entry; it skips a key with no ix member or one not due before check_key),
// and admin_clear refuses with the row's reason, writing nothing (bucket_missing for absent);
// a shared row sets the hold. This covers the rows S4–S7 left out for each script (DESIGN §C3).
func TestCheckKey_Rows(t *testing.T) {
	p := testPolicy()
	for _, row := range checkKeyRows() {
		for _, script := range []string{"acquire", "refund", "cleanup_entry", "admin_clear"} {
			t.Run(row.name+"/"+script, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t, p)
				row.start(e, script)
				before := e.snap()
				var got []any
				switch script {
				case "acquire":
					got = e.acquire(encA)
				case "refund":
					got = e.refundOf(encA, 2, rExp2)
				case "cleanup_entry":
					got = e.step(encA)
				case "admin_clear":
					got = e.clear(encA)
				}
				wantReply, wantState := row.expect(t, script, before)
				checkReply(t, script, got, wantReply)
				checkState(t, script, e.snap(), wantState)
			})
		}
	}
}
