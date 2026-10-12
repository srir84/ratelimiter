//go:build conformance

package conformance

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
)

func TestC2_StoreVersion(t *testing.T) {
	bin, err := storeproc.Bin()
	if err != nil {
		t.Fatal(err)
	}
	product, version, err := storeproc.Version(t.Context(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible(product, version) {
		t.Fatalf("%s is %s %s: want Valkey 9.1.x; a fallback store is not conformance-eligible (DESIGN §C1, §C2)", bin, product, version)
	}
	// The running server reports the same version.
	s := start(t, storeproc.Fast)
	srv := info(t, conn(t, s, "rl_ops"), "server")
	if srv["server_name"] != "valkey" || srv["valkey_version"] != version {
		t.Fatalf("INFO server: server_name %q, valkey_version %q; want valkey %s", srv["server_name"], srv["valkey_version"], version)
	}
}

// checkProfile compares CONFIG GET (as rl_ops) with the profile's DESIGN §A2 settings
// and returns the rl_ops connection.
func checkProfile(t *testing.T, p storeproc.Profile, want map[string]string) *storeproc.Client {
	t.Helper()
	s := start(t, p)
	ops := conn(t, s, "rl_ops")
	for name, v := range want {
		if got := configGet(t, ops, name); got != v {
			t.Errorf("%s profile: %s = %q, want %q", p, name, got, v)
		}
	}
	return ops
}

// checkFileLimit checks that the store runs under the spawn wrapper's file limit: exactly
// 8,192 descriptors, since /bin/sh's ulimit -n sets the soft and the hard limit (DESIGN
// §B1). Valkey keeps 32 in reserve, so maxclients 8000 fits and 8161 does not. A store
// spawned without the wrapper fails one of the two: from the macOS default soft limit of
// 256, Valkey raises its own limit to 4,128 and caps the hard limit there, so 8000 is
// refused; from a high inherited limit, 8161 is accepted. ops is an rl_ops connection.
func checkFileLimit(t *testing.T, ops *storeproc.Client) {
	t.Helper()
	set := func(n string) storeproc.Reply {
		t.Helper()
		rep, err := ops.Do("CONFIG", "SET", "maxclients", n)
		if err != nil {
			t.Fatalf("CONFIG SET maxclients %s: %v", n, err)
		}
		return rep
	}
	if rep := set("8000"); rep.Kind != '+' {
		t.Fatalf("CONFIG SET maxclients 8000: %s; want OK: the store has fewer than 8,032 descriptors (spawned without ulimit -n 8192?)", rep)
	}
	if rep := set("8161"); rep.Kind != '-' || !strings.Contains(rep.Str, "try with 8160") {
		t.Errorf("CONFIG SET maxclients 8161: %s; want a refusal naming 8160: the store's file limit is not the wrapper's 8,192 (spawned without ulimit -n 8192?)", rep)
	}
	configSet(t, ops, "maxclients", "4096")
	if got := configGet(t, ops, "maxclients"); got != "4096" {
		t.Fatalf("maxclients %q after restoring 4096", got)
	}
}

func TestC2_DurableProfile(t *testing.T) {
	ops := checkProfile(t, storeproc.Durable, map[string]string{
		"appendonly":                  "yes",
		"appendfsync":                 "always",
		"no-appendfsync-on-rewrite":   "no",
		"aof-use-rdb-preamble":        "yes",
		"save":                        "",
		"maxmemory":                   strconv.Itoa(512 << 20),
		"maxmemory-policy":            "noeviction",
		"maxclients":                  "4096", // lowered when the hard file limit is below maxclients + 32 (DESIGN §B1)
		"auto-aof-rewrite-percentage": "100",
		"auto-aof-rewrite-min-size":   strconv.Itoa(1 << 30),
		"bind":                        "127.0.0.1",
		"protected-mode":              "yes",
	})
	checkFileLimit(t, ops)
}

func TestC2_FastProfile(t *testing.T) {
	checkProfile(t, storeproc.Fast, map[string]string{
		"appendonly":                  "yes",
		"appendfsync":                 "no",
		"no-appendfsync-on-rewrite":   "no",
		"aof-use-rdb-preamble":        "yes",
		"save":                        "",
		"maxmemory":                   strconv.Itoa(128 << 20),
		"maxmemory-policy":            "noeviction",
		"maxclients":                  "4096",
		"auto-aof-rewrite-percentage": "0",
		"bind":                        "127.0.0.1",
		"protected-mode":              "yes",
	})
}

func TestC2_DefaultUserOff(t *testing.T) {
	s := start(t, storeproc.Fast)
	c, err := storeproc.Dial(s.Addr, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	refused(t, c, "NOAUTH", "PING")
}

func TestC2_Row01_LuaDoubles(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	rep := do(t, lib, evalsha(load(t, lib, "probe_row01_lua"), nil)...)
	if rep.Kind != '*' || len(rep.Arr) != 2 {
		t.Fatalf("reply %s, want [_VERSION, flag]", rep)
	}
	if rep.Arr[0].Str != "Lua 5.1" {
		t.Errorf("_VERSION = %s, want \"Lua 5.1\"", rep.Arr[0])
	}
	if rep.Arr[1].Kind != ':' || rep.Arr[1].Int != 1 {
		t.Errorf("2^53 + 1 == 2^53 gave %s, want 1 (true: numbers are IEEE doubles)", rep.Arr[1])
	}
}

func TestC2_Row02_ExactIntegers(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	const n = 1<<53 - 1
	rep := do(t, lib, evalsha(load(t, lib, "probe_row02_exact"), []string{"rl:conf:row02:h", "rl:conf:row02:z"}, strconv.Itoa(n))...)
	if rep.Kind != '*' || len(rep.Arr) != 3 {
		t.Fatalf("reply %s, want [integer, stored value, score]", rep)
	}
	if rep.Arr[0].Kind != ':' || rep.Arr[0].Int != n {
		t.Errorf("integer reply %s, want %d", rep.Arr[0], n)
	}
	if rep.Arr[1].Str != strconv.Itoa(n) {
		t.Errorf("number passed to HSET stored as %s, want %d", rep.Arr[1], n)
	}
	if rep.Arr[2].Str != strconv.Itoa(n) {
		t.Errorf("ZADD/ZSCORE score %s, want %d", rep.Arr[2], n)
	}
}

func TestC2_Row03_Tostring(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	rep := do(t, lib, evalsha(load(t, lib, "probe_row03_tostring"), nil)...)
	if rep.Kind != '*' || len(rep.Arr) != 2 {
		t.Fatalf("reply %s, want [tostring, string.format]", rep)
	}
	if rep.Arr[0].Str != "1.7600000000001e+15" {
		t.Errorf("tostring(1760000000000123) = %s, want \"1.7600000000001e+15\"", rep.Arr[0])
	}
	if rep.Arr[1].Str != "1760000000000123" {
		t.Errorf("string.format('%%d', 1760000000000123) = %s, want \"1760000000000123\"", rep.Arr[1])
	}
}

func TestC2_Row04_Time(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	for _, c := range []struct {
		probe string
		keys  []string
	}{
		{"probe_row04_time_write", []string{"rl:conf:row04"}},
		{"probe_row04_time_read", nil},
	} {
		sha := load(t, lib, c.probe)
		before := time.Now()
		rep := do(t, lib, evalsha(sha, c.keys)...)
		after := time.Now()
		if rep.Kind != '*' || len(rep.Arr) != 2 {
			t.Fatalf("%s: reply %s, want {seconds, microseconds}", c.probe, rep)
		}
		sec, err1 := strconv.ParseInt(rep.Arr[0].Str, 10, 64)
		usec, err2 := strconv.ParseInt(rep.Arr[1].Str, 10, 64)
		if err1 != nil || err2 != nil || usec < 0 || usec >= 1e6 {
			t.Fatalf("%s: TIME reply %s", c.probe, rep)
		}
		store := sec*1e6 + usec
		lo, hi := before.UnixMicro()-1e6, after.UnixMicro()+1e6
		if store < lo || store > hi {
			t.Errorf("%s: TIME %d µs is more than 1 s from Go's clock [%d, %d]", c.probe, store, lo+1e6, hi-1e6)
		}
	}
}

func TestC2_Row05_ErrorAfterWrite(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	key := "rl:conf:row05"
	rep, err := lib.Do(evalsha(load(t, lib, "probe_row05_error"), []string{key})...)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Kind != '-' || !strings.Contains(rep.Str, "probe error after a write") {
		t.Fatalf("reply %s, want the script's error", rep)
	}
	if got := integer(t, "HEXISTS", do(t, lib, evalsha(load(t, lib, "probe_read"), []string{key}, "f")...)); got != 1 {
		t.Fatal("the write before the error was rolled back; DESIGN §A3.0 row 5 says it stays")
	}
}

func TestC2_Row06_OOM(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib, ops, fx := conn(t, s, "rl_lib"), conn(t, s, "rl_ops"), conn(t, s, "rl_fixture")
	write, writeOOM, read := load(t, lib, "probe_write"), load(t, lib, "probe_write_oom"), load(t, lib, "probe_read")
	// About 2 MB of data, then maxmemory at half of used_memory.
	fill := make([][]string, 32)
	value := strings.Repeat("x", 64<<10)
	for i := range fill {
		fill[i] = []string{"SET", "rl:conf:row06:fill:" + strconv.Itoa(i), value}
	}
	pipelined(t, []*storeproc.Client{fx}, fill)
	used, err := strconv.ParseInt(info(t, ops, "memory")["used_memory"], 10, 64)
	if err != nil {
		t.Fatalf("INFO memory used_memory: %v", err)
	}
	orig := configGet(t, ops, "maxmemory")
	configSet(t, ops, "maxmemory", strconv.FormatInt(used/2, 10))

	refused(t, lib, "OOM", evalsha(write, []string{"rl:conf:row06:plain"}, "f", "1")...)
	integer(t, "allow-oom probe", do(t, lib, evalsha(writeOOM, []string{"rl:conf:row06:oom"}, "f", "1")...))
	integer(t, "no-writes probe", do(t, lib, evalsha(read, []string{"rl:conf:row06:oom"}, "f")...))

	configSet(t, ops, "maxmemory", orig)
	if got := keyType(t, fx, "rl:conf:row06:plain"); got != "none" {
		t.Errorf("the refused #!lua probe's key is %q, want none (refused before it ran)", got)
	}
	if got := keyType(t, fx, "rl:conf:row06:oom"); got != "hash" {
		t.Errorf("the allow-oom probe's key is %q, want hash (it ran and wrote)", got)
	}
}

func TestC2_Row07_NoScript(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	refused(t, lib, "NOSCRIPT", evalsha(strings.Repeat("0", 40), nil)...)
}

func TestC2_Row10_NilArray(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	rep := do(t, lib, evalsha(load(t, lib, "probe_row10_nil"), nil)...)
	if rep.Kind != '*' || len(rep.Arr) != 1 || rep.Arr[0].Kind != ':' || rep.Arr[0].Int != 1 {
		t.Fatalf("{1, nil, 3} replied %s, want [:1] (the array stops at the nil)", rep)
	}
}

func TestC2_Row11_Unpack(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib, fx := conn(t, s, "rl_lib"), conn(t, s, "rl_fixture")
	sha := load(t, lib, "probe_row11_unpack")
	key := "rl:conf:row11"
	rep, err := lib.Do(evalsha(sha, []string{key}, "10000")...)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Kind != '-' || !strings.Contains(rep.Str, "too many results to unpack") {
		t.Fatalf("unpack of 10,000: reply %s, want \"too many results to unpack\"", rep)
	}
	hset := []string{"HSET", key}
	for i := 1; i <= 500; i++ {
		hset = append(hset, "f"+strconv.Itoa(i), "1")
	}
	do(t, fx, hset...)
	if got := integer(t, "HDEL of 500", do(t, lib, evalsha(sha, []string{key}, "500")...)); got != 500 {
		t.Fatalf("HDEL of 500 through unpack removed %d, want 500", got)
	}
}

func TestC2_Row12_ACLInner(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	rep, err := lib.Do(evalsha(load(t, lib, "probe_row12_hget"), []string{"rl:conf:row12"})...)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Kind != '-' || !strings.Contains(rep.Str, "no permissions to run the 'hget' command") {
		t.Errorf("rl_lib script calling HGET: reply %s, want \"no permissions to run the 'hget' command\"", rep)
	}
	refused(t, lib, "NOPERM", "FLUSHALL")
}

func TestC2_Row16_Busy(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib, ops, fx := conn(t, s, "rl_lib"), conn(t, s, "rl_ops"), conn(t, s, "rl_fixture")
	write := load(t, lib, "probe_write")
	configSet(t, ops, "busy-reply-threshold", "100")
	key := "rl:conf:row16:busy"

	type result struct {
		rep storeproc.Reply
		err error
		dur time.Duration
	}
	loop := probe(t, "busy_loop")
	done := make(chan result, 1)
	go func() {
		t0 := time.Now()
		rep, err := fx.Do("EVAL", loop, "0", "1.8")
		done <- result{rep, err, time.Since(t0)}
	}()
	time.Sleep(400 * time.Millisecond) // past the 100 ms threshold
	rep, err := lib.Do(evalsha(write, []string{key}, "f", "1")...)
	busy := <-done
	if busy.err != nil || busy.rep.Kind != ':' {
		t.Fatalf("rl_fixture's no-write loop: %v %v", busy.rep, busy.err)
	}
	if busy.dur < 1500*time.Millisecond {
		t.Fatalf("the no-write loop ran %v, want ≥ 1.5 s", busy.dur)
	}
	if err != nil {
		t.Fatal(err)
	}
	if rep.Kind != '-' || !strings.HasPrefix(rep.Str, "BUSY") {
		t.Fatalf("rl_lib's probe during the loop: reply %s, want BUSY…", rep)
	}
	if got := keyType(t, fx, key); got != "none" {
		t.Fatalf("the BUSY-refused probe's key is %q, want none (refused before it ran)", got)
	}
}

func TestC2_SortedSets(t *testing.T) {
	s := start(t, storeproc.Fast)
	lib := conn(t, s, "rl_lib")
	zadd, zrange := load(t, lib, "probe_zadd"), load(t, lib, "probe_zrange")
	key := "rl:conf:zset"
	steps := []struct {
		flag, score, member string
		want                string // the member's score afterwards; "" = absent
	}{
		{"", "10", "a", "10"},
		{"", "20", "b", "20"},
		{"", "30", "c", "30"},
		{"GT", "5", "a", "10"},  // GT never lowers
		{"GT", "15", "a", "15"}, // GT raises
		{"GT", "1", "d", "1"},   // GT still adds a new member
		{"XX", "40", "e", ""},   // XX never adds
		{"XX", "25", "b", "25"}, // XX updates an existing member
		{"LT", "30", "a", "15"}, // LT never raises
		{"LT", "12", "a", "12"}, // LT lowers
	}
	for _, st := range steps {
		rep := do(t, lib, evalsha(zadd, []string{key}, st.flag, st.score, st.member)...)
		switch {
		case st.want == "" && (rep.Kind != '$' || !rep.Nil):
			t.Errorf("ZADD %s %s %s: score %s, want absent", st.flag, st.score, st.member, rep)
		case st.want != "" && (rep.Kind != '$' || rep.Str != st.want):
			t.Errorf("ZADD %s %s %s: score %s, want %s", st.flag, st.score, st.member, rep, st.want)
		}
	}
	// Scores now: d 1, a 12, b 25, c 30.
	for _, c := range []struct {
		min, max, offset, count string
		want                    []string
	}{
		{"0", "100", "1", "2", []string{"a", "12", "b", "25"}},
		{"(12", "+inf", "0", "10", []string{"b", "25", "c", "30"}},
		{"-inf", "12", "0", "1", []string{"d", "1"}},
		{"31", "+inf", "0", "10", nil},
	} {
		rep := do(t, lib, evalsha(zrange, []string{key}, c.min, c.max, c.offset, c.count)...)
		var got []string
		for _, e := range rep.Arr {
			got = append(got, e.Str)
		}
		if rep.Kind != '*' || strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("ZRANGE %s %s BYSCORE LIMIT %s %s WITHSCORES = %s, want %v", c.min, c.max, c.offset, c.count, rep, c.want)
		}
	}
}

func TestC2_CommandstatsCountsScriptCommands(t *testing.T) {
	s := start(t, storeproc.Fast)
	admin, ops := conn(t, s, "rl_admin"), conn(t, s, "rl_ops")
	sha := load(t, admin, "probe_commandstats")
	calls := func() map[string]int64 {
		stats := info(t, ops, "commandstats")
		m := map[string]int64{}
		for _, cmd := range [...]string{"evalsha", "hset", "hget"} {
			m[cmd] = callsOf(t, stats["cmdstat_"+cmd])
		}
		return m
	}
	before := calls()
	for range 2 {
		do(t, admin, evalsha(sha, []string{"rl:conf:cmdstat"})...)
	}
	after := calls()
	for cmd, want := range map[string]int64{"evalsha": 2, "hset": 2, "hget": 4} {
		if d := after[cmd] - before[cmd]; d != want {
			t.Errorf("INFO commandstats %s delta %d, want %d", cmd, d, want)
		}
	}
}

// callsOf reads calls=N from a cmdstat_<name> value; a missing entry is 0 calls.
func callsOf(t *testing.T, v string) int64 {
	t.Helper()
	if v == "" {
		return 0
	}
	for _, kv := range strings.Split(v, ",") {
		if n, ok := strings.CutPrefix(kv, "calls="); ok {
			c, err := strconv.ParseInt(n, 10, 64)
			if err != nil || c < 0 {
				t.Fatalf("commandstats entry %q", v)
			}
			return c
		}
	}
	t.Fatalf("commandstats entry %q has no calls=", v)
	return 0
}
