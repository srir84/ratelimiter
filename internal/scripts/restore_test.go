//go:build scripttest

package scripts_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/scriptrun"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/internal/testutil/storestate"
)

// The S8 admin script tests' values (IMPLEMENTATION_PLAN S8 Tests (a)): the balance most
// restores send, 3 tokens of 10^9 units, and the marker expiries of the counting cases, µs at
// least 10^15 and not round (AGENTS.md §3).
const (
	restoreBalance = 3_000_000_000
	exp1           = hwm0 + 10_000_000
	exp2           = hwm0 + 20_000_000
	exp3           = hwm0 + 30_000_000
	exp4           = hwm0 + 40_000_000
)

// Positions in admin_restore's and admin_clear's ARGV (§A4.1): expected_id, fp, enc, then
// admin_restore's balance_units.
const (
	aArgExpected = 0
	aArgFP       = 1
	aArgEnc      = 2
	aArgBalance  = 3
)

// adminKeys are the KEYS of admin_restore and admin_clear for enc in epochID (§A4.1): epoch,
// ctl, ix, q, k:<enc>, mi:<enc>.
func adminKeys(enc string) []string {
	return []string{keys.EpochKey, keys.CtlTimeKey, ixKey, qKey, kKey(enc), miKey(enc)}
}

// restoreReq is an admin_restore of enc with balance in epochID, with fingerprint fp and the
// test policy's cap_units and receipt lifetime (§A4.1 ARGV: expected_id, fp, enc,
// balance_units, cap_units, lifetime_us).
func restoreReq(fp, enc, balance string) store.Request {
	return store.Request{
		Kind: store.Call, Script: scripts.AdminRestore, Keys: adminKeys(enc),
		Args: []string{epochID, fp, enc, balance, num(capUnits), num(lifetimeUs)},
	}
}

// clearReq is an admin_clear of enc in epochID, with fingerprint fp and the test policy's
// cap_units (§A4.1 ARGV: expected_id, fp, enc, cap_units).
func clearReq(fp, enc string) store.Request {
	return store.Request{
		Kind: store.Call, Script: scripts.AdminClear, Keys: adminKeys(enc),
		Args: []string{epochID, fp, enc, num(capUnits)},
	}
}

// restore runs, as rl_admin, an admin_restore of enc with balance and returns its reply.
func (e *env) restore(enc string, balance int64) []any {
	e.t.Helper()
	return e.run(e.aa, restoreReq(e.fp, enc, num(balance)))
}

// clear runs, as rl_admin, an admin_clear of enc and returns its reply.
func (e *env) clear(enc string) []any {
	e.t.Helper()
	return e.run(e.aa, clearReq(e.fp, enc))
}

// flag is a stored quarantine flag of reason, set at hwm0 (§A3.2: <reason>|<t_us>).
func flag(reason string) string { return reason + "|" + num(hwm0) }

// quarantinedA writes the healthy record, the controlled time now0 and encA's bucket, then
// breaks the key as index_without_bucket (its k deleted, its ix member kept) and flags it: the
// state most admin_restore cases start from. encA's bucket key is declared Damaged.
func (e *env) quarantinedA() {
	e.t.Helper()
	e.base(encA)
	e.do("DEL", kKey(encA))
	e.do("HSET", qKey, encA, flag("index_without_bucket"))
	e.h.Damaged(kKey(encA))
}

// flaggedA writes the healthy record, now0 and encA's bucket, consistent, and flags the key,
// as an operator finds it after admin_restore: the state most admin_clear cases start from.
// Nothing is declared: a refused case declares encA's bucket key, whose q field stays.
func (e *env) flaggedA() {
	e.t.Helper()
	e.base(encA)
	e.do("HSET", qKey, encA, flag("malformed_bucket"))
}

// restored is the state a successful admin_restore of enc at budget time at leaves after
// before (DESIGN §A4.6): k and mi replaced by the bucket u = balance, t = at, g, s = 0, x = at +
// lifetime, mc = 0; enc's ix member at at; the record's gen = g and nb and nm as given; hwm = at
// when later; q unchanged.
func restored(t *testing.T, before storestate.State, enc string, balance, at, g, nb, nm int64) storestate.State {
	t.Helper()
	want := cloneState(before)
	delete(want, miKey(enc))
	want[kKey(enc)] = bucketValue(balance, at, g, 0, at+lifetimeUs, 0)
	setMember(want, ixKey, enc, num(at))
	setField(want, keys.EpochKey, "gen", num(g))
	setField(want, keys.EpochKey, "nb", num(nb))
	setField(want, keys.EpochKey, "nm", num(nm))
	stepHwm(t, want, at)
	return want
}

// cleared is the state a successful admin_clear of enc at budget time at leaves after before
// (DESIGN §A4.6): enc's q field gone, its ix score at, and hwm = at when later.
func cleared(t *testing.T, before storestate.State, enc string, at int64) storestate.State {
	t.Helper()
	want := cloneState(before)
	delMember(want, qKey, enc)
	setMember(want, ixKey, enc, num(at))
	stepHwm(t, want, at)
	return want
}

// held is the state the hold shared_key_malformed leaves after before: the hold only, hwm
// unchanged (§A4.2 hold).
func held(before storestate.State) storestate.State {
	want := cloneState(before)
	setField(want, keys.EpochKey, "hold", "shared_key_malformed")
	return want
}

// fenceCase is one case of the fence admin_restore and admin_clear share (DESIGN §A4.6): a
// setup after the start state, a change to the request, the keys it breaks, and the refusal; a
// nil want means the fence passes and the script succeeds at budget time at.
type fenceCase struct {
	name    string
	setup   func(e *env)
	req     func(r *store.Request)
	damaged []string
	want    []any
	at      int64
}

// fenceCases are the fence's refusals, each with its exact reply, in the order of §A4.6: every
// load_epoch reason (§A4.2), then the expected id, then the fingerprint; the earlier check
// wins each pair, and the fence comes before the KEYS assertion.
func fenceCases(fp string) []fenceCase {
	base := healthy(fp)
	ep := []string{keys.EpochKey}
	otherFP := fingerprint.Of(storeClockPolicy())
	record := func(r rec) func(e *env) { return func(e *env) { e.record(r) } }
	staleID := func(r *store.Request) { r.Args[aArgExpected] = otherID }
	return []fenceCase{
		{name: "record missing", setup: func(e *env) { e.do("DEL", keys.EpochKey) }, want: rep(41, 0, "epoch_record_missing")},
		{name: "record malformed: schema 2", setup: record(base.with("schema", "2")), damaged: ep, want: rep(41, 0, "epoch_record_malformed")},
		{name: "record malformed: nb -1", setup: record(base.with("nb", "-1")), damaged: ep, want: rep(41, 0, "epoch_record_malformed")},
		{name: "rl:epoch a string", setup: func(e *env) { e.do("DEL", keys.EpochKey); e.do("SET", keys.EpochKey, "x") },
			damaged: ep, want: rep(41, 0, "epoch_record_malformed")},
		{name: "hwm missing", setup: record(base.with("hwm", "<del>")), damaged: ep, want: rep(41, 0, "hwm_missing")},
		{name: "hwm x", setup: record(base.with("hwm", "x")), damaged: ep, want: rep(41, 0, "hwm_malformed")},
		{name: "hwm TBOUND", setup: record(base.with("hwm", num(tBound))), damaged: ep, want: rep(41, 0, "hwm_malformed")},
		{name: "hold set", setup: record(base.with("hold", "store_rollback")), want: rep(41, 0, "integrity_hold")},
		{name: "a stale expected id", req: staleID, want: rep(41, 0, "epoch_changed")},
		{name: "another fingerprint", req: func(r *store.Request) { r.Args[aArgFP] = otherFP }, want: rep(41, 0, "fingerprint_mismatch")},
		{name: "a stale id and another fingerprint: the id first", req: func(r *store.Request) {
			r.Args[aArgExpected], r.Args[aArgFP] = otherID, otherFP
		}, want: rep(41, 0, "epoch_changed")},
		{name: "a hold and a stale id: load_epoch first", setup: record(base.with("hold", "store_rollback")), req: staleID,
			want: rep(41, 0, "integrity_hold")},
		// rladmin builds KEYS from the id it read: the fence comes before the KEYS assertion.
		{name: "a stale id, KEYS built from it", req: func(r *store.Request) {
			r.Args[aArgExpected] = otherID
			enc := r.Args[aArgEnc]
			r.Keys = []string{keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(otherID), keys.QuarantineKey(otherID),
				keys.BucketKey(otherID, enc), keys.MarkerIndexKey(otherID, enc)}
		}, want: rep(41, 0, "epoch_changed")},
		{name: "another fingerprint and a wrong k KEYS: the fence first", req: func(r *store.Request) {
			r.Args[aArgFP] = otherFP
			r.Keys[4] = kKey(encB)
		}, want: rep(41, 0, "fingerprint_mismatch")},
		{name: "the expected id and fingerprint pass", at: now0},
	}
}

// keysCases are the KEYS assertion's cases (§A4.1): each per-key or shared KEYS entry not the
// enc's in the record's epoch is {25, 0, 'keys_mismatch'}, also when the enc in ARGV is
// another key's, and the assertion comes right after the fence, before the q and ix reads, the
// quarantine flag, the balance and the clock.
func keysCases() []fenceCase {
	wrongK := func(r *store.Request) { r.Keys[4] = kKey(encB) }
	bad := rep(25, 0, "keys_mismatch")
	return []fenceCase{
		{name: "a wrong k KEYS", req: wrongK, want: bad},
		{name: "a wrong mi KEYS", req: func(r *store.Request) { r.Keys[5] = miKey(encB) }, want: bad},
		{name: "a wrong ix KEYS", req: func(r *store.Request) { r.Keys[2] = keys.IndexKey(otherID) }, want: bad},
		{name: "a wrong q KEYS", req: func(r *store.Request) { r.Keys[3] = keys.QuarantineKey(otherID) }, want: bad},
		{name: "another enc in ARGV", req: func(r *store.Request) { r.Args[aArgEnc] = encB }, want: bad},
		{name: "a wrong k KEYS, ix a string: before the shared reads", setup: func(e *env) {
			e.do("DEL", ixKey)
			e.do("SET", ixKey, "x")
		}, req: wrongK, damaged: []string{ixKey}, want: bad},
		{name: "a wrong k KEYS, q a string: before the shared reads", setup: func(e *env) { e.do("SET", qKey, "x") },
			req: wrongK, damaged: []string{qKey}, want: bad},
		{name: "a wrong k KEYS, not quarantined: before the flag", setup: func(e *env) { e.do("HDEL", qKey, encA) },
			req: wrongK, want: bad},
		{name: "a wrong k KEYS, controlled time missing: before the clock", setup: func(e *env) { e.do("DEL", keys.CtlTimeKey) },
			req: wrongK, want: bad},
	}
}

// runFence runs one fence or KEYS case of script (restore or clear, by req) from the start
// state start writes: the refusal and an unchanged store, or the success want gives.
func runFence(t *testing.T, c fenceCase, start func(e *env), req func(fp string) store.Request,
	success func(t *testing.T, before storestate.State, at int64) storestate.State) {
	t.Helper()
	t.Parallel()
	e := newEnv(t, testPolicy())
	start(e)
	if c.setup != nil {
		c.setup(e)
	}
	e.h.Damaged(c.damaged...)
	r := req(e.fp)
	if c.req != nil {
		c.req(&r)
	}
	before := e.snap()
	got := e.run(e.aa, r)
	if c.want != nil {
		checkReply(t, c.name, got, c.want)
		e.checkUnchanged(c.name, before)
		return
	}
	g := int64(0)
	if r.Script.Name == "admin_restore" {
		g = gen0 + 1
	}
	checkReply(t, c.name, got, rep(40, c.at, "", g))
	checkState(t, c.name, e.snap(), success(t, before, c.at))
}

// TestAdminRestore_FenceEach (plan S8, L2; SPEC ADM-2; DESIGN §A4.6): as rl_admin, each fence
// refusal of admin_restore — every load_epoch reason, a stale expected id, another fingerprint —
// is {41, 0, reason} with nothing written, in the documented order; the fence comes before the
// KEYS assertion; hwm = TBOUND − 1 and the expected id and fingerprint pass.
func TestAdminRestore_FenceEach(t *testing.T) {
	cases := append(fenceCases(fingerprint.Of(testPolicy())), fenceCase{
		name: "hwm TBOUND − 1 passes the fence", setup: func(e *env) { e.record(healthy(e.fp).with("hwm", num(tBound-1))) }, at: tBound - 1,
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runFence(t, c, (*env).quarantinedA, func(fp string) store.Request { return restoreReq(fp, encA, num(restoreBalance)) },
				func(t *testing.T, before storestate.State, at int64) storestate.State {
					// encA had its ix member: nb stays 1; no marker: nm stays 0.
					return restored(t, before, encA, restoreBalance, at, gen0+1, 1, 0)
				})
		})
	}
}

// TestAdminRestore_KeysMismatch (plan S8, L2; DESIGN §A4.1, §A4.6): admin_restore asserts its
// KEYS right after the fence: {25, 0, 'keys_mismatch'} with nothing written, before the q and
// ix reads, the quarantine flag, the balance and the clock.
func TestAdminRestore_KeysMismatch(t *testing.T) {
	cases := append(keysCases(), fenceCase{name: "a wrong k KEYS, the balance out of range: before the balance",
		req: func(r *store.Request) {
			r.Keys[4] = kKey(encB)
			r.Args[aArgBalance] = num(capUnits + 1)
		}, want: rep(25, 0, "keys_mismatch")})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runFence(t, c, (*env).quarantinedA, func(fp string) store.Request { return restoreReq(fp, encA, num(restoreBalance)) }, nil)
		})
	}
}

// TestAdminRestore_NotQuarantined (plan S8, L2; SPEC ADM-2; DESIGN §A4.6): a key with no q field
// — a healthy bucket, an absent key, a broken key not yet flagged, a key while another one is
// flagged — is {41, 0, 'not_quarantined'} with nothing written; the flag is checked before the
// balance and the clock.
func TestAdminRestore_NotQuarantined(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(e *env)
		balance int64
		damaged []string
	}{
		{name: "a healthy bucket", setup: func(e *env) { e.base(encA) }, balance: restoreBalance},
		{name: "an absent key", setup: func(e *env) {
			e.record(healthy(e.fp).with("nb", "0"))
			e.clock(now0)
		}, balance: restoreBalance},
		{name: "a broken key not yet flagged", setup: func(e *env) {
			e.base(encA)
			e.do("DEL", kKey(encA))
		}, balance: restoreBalance, damaged: []string{kKey(encA)}},
		{name: "another key flagged", setup: func(e *env) {
			e.base(encA)
			e.bucketA(encB)
			e.do("HSET", keys.EpochKey, "nb", "2")
			e.do("HSET", qKey, encB, flag("malformed_bucket"))
		}, balance: restoreBalance, damaged: []string{kKey(encB)}},
		{name: "the balance out of range: the flag first", setup: func(e *env) { e.base(encA) }, balance: capUnits + 1},
		{name: "controlled time missing: the flag first", setup: func(e *env) {
			e.base(encA)
			e.do("DEL", keys.CtlTimeKey)
		}, balance: restoreBalance},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			c.setup(e)
			e.h.Damaged(c.damaged...)
			before := e.snap()
			checkReply(t, c.name, e.restore(encA, c.balance), rep(41, 0, "not_quarantined"))
			e.checkUnchanged(c.name, before)
		})
	}
}

// TestAdminRestore_BalanceRange (plan S8, L2; SPEC ADM-2; DESIGN §A4.6): the balance must be a
// canonical decimal (is_uint53, plan choice: a stored "01" would make the bucket malformed) in 0
// … cap_units: cap_units + 1, −1, 1.5, 01, x, "" and 2^53 are {41, 0, 'balance_out_of_range'}
// with nothing written, checked before the clock; 0 and cap_units are restored as they are.
func TestAdminRestore_BalanceRange(t *testing.T) {
	refused := rep(41, 0, "balance_out_of_range")
	cases := []struct {
		name, balance string
		noClock       bool  // the controlled time deleted
		u             int64 // the restored balance, when it is in range
		want          []any
	}{
		{name: "cap_units + 1", balance: num(capUnits + 1), want: refused},
		{name: "-1", balance: "-1", want: refused},
		{name: "1.5", balance: "1.5", want: refused},
		{name: "01", balance: "01", want: refused},
		{name: "x", balance: "x", want: refused},
		{name: "empty", balance: "", want: refused},
		{name: "2^53", balance: num(two53), want: refused},
		{name: "cap_units + 1, controlled time missing: the balance first", balance: num(capUnits + 1), noClock: true, want: refused},
		{name: "cap_units", balance: num(capUnits), u: capUnits},
		{name: "0", balance: "0", u: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			e.quarantinedA()
			if c.noClock {
				e.do("DEL", keys.CtlTimeKey)
			}
			before := e.snap()
			got := e.run(e.aa, restoreReq(e.fp, encA, c.balance))
			if c.want != nil {
				checkReply(t, c.name, got, c.want)
				e.checkUnchanged(c.name, before)
				return
			}
			checkReply(t, c.name, got, rep(40, now0, "", gen0+1))
			checkState(t, c.name, e.snap(), restored(t, before, encA, c.u, now0, gen0+1, 1, 0))
		})
	}
}

// TestAdminRestore_Success (plan S8, L2; SPEC ADM-2; DESIGN §A4.6, §D2 decision 5): a flagged
// key with a malformed bucket, two markers and their mi members gets the new bucket u =
// balance, t, g = gen + 1, s = 0, x = t + lifetime, mc = 0; mi is gone, the ix score is t, nm
// loses the two markers, nb stays (the key had its ix member), hwm = t, and the q flag stays,
// so an acquire is still refused 'quarantined'; another key is untouched. t is max(clock, hwm):
// hwm when the controlled time is behind it, and the store's TIME under the store clock.
func TestAdminRestore_Success(t *testing.T) {
	t.Run("a malformed bucket with two markers", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, testPolicy())
		e.base(encA)
		e.bucketA(encB)
		e.hset(kKey(encA), "u", "x", "t", num(bucketT), "g", num(gen0), "s", num(bucketS), "x", num(bucketX), "mc", "2",
			"m:7:1", num(exp1)+":"+num(mCredit), "m:7:2", num(exp2)+":"+num(mCredit))
		e.do("ZADD", miKey(encA), num(exp1), "7:1", num(exp2), "7:2")
		e.do("HSET", keys.EpochKey, "nb", "2", "nm", "2")
		e.do("HSET", qKey, encA, flag("malformed_bucket"))
		e.h.Damaged(kKey(encA))
		before := e.snap()
		checkReply(t, "restore", e.restore(encA, restoreBalance), rep(40, now0, "", gen0+1))
		want := restored(t, before, encA, restoreBalance, now0, gen0+1, 2, 0)
		if got := want[qKey].Fields[encA]; got != flag("malformed_bucket") {
			t.Fatalf("the expected state lost the flag: %q", got)
		}
		checkState(t, "restore", e.snap(), want)
		checkReply(t, "acquire after the restore", e.acquire(encA), rep(23, now0, "quarantined"))
	})
	t.Run("the controlled time behind hwm: t is hwm", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, testPolicy())
		e.quarantinedA()
		e.clock(hwm0 - 5_000_000)
		before := e.snap()
		checkReply(t, "restore", e.restore(encA, restoreBalance), rep(40, hwm0, "", gen0+1))
		checkState(t, "restore", e.snap(), restored(t, before, encA, restoreBalance, hwm0, gen0+1, 1, 0))
	})
	t.Run("the store's clock", func(t *testing.T) {
		t.Parallel()
		sp := storeClockPolicy()
		e := newEnv(t, sp)
		e.record(healthy(e.fp).with("clock", "store"))
		e.bucketA(encA)
		e.do("DEL", kKey(encA))
		e.do("HSET", qKey, encA, flag("index_without_bucket"))
		e.h.Damaged(kKey(encA))
		before := e.snap()
		lo := e.storeTime()
		got := e.restore(encA, restoreBalance)
		hi := e.storeTime()
		at, ok := got[1].(int64)
		if !ok || at < lo || at > hi {
			t.Fatalf("t_us %v, want the store's clock, in [%d, %d]", got[1], lo, hi)
		}
		checkReply(t, "restore", got, rep(40, at, "", gen0+1))
		checkState(t, "restore", e.snap(), restored(t, before, encA, restoreBalance, at, gen0+1, 1, 0))
	})
}

// TestAdminRestore_CountsAndNb (plan S8, L2; DESIGN §A4.6, §A3.3): the markers taken off nm are
// max(m:* fields of k, ZCARD mi), each 0 when its key is of the wrong type, and nm stops at 0
// (dec_counter); nb gains 1 exactly when the key had no ix member. Every case restores the
// key; the expected counters are worked out by hand.
func TestAdminRestore_CountsAndNb(t *testing.T) {
	marker := func(exp int64) string { return num(exp) + ":" + num(mCredit) }
	bucket := []string{"u", num(bucketU), "t", num(bucketT), "g", num(gen0), "s", num(bucketS), "x", num(bucketX)}
	cases := []struct {
		name          string
		nb, nm        int64 // the record's counters before
		setup         func(e *env)
		wantNb, wantM int64
	}{
		{name: "3 marker fields, 2 mi members: 3", nb: 1, nm: 5, setup: func(e *env) {
			e.hset(kKey(encA), append(bucket, "mc", "3", "m:7:1", marker(exp1), "m:7:2", marker(exp2), "m:7:3", marker(exp3))...)
			e.do("ZADD", miKey(encA), num(exp1), "7:1", num(exp2), "7:2")
		}, wantNb: 1, wantM: 2},
		{name: "1 marker field, 4 mi members: 4", nb: 1, nm: 5, setup: func(e *env) {
			e.hset(kKey(encA), append(bucket, "mc", "1", "m:7:1", marker(exp1))...)
			e.do("ZADD", miKey(encA), num(exp1), "7:1", num(exp2), "7:2", num(exp3), "7:3", num(exp4), "7:4")
		}, wantNb: 1, wantM: 1},
		{name: "k a string, 2 mi members: 2", nb: 1, nm: 5, setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("SET", kKey(encA), "x")
			e.do("ZADD", miKey(encA), num(exp1), "7:1", num(exp2), "7:2")
		}, wantNb: 1, wantM: 3},
		{name: "2 marker fields, mi a string: 2", nb: 1, nm: 5, setup: func(e *env) {
			e.hset(kKey(encA), append(bucket, "mc", "2", "m:7:1", marker(exp1), "m:7:2", marker(exp2))...)
			e.do("SET", miKey(encA), "x")
		}, wantNb: 1, wantM: 3},
		{name: "k and mi strings: 0", nb: 1, nm: 5, setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("SET", kKey(encA), "x")
			e.do("SET", miKey(encA), "x")
		}, wantNb: 1, wantM: 5},
		{name: "marker fields only: 2", nb: 1, nm: 5, setup: func(e *env) {
			e.hset(kKey(encA), "m:7:1", marker(exp1), "m:7:2", marker(exp2))
		}, wantNb: 1, wantM: 3},
		{name: "3 markers, nm 1: nm stops at 0", nb: 1, nm: 1, setup: func(e *env) {
			e.hset(kKey(encA), append(bucket, "mc", "3", "m:7:1", marker(exp1), "m:7:2", marker(exp2), "m:7:3", marker(exp3))...)
			e.do("ZADD", miKey(encA), num(exp1), "7:1", num(exp2), "7:2", num(exp3), "7:3")
		}, wantNb: 1, wantM: 0},
		{name: "no ix member, one mi member: nb + 1", nb: 1, nm: 1, setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
			e.do("ZADD", miKey(encA), num(exp1), "7:1")
		}, wantNb: 2, wantM: 0},
		{name: "only the flag: nb + 1, count 0", nb: 1, nm: 0, setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZREM", ixKey, encA)
		}, wantNb: 2, wantM: 0},
		{name: "its ix member, no bucket: nb stays", nb: 1, nm: 0, setup: func(e *env) { e.do("DEL", kKey(encA)) }, wantNb: 1, wantM: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			e.base(encA)
			e.do("HSET", keys.EpochKey, "nb", num(c.nb), "nm", num(c.nm))
			c.setup(e)
			e.do("HSET", qKey, encA, flag("malformed_marker"))
			e.h.Damaged(kKey(encA), miKey(encA))
			before := e.snap()
			checkReply(t, c.name, e.restore(encA, restoreBalance), rep(40, now0, "", gen0+1))
			checkState(t, c.name, e.snap(), restored(t, before, encA, restoreBalance, now0, gen0+1, c.wantNb, c.wantM))
		})
	}
}

// TestAdminRestore_WrongTypeIxQ (plan S8, L2; DESIGN §A4.6, §A4.8 rule 2; SPEC STA-9): an ix or a
// q of the wrong type makes the first reads fail: the hold shared_key_malformed, {22, 0,
// 'integrity_hold'}, with only the hold written (hwm not advanced); the reads come before the
// quarantine flag, the balance and the clock.
func TestAdminRestore_WrongTypeIxQ(t *testing.T) {
	ixString := func(e *env) {
		e.do("DEL", ixKey)
		e.do("SET", ixKey, "x")
	}
	qString := func(e *env) { e.do("SET", qKey, "x") }
	cases := []struct {
		name    string
		setup   func(e *env)
		balance int64
		damaged []string
	}{
		{name: "ix a string", setup: ixString, balance: restoreBalance, damaged: []string{ixKey}},
		{name: "q a string", setup: qString, balance: restoreBalance, damaged: []string{qKey}},
		{name: "ix a string, not quarantined: the reads first", setup: func(e *env) {
			e.do("HDEL", qKey, encA)
			ixString(e)
		}, balance: restoreBalance, damaged: []string{ixKey}},
		{name: "q a string, the balance out of range: the reads first", setup: qString, balance: capUnits + 1, damaged: []string{qKey}},
		{name: "ix a string, controlled time missing: the reads first", setup: func(e *env) {
			ixString(e)
			e.do("DEL", keys.CtlTimeKey)
		}, balance: restoreBalance, damaged: []string{ixKey}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			e.quarantinedA()
			c.setup(e)
			e.h.Damaged(c.damaged...)
			before := e.snap()
			checkReply(t, c.name, e.restore(encA, c.balance), rep(22, 0, "integrity_hold"))
			checkState(t, c.name, e.snap(), held(before))
		})
	}
}

// TestAdminRestore_UnreadableKMi (plan S8, L2; DESIGN §A4.6): a k or mi of the wrong type cannot
// be read but is deleted all the same (DEL works on any type): the restore succeeds, counting 0
// markers for each unreadable key, and writes the new bucket.
func TestAdminRestore_UnreadableKMi(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(e *env)
		wantNb int64
	}{
		{name: "k a string", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("SET", kKey(encA), "x")
		}, wantNb: 1},
		{name: "mi a string", setup: func(e *env) { e.do("SET", miKey(encA), "x") }, wantNb: 1},
		{name: "k a sorted set, mi a hash", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("ZADD", kKey(encA), "1", "m:7:1")
			e.do("HSET", miKey(encA), "7:1", num(exp1))
		}, wantNb: 1},
		{name: "k and mi strings, no ix member: nb + 1", setup: func(e *env) {
			e.do("DEL", kKey(encA))
			e.do("SET", kKey(encA), "x")
			e.do("SET", miKey(encA), "x")
			e.do("ZREM", ixKey, encA)
		}, wantNb: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			e.base(encA)
			e.do("HSET", keys.EpochKey, "nm", "3") // unchanged: no marker counted
			c.setup(e)
			e.do("HSET", qKey, encA, flag("malformed_bucket"))
			e.h.Damaged(kKey(encA), miKey(encA))
			before := e.snap()
			checkReply(t, c.name, e.restore(encA, restoreBalance), rep(40, now0, "", gen0+1))
			checkState(t, c.name, e.snap(), restored(t, before, encA, restoreBalance, now0, gen0+1, c.wantNb, 3))
		})
	}
}

// TestAdminRestore_Clock (plan S8, L2; SPEC CLK-8; DESIGN §A4.6, §A4.2 read_clock): a controlled
// time that is missing, not is_uint53 (abc, -5) or at TBOUND is {21, 0, 'clock'} with nothing
// written; TBOUND − 1 is a budget time, restored at it.
func TestAdminRestore_Clock(t *testing.T) {
	cases := []struct {
		name, ctl string // "<del>" deletes the controlled time
		want      []any
	}{
		{name: "missing", ctl: "<del>", want: rep(21, 0, "clock")},
		{name: "abc", ctl: "abc", want: rep(21, 0, "clock")},
		{name: "-5", ctl: "-5", want: rep(21, 0, "clock")},
		{name: "TBOUND", ctl: num(tBound), want: rep(21, 0, "clock")},
		{name: "TBOUND − 1", ctl: num(tBound - 1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			e.quarantinedA()
			if c.ctl == "<del>" {
				e.do("DEL", keys.CtlTimeKey)
			} else {
				e.do("SET", keys.CtlTimeKey, c.ctl)
			}
			before := e.snap()
			got := e.restore(encA, restoreBalance)
			if c.want != nil {
				checkReply(t, c.name, got, c.want)
				e.checkUnchanged(c.name, before)
				return
			}
			checkReply(t, c.name, got, rep(40, tBound-1, "", gen0+1))
			checkState(t, c.name, e.snap(), restored(t, before, encA, restoreBalance, tBound-1, gen0+1, 1, 0))
		})
	}
}

// TestAdminClear_Fence (plan S8, L2; SPEC ADM-3; DESIGN §A4.6, §A4.1): admin_clear's fence, as
// admin_restore's (incl. fp): each refusal {41, 0, reason} with nothing written, in order; then
// the KEYS assertion, {25, 0, 'keys_mismatch'} with nothing written, before the q read, the
// quarantine flag, check_key and the clock.
func TestAdminClear_Fence(t *testing.T) {
	encAKey := []string{kKey(encA)} // its q field stays on every refusal
	cases := fenceCases(fingerprint.Of(testPolicy()))
	for _, c := range keysCases() {
		c.name = "KEYS: " + c.name
		cases = append(cases, c)
	}
	cases = append(cases, fenceCase{name: "KEYS: a wrong k KEYS, the bucket missing: before check_key",
		setup: func(e *env) { e.do("DEL", kKey(encA)) }, req: func(r *store.Request) { r.Keys[4] = kKey(encB) },
		want: rep(25, 0, "keys_mismatch")})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.want != nil {
				c.damaged = append(c.damaged, encAKey...)
			}
			runFence(t, c, (*env).flaggedA, func(fp string) store.Request { return clearReq(fp, encA) },
				func(t *testing.T, before storestate.State, at int64) storestate.State {
					return cleared(t, before, encA, at)
				})
		})
	}
}

// TestAdminClear_NotQuarantined (plan S8, L2; SPEC ADM-3; DESIGN §A4.6): a key with no q field —
// a healthy bucket, an absent key (the flag before check_key), a broken key not yet flagged,
// a key while another one is flagged — is {41, 0, 'not_quarantined'} with nothing written, also
// when the controlled time is missing (the clock comes last).
func TestAdminClear_NotQuarantined(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(e *env)
		damaged []string
	}{
		{name: "a healthy bucket", setup: func(e *env) { e.base(encA) }},
		{name: "an absent key", setup: func(e *env) {
			e.record(healthy(e.fp).with("nb", "0"))
			e.clock(now0)
		}},
		{name: "a broken key not yet flagged", setup: func(e *env) {
			e.base(encA)
			e.do("HSET", kKey(encA), "u", "x")
		}, damaged: []string{kKey(encA)}},
		{name: "another key flagged", setup: func(e *env) {
			e.base(encA)
			e.bucketA(encB)
			e.do("HSET", keys.EpochKey, "nb", "2")
			e.do("HSET", qKey, encB, flag("malformed_bucket"))
		}, damaged: []string{kKey(encB)}},
		{name: "controlled time missing: the flag first", setup: func(e *env) {
			e.base(encA)
			e.do("DEL", keys.CtlTimeKey)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			c.setup(e)
			e.h.Damaged(c.damaged...)
			before := e.snap()
			checkReply(t, c.name, e.clear(encA), rep(41, 0, "not_quarantined"))
			e.checkUnchanged(c.name, before)
		})
	}
}

// TestAdminClear_EachCheckKeyRow (plan S8, L2; SPEC ADM-3, ADM-2; DESIGN §A4.6, §A3.3): for each
// §A3.3 row a flagged key can be in (rows 2 to 11, as checkKeyRows breaks them), admin_clear
// refuses with the row's reason, {41, 0, reason}, and writes nothing; a shared row (ix or q of
// the wrong type) sets the hold shared_key_malformed and writes only it. Each refused key is
// then recovered as an operator would: admin_restore (3 tokens, nb + 1 when the row left no ix
// member, nm stopping at 0) and admin_clear, which now succeeds.
func TestAdminClear_EachCheckKeyRow(t *testing.T) {
	for _, row := range checkKeyRows() {
		if row.absent || row.ok {
			continue // row 1: TestAdminClear_Absent; row 12: TestAdminClear_Success
		}
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			row.start(e, "admin_clear")
			before := e.snap()
			got := e.clear(encA)
			if row.shared {
				checkReply(t, "clear", got, rep(22, 0, "integrity_hold"))
				checkState(t, "clear", e.snap(), held(before))
				return
			}
			checkReply(t, "clear", got, rep(41, 0, row.reason))
			e.checkUnchanged("clear", before)
			nb := int64(1)
			if row.noIx {
				nb = 2
			}
			checkReply(t, "restore", e.restore(encA, restoreBalance), rep(40, now0, "", gen0+1))
			afterRestore := restored(t, before, encA, restoreBalance, now0, gen0+1, nb, 0)
			checkState(t, "restore", e.snap(), afterRestore)
			checkReply(t, "clear after the restore", e.clear(encA), rep(40, now0, "", 0))
			checkState(t, "clear after the restore", e.snap(), cleared(t, afterRestore, encA, now0))
		})
	}
}

// TestAdminClear_Absent (plan S8, L2; SPEC ADM-3; DESIGN §A4.6): a flagged key with no k, no mi
// and no ix member (check_key absent) is {41, 0, 'bucket_missing'} with nothing written: ADM-3
// needs a bucket; check_key comes before the clock.
func TestAdminClear_Absent(t *testing.T) {
	for _, noClock := range []bool{false, true} {
		t.Run(fmt.Sprintf("controlled time missing %v", noClock), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			e.record(healthy(e.fp))
			e.clock(now0)
			e.do("HSET", qKey, encA, flag("index_without_bucket"))
			if noClock {
				e.do("DEL", keys.CtlTimeKey)
			}
			e.h.Damaged(kKey(encA))
			before := e.snap()
			checkReply(t, "clear", e.clear(encA), rep(41, 0, "bucket_missing"))
			e.checkUnchanged("clear", before)
		})
	}
}

// TestAdminClear_Success (plan S8, L2; SPEC ADM-3; DESIGN §A4.6, §A4.5): a flagged key whose
// bucket and entries are consistent — its ix score pushed as a quarantine leaves it, or two
// markers with their mi members — is cleared: {40, t, "", 0}, the q field gone, the ix score t
// (ZADD XX, so the next pass reconsiders the key at once), hwm = t; nothing else changes, and an
// acquire is allowed again. t is max(clock, hwm), the store's TIME under the store clock; a
// controlled time missing or at TBOUND is {21, 0, 'clock'} with nothing written.
func TestAdminClear_Success(t *testing.T) {
	t.Run("its ix score pushed", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, testPolicy())
		e.flaggedA()
		e.do("ZADD", ixKey, num(bucketX+lifetimeUs), encA)
		before := e.snap()
		checkReply(t, "clear", e.clear(encA), rep(40, now0, "", 0))
		want := cleared(t, before, encA, now0)
		if _, ok := want[qKey]; ok {
			t.Fatalf("the expected state kept q: %v", want[qKey])
		}
		checkState(t, "clear", e.snap(), want)
		// 5 tokens at bucketT, refilled for 1.1 s at 10 tokens/s: 16 tokens, less one.
		checkReply(t, "acquire after the clear", e.acquire(encA), rep(1, now0, "", 15_000_000_000, gen0, bucketS+1, now0+lifetimeUs))
	})
	t.Run("two markers", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, testPolicy())
		e.flaggedA()
		e.do("HSET", kKey(encA), "mc", "2", "m:7:1", num(exp1)+":"+num(mCredit), "m:7:2", num(exp2)+":"+num(mCredit))
		e.do("ZADD", miKey(encA), num(exp1), "7:1", num(exp2), "7:2")
		e.do("HSET", keys.EpochKey, "nm", "2")
		before := e.snap()
		checkReply(t, "clear", e.clear(encA), rep(40, now0, "", 0))
		checkState(t, "clear", e.snap(), cleared(t, before, encA, now0))
	})
	t.Run("the controlled time behind hwm: t is hwm", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, testPolicy())
		e.flaggedA()
		e.clock(hwm0 - 5_000_000)
		before := e.snap()
		checkReply(t, "clear", e.clear(encA), rep(40, hwm0, "", 0))
		checkState(t, "clear", e.snap(), cleared(t, before, encA, hwm0))
	})
	t.Run("the store's clock", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, storeClockPolicy())
		// bucketA's state, flagged, with hwm0 replaced by a mark 1 s behind the store's clock:
		// the clear's time, the store's TIME, is past the mark, and before the bucket's
		// eligibility x = mark + 59 s, so its new ix score passes the teardown audit with no
		// key declared.
		mark := e.storeTime() - 1_000_000
		e.record(healthy(e.fp).with("clock", "store", "hwm", num(mark)))
		e.hset(kKey(encA), "u", num(bucketU), "t", num(mark-1_000_000), "g", num(gen0), "s", num(bucketS),
			"x", num(mark+59_000_000), "mc", "0")
		e.do("ZADD", ixKey, num(mark+59_000_000), encA)
		e.do("HSET", qKey, encA, "malformed_bucket|"+num(mark))
		before := e.snap()
		lo := e.storeTime()
		got := e.clear(encA)
		hi := e.storeTime()
		at, ok := got[1].(int64)
		if !ok || at < lo || at > hi {
			t.Fatalf("t_us %v, want the store's clock, in [%d, %d]", got[1], lo, hi)
		}
		checkReply(t, "clear", got, rep(40, at, "", 0))
		checkState(t, "clear", e.snap(), cleared(t, before, encA, at))
	})
	for _, ctl := range []string{"<del>", num(tBound)} {
		t.Run("controlled time "+ctl, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testPolicy())
			e.flaggedA()
			if ctl == "<del>" {
				e.do("DEL", keys.CtlTimeKey)
			} else {
				e.do("SET", keys.CtlTimeKey, ctl)
			}
			e.h.Damaged(kKey(encA))
			before := e.snap()
			checkReply(t, "clear", e.clear(encA), rep(21, 0, "clock"))
			e.checkUnchanged("clear", before)
		})
	}
}

// TestAdminOOM (plan S8, L2; DESIGN §A4.8 rule 1, §C2 bullet 2): with maxmemory set to 1 byte by
// rl_ops under noeviction, the store refuses acquire (plain #!lua) before it runs —
// capacity_error with an OOM store error, the store unchanged — while the allow-oom scripts
// admin_restore and admin_clear run and write: a flagged key is restored, then cleared.
// maxmemory is restored afterwards.
func TestAdminOOM(t *testing.T) {
	p := testPolicy()
	e := newEnv(t, p)
	e.quarantinedA()
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
	mustDo(t, ops, "CONFIG", "SET", "maxmemory", "1")
	used, err := strconv.ParseInt(storeproc.ParseInfo(mustDo(t, ops, "INFO", "memory").Str)["used_memory"], 10, 64)
	if err != nil || used <= 1 {
		t.Fatalf("used_memory %d (%v), want above the 1-byte maxmemory", used, err)
	}

	before := e.snap()
	r := scriptrun.Run(t, e.la, acquireReq(p, e.fp, encB))
	if r.Outcome != store.CapacityError || !strings.HasPrefix(r.StoreError, "OOM") {
		t.Errorf("acquire under OOM: outcome %v, store error %q; want capacity_error with an OOM store error", r.Outcome, r.StoreError)
	}
	e.checkUnchanged("acquire under OOM", before)

	checkReply(t, "admin_restore under OOM", e.restore(encA, restoreBalance), rep(40, now0, "", gen0+1))
	afterRestore := restored(t, before, encA, restoreBalance, now0, gen0+1, 1, 0)
	checkState(t, "admin_restore under OOM", e.snap(), afterRestore)
	checkReply(t, "admin_clear under OOM", e.clear(encA), rep(40, now0, "", 0))
	checkState(t, "admin_clear under OOM", e.snap(), cleared(t, afterRestore, encA, now0))

	restore()
	if got := mustDo(t, ops, "CONFIG", "GET", "maxmemory"); len(got.Arr) != 2 || got.Arr[1].Str != old.Arr[1].Str {
		t.Errorf("maxmemory after the test: %s, want %s", got, old.Arr[1].Str)
	}
}

// TestRestoreThenOldRefund (plan S8, L2; DESIGN §C3 bullet 6, §A4.5, §A4.6; SPEC ADM-2, RCP-2):
// a key quarantined by an acquire (its bucket deleted) is restored with a full balance at t1,
// cleared at t2, and a pass at t3 moves its entry to the restored x = t1 + lifetime; the refund
// at t4 of a receipt from before the restore, (gen0, 2), still unexpired, is receipt_invalid
// (generation_or_sequence), writes only hwm and quarantines nothing. A pass 1 µs before t1 +
// lifetime finds nothing due; the pass at t1 + lifetime removes the bucket (nb back to 0). Every
// state is worked out by hand; at the end the store holds no entry of the key.
func TestRestoreThenOldRefund(t *testing.T) {
	const (
		t1 = now0 + 1_000_000 // the restore
		t2 = t1 + 1_000_000   // the clear
		t3 = t2 + 1_000_000   // the first pass
		t4 = t3 + 1_000_000   // the old refund: hwm0 + 4.1 s, before rExp2 = hwm0 + 49 s
		t5 = t1 + lifetimeUs  // the restored x, the bucket's eligibility (§A4.6, §D2 decision 5)
		g1 = gen0 + 1         // the restored generation
	)
	if t4 >= rExp2 {
		t.Fatalf("t4 %d is not before the old receipt's expiry %d", t4, rExp2)
	}
	e := newEnv(t, testPolicy())
	// atTime sets the controlled time to at, in the store and in want (when not nil).
	atTime := func(want storestate.State, at int64) {
		t.Helper()
		e.clock(at)
		if want != nil {
			want[keys.CtlTimeKey] = storestate.Value{Type: storestate.String, Str: num(at)}
		}
	}
	e.base(encA)
	e.do("DEL", kKey(encA))

	checkReply(t, "acquire", e.acquire(encA), rep(23, now0, "index_without_bucket"))
	before := e.snap()
	if before[qKey].Fields[encA] != "index_without_bucket|"+num(now0) {
		t.Fatalf("the acquire did not quarantine the key: q %v", before[qKey].Fields)
	}
	atTime(before, t1)
	checkReply(t, "restore", e.restore(encA, capUnits), rep(40, t1, "", g1))
	want := restored(t, before, encA, capUnits, t1, g1, 1, 0)
	checkState(t, "restore", e.snap(), want)

	atTime(want, t2)
	checkReply(t, "clear", e.clear(encA), rep(40, t2, "", 0))
	want = cleared(t, want, encA, t2)
	checkState(t, "clear", e.snap(), want)

	// The pass at t3: the page holds the key (score t2), whose full bucket is not eligible
	// before x = t5: moved there; then the empty page (oldest 0).
	atTime(want, t3)
	checkReply(t, "pass at t3: due_entries", e.run(e.la, dueReq(e.fp, 1000, 0)), rep(0, t3, "", t2, encA))
	checkReply(t, "pass at t3: cleanup_entry", e.step(encA), rep(31, t3, "", 0))
	checkReply(t, "pass at t3: the next page", e.run(e.la, dueReq(e.fp, 1000, t3)), rep(0, t3, "", 0))
	want = movedState(t, want, encA, nil, t5, t3)
	checkState(t, "pass at t3", e.snap(), want)

	atTime(want, t4)
	checkReply(t, "old refund", e.refundOf(encA, 2, rExp2), rep(10, t4, "generation_or_sequence"))
	setField(want, keys.EpochKey, "hwm", num(t4))
	checkState(t, "old refund", e.snap(), want)
	if q, ok := e.snap()[qKey]; ok {
		t.Fatalf("the old refund quarantined a key: q %v", q.Fields)
	}

	atTime(nil, t5-1)
	checkReply(t, "pass 1 µs before x", e.run(e.la, dueReq(e.fp, 1000, 0)), rep(0, t5-1, "", 0))
	atTime(nil, t5)
	checkReply(t, "pass at x: due_entries", e.run(e.la, dueReq(e.fp, 1000, 0)), rep(0, t5, "", t5, encA))
	checkReply(t, "pass at x: cleanup_entry", e.step(encA), rep(30, t5, "", 0))
	checkReply(t, "pass at x: the next page", e.run(e.la, dueReq(e.fp, 1000, t5)), rep(0, t5, "", 0))
	final := storestate.State{
		keys.EpochKey: {Type: storestate.Hash, Fields: map[string]string{
			"id": epochID, "fp": e.fp, "schema": "1", "clock": "controlled", "hold": "",
			"hwm": num(t5), "origin": num(origin0), "created": num(origin0), "gen": num(g1), "nb": "0", "nm": "0",
		}},
		keys.CtlTimeKey: {Type: storestate.String, Str: num(t5)},
	}
	checkState(t, "pass at x", e.snap(), final)
}
