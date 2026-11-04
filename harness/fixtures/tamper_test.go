//go:build rltest

package fixtures_test

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
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

// recConn records every command a Tamper sends, then passes it to the driver's recording
// rl_fixture connection, which declares the keys it writes for the teardown audit.
type recConn struct {
	inner fixtures.TamperConn
	cmds  [][]string
}

func (r *recConn) Do(args ...string) (storeproc.Reply, error) {
	r.cmds = append(r.cmds, slices.Clone(args))
	return r.inner.Do(args...)
}

// tamperEnv is one deployment in the state every tamper operation starts from: in epoch id,
// key k1 has a bucket and two markers <g>:<s1> and <g>:<s2> (three acquires, two of them
// released and refunded) and key k2 a bucket; the instance is closed, so nothing but the
// tamper tool writes to the store.
type tamperEnv struct {
	d          *driver.Driver
	id         string
	tm         *fixtures.Tamper
	rec        *recConn
	enc1, enc2 string
	g, s1, s2  int64
}

func newTamperEnv(t *testing.T) *tamperEnv {
	t.Helper()
	d := driver.New(t, driver.Options{Instances: 1})
	ops := []driver.Op{d.Acquire(0, "k1"), d.Acquire(0, "k1"), d.Acquire(0, "k1"), d.Acquire(0, "k2")}
	for i, op := range ops {
		if op.Kind != limiter.Allowed || !op.Replied {
			t.Fatalf("acquire %d of %q: %v, want allowed with a reply", i, op.Key, op.Kind)
		}
	}
	d.Release(ops[0])
	d.Release(ops[1])
	d.Done(ops[2])
	d.Done(ops[3])
	for _, op := range ops[:2] {
		ev, err := d.WaitRefund(op, 10*time.Second)
		if err != nil || ev.Result != limiter.Applied {
			t.Fatalf("refund of %q: %+v, %v; want applied", op.Key, ev, err)
		}
	}
	d.Close(0)
	e := &tamperEnv{d: d, id: d.ReadEpoch().ID, enc1: "azE", enc2: "azI"} // base64url of k1, k2
	if len(e.id) != 32 {
		t.Fatalf("epoch id %q", e.id)
	}
	// The receipts' g and s (§A4.9: {1, t, '', u, g, s, expiry_us}).
	e.g, e.s1, e.s2 = ops[0].Reply.Elems[4].Int, ops[0].Reply.Elems[5].Int, ops[1].Reply.Elems[5].Int
	// The cases name marker <g>:<s2>, so a swap of g and s cannot go unseen.
	if ops[1].Reply.Elems[4].Int != e.g || e.s1 == e.s2 || e.g == e.s2 {
		t.Fatalf("receipts %v and %v: want one generation, two sequences, the second other than the generation",
			ops[0].Reply.Elems, ops[1].Reply.Elems)
	}
	e.rec = &recConn{inner: d.Fixture()}
	e.tm = fixtures.NewTamper(e.rec, e.id)
	return e
}

func (e *tamperEnv) k1() string  { return keys.BucketKey(e.id, e.enc1) }
func (e *tamperEnv) mi1() string { return keys.MarkerIndexKey(e.id, e.enc1) }
func (e *tamperEnv) ix() string  { return keys.IndexKey(e.id) }
func (e *tamperEnv) q() string   { return keys.QuarantineKey(e.id) }
func (e *tamperEnv) m(s int64) string {
	return strconv.FormatInt(e.g, 10) + ":" + strconv.FormatInt(s, 10)
}

// view is what ADM-4 shows: status and both keys' inspect (through the driver, as rl_admin).
type view struct {
	Status driver.Status
	K1, K2 driver.Inspect
}

func (e *tamperEnv) view() view {
	return view{Status: e.d.Status(), K1: e.d.Inspect("k1"), K2: e.d.Inspect("k2")}
}

func cloneState(s storestate.State) storestate.State {
	out := storestate.State{}
	for k, v := range s {
		v.Fields = maps.Clone(v.Fields)
		out[k] = v
	}
	return out
}

// diffState lists the keys whose values differ between got and want.
func diffState(got, want storestate.State) string {
	var lines []string
	for _, k := range slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
		for k := range got {
			yield(k, true)
		}
		for k := range want {
			yield(k, true)
		}
	}))) {
		g, gok := got[k]
		w, wok := want[k]
		if gok != wok || !reflect.DeepEqual(g, w) {
			lines = append(lines, fmt.Sprintf("  %s: got %+v (present %v), want %+v (present %v)", k, g, gok, w, wok))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestTamperOps (DESIGN §B6; HARNESS FLT-7, FLT-11; plan S8 step 4): each tamper operation,
// from the same state, makes exactly its change: the store's keys afterwards equal the keys
// before with that change made by hand, it sends exactly the plain commands written here as
// rl_fixture (never a script), and status and inspect (ADM-4) show its effect and nothing
// else. Deletions that find nothing, and invalid arguments, fail and change nothing; a
// deletion that finds only part of its items fails, and CorruptRecord writes nothing without
// a record.
func TestTamperOps(t *testing.T) {
	type state = storestate.State
	unchanged := func(*tamperEnv, *view) {}
	refusedAll := func(reason string) func(*tamperEnv, *view) {
		return func(_ *tamperEnv, v *view) {
			v.Status = driver.Status{Code: 41, Reason: reason}
			v.K1 = driver.Inspect{Code: 41, Reason: reason}
			v.K2 = driver.Inspect{Code: 41, Reason: reason}
		}
	}
	cases := []struct {
		name string
		op   func(e *tamperEnv) error
		cmds func(e *tamperEnv) [][]string // the commands sent, in order
		keys func(e *tamperEnv, s state)   // the change to the store's keys
		view func(e *tamperEnv, v *view)   // the change status and inspect show
	}{
		{
			name: "DeleteBucketFields all",
			op:   func(e *tamperEnv) error { return e.tm.DeleteBucketFields("k1") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"HDEL", e.k1(), "u", "t", "g", "s", "x", "mc"}} },
			keys: func(e *tamperEnv, s state) {
				for _, f := range []string{"u", "t", "g", "s", "x", "mc"} {
					delete(s[e.k1()].Fields, f)
				}
			},
			view: func(_ *tamperEnv, v *view) {
				v.K1.U, v.K1.T, v.K1.G, v.K1.S, v.K1.X, v.K1.MC = -1, -1, -1, -1, -1, -1 // markers keep the hash
			},
		},
		{
			name: "DeleteBucketFields u and mc",
			op:   func(e *tamperEnv) error { return e.tm.DeleteBucketFields("k1", "u", "mc") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"HDEL", e.k1(), "u", "mc"}} },
			keys: func(e *tamperEnv, s state) { delete(s[e.k1()].Fields, "u"); delete(s[e.k1()].Fields, "mc") },
			view: func(_ *tamperEnv, v *view) { v.K1.U, v.K1.MC = -1, -1 },
		},
		{
			name: "DeleteBucket",
			op:   func(e *tamperEnv) error { return e.tm.DeleteBucket("k1") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"DEL", e.k1()}} },
			keys: func(e *tamperEnv, s state) { delete(s, e.k1()) },
			view: func(_ *tamperEnv, v *view) {
				v.K1.Present = 0
				v.K1.U, v.K1.T, v.K1.G, v.K1.S, v.K1.X, v.K1.MC = -1, -1, -1, -1, -1, -1
			},
		},
		{
			name: "DeleteMarkerIndex",
			op:   func(e *tamperEnv) error { return e.tm.DeleteMarkerIndex("k1") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"DEL", e.mi1()}} },
			keys: func(e *tamperEnv, s state) { delete(s, e.mi1()) },
			view: func(_ *tamperEnv, v *view) { v.K1.Expiries = nil },
		},
		{
			name: "DeleteMarkerIndexMember",
			op:   func(e *tamperEnv) error { return e.tm.DeleteMarkerIndexMember("k1", e.g, e.s2) },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"ZREM", e.mi1(), e.m(e.s2)}} },
			keys: func(e *tamperEnv, s state) { delete(s[e.mi1()].Fields, e.m(e.s2)) },
			view: func(_ *tamperEnv, v *view) { v.K1.Expiries = v.K1.Expiries[1:] }, // both markers expire at once
		},
		{
			name: "DeleteIndexMember",
			op:   func(e *tamperEnv) error { return e.tm.DeleteIndexMember("k1") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"ZREM", e.ix(), e.enc1}} },
			keys: func(e *tamperEnv, s state) { delete(s[e.ix()].Fields, e.enc1) },
			view: func(_ *tamperEnv, v *view) { v.K1.IxScore = -1 },
		},
		{
			name: "DeleteMarkerField",
			op:   func(e *tamperEnv) error { return e.tm.DeleteMarkerField("k1", e.g, e.s2) },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"HDEL", e.k1(), "m:" + e.m(e.s2)}} },
			keys: func(e *tamperEnv, s state) { delete(s[e.k1()].Fields, "m:"+e.m(e.s2)) },
			view: unchanged, // inspect shows mc and the expiries from mi, which both stay
		},
		{
			name: "CorruptField bucket",
			op:   func(e *tamperEnv) error { return e.tm.CorruptField(e.k1(), "u", "x") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"HSET", e.k1(), "u", "x"}} },
			keys: func(e *tamperEnv, s state) { s[e.k1()].Fields["u"] = "x" },
			view: func(_ *tamperEnv, v *view) { v.K1.U = -1 },
		},
		{
			name: "CorruptField index score",
			op:   func(e *tamperEnv) error { return e.tm.CorruptField(e.ix(), e.enc1, "1.5") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"ZADD", e.ix(), "1.5", e.enc1}} },
			keys: func(e *tamperEnv, s state) { s[e.ix()].Fields[e.enc1] = "1.5" },
			view: func(_ *tamperEnv, v *view) { v.K1.IxScore = -1 },
		},
		{
			name: "CorruptField marker index score",
			op:   func(e *tamperEnv) error { return e.tm.CorruptField(e.mi1(), e.m(e.s2), "1.5") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"ZADD", e.mi1(), "1.5", e.m(e.s2)}} },
			keys: func(e *tamperEnv, s state) { s[e.mi1()].Fields[e.m(e.s2)] = "1.5" },
			// 1.5 fails is_score, so that expiry shows as '' and, the lowest score, comes first;
			// both markers expire at once, so the other keeps the same value.
			view: func(_ *tamperEnv, v *view) { v.K1.Expiries = []int64{-1, v.K1.Expiries[0]} },
		},
		{
			name: "CorruptField quarantine flag",
			op:   func(e *tamperEnv) error { return e.tm.CorruptField(e.q(), e.enc1, "x") },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"HSET", e.q(), e.enc1, "x"}} },
			keys: func(e *tamperEnv, s state) {
				s[e.q()] = storestate.Value{Type: storestate.Hash, Fields: map[string]string{e.enc1: "x"}} // no q before
			},
			view: func(_ *tamperEnv, v *view) { v.K1.Quarantine, v.Status.QuarantinedCount = "x", 1 },
		},
		{
			name: "CorruptField hwm",
			op:   func(e *tamperEnv) error { return e.tm.CorruptField(keys.EpochKey, "hwm", "x") },
			cmds: func(*tamperEnv) [][]string { return [][]string{{"HSET", "rl:epoch", "hwm", "x"}} },
			keys: func(_ *tamperEnv, s state) { s["rl:epoch"].Fields["hwm"] = "x" },
			view: func(_ *tamperEnv, v *view) { v.Status.HWM = -1 },
		},
		{
			name: "AddMalformedIndexMember",
			op:   func(e *tamperEnv) error { return e.tm.AddMalformedIndexMember("!", 0) },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"ZADD", e.ix(), "0", "!"}} },
			keys: func(e *tamperEnv, s state) { s[e.ix()].Fields["!"] = "0" },
			view: unchanged,
		},
		{
			name: "ChangeSharedKeyType ix",
			op:   func(e *tamperEnv) error { return e.tm.ChangeSharedKeyType(fixtures.SharedIndex) },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"DEL", e.ix()}, {"SET", e.ix(), "tampered"}} },
			keys: func(e *tamperEnv, s state) { s[e.ix()] = storestate.Value{Type: storestate.String, Str: "tampered"} },
			view: func(_ *tamperEnv, v *view) {
				v.K1 = driver.Inspect{Code: 41, Reason: "shared_key_malformed"}
				v.K2 = driver.Inspect{Code: 41, Reason: "shared_key_malformed"}
			},
		},
		{
			name: "ChangeSharedKeyType q",
			op:   func(e *tamperEnv) error { return e.tm.ChangeSharedKeyType(fixtures.SharedQuarantine) },
			cmds: func(e *tamperEnv) [][]string { return [][]string{{"DEL", e.q()}, {"SET", e.q(), "tampered"}} },
			keys: func(e *tamperEnv, s state) { s[e.q()] = storestate.Value{Type: storestate.String, Str: "tampered"} },
			view: refusedAll("shared_key_malformed"),
		},
		{
			name: "ChangeSharedKeyType epoch",
			op:   func(e *tamperEnv) error { return e.tm.ChangeSharedKeyType(fixtures.SharedEpoch) },
			cmds: func(*tamperEnv) [][]string { return [][]string{{"DEL", "rl:epoch"}, {"SET", "rl:epoch", "tampered"}} },
			keys: func(_ *tamperEnv, s state) {
				s["rl:epoch"] = storestate.Value{Type: storestate.String, Str: "tampered"}
			},
			view: refusedAll("epoch_record_malformed"),
		},
		{
			name: "DeleteHwm",
			op:   func(e *tamperEnv) error { return e.tm.DeleteHwm() },
			cmds: func(*tamperEnv) [][]string { return [][]string{{"HDEL", "rl:epoch", "hwm"}} },
			keys: func(_ *tamperEnv, s state) { delete(s["rl:epoch"].Fields, "hwm") },
			view: func(_ *tamperEnv, v *view) { v.Status.HWM = -1 },
		},
		{
			name: "CorruptRecord",
			op:   func(e *tamperEnv) error { return e.tm.CorruptRecord() },
			cmds: func(*tamperEnv) [][]string {
				return [][]string{{"TYPE", "rl:epoch"}, {"HSET", "rl:epoch", "schema", "corrupt"}}
			},
			keys: func(_ *tamperEnv, s state) { s["rl:epoch"].Fields["schema"] = "corrupt" },
			view: refusedAll("epoch_record_malformed"),
		},
		{
			name: "Wipe",
			op:   func(e *tamperEnv) error { return e.tm.Wipe() },
			cmds: func(*tamperEnv) [][]string { return [][]string{{"FLUSHALL"}} },
			keys: func(_ *tamperEnv, s state) { clear(s) }, // rl:ctl:time too
			view: refusedAll("epoch_record_missing"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTamperEnv(t)
			before, vBefore := e.d.Dump(), e.view()
			if vBefore.Status.Code != 40 || vBefore.K1.Code != 40 || vBefore.K1.Present != 1 || len(vBefore.K1.Expiries) != 2 ||
				vBefore.K2.Present != 1 || vBefore.Status.HWM < 0 || vBefore.K1.IxScore < 0 || vBefore.Status.QuarantinedCount != 0 {
				t.Fatalf("the starting state is not as built: %+v", vBefore)
			}
			if err := c.op(e); err != nil {
				t.Fatal(err)
			}
			if want := c.cmds(e); !reflect.DeepEqual(e.rec.cmds, want) {
				t.Errorf("sent %q, want %q", e.rec.cmds, want)
			}
			want := cloneState(before)
			c.keys(e, want)
			if got := e.d.Dump(); !reflect.DeepEqual(got, want) {
				t.Errorf("store keys after the operation differ from the keys before with its change:\n%s", diffState(got, want))
			}
			wantView := vBefore
			wantView.K1.Expiries = slices.Clone(vBefore.K1.Expiries)
			c.view(e, &wantView)
			if got := e.view(); !reflect.DeepEqual(got, wantView) {
				t.Errorf("status and inspect:\n  got  %+v\n  want %+v", got, wantView)
			}
		})
	}

	t.Run("refusals change nothing", func(t *testing.T) {
		e := newTamperEnv(t)
		before := e.d.Dump()
		other := strings.Repeat("ab", 16)
		for name, err := range map[string]error{
			"DeleteBucket of a key without a bucket":  e.tm.DeleteBucket("k3"),
			"DeleteBucketFields of a marker field":    e.tm.DeleteBucketFields("k1", "m:"+e.m(e.s1)),
			"DeleteBucketFields naming u twice":       e.tm.DeleteBucketFields("k1", "u", "u"),
			"DeleteBucketFields of a key without one": e.tm.DeleteBucketFields("k3"),
			"DeleteBucketFields naming u2, no field":  e.tm.DeleteBucketFields("k2", "u", "u2"),
			"DeleteMarkerIndex of a key without one":  e.tm.DeleteMarkerIndex("k2"),
			"DeleteMarkerIndexMember of no member":    e.tm.DeleteMarkerIndexMember("k1", e.g, e.s1+e.s2+1),
			"DeleteMarkerField of no field":           e.tm.DeleteMarkerField("k2", e.g, e.s1),
			"DeleteIndexMember of no member":          e.tm.DeleteIndexMember("k3"),
			"AddMalformedIndexMember of a valid enc":  e.tm.AddMalformedIndexMember(e.enc1, 0),
			"CorruptField of another epoch":           e.tm.CorruptField(keys.BucketKey(other, e.enc1), "u", "x"),
			"CorruptField of the controlled clock":    e.tm.CorruptField(keys.CtlTimeKey, "x", "1"),
			"CorruptField outside the layout":         e.tm.CorruptField("rl:e:"+e.id+":zz", "x", "1"),
			"ChangeSharedKeyType of no shared key":    e.tm.ChangeSharedKeyType(0),
			"a key that is not UTF-8":                 e.tm.DeleteBucket("\xff"),
			"no epoch ID":                             fixtures.NewTamper(e.rec, "").DeleteBucket("k1"),
		} {
			if err == nil {
				t.Errorf("%s: no error", name)
			}
		}
		if got := e.d.Dump(); !reflect.DeepEqual(got, before) {
			t.Errorf("refused operations changed the store:\n%s", diffState(got, before))
		}
		// The deletions sent found nothing; the invalid arguments sent nothing.
		for _, c := range e.rec.cmds {
			switch c[0] {
			case "DEL", "HDEL", "ZREM":
			default:
				t.Errorf("a refused operation sent %q", c)
			}
		}
	})
	// A deletion that removes only part of what it names is an error too (the count guard),
	// though the store changed: u is gone, so the second HDEL removes 5 of its 6 fields.
	t.Run("partial delete is an error", func(t *testing.T) {
		e := newTamperEnv(t)
		if err := e.tm.DeleteBucketFields("k1", "u"); err != nil {
			t.Fatal(err)
		}
		if err := e.tm.DeleteBucketFields("k1"); err == nil {
			t.Error("DeleteBucketFields of the six fields, u among them already gone: no error")
		}
		if want := [][]string{{"HDEL", e.k1(), "u"}, {"HDEL", e.k1(), "u", "t", "g", "s", "x", "mc"}}; !reflect.DeepEqual(e.rec.cmds, want) {
			t.Errorf("sent %q, want %q", e.rec.cmds, want)
		}
	})
	// CorruptRecord reads rl:epoch's type first and writes nothing unless it is a hash.
	t.Run("CorruptRecord with no record", func(t *testing.T) {
		e := newTamperEnv(t)
		if err := e.tm.Wipe(); err != nil {
			t.Fatal(err)
		}
		if err := e.tm.CorruptRecord(); err == nil {
			t.Error("CorruptRecord after a wipe: no error")
		}
		if got := e.d.Dump(); len(got) != 0 {
			t.Errorf("CorruptRecord after a wipe wrote %+v, want no key", got)
		}
		if want := [][]string{{"FLUSHALL"}, {"TYPE", "rl:epoch"}}; !reflect.DeepEqual(e.rec.cmds, want) {
			t.Errorf("sent %q, want %q", e.rec.cmds, want)
		}
	})
}
