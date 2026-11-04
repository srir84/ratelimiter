//go:build scripttest

package scriptrun_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/fingerprint"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/settings"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/testutil/scriptrun"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
	"github.com/srir84/ratelimiter/internal/testutil/storestate"
)

// countCaller is a scriptrun.Caller that only counts its calls.
type countCaller struct{ calls atomic.Int64 }

func (c *countCaller) Call(context.Context, store.Request) store.Response {
	c.calls.Add(1)
	return store.Response{}
}

const (
	epochID = "9c56cc51b374c3ba189210d5b6d4bf57"
	now0    = 1_760_000_000_223_456 // the controlled time, µs
	hwm0    = 1_760_000_000_123_456
)

// testPolicy is the default policy with the required limits set and the controlled clock.
func testPolicy() settings.Policy {
	p := settings.DefaultPolicy()
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 1000, 100_000
	p.ClockSource = settings.Controlled
	return p
}

// plainStore starts a fast-profile store with no teardown audit: TestScriptAudit is a
// checker self-test, so it uses plain stores (AGENTS.md §5; plan S4 Tests (a)).
func plainStore(t *testing.T) *storeproc.Store {
	t.Helper()
	return storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
}

// libAdapter is a store adapter as rl_lib with policy p's reply limits, built as
// scriptrun's Handle.Adapter builds one; Close runs at cleanup.
func libAdapter(t *testing.T, s *storeproc.Store, p settings.Policy) *store.StoreAdapter {
	t.Helper()
	a := store.New(t.Context(), store.Options{
		Addr: s.Addr, User: "rl_lib", Password: s.PW.Lib,
		StoreConns: 1, RefundConns: 1,
		DependencyDeadline: 10 * time.Second,
		Limits:             store.LimitsFor(p),
	})
	t.Cleanup(a.Close)
	return a
}

// do runs one command on c and fails the test on an error or an error reply.
func do(t *testing.T, c *storeproc.Client, args ...string) {
	t.Helper()
	r, err := c.Do(args...)
	if err != nil || r.Kind == '-' {
		t.Fatalf("%s: %v %s", strings.Join(args, " "), err, r)
	}
}

func dial(t *testing.T, s *storeproc.Store, user string) *storeproc.Client {
	t.Helper()
	c, err := s.Conn(user)
	if err != nil {
		t.Fatalf("connecting as %s: %v", user, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func snapshot(t *testing.T, c *storeproc.Client) storestate.State {
	t.Helper()
	s, err := storestate.Snapshot(c)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

// instrument is scriptrun.Instrument, failing the test on an error.
func instrument(t *testing.T, s scripts.Script) scripts.Script {
	t.Helper()
	inst, err := scriptrun.Instrument(s)
	if err != nil {
		t.Fatalf("Instrument(%s): %v", s.Name, err)
	}
	return inst
}

// TestScriptAudit (plan S4, L2; DESIGN §C3, §C9): the discipline run, through a fake
// testing.TB, on plain stores. Fixture scripts that break one rule each fail with AUDIT,
// the wrappers' checks running before the call, so the script's own pcall never catches
// them, also inside the script's own pcall(function … end) and when the script then fails on
// its own (S4 repair F2); a DEL of an unread key followed by an HSET to it passes; Instrument
// rewrites calls split across lines, instruments a script that merely holds the wrapper mark,
// keeps its own output unchanged, and refuses a redis.call or redis.pcall it cannot rewrite,
// a first line that is not a #!lua header and its own output with a raw call added, which Run
// then never calls; the instrumented acquire passes; and acquire and Instrument(acquire),
// called straight through the adapter on two stores in the same state, give identical
// replies and store diffs.
func TestScriptAudit(t *testing.T) {
	qKey := keys.QuarantineKey(epochID)
	// Every fixture runs with these KEYS, as rl_lib; no epoch record exists.
	fixtureKeys := []string{keys.EpochKey, keys.CtlTimeKey, qKey}
	for _, c := range []struct {
		name, body string
		audit      string // a fragment of the AUDIT message; "" when the run passes
		writes     bool   // the fixture writes before the refused command
	}{
		{"read after a write", "redis.call('HGETALL', KEYS[3])\nredis.call('HSET', KEYS[3], 'f', '1')\nreturn redis.call('HLEN', KEYS[3])",
			"AUDIT HLEN reads after the first write", true},
		{"read after a write, redis.pcall", "redis.pcall('HGETALL', KEYS[3])\nredis.pcall('HSET', KEYS[3], 'f', '1')\nreturn redis.pcall('HEXISTS', KEYS[3], 'f')",
			"AUDIT HEXISTS reads after the first write", true},
		{"read after a DEL", "redis.call('DEL', KEYS[3])\nreturn redis.call('HGETALL', KEYS[3])",
			"AUDIT HGETALL reads after the first write", true},
		{"two clock reads", "redis.call('HGETALL', KEYS[1])\nredis.call('TIME')\nreturn redis.call('TIME')",
			"AUDIT TIME is a second clock read", false},
		{"two clock reads, TIME then the controlled time", "redis.call('HGETALL', KEYS[1])\nredis.call('TIME')\nreturn {pcall(redis.call, 'GET', KEYS[2])}",
			"AUDIT GET is a second clock read", false},
		{"clock read before the epoch record", "redis.call('TIME')\nreturn redis.call('HGETALL', KEYS[1])",
			"AUDIT TIME reads the clock before the epoch record", false},
		{"clock read after another key's read, before the epoch record", "redis.call('HGETALL', KEYS[3])\nreturn redis.call('TIME')",
			"AUDIT TIME reads the clock before the epoch record", false},
		{"controlled time before the epoch record, inside pcall(redis.call, …)", "local ok = pcall(redis.call, 'GET', KEYS[2])\nreturn ok and 1 or 0",
			"AUDIT GET reads the clock before the epoch record", false},
		// As stylua splits a long call: pcall( / redis.call, / one argument per line.
		{"controlled time before the epoch record, pcall(redis.call, …) split across lines", "local ok = pcall(\n  redis.call,\n  'GET',\n  KEYS[2]\n)\nreturn ok and 1 or 0",
			"AUDIT GET reads the clock before the epoch record", false},
		{"write to an unread key", "redis.call('HSET', KEYS[3], 'f', '1')\nreturn 1",
			"AUDIT HSET writes " + qKey + ", which was neither read nor deleted before", false},
		{"write to a key whose read failed", "redis.pcall('HGETALL', KEYS[2])\nredis.call('HSET', KEYS[2], 'f', '1')\nreturn 1",
			"AUDIT HSET writes rl:ctl:time, which was neither read nor deleted before", false},
		{"write to a key whose pcall(redis.call, …) read failed", "pcall(redis.call, 'HGETALL', KEYS[2])\nredis.call('HSET', KEYS[2], 'f', '1')\nreturn 1",
			"AUDIT HSET writes rl:ctl:time, which was neither read nor deleted before", false},
		{"key not in KEYS, inside pcall(redis.call, …)", "local ok = pcall(redis.call, 'HGETALL', 'rl:e:" + epochID + ":k:YQ')\nreturn ok and 1 or 0",
			"AUDIT HGETALL: key rl:e:" + epochID + ":k:YQ is not in KEYS", false},
		{"key not in KEYS, a DEL", "redis.call('DEL', KEYS[3], 'rl:e:" + epochID + ":ix')\nreturn 1",
			"AUDIT DEL: key rl:e:" + epochID + ":ix is not in KEYS", false},
		{"a command the run cannot classify", "redis.call('HGETALL', KEYS[3])\nreturn redis.call('EXISTS', KEYS[3])",
			"AUDIT EXISTS is not a command the discipline run knows", false},
		{"DEL of an unread key, then HSET", "redis.call('DEL', KEYS[3])\nredis.call('HSET', KEYS[3], 'f', '1')\nreturn 1", "", true},
		// Inside the script's own pcall(function … end), each rule still fails the run: the
		// script's pcall catches the wrapper's error, but the run keeps the first violation and
		// raises it again once the script returns, or in place of the script's own error.
		{"read after a write, inside pcall(function)", "redis.call('HGETALL', KEYS[3])\nredis.call('HSET', KEYS[3], 'f', '1')\nlocal ok = pcall(function() return redis.call('HLEN', KEYS[3]) end)\nreturn ok and 1 or 0",
			"AUDIT HLEN reads after the first write", true},
		{"two clock reads, inside pcall(function)", "redis.call('HGETALL', KEYS[1])\nredis.call('TIME')\nlocal ok = pcall(function() return redis.call('TIME') end)\nreturn ok and 1 or 0",
			"AUDIT TIME is a second clock read", false},
		{"write to an unread key, inside pcall(function)", "local ok = pcall(function() return redis.call('HSET', KEYS[3], 'f', '1') end)\nreturn ok and 1 or 0",
			"AUDIT HSET writes " + qKey + ", which was neither read nor deleted before", false},
		{"write to an unread key, inside pcall(function), then the script's own error", "local ok = pcall(function() return redis.call('HSET', KEYS[3], 'f', '1') end)\nif not ok then error('the script fails on its own') end\nreturn 1",
			"AUDIT HSET writes " + qKey + ", which was neither read nor deleted before", false},
		// Only Instrument's exact output counts as instrumented: the wrapper mark alone, right
		// after the header or later, is a comment of a script still to instrument.
		{"the wrapper mark right after the header", "-- scriptrun: discipline-run wrappers\nredis.call('HGETALL', KEYS[3])\nredis.call('HSET', KEYS[3], 'f', '1')\nreturn redis.call('HLEN', KEYS[3])",
			"AUDIT HLEN reads after the first write", true},
		{"the wrapper mark later in the script", "redis.call('HGETALL', KEYS[3])\nredis.call('HSET', KEYS[3], 'f', '1')\n-- scriptrun: discipline-run wrappers\nreturn redis.call('HLEN', KEYS[3])",
			"AUDIT HLEN reads after the first write", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st := plainStore(t)
			do(t, dial(t, st, "rl_fixture"), "SET", keys.CtlTimeKey, "1760000000223456")
			lib := dial(t, st, "rl_lib")
			s := instrument(t, scripts.New("audit_fixture", "#!lua\n"+c.body+"\n"))
			if again := instrument(t, s); again != s {
				t.Errorf("Instrument of an instrumented script changed it")
			}
			before := snapshot(t, lib)
			rec := &recordTB{TB: t}
			r := scriptrun.Run(rec, libAdapter(t, st, testPolicy()), store.Request{Kind: store.Call, Script: s, Keys: fixtureKeys})
			msgs := rec.messages()
			after := snapshot(t, lib)
			if c.audit == "" {
				if len(msgs) > 0 || strings.Contains(r.StoreError, "AUDIT") {
					t.Fatalf("Run reported %q (store error %q), want the run to pass", msgs, r.StoreError)
				}
				if v := after[qKey]; v.Type != storestate.Hash || !reflect.DeepEqual(v.Fields, map[string]string{"f": "1"}) {
					t.Errorf("after DEL and HSET, %s = %+v, want the hash {f: 1}", qKey, v)
				}
				return
			}
			if len(msgs) != 1 || !strings.Contains(msgs[0], c.audit) || !strings.Contains(r.StoreError, c.audit) {
				t.Fatalf("Run reported %q (store error %q), want one failure holding %q", msgs, r.StoreError, c.audit)
			}
			// The wrapper refuses before it calls: the offending command never ran.
			if !c.writes && !reflect.DeepEqual(after, before) {
				t.Errorf("the refused command changed the store:\nbefore %v\nafter  %v", before, after)
			}
		})
	}

	// Instrument fails closed: a redis.call or redis.pcall it cannot rewrite is an error.
	for _, c := range []struct{ name, body, token string }{
		{"redis.call as a value", "local f = redis.call\nreturn f('HGETALL', KEYS[1])", "redis.call"},
		{"a comment between pcall( and redis.call", "local ok = pcall( -- the controlled time\n  redis.call, 'GET', KEYS[2])\nreturn ok and 1 or 0", "redis.call"},
		{"redis.pcall as a value", "local f = redis.pcall\nreturn f('HGETALL', KEYS[1])", "redis.pcall"},
	} {
		t.Run("Instrument refuses "+c.name, func(t *testing.T) {
			t.Parallel()
			s, err := scriptrun.Instrument(scripts.New("audit_fixture", "#!lua\n"+c.body+"\n"))
			if err == nil || !strings.Contains(err.Error(), "AUDIT audit_fixture: "+fmt.Sprintf("%q", c.token)+" is not called as") {
				t.Errorf("Instrument: script %q, error %v; want an AUDIT error naming %s", s.Text, err, c.token)
			}
		})
	}

	// Instrument refuses a script whose first line is not a #!lua header (#!lua, or #!lua and
	// its options after a space), since it keeps that line as it is and a call on it would go
	// unchecked; and a text that starts as its own output but is not exactly that.
	for _, c := range []struct{ name, text, want string }{
		{"a script with no header line", "redis.call('HSET', KEYS[3], 'f', '1')\nreturn 1\n", `the first line "redis.call('HSET', KEYS[3], 'f', '1')" is not a #!lua header`},
		{"a script whose first line is empty", "\n#!lua\nreturn redis.call('HGETALL', KEYS[1])\n", `the first line "" is not a #!lua header`},
		{"a header of another engine", "#!luajit\nreturn redis.call('HGETALL', KEYS[1])\n", `the first line "#!luajit" is not a #!lua header`},
	} {
		t.Run("Instrument refuses "+c.name, func(t *testing.T) {
			t.Parallel()
			s, err := scriptrun.Instrument(scripts.New("audit_fixture", c.text))
			if err == nil || !strings.Contains(err.Error(), "AUDIT audit_fixture: "+c.want) {
				t.Errorf("Instrument: script %q, error %v; want an AUDIT error holding %q", s.Text, err, c.want)
			}
		})
	}
	t.Run("Instrument keeps a header with options", func(t *testing.T) {
		t.Parallel()
		inst := instrument(t, scripts.New("audit_fixture", "#!lua flags=no-writes\nreturn redis.call('HGETALL', KEYS[1])\n"))
		if !strings.HasPrefix(inst.Text, "#!lua flags=no-writes\n-- scriptrun: discipline-run wrappers\n") || strings.Contains(inst.Text, "redis.call('HGETALL'") {
			t.Errorf("Instrument kept the header %q, want it first and the call rewritten:\n%s", "#!lua flags=no-writes", inst.Text)
		}
	})
	t.Run("Instrument refuses its own output with a raw call added", func(t *testing.T) {
		t.Parallel()
		inst := instrument(t, scripts.New("audit_fixture", "#!lua\nreturn redis.call('HGETALL', KEYS[1])\n"))
		head, rest, ok := strings.Cut(inst.Text, "local function audit_body(...)\n")
		if !ok {
			t.Fatalf("Instrument's output has no audit_body:\n%s", inst.Text)
		}
		tampered := scripts.New("audit_fixture", head+"local function audit_body(...)\nredis.call('HSET', KEYS[3], 'f', '1')\n"+rest)
		s, err := scriptrun.Instrument(tampered)
		if err == nil || !strings.Contains(err.Error(), "AUDIT audit_fixture: the script starts with the discipline-run wrappers, but is not as Instrument writes it") {
			t.Errorf("Instrument of a tampered instrumented script: %q, error %v; want an AUDIT error", s.Text, err)
		}
	})

	// Run in the discipline run never calls a script Instrument refuses, and reports it.
	t.Run("Run refuses a script Instrument cannot rewrite", func(t *testing.T) {
		t.Setenv(scriptrun.AuditEnv, "1")
		rec := &recordTB{TB: t}
		caller := &countCaller{}
		s := scripts.New("audit_fixture", "#!lua\nlocal f = redis.call\nreturn f('HGETALL', KEYS[1])\n")
		r := scriptrun.Run(rec, caller, store.Request{Kind: store.Call, Script: s, Keys: fixtureKeys})
		msgs := rec.messages()
		if n := caller.calls.Load(); n != 0 {
			t.Errorf("Run called the script %d times, want none", n)
		}
		if len(msgs) != 1 || !strings.Contains(msgs[0], "AUDIT audit_fixture") || r.Outcome == store.OK || !strings.Contains(r.StoreError, "AUDIT") {
			t.Errorf("Run reported %q, outcome %v, store error %q; want one AUDIT failure and no decoded reply", msgs, r.Outcome, r.StoreError)
		}
	})

	// The instrumented acquire passes, and its reply and writes equal the plain script's.
	t.Run("acquire, plain and instrumented", func(t *testing.T) {
		t.Parallel()
		p := testPolicy()
		inst := instrument(t, scripts.Acquire)
		if inst == scripts.Acquire || inst.Name != "acquire" || instrument(t, inst) != inst {
			t.Fatalf("Instrument(acquire): name %q, changed %v, idempotent %v", inst.Name, inst != scripts.Acquire, instrument(t, inst) == inst)
		}
		_, body, _ := strings.Cut(inst.Text, "\nlocal E, T, ENC, K")
		for _, call := range []string{"redis.call(", "redis.pcall(", "pcall(redis.call,"} {
			if strings.Contains(body, call) {
				t.Errorf("the instrumented prelude and body still hold %q", call)
			}
		}
		var replies [2][][]any
		var diffs [2][2]storestate.State
		for i, s := range []scripts.Script{scripts.Acquire, inst} {
			st := plainStore(t)
			fix, lib := dial(t, st, "rl_fixture"), dial(t, st, "rl_lib")
			do(t, fix, "HSET", keys.EpochKey, "id", epochID, "fp", fingerprint.Of(p), "schema", "1", "clock", "controlled",
				"hold", "", "hwm", "1760000000123456", "origin", "1759999000000001", "created", "1759999000000001",
				"gen", "7", "nb", "0", "nm", "0")
			do(t, fix, "SET", keys.CtlTimeKey, "1760000000223456")
			a := libAdapter(t, st, p)
			diffs[i][0] = snapshot(t, lib)
			for _, at := range []string{"1760000000223456", "1760000000323456"} {
				do(t, fix, "SET", keys.CtlTimeKey, at)
				req := store.Request{
					Kind: store.Call, Script: s,
					Keys: []string{keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(epochID), qKey, keys.BucketKey(epochID, "YQ"), keys.MarkerIndexKey(epochID, "YQ")},
					Args: []string{epochID, fingerprint.Of(p), "YQ", "20000000000", "1000000000", "10", "60000000", "1000", "0", "0"},
				}
				var r store.Response
				if i == 0 {
					r = a.Call(t.Context(), req) // straight through the adapter: plain even in the discipline run
				} else {
					rec := &recordTB{TB: t}
					r = scriptrun.Run(rec, a, req)
					if msgs := rec.messages(); len(msgs) > 0 {
						t.Errorf("the instrumented acquire failed the discipline run: %q", msgs)
					}
				}
				if r.Outcome != store.OK || r.Reply.Code != 1 {
					t.Fatalf("%s at %s: outcome %v, code %d, store error %q, raw %s; want an allowed acquire", s.Name, at, r.Outcome, r.Reply.Code, r.StoreError, r.Raw)
				}
				var got []any
				for _, v := range r.Reply.Elems {
					got = append(got, v.String())
				}
				replies[i] = append(replies[i], got)
			}
			diffs[i][1] = snapshot(t, lib)
		}
		if !reflect.DeepEqual(replies[0], replies[1]) {
			t.Errorf("replies differ:\nplain        %v\ninstrumented %v", replies[0], replies[1])
		}
		if !reflect.DeepEqual(diffs[0], diffs[1]) {
			t.Errorf("store states differ:\nplain        %v\ninstrumented %v", diffs[0], diffs[1])
		}
	})
}

// TestStoreAuditAtTeardown (plan S4 step 14; DESIGN §C3; S4 review F3): Store registers the
// store audit in t.Cleanup, to run while the store is still up, and the audit reports each
// violation with t.Errorf. A subtest's store gets, as rl_fixture, an undeclared stray key and
// a key broken on purpose that the test declares with Damaged; once the subtest has returned,
// its recording testing.TB holds exactly one error, the stray key's, and none for the Damaged
// key. The audit not registered, run after the store stops, reporting with Logf, or ignoring
// Damaged, each fails it.
func TestStoreAuditAtTeardown(t *testing.T) {
	const stray = "rl:junk"
	damaged := keys.BucketKey(epochID, "YQ")
	var rec *recordTB
	t.Run("store with a stray key and a Damaged key", func(t *testing.T) {
		rec = &recordTB{TB: t} // Cleanup, Fatalf and Logf go to the subtest; Errorf is recorded
		h := scriptrun.Store(rec, testPolicy())
		fix := dial(t, h.Store, "rl_fixture")
		do(t, fix, "SET", stray, "x")
		do(t, fix, "SET", damaged, "x") // a string where §A3.2 has a hash
		h.Damaged(damaged)
		if msgs := rec.messages(); len(msgs) > 0 {
			t.Fatalf("errors before teardown: %q", msgs)
		}
	})
	want := []string{`store audit: "rl:junk": not a key of the §A3.2 layout`}
	if got := rec.messages(); !reflect.DeepEqual(got, want) {
		t.Errorf("the teardown audit reported %q, want exactly %q", got, want)
	}
}
