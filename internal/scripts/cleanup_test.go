//go:build scripttest

package scripts_test

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/scriptrun"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/internal/testutil/storestate"
)

// The cleanup tests' times (IMPLEMENTATION_PLAN S7 Tests (a)), µs at least 10^15 and not
// round (AGENTS.md §3). Steps run at now0, 100 ms after hwm0. A bucket last updated at oldT
// with no units left is full again 2 s later (capacity 20 tokens at 10 tokens/s), long before
// now0; pastX is a latest receipt expiry already passed at now0.
const (
	oldT    = hwm0 - 20_000_000
	pastX   = hwm0 - 10_000_000
	unitsUs = rateTokens * 1000 // units refilled per µs (§A3.1)
)

// Positions in cleanup_entry's ARGV (§A4.1): epoch_id, fp, member, cap_units, rate_tokens,
// push_us, floor_hwm, floor_gen.
const (
	cArgEpochID  = 0
	cArgFP       = 1
	cArgFloorHwm = 6
	cArgFloorGen = 7
)

// Positions in due_entries' ARGV (§A4.1): epoch_id, fp, limit, cutoff, floor_hwm, floor_gen.
const (
	dArgEpochID  = 0
	dArgFP       = 1
	dArgFloorHwm = 4
	dArgFloorGen = 5
)

// dueReq is a due_entries call in epochID with fingerprint fp, at most limit members up to
// cutoff (0 = none), floors 0, on the maintenance connection (§A4.1, §A5.8).
func dueReq(fp string, limit, cutoff int64) store.Request {
	return store.Request{
		Kind:   store.Maintenance,
		Script: scripts.DueEntries,
		Keys:   []string{keys.EpochKey, keys.CtlTimeKey, ixKey},
		Args:   []string{epochID, fp, num(limit), num(cutoff), "0", "0"},
	}
}

// cleanupReq is a cleanup_entry step for the ix member member in epochID with fingerprint fp
// and policy p's values, floors 0 (§A4.1): its k and mi KEYS, or the placeholders when member
// fails is_enc, as Go passes them.
func cleanupReq(p settings.Policy, fp, member string) store.Request {
	k, mi := keys.BucketPlaceholder(epochID), keys.MarkerIndexPlaceholder(epochID)
	if keys.IsEnc(member) {
		k, mi = kKey(member), miKey(member)
	}
	return store.Request{
		Kind:   store.Maintenance,
		Script: scripts.CleanupEntry,
		Keys:   []string{keys.EpochKey, keys.CtlTimeKey, ixKey, qKey, k, mi},
		Args: []string{
			epochID, fp, member, num(p.Capacity * p.RatePeriodNs), num(p.RateTokens),
			num(p.ReceiptLifetimeNs / 1000), "0", "0",
		},
	}
}

// step runs a cleanup_entry step for member as rl_lib and returns its decoded reply.
func (e *env) step(member string) []any {
	e.t.Helper()
	return e.run(e.la, cleanupReq(e.p, e.fp, member))
}

// mark is one refund marker of a test bucket: the receipt (gen0, s), its expiry and its
// credit time.
type mark struct{ s, expiry, credit int64 }

func (m mark) field() string  { return "m:" + num(gen0) + ":" + num(m.s) }
func (m mark) member() string { return num(gen0) + ":" + num(m.s) }
func (m mark) value() string  { return num(m.expiry) + ":" + num(m.credit) }

// marks returns n markers for the receipts (gen0, 1) … (gen0, n): receipt s expires at
// last − (n − s) µs, so the last one at last, each credited at oldT.
func marks(n int, last int64) []mark {
	out := make([]mark, n)
	for i := range out {
		s := int64(i + 1)
		out[i] = mark{s: s, expiry: last - int64(n) + s, credit: oldT}
	}
	return out
}

// writeKey writes, as rl_fixture, enc's bucket (u units at time bt, generation gen0, sequence
// s, latest receipt expiry x, mc = len(ms)), its markers and their mi entries in commands of
// 500 at most, and enc's ix member scored score. The record's counters are the test's.
func (e *env) writeKey(enc string, u, bt, s, x int64, score string, ms ...mark) {
	e.t.Helper()
	e.hset(kKey(enc), "u", num(u), "t", num(bt), "g", num(gen0), "s", num(s), "x", num(x), "mc", num(int64(len(ms))))
	e.do("DEL", miKey(enc))
	for start := 0; start < len(ms); start += 500 {
		hs, zs := []string{"HSET", kKey(enc)}, []string{"ZADD", miKey(enc)}
		for _, m := range ms[start:min(start+500, len(ms))] {
			hs = append(hs, m.field(), m.value())
			zs = append(zs, num(m.expiry), m.member())
		}
		e.do(hs...)
		e.do(zs...)
	}
	e.do("ZADD", ixKey, score, enc)
}

// fullKey writes enc's bucket full by now0 (no units at oldT), sequence 3, latest receipt
// expiry x, no markers, and its ix member at x, its eligibility time (§A3.2).
func (e *env) fullKey(enc string, x int64) {
	e.t.Helper()
	e.writeKey(enc, 0, oldT, 3, x, num(x))
}

// delMember deletes a hash field or sorted set member from st; a key left empty is gone, as
// the store keeps no empty hash or sorted set.
func delMember(st storestate.State, key, member string) {
	v, ok := st[key]
	if !ok {
		return
	}
	delete(v.Fields, member)
	if len(v.Fields) == 0 {
		delete(st, key)
	}
}

// counter returns a record counter of st.
func counter(t *testing.T, st storestate.State, f string) int64 {
	t.Helper()
	return mustInt(t, st[keys.EpochKey].Fields[f])
}

// stepHwm sets hwm = at in want when at is later than the stored mark (write_hwm, CLK-4).
func stepHwm(t *testing.T, want storestate.State, at int64) {
	t.Helper()
	if at > counter(t, want, "hwm") {
		setField(want, keys.EpochKey, "hwm", num(at))
	}
}

// removedState is what an E_REMOVED step at budget time at leaves after before (DESIGN §A4.5):
// enc's k and mi gone, its ix member gone, nb − 1 and nm − mc, neither below 0 (dec_counter),
// and hwm = at.
func removedState(t *testing.T, before storestate.State, enc string, at int64) storestate.State {
	t.Helper()
	want := cloneState(before)
	mc := mustInt(t, want[kKey(enc)].Fields["mc"])
	delete(want, kKey(enc))
	delete(want, miKey(enc))
	delMember(want, ixKey, enc)
	setField(want, keys.EpochKey, "nb", num(max(counter(t, want, "nb")-1, 0)))
	setField(want, keys.EpochKey, "nm", num(max(counter(t, want, "nm")-mc, 0)))
	stepHwm(t, want, at)
	return want
}

// movedState is what an E_MOVED step at budget time at leaves after before (DESIGN §A4.5): the
// due markers' fields and mi members gone, mc less their number, nm too (never below 0), enc's
// ix score = score, and hwm = at.
func movedState(t *testing.T, before storestate.State, enc string, due []mark, score, at int64) storestate.State {
	t.Helper()
	want := cloneState(before)
	for _, m := range due {
		delMember(want, kKey(enc), m.field())
		delMember(want, miKey(enc), m.member())
	}
	n := int64(len(due))
	if n > 0 {
		setField(want, kKey(enc), "mc", num(mustInt(t, want[kKey(enc)].Fields["mc"])-n))
		setField(want, keys.EpochKey, "nm", num(max(counter(t, want, "nm")-n, 0)))
	}
	setMember(want, ixKey, enc, num(score))
	stepHwm(t, want, at)
	return want
}

// pushedState is what a step that pushes a quarantined or newly quarantined key at budget
// time at leaves after before (DESIGN §A4.5): enc's ix score t + push_us (the receipt
// lifetime), its q field reason|t when reason is not "", and hwm = at.
func pushedState(t *testing.T, before storestate.State, enc, reason string, at int64) storestate.State {
	t.Helper()
	want := cloneState(before)
	if reason != "" {
		setField(want, qKey, enc, reason+"|"+num(at))
	}
	setMember(want, ixKey, enc, num(at+lifetimeUs))
	stepHwm(t, want, at)
	return want
}

// TestDueEntries_Page (plan S7, L2; DESIGN §A4.5; kills mutant PB10): a page holds the ix
// members whose score is at most min(T0, cutoff), cutoff 0 being none, in score order and at
// most limit of them, both sides of each bound; the reply carries T0 and the oldest score,
// the members without their scores; the page stores hwm = T0 when T0 is later, also when it
// is empty. With the clock behind the high-water mark, T0 is the mark.
func TestDueEntries_Page(t *testing.T) {
	p := testPolicy()
	// Six full buckets, each scored at its latest receipt expiry: three before hwm0, one at
	// now0 (T0 exactly), one at T0 + 1 and one 50 s later.
	scores := []struct {
		enc   string
		score int64
	}{
		{encA, hwm0 - 5_000_000}, {encB, hwm0 - 3_000_000}, {encC, hwm0 - 1_000_000},
		{encD, now0}, {"ZQ", now0 + 1}, {"Zg", now0 + 50_000_000},
	}
	oldest := scores[0].score
	cases := []struct {
		name          string
		clock         int64
		limit, cutoff int64
		want          []any
		hwm           int64 // the stored mark afterwards
	}{
		{"limit 3", now0, 3, 0, rep(0, now0, "", oldest, encA, encB, encC), now0},
		{"limit 1", now0, 1, 0, rep(0, now0, "", oldest, encA), now0},
		{"every member due at T0, none after it", now0, 10, 0, rep(0, now0, "", oldest, encA, encB, encC, encD), now0},
		{"cutoff equal to a score", now0, 10, hwm0 - 3_000_000, rep(0, now0, "", oldest, encA, encB), now0},
		{"cutoff 1 µs before a score", now0, 10, hwm0 - 3_000_001, rep(0, now0, "", oldest, encA), now0},
		{"cutoff after T0", now0, 10, now0 + 50_000_000, rep(0, now0, "", oldest, encA, encB, encC, encD), now0},
		{"an empty page", now0, 10, hwm0 - 6_000_000, rep(0, now0, "", 0), now0},
		{"the clock behind the high-water mark", hwm0 - 2_000_000, 10, 0, rep(0, hwm0, "", oldest, encA, encB, encC), hwm0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.record(healthy(e.fp).with("nb", num(int64(len(scores)))))
			e.clock(c.clock)
			for _, s := range scores {
				e.fullKey(s.enc, s.score)
			}
			before := e.snap()
			checkReply(t, c.name, e.run(e.la, dueReq(e.fp, c.limit, c.cutoff)), c.want)
			want := cloneState(before)
			setField(want, keys.EpochKey, "hwm", num(c.hwm))
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// TestDueEntries_OldestScore (plan S7, L2; DESIGN §A4.5, §A4.9): the reply's oldest is the
// first member's score when it passes is_score, else 0, so a tampered score (1.5, -inf, -5;
// the key declared damaged) cannot fail the decoder's range check; the member is still on the
// page and hwm = T0 is stored.
func TestDueEntries_OldestScore(t *testing.T) {
	p := testPolicy()
	for _, score := range []string{"1.5", "-inf", "-5"} {
		t.Run(score, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.record(healthy(e.fp).with("nb", "2"))
			e.clock(now0)
			e.fullKey(encA, pastX)
			e.do("ZADD", ixKey, score, encA)
			e.h.Damaged(kKey(encA))
			e.fullKey(encB, hwm0-3_000_000)
			before := e.snap()
			checkReply(t, score, e.run(e.la, dueReq(e.fp, 10, 0)), rep(0, now0, "", 0, encA, encB))
			want := cloneState(before)
			setField(want, keys.EpochKey, "hwm", num(now0))
			checkState(t, score, e.snap(), want)
		})
	}
}

// TestDueEntries_Refusals (plan S7, L2; DESIGN §A4.2, §A4.5): the prelude's refusals in its
// order, each with its exact reply and nothing written; floors above the stored record set the
// hold store_rollback, and an ix of the wrong type the hold shared_key_malformed (§A4.8 rule
// 2), each writing only the hold, never hwm.
func TestDueEntries_Refusals(t *testing.T) {
	p := testPolicy()
	fp := fingerprint.Of(p)
	base := healthy(fp)
	held := rep(22, 0, "integrity_hold")
	cases := []struct {
		name    string
		record  rec // nil: no record
		ctl     string
		req     func(*store.Request)
		setup   func(e *env)
		damaged []string
		want    []any
		hold    string
	}{
		{name: "record missing", want: rep(22, 0, "epoch_record_missing")},
		{name: "schema 2", record: base.with("schema", "2"), damaged: []string{keys.EpochKey}, want: rep(22, 0, "epoch_record_malformed")},
		{name: "hwm absent", record: base.with("hwm", "<del>"), damaged: []string{keys.EpochKey}, want: rep(22, 0, "hwm_missing")},
		{name: "hwm TBOUND", record: base.with("hwm", num(tBound)), damaged: []string{keys.EpochKey}, want: rep(22, 0, "hwm_malformed")},
		{name: "hold set", record: base.with("hold", "operator"), want: held},
		{name: "wrong id", record: base, req: func(r *store.Request) { r.Args[dArgEpochID] = otherID }, want: rep(20, 0, "epoch_or_fingerprint")},
		{name: "wrong fp", record: base, req: func(r *store.Request) { r.Args[dArgFP] = fingerprint.Of(storeClockPolicy()) },
			want: rep(20, 0, "epoch_or_fingerprint")},
		{name: "hwm below floor_hwm", record: base, req: func(r *store.Request) { r.Args[dArgFloorHwm] = num(hwm0 + 1) }, want: held, hold: "store_rollback"},
		{name: "gen below floor_gen", record: base, req: func(r *store.Request) { r.Args[dArgFloorGen] = num(gen0 + 1) }, want: held, hold: "store_rollback"},
		{name: "controlled time missing", record: base, ctl: "<del>", want: rep(21, 0, "clock")},
		{name: "controlled time TBOUND", record: base, ctl: num(tBound), want: rep(21, 0, "clock")},
		{name: "ix of the wrong type", record: base, setup: func(e *env) {
			e.do("DEL", ixKey)
			e.do("SET", ixKey, "x")
		}, damaged: []string{ixKey}, want: held, hold: "shared_key_malformed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			if c.record != nil {
				e.record(c.record)
			}
			e.h.Damaged(c.damaged...)
			e.fullKey(encA, pastX)
			switch c.ctl {
			case "":
				e.clock(now0)
			case "<del>":
			default:
				e.do("SET", keys.CtlTimeKey, c.ctl)
			}
			if c.setup != nil {
				c.setup(e)
			}
			req := dueReq(fp, 10, 0)
			if c.req != nil {
				c.req(&req)
			}
			before := e.snap()
			checkReply(t, c.name, e.run(e.la, req), c.want)
			want := cloneState(before)
			if c.hold != "" {
				setField(want, keys.EpochKey, "hold", c.hold)
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// TestCleanupEntry_Refusals (plan S7, L2; DESIGN §A4.1, §A4.2, §A4.5): the prelude's refusals,
// each with its exact reply and nothing written but the hold where one is set; the ix and q
// read errors and a member that fails is_enc (its KEYS the placeholders Go passes) set the
// hold, never hwm (§A4.8 rule 8); a KEYS mismatch checked after the gone, not_due and is_enc
// checks is {25, 0, 'keys_mismatch'} with nothing written. Floors equal to the record pass.
func TestCleanupEntry_Refusals(t *testing.T) {
	p := testPolicy()
	fp := fingerprint.Of(p)
	base := healthy(fp)
	held := rep(22, 0, "integrity_hold")
	bad := rep(25, 0, "keys_mismatch")
	cases := []struct {
		name    string
		record  rec // nil: no record
		ctl     string
		member  string // "" = encA
		req     func(*store.Request)
		setup   func(e *env)
		damaged []string
		want    []any
		hold    string
		removed bool // the step passes and removes encA's bucket
	}{
		{name: "record missing", want: rep(22, 0, "epoch_record_missing")},
		{name: "id upper case", record: base.with("id", strings.ToUpper(epochID)), damaged: []string{keys.EpochKey}, want: rep(22, 0, "epoch_record_malformed")},
		{name: "rl:epoch a string", setup: func(e *env) { e.do("SET", keys.EpochKey, "x") }, damaged: []string{keys.EpochKey}, want: rep(22, 0, "epoch_record_malformed")},
		{name: "hwm absent", record: base.with("hwm", "<del>"), damaged: []string{keys.EpochKey}, want: rep(22, 0, "hwm_missing")},
		{name: "hwm x", record: base.with("hwm", "x"), damaged: []string{keys.EpochKey}, want: rep(22, 0, "hwm_malformed")},
		{name: "hold set", record: base.with("hold", "operator"), want: held},
		{name: "wrong id", record: base, req: func(r *store.Request) { r.Args[cArgEpochID] = otherID }, want: rep(20, 0, "epoch_or_fingerprint")},
		{name: "wrong fp", record: base, req: func(r *store.Request) { r.Args[cArgFP] = fingerprint.Of(storeClockPolicy()) },
			want: rep(20, 0, "epoch_or_fingerprint")},
		{name: "hwm below floor_hwm", record: base, req: func(r *store.Request) { r.Args[cArgFloorHwm] = num(hwm0 + 1) }, want: held, hold: "store_rollback"},
		{name: "gen below floor_gen", record: base, req: func(r *store.Request) { r.Args[cArgFloorGen] = num(gen0 + 1) }, want: held, hold: "store_rollback"},
		{name: "floors equal to the record", record: base, req: func(r *store.Request) {
			r.Args[cArgFloorHwm], r.Args[cArgFloorGen] = num(hwm0), num(gen0)
		}, want: rep(30, now0, "", 0), removed: true},
		{name: "controlled time missing", record: base, ctl: "<del>", want: rep(21, 0, "clock")},
		{name: "controlled time 1.5", record: base, ctl: "1.5", want: rep(21, 0, "clock")},
		{name: "ix of the wrong type", record: base, setup: func(e *env) {
			e.do("DEL", ixKey)
			e.do("SET", ixKey, "x")
		}, damaged: []string{ixKey}, want: held, hold: "shared_key_malformed"},
		{name: "q of the wrong type", record: base, setup: func(e *env) { e.do("SET", qKey, "x") },
			damaged: []string{qKey}, want: held, hold: "shared_key_malformed"},
		{name: "member YR (not canonical)", record: base, member: "YR", want: held, hold: "unattributable_entry"},
		{name: "member Y (length 1)", record: base, member: "Y", want: held, hold: "unattributable_entry"},
		{name: "member Y. (a character outside base64url)", record: base, member: "Y.", want: held, hold: "unattributable_entry"},
		{name: "member empty", record: base, member: "<empty>", want: held, hold: "unattributable_entry"},
		{name: "wrong k KEYS", record: base, req: func(r *store.Request) { r.Keys[4] = kKey(encB) }, want: bad},
		{name: "wrong mi KEYS", record: base, req: func(r *store.Request) { r.Keys[5] = miKey(encB) }, want: bad},
		{name: "wrong q KEYS", record: base, req: func(r *store.Request) { r.Keys[3] = keys.QuarantineKey(otherID) }, want: bad},
		// An old namespace's ix that holds encA due passes the gone and not_due checks, which
		// come first, and fails the KEYS assertion.
		{name: "wrong ix KEYS", record: base, setup: func(e *env) { e.do("ZADD", keys.IndexKey(otherID), num(pastX), encA) },
			req: func(r *store.Request) { r.Keys[2] = keys.IndexKey(otherID) }, want: bad},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			if c.record != nil {
				e.record(c.record)
			}
			e.h.Damaged(c.damaged...)
			e.fullKey(encA, pastX)
			switch c.ctl {
			case "":
				e.clock(now0)
			case "<del>":
			default:
				e.do("SET", keys.CtlTimeKey, c.ctl)
			}
			member := encA
			switch c.member {
			case "":
			case "<empty>":
				member = ""
			default:
				member = c.member
			}
			if member != encA {
				// The ix member that fails is_enc, due; the audit exempts it by its bucket name.
				e.do("ZADD", ixKey, num(pastX), member)
				e.h.Damaged(kKey(member))
			}
			if c.setup != nil {
				c.setup(e)
			}
			req := cleanupReq(p, fp, member)
			if c.req != nil {
				c.req(&req)
			}
			before := e.snap()
			checkReply(t, c.name, e.run(e.la, req), c.want)
			want := cloneState(before)
			switch {
			case c.hold != "":
				setField(want, keys.EpochKey, "hold", c.hold)
			case c.removed:
				want = removedState(t, before, encA, now0)
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// TestCleanupEntry_Skipped (plan S7, L2; DESIGN §A4.5, §A4.8 rule 8; SPEC CLK-4): a member
// with no ix entry is {33, t, 'gone', 0} and one scored after t (t + 1, inf) {33, t, 'not_due',
// 0}, both writing nothing, not even hwm, though t is later than the mark; a score equal to t
// is due. With the clock behind the mark, t is the mark.
func TestCleanupEntry_Skipped(t *testing.T) {
	p := testPolicy()
	cases := []struct {
		name    string
		clock   int64
		member  string
		score   string // encA's ix score
		damaged bool
		want    []any
		removed bool
	}{
		{name: "gone", clock: now0, member: encB, score: num(pastX), want: rep(33, now0, "gone", 0)},
		{name: "scored t + 1", clock: now0, member: encA, score: num(now0 + 1), want: rep(33, now0, "not_due", 0)},
		{name: "scored inf", clock: now0, member: encA, score: "inf", damaged: true, want: rep(33, now0, "not_due", 0)},
		{name: "scored t", clock: now0, member: encA, score: num(now0), want: rep(30, now0, "", 0), removed: true},
		{name: "the clock behind the mark, scored hwm + 1", clock: hwm0 - 1_000, member: encA, score: num(hwm0 + 1),
			want: rep(33, hwm0, "not_due", 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.record(healthy(e.fp))
			e.clock(c.clock)
			// Full at now0; x is the score when it is an integer, so the ix rule holds (§A3.2).
			x := int64(now0)
			if c.score != "inf" {
				x = mustInt(t, c.score)
			}
			e.writeKey(encA, 0, oldT, 3, x, c.score)
			if c.damaged {
				e.h.Damaged(kKey(encA))
			}
			before := e.snap()
			checkReply(t, c.name, e.step(c.member), c.want)
			want := before
			if c.removed {
				want = removedState(t, before, encA, c.clock)
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// TestCleanupEntry_Quarantined (plan S7, L2; DESIGN §A4.5; SPEC STA-4, STA-10): a quarantined
// key is never removed: the step pushes its ix score to t + push_us and replies {31, t,
// 'quarantined', 0}, writing that and hwm only, whether its bucket is eligible or gone.
func TestCleanupEntry_Quarantined(t *testing.T) {
	p := testPolicy()
	for _, c := range []struct {
		name     string
		noBucket bool
	}{{"eligible bucket", false}, {"bucket gone", true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.record(healthy(e.fp))
			e.clock(now0)
			e.fullKey(encA, pastX)
			if c.noBucket {
				e.do("DEL", kKey(encA))
			}
			e.do("HSET", qKey, encA, "malformed_bucket|"+num(hwm0))
			e.h.Damaged(kKey(encA))
			before := e.snap()
			checkReply(t, c.name, e.step(encA), rep(31, now0, "quarantined", 0))
			checkState(t, c.name, e.snap(), pushedState(t, before, encA, "", now0))
		})
	}
}

// TestCleanupEntry_CheckKeyRows (plan S7, L2; DESIGN §A3.3, §A4.5; SPEC STA-5, STA-10): each
// §A3.3 row that can hold while the key's ix member exists (so not bucket_without_index)
// quarantines the key: {32, t, reason, 0}, its q field reason|t, its ix score t + push_us and
// hwm, nothing else; the entry is kept, never silently deleted.
func TestCleanupEntry_CheckKeyRows(t *testing.T) {
	p := testPolicy()
	const marker = "1759999985123456:1759999980123456" // expiry pastX − 5 s, credit oldT
	cases := []struct {
		name, reason string
		apply        func(e *env)
		score        string // encA's ix score; "" = pastX
	}{
		{name: "k absent, mi present", reason: "marker_index_without_bucket", apply: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZADD", miKey(encA), "1759999985123456", num(gen0)+":1")
		}},
		{name: "k absent", reason: "index_without_bucket", apply: func(e *env) { e.do("DEL", kKey(encA)) }},
		{name: "k a string", reason: "malformed_bucket", apply: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("SET", kKey(encA), "x")
		}},
		{name: "s missing", reason: "malformed_bucket", apply: func(e *env) { e.do("HDEL", kKey(encA), "s") }},
		{name: "u above cap_units", reason: "malformed_bucket", apply: func(e *env) { e.do("HSET", kKey(encA), "u", num(capUnits+1)) }},
		{name: "t 1.5", reason: "malformed_bucket", apply: func(e *env) { e.do("HSET", kKey(encA), "t", "1.5") }},
		{name: "a field outside the layout", reason: "malformed_bucket", apply: func(e *env) { e.do("HSET", kKey(encA), "zz", "1") }},
		{name: "g 0", reason: "malformed_bucket", apply: func(e *env) { e.do("HSET", kKey(encA), "g", "0") }},
		{name: "marker fields only", reason: "marker_without_bucket", apply: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("HSET", kKey(encA), "m:"+num(gen0)+":1", marker)
		}},
		{name: "marker field m:7:x", reason: "malformed_marker", apply: func(e *env) {
			e.do("HSET", kKey(encA), "m:"+num(gen0)+":x", marker, "mc", "1")
		}},
		{name: "marker of another generation", reason: "malformed_marker", apply: func(e *env) {
			e.do("HSET", kKey(encA), "m:"+num(gen0+1)+":1", marker, "mc", "1")
		}},
		{name: "mc 2 with one marker", reason: "marker_count", apply: func(e *env) {
			e.do("HSET", kKey(encA), "m:"+num(gen0)+":1", marker, "mc", "2")
			e.do("ZADD", miKey(encA), "1759999985123456", num(gen0)+":1")
		}},
		{name: "mi a string", reason: "marker_index_mismatch", apply: func(e *env) { e.do("SET", miKey(encA), "x") }},
		{name: "mi member not g:s", reason: "marker_index_mismatch", apply: func(e *env) { e.do("ZADD", miKey(encA), "5", "x") }},
		{name: "mi member without its marker", reason: "marker_index_mismatch", apply: func(e *env) {
			e.do("ZADD", miKey(encA), "1759999985123456", num(gen0)+":1")
		}},
		{name: "mi score not the expiry", reason: "marker_index_mismatch", apply: func(e *env) {
			e.do("HSET", kKey(encA), "m:"+num(gen0)+":1", marker, "mc", "1")
			e.do("ZADD", miKey(encA), "1759999985123457", num(gen0)+":1")
		}},
		{name: "ix score 1.5", reason: "malformed_index", score: "1.5"},
		{name: "ix score -inf", reason: "malformed_index", score: "-inf"},
		{name: "ix score -5", reason: "malformed_index", score: "-5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.record(healthy(e.fp))
			e.clock(now0)
			score := c.score
			if score == "" {
				score = num(pastX)
			}
			e.writeKey(encA, 0, oldT, 3, pastX, score)
			if c.apply != nil {
				c.apply(e)
			}
			e.h.Damaged(kKey(encA), miKey(encA))
			before := e.snap()
			checkReply(t, c.name, e.step(encA), rep(32, now0, c.reason, 0))
			checkState(t, c.name, e.snap(), pushedState(t, before, encA, c.reason, now0))
		})
	}
}

// TestCleanupEntry_Eligible (plan S7, L2; DESIGN §A4.5; SPEC STA-3, STA-4): a bucket refilled to
// capacity whose latest receipt and every marker have expired is removed in one step with its
// markers, mi entries and ix member: {30, t, "", mc}, nb − 1, nm − mc, hwm = t; another key
// stays as it was. Each condition holds at its boundary: x = t, a marker expiring at t, a
// bucket refilled exactly to capacity at t.
func TestCleanupEntry_Eligible(t *testing.T) {
	p := testPolicy()
	cases := []struct {
		name  string
		u, bt int64
		x     int64
		ms    []mark
	}{
		{name: "no markers", u: 0, bt: oldT, x: pastX},
		{name: "three expired markers", u: 0, bt: oldT, x: pastX, ms: marks(3, pastX)},
		{name: "x = t, a marker expiring at t", u: 0, bt: oldT, x: now0, ms: marks(2, now0)},
		// 1,000 µs × 10,000 units/µs = 10^7 units: exactly capacity at t (§A3.1).
		{name: "refilled exactly to capacity at t", u: capUnits - 10_000_000, bt: now0 - 1_000, x: pastX},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.record(healthy(e.fp).with("nb", "2", "nm", num(int64(len(c.ms)))))
			e.clock(now0)
			score := c.x
			if len(c.ms) > 0 {
				score = c.ms[0].expiry
			}
			e.writeKey(encA, c.u, c.bt, 3, c.x, num(score), c.ms...)
			e.fullKey(encB, now0+5_000_000)
			before := e.snap()
			checkReply(t, c.name, e.step(encA), rep(30, now0, "", len(c.ms)))
			checkState(t, c.name, e.snap(), removedState(t, before, encA, now0))
		})
	}
}

// TestCleanupEntry_NotEligible (plan S7, L2; DESIGN §A4.5; SPEC STA-3, STA-5): a bucket that is
// not eligible keeps its bucket; the step removes its due markers (expiry ≤ t) and their mi
// entries, takes their number off mc and nm, and moves the ix score to
// min(max(t + ⌈(cap_units − u) / (rate_tokens × 1000)⌉, x, the latest other marker), the
// earliest other marker), always after t: {31, t, "", removed}. Each score below is computed
// by hand from that formula. The cases include a key whose only marker is due.
func TestCleanupEntry_NotEligible(t *testing.T) {
	p := testPolicy()
	mixed := []mark{
		{s: 1, expiry: now0 - 2_000_000, credit: oldT}, // due
		{s: 2, expiry: now0, credit: oldT},             // due: expiry = t
		{s: 3, expiry: now0 + 1, credit: oldT},         // not due
		{s: 4, expiry: now0 + 20_000_000, credit: oldT},
	}
	partial := []mark{
		{s: 1, expiry: now0 - 1_000_000, credit: oldT},
		{s: 2, expiry: now0 + 500_000, credit: oldT},
	}
	cases := []struct {
		name  string
		u, bt int64
		x     int64
		ms    []mark
		due   int   // the first due markers of ms
		score int64 // the new ix score
	}{
		// Full at t, x = t + 1: max(t + 0, t + 1) = t + 1.
		{name: "latest receipt unexpired", u: 0, bt: oldT, x: now0 + 1, score: now0 + 1},
		// u = cap − 1 at t: max(t + ⌈1 / 10,000⌉, pastX) = t + 1.
		{name: "refilled 1 unit short", u: capUnits - 1 - 10_000_000, bt: now0 - 1_000, x: pastX, score: now0 + 1},
		// Full; max(t, t + 30 s, t + 20 s) = t + 30 s; min with the earliest other marker, t + 1.
		{name: "two markers due, two not", u: 0, bt: oldT, x: now0 + 30_000_000, ms: mixed, due: 2, score: now0 + 1},
		// 500 ms × 10,000 units/µs = 5 × 10^9 units at t: ⌈1.5 × 10^10 / 10^4⌉ = 1.5 s, so
		// max(t + 1.5 s, t + 1 s, t + 0.5 s) = t + 1.5 s; min with t + 0.5 s.
		{name: "not full, one marker due", u: 0, bt: now0 - 500_000, x: now0 + 1_000_000, ms: partial, due: 1, score: now0 + 500_000},
		// Full; max(t, t + 10 s) = t + 10 s, no other marker.
		{name: "the only marker due", u: 0, bt: oldT, x: now0 + 10_000_000, ms: marks(1, now0-1_000_000), due: 1, score: now0 + 10_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.record(healthy(e.fp).with("nm", num(int64(len(c.ms)))))
			e.clock(now0)
			score := int64(pastX) // at most the eligibility time and every marker expiry (§A3.2)
			if len(c.ms) > 0 {
				score = c.ms[0].expiry
			}
			e.writeKey(encA, c.u, c.bt, 3, c.x, num(score), c.ms...)
			before := e.snap()
			checkReply(t, c.name, e.step(encA), rep(31, now0, "", c.due))
			if c.score <= now0 {
				t.Errorf("%s: the expected score %d is not after t", c.name, c.score)
			}
			checkState(t, c.name, e.snap(), movedState(t, before, encA, c.ms[:c.due], c.score, now0))
		})
	}
}

// TestCleanupEntry_NotFullDueKept (plan S7, L2; DESIGN §A4.5; SPEC STA-4; kills mutant PB9): a
// crafted due bucket whose latest receipt and only marker have expired but which has not
// refilled to capacity at t is kept: {31, t, "", 1}, the marker removed, the ix score moved to
// max(t + ⌈(cap_units − u) / (rate_tokens × 1000)⌉, x), after t. (With a receipt lifetime
// longer than the time to fill, x ≤ t already implies a full bucket, so only a crafted state
// tells the capacity condition apart.)
func TestCleanupEntry_NotFullDueKept(t *testing.T) {
	p := testPolicy()
	e := newEnv(t, p)
	e.record(healthy(e.fp).with("nm", "1"))
	e.clock(now0)
	// No units at hwm0 − 500 ms; at now0, 600 ms later, 6 × 10^9 units (§A3.1), 1.4 × 10^10
	// short of capacity: ⌈1.4 × 10^10 / 10^4⌉ = 1.4 s, so the score is now0 + 1.4 s.
	m := mark{s: 1, expiry: hwm0 - 450_000, credit: hwm0 - 500_000}
	e.writeKey(encA, 0, hwm0-500_000, 1, hwm0-400_000, num(m.expiry), m)
	before := e.snap()
	checkReply(t, "not full", e.step(encA), rep(31, now0, "", 1))
	checkState(t, "not full", e.snap(), movedState(t, before, encA, []mark{m}, now0+1_400_000, now0))
}

// TestCleanupEntry_UnexpiredMarkerKept (plan S7, L2; DESIGN §A4.5; SPEC STA-4 "every marker
// expired"): a crafted full bucket whose latest receipt expiry x has passed but which holds one
// marker expiring after t (beyond x, so k and mi are declared Damaged) is kept: {31, t, "", 0},
// the bucket and the marker untouched, and the ix score moved to min(max(t + 0, x, the
// marker's expiry), the marker's expiry) = the marker's expiry, t + 10 s. (In a state the
// scripts write, every marker expires by x, so x ≤ t already makes every marker due; only a
// crafted state tells the marker condition apart.)
func TestCleanupEntry_UnexpiredMarkerKept(t *testing.T) {
	p := testPolicy()
	e := newEnv(t, p)
	e.record(healthy(e.fp).with("nm", "1"))
	e.clock(now0)
	// No units at oldT: full again 2 s later, long before now0.
	m := mark{s: 1, expiry: now0 + 10_000_000, credit: oldT}
	e.writeKey(encA, 0, oldT, 1, pastX, num(pastX), m)
	e.h.Damaged(kKey(encA), miKey(encA))
	before := e.snap()
	checkReply(t, "unexpired marker", e.step(encA), rep(31, now0, "", 0))
	checkState(t, "unexpired marker", e.snap(), movedState(t, before, encA, nil, now0+10_000_000, now0))
}

// TestCleanupEntry_CountersStopAtZero (plan S7, L2; DESIGN §A3.3 dec_counter): with nb and nm
// tampered low, a removal and a marker removal take them to 0, never below.
func TestCleanupEntry_CountersStopAtZero(t *testing.T) {
	p := testPolicy()
	t.Run("removal", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, p)
		e.record(healthy(e.fp).with("nb", "0", "nm", "1"))
		e.clock(now0)
		ms := marks(3, pastX)
		e.writeKey(encA, 0, oldT, 3, pastX, num(ms[0].expiry), ms...)
		before := e.snap()
		checkReply(t, "removal", e.step(encA), rep(30, now0, "", 3))
		want := removedState(t, before, encA, now0)
		if nb, nm := counter(t, want, "nb"), counter(t, want, "nm"); nb != 0 || nm != 0 {
			t.Fatalf("expected counters nb %d, nm %d; want 0 and 0", nb, nm)
		}
		checkState(t, "removal", e.snap(), want)
	})
	t.Run("marker removal", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, p)
		e.record(healthy(e.fp).with("nm", "1"))
		e.clock(now0)
		ms := marks(2, now0-1_000_000)
		e.writeKey(encA, 0, oldT, 3, now0+10_000_000, num(ms[0].expiry), ms...)
		before := e.snap()
		checkReply(t, "marker removal", e.step(encA), rep(31, now0, "", 2))
		want := movedState(t, before, encA, ms, now0+10_000_000, now0)
		if nm := counter(t, want, "nm"); nm != 0 {
			t.Fatalf("expected nm %d, want 0", nm)
		}
		checkState(t, "marker removal", e.snap(), want)
	})
}

// tenK is testPolicy with max_markers_per_key raised to 10,000 (plan S7 Tests (a), §C3).
func tenK() settings.Policy {
	p := testPolicy()
	p.MaxMarkersPerKey = 10_000
	return p
}

// TestCleanupEntry_10kMarkersEligible (plan S7, L2; DESIGN §C3, §A7): an eligible key holding
// 10,000 expired markers is removed in one step, {30, t, "", 10000}, with no k or mi left and
// nm − 10,000: no partial write.
func TestCleanupEntry_10kMarkersEligible(t *testing.T) {
	p := tenK()
	e := newEnv(t, p)
	e.record(healthy(e.fp).with("nm", "10000"))
	e.clock(now0)
	ms := marks(10_000, pastX)
	e.writeKey(encA, 0, oldT, 10_000, pastX, num(ms[0].expiry), ms...)
	before := e.snap()
	checkReply(t, "10,000 markers", e.step(encA), rep(30, now0, "", 10_000))
	got := e.snap()
	if _, ok := got[kKey(encA)]; ok {
		t.Errorf("%s still exists", kKey(encA))
	}
	if _, ok := got[miKey(encA)]; ok {
		t.Errorf("%s still exists", miKey(encA))
	}
	checkState(t, "10,000 markers", got, removedState(t, before, encA, now0))
}

// TestCleanupEntry_10kDueMarkersNotEligible (plan S7, L2; DESIGN §C3, §A4.8 rule 3): one step
// on a key that is not eligible removes all of its 10,000 due markers in chunks of 500, then
// mc = 0, nm is 10,000 less and the ix score is max(t, x) = x (full, no other marker).
func TestCleanupEntry_10kDueMarkersNotEligible(t *testing.T) {
	p := tenK()
	e := newEnv(t, p)
	e.record(healthy(e.fp).with("nm", "10000"))
	e.clock(now0)
	ms := marks(10_000, now0)
	x := int64(now0 + 5_000_000)
	e.writeKey(encA, 0, oldT, 10_000, x, num(ms[0].expiry), ms...)
	before := e.snap()
	checkReply(t, "10,000 due markers", e.step(encA), rep(31, now0, "", 10_000))
	got := e.snap()
	if mc := got[kKey(encA)].Fields["mc"]; mc != "0" {
		t.Errorf("mc = %q, want 0", mc)
	}
	if nm := got[keys.EpochKey].Fields["nm"]; nm != "0" {
		t.Errorf("nm = %q, want 0", nm)
	}
	if s := got[ixKey].Fields[encA]; s != num(x) {
		t.Errorf("ix score %q, want %d", s, x)
	}
	checkState(t, "10,000 due markers", got, movedState(t, before, encA, ms, x, now0))
}

// TestCeilDiv_Boundaries (plan S7, L2; DESIGN §A3.1, §C3): ceil_div equals ⌈a / b⌉ in math/big
// at the boundaries of cleanup's use, a = cap_units − u in [0, 2^52) and b = rate_tokens × 1000
// in [1,000, (2^31 − 1) × 1,000]: a = 0, a around multiples of b, and the largest values.
// Then cleanup_entry's score at t + ⌈(cap_units − u) / 10,000⌉ for cap_units − u = 29,999,
// 30,000 and 30,001: t + 3, t + 3 and t + 4.
func TestCeilDiv_Boundaries(t *testing.T) {
	const (
		maxB = (1<<31 - 1) * 1000 // the largest rate_tokens × 1000
		maxA = 1<<52 - 1          // the largest cap_units − u
	)
	c := preludeClient(t)
	cases := [][]int64{
		{0, 1000}, {1, 1000}, {999, 1000}, {1000, 1000}, {1001, 1000}, {1999, 1000}, {2000, 1000}, {2001, 1000},
		{0, unitsUs}, {1, unitsUs}, {capUnits - 1, unitsUs}, {capUnits, unitsUs}, {capUnits + 1, unitsUs},
		{maxA, 1000}, {maxA, maxB}, {maxA - 1, maxB}, {maxB - 1, maxB}, {maxB, maxB}, {maxB + 1, maxB},
		{2 * maxB, maxB}, {2*maxB + 1, maxB}, {maxA - maxA%maxB, maxB}, {maxA - maxA%maxB + 1, maxB},
	}
	checkBatches(t, c, "ceil_div", cases, func(in []int64) *big.Int { return ceilDivWant(in[0], in[1]) })

	p := testPolicy()
	for _, k := range []struct{ missing, score int64 }{{29_999, now0 + 3}, {30_000, now0 + 3}, {30_001, now0 + 4}} {
		t.Run(num(k.missing), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.record(healthy(e.fp))
			e.clock(now0)
			// Last updated at now0: no refill, so u at t is the stored u.
			e.writeKey(encA, capUnits-k.missing, now0, 3, pastX, num(pastX))
			before := e.snap()
			checkReply(t, num(k.missing), e.step(encA), rep(31, now0, "", 0))
			checkState(t, num(k.missing), e.snap(), movedState(t, before, encA, nil, k.score, now0))
		})
	}
}

// markerLatencyFile is where TestAcquire_10kMarkersLatency writes its timings, under the module
// root (plan S7 Tests (a); the file name is a plan choice).
const markerLatencyFile = ".out/marker-latency.json"

// markerLatency is one size's timings in markerLatencyFile: the wall time, as the test sees it
// through the store adapter, of an acquire on a key holding that many markers and of the
// cleanup step that then removes the key.
type markerLatency struct {
	Markers       int   `json:"markers"`
	AcquireUs     int64 `json:"acquire_us"`
	CleanupStepUs int64 `json:"cleanup_step_us"`
}

// TestAcquire_10kMarkersLatency (plan S7, L2; DESIGN §A7 per-key step cost, §C3): on a key
// holding 1,000 and then 10,000 unexpired markers (max_markers_per_key 10,000), an acquire is
// allowed, and once everything has expired one cleanup step removes the key in one reply with
// no partial write, {30, t, "", n}. Both are timed with the scripts loaded, and the timings go
// to .out/marker-latency.json only from a plain run that passed (scriptrun.EvidenceRun, plan S7
// step 12): not from the discipline run, whose instrumented scripts are slower, nor from a
// bin/mutate run of mutated scripts, nor after a failed check.
func TestAcquire_10kMarkersLatency(t *testing.T) {
	p := tenK()
	var points []markerLatency
	for _, n := range []int{1000, 10_000} {
		e := newEnv(t, p)
		e.record(healthy(e.fp).with("nm", num(int64(n))))
		e.clock(now0)
		x := int64(now0 + 30_000_000)
		ms := marks(n, x) // all unexpired at now0, none after x
		e.writeKey(encA, 0, oldT, int64(n), x, num(ms[0].expiry), ms...)
		// Load both scripts first: an acquire of a new key, and a step whose member is gone.
		checkReply(t, "warm acquire", e.acquire(encB), rep(1, now0, "", capUnits-ratePeriod, gen0+1, 1, now0+lifetimeUs))
		checkReply(t, "warm step", e.step(encC), rep(33, now0, "gone", 0))

		start := time.Now()
		got := e.acquire(encA)
		acquireUs := time.Since(start).Microseconds()
		// Full at now0, so one token less; the sequence after n; the receipt expires 60 s on.
		checkReply(t, num(int64(n))+" markers: acquire", got, rep(1, now0, "", capUnits-ratePeriod, gen0, n+1, now0+lifetimeUs))

		t1 := int64(now0 + 100_000_000) // after the new receipt's expiry and every marker's
		e.clock(t1)
		start = time.Now()
		got = e.step(encA)
		stepUs := time.Since(start).Microseconds()
		checkReply(t, num(int64(n))+" markers: cleanup step", got, rep(30, t1, "", n))
		st := e.snap()
		if _, ok := st[kKey(encA)]; ok {
			t.Errorf("%d markers: %s left after the step", n, kKey(encA))
		}
		if _, ok := st[miKey(encA)]; ok {
			t.Errorf("%d markers: %s left after the step", n, miKey(encA))
		}
		points = append(points, markerLatency{Markers: n, AcquireUs: acquireUs, CleanupStepUs: stepUs})
	}
	t.Logf("marker latency: %+v", points)
	if t.Failed() || !scriptrun.EvidenceRun() {
		return
	}
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(struct {
		Points []markerLatency `json:"points"`
	}{points}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(markerLatencyFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
