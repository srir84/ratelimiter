//go:build scripttest

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ratelimiter/internal/testutil/scriptrun"
	"ratelimiter/internal/testutil/storeproc"
)

// infoOf reads one INFO section through c.
func infoOf(t *testing.T, c *storeproc.Client, section string) map[string]string {
	t.Helper()
	r, err := c.Do("INFO", section)
	if err != nil || r.Kind != '$' {
		t.Fatalf("INFO %s: %v %s", section, err, r)
	}
	return storeproc.ParseInfo(r.Str)
}

// TestStoreInfoAndRewrite (DESIGN §A6; plan S8 step 3; L2, a real store as rl_admin):
// store-info --json holds the four INFO sections (persistence, clients, memory, stats) and the
// slow log, and store-rewrite exits 0 once its rewrite is done: the store then shows
// aof_rewrite_in_progress:0, one more completed rewrite and aof_last_bgrewrite_status:ok.
// No password reaches either output.
func TestStoreInfoAndRewrite(t *testing.T) {
	st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
	ops, err := st.Conn("rl_ops")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ops.Close() }()
	// Log every command, so the slow log holds the commands store-info itself sends.
	if r, err := ops.Do("CONFIG", "SET", "slowlog-log-slower-than", "0"); err != nil || r.Kind != '+' {
		t.Fatalf("CONFIG SET slowlog-log-slower-than 0: %v %s", err, r)
	}
	env := map[string]string{"RL_STORE_ADDR": st.Addr, "RL_ADMIN_PASSWORD": st.PW.Admin}
	noSecrets := func(what, out string) {
		t.Helper()
		for _, pw := range []string{st.PW.Lib, st.PW.Admin, st.PW.Fixture, st.PW.Ops} {
			if strings.Contains(out, pw) {
				t.Errorf("%s holds a store password:\n%s", what, out)
			}
		}
	}

	code, stdout, stderr := runWith(t, env, "store-info", "--json")
	noSecrets("store-info", stdout+stderr)
	if code != 0 {
		t.Fatalf("store-info --json: exit %d, want 0\n%s", code, stderr)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		t.Fatalf("store-info --json is not one JSON object: %v\n%s", err, stdout)
	}
	// One field each section always has, with the deploy/ profile's value where it is fixed.
	for sec, field := range map[string]string{
		"persistence": "aof_enabled", "clients": "connected_clients", "memory": "used_memory", "stats": "total_commands_processed",
	} {
		var fields map[string]string
		if err := json.Unmarshal(info[sec], &fields); err != nil {
			t.Errorf("section %s: %v (%s)", sec, err, info[sec])
			continue
		}
		if _, ok := fields[field]; !ok {
			t.Errorf("section %s has no %s field: %v", sec, field, fields)
		}
	}
	var persistence map[string]string
	if err := json.Unmarshal(info["persistence"], &persistence); err == nil && persistence["aof_enabled"] != "1" {
		t.Errorf("persistence aof_enabled = %q, want 1 (appendonly yes)", persistence["aof_enabled"])
	}
	var slow []slowEntry
	if err := json.Unmarshal(info["slowlog"], &slow); err != nil {
		t.Fatalf("slowlog: %v (%s)", err, info["slowlog"])
	}
	sawInfo := false
	for _, e := range slow {
		if len(e.Args) > 0 && strings.EqualFold(e.Args[0], "INFO") {
			sawInfo = true
		}
	}
	if !sawInfo {
		t.Errorf("the slow log has no INFO entry, though every command is logged: %+v", slow)
	}
	if len(info) != 5 {
		t.Errorf("store-info --json has %d members, want the four sections and slowlog: %s", len(info), stdout)
	}

	admin, err := st.Conn("rl_admin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	before, err := strconv.Atoi(infoOf(t, admin, "persistence")["aof_rewrites"])
	if err != nil {
		t.Fatalf("aof_rewrites before: %v", err)
	}
	code, stdout, stderr = runWith(t, env, "store-rewrite")
	noSecrets("store-rewrite", stdout+stderr)
	if code != 0 {
		t.Fatalf("store-rewrite: exit %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "aof_rewrite_in_progress    0") || !strings.Contains(stdout, "aof_last_bgrewrite_status  ok") {
		t.Errorf("store-rewrite printed:\n%s\nwant aof_rewrite_in_progress 0 and aof_last_bgrewrite_status ok", stdout)
	}
	p := infoOf(t, admin, "persistence")
	if p["aof_rewrite_in_progress"] != "0" || p["aof_rewrite_scheduled"] != "0" || p["aof_last_bgrewrite_status"] != "ok" {
		t.Errorf("after store-rewrite: aof_rewrite_in_progress:%s aof_rewrite_scheduled:%s aof_last_bgrewrite_status:%s, want 0, 0, ok",
			p["aof_rewrite_in_progress"], p["aof_rewrite_scheduled"], p["aof_last_bgrewrite_status"])
	}
	if after, err := strconv.Atoi(p["aof_rewrites"]); err != nil || after != before+1 {
		t.Errorf("aof_rewrites %s after store-rewrite, want %d", p["aof_rewrites"], before+1)
	}
}

// TestStoreRewriteAfterRunning (DESIGN §A6, §A8 planned restart; review A1; L2, a real store
// as rl_admin): store-rewrite started while a rewrite already runs waits for it to end, then
// runs its own and exits 0. The store then shows aof_rewrite_in_progress:0,
// aof_rewrite_scheduled:0, aof_last_bgrewrite_status:ok and two more completed rewrites, and
// its command stats show rladmin's two BGREWRITEAOF after the first: three calls, of which one
// failed (the "already in progress" reply). The first rewrite, sent as rl_ops over 100 keys
// written as rl_fixture, is slowed by rdb-key-save-delay 20,000 µs per key (about 2 s); the
// setting is set back to 0 once that rewrite runs (the forked rewrite keeps the delay it
// started with), and the store shows 0 at the end. No password reaches the output.
func TestStoreRewriteAfterRunning(t *testing.T) {
	st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
	conn := func(user string) *storeproc.Client {
		t.Helper()
		c, err := st.Conn(user)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	ops, fx, admin := conn("rl_ops"), conn("rl_fixture"), conn("rl_admin")
	setDelay := func(us string) {
		t.Helper()
		if r, err := ops.Do("CONFIG", "SET", "rdb-key-save-delay", us); err != nil || r.Kind != '+' {
			t.Fatalf("CONFIG SET rdb-key-save-delay %s: %v %s", us, err, r)
		}
	}
	for i := range 100 {
		if r, err := fx.Do("SET", "rl:fill:"+strconv.Itoa(i), "x"); err != nil || r.Kind != '+' {
			t.Fatalf("SET rl:fill:%d: %v %s", i, err, r)
		}
	}
	bgStats := func() string {
		t.Helper()
		line := infoOf(t, admin, "commandstats")["cmdstat_bgrewriteaof"]
		var calls, failed string
		for _, kv := range strings.Split(line, ",") {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "calls":
				calls = v
			case "failed_calls":
				failed = v
			}
		}
		return "calls=" + calls + " failed_calls=" + failed
	}
	before, err := strconv.Atoi(infoOf(t, admin, "persistence")["aof_rewrites"])
	if err != nil {
		t.Fatalf("aof_rewrites before: %v", err)
	}
	setDelay("20000")
	if r, err := ops.Do("BGREWRITEAOF"); err != nil || r.Kind != '+' || r.Str != "Background append only file rewriting started" {
		setDelay("0")
		t.Fatalf("the first BGREWRITEAOF: %v %s", err, r)
	}
	setDelay("0")
	if p := infoOf(t, admin, "persistence"); p["aof_rewrite_in_progress"] != "1" {
		t.Fatalf("aof_rewrite_in_progress:%s before store-rewrite, want 1 (the first rewrite running)", p["aof_rewrite_in_progress"])
	}
	if s := bgStats(); s != "calls=1 failed_calls=0" {
		t.Fatalf("BGREWRITEAOF stats before store-rewrite: %s, want calls=1 failed_calls=0", s)
	}

	env := map[string]string{"RL_STORE_ADDR": st.Addr, "RL_ADMIN_PASSWORD": st.PW.Admin}
	code, stdout, stderr := runWith(t, env, "store-rewrite")
	for _, pw := range []string{st.PW.Lib, st.PW.Admin, st.PW.Fixture, st.PW.Ops} {
		if strings.Contains(stdout+stderr, pw) {
			t.Errorf("store-rewrite's output holds a store password:\n%s%s", stdout, stderr)
		}
	}
	if code != 0 {
		t.Fatalf("store-rewrite while a rewrite runs: exit %d, want 0\n%s", code, stderr)
	}
	p := infoOf(t, admin, "persistence")
	if p["aof_rewrite_in_progress"] != "0" || p["aof_rewrite_scheduled"] != "0" || p["aof_last_bgrewrite_status"] != "ok" {
		t.Errorf("after store-rewrite: aof_rewrite_in_progress:%s aof_rewrite_scheduled:%s aof_last_bgrewrite_status:%s, want 0, 0, ok",
			p["aof_rewrite_in_progress"], p["aof_rewrite_scheduled"], p["aof_last_bgrewrite_status"])
	}
	if after, err := strconv.Atoi(p["aof_rewrites"]); err != nil || after != before+2 {
		t.Errorf("aof_rewrites %s after store-rewrite, want %d (the first rewrite and rladmin's)", p["aof_rewrites"], before+2)
	}
	if s := bgStats(); s != "calls=3 failed_calls=1" {
		t.Errorf("BGREWRITEAOF stats after store-rewrite: %s, want calls=3 failed_calls=1", s)
	}
	if r, err := ops.Do("CONFIG", "GET", "rdb-key-save-delay"); err != nil || len(r.Arr) != 2 || r.Arr[1].Str != "0" {
		t.Errorf("CONFIG GET rdb-key-save-delay at the end: %v %s, want 0", err, r)
	}
}

// TestAdminScriptsOnStore (DESIGN §A4.6, §A6; L2, a real store as rl_admin): every admin
// command runs its real script with the KEYS and ARGV of §A4.1 and reports its result: no
// epoch (exit 2 with the recovery hint), start-epoch, a second start-epoch without the flag
// (2), status and inspect, restore of a key that is not quarantined (2), restore of a
// quarantined key (new generation 1, the bucket of ADM-2), clear (the flag lifted), a stale
// --expect-epoch (2), a replace, and a controlled-clock start with no controlled time (4).
// Expected values are literals or the replies' own budget times. The store comes from
// scriptrun.Store, so its state is audited at teardown (DESIGN §C3) with the test's policy.
func TestAdminScriptsOnStore(t *testing.T) {
	pi, err := decodePolicy([]byte(policyTOML))
	if err != nil {
		t.Fatal(err)
	}
	st := scriptrun.Store(t, pi.p)
	env := map[string]string{"RL_STORE_ADDR": st.Addr, "RL_ADMIN_PASSWORD": st.PW.Admin}
	dir := t.TempDir()
	pol := filepath.Join(dir, "shared.toml")
	if err := os.WriteFile(pol, []byte(policyTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := func(args ...string) map[string]any {
		t.Helper()
		code, stdout, stderr := runWith(t, env, append(args, "--json")...)
		if code != 0 {
			t.Fatalf("rladmin %q: exit %d, want 0\n%s", args, code, stderr)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(stdout), &v); err != nil {
			t.Fatalf("rladmin %q: %v\n%s", args, err, stdout)
		}
		return v
	}
	refused := func(code int, reason string, args ...string) {
		t.Helper()
		got, _, stderr := runWith(t, env, args...)
		if got != code || !strings.Contains(stderr, reason) {
			t.Fatalf("rladmin %q: exit %d, want %d with %q\n%s", args, got, code, reason, stderr)
		}
	}
	num := func(v map[string]any, k string) int64 {
		t.Helper()
		f, isNum := v[k].(float64)
		if !isNum {
			t.Fatalf("%s = %v, want a number", k, v[k])
		}
		return int64(f)
	}

	refused(2, "start-epoch --policy FILE", "status")
	start := ok("start-epoch", "--policy", pol)
	id, _ := start["epoch_id"].(string)
	t0 := num(start, "t_us")
	if !isEpochID(id) || t0 <= 0 {
		t.Fatalf("start-epoch printed %v, want a new epoch ID and its start time", start)
	}
	refused(2, "epoch_exists", "start-epoch", "--policy", pol)
	if s := ok("status"); s["epoch_id"] != id || s["fingerprint"] != fpOf(defaultLines) || s["hold"] != "" ||
		num(s, "hwm_us") != t0 || num(s, "tracked_buckets") != 0 || num(s, "live_markers") != 0 || num(s, "quarantined_keys") != 0 {
		t.Errorf("status %v, want epoch %s, the policy's fingerprint, no hold, hwm %d and no keys", s, id, t0)
	}
	if in := ok("inspect", "--key", "alice"); num(in, "present") != 0 || in["u"] != nil || in["ix_score"] != nil || in["quarantine"] != "" {
		t.Errorf("inspect of an absent key: %v", in)
	}
	restoreArgs := []string{"restore", "--policy", pol, "--key", "alice", "--expect-epoch", id, "--balance-tokens", "3"}
	refused(2, "not_quarantined", restoreArgs...)

	fx, err := st.Conn("rl_fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fx.Close() }()
	flag := "index_without_bucket|" + strconv.FormatInt(t0, 10)
	if r, err := fx.Do("HSET", "rl:e:"+id+":q", keyEnc, flag); err != nil || r.Kind != ':' {
		t.Fatalf("quarantining alice: %v %s", err, r)
	}
	rs := ok(restoreArgs...)
	tr := num(rs, "t_us")
	if num(rs, "g") != 1 || tr < t0 {
		t.Fatalf("restore printed %v, want generation 1 at or after %d", rs, t0)
	}
	in := ok("inspect", "--key", "alice")
	if num(in, "present") != 1 || num(in, "u") != 3_000_000_000 || num(in, "t_us") != tr || num(in, "g") != 1 || num(in, "s") != 0 ||
		num(in, "x_us") != tr+60_000_000 || num(in, "mc") != 0 || num(in, "ix_score") != tr || in["quarantine"] != flag {
		t.Errorf("inspect after restore: %v, want u 3e9, t %d, g 1, s 0, x t + 60 s, mc 0, ix at t, the flag kept", in, tr)
	}
	if s := ok("status"); num(s, "tracked_buckets") != 1 || num(s, "quarantined_keys") != 1 {
		t.Errorf("status after restore: %v, want 1 tracked bucket and 1 quarantined key", s)
	}
	clearArgs := []string{"clear", "--policy", pol, "--key", "alice", "--expect-epoch", id}
	tc := num(ok(clearArgs...), "t_us")
	if in := ok("inspect", "--key", "alice"); in["quarantine"] != "" || num(in, "ix_score") != tc || num(in, "u") != 3_000_000_000 {
		t.Errorf("inspect after clear: %v, want no flag, ix at %d, the bucket kept", in, tc)
	}
	refused(2, "not_quarantined", clearArgs...)
	stale := strings.Repeat("ab", 16)
	refused(2, "epoch_changed", "restore", "--policy", pol, "--key", "alice", "--expect-epoch", stale)
	refused(2, "epoch_changed", "start-epoch", "--policy", pol, "--replace", "--expect-epoch", stale)

	id2, _ := ok("start-epoch", "--policy", pol, "--replace", "--expect-epoch", id)["epoch_id"].(string)
	if !isEpochID(id2) || id2 == id {
		t.Fatalf("the replace started %q, want a new epoch ID", id2)
	}
	if s := ok("status"); s["epoch_id"] != id2 || num(s, "tracked_buckets") != 0 || num(s, "quarantined_keys") != 0 {
		t.Errorf("status after the replace: %v, want epoch %s with no keys", s, id2)
	}
	controlled := filepath.Join(dir, "controlled.toml")
	if err := os.WriteFile(controlled, []byte(policyTOML+"\n[policy.x]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused(2, "invalid policy", "start-epoch", "--policy", controlled)
	if err := os.WriteFile(controlled, []byte(strings.Replace(policyTOML, "[policy]\n", "[policy]\nclock_source = \"controlled\"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	refused(4, "clock_error", "start-epoch", "--policy", controlled, "--replace", "--expect-epoch", id2)
	if s := ok("status"); s["epoch_id"] != id2 {
		t.Errorf("a refused replace changed the epoch: %v", s)
	}
}
