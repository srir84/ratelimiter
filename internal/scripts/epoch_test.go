//go:build scripttest

package scripts_test

import (
	"strconv"
	"testing"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/storestate"
)

// storeTime reads the store's clock as rl_lib (TIME is in its ACL list), in µs.
func (e *env) storeTime() int64 {
	e.t.Helper()
	r := mustDo(e.t, e.lib, "TIME")
	if r.Kind != '*' || len(r.Arr) != 2 {
		e.t.Fatalf("TIME: %s", r)
	}
	s, err1 := strconv.ParseInt(r.Arr[0].Str, 10, 64)
	us, err2 := strconv.ParseInt(r.Arr[1].Str, 10, 64)
	if err1 != nil || err2 != nil {
		e.t.Fatalf("TIME: %s", r)
	}
	return s*1_000_000 + us
}

// storeClock marks the expected clock_us of a read_epoch reply read from the store's TIME.
const storeClock = "<the store's clock>"

// readEpochReq is a read_epoch call with the given q key (§A4.1: epoch, ctl, q).
func readEpochReq(q string) store.Request {
	return store.Request{Kind: store.Call, Script: scripts.ReadEpoch, Keys: []string{keys.EpochKey, keys.CtlTimeKey, q}}
}

// TestReadEpoch (plan S4, L2; DESIGN §A4.9): read_epoch as rl_lib and as rl_admin. The
// state and reason follow load_epoch; id and fp come back whenever they validate, also
// under a hold and with hwm missing; quarantined only for the current epoch's q; clock_us
// is the controlled time or the store's clock (0 when unreadable); the store never changes.
func TestReadEpoch(t *testing.T) {
	p, sp := testPolicy(), storeClockPolicy()
	fp, sfp := fingerprint.Of(p), fingerprint.Of(sp)
	base := healthy(fp).with("nb", "0")
	cases := []struct {
		name    string
		setup   func(e *env)
		q       string // KEYS[3]; "" for the current epoch's q
		damaged []string
		want    []any
	}{
		{name: "healthy", setup: func(e *env) { e.record(base) },
			want: rep(0, 0, "", epochID, fp, hwm0, gen0, now0, 0, 0, 0)},
		{name: "two quarantined keys", setup: func(e *env) {
			e.record(base)
			e.do("HSET", qKey, encB, "malformed_bucket|"+num(hwm0), encC, "marker_count|"+num(hwm0))
		}, damaged: []string{kKey(encB), kKey(encC)}, want: rep(0, 0, "", epochID, fp, hwm0, gen0, now0, 0, 0, 2)},
		{name: "KEYS[3] the placeholder", setup: func(e *env) {
			e.record(base)
			e.do("HSET", qKey, encB, "malformed_bucket|"+num(hwm0))
		}, q: keys.QuarantinePlaceholder, damaged: []string{kKey(encB)}, want: rep(0, 0, "", epochID, fp, hwm0, gen0, now0, 0, 0, "")},
		{name: "KEYS[3] another epoch's q", setup: func(e *env) {
			e.record(base)
			e.do("HSET", keys.QuarantineKey(otherID), encB, "malformed_bucket|"+num(hwm0))
		}, q: keys.QuarantineKey(otherID), want: rep(0, 0, "", epochID, fp, hwm0, gen0, now0, 0, 0, "")},
		{name: "q a string", setup: func(e *env) { e.record(base); e.do("SET", qKey, "x") },
			damaged: []string{qKey}, want: rep(0, 0, "", epochID, fp, hwm0, gen0, now0, 0, 0, "")},
		{name: "controlled time missing", setup: func(e *env) { e.record(base); e.do("DEL", keys.CtlTimeKey) },
			want: rep(0, 0, "", epochID, fp, hwm0, gen0, 0, 0, 0, 0)},
		{name: "controlled time TBOUND", setup: func(e *env) { e.record(base); e.clock(tBound) },
			want: rep(0, 0, "", epochID, fp, hwm0, gen0, 0, 0, 0, 0)},
		{name: "store clock", setup: func(e *env) { e.record(base.with("clock", "store", "fp", sfp)) },
			want: rep(0, 0, "", epochID, sfp, hwm0, gen0, storeClock, 0, 0, 0)},
		{name: "record missing", setup: func(*env) {},
			want: rep(22, 0, "epoch_record_missing", "", "", 0, 0, storeClock, 0, 0, "")},
		{name: "record a string", setup: func(e *env) { e.do("SET", keys.EpochKey, "x") }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "epoch_record_malformed", "", "", 0, 0, storeClock, 0, 0, "")},
		{name: "schema 2", setup: func(e *env) { e.record(base.with("schema", "2", "nb", "5")) }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "epoch_record_malformed", epochID, fp, hwm0, gen0, now0, 5, 0, 0)},
		{name: "id malformed", setup: func(e *env) { e.record(base.with("id", "9C56CC51B374C3BA189210D5B6D4BF57")) }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "epoch_record_malformed", "", fp, hwm0, gen0, now0, 0, 0, "")},
		{name: "fp malformed", setup: func(e *env) { e.record(base.with("fp", "xyz")) }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "epoch_record_malformed", epochID, "", hwm0, gen0, now0, 0, 0, 0)},
		{name: "gen malformed", setup: func(e *env) { e.record(base.with("gen", "x", "nm", "2")) }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "epoch_record_malformed", epochID, fp, hwm0, 0, now0, 0, 2, 0)},
		{name: "gen with a leading zero", setup: func(e *env) { e.record(base.with("gen", "01")) }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "epoch_record_malformed", epochID, fp, hwm0, 0, now0, 0, 0, 0)},
		{name: "hwm missing", setup: func(e *env) { e.record(base.with("hwm", "<del>")) }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "hwm_missing", epochID, fp, 0, gen0, now0, 0, 0, 0)},
		{name: "hwm x", setup: func(e *env) { e.record(base.with("hwm", "x")) }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "hwm_malformed", epochID, fp, 0, gen0, now0, 0, 0, 0)},
		{name: "hwm TBOUND", setup: func(e *env) { e.record(base.with("hwm", num(tBound))) }, damaged: []string{keys.EpochKey},
			want: rep(22, 0, "hwm_malformed", epochID, fp, 0, gen0, now0, 0, 0, 0)},
		{name: "hold set", setup: func(e *env) { e.record(base.with("hold", "store_rollback")) },
			want: rep(22, 0, "integrity_hold", epochID, fp, hwm0, gen0, now0, 0, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p, sp)
			e.clock(now0)
			c.setup(e)
			e.h.Damaged(c.damaged...)
			q := c.q
			if q == "" {
				q = qKey
			}
			before := e.snap()
			for _, a := range []struct {
				user string
				a    *store.StoreAdapter
			}{{"rl_lib", e.la}, {"rl_admin", e.aa}} {
				lo := e.storeTime()
				got := e.run(a.a, readEpochReq(q))
				hi := e.storeTime()
				want := append([]any(nil), c.want...)
				if want[7] == storeClock {
					if v, ok := got[7].(int64); ok && v >= lo && v <= hi {
						want[7] = v
					} else {
						t.Errorf("%s: clock_us %v, want the store's clock, in [%d, %d]", a.user, got[7], lo, hi)
					}
				}
				checkReply(t, a.user, got, want)
			}
			e.checkUnchanged(c.name, before)
		})
	}
}

// holdReq is a hold call for the expected epoch id with reason.
func holdReq(id, reason string) store.Request {
	return store.Request{Kind: store.Call, Script: scripts.Hold, Keys: []string{keys.EpochKey}, Args: []string{id, reason}}
}

// TestHold (plan S4, L2; DESIGN §C3): hold sets the integrity hold only for the expected
// epoch and only when it is empty; an unreadable record or another epoch is epoch_changed;
// hwm never changes, even when the clock is later.
func TestHold(t *testing.T) {
	p := testPolicy()
	fp := fingerprint.Of(p)
	base := healthy(fp).with("nb", "0")
	changed := rep(41, 0, "epoch_changed")
	cases := []struct {
		name    string
		setup   func(e *env)
		id      string
		damaged []string
		want    []any
		set     bool // the hold is set to the request's reason
	}{
		{name: "empty hold, expected id", setup: func(e *env) { e.record(base) }, id: epochID, want: rep(40, 0, "", 0), set: true},
		{name: "an existing hold stays", setup: func(e *env) { e.record(base.with("hold", "operator")) }, id: epochID, want: rep(40, 0, "", 0)},
		{name: "wrong id", setup: func(e *env) { e.record(base) }, id: otherID, want: changed},
		{name: "record missing", setup: func(*env) {}, id: epochID, want: changed},
		{name: "record malformed", setup: func(e *env) { e.record(base.with("nm", "-1")) }, id: epochID, damaged: []string{keys.EpochKey}, want: changed},
		{name: "record a string", setup: func(e *env) { e.do("SET", keys.EpochKey, "x") }, id: epochID, damaged: []string{keys.EpochKey}, want: changed},
		{name: "hwm missing: the hold is set, hwm stays missing", setup: func(e *env) { e.record(base.with("hwm", "<del>")) },
			id: epochID, damaged: []string{keys.EpochKey}, want: rep(40, 0, "", 0), set: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			e.clock(now0) // later than hwm0: a hold must still leave hwm alone
			c.setup(e)
			e.h.Damaged(c.damaged...)
			before := e.snap()
			checkReply(t, c.name, e.run(e.la, holdReq(c.id, "store_rollback")), c.want)
			want := cloneState(before)
			if c.set {
				setField(want, keys.EpochKey, "hold", "store_rollback")
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}

// startReq is an admin_start_epoch call (§A4.1 ARGV: new_id, fp, schema, clock, replace,
// expected_id).
func startReq(fp, clock, replace, expected string) store.Request {
	return store.Request{
		Kind: store.Call, Script: scripts.AdminStartEpoch,
		Keys: []string{keys.EpochKey, keys.CtlTimeKey},
		Args: []string{newID, fp, "1", clock, replace, expected},
	}
}

// newRecord is the record admin_start_epoch writes at budget time t (§A4.6).
func newRecord(fp, clock string, t int64) storestate.Value {
	return storestate.Value{Type: storestate.Hash, Fields: map[string]string{
		"id": newID, "fp": fp, "schema": "1", "clock": clock, "hold": "",
		"hwm": num(t), "origin": num(t), "created": num(t), "gen": "0", "nb": "0", "nm": "0",
	}}
}

// TestAdminStartEpoch (plan S4, L2; SPEC ADM-1; DESIGN §A4.6): as rl_admin. Without the
// flag it starts on a fresh store or over a malformed record and refuses a readable one;
// with the flag it needs a readable record of the expected epoch (under a hold or with hwm
// missing too); the new record has hwm = origin = created = the clock reading taken with
// the new clock setting, and a clock error writes nothing.
func TestAdminStartEpoch(t *testing.T) {
	p, sp := testPolicy(), storeClockPolicy()
	fp, sfp := fingerprint.Of(p), fingerprint.Of(sp)
	base := healthy(fp).with("nb", "0")
	cases := []struct {
		name          string
		setup         func(e *env)
		clock         string // ARGV clock; "" for controlled
		replace       string
		expected      string
		damaged       []string
		want          []any // the reply; nil when a new record is written
		storeTimeUsed bool  // the new record's time comes from TIME
	}{
		{name: "fresh store", setup: func(*env) {}, replace: "0"},
		{name: "readable record, no flag", setup: func(e *env) { e.record(base) }, replace: "0", want: rep(41, 0, "epoch_exists")},
		{name: "readable record under a hold, no flag", setup: func(e *env) { e.record(base.with("hold", "operator")) },
			replace: "0", want: rep(41, 0, "epoch_exists")},
		{name: "readable record with hwm missing, no flag", setup: func(e *env) { e.record(base.with("hwm", "<del>")) },
			replace: "0", damaged: []string{keys.EpochKey}, want: rep(41, 0, "epoch_exists")},
		{name: "malformed record, no flag: replaced", setup: func(e *env) { e.record(base.with("schema", "2")) }, replace: "0"},
		{name: "rl:epoch a string, no flag: replaced", setup: func(e *env) { e.do("SET", keys.EpochKey, "x") }, replace: "0"},
		{name: "replace under a hold", setup: func(e *env) { e.record(base.with("hold", "store_rollback")) }, replace: "1", expected: epochID},
		{name: "replace with hwm missing", setup: func(e *env) { e.record(base.with("hwm", "<del>")) }, replace: "1", expected: epochID},
		{name: "replace with a stale id", setup: func(e *env) { e.record(base) }, replace: "1", expected: otherID, want: rep(41, 0, "epoch_changed")},
		{name: "replace on an empty store", setup: func(*env) {}, replace: "1", expected: epochID, want: rep(41, 0, "epoch_record_missing")},
		{name: "replace over a malformed record", setup: func(e *env) { e.record(base.with("schema", "2")) },
			replace: "1", expected: epochID, damaged: []string{keys.EpochKey}, want: rep(41, 0, "epoch_record_malformed")},
		{name: "replace over a string", setup: func(e *env) { e.do("SET", keys.EpochKey, "x") },
			replace: "1", expected: epochID, damaged: []string{keys.EpochKey}, want: rep(41, 0, "epoch_record_malformed")},
		{name: "controlled time missing", setup: func(e *env) { e.do("DEL", keys.CtlTimeKey) }, replace: "0", want: rep(21, 0, "clock")},
		{name: "replace, controlled time missing", setup: func(e *env) { e.do("DEL", keys.CtlTimeKey); e.record(base) },
			replace: "1", expected: epochID, want: rep(21, 0, "clock")},
		// The refusals come before the clock read (§A4.6).
		{name: "readable record, no flag, controlled time missing", setup: func(e *env) { e.do("DEL", keys.CtlTimeKey); e.record(base) },
			replace: "0", want: rep(41, 0, "epoch_exists")},
		{name: "store clock", setup: func(*env) {}, clock: "store", replace: "0", storeTimeUsed: true},
		{name: "the new clock setting, not the old record's: controlled", setup: func(e *env) {
			e.record(base.with("clock", "store", "fp", sfp))
		}, replace: "1", expected: epochID},
		{name: "the new clock setting, not the old record's: store", setup: func(e *env) {
			e.do("DEL", keys.CtlTimeKey)
			e.record(base)
		}, clock: "store", replace: "1", expected: epochID, storeTimeUsed: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p, sp)
			e.clock(now0)
			c.setup(e)
			e.h.Damaged(c.damaged...)
			clock, newFP := c.clock, fp
			if clock == "" {
				clock = "controlled"
			}
			if clock == "store" {
				newFP = sfp
			}
			before := e.snap()
			lo := e.storeTime()
			got := e.run(e.aa, startReq(newFP, clock, c.replace, c.expected))
			hi := e.storeTime()
			if c.want != nil {
				checkReply(t, c.name, got, c.want)
				e.checkUnchanged(c.name, before)
				return
			}
			at := int64(now0)
			if c.storeTimeUsed {
				v, ok := got[1].(int64)
				if !ok || v < lo || v > hi {
					t.Fatalf("t_us %v, want the store's clock, in [%d, %d]", got[1], lo, hi)
				}
				at = v
			}
			checkReply(t, c.name, got, rep(40, at, "", newID))
			want := cloneState(before)
			want[keys.EpochKey] = newRecord(newFP, clock, at)
			checkState(t, c.name, e.snap(), want)
		})
	}
}
