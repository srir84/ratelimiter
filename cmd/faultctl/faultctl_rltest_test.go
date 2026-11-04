//go:build rltest

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
	"github.com/srir84/ratelimiter/internal/store/resp"
	"github.com/srir84/ratelimiter/internal/testutil/fakeresp"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// testPassword is the fake store's rl_fixture password in these tests.
const testPassword = "f1xture-test-password"

// fixtureClient is rl_fixture's client command list (DESIGN §A2: +set +get +del +hset +hdel
// +zrem +zadd +flushall +ping +auth +type +eval), as the fake store labels commands.
func fixtureClient() []string {
	return []string{"SET", "GET", "DEL", "HSET", "HDEL", "ZREM", "ZADD", "FLUSHALL", "PING", "AUTH", "TYPE", "EVAL"}
}

// writeRun makes a run directory whose config.toml is the repository's harness.toml with the
// store at addr, and returns its path.
func writeRun(t *testing.T, addr string) string {
	t.Helper()
	dir := newRunDir(t)
	writeConfig(t, dir, addr, nil)
	return dir
}

// newRunDir makes an empty run directory, <temporary root>/runs/<id>, as the orchestrator lays
// them out in the repository root, and returns its path.
func newRunDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runs", "20261009-120000-faultctl-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeConfig writes the repository's harness.toml, with the store at addr and edited by edit
// when not nil, to the run directory's config.toml, and returns its path.
func writeConfig(t *testing.T, dir, addr string, edit func(*config.File)) string {
	t.Helper()
	return writeConfigAs(t, filepath.Join(dir, "config.toml"), addr, edit)
}

// writeConfigAs writes the repository's harness.toml, with the store at addr and edited by
// edit when not nil, to path, and returns path.
func writeConfigAs(t *testing.T, path, addr string, edit func(*config.File)) string {
	t.Helper()
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	f.Store.Addr = addr
	if edit != nil {
		edit(&f)
	}
	out, err := config.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runCLI runs faultctl with args and the environment env, and returns its exit code, stdout
// and stderr. A process it restarts gets PATH alone.
func runCLI(t *testing.T, args []string, env map[string]string) (int, string, string) {
	t.Helper()
	return runCLIWith(t, args, env, []string{"PATH=/usr/bin:/bin"})
}

// runCLIWith is runCLI with environ as the environment a process faultctl restarts starts from
// (run's environ: os.Environ in main).
func runCLIWith(t *testing.T, args []string, env map[string]string, environ []string) (int, string, string) {
	t.Helper()
	return runCLIClock(t, args, env, environ, time.Now)
}

// runCLIClock is runCLIWith with now as faultctl's clock, which times the command (run's now:
// time.Now in main).
func runCLIClock(t *testing.T, args []string, env map[string]string, environ []string, now func() time.Time) (int, string, string) {
	t.Helper()
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), args, lookup, environ, newClient(), now, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// sentSince returns the commands the fake store received after the first n, AUTH left out,
// and fails the test on a command outside rl_fixture's client list.
func sentSince(t *testing.T, fake *fakeresp.Server, n int, what string) [][]string {
	t.Helper()
	var out [][]string
	for _, c := range fake.Commands()[n:] {
		label := fakeresp.Label(c.Args)
		if !slices.Contains(fixtureClient(), label) {
			t.Errorf("%s sent %s, which is not in rl_fixture's client list (DESIGN §A2)", what, label)
		}
		if label != "AUTH" {
			out = append(out, c.Args)
		}
	}
	return out
}

// checkLine decodes faultctl's one stdout line and checks its cmd, its times and that it has
// no outage_ns and no error.
func checkLine(t *testing.T, out, cmd string) {
	t.Helper()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("stdout %q: want one line", out)
	}
	var line map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&line); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	keys := make([]string, 0, len(line))
	for k := range line {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"cmd", "end_ns", "start_ns"}; !slices.Equal(keys, want) {
		t.Errorf("line %s: keys %v, want %v", out, keys, want)
	}
	if line["cmd"] != cmd {
		t.Errorf("line %s: cmd %v, want %q", out, line["cmd"], cmd)
	}
	start, err1 := line["start_ns"].(json.Number).Int64()
	end, err2 := line["end_ns"].(json.Number).Int64()
	if err1 != nil || err2 != nil || start <= 0 || end < start {
		t.Errorf("line %s: start_ns %v, end_ns %v: want 0 < start_ns <= end_ns", out, line["start_ns"], line["end_ns"])
	}
}

// checkErrorLine decodes faultctl's one stdout line of a command that failed and checks its
// cmd, its times and its error, which must be set.
func checkErrorLine(t *testing.T, out, cmd string) {
	t.Helper()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("stdout %q: want one line", out)
	}
	var line map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&line); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	keys := make([]string, 0, len(line))
	for k := range line {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"cmd", "end_ns", "error", "start_ns"}; !slices.Equal(keys, want) {
		t.Errorf("line %s: keys %v, want %v", out, keys, want)
	}
	if line["cmd"] != cmd {
		t.Errorf("line %s: cmd %v, want %q", out, line["cmd"], cmd)
	}
	if msg, _ := line["error"].(string); msg == "" {
		t.Errorf("line %s: error %v, want the failure's message", out, line["error"])
	}
	start, err1 := line["start_ns"].(json.Number).Int64()
	end, err2 := line["end_ns"].(json.Number).Int64()
	if err1 != nil || err2 != nil || start <= 0 || end < start {
		t.Errorf("line %s: start_ns %v, end_ns %v: want 0 < start_ns <= end_ns", out, line["start_ns"], line["end_ns"])
	}
}

// TestClockArgs (IMPLEMENTATION_PLAN H4; DESIGN §B6 "Controlled time"): faultctl clock takes ns
// and refuses, with exit 2 and nothing sent, a value that is negative, not a multiple of 1,000
// or not a whole number; TBOUND µs × 1,000 = 8998403161718784000 ns (TBOUND = 2^53 − 2^43, the
// time bound of DESIGN §A3.1) is accepted and written as rl:ctl:time in µs. advance and back
// step from the value the store holds.
func TestClockArgs(t *testing.T) {
	fake := fakeresp.Start(t, "rl_fixture", testPassword)
	dir := writeRun(t, fake.Addr())
	env := map[string]string{"RL_FIXTURE_PASSWORD": testPassword}
	for _, args := range [][]string{
		{"clock", "set", "-1000"},
		{"clock", "advance", "-1000"},
		{"clock", "back", "-5000"},
		{"clock", "set", "1500"},
		{"clock", "advance", "999"},
		{"clock", "back", "1000001"},
		{"clock", "set", "8998403161718784001"},
		{"clock", "set", "9223372036854775808"}, // above int64
		{"clock", "set", "1e6"},
		{"clock", "set", "4000.0"},
		{"clock", "set"},
		{"clock", "jump", "1000"},
	} {
		code, out, _ := runCLI(t, append([]string{"--run", dir}, args...), env)
		if code != 2 || out != "" {
			t.Errorf("faultctl %s: exit %d, stdout %q; want exit 2 and no line", strings.Join(args, " "), code, out)
		}
	}
	if n := fake.Accepted(); n != 0 {
		t.Fatalf("the refused clock values opened %d store connections, want none", n)
	}

	ok := resp.Value{Kind: resp.Simple, Str: "OK"}
	fake.ReplyValue("SET", ok, 1)
	n := len(fake.Commands())
	code, out, errOut := runCLI(t, []string{"--run", dir, "clock", "set", "8998403161718784000"}, env)
	if code != 0 {
		t.Fatalf("faultctl clock set 8998403161718784000: exit %d, stderr %s; want 0", code, errOut)
	}
	checkLine(t, out, "clock set 8998403161718784000")
	if got, want := sentSince(t, fake, n, "clock set"), [][]string{{"SET", "rl:ctl:time", "8998403161718784"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("clock set sent %q, want %q", got, want)
	}

	for _, c := range []struct {
		args   []string
		stored string
		want   string
	}{
		{[]string{"clock", "advance", "1000000"}, "8998403161717784", "8998403161718784"},
		{[]string{"clock", "back", "1000000"}, "1000", "0"},
	} {
		fake.ReplyValue("GET", resp.Value{Kind: resp.Bulk, Str: c.stored}, 1)
		fake.ReplyValue("SET", ok, 1)
		n := len(fake.Commands())
		code, out, errOut := runCLI(t, append([]string{"--run", dir}, c.args...), env)
		if code != 0 {
			t.Fatalf("faultctl %s: exit %d, stderr %s; want 0", strings.Join(c.args, " "), code, errOut)
		}
		checkLine(t, out, strings.Join(c.args, " "))
		want := [][]string{{"GET", "rl:ctl:time"}, {"SET", "rl:ctl:time", c.want}}
		if got := sentSince(t, fake, n, c.args[1]); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("faultctl %s from %s µs sent %q, want %q", strings.Join(c.args, " "), c.stored, got, want)
		}
	}
}

// TestTamperOpNames (IMPLEMENTATION_PLAN H4 step 3; DESIGN §B6 FLT-7, FLT-11; fake store):
// every §B6 tamper item maps to one faultctl tamper operation, each a different one, which
// sends exactly its change (corrupt-field, add-bad-ix and retype-shared of ix or q after a
// TYPE read of their target), every command in rl_fixture's client list (DESIGN §A2); an
// operation the store shows did less than it says (nothing deleted, or a target that is not
// the live epoch's, so nothing written) exits 1 with one line whose error is set; an unknown
// operation, a flag the operation does not take and a namespace operation without --epoch on a
// run whose instances give no epoch (no pids.json) exit 2 with nothing sent.
func TestTamperOpNames(t *testing.T) {
	fake := fakeresp.Start(t, "rl_fixture", testPassword)
	dir := writeRun(t, fake.Addr())
	env := map[string]string{"RL_FIXTURE_PASSWORD": testPassword}
	const epoch = "0123456789abcdef0123456789abcdef"
	// Budget key k1's encoding is base64url without padding (DESIGN §A3.2): "azE".
	const (
		k  = "rl:e:" + epoch + ":k:azE"
		mi = "rl:e:" + epoch + ":mi:azE"
		ix = "rl:e:" + epoch + ":ix"
		q  = "rl:e:" + epoch + ":q"
	)
	type reply struct {
		label string
		v     resp.Value
	}
	ok := reply{"", resp.Value{Kind: resp.Simple, Str: "OK"}}
	num := func(label string, n int64) reply { return reply{label, resp.Value{Kind: resp.Int, Int: n}} }
	simple := func(label, s string) reply { return reply{label, resp.Value{Kind: resp.Simple, Str: s}} }
	withLabel := func(r reply, label string) reply { r.label = label; return r }
	cases := []struct {
		item    string // DESIGN §B6's item; "" for a second form of an operation
		args    []string
		replies []reply
		want    [][]string
	}{
		{"delete bucket fields", []string{"del-bucket-fields", "--key", "k1", "--epoch", epoch},
			[]reply{num("HDEL", 6)}, [][]string{{"HDEL", k, "u", "t", "g", "s", "x", "mc"}}},
		{"delete k", []string{"del-k", "--key", "k1", "--epoch", epoch},
			[]reply{num("DEL", 1)}, [][]string{{"DEL", k}}},
		{"delete mi", []string{"del-mi", "--key", "k1", "--epoch", epoch},
			[]reply{num("DEL", 1)}, [][]string{{"DEL", mi}}},
		{"delete one of mi's members", []string{"del-mi-member", "--key", "k1", "--member", "3:7", "--epoch", epoch},
			[]reply{num("ZREM", 1)}, [][]string{{"ZREM", mi, "3:7"}}},
		{"delete the ix member", []string{"del-ix", "--key", "k1", "--epoch", epoch},
			[]reply{num("ZREM", 1)}, [][]string{{"ZREM", ix, "azE"}}},
		{"delete a marker field", []string{"del-marker", "--key", "k1", "--member", "3:7", "--epoch", epoch},
			[]reply{num("HDEL", 1)}, [][]string{{"HDEL", k, "m:3:7"}}},
		{"corrupt a field", []string{"corrupt-field", "--key", "k1", "--field", "u", "--value", "x", "--epoch", epoch},
			[]reply{simple("TYPE", "hash"), num("HSET", 0)}, [][]string{{"TYPE", k}, {"HSET", k, "u", "x"}}},
		{"add a malformed ix member", []string{"add-bad-ix", "--epoch", epoch},
			[]reply{simple("TYPE", "zset"), num("ZADD", 1)}, [][]string{{"TYPE", ix}, {"ZADD", ix, "0", "!bad"}}},
		{"change a shared key's type", []string{"retype-shared", "--target", "q", "--epoch", epoch},
			[]reply{simple("TYPE", "zset"), num("DEL", 0), withLabel(ok, "SET")}, [][]string{{"TYPE", ix}, {"DEL", q}, {"SET", q, "tampered"}}},
		{"delete hwm", []string{"del-hwm"},
			[]reply{num("HDEL", 1)}, [][]string{{"HDEL", "rl:epoch", "hwm"}}},
		{"corrupt the record", []string{"corrupt-record"},
			[]reply{simple("TYPE", "hash"), num("HSET", 0)}, [][]string{{"TYPE", "rl:epoch"}, {"HSET", "rl:epoch", "schema", "corrupt"}}},
		{"wipe", []string{"wipe"},
			[]reply{withLabel(ok, "FLUSHALL")}, [][]string{{"FLUSHALL"}}},

		{"", []string{"del-bucket-fields", "--key", "k1", "--field", "mc", "--epoch", epoch},
			[]reply{num("HDEL", 1)}, [][]string{{"HDEL", k, "mc"}}},
		{"", []string{"add-bad-ix", "--member", "1:2", "--value", "-5", "--epoch", epoch},
			[]reply{simple("TYPE", "zset"), num("ZADD", 1)}, [][]string{{"TYPE", ix}, {"ZADD", ix, "-5", "1:2"}}},
		{"", []string{"retype-shared", "--target", "ix", "--epoch", epoch},
			[]reply{simple("TYPE", "zset"), num("DEL", 1), withLabel(ok, "SET")}, [][]string{{"TYPE", ix}, {"DEL", ix}, {"SET", ix, "tampered"}}},
		{"", []string{"retype-shared", "--target", "epoch"},
			[]reply{num("DEL", 1), withLabel(ok, "SET")}, [][]string{{"DEL", "rl:epoch"}, {"SET", "rl:epoch", "tampered"}}},
		{"", []string{"corrupt-record", "--field", "hwm"},
			[]reply{simple("TYPE", "hash"), num("HSET", 0)}, [][]string{{"TYPE", "rl:epoch"}, {"HSET", "rl:epoch", "hwm", "tampered"}}},
	}
	items := map[string]string{} // operation → the §B6 item it serves
	for _, c := range cases {
		op := c.args[0]
		if c.item != "" {
			if prev, dup := items[op]; dup {
				t.Errorf("tamper %s serves both %q and %q: want one operation per item", op, prev, c.item)
			}
			items[op] = c.item
		}
		for _, r := range c.replies {
			fake.ReplyValue(r.label, r.v, 1)
		}
		n := len(fake.Commands())
		args := append([]string{"--run", dir, "tamper"}, c.args...)
		code, out, errOut := runCLI(t, args, env)
		if code != 0 {
			t.Errorf("faultctl tamper %s: exit %d, stderr %s; want 0", strings.Join(c.args, " "), code, errOut)
			continue
		}
		checkLine(t, out, "tamper "+strings.Join(c.args, " "))
		if got := sentSince(t, fake, n, "tamper "+op); !slices.EqualFunc(got, c.want, slices.Equal) {
			t.Errorf("faultctl tamper %s sent %q, want %q", strings.Join(c.args, " "), got, c.want)
		}
	}
	if len(items) != 12 {
		t.Errorf("%d tamper operations serve DESIGN §B6's 12 items, want 12: %v", len(items), items)
	}

	// An operation the store shows did less than it says fails: exit 1 and one line whose error
	// is set. A deletion that removes nothing; a write whose target is not the live epoch's,
	// here under a wrong --epoch, whose keys the store does not hold (TYPE none), or a k:<enc>
	// that is no hash, which sends the TYPE read and no write.
	const wrong = "fedcba9876543210fedcba9876543210"
	wk, wix := "rl:e:"+wrong+":k:azE", "rl:e:"+wrong+":ix"
	for _, c := range []struct {
		args    []string
		replies []reply
		want    [][]string
	}{
		{[]string{"del-hwm"}, []reply{num("HDEL", 0)}, [][]string{{"HDEL", "rl:epoch", "hwm"}}},
		{[]string{"corrupt-field", "--key", "k1", "--field", "u", "--value", "x", "--epoch", wrong},
			[]reply{simple("TYPE", "none")}, [][]string{{"TYPE", wk}}},
		{[]string{"corrupt-field", "--key", "k1", "--field", "u", "--value", "x", "--epoch", epoch},
			[]reply{simple("TYPE", "zset")}, [][]string{{"TYPE", k}}},
		{[]string{"add-bad-ix", "--epoch", wrong}, []reply{simple("TYPE", "none")}, [][]string{{"TYPE", wix}}},
		{[]string{"retype-shared", "--target", "ix", "--epoch", wrong}, []reply{simple("TYPE", "none")}, [][]string{{"TYPE", wix}}},
		{[]string{"retype-shared", "--target", "q", "--epoch", wrong}, []reply{simple("TYPE", "none")}, [][]string{{"TYPE", wix}}},
	} {
		for _, r := range c.replies {
			fake.ReplyValue(r.label, r.v, 1)
		}
		n := len(fake.Commands())
		code, out, _ := runCLI(t, append([]string{"--run", dir, "tamper"}, c.args...), env)
		if code != 1 {
			t.Errorf("faultctl tamper %s: exit %d, want 1 (the command failed)", strings.Join(c.args, " "), code)
		}
		checkErrorLine(t, out, "tamper "+strings.Join(c.args, " "))
		if got := sentSince(t, fake, n, "tamper "+c.args[0]); !slices.EqualFunc(got, c.want, slices.Equal) {
			t.Errorf("faultctl tamper %s sent %q, want %q", strings.Join(c.args, " "), got, c.want)
		}
	}

	accepted := fake.Accepted()
	for _, args := range [][]string{
		{"explode", "--key", "k1", "--epoch", epoch},
		{"del_k", "--key", "k1", "--epoch", epoch},
		{"del-k", "--key", "k1", "--member", "1:2", "--epoch", epoch},
		{"del-k", "--epoch", epoch},
		{"del-k", "--key", "k1"},
		{"del-k", "--key", "", "--epoch", epoch},
		{"del-k", "--key", "k1", "--epoch", "0123"},
		{"del-k", "--key", "k1", "--epoch", epoch, "extra"},
		{"del-bucket-fields", "--key", "k1", "--field", "m:1:2", "--epoch", epoch},
		{"del-marker", "--key", "k1", "--member", "3", "--epoch", epoch},
		{"del-marker", "--key", "k1", "--member", "-3:7", "--epoch", epoch},
		{"corrupt-field", "--key", "k1", "--field", "u", "--epoch", epoch},
		{"add-bad-ix", "--member", "azE", "--epoch", epoch},
		{"add-bad-ix", "--value", "1.5", "--epoch", epoch},
		{"retype-shared", "--target", "k", "--epoch", epoch},
		{"retype-shared", "--target", "q"},
		{"del-hwm", "--key", "k1"},
		{"corrupt-record", "--value", "x"},
		{"wipe", "--field", "x"},
	} {
		code, out, _ := runCLI(t, append([]string{"--run", dir, "tamper"}, args...), env)
		if code != 2 || out != "" {
			t.Errorf("faultctl tamper %s: exit %d, stdout %q; want exit 2 and no line", strings.Join(args, " "), code, out)
		}
	}
	if code, out, _ := runCLI(t, []string{"--run", dir, "tamper", "wipe"}, map[string]string{}); code != 2 || out != "" {
		t.Errorf("faultctl tamper wipe without RL_FIXTURE_PASSWORD: exit %d, stdout %q; want exit 2 and no line", code, out)
	}
	if n := fake.Accepted(); n != accepted {
		t.Errorf("the refused tamper commands opened %d store connections, want none", n-accepted)
	}
}
