//go:build rltest

package main

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/config"
)

// testBins are Go binaries built from testdata/buildinfo: clean without tags, rltest with
// -tags rltest (buildBins). Empty paths stand for shell scripts, enough for test runs, whose
// binaries preflight only checks for existence.
type testBins struct {
	clean, rltest string
}

// buildBins builds testdata/buildinfo twice into a temporary directory.
func buildBins(t *testing.T) testBins {
	t.Helper()
	dir := t.TempDir()
	b := testBins{clean: filepath.Join(dir, "clean"), rltest: filepath.Join(dir, "rltest")}
	for _, args := range [][]string{
		{"build", "-o", b.clean, "./testdata/buildinfo"},
		{"build", "-tags", rltestTag, "-o", b.rltest, "./testdata/buildinfo"},
	} {
		if out, err := exec.CommandContext(t.Context(), "go", args...).CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return b
}

// writeFile writes content to path with mode, creating the directories above it.
func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile keeps an existing file's mode
		t.Fatal(err)
	}
}

// copyFile copies src to dst with mode.
func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, string(data), mode)
}

// fakeBinary is an executable stand-in for a binary preflight only checks for existence.
const fakeBinary = "#!/bin/sh\nexit 0\n"

// newTestSite builds a repository tree in a temporary directory that a pf-1 scenario passes
// preflight in, run kind test or bench: the repository's harness.toml and
// deploy/acl.conf.tmpl; bin/test with hitapi, rladmin, hitgen and sampler; bin/bench with
// hitapi and rladmin (bins.clean copies when given); .data/conformance.json (conformanceJSON);
// runs/thresholds/pf-1.json; and four consecutive free loopback ports, the store's and
// three instances'.
func newTestSite(t *testing.T, bins testBins) site {
	t.Helper()
	root := t.TempDir()
	base := freePortBlock(t, 4)
	s := site{root: root, storePort: base, apiBase: base + 1}
	copyFile(t, filepath.Join("..", "..", "harness.toml"), s.path(harnessConfigFile), 0o600)
	copyFile(t, filepath.Join("..", "..", "deploy", "acl.conf.tmpl"), s.path(aclTemplate), 0o600)
	for _, name := range []string{"hitapi", "rladmin", "hitgen", "sampler"} {
		writeFile(t, s.path("bin/test/"+name), fakeBinary, 0o755)
	}
	for _, name := range []string{"hitapi", "rladmin"} {
		if bins.clean == "" {
			writeFile(t, s.path("bin/bench/"+name), fakeBinary, 0o755)
		} else {
			copyFile(t, bins.clean, s.path("bin/bench/"+name), 0o755)
		}
	}
	writeFile(t, s.path(conformanceFile), conformanceJSON, 0o600)
	writeFile(t, s.path(thresholdsDir+"/pf-1.json"), thresholdsJSON, 0o600)
	return s
}

// conformanceJSON is a .data/conformance.json as S1's suite writes it (IMPLEMENTATION_PLAN S1
// step 6's keys), with both fsync measurements a benchmark run's context.json must carry
// (DESIGN §B8: the F_FULLFSYNC floor and the durable single-client script p50, in µs);
// conformanceHead and conformanceTail surround its fsync value.
const (
	conformanceJSON = conformanceHead +
		`{"f_fullfsync_floor_p50_us":5667.9,"plain_fsync_p50_us":21.4,"durable_script_p50_us":6460.5}` + conformanceTail
	conformanceHead = `{"generated_at":"2026-10-05T09:54:12Z","store_bin":"/usr/local/bin/valkey-server",` +
		`"product":"Valkey","store_version":"9.1.0","os":"darwin/arm64","go":"go1.27.1","eligible":true,"passed":true,"fsync":`
	conformanceTail = `,"row9":[{"clients":50,"appendfsync":"always","ops_per_s":7412.6,"p50_us":6520.3,"p99_us":9104.8,"ops":222378}],` +
		`"row13":[{"conns":4,"depth":64,"size":"max","cmd_bytes":412,"ops_per_s":38250.1,"p50_us":6612.4,"p99_us":8890.2,"ops":1147503}],` +
		`"recommended_store_conns":4,"row14":{"idle_p50_us":38.5,"loaded_p50_us":41,"writers":4},"scan_calls_200k":2,` +
		`"crash":[{"acked":5000,"lost":0,"in_flight":1,"kept_unacked":1}],"reload_ms_a7":512.5}` + "\n"
)

// conformanceWith is conformanceJSON with fsync as its fsync value.
func conformanceWith(fsync string) string { return conformanceHead + fsync + conformanceTail }

// thresholdsJSON is a frozen thresholds file's content; its SHA-256 is thresholdsSHA256.
const (
	thresholdsJSON   = `{"steps":[{"step":0,"p99_ms":12}]}` + "\n"
	thresholdsSHA256 = "7f872c41bc60ca88ba0d757202ffa03882fb56af77bcf4bb910ea7b4ac1c14df"
)

// The loopback ports freePortBlock picks from: below the kernels' ephemeral ranges (macOS
// 49152–65535, Linux 32768–60999), so a listener another process opens on port 0 never lands in
// a block once it is picked.
const (
	portRangeLow  = 20_000
	portRangeHigh = 32_000
)

// freePortBlock returns the first of n consecutive loopback ports that preflight's own check
// finds free now (portHeld: no connection to the port succeeds, so nothing listens on it or on
// every address, and it binds). It walks the n-port blocks of [portRangeLow, portRangeHigh)
// once, from a block derived from the test's name and this process's PID, so the tests and
// concurrent test processes start apart. clock_stub_test.go holds the untagged build's copy.
func freePortBlock(t *testing.T, n int) int {
	t.Helper()
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name() + " " + strconv.Itoa(os.Getpid()))) // a hash write never fails
	blocks := (portRangeHigh - portRangeLow) / n
	start := int(h.Sum32()) % blocks
	for i := range blocks {
		base := portRangeLow + (start+i)%blocks*n
		free := true
		for p := base; p < base+n && free; p++ {
			held, _ := portHeld(t.Context(), p)
			free = !held
		}
		if free {
			return base
		}
	}
	t.Fatalf("no %d consecutive free loopback ports in [%d, %d)", n, portRangeLow, portRangeHigh)
	return 0
}

// listen holds host:port until the test ends; host "" listens on every address.
func listen(t *testing.T, host string, port int) {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

// scenario is a scenario file's text: name pf, seed 1, run_kind kind, the top-level lines
// top, then tables, or one 30/s × 10 s step and 50 users when tables is "".
func scenario(kind, top, tables string) string {
	if tables == "" {
		tables = oneStep
	}
	return "name = \"pf\"\nseed = 1\nrun_kind = \"" + kind + "\"\n" + top + "\n" + tables
}

const oneStep = "[[steps]]\nrate_per_s = 30\nduration_s = 10\n\n[users]\ncount = 50\n"

// refusalReasons returns the reasons of the "refusing the run" lines in the orchestrator's
// stderr, sorted, failing the test on a line that is not JSON or a refusal without a detail.
func refusalReasons(t *testing.T, stderr string) []string {
	t.Helper()
	var reasons []string
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if line == "" {
			continue
		}
		var rec struct{ Msg, Reason, Detail string }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not JSON: %v", line, err)
		}
		if rec.Msg != "refusing the run" {
			continue
		}
		if rec.Reason == "" || rec.Detail == "" {
			t.Errorf("refusal line without a reason or a detail: %s", line)
		}
		reasons = append(reasons, rec.Reason)
	}
	slices.Sort(reasons)
	return reasons
}

// testNow is the wall clock the tests give the orchestrator.
func testNow() time.Time { return time.Date(2026, 10, 8, 14, 22, 33, 123456789, time.UTC) }

// /_test/faults bodies (DESIGN §B6). hitapi decodes a body into typed fields (harness/httpapi
// faultRequest): a later null leaves an earlier duration_ms in place, a key matches its field
// in any case, and an absent or 0 duration_ms means no limit.
const (
	// refuseBody arms a timed store refusal: one outage segment. So do refuseNullRepeat and
	// refuseNullCase, each a 4000 ms refusal to hitapi.
	refuseBody       = `{"target":"store","mode":"refuse","match":"acquire","duration_ms":4000}`
	refuseNullRepeat = `{"target":"store","mode":"refuse","match":"acquire","duration_ms":4000,"duration_ms":null}`
	refuseNullCase   = `{"target":"store","mode":"refuse","match":"acquire","duration_ms":4000,"Duration_MS":null}`
	// refuseNoLimit refuses until disarmed: no timed segment.
	refuseNoLimit = `{"target":"store","mode":"refuse","match":"acquire","duration_ms":0}`
	// delayBody and dropReplyBody are timed store faults that are not refusals.
	delayBody     = `{"target":"store","mode":"delay","match":"acquire","delay_ms":100,"duration_ms":4000}`
	dropReplyBody = `{"target":"store","mode":"drop_reply","match":"refund","duration_ms":4000}`
)

// TestPreflight_Refusals (plan H3 step 1): each rule refuses the run with exit 2 and logs its
// reason, before anything starts: no run directory and no pid file. Each case starts from a
// tree its scenario passes in (the "passes" cases, a valid test scenario among them, check
// that) and breaks one rule. A case with a secret, a password in a db_dsn, also checks that
// no output line holds it.
func TestPreflight_Refusals(t *testing.T) {
	bins := buildBins(t)
	instanceConfig := filepath.Join("..", "..", "harness.toml")  // a config file that loads
	unparsable := filepath.Join(t.TempDir(), "unknown-key.toml") // a config file that exists and does not load
	writeFile(t, unparsable, "surplus = 1\n", 0o600)
	// secret stands for a database password in a db_dsn: no refusal may quote it.
	const secret = "SECRETMARK"
	harnessText, err := os.ReadFile(instanceConfig)
	if err != nil {
		t.Fatal(err)
	}
	// withDSN is harness.toml with [api] db_dsn = dsn.
	withDSN := func(dsn string) string {
		const line = `db_dsn = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64"`
		text := strings.Replace(string(harnessText), line, "db_dsn = '"+dsn+"'", 1)
		if text == string(harnessText) {
			t.Fatalf("harness.toml has no line %s", line)
		}
		return text
	}
	passwordConfig := filepath.Join(t.TempDir(), "password-dsn.toml") // harness.toml, its db_dsn with a ?password=
	writeFile(t, passwordConfig, withDSN("postgres://rl@127.0.0.1:5433/hits?password="+secret+"&pool_max_conns=64"), 0o600)
	otherDBConfig := filepath.Join(t.TempDir(), "other-db.toml") // harness.toml, its db_dsn naming another database
	writeFile(t, otherDBConfig, withDSN("postgres://rl@127.0.0.1:5433/other?statement_timeout=2500&pool_max_conns=64"), 0o600)
	cases := []struct {
		name     string
		scenario string
		args     []string
		edit     func(t *testing.T, s site)
		want     []string
		secret   string // a text no line of the orchestrator's output may hold
	}{
		{name: "bench with [[faults]]", want: []string{reasonBenchFaults},
			scenario: scenario("bench", "", oneStep+"[[faults]]\nat_s = 1\ncmd = '{\"target\":\"db\",\"mode\":\"fail_before\"}'\n")},
		{name: "bench overriding clock_source, even to store", want: []string{reasonBenchClockOverride},
			scenario: scenario("bench", "", "[policy]\nclock_source = \"store\"\n"+oneStep)},
		{name: "bench with a controlled clock override", want: []string{reasonBenchClockOverride, reasonBenchClock},
			scenario: scenario("bench", "", "[policy]\nclock_source = \"controlled\"\n"+oneStep)},
		{name: "bench with harness.toml set to the controlled clock", want: []string{reasonBenchClock},
			scenario: scenario("bench", "", ""),
			edit: func(t *testing.T, s site) {
				data, err := os.ReadFile(s.path(harnessConfigFile))
				if err != nil {
					t.Fatal(err)
				}
				edited := strings.Replace(string(data), `clock_source = "store"`, `clock_source = "controlled"`, 1)
				if edited == string(data) {
					t.Fatal(`harness.toml has no clock_source = "store" line to edit`)
				}
				writeFile(t, s.path(harnessConfigFile), edited, 0o600)
			}},
		{name: "refuse_after_acquire_bp 501", want: []string{reasonRefuseBP},
			scenario: scenario("test", "", "[api]\nrefuse_after_acquire_bp = 501\n"+oneStep)},
		{name: "100 bp x 6000/s = 60/s above 50/s x 1 instance", want: []string{reasonRefusalRate},
			scenario: scenario("test", "instances = 1", "[api]\nrefuse_after_acquire_bp = 100\n"+
				"[[steps]]\nrate_per_s = 100\nduration_s = 1\n[[steps]]\nrate_per_s = 6000\nduration_s = 1\n[users]\ncount = 50\n")},
		{name: "100 bp x 5001/s = 50.01/s above 50/s x 1 instance", want: []string{reasonRefusalRate},
			scenario: scenario("test", "instances = 1", "[api]\nrefuse_after_acquire_bp = 100\n"+
				"[[steps]]\nrate_per_s = 5001\nduration_s = 1\n[users]\ncount = 50\n")},
		{name: "100 bp x the highest of 100, 6000 and 100/s = 60/s, 1 instance", want: []string{reasonRefusalRate},
			scenario: scenario("test", "instances = 1", "[api]\nrefuse_after_acquire_bp = 100\n"+
				"[[steps]]\nrate_per_s = 100\nduration_s = 1\n[[steps]]\nrate_per_s = 6000\nduration_s = 1\n"+
				"[[steps]]\nrate_per_s = 100\nduration_s = 1\n[users]\ncount = 50\n")},
		// The rate is exactly 3123612578909091 / 2^36, so 11 / 10,000 of it is 50 + 1/687194767360000
		// refusals/s, above 50; in float64, 11 × the rate rounds to exactly 500,000.
		{name: "11 bp x 45454.545454545456/s just above 50/s x 1 instance", want: []string{reasonRefusalRate},
			scenario: scenario("test", "instances = 1", "[api]\nrefuse_after_acquire_bp = 11\n"+
				"[[steps]]\nrate_per_s = 45454.545454545456\nduration_s = 1\n[users]\ncount = 50\n")},
		{name: "spike recovery step 89 s", want: []string{reasonSpikeStep},
			scenario: scenario("test", "verdict = \"spike\"\nspike_step = 0",
				"[[steps]]\nrate_per_s = 100\nduration_s = 30\n[[steps]]\nrate_per_s = 10\nduration_s = 89\n[users]\ncount = 50\n")},
		{name: "bench hitapi built with -tags rltest", want: []string{reasonBenchRltest},
			scenario: scenario("bench", "", ""),
			edit: func(t *testing.T, s site) {
				copyFile(t, bins.rltest, s.path("bin/bench/hitapi"), 0o755)
			}},
		{name: "--bin-dir whose hitapi is an rltest build", want: []string{reasonBenchRltest},
			scenario: scenario("bench", "", ""),
			args:     []string{"--bin-dir", "BINDIR"},
			edit: func(t *testing.T, s site) {
				copyFile(t, bins.rltest, s.path("bindir/hitapi"), 0o755)
				copyFile(t, bins.clean, s.path("bindir/rladmin"), 0o755)
			}},
		{name: "bench rladmin without Go build info", want: []string{reasonBenchUnverified},
			scenario: scenario("bench", "", ""),
			edit: func(t *testing.T, s site) {
				writeFile(t, s.path("bin/bench/rladmin"), fakeBinary, 0o755)
			}},
		{name: "missing bin/test/hitapi", want: []string{reasonBinaryMissing},
			scenario: scenario("test", "", ""),
			edit:     func(t *testing.T, s site) { remove(t, s.path("bin/test/hitapi")) }},
		{name: "missing bin/bench/rladmin", want: []string{reasonBinaryMissing},
			scenario: scenario("bench", "", ""),
			edit:     func(t *testing.T, s site) { remove(t, s.path("bin/bench/rladmin")) }},
		{name: "missing bin/test/hitgen", want: []string{reasonBinaryMissing},
			scenario: scenario("bench", "", ""), // harness tools run from bin/test in both run kinds
			edit:     func(t *testing.T, s site) { remove(t, s.path("bin/test/hitgen")) }},
		{name: "bin/test/sampler not executable", want: []string{reasonBinaryMissing},
			scenario: scenario("test", "", ""),
			edit:     func(t *testing.T, s site) { writeFile(t, s.path("bin/test/sampler"), fakeBinary, 0o644) }},
		{name: "store port bound", want: []string{reasonPortBound},
			scenario: scenario("test", "", ""),
			edit:     func(t *testing.T, s site) { listen(t, "127.0.0.1", s.storePort) }},
		{name: "store port held by a listener on every address", want: []string{reasonPortBound},
			scenario: scenario("test", "", ""), // on macOS, 127.0.0.1:P still binds: only the dial sees it
			edit:     func(t *testing.T, s site) { listen(t, "", s.storePort) }},
		{name: "instance 3's port bound", want: []string{reasonPortBound},
			scenario: scenario("test", "", ""),
			edit:     func(t *testing.T, s site) { listen(t, "127.0.0.1", s.apiBase+2) }},
		{name: "pid file naming a live process", want: []string{reasonRunning},
			scenario: scenario("test", "", ""),
			edit: func(t *testing.T, s site) {
				writeFile(t, s.path(pidFileName), strconv.Itoa(os.Getpid())+"\n", 0o600)
			}},
		{name: "bench limiter run without frozen thresholds", want: []string{reasonThresholds},
			scenario: scenario("bench", "", ""),
			edit:     func(t *testing.T, s site) { remove(t, s.path(thresholdsDir+"/pf-1.json")) }},
		{name: "bench run without .data/conformance.json", want: []string{reasonConformance},
			scenario: scenario("bench", "", ""),
			edit:     func(t *testing.T, s site) { remove(t, s.path(conformanceFile)) }},
		{name: "bench --baseline run without .data/conformance.json", want: []string{reasonConformance},
			scenario: scenario("bench", "", ""),
			args:     []string{"--baseline"},
			edit:     func(t *testing.T, s site) { remove(t, s.path(conformanceFile)) }},
		{name: "two store refuse bodies with duration_ms", want: []string{reasonOutageSegments},
			scenario: scenario("test", "", oneStep+
				"[[faults]]\nat_s = 1\ncmd = '"+refuseBody+"'\n[[faults]]\nat_s = 5\ninstance = 2\ncmd = '"+refuseBody+"'\n")},
		{name: "two refuse bodies whose duration_ms repeats as null, then in another case", want: []string{reasonOutageSegments},
			scenario: scenario("test", "", oneStep+
				"[[faults]]\nat_s = 1\ncmd = '"+refuseNullRepeat+"'\n[[faults]]\nat_s = 5\ncmd = '"+refuseNullCase+"'\n")},
		{name: "--instance-config on a bench run", want: []string{reasonInstanceConfBench},
			scenario: scenario("bench", "", ""),
			args:     []string{"--instance-config", "3=" + instanceConfig}},
		{name: "--instance-config for an instance the scenario lacks", want: []string{reasonInstanceConf},
			scenario: scenario("test", "instances = 1", ""),
			args:     []string{"--instance-config", "3=" + instanceConfig}},
		{name: "--instance-config file missing", want: []string{reasonInstanceConf},
			scenario: scenario("test", "", ""),
			args:     []string{"--instance-config", "2=" + filepath.Join(t.TempDir(), "absent.toml")}},
		{name: "--instance-config file that does not parse", want: []string{reasonInstanceConf},
			scenario: scenario("test", "", ""),
			args:     []string{"--instance-config", "2=" + unparsable}},
		{name: "scenario that does not load", want: []string{reasonScenario},
			scenario: "name = \"pf\"\n"},
		{name: "harness.toml with an unknown key", want: []string{reasonConfig},
			scenario: scenario("test", "", ""),
			edit: func(t *testing.T, s site) {
				data, err := os.ReadFile(s.path(harnessConfigFile))
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, s.path(harnessConfigFile), string(data)+"surplus = 1\n", 0o600)
			}},
		// A db_dsn that carries a password is refused before anything is written, whichever file
		// it comes from (AGENTS.md §1 Secrets): config.toml, context.json and the instance-N.toml
		// copy would hold it.
		{name: "harness.toml's db_dsn with a password", want: []string{reasonDSNPassword}, secret: secret,
			scenario: scenario("test", "", ""),
			edit: func(t *testing.T, s site) {
				writeFile(t, s.path(harnessConfigFile), withDSN("postgres://rl:"+secret+"@127.0.0.1:5433/hits?pool_max_conns=64"), 0o600)
			}},
		{name: "harness.toml's db_dsn, a password in a URL that does not parse", want: []string{reasonConfig}, secret: secret,
			scenario: scenario("test", "", ""),
			edit: func(t *testing.T, s site) {
				writeFile(t, s.path(harnessConfigFile), withDSN("postgres://rl:"+secret+"@127.0.0.1:bad/hits"), 0o600)
			}},
		{name: "--instance-config whose db_dsn has a password", want: []string{reasonDSNPassword, reasonInstanceConfDSN}, secret: secret,
			scenario: scenario("test", "", ""),
			args:     []string{"--instance-config", "2=" + passwordConfig}},
		{name: "--instance-config whose db_dsn names another database", want: []string{reasonInstanceConfDSN},
			scenario: scenario("test", "", ""),
			args:     []string{"--instance-config", "3=" + otherDBConfig}},
		// Overrides DESIGN §B4 does not list, refused in every run kind: --baseline is the limiter
		// switch (HAR-17), and the run's database is harness.toml's.
		{name: "test scenario overriding limiter_enabled", want: []string{reasonLimiterOverride},
			scenario: scenario("test", "", "[api]\nlimiter_enabled = false\n"+oneStep)},
		{name: "test scenario overriding limiter_enabled, even to true", want: []string{reasonLimiterOverride},
			scenario: scenario("test", "", "[api]\nlimiter_enabled = true\n"+oneStep)},
		{name: "bench scenario overriding limiter_enabled", want: []string{reasonLimiterOverride},
			scenario: scenario("bench", "", "[api]\nlimiter_enabled = false\n"+oneStep)},
		{name: "test scenario overriding db_dsn", want: []string{reasonDSNOverride},
			scenario: scenario("test", "", "[api]\ndb_dsn = 'postgres://rl@127.0.0.1:5433/other'\n"+oneStep)},
		{name: "bench scenario overriding db_dsn", want: []string{reasonDSNOverride},
			scenario: scenario("bench", "", "[api]\ndb_dsn = 'postgres://rl@127.0.0.1:5433/other'\n"+oneStep)},
		{name: "scenario overriding db_dsn with a password", want: []string{reasonDSNOverride, reasonDSNPassword}, secret: secret,
			scenario: scenario("test", "", "[api]\ndb_dsn = 'postgres://rl:"+secret+"@127.0.0.1:5433/hits'\n"+oneStep)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestSite(t, bins)
			if c.edit != nil {
				c.edit(t, s)
			}
			path := filepath.Join(t.TempDir(), "scenario.toml")
			writeFile(t, path, c.scenario, 0o600)
			args := []string{"--scenario", path}
			for _, a := range c.args {
				args = append(args, strings.ReplaceAll(a, "BINDIR", s.path("bindir")))
			}
			pidBefore, pidErr := os.ReadFile(s.path(pidFileName))
			var stderr bytes.Buffer
			code := orchestrate(t.Context(), args, s, nil, os.Getpid(), testNow, &stderr)
			if code != exitRefused {
				t.Fatalf("exit code %d, want %d\nstderr:\n%s", code, exitRefused, stderr.String())
			}
			if c.secret != "" && strings.Contains(stderr.String(), c.secret) {
				t.Fatalf("the orchestrator's output holds the DSN's password %s", c.secret)
			}
			want := slices.Sorted(slices.Values(c.want))
			if got := refusalReasons(t, stderr.String()); !slices.Equal(got, want) {
				t.Errorf("refusal reasons %q, want %q\nstderr:\n%s", got, want, stderr.String())
			}
			entries, err := os.ReadDir(s.path(runsDir))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "thresholds" {
					t.Errorf("a refused run created %s/%s", runsDir, e.Name())
				}
			}
			pidAfter, err := os.ReadFile(s.path(pidFileName))
			if pidErr != nil {
				if err == nil {
					t.Errorf("a refused run wrote %s", pidFileName)
				}
			} else if !bytes.Equal(pidAfter, pidBefore) {
				t.Errorf("a refused run changed %s from %q to %q (err %v)", pidFileName, pidBefore, pidAfter, err)
			}
		})
	}

	passes := []struct {
		name     string
		scenario string
		fl       flags
		edit     func(t *testing.T, s site)
	}{
		{name: "valid test scenario with the controlled clock",
			scenario: scenario("test", "", "[policy]\nclock_source = \"controlled\"\n"+oneStep)},
		{name: "valid bench scenario", scenario: scenario("bench", "", "")},
		{name: "bench baseline without frozen thresholds", scenario: scenario("bench", "", ""), fl: flags{baseline: true},
			edit: func(t *testing.T, s site) { remove(t, s.path(thresholdsDir+"/pf-1.json")) }},
		{name: "refuse_after_acquire_bp 500", scenario: scenario("test", "", "[api]\nrefuse_after_acquire_bp = 500\n"+oneStep)},
		{name: "100 bp x 5000/s = 50/s, 1 instance", scenario: scenario("test", "instances = 1", "[api]\nrefuse_after_acquire_bp = 100\n"+
			"[[steps]]\nrate_per_s = 5000\nduration_s = 1\n[users]\ncount = 50\n")},
		{name: "100 bp x 15000/s = 150/s, 3 instances", scenario: scenario("test", "instances = 3", "[api]\nrefuse_after_acquire_bp = 100\n"+
			"[[steps]]\nrate_per_s = 15000\nduration_s = 1\n[users]\ncount = 50\n")},
		{name: "spike recovery step 90 s", scenario: scenario("test", "verdict = \"spike\"\nspike_step = 0",
			"[[steps]]\nrate_per_s = 100\nduration_s = 30\n[[steps]]\nrate_per_s = 10\nduration_s = 90\n[users]\ncount = 50\n")},
		{name: "one refuse body with duration_ms, one without", scenario: scenario("test", "", oneStep+
			"[[faults]]\nat_s = 1\ncmd = '"+refuseBody+"'\n"+
			"[[faults]]\nat_s = 5\ncmd = '{\"target\":\"store\",\"mode\":\"refuse\",\"match\":\"acquire\",\"count\":3}'\n")},
		{name: "two refuse bodies with duration_ms 0, no limit to hitapi", scenario: scenario("test", "", oneStep+
			"[[faults]]\nat_s = 1\ncmd = '"+refuseNoLimit+"'\n[[faults]]\nat_s = 5\ncmd = '"+refuseNoLimit+"'\n")},
		{name: "a refuse body and timed store delay and drop_reply bodies", scenario: scenario("test", "", oneStep+
			"[[faults]]\nat_s = 1\ncmd = '"+refuseBody+"'\n[[faults]]\nat_s = 2\ncmd = '"+delayBody+"'\n"+
			"[[faults]]\nat_s = 3\ncmd = '"+dropReplyBody+"'\n")},
		{name: "stale pid file", scenario: scenario("test", "", ""),
			edit: func(t *testing.T, s site) {
				writeFile(t, s.path(pidFileName), strconv.Itoa(deadPID(t))+"\n", 0o600)
			}},
		{name: "--instance-config for instance 3", scenario: scenario("test", "", ""),
			fl: flags{instanceConfigs: map[int]string{3: instanceConfig}}},
	}
	for _, c := range passes {
		t.Run("passes "+c.name, func(t *testing.T) {
			s := newTestSite(t, bins)
			if c.edit != nil {
				c.edit(t, s)
			}
			c.fl.scenario = filepath.Join(t.TempDir(), "scenario.toml")
			writeFile(t, c.fl.scenario, c.scenario, 0o600)
			in, refs := preflight(t.Context(), s, c.fl)
			if len(refs) > 0 {
				t.Fatalf("refusals %+v, want none", refs)
			}
			kind := in.scenario.RunKind
			if in.binDir != s.path("bin/"+kind) || in.testBinDir != s.path("bin/test") {
				t.Errorf("binDir %s, testBinDir %s; want bin/%s and bin/test under %s", in.binDir, in.testBinDir, kind, s.root)
			}
			var wantPorts []int
			for n := range in.scenario.Instances {
				wantPorts = append(wantPorts, s.apiBase+n)
			}
			if in.storePort != s.storePort || !slices.Equal(in.apiPorts, wantPorts) {
				t.Errorf("ports %d and %v, want %d and %v", in.storePort, in.apiPorts, s.storePort, wantPorts)
			}
			wantThresholds := s.path(thresholdsDir + "/pf-1.json")
			if c.fl.baseline {
				wantThresholds = ""
			}
			if in.thresholds != wantThresholds {
				t.Errorf("thresholds %q, want %q", in.thresholds, wantThresholds)
			}
			if len(in.config) == 0 || in.settings.API.LimiterEnabled == c.fl.baseline {
				t.Errorf("config of %d bytes with limiter_enabled = %v; want the merged config, the limiter off only with --baseline",
					len(in.config), in.settings.API.LimiterEnabled)
			}
		})
	}
}

// TestPreflight_OutageSegments (plan H3 step 1): the store outage segments the [[faults]] arm,
// one per entry: each faultctl store down or store pause entry, and each /_test/faults body
// that, read in hitapi's field types, arms a timed store refusal (target store, mode refuse,
// duration_ms above 0; unknown keys ignored, so the count fails closed); more than one is
// refused as store_outage_segments. Checked here directly, so the cases with faultctl entries
// do not depend on the interim faultctl refusal.
func TestPreflight_OutageSegments(t *testing.T) {
	const (
		down  = "faultctl store down 4s"
		pause = "faultctl store pause 4.5s"
	)
	for _, c := range []struct {
		name string
		cmds []string
		want int
	}{
		{"store down and a refuse body", []string{down, refuseBody}, 2},
		{"two store pauses", []string{pause, pause}, 2},
		{"a store pause and a refuse body", []string{pause, refuseBody}, 2},
		{"a faultctl cmd that is no store outage", []string{"faultctl api kill 2 --restart"}, 0},
		{"faultctl store without a command", []string{"faultctl store"}, 0},
		{"duration_ms repeated as null", []string{refuseNullRepeat}, 1},
		{"duration_ms repeated as null in another case", []string{refuseNullCase}, 1},
		{"duration_ms in another case", []string{`{"target":"store","mode":"refuse","match":"acquire","DURATION_MS":4000}`}, 1},
		{"duration_ms 0", []string{refuseNoLimit}, 0},
		{"duration_ms null", []string{`{"target":"store","mode":"refuse","match":"acquire","duration_ms":null}`}, 0},
		{"a count and no duration_ms", []string{`{"target":"store","mode":"refuse","match":"acquire","count":3}`}, 0},
		{"timed store delay and drop_reply", []string{delayBody, dropReplyBody}, 0},
		{"target db with mode refuse", []string{`{"target":"db","mode":"refuse","duration_ms":4000}`}, 0},
		// Unknown keys are ignored, so the count fails closed: a timed store refusal that
		// carries a key hitapi gains later (H4 step 4's "at") still counts, and one with a key
		// hitapi does not know (it answers 400) can only add to the count.
		{"a timed store refuse body with at", []string{`{"target":"store","mode":"refuse","match":"acquire","duration_ms":4000,"at":"before_send"}`}, 1},
		{"a refuse body and one with at", []string{refuseBody, `{"target":"store","mode":"refuse","match":"acquire","duration_ms":4000,"at":"before_send"}`}, 2},
		{"an unknown key", []string{`{"target":"store","mode":"refuse","match":"acquire","duration_ms":4000,"instance":2}`}, 1},
		// Bodies hitapi answers with 400, arming nothing.
		{"a negative duration_ms", []string{`{"target":"store","mode":"refuse","match":"acquire","duration_ms":-5}`}, 0},
		{"duration_ms as a string", []string{`{"target":"store","mode":"refuse","match":"acquire","duration_ms":"4000"}`}, 0},
		{"a second JSON value", []string{refuseBody + " {}"}, 0},
	} {
		faults := make([]config.Fault, 0, len(c.cmds))
		for i, cmd := range c.cmds {
			faults = append(faults, config.Fault{AtS: float64(i), Cmd: cmd, Targets: []int{1}})
		}
		if got := outageSegments(faults); got != c.want {
			t.Errorf("%s: outageSegments = %d, want %d", c.name, got, c.want)
		}
		var refs refusals
		scenarioRules(&refs, config.Scenario{Faults: faults})
		refused := slices.ContainsFunc(refs, func(r refusal) bool { return r.reason == reasonOutageSegments })
		if refused != (c.want > 1) {
			t.Errorf("%s: refused as %s = %v, want %v (refusals %+v)", c.name, reasonOutageSegments, refused, c.want > 1, refs)
		}
	}
}

// TestPreflight_RefusalRateNotFinite: a step rate that is not finite, which the scenario
// loader refuses before preflight, is refused as an invalid scenario, never compared.
func TestPreflight_RefusalRateNotFinite(t *testing.T) {
	for _, rate := range []float64{math.Inf(1), math.NaN()} {
		var refs refusals
		refusalRules(&refs, config.Scenario{Instances: 1, Steps: []config.Step{{RatePerS: rate, DurationS: 1}}}, 100)
		if len(refs) != 1 || refs[0].reason != reasonScenario {
			t.Errorf("rate %v: refusals %+v, want one %s", rate, refs, reasonScenario)
		}
	}
}

// remove deletes path, which must exist.
func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// deadPID returns the PID of a child that has exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// TestPreflight_BuildInfoRltest (plan H3 step 1): build info lists the rltest tag only when a
// build line's -tags setting holds rltest among its tags. The texts are `go version -m`
// output (as for bin/test/hitapi and bin/bench/hitapi at H2) and debug/buildinfo's form of it.
func TestPreflight_BuildInfoRltest(t *testing.T) {
	const head = "bin/test/hitapi: go1.27.1\n" +
		"\tpath\tratelimiter/cmd/hitapi\n" +
		"\tmod\tratelimiter\tv0.0.0-20261008160032-3fa64d09a5b5+dirty\t\n" +
		"\tdep\tgithub.com/BurntSushi/toml\tv1.6.0\th1:dRaEfpa2VI55EwlIW72hMRHdWouJeRF7TPYhI+AUQjk=\n" +
		"\tbuild\t-buildmode=exe\n" +
		"\tbuild\t-compiler=gc\n"
	const tail = "\tbuild\tDefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0\n" +
		"\tbuild\tCGO_ENABLED=1\n" +
		"\tbuild\tGOARCH=arm64\n" +
		"\tbuild\tGOOS=darwin\n" +
		"\tbuild\tvcs=git\n" +
		"\tbuild\tvcs.modified=true\n"
	for _, c := range []struct {
		name, text string
		want       bool
	}{
		{"go version -m of an rltest build", head + "\tbuild\t-tags=rltest\n" + tail, true},
		{"go version -m of an untagged build", head + tail, false},
		{"rltest among several tags", head + "\tbuild\t-tags=conformance,rltest\n" + tail, true},
		{"quoted tags", head + "\tbuild\t-tags=\"rltest,scripttest\"\n" + tail, true},
		{"another tag only", head + "\tbuild\t-tags=scripttest\n" + tail, false},
		{"a tag that only starts with rltest", head + "\tbuild\t-tags=rltestx\n" + tail, false},
		{"rltest in another setting", head + "\tbuild\t-ldflags=-X main.tag=rltest\n" + tail, false},
		{"rltest in a module path", strings.Replace(head, "ratelimiter/cmd/hitapi", "example.com/rltest", 1) + tail, false},
		{"debug/buildinfo form", "go\tgo1.27.1\npath\tratelimiter/cmd/hitapi\nbuild\t-compiler=gc\nbuild\t-tags=rltest\n", true},
		{"debug/buildinfo form, untagged", "go\tgo1.27.1\npath\tratelimiter/cmd/hitapi\nbuild\t-compiler=gc\n", false},
		{"empty", "", false},
	} {
		if got := listsRltest(c.text); got != c.want {
			t.Errorf("%s: listsRltest = %v, want %v\ntext:\n%s", c.name, got, c.want, c.text)
		}
	}
}
