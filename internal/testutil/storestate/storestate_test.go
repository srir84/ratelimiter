//go:build scripttest

package storestate_test

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/internal/testutil/storestate"
)

const (
	// fillKeys is how many string keys the test store holds besides its hashes and sorted
	// sets: well over SCAN's COUNT 1000, so a snapshot takes several SCAN pages.
	fillKeys = 2000
	// callBudget is how many calls one Snapshot may make through guardConn: about 8 times
	// the roughly 6,000 a snapshot of the test store needs (up to 3 reads a key).
	callBudget = 50_000
)

// answer is a reply that hookConn returns instead of the store's.
type answer struct {
	rep storeproc.Reply
	err error
}

// hookConn is a storestate.Conn over a real client. It records every call; replace, when
// not nil, may answer a call instead of the store (it gets the call's number, from 1), and
// rewrite, when not nil, may change a forwarded call's reply.
type hookConn struct {
	c       *storeproc.Client
	calls   [][]string
	replace func(n int, args []string) *answer
	rewrite func(args []string, rep storeproc.Reply) storeproc.Reply
}

func (h *hookConn) Do(args ...string) (storeproc.Reply, error) {
	h.calls = append(h.calls, args)
	if h.replace != nil {
		if a := h.replace(len(h.calls), args); a != nil {
			return a.rep, a.err
		}
	}
	rep, err := h.c.Do(args...)
	if err == nil && h.rewrite != nil {
		rep = h.rewrite(args, rep)
	}
	return rep, err
}

// guardConn is the storestate.Conn every Snapshot in TestSnapshotDetectsEachChange goes
// through, one per Snapshot call. Each SCAN must send the cursor the reply to the SCAN
// before it returned ("0" for the first), and no call may pass callBudget: otherwise it
// fails the test and the call, so a Snapshot that loops fails at once instead of hanging
// the test until its timeout (which also skips the store's cleanup).
type guardConn struct {
	t      testing.TB
	next   storestate.Conn
	calls  int
	cursor string // the cursor the next SCAN must send
}

func guard(t testing.TB, next storestate.Conn) *guardConn {
	return &guardConn{t: t, next: next, cursor: "0"}
}

func (g *guardConn) Do(args ...string) (storeproc.Reply, error) {
	g.calls++
	if g.calls > callBudget {
		g.t.Errorf("call %d %q: Snapshot made more than %d calls", g.calls, args, callBudget)
		return storeproc.Reply{}, fmt.Errorf("call %d: over the budget of %d calls", g.calls, callBudget)
	}
	if args[0] == "SCAN" && (len(args) < 2 || args[1] != g.cursor) {
		g.t.Errorf("call %d %q: want cursor %q, which the previous SCAN reply returned", g.calls, args, g.cursor)
		return storeproc.Reply{}, fmt.Errorf("call %d: SCAN with the wrong cursor", g.calls)
	}
	rep, err := g.next.Do(args...)
	if err == nil && args[0] == "SCAN" && rep.Kind == '*' && len(rep.Arr) == 2 && rep.Arr[0].Kind == '$' {
		g.cursor = rep.Arr[0].Str
	}
	return rep, err
}

func dial(t *testing.T, s *storeproc.Store, user string) *storeproc.Client {
	t.Helper()
	c, err := s.Conn(user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// do sends cmds in one pipeline and fails the test on any error.
func do(t *testing.T, c *storeproc.Client, cmds ...[]string) {
	t.Helper()
	reps, err := c.Pipeline(cmds)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range reps {
		if r.Kind == '-' {
			t.Fatalf("%q: %s", cmds[i], r)
		}
	}
}

// snapshot takes a snapshot through a guardConn over c and fails the test on an error.
func snapshot(t *testing.T, c storestate.Conn) storestate.State {
	t.Helper()
	st, err := storestate.Snapshot(guard(t, c))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// changedKeys returns the keys whose values differ between a and b, sorted.
func changedKeys(a, b storestate.State) []string {
	var out []string
	for k, va := range a {
		if vb, ok := b[k]; !ok || !reflect.DeepEqual(va, vb) {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func bulk(s string) storeproc.Reply { return storeproc.Reply{Kind: '$', Str: s} }

func errReply(s string) storeproc.Reply { return storeproc.Reply{Kind: '-', Str: s} }

// TestSnapshotDetectsEachChange (plan S4, L2): on a store with more than 1,000 keys, a
// snapshot as rl_lib reads every key exactly, an unchanged store compares equal, and each
// kind of change shows up as a difference in exactly the changed key. A failed read, an
// unexpected error reply or a reply of the wrong shape fails the snapshot, with no partial
// State; a key SCAN returns twice is read once, and a key gone before its read is left out.
// Every Snapshot goes through a guardConn, which checks each SCAN's cursor and bounds the
// number of calls.
func TestSnapshotDetectsEachChange(t *testing.T) {
	s := storeproc.Start(t, storeproc.Options{})
	fix := dial(t, s, "rl_fixture")
	lib := dial(t, s, "rl_lib")

	// Each change has a key of its own, which the base fixture writes (but for "key
	// added"), so each change subtest stands alone (AGENTS.md §5): it compares the
	// snapshots taken just before and just after its own change.
	changes := []struct {
		name  string
		key   string
		setup []string         // writes the key's base value; nil: no key before the change
		was   storestate.Value // the key's base value
		cmds  [][]string       // the change
		want  storestate.Value // the key's value afterwards
		gone  bool             // the key no longer exists
	}{
		{name: "hash field added", key: "rl:c:hadd",
			setup: []string{"HSET", "rl:c:hadd", "a", "1", "b", "2"}, was: hash("a", "1", "b", "2"),
			cmds: [][]string{{"HSET", "rl:c:hadd", "c", "3"}}, want: hash("a", "1", "b", "2", "c", "3")},
		{name: "hash field changed", key: "rl:c:hset",
			setup: []string{"HSET", "rl:c:hset", "a", "1", "b", "2"}, was: hash("a", "1", "b", "2"),
			cmds: [][]string{{"HSET", "rl:c:hset", "a", "9"}}, want: hash("a", "9", "b", "2")},
		{name: "hash field deleted", key: "rl:c:hdel",
			setup: []string{"HSET", "rl:c:hdel", "a", "1", "b", "2"}, was: hash("a", "1", "b", "2"),
			cmds: [][]string{{"HDEL", "rl:c:hdel", "b"}}, want: hash("a", "1")},
		{name: "zset member added", key: "rl:c:zadd",
			setup: []string{"ZADD", "rl:c:zadd", "1", "m1", "1760000060300000", "m2"}, was: zset("m1", "1", "m2", "1760000060300000"),
			cmds: [][]string{{"ZADD", "rl:c:zadd", "3", "m3"}}, want: zset("m1", "1", "m2", "1760000060300000", "m3", "3")},
		{name: "zset member re-scored", key: "rl:c:zscore",
			setup: []string{"ZADD", "rl:c:zscore", "1", "m1", "1760000060300000", "m2"}, was: zset("m1", "1", "m2", "1760000060300000"),
			cmds: [][]string{{"ZADD", "rl:c:zscore", "1.5", "m1"}}, want: zset("m1", "1.5", "m2", "1760000060300000")},
		{name: "zset member removed", key: "rl:c:zrem",
			setup: []string{"ZADD", "rl:c:zrem", "1", "m1", "1760000060300000", "m2"}, was: zset("m1", "1", "m2", "1760000060300000"),
			cmds: [][]string{{"ZREM", "rl:c:zrem", "m2"}}, want: zset("m1", "1")},
		{name: "string changed", key: "rl:c:sset",
			setup: []string{"SET", "rl:c:sset", "old"}, was: str("old"),
			cmds: [][]string{{"SET", "rl:c:sset", "new"}}, want: str("new")},
		{name: "key added", key: "rl:c:new",
			cmds: [][]string{{"SET", "rl:c:new", "x"}}, want: str("x")},
		{name: "key deleted", key: "rl:c:del",
			setup: []string{"SET", "rl:c:del", "x"}, was: str("x"),
			cmds: [][]string{{"DEL", "rl:c:del"}}, gone: true},
		{name: "type changed: string to hash", key: "rl:c:s2h",
			setup: []string{"SET", "rl:c:s2h", "x"}, was: str("x"),
			cmds: [][]string{{"DEL", "rl:c:s2h"}, {"HSET", "rl:c:s2h", "f", "v"}}, want: hash("f", "v")},
		{name: "type changed: hash to sorted set", key: "rl:c:h2z",
			setup: []string{"HSET", "rl:c:h2z", "a", "1"}, was: hash("a", "1"),
			cmds: [][]string{{"DEL", "rl:c:h2z"}, {"ZADD", "rl:c:h2z", "7", "m"}}, want: zset("m", "7")},
		{name: "type changed: sorted set to string", key: "rl:c:z2s",
			setup: []string{"ZADD", "rl:c:z2s", "5", "m"}, was: zset("m", "5"),
			cmds: [][]string{{"DEL", "rl:c:z2s"}, {"SET", "rl:c:z2s", "s"}}, want: str("s")},
	}

	cmds := [][]string{
		{"HSET", "rl:h:0", "a", "1", "b", "2"},
		{"ZADD", "rl:z:0", "1", "m1", "1760000060300000", "m2"},
		{"SET", "rl:s:0", "old"},
	}
	for _, c := range changes {
		if c.setup != nil {
			cmds = append(cmds, c.setup)
		}
	}
	for i := range fillKeys {
		cmds = append(cmds, []string{"SET", fmt.Sprintf("rl:fill:%04d", i), fmt.Sprintf("v%04d", i)})
	}
	do(t, fix, cmds...)

	base := snapshot(t, lib)
	if len(base) != len(cmds) {
		t.Fatalf("the snapshot holds %d keys, want %d", len(base), len(cmds))
	}
	for name, want := range map[string]storestate.Value{
		"rl:h:0":       hash("a", "1", "b", "2"),
		"rl:z:0":       zset("m1", "1", "m2", "1760000060300000"),
		"rl:s:0":       str("old"),
		"rl:fill:0000": str("v0000"),
		"rl:fill:1999": str("v1999"),
	} {
		if got := base[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %+v, want %+v", name, got, want)
		}
	}

	t.Run("unchanged store compares equal", func(t *testing.T) {
		if again := snapshot(t, lib); !reflect.DeepEqual(base, again) {
			t.Errorf("two snapshots of an unchanged store differ in %q", changedKeys(base, again))
		}
	})

	t.Run("SCAN pages", func(t *testing.T) {
		h := &hookConn{c: lib}
		snapshot(t, h)
		scans := 0
		for _, c := range h.calls {
			if c[0] == "SCAN" {
				scans++
			}
		}
		if scans < 2 {
			t.Errorf("%d SCAN calls for %d keys, want at least 2", scans, len(base))
		}
	})

	for _, c := range changes {
		t.Run(c.name, func(t *testing.T) {
			before := snapshot(t, lib)
			if got, ok := before[c.key]; ok != (c.setup != nil) || (ok && !reflect.DeepEqual(got, c.was)) {
				t.Fatalf("before the change: %s = %+v (present: %v), want %+v (present: %v)", c.key, got, ok, c.was, c.setup != nil)
			}
			do(t, fix, c.cmds...)
			after := snapshot(t, lib)
			if reflect.DeepEqual(before, after) {
				t.Fatal("the snapshot did not change")
			}
			if got := changedKeys(before, after); !slices.Equal(got, []string{c.key}) {
				t.Fatalf("changed keys %q, want only %q", got, c.key)
			}
			got, ok := after[c.key]
			switch {
			case c.gone && ok:
				t.Errorf("%s is still in the snapshot: %+v", c.key, got)
			case !c.gone && !reflect.DeepEqual(got, c.want):
				t.Errorf("%s = %+v, want %+v", c.key, got, c.want)
			}
		})
	}

	t.Run("failed read fails the snapshot", func(t *testing.T) {
		h := &hookConn{c: lib}
		snapshot(t, h)
		first := func(cmd string) int {
			return slices.IndexFunc(h.calls, func(c []string) bool { return c[0] == cmd }) + 1
		}
		picks := map[string]int{
			"first SCAN":    1,
			"second SCAN":   2,
			"first HGETALL": first("HGETALL"),
			"first ZRANGE":  first("ZRANGE"),
			"first GET":     first("GET"),
			"last call":     len(h.calls),
		}
		injected := errors.New("injected read error")
		for label, n := range picks {
			if n < 1 || (label == "second SCAN" && h.calls[1][0] != "SCAN") {
				t.Fatalf("%s: no such call in %d calls", label, len(h.calls))
			}
			for _, a := range []answer{{err: injected}, {rep: errReply("ERR injected")}, {rep: errReply("NOPERM injected")}} {
				f := &hookConn{c: lib, replace: func(i int, _ []string) *answer {
					if i == n {
						return &a
					}
					return nil
				}}
				st, err := storestate.Snapshot(guard(t, f))
				switch {
				case err == nil:
					t.Errorf("%s (call %d) answered %+v: no error", label, n, a)
				case st != nil:
					t.Errorf("%s (call %d) answered %+v: a partial State of %d keys", label, n, a, len(st))
				case a.err != nil && !errors.Is(err, injected):
					t.Errorf("%s (call %d): error %v does not wrap the read error", label, n, err)
				case len(f.calls) != n:
					t.Errorf("%s (call %d) answered %+v: %d calls, want none after the failed one", label, n, a, len(f.calls))
				}
			}
		}
	})

	t.Run("malformed reply fails the snapshot", func(t *testing.T) {
		pages := func(cursor storeproc.Reply, names ...storeproc.Reply) *answer {
			return &answer{rep: storeproc.Reply{Kind: '*', Arr: []storeproc.Reply{cursor, {Kind: '*', Arr: names}}}}
		}
		for _, c := range []struct {
			name string
			cmd  string // the command whose reply is replaced
			key  string // its key ("" for SCAN)
			rep  *answer
		}{
			{"SCAN reply not an array", "SCAN", "", &answer{rep: storeproc.Reply{Kind: ':', Int: 0}}},
			{"SCAN reply of one element", "SCAN", "", &answer{rep: storeproc.Reply{Kind: '*', Arr: []storeproc.Reply{bulk("0")}}}},
			{"SCAN cursor not a number", "SCAN", "", pages(bulk("x"))},
			{"SCAN cursor an integer reply", "SCAN", "", pages(storeproc.Reply{Kind: ':', Int: 0})},
			{"SCAN name not a bulk string", "SCAN", "", pages(bulk("0"), storeproc.Reply{Kind: ':', Int: 1})},
			{"HGETALL odd length", "HGETALL", "rl:h:0", &answer{rep: storeproc.Reply{Kind: '*', Arr: []storeproc.Reply{bulk("a"), bulk("1"), bulk("c")}}}},
			{"HGETALL field twice", "HGETALL", "rl:h:0", &answer{rep: storeproc.Reply{Kind: '*', Arr: []storeproc.Reply{bulk("a"), bulk("1"), bulk("a"), bulk("2")}}}},
			{"HGETALL nil array", "HGETALL", "rl:h:0", &answer{rep: storeproc.Reply{Kind: '*', Nil: true}}},
			{"ZRANGE nil element", "ZRANGE", "rl:z:0", &answer{rep: storeproc.Reply{Kind: '*', Arr: []storeproc.Reply{bulk("m1"), {Kind: '$', Nil: true}}}}},
			{"GET integer reply", "GET", "rl:s:0", &answer{rep: storeproc.Reply{Kind: ':', Int: 5}}},
			{"every read WRONGTYPE", "", "rl:s:0", &answer{rep: errReply("WRONGTYPE Operation against a key holding the wrong kind of value")}},
		} {
			f := &hookConn{c: lib, replace: func(_ int, args []string) *answer {
				switch {
				case c.key == "" && args[0] == c.cmd:
					return c.rep
				case c.key != "" && args[0] != "SCAN" && args[1] == c.key && (c.cmd == "" || args[0] == c.cmd):
					return c.rep
				}
				return nil
			}}
			if st, err := storestate.Snapshot(guard(t, f)); err == nil || st != nil {
				t.Errorf("%s: Snapshot returned %d keys, error %v; want no State and an error", c.name, len(st), err)
			}
		}
	})

	t.Run("key that SCAN returns twice is read once", func(t *testing.T) {
		current := snapshot(t, lib)
		pagesSeen := 0
		h := &hookConn{c: lib, rewrite: func(args []string, rep storeproc.Reply) storeproc.Reply {
			if args[0] != "SCAN" || len(rep.Arr) != 2 {
				return rep
			}
			// Every page also returns rl:h:0, which one page returns anyway, and rl:gone,
			// which does not exist: read once and then left out.
			pagesSeen++
			rep.Arr[1].Arr = append(rep.Arr[1].Arr, bulk("rl:h:0"), bulk("rl:gone"))
			return rep
		}}
		st := snapshot(t, h)
		if pagesSeen < 2 {
			t.Fatalf("%d SCAN pages, want at least 2", pagesSeen)
		}
		if !reflect.DeepEqual(st, current) {
			t.Errorf("snapshot differs from the store in %q", changedKeys(current, st))
		}
		reads := map[string]int{}
		for _, c := range h.calls {
			if c[0] == "HGETALL" {
				reads[c[1]]++
			}
		}
		for name, n := range reads {
			if n != 1 {
				t.Errorf("%s read %d times", name, n)
			}
		}
		if want := len(current) + 1; len(reads) != want {
			t.Errorf("%d keys read, want %d (every key and rl:gone)", len(reads), want)
		}
		if reads["rl:gone"] != 1 {
			t.Errorf("rl:gone read %d times, want 1", reads["rl:gone"])
		}
	})

	t.Run("key gone before its later reads is left out", func(t *testing.T) {
		// The replies the store gives once a key is deleted, after an earlier read of it
		// answered WRONGTYPE: an empty ZRANGE array, or a nil GET.
		current := snapshot(t, lib)
		wrongType := &answer{rep: errReply("WRONGTYPE Operation against a key holding the wrong kind of value")}
		for _, c := range []struct {
			name    string
			key     string
			answers map[string]*answer // the replies to the key's reads, by command
		}{
			{"WRONGTYPE, then an empty ZRANGE", "rl:z:0", map[string]*answer{
				"HGETALL": wrongType,
				"ZRANGE":  {rep: storeproc.Reply{Kind: '*', Arr: []storeproc.Reply{}}},
			}},
			{"WRONGTYPE, WRONGTYPE, then a nil GET", "rl:s:0", map[string]*answer{
				"HGETALL": wrongType,
				"ZRANGE":  wrongType,
				"GET":     {rep: storeproc.Reply{Kind: '$', Nil: true}},
			}},
		} {
			f := &hookConn{c: lib, replace: func(_ int, args []string) *answer {
				if args[0] != "SCAN" && args[1] == c.key {
					return c.answers[args[0]]
				}
				return nil
			}}
			st, err := storestate.Snapshot(guard(t, f))
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
				continue
			}
			want := maps.Clone(current)
			delete(want, c.key)
			if !reflect.DeepEqual(st, want) {
				t.Errorf("%s: the snapshot differs from the store without %s in %q", c.name, c.key, changedKeys(want, st))
			}
		}
	})

	t.Run("reads follow HGETALL, ZRANGE, GET", func(t *testing.T) {
		h := &hookConn{c: lib}
		snapshot(t, h)
		perKey := map[string][]string{}
		for _, c := range h.calls {
			if c[0] != "SCAN" {
				perKey[c[1]] = append(perKey[c[1]], strconv.Quote(c[0])+" "+fmt.Sprint(c[2:]))
			}
		}
		for key, want := range map[string][]string{
			"rl:h:0": {`"HGETALL" []`},
			"rl:z:0": {`"HGETALL" []`, `"ZRANGE" [0 -1 WITHSCORES]`},
			"rl:s:0": {`"HGETALL" []`, `"ZRANGE" [0 -1 WITHSCORES]`, `"GET" []`},
		} {
			if got := perKey[key]; !slices.Equal(got, want) {
				t.Errorf("reads of %s: %q, want %q", key, got, want)
			}
		}
	})
}
