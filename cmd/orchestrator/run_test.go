//go:build rltest

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/runlog"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
)

// lockedBuffer is a log destination that goroutines may write while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runTestSite is newTestSite with harness.toml's [store] addr on the site's store port, so a
// runner's prepare accepts it, and the repository's durable store profile.
func runTestSite(t *testing.T) site {
	t.Helper()
	return runTestSiteWith(t, testBins{})
}

// runTestSiteWith is runTestSite with bins in bin/bench (buildBins): a site a benchmark run
// passes preflight in.
func runTestSiteWith(t *testing.T, bins testBins) site {
	t.Helper()
	s := newTestSite(t, bins)
	data, err := os.ReadFile(s.path(harnessConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	const line = `addr = "127.0.0.1:6390"`
	if !bytes.Contains(data, []byte(line)) {
		t.Fatalf("harness.toml has no line %s", line)
	}
	text := strings.Replace(string(data), line, fmt.Sprintf(`addr = "127.0.0.1:%d"`, s.storePort), 1)
	writeFile(t, s.path(harnessConfigFile), text, 0o600)
	copyFile(t, filepath.Join("..", "..", "deploy", "valkey-durable.conf"), s.path("deploy/valkey-durable.conf"), 0o600)
	return s
}

// testRunner prepares a run of scenario text with fl in a fresh runTestSite (preflight, the run
// directory, the children's environments from a PATH-only base) and returns its runner, whose
// log goes to the returned buffer.
func testRunner(t *testing.T, text string, fl flags) (*runner, *lockedBuffer) {
	t.Helper()
	return testRunnerIn(t, runTestSite(t), text, fl)
}

// testRunnerIn is testRunner in the site s.
func testRunnerIn(t *testing.T, s site, text string, fl flags) (*runner, *lockedBuffer) {
	t.Helper()
	fl.scenario = filepath.Join(t.TempDir(), "scenario.toml")
	writeFile(t, fl.scenario, text, 0o600)
	in, refs := preflight(t.Context(), s, fl)
	if len(refs) > 0 {
		t.Fatalf("preflight refused the run: %+v", refs)
	}
	rd, err := createRunDir(s, in, testNow())
	if err != nil {
		t.Fatal(err)
	}
	envs, err := rd.pw.envs([]string{"PATH=/usr/bin:/bin"}, in.settings.Store.Addr)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	r := newRunner(slog.New(slog.NewJSONHandler(logs, nil)), s, in, rd, envs, newHTTPClient(), time.Now)
	if err := r.prepare(); err != nil {
		t.Fatal(err)
	}
	return r, logs
}

// bareRunner is a runner with only what the fault, rewrite and guard steps use: a log, main's
// HTTP client, the wall clock, a run directory and the store's rl_ops password.
func bareRunner(t *testing.T, opsPassword string) (*runner, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	dir := t.TempDir()
	return &runner{logger: slog.New(slog.NewJSONHandler(logs, nil)), client: newHTTPClient(), now: time.Now,
		runPath: dir, rd: runDir{id: "test-run", path: dir, pw: &passwords{ops: opsPassword}}}, logs
}

// Scripts for stand-in children: each prints "ready" to stderr once its SIGTERM handling is in
// place. ignoreTERM ignores SIGTERM (so do the sleeps it starts); exitOnTERM exits 0 300 ms
// after SIGTERM; dieOnTERM ends at SIGTERM.
const (
	ignoreTERM = `trap '' TERM; echo ready >&2; while :; do sleep 0.05; done`
	exitOnTERM = `trap 'sleep 0.3; exit 0' TERM; echo ready >&2; while :; do sleep 0.05; done`
	dieOnTERM  = `echo ready >&2; exec sleep 60`
)

// startStandIn starts script as the child name of role in dir through startChild, and waits
// until it printed "ready" to its output file. Cleanup kills its group if it still runs.
func startStandIn(t *testing.T, dir, name, role string, instance int, script string, env []string) *child {
	t.Helper()
	return startStandInCtx(t.Context(), t, dir, name, role, instance, script, env)
}

// startStandInCtx is startStandIn with the child started on ctx.
func startStandInCtx(ctx context.Context, t *testing.T, dir, name, role string, instance int, script string, env []string) *child {
	t.Helper()
	c, err := startChild(ctx, dir, childSpec{name: name, role: role, instance: instance, path: "/bin/sh",
		args: []string{"-c", script}, env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !c.exited() {
			_ = syscall.Kill(-c.proc.PID, syscall.SIGKILL)
			c.waitFor(reapWait)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, _ := os.ReadFile(filepath.Join(dir, name+".stderr")); bytes.Contains(data, []byte("ready")) {
			return c
		}
		if time.Now().After(deadline) || c.exited() {
			t.Fatalf("%s did not print ready (exited: %v)", name, c.exited())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rlNamesLine is the shell line with which a stand-in program prints the names of the RL_
// variables in its environment, sorted, on one line after "RL_NAMES:", never their values.
const rlNamesLine = `echo "RL_NAMES:$(/usr/bin/env | /usr/bin/sed -n 's/^\(RL_[A-Za-z0-9_]*\)=.*/\1/p' | /usr/bin/sort | /usr/bin/tr '\n' ' ')"`

// rlNames returns the RL_ variable names a stand-in printed in output (its RL_NAMES line).
func rlNames(t *testing.T, who string, output []byte) []string {
	t.Helper()
	for _, line := range strings.Split(string(output), "\n") {
		if names, ok := strings.CutPrefix(line, "RL_NAMES:"); ok {
			return strings.Fields(names)
		}
	}
	t.Fatalf("the stand-in for %s printed no RL_NAMES line: %q", who, output)
	return nil
}

// runFiles writes the run files the orchestrator itself produces, for the password checks
// (AGENTS.md §1 Secrets): rladmin start-epoch's saved stderr, through the run's own step with a
// stand-in rladmin; the report steps' saved output (the builder's, validate.py's, which fails,
// the marking invalid that follows and the thresholds freeze's), through the run's own steps with
// a stand-in python3 first on PATH; each child's saved output, started from the run's own child
// specs (arguments and environment as the run gives them) with a stand-in program that prints
// its arguments and the names of its RL_ variables; and context.json with the host part
// gathered. A run without instances gets one. Each child and command must have exactly the RL_
// variables of the store users it uses (DESIGN §A2 "Used by"; plan H3 step 2): none for
// valkey-server, hitgen, the report steps and pg_ctl, RL_LIB_PASSWORD for each hitapi,
// RL_ADMIN_PASSWORD, RL_OPS_PASSWORD and RL_STORE_ADDR for the sampler, RL_ADMIN_PASSWORD and
// RL_STORE_ADDR for rladmin. pg_ctl is checked on both of its
// calls (the run's own step, a stand-in first on PATH that answers status with 3, not running,
// so the step goes on to start the cluster): `-D <data> status`, then `-D <data> -l <log> -w
// start`, the postmaster that start leaves running inheriting that environment. It returns the
// files' names in the run directory.
func runFiles(t *testing.T, r *runner) []string {
	t.Helper()
	fake := t.TempDir()
	standIn := filepath.Join(fake, "standin")
	writeFile(t, standIn, "#!/bin/sh\necho \"$0 $*\" >&2\n"+rlNamesLine+" >&2\n", 0o700)
	writeFile(t, filepath.Join(fake, "rladmin"),
		"#!/bin/sh\necho \"$0 $*\" >&2\n"+rlNamesLine+" >&2\necho '{\"epoch_id\":\"5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f\",\"t_us\":1}'\n", 0o700)
	writeFile(t, filepath.Join(fake, "python3"), "#!/bin/sh\necho \"$0 $*\" >&2\n"+rlNamesLine+" >&2\n"+
		"[ \"$1\" != report/validate.py ] || exit 1\n", 0o700)
	pgEnv := filepath.Join(fake, "pg_ctl.env")
	writeFile(t, filepath.Join(fake, "pg_ctl"), "#!/bin/sh\n{ echo \"CALL $*\"; "+rlNamesLine+"; } >> '"+pgEnv+"'\n"+
		"for a; do last=$a; done\n[ \"$last\" != status ] || exit 3\n", 0o700)
	wantNames := map[string][]string{
		"valkey-server": nil, "hitgen": nil, "report": nil, "validate": nil, "mark-invalid": nil, "thresholds": nil,
		"pg_ctl status": nil, "pg_ctl start": nil,
		"hitapi-1": {"RL_LIB_PASSWORD"}, "hitapi-2": {"RL_LIB_PASSWORD"}, "hitapi-3": {"RL_LIB_PASSWORD"},
		"sampler": {"RL_ADMIN_PASSWORD", "RL_OPS_PASSWORD", "RL_STORE_ADDR"},
		"rladmin": {"RL_ADMIN_PASSWORD", "RL_STORE_ADDR"},
	}
	checkNames := func(who string, output []byte) {
		t.Helper()
		want, ok := wantNames[who]
		if !ok {
			t.Fatalf("no environment expected for %s", who)
		}
		if got := rlNames(t, who, output); !slices.Equal(got, want) {
			t.Errorf("%s's environment has the RL_ variables %q, want %q", who, got, want)
		}
	}

	r.binDir = fake
	if err := r.startEpoch(t.Context()); err != nil || r.rec.EpochID != "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f" {
		t.Fatalf("start-epoch with the stand-in rladmin: epoch_id %q, %v", r.rec.EpochID, err)
	}
	data, err := os.ReadFile(filepath.Join(r.runPath, "rladmin.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	checkNames("rladmin", data)
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, prog := range []string{"pg_ctl", "python3"} {
		if p, err := exec.LookPath(prog); err != nil || p != filepath.Join(fake, prog) {
			t.Fatalf("%s resolves to %q (%v), want the stand-in in %s", prog, p, err, fake)
		}
	}
	if err := r.startPostgres(t.Context()); err != nil {
		t.Fatalf("pg_ctl status, then start, through the stand-in: %v", err)
	}
	if data, err = os.ReadFile(pgEnv); err != nil {
		t.Fatal(err)
	}
	pgData, pgLogFile := filepath.Join(r.s.root, ".data", "pg"), filepath.Join(r.s.root, ".data", "pg.log")
	wantCalls := [][]string{{"-D", pgData, "status"}, {"-D", pgData, "-l", pgLogFile, "-w", "start"}}
	calls := strings.Split(strings.TrimPrefix(string(data), "CALL "), "\nCALL ")
	if len(calls) != len(wantCalls) {
		t.Fatalf("pg_ctl was called %d times: %q; want status, then start", len(calls), data)
	}
	for i, call := range calls {
		args, _, _ := strings.Cut(call, "\n")
		if !slices.Equal(strings.Fields(args), wantCalls[i]) {
			t.Errorf("pg_ctl call %d: %q, want %q", i+1, args, wantCalls[i])
		}
		checkNames("pg_ctl "+wantCalls[i][len(wantCalls[i])-1], []byte(call))
	}
	if err := r.report(t.Context()); err == nil || !strings.Contains(err.Error(), "report/validate.py does not pass the report") {
		t.Fatalf("the report through the stand-in python3: %v, want validate.py's failure", err)
	}
	if err := r.freeze(t.Context()); err != nil {
		t.Fatalf("the thresholds freeze through the stand-in python3: %v", err)
	}
	for name, args := range map[string]string{"report": "-m report " + r.runPath, "validate": "report/validate.py " + r.runPath,
		"mark-invalid": "-m report --mark-invalid " + r.runPath, "thresholds": "-m report.thresholds " + r.runPath + " --out "} {
		if data, err = os.ReadFile(filepath.Join(r.runPath, name+".stderr")); err != nil || !bytes.Contains(data, []byte(args)) {
			t.Fatalf("%s.stderr holds %q (%v), want the stand-in's line for python3 %s", name, data, err, args)
		}
		checkNames(name, data)
	}

	if len(r.apis) == 0 {
		r.apis = []apiInstance{{addr: "127.0.0.1:8081", config: filepath.Join(r.runPath, configFile), grace: 45 * time.Second}}
	}
	names := []string{"context.json", "rladmin.stderr", "report.stderr", "validate.stderr", "mark-invalid.stderr", "thresholds.stderr"}
	specs := []childSpec{r.storeSpec(), r.samplerSpec(), r.hitgenSpec(newT0Watch())}
	for i := range r.apis {
		specs = append(specs, r.apiSpec(i))
	}
	for _, sp := range specs {
		sp.path = standIn
		c, err := startChild(t.Context(), r.runPath, sp)
		if err != nil {
			t.Fatal(err)
		}
		if !c.waitFor(10 * time.Second) {
			t.Fatalf("the stand-in for %s did not exit", sp.name)
		}
		data, err := os.ReadFile(filepath.Join(r.runPath, sp.name+".stderr"))
		if err != nil || !bytes.Contains(data, []byte(standIn)) {
			t.Fatalf("%s.stderr holds %q (%v), want the stand-in's line", sp.name, data, err)
		}
		checkNames(sp.name, data)
		names = append(names, sp.name+".stderr")
	}
	r.gatherHost(t.Context())
	if err := r.writeContext(); err != nil {
		t.Fatal(err)
	}
	return names
}

// gone reports whether no process pid exists any more.
func gone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// TestRun_KillGrace (plan H3 step 3; DESIGN §B10; HARNESS HAR-13, HAR-15): the kill grace is
// P + drain_timeout + 5 s from the config file an instance starts with, 45 s from harness.toml
// (5 s + 35 s + 5 s); instance 3, started with its own file setting P = 2 s and drain_timeout
// = 10 s, gets 17 s, and its file is copied into the run directory. Stopping the instances
// sends SIGTERM to each at once, then waits each one's own grace before SIGKILL, which is
// recorded: an instance that ignores SIGTERM is killed only once its grace has passed, and
// one that exits within its grace is not killed.
func TestRun_KillGrace(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("..", "..", "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	_, f, err := runConfig(base, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := killGrace(f); got != 45*time.Second {
		t.Errorf("kill grace from harness.toml %v, want 45s (P 5 s + drain_timeout 35 s + 5 s)", got)
	}

	s := runTestSite(t)
	siteBase, err := os.ReadFile(s.path(harnessConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	own, err := config.Merge(siteBase, []byte("[instance]\nmax_release_delay_ns = 2_000_000_000\ndrain_timeout_ns = 10_000_000_000\n"))
	if err != nil {
		t.Fatal(err)
	}
	ownPath := filepath.Join(t.TempDir(), "own.toml")
	writeFile(t, ownPath, string(own), 0o600)
	r, _ := testRunner(t, scenario("test", "", ""), flags{instanceConfigs: map[int]string{3: ownPath}})
	for i, want := range []time.Duration{45 * time.Second, 45 * time.Second, 17 * time.Second} {
		if got := r.apis[i].grace; got != want {
			t.Errorf("instance %d's kill grace %v, want %v", i+1, got, want)
		}
	}
	if got, want := r.apis[2].config, filepath.Join(r.runPath, "instance-3.toml"); got != want {
		t.Errorf("instance 3's config file %s, want its copy %s", got, want)
	} else if data, err := os.ReadFile(got); err != nil || !bytes.Equal(data, own) {
		t.Errorf("instance 3's copy holds %q (%v), want the --instance-config file's bytes", data, err)
	}
	if got, want := r.apis[0].config, filepath.Join(r.runPath, "config.toml"); got != want {
		t.Errorf("instance 1's config file %s, want the run's %s", got, want)
	}
	if ic := r.rec.InstanceConfigs; len(ic) != 1 || ic[0].Instance != 3 || ic[0].Source != ownPath || ic[0].File != "instance-3.toml" {
		t.Errorf("context.json instance_configs %+v, want instance 3 from %s as instance-3.toml", ic, ownPath)
	}

	// The stop: the graces are shortened, so the test waits about 1.2 s, not 45 s.
	graces := []time.Duration{400 * time.Millisecond, time.Second, 1200 * time.Millisecond}
	scripts := []string{ignoreTERM, exitOnTERM, ignoreTERM}
	for i := range r.apis {
		r.apis[i].grace = graces[i]
		r.apiProcs = append(r.apiProcs, startStandIn(t, r.runPath, fmt.Sprintf("hitapi-%d", i+1), runlog.RoleHitAPI, i+1, scripts[i], nil))
	}
	start := time.Now()
	r.stopAPIs()
	elapsed := time.Since(start)
	if elapsed < graces[2] || elapsed > graces[2]+reapWait {
		t.Errorf("stopping the instances took %v, want instance 3's grace %v and little more", elapsed, graces[2])
	}
	if len(r.rec.Children) != 3 {
		t.Fatalf("records %+v, want one per instance", r.rec.Children)
	}
	for i, want := range []struct {
		killed bool
		exit   string
	}{{true, "signal: killed"}, {false, "exit 0"}, {true, "signal: killed"}} {
		rec := r.rec.Children[i]
		if rec.Instance != i+1 || rec.Killed != want.killed || rec.Exit != want.exit || rec.GraceNs != graces[i].Nanoseconds() ||
			rec.Early || rec.Error != "" {
			t.Errorf("instance %d's record %+v, want killed %v, %s, grace_ns %d", i+1, rec, want.killed, want.exit, graces[i].Nanoseconds())
		}
		if !gone(rec.PID) {
			t.Errorf("instance %d (pid %d) still runs after the stop", i+1, rec.PID)
		}
	}

	// At once: three instances that ignore SIGTERM, each with a 1 s grace, are all killed about
	// 1 s after the stop began; stopping them one at a time would take 3 s.
	br, _ := bareRunner(t, "")
	for i := range 3 {
		br.apis = append(br.apis, apiInstance{grace: time.Second})
		br.apiProcs = append(br.apiProcs, startStandIn(t, br.runPath, fmt.Sprintf("hitapi-%d", i+1), runlog.RoleHitAPI, i+1, ignoreTERM, nil))
	}
	start = time.Now()
	br.stopAPIs()
	if elapsed := time.Since(start); elapsed < time.Second || elapsed >= 2*time.Second {
		t.Errorf("stopping 3 instances that ignore SIGTERM, each with a 1 s grace, took %v, want about 1 s and under 2 s", elapsed)
	}
	for i, rec := range br.rec.Children {
		if !rec.Killed || rec.Exit != "signal: killed" || !gone(rec.PID) {
			t.Errorf("instance %d's record %+v, want killed after its grace and gone", i+1, rec)
		}
	}
}

// Stub INFO persistence replies: a rewrite in progress, done, only scheduled, and done with
// aof_last_bgrewrite_status err.
const (
	infoRewriting = "# Persistence\r\nloading:0\r\naof_enabled:1\r\naof_rewrite_in_progress:1\r\naof_rewrite_scheduled:0\r\n" +
		"aof_last_bgrewrite_status:ok\r\n"
	infoRewritten = "# Persistence\r\nloading:0\r\naof_enabled:1\r\naof_rewrite_in_progress:0\r\naof_rewrite_scheduled:0\r\n" +
		"aof_last_bgrewrite_status:ok\r\n"
	infoScheduled = "# Persistence\r\nloading:0\r\naof_enabled:1\r\naof_rewrite_in_progress:0\r\naof_rewrite_scheduled:1\r\n" +
		"aof_last_bgrewrite_status:ok\r\n"
	infoRewriteFailed = "# Persistence\r\nloading:0\r\naof_enabled:1\r\naof_rewrite_in_progress:0\r\naof_rewrite_scheduled:0\r\n" +
		"aof_last_bgrewrite_status:err\r\n"
)

// The store's replies to BGREWRITEAOF (Valkey's texts): a rewrite started, one already running.
const (
	rewriteStartedReply  = "Background append only file rewriting started"
	rewriteRunningReply  = "ERR Background append only file rewriting already in progress"
	rewriteCannotReply   = "ERR Can't execute an AOF background rewriting. Please check the server logs for more information."
	rewriteCannotExcerpt = "Can't execute an AOF background rewriting"
)

// TestRun_StoreArgs (plan H3 step 3; DESIGN §A2, §B5 "Forced rewrites"): without
// forced_aof_rewrites_s the store starts with the durable profile, the run's port, store
// directory and ACL file, and --auto-aof-rewrite-percentage 0, and no rewrite is scheduled;
// with [180, 420] it keeps the profile's production rewrite settings (no override) and
// BGREWRITEAOF is scheduled 180 s and 420 s after hitgen's t0. Against a stub store, each
// rewrite runs at its offset as rl_ops: BGREWRITEAOF, then INFO persistence at once and every
// 100 ms until aof_rewrite_in_progress:0 (a rewrite the store only scheduled is waited for
// too), recorded as {start_ns, end_ns} with the final aof_last_bgrewrite_status. The offsets
// are shortened: the test never waits 180 s.
func TestRun_StoreArgs(t *testing.T) {
	r, _ := testRunner(t, scenario("test", "", ""), flags{})
	dir := filepath.Join(r.runPath, "store")
	conf, err := filepath.Abs(r.s.path("deploy/valkey-durable.conf"))
	if err != nil {
		t.Fatal(err)
	}
	head := []string{conf, "--port", strconv.Itoa(r.s.storePort), "--dir", dir, "--aclfile", filepath.Join(dir, "acl.conf")}
	if got, want := r.storeArgs(), append(slices.Clone(head), "--auto-aof-rewrite-percentage", "0"); !slices.Equal(got, want) {
		t.Errorf("valkey-server arguments without forced rewrites\n%q\nwant\n%q", got, want)
	}
	if got := rewriteSchedule(r.in.scenario.ForcedAOFRewritesS); len(got) != 0 {
		t.Errorf("rewrites scheduled %v, want none", got)
	}

	const long = "[[steps]]\nrate_per_s = 30\nduration_s = 600\n\n[users]\ncount = 50\n"
	forced, _ := testRunner(t, scenario("test", "forced_aof_rewrites_s = [180, 420]", long), flags{})
	dir = filepath.Join(forced.runPath, "store")
	conf, err = filepath.Abs(forced.s.path("deploy/valkey-durable.conf"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{conf, "--port", strconv.Itoa(forced.s.storePort), "--dir", dir, "--aclfile", filepath.Join(dir, "acl.conf")}
	if got := forced.storeArgs(); !slices.Equal(got, want) {
		t.Errorf("valkey-server arguments with forced rewrites\n%q\nwant\n%q (the profile's production rewrite settings)", got, want)
	}
	if got, want := rewriteSchedule(forced.in.scenario.ForcedAOFRewritesS), []time.Duration{180 * time.Second, 420 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("rewrites scheduled at %v, want %v after t0", got, want)
	}

	const opsPassword = "ops-pw-5be1c0d2"
	store := fakeresp.Start(t, "rl_ops", opsPassword)
	store.ReplyValue("BGREWRITEAOF", resp.Value{Kind: resp.Simple, Str: "Background append only file rewriting started"}, 1)
	store.ReplyValue("BGREWRITEAOF", resp.Value{Kind: resp.Simple, Str: "Background append only file rewriting scheduled"}, 1)
	for _, info := range []string{infoRewriting, infoRewriting, infoRewritten, infoScheduled, infoRewriting, infoRewritten} {
		store.ReplyValue("INFO", resp.Value{Kind: resp.Bulk, Str: info}, 1)
	}
	br, _ := bareRunner(t, opsPassword)
	br.storeAddr = store.Addr()
	offsets := []time.Duration{100 * time.Millisecond, 700 * time.Millisecond}
	t0, wall := time.Now(), time.Now().UnixNano()
	recs := br.runRewrites(t.Context(), t0, offsets, make(chan struct{}))
	if len(recs) != 2 {
		t.Fatalf("rewrites %+v, want 2", recs)
	}
	for i, rec := range recs {
		due := wall + offsets[i].Nanoseconds()
		// Three polls, 100 ms apart, each: the third sees the rewrite done.
		if rec.Error != "" || rec.Status != "ok" || rec.AtS != offsets[i].Seconds() || rec.StartNs < due-int64(time.Millisecond) ||
			rec.StartNs > due+int64(100*time.Millisecond) || rec.EndNs-rec.StartNs < int64(190*time.Millisecond) ||
			rec.EndNs-rec.StartNs > int64(2*time.Second) {
			t.Errorf("rewrite %d: %+v; want status ok, start_ns from %d (t0 + %v), end_ns 200 ms or more later", i+1, rec, due, offsets[i])
		}
	}
	var sent [][]string
	for _, c := range store.Commands() {
		sent = append(sent, c.Args)
	}
	one := [][]string{{"AUTH", "rl_ops", opsPassword}, {"BGREWRITEAOF"}, {"INFO", "persistence"}, {"INFO", "persistence"}, {"INFO", "persistence"}}
	if want := append(slices.Clone(one), one...); !slices.EqualFunc(sent, want, slices.Equal[[]string]) {
		t.Errorf("the store got %v, want per rewrite AUTH as rl_ops, BGREWRITEAOF and 3 INFO persistence", store.Commands())
	}
}

// TestRun_RewriteReplies (IMPLEMENTATION_PLAN H3 step 3; DESIGN §B5 "Forced rewrites"): a forced
// rewrite that finds a rewrite running, such as an automatic one (BGREWRITEAOF answers "ERR
// Background append only file rewriting already in progress"), joins it: INFO persistence is
// polled until it is done and its window is recorded with the status "joined". Any other error
// reply fails the rewrite at once, with no poll. A rewrite that ends with
// aof_last_bgrewrite_status err fails, joined or not, which fails the run (TestRun_FinishRun).
func TestRun_RewriteReplies(t *testing.T) {
	const opsPassword = "ops-pw-c41e8b07"
	started := resp.Value{Kind: resp.Simple, Str: rewriteStartedReply}
	running := resp.Value{Kind: resp.Error, Str: rewriteRunningReply}
	for _, c := range []struct {
		name   string
		reply  resp.Value
		infos  []string // INFO persistence replies, in order
		status string
		err    string // in the record's error; "" for none
	}{
		{"joins a running rewrite", running, []string{infoRewriting, infoRewritten}, "joined", ""},
		{"another error reply", resp.Value{Kind: resp.Error, Str: rewriteCannotReply}, nil, "", rewriteCannotExcerpt},
		{"ends with status err", started, []string{infoRewriting, infoRewriteFailed}, "err", `aof_last_bgrewrite_status "err"`},
		{"joins a rewrite that ends with status err", running, []string{infoRewriting, infoRewriteFailed}, "joined",
			`aof_last_bgrewrite_status "err"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := fakeresp.Start(t, "rl_ops", opsPassword)
			store.ReplyValue("BGREWRITEAOF", c.reply, 1)
			for _, info := range c.infos {
				store.ReplyValue("INFO", resp.Value{Kind: resp.Bulk, Str: info}, 1)
			}
			r, _ := bareRunner(t, opsPassword)
			r.storeAddr = store.Addr()
			recs := r.runRewrites(t.Context(), time.Now(), []time.Duration{0}, make(chan struct{}))
			if len(recs) != 1 {
				t.Fatalf("rewrites %+v, want 1", recs)
			}
			rec := recs[0]
			if rec.Status != c.status || (rec.Error == "") != (c.err == "") || !strings.Contains(rec.Error, c.err) {
				t.Errorf("rewrite %+v, want status %q and an error holding %q", rec, c.status, c.err)
			}
			want := [][]string{{"AUTH", "rl_ops", opsPassword}, {"BGREWRITEAOF"}}
			for range c.infos {
				want = append(want, []string{"INFO", "persistence"})
			}
			var sent [][]string
			for _, cmd := range store.Commands() {
				sent = append(sent, cmd.Args)
			}
			if !slices.EqualFunc(sent, want, slices.Equal[[]string]) {
				t.Errorf("the store got %v, want AUTH as rl_ops, BGREWRITEAOF and %d INFO persistence", store.Commands(), len(c.infos))
			}
			// Two polls, 100 ms apart: the second sees the rewrite done.
			if len(c.infos) == 2 && (rec.StartNs == 0 || rec.EndNs-rec.StartNs < int64(90*time.Millisecond)) {
				t.Errorf("rewrite %+v, want its window from BGREWRITEAOF to the poll that saw it done, 100 ms or more", rec)
			}
		})
	}
}

// sendingLine is a hitgen "sending" log line, the line that marks its t0.
const sendingLine = `{"time":"2026-10-08T14:22:34Z","level":"INFO","msg":"sending","entries":300}`

// startHitgenStandIn starts script as r's hitgen, its output read for t0 as a run reads it.
// Cleanup kills its group if it still runs.
func startHitgenStandIn(t *testing.T, r *runner, t0 *t0Watch, script string) *child {
	t.Helper()
	c, err := startChild(t.Context(), r.runPath, childSpec{name: nameHitgen, role: runlog.RoleHitgen, path: "/bin/sh",
		args: []string{"-c", script}, capture: &captureWriter{onLine: t0.line}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !c.exited() {
			_ = syscall.Kill(-c.proc.PID, syscall.SIGKILL)
			c.waitFor(reapWait)
		}
	})
	r.hitgen = c
	return c
}

// TestRun_RewritesFromT0 (IMPLEMENTATION_PLAN H3 step 3 "Forced rewrites"; plan choice: the
// offsets count from hitgen's t0): with forced_aof_rewrites_s = [0.3] and a stand-in hitgen
// that prints its "sending" line 500 ms after it starts, the run sends BGREWRITEAOF as rl_ops
// 0.3 s after the line was read, not after hitgen's start, polls INFO persistence until the
// rewrite is done, and records it for context.json's rewrites: one record, no error, start_ns
// at t0 + 0.3 s ± 100 ms, end_ns from start_ns on.
func TestRun_RewritesFromT0(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.toml")
	writeFile(t, path, scenario("test", "forced_aof_rewrites_s = [0.3]", ""), 0o600)
	sc, err := config.LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	const opsPassword = "ops-pw-7d3a9e21"
	store := fakeresp.Start(t, "rl_ops", opsPassword)
	store.ReplyValue("BGREWRITEAOF", resp.Value{Kind: resp.Simple, Str: rewriteStartedReply}, 1)
	store.ReplyValue("INFO", resp.Value{Kind: resp.Bulk, Str: infoRewritten}, 1)
	r, _ := bareRunner(t, opsPassword)
	r.storeAddr = store.Addr()
	r.in.scenario = sc
	t0 := newT0Watch()
	hg := startHitgenStandIn(t, r, t0, `sleep 0.5; echo '`+sendingLine+`' >&2; sleep 1`)
	r.drive(t.Context(), t0)
	if !hg.exited() {
		t.Error("drive returned while hitgen still ran")
	}
	if len(r.rec.Rewrites) != 1 {
		t.Fatalf("rewrite records %+v, want 1", r.rec.Rewrites)
	}
	const slack = 100 * time.Millisecond
	rec, due := r.rec.Rewrites[0], t0.at.UnixNano()+int64(300*time.Millisecond)
	if rec.Error != "" || rec.Status != "ok" || rec.AtS != 0.3 || rec.StartNs < due-int64(slack) || rec.StartNs > due+int64(slack) ||
		rec.EndNs < rec.StartNs {
		t.Errorf("rewrite %+v; want no error, status ok, start_ns within ±%v of %d (t0 + 0.3 s), end_ns from start_ns on", rec, slack, due)
	}
	var sent [][]string
	for _, c := range store.Commands() {
		sent = append(sent, c.Args)
	}
	if want := [][]string{{"AUTH", "rl_ops", opsPassword}, {"BGREWRITEAOF"}, {"INFO", "persistence"}}; !slices.EqualFunc(sent, want, slices.Equal[[]string]) {
		t.Errorf("the store got %v, want AUTH as rl_ops, BGREWRITEAOF and INFO persistence", store.Commands())
	}

	// context.json's rewrites carry each window as start_ns and end_ns: the report reads those
	// keys (DESIGN §B8 "p99 inside AOF rewrite windows"; H5).
	if err := r.writeContext(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(r.runPath, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Rewrites []map[string]json.RawMessage `json:"rewrites"`
	}
	if err := json.Unmarshal(data, &file); err != nil || len(file.Rewrites) != 1 {
		t.Fatalf("context.json rewrites %s (%v), want one window", data, err)
	}
	for key, want := range map[string]int64{"start_ns": rec.StartNs, "end_ns": rec.EndNs} {
		var got int64
		if raw, ok := file.Rewrites[0][key]; !ok || json.Unmarshal(raw, &got) != nil || got != want {
			t.Errorf("context.json rewrites[0] %v: %s = %s, want %d", file.Rewrites[0], key, file.Rewrites[0][key], want)
		}
	}
}

// faultRequest is one request a stub API received.
type faultRequest struct {
	at          time.Time
	method      string
	path        string
	contentType string
	body        string
}

// faultAPI is a stub instance that records every request and answers it.
type faultAPI struct {
	addr string
	mu   sync.Mutex
	got  []faultRequest
}

// newFaultAPI is a stub instance that answers every request at once with 200 {"ok":true}.
func newFaultAPI(t *testing.T) *faultAPI {
	t.Helper()
	return newFaultAPIWith(t, http.StatusOK, 0)
}

// newFaultAPIWith is a stub instance that records every request as it arrives and answers it
// with status after delay: {"ok":true} for 200, an error body otherwise.
func newFaultAPIWith(t *testing.T, status int, delay time.Duration) *faultAPI {
	t.Helper()
	a := &faultAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.got = append(a.got, faultRequest{at: time.Now(), method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: string(body)})
		a.mu.Unlock()
		if delay > 0 {
			wait := time.NewTimer(delay)
			defer wait.Stop()
			select {
			case <-wait.C:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		_, _ = io.WriteString(w, `{"error":"stub_failure"}`)
	}))
	t.Cleanup(srv.Close)
	a.addr = srv.Listener.Addr().String()
	return a
}

func (a *faultAPI) requests() []faultRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.got)
}

// TestRun_FaultSchedule (plan H3 step 3; DESIGN §B4 [[faults]], §B5 "Faults"): with three
// stub instances and hitgen's t0 taken now, a body with instance = 2 at 0.5 s is posted to
// /_test/faults of instance 2 only, and a body without instance at 1.0 s to all three, each
// as JSON, within ±100 ms of its time; each fault's record has its cmd, its instance (null
// for all), its start and end in wall-clock ns, and every target's 200. Instance 3 answers
// 300 ms late, so the all-instance fault ends 300 ms to 600 ms after it started: its end_ns
// comes once every target has answered. A fault whose time comes after hitgen exited is
// recorded as not run and never sent.
func TestRun_FaultSchedule(t *testing.T) {
	const (
		dbBody    = `{"target":"db","mode":"fail_before","count":1}`
		storeBody = `{"target":"store","mode":"drop_reply","match":"refund","count":1}`
		lateBody  = `{"target":"db","mode":"fail_after_commit","count":1}`
	)
	text := scenario("test", "", oneStep+"\n[[faults]]\nat_s = 1.0\ncmd = '"+storeBody+"'\n"+
		"\n[[faults]]\nat_s = 0.5\ncmd = '"+dbBody+"'\ninstance = 2\n"+
		"\n[[faults]]\nat_s = 3.0\ncmd = '"+lateBody+"'\n")
	path := filepath.Join(t.TempDir(), "scenario.toml")
	writeFile(t, path, text, 0o600)
	sc, err := config.LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	const late = 300 * time.Millisecond // instance 3's answer
	apis := []*faultAPI{newFaultAPI(t), newFaultAPI(t), newFaultAPIWith(t, http.StatusOK, late)}
	r, _ := bareRunner(t, "")
	for _, a := range apis {
		r.apis = append(r.apis, apiInstance{addr: a.addr})
	}
	stop := make(chan struct{})
	time.AfterFunc(1500*time.Millisecond, func() { close(stop) }) // hitgen exits at 1.5 s
	t0, wall := time.Now(), time.Now().UnixNano()
	recs := r.runFaults(t.Context(), t0, sc.Faults, stop)

	const slack = 100 * time.Millisecond
	within := func(at time.Time, offset time.Duration) bool {
		d := at.Sub(t0) - offset
		return d >= -slack && d <= slack
	}
	for i, a := range apis {
		var want []string
		if i == 1 {
			want = append(want, dbBody)
		}
		want = append(want, storeBody)
		got := a.requests()
		var bodies []string
		for _, q := range got {
			bodies = append(bodies, q.body)
			if q.method != http.MethodPost || q.path != "/_test/faults" || q.contentType != "application/json" {
				t.Errorf("instance %d got %s %s (%s), want POST /_test/faults as application/json", i+1, q.method, q.path, q.contentType)
			}
			switch q.body {
			case dbBody:
				if !within(q.at, 500*time.Millisecond) {
					t.Errorf("instance %d got the 0.5 s fault at %v after t0, want within ±%v", i+1, q.at.Sub(t0), slack)
				}
			case storeBody:
				if !within(q.at, time.Second) {
					t.Errorf("instance %d got the 1.0 s fault at %v after t0, want within ±%v", i+1, q.at.Sub(t0), slack)
				}
			}
		}
		if !slices.Equal(bodies, want) {
			t.Errorf("instance %d got the bodies %q, want %q", i+1, bodies, want)
		}
	}

	if len(recs) != 3 {
		t.Fatalf("records %+v, want one per [[faults]] entry", recs)
	}
	for i, want := range []struct {
		at       float64
		cmd      string
		instance *int
		targets  []int
		slow     bool // instance 3 is a target: the fault ends late after it starts
	}{{1.0, storeBody, nil, []int{1, 2, 3}, true}, {0.5, dbBody, &[]int{2}[0], []int{2}, false}} {
		rec := recs[i]
		due := wall + int64(want.at*float64(time.Second))
		endOK := rec.EndNs >= rec.StartNs && rec.EndNs <= due+int64(2*slack)
		if span := time.Duration(rec.EndNs - rec.StartNs); want.slow {
			endOK = span >= late && span < 2*late
		}
		if rec.AtS != want.at || rec.Cmd != want.cmd || rec.Error != "" || !rec.sent() || rec.StartNs < due-int64(slack) ||
			rec.StartNs > due+int64(slack) || !endOK {
			t.Errorf("record %d %+v, want at_s %v, cmd %s, start_ns within ±%v of %d, end_ns after it (%v to %v later with instance 3)",
				i, rec, want.at, want.cmd, slack, due, late, 2*late)
		}
		if (rec.Instance == nil) != (want.instance == nil) || (rec.Instance != nil && *rec.Instance != *want.instance) {
			t.Errorf("record %d instance %v, want %v (null: every instance)", i, rec.Instance, want.instance)
		}
		var targets []int
		for _, tg := range rec.Targets {
			targets = append(targets, tg.Instance)
			if tg.Status != http.StatusOK || tg.Error != "" {
				t.Errorf("record %d target %+v, want 200", i, tg)
			}
		}
		if !slices.Equal(targets, want.targets) {
			t.Errorf("record %d targets %v, want %v", i, targets, want.targets)
		}
	}
	if late := recs[2]; late.Error != notRunHitgen || late.StartNs != 0 || late.EndNs != 0 || len(late.Targets) != 0 {
		t.Errorf("the 3.0 s fault after hitgen exited: %+v, want not run, never sent", late)
	}
	data, err := json.Marshal(recs[0])
	if err != nil {
		t.Fatal(err)
	}
	var shown map[string]json.RawMessage
	if err := json.Unmarshal(data, &shown); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"cmd", "instance", "start_ns", "end_ns"} {
		if _, ok := shown[k]; !ok {
			t.Errorf("a fault's context.json entry %s has no %s", data, k)
		}
	}
	if string(shown["instance"]) != "null" {
		t.Errorf("the all-instances fault's instance %s, want null", shown["instance"])
	}
}

// TestRun_FaultScheduleIndependent (plan H3 step 3, "at each [[faults]].at_s"; DESIGN §B5
// "Faults"): each fault fires at its own time, whatever another fault's targets take to
// answer. Fault 1, at 0.2 s, goes to instance 1, which answers 500 ms late; fault 2, at 0.4 s,
// still reaches instance 2 within ±100 ms of its time. hitgen exits at 0.5 s, while fault 1
// still waits for its answer: runFaults returns only once that answer has come (0.7 s after
// t0 at the earliest), with fault 1's record final: armed, end_ns at least 500 ms after start_ns.
func TestRun_FaultScheduleIndependent(t *testing.T) {
	const (
		slowBody = `{"target":"db","mode":"fail_before","count":1}`
		fastBody = `{"target":"db","mode":"fail_after_commit","count":1}`
		slow     = 500 * time.Millisecond // instance 1's answer
		slack    = 100 * time.Millisecond
	)
	text := scenario("test", "", oneStep+"\n[[faults]]\nat_s = 0.2\ncmd = '"+slowBody+"'\ninstance = 1\n"+
		"\n[[faults]]\nat_s = 0.4\ncmd = '"+fastBody+"'\ninstance = 2\n")
	path := filepath.Join(t.TempDir(), "scenario.toml")
	writeFile(t, path, text, 0o600)
	sc, err := config.LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	apis := []*faultAPI{newFaultAPIWith(t, http.StatusOK, slow), newFaultAPI(t)}
	r, _ := bareRunner(t, "")
	for _, a := range apis {
		r.apis = append(r.apis, apiInstance{addr: a.addr})
	}
	stop := make(chan struct{})
	exit := time.AfterFunc(500*time.Millisecond, func() { close(stop) }) // hitgen exits at 0.5 s
	defer exit.Stop()
	t0 := time.Now()
	recs := r.runFaults(t.Context(), t0, sc.Faults, stop)
	returned := time.Since(t0)

	for i, want := range []struct {
		body string
		at   time.Duration
	}{{slowBody, 200 * time.Millisecond}, {fastBody, 400 * time.Millisecond}} {
		got := apis[i].requests()
		if len(got) != 1 || got[0].body != want.body {
			t.Errorf("instance %d got %d requests %+v, want its one fault, due %v after t0", i+1, len(got), got, want.at)
			continue
		}
		if d := got[0].at.Sub(t0) - want.at; d < -slack || d > slack {
			t.Errorf("instance %d got its fault %v after t0, want %v ± %v: a fault waits for no other's answers",
				i+1, got[0].at.Sub(t0), want.at, slack)
		}
	}
	if len(recs) != 2 {
		t.Fatalf("records %+v, want one per [[faults]] entry", recs)
	}
	if rec := recs[0]; !rec.sent() || rec.Error != "" || time.Duration(rec.EndNs-rec.StartNs) < slow {
		t.Errorf("the slow fault's record %+v, want armed, with end_ns at least %v after start_ns: its answer, which came after "+
			"hitgen exited, is waited for", rec, slow)
	}
	if rec := recs[1]; !rec.sent() || rec.Error != "" {
		t.Errorf("the 0.4 s fault's record %+v, want armed", rec)
	}
	if earliest := 200*time.Millisecond + slow; returned < earliest {
		t.Errorf("runFaults returned %v after t0, before the slow fault's answer (%v at the earliest): every post is joined first",
			returned, earliest)
	}
}

// healthJSON is a /health body with the fields the guard reads.
func healthJSON(clock string, fixtures bool) string {
	return fmt.Sprintf(`{"status":"ready","reason":"ready","epoch_id":"5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f","clock_source":%q,`+
		`"listener_drops":0,"store_error":"","fixtures":%t,"log_drops":0,"in_flight":0,"nofile_limit":8192}`, clock, fixtures)
}

// healthAPI is a stub instance serving body at /health with status.
func healthAPI(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// TestRun_HealthFixturesGuard (plan H3 step 3; DESIGN §B5 "Benchmark guard"; HARNESS HAR-24):
// once a benchmark run's instances are ready, the guard reads each /health and refuses the run
// when one reports fixtures: true (a test-build limiter) or clock_source controlled, or when
// one cannot be read or does not show those fields; instances that show production
// construction pass. A refusal stops every running instance (SIGTERM, then SIGKILL after its
// grace), logs "refusing the run" with the reason, records it in context.json and exits 2.
func TestRun_HealthFixturesGuard(t *testing.T) {
	clean := healthJSON("store", false)
	for _, c := range []struct {
		name   string
		health [3]string // "" serves clean
		status int       // instance 3's status
		want   []string
	}{
		{"production construction", [3]string{}, http.StatusOK, nil},
		{"fixtures on instance 2", [3]string{1: healthJSON("store", true)}, http.StatusOK, []string{reasonBenchHealthFixtures}},
		{"controlled clock on instance 3", [3]string{2: healthJSON("controlled", false)}, http.StatusOK, []string{reasonBenchHealthClock}},
		{"fixtures and controlled clock", [3]string{0: healthJSON("controlled", true)}, http.StatusOK,
			[]string{reasonBenchHealthClock, reasonBenchHealthFixtures}},
		{"no fixtures field", [3]string{0: `{"status":"ready","reason":"ready","clock_source":"store"}`}, http.StatusOK,
			[]string{reasonBenchHealthUnread}},
		{"not JSON", [3]string{1: "ready"}, http.StatusOK, []string{reasonBenchHealthUnread}},
		{"instance 3 answers 500", [3]string{}, http.StatusInternalServerError, []string{reasonBenchHealthUnread}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := bareRunner(t, "")
			for i, body := range c.health {
				if body == "" {
					body = clean
				}
				status := http.StatusOK
				if i == 2 {
					status = c.status
				}
				r.apis = append(r.apis, apiInstance{addr: healthAPI(t, status, body)})
			}
			var got []string
			for _, ref := range r.guardHealth(t.Context()) {
				got = append(got, ref.reason)
				if ref.detail == "" {
					t.Errorf("refusal %s without a detail", ref.reason)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("guard refusals %q, want %q", got, c.want)
			}
		})
	}

	t.Run("a refusal tears the run down", func(t *testing.T) {
		r, logs := bareRunner(t, "")
		bodies := []string{clean, healthJSON("store", true), clean}
		scripts := []string{dieOnTERM, exitOnTERM, ignoreTERM}
		for i, body := range bodies {
			r.apis = append(r.apis, apiInstance{addr: healthAPI(t, http.StatusOK, body), grace: 500 * time.Millisecond})
			r.apiProcs = append(r.apiProcs, startStandIn(t, r.runPath, fmt.Sprintf("hitapi-%d", i+1), runlog.RoleHitAPI, i+1, scripts[i], nil))
		}
		refs := r.guardHealth(t.Context())
		if len(refs) != 1 || refs[0].reason != reasonBenchHealthFixtures || !strings.Contains(refs[0].detail, "instance 2") {
			t.Fatalf("refusals %+v, want fixtures on instance 2", refs)
		}
		if code := r.refuse(t.Context(), refs...); code != exitRefused {
			t.Errorf("exit code %d, want %d", code, exitRefused)
		}
		if len(r.apiProcs) != 0 || len(r.rec.Children) != 3 {
			t.Fatalf("after the refusal: %d instances still held, records %+v; want every instance stopped", len(r.apiProcs), r.rec.Children)
		}
		for i, rec := range r.rec.Children {
			if !gone(rec.PID) || rec.Killed != (i == 2) {
				t.Errorf("instance %d: %+v, gone %v; want it stopped, killed only when it ignored SIGTERM", i+1, rec, gone(rec.PID))
			}
		}
		if got := refusalReasons(t, logs.String()); !slices.Equal(got, []string{reasonBenchHealthFixtures}) {
			t.Errorf("logged refusals %q, want %q\n%s", got, reasonBenchHealthFixtures, logs.String())
		}
		var rc runContext
		data, err := os.ReadFile(filepath.Join(r.runPath, "context.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &rc); err != nil || rc.Outcome != "refused" || !strings.Contains(rc.Error, reasonBenchHealthFixtures) ||
			len(rc.Children) != 3 {
			t.Errorf("context.json outcome %q, error %q, %d children (%v); want refused for %s, the 3 instances recorded",
				rc.Outcome, rc.Error, len(rc.Children), err, reasonBenchHealthFixtures)
		}
	})

	// The run's instances step: stand-in hitapi programs started as the run starts them, whose
	// /health (stubs) shows fixtures: true on instance 2 once ready. A benchmark run is refused
	// there with exit 2 and every instance stopped; a test run goes on.
	for _, c := range []struct {
		kind    string
		refused bool
	}{{config.RunBench, true}, {config.RunTest, false}} {
		t.Run("the instances step of a "+c.kind+" run", func(t *testing.T) {
			r, logs := bareRunner(t, "")
			r.in.scenario.RunKind = c.kind
			r.envs = &childEnvs{hitapi: []string{"PATH=/usr/bin:/bin"}}
			r.binDir = t.TempDir()
			writeFile(t, filepath.Join(r.binDir, "hitapi"), "#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 0.05; done\n", 0o700)
			for _, body := range []string{clean, healthJSON("store", true), clean} {
				r.apis = append(r.apis, apiInstance{addr: healthAPI(t, http.StatusOK, body), grace: 5 * time.Second})
			}
			t.Cleanup(r.stopAPIs) // the instances a failed check left running
			code, ok := r.instancesStep(t.Context())
			if !c.refused {
				if !ok || code != exitOK || len(r.apiProcs) != 3 {
					t.Fatalf("the instances step of a test run = %d, %v with %d instances; want it to go on with 3\n%s",
						code, ok, len(r.apiProcs), logs.String())
				}
				if got := refusalReasons(t, logs.String()); len(got) != 0 {
					t.Errorf("a test run logged the refusals %q, want none", got)
				}
				r.stopAPIs()
				for i, rec := range r.rec.Children {
					if !gone(rec.PID) {
						t.Errorf("instance %d (pid %d) still runs after its stop", i+1, rec.PID)
					}
				}
				return
			}
			if ok || code != exitRefused {
				t.Fatalf("the instances step of a benchmark run = %d, %v; want exit %d, refused\n%s", code, ok, exitRefused, logs.String())
			}
			if got := refusalReasons(t, logs.String()); !slices.Equal(got, []string{reasonBenchHealthFixtures}) {
				t.Errorf("logged refusals %q, want %q", got, reasonBenchHealthFixtures)
			}
			if len(r.apiProcs) != 0 || len(r.rec.Children) != 3 {
				t.Fatalf("after the refusal: %d instances still held, records %+v; want the 3 instances stopped", len(r.apiProcs), r.rec.Children)
			}
			for i, rec := range r.rec.Children {
				if rec.Name != fmt.Sprintf("hitapi-%d", i+1) || !gone(rec.PID) {
					t.Errorf("record %d %+v, gone %v; want hitapi-%d stopped", i, rec, gone(rec.PID), i+1)
				}
			}
			if rc := readContext(t, r.runPath); rc.Outcome != "refused" || !strings.Contains(rc.Error, reasonBenchHealthFixtures) {
				t.Errorf("context.json outcome %q, error %q; want refused for %s", rc.Outcome, rc.Error, reasonBenchHealthFixtures)
			}
		})
	}
}

// readContext decodes the run directory's context.json.
func readContext(t *testing.T, runPath string) runContext {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runPath, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rc runContext
	if err := json.Unmarshal(data, &rc); err != nil {
		t.Fatalf("context.json: %v\n%s", err, data)
	}
	return rc
}

// TestRun_HitgenT0 (plan H3 step 3; plan choice: faults and rewrites count from hitgen's t0):
// hitgen's output reaches its file byte for byte while the orchestrator reads its lines; t0 is
// the moment the whole "sending" log line has been read, once, whatever the writes' split;
// other lines, lines that are not JSON and lines over 64 KiB are not t0. A failed file write
// never fails the child's write: it is kept for the run's end, and lines are still read. A
// child that writes megabytes at once is never held up by the capture.
func TestRun_HitgenT0(t *testing.T) {
	const sending = `{"time":"2026-10-08T14:22:34Z","level":"INFO","msg":"sending","scenario":"pf","entries":300}`
	path := filepath.Join(t.TempDir(), "hitgen.stderr")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := newT0Watch()
	cw := &captureWriter{f: f, onLine: w.line}
	seen := func() bool {
		select {
		case <-w.seen:
			return true
		default:
			return false
		}
	}
	long := `{"msg":"sending","pad":"` + strings.Repeat("x", maxCaptureLine) + `"}`
	var all bytes.Buffer
	for _, chunk := range []string{
		"plain text line\n",
		`{"level":"INFO","msg":"sending done"}` + "\n",
		long[:1000], long[1000:] + "\n",
		"not json sending\n",
		sending[:20],
	} {
		if n, err := cw.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(chunk))
		}
		all.WriteString(chunk)
	}
	if seen() {
		t.Fatal("t0 taken before the sending line was complete")
	}
	before := time.Now()
	for _, chunk := range []string{sending[20:] + "\n", sending + "\n"} {
		if _, err := cw.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
		all.WriteString(chunk)
	}
	if !seen() || w.at.Before(before) || w.at.After(time.Now()) {
		t.Fatalf("t0 seen %v at %v, want taken when the sending line's last byte arrived (after %v)", seen(), w.at, before)
	}
	first := w.at
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, all.Bytes()) {
		t.Errorf("the file holds %d bytes (%v), want the %d bytes written, in order", len(data), err, all.Len())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := cw.Write([]byte("after the file closed\n")); n != 22 || err != nil {
		t.Errorf("Write after the file closed = %d, %v; want 22, nil: the child's pipe must keep draining", n, err)
	}
	if cw.writeErr() == nil {
		t.Error("writeErr after a failed file write: nil, want the error")
	}
	if w.at != first {
		t.Error("a second sending line moved t0")
	}

	// A real child: 2 MB in one go, then the sending line; it exits at once and every byte is
	// on the file.
	dir := t.TempDir()
	w2 := newT0Watch()
	c, err := startChild(t.Context(), dir, childSpec{name: nameHitgen, role: runlog.RoleHitgen, path: "/bin/sh",
		args:    []string{"-c", `head -c 2000000 /dev/zero | tr '\0' x; echo; echo '` + sending + `' >&2`},
		capture: &captureWriter{onLine: w2.line}})
	if err != nil {
		t.Fatal(err)
	}
	if !c.waitFor(20 * time.Second) {
		_ = syscall.Kill(-c.proc.PID, syscall.SIGKILL)
		t.Fatal("the child writing 2 MB did not exit within 20 s")
	}
	select {
	case <-w2.seen:
	default:
		t.Error("the child's sending line was not read")
	}
	if info, err := os.Stat(filepath.Join(dir, "hitgen.stderr")); err != nil || info.Size() != int64(2_000_000+1+len(sending)+1) {
		t.Errorf("hitgen.stderr: %v, %v; want %d bytes", info, err, 2_000_000+1+len(sending)+1)
	}
	if err := c.capture.writeErr(); err != nil {
		t.Errorf("writeErr %v, want nil", err)
	}
}

// TestRun_WaitReady (plan H3 steps 3 and 6): the run waits until every instance's /health has
// status ready; an instance started with --instance-config is ready once its reason is
// anything but no_epoch (fingerprint_mismatch, T-HAR-1); an instance still on no_epoch when
// the wait's bound passes fails it, naming the instance and its last answer; an instance that
// exits fails it at once.
func TestRun_WaitReady(t *testing.T) {
	notReady := func(reason string) string {
		return `{"status":"not_ready","reason":"` + reason + `","clock_source":"store","fixtures":true}`
	}
	ready := `{"status":"ready","reason":"ready","clock_source":"store","fixtures":true}`
	// late serves no_epoch until ok is set, then ready.
	type late struct {
		mu sync.Mutex
		ok bool
	}
	lateAPI := func(l *late) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.ok {
				_, _ = io.WriteString(w, ready)
				return
			}
			_, _ = io.WriteString(w, notReady("no_epoch"))
		}))
		t.Cleanup(srv.Close)
		return srv.Listener.Addr().String()
	}
	setup := func(t *testing.T, addrs []string, own []bool, scripts []string) *runner {
		t.Helper()
		r, _ := bareRunner(t, "")
		for i, a := range addrs {
			r.apis = append(r.apis, apiInstance{addr: a, own: own[i]})
			r.apiProcs = append(r.apiProcs, startStandIn(t, r.runPath, fmt.Sprintf("hitapi-%d", i+1), runlog.RoleHitAPI, i+1, scripts[i], nil))
		}
		return r
	}
	sleeps := []string{dieOnTERM, dieOnTERM, dieOnTERM}

	t.Run("ready, a late instance and an instance with its own config", func(t *testing.T) {
		l := &late{}
		r := setup(t, []string{healthAPI(t, http.StatusOK, ready), lateAPI(l), healthAPI(t, http.StatusOK, notReady("fingerprint_mismatch"))},
			[]bool{false, false, true}, sleeps)
		time.AfterFunc(300*time.Millisecond, func() {
			l.mu.Lock()
			l.ok = true
			l.mu.Unlock()
		})
		start := time.Now()
		if err := r.waitReady(t.Context(), 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d < 300*time.Millisecond {
			t.Errorf("the wait ended after %v, before instance 2 was ready (300 ms)", d)
		}
	})
	t.Run("own config still on no_epoch", func(t *testing.T) {
		r := setup(t, []string{healthAPI(t, http.StatusOK, ready), healthAPI(t, http.StatusOK, ready),
			healthAPI(t, http.StatusOK, notReady("no_epoch"))}, []bool{false, false, true}, sleeps)
		err := r.waitReady(t.Context(), time.Second)
		if err == nil || !strings.Contains(err.Error(), "hitapi 3") || !strings.Contains(err.Error(), "no_epoch") {
			t.Errorf("waitReady = %v, want a timeout naming hitapi 3 and no_epoch", err)
		}
	})
	t.Run("an instance with the run's config is ready only at status ready", func(t *testing.T) {
		r := setup(t, []string{healthAPI(t, http.StatusOK, notReady("fingerprint_mismatch"))}, []bool{false}, sleeps[:1])
		if err := r.waitReady(t.Context(), time.Second); err == nil || !strings.Contains(err.Error(), "hitapi 1") {
			t.Errorf("waitReady = %v, want a timeout naming hitapi 1", err)
		}
	})
	t.Run("an instance exits", func(t *testing.T) {
		r := setup(t, []string{healthAPI(t, http.StatusOK, notReady("no_epoch")), healthAPI(t, http.StatusOK, ready)},
			[]bool{false, false}, []string{`echo ready >&2; sleep 0.2; exit 3`, dieOnTERM})
		start := time.Now()
		err := r.waitReady(t.Context(), 10*time.Second)
		if err == nil || !strings.Contains(err.Error(), "hitapi 1") || !strings.Contains(err.Error(), "exit 3") {
			t.Errorf("waitReady = %v, want hitapi 1's exit 3", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("the wait took %v after the instance exited, want it ended at once", d)
		}
	})
}

// TestRun_FaultsFromT0 (plan choice for H3 step 3, from H2's review: offsets count from
// hitgen's t0, not its start): with a stand-in hitgen that prints its "sending" line 500 ms
// after it starts and exits 1.5 s later, a fault at 0.3 s reaches its instance 0.3 s after the
// line was read, 0.8 s after the start, and one at 2.5 s, due after hitgen exited, is recorded
// as not run; a hitgen that exits without the line runs no fault at all.
func TestRun_FaultsFromT0(t *testing.T) {
	const body = `{"target":"db","mode":"fail_before","count":1}`
	text := scenario("test", "", oneStep+"\n[[faults]]\nat_s = 0.3\ncmd = '"+body+"'\ninstance = 1\n"+
		"\n[[faults]]\nat_s = 2.5\ncmd = '"+body+"'\ninstance = 1\n")
	path := filepath.Join(t.TempDir(), "scenario.toml")
	writeFile(t, path, text, 0o600)
	sc, err := config.LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	const sending = `{"time":"2026-10-08T14:22:34Z","level":"INFO","msg":"sending","entries":300}`
	for _, c := range []struct {
		name, script string
		sends        bool
	}{
		{"sending after 500 ms", `sleep 0.5; echo '` + sending + `' >&2; sleep 1.5`, true},
		{"never sending", `echo '{"level":"ERROR","msg":"refusing the scenario"}' >&2; sleep 0.5; exit 2`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			api := newFaultAPI(t)
			r, _ := bareRunner(t, "")
			r.apis = []apiInstance{{addr: api.addr}}
			r.in.scenario = sc
			t0 := newT0Watch()
			start := time.Now()
			hg, err := startChild(t.Context(), r.runPath, childSpec{name: nameHitgen, role: runlog.RoleHitgen, path: "/bin/sh",
				args: []string{"-c", c.script}, capture: &captureWriter{onLine: t0.line}})
			if err != nil {
				t.Fatal(err)
			}
			r.hitgen = hg
			t.Cleanup(func() {
				_ = syscall.Kill(-hg.proc.PID, syscall.SIGKILL)
				hg.waitFor(reapWait)
			})
			r.drive(t.Context(), t0)
			if !hg.exited() {
				t.Error("drive returned while hitgen still ran")
			}
			got := api.requests()
			if len(r.rec.Faults) != 2 {
				t.Fatalf("fault records %+v, want 2", r.rec.Faults)
			}
			if !c.sends {
				for _, f := range r.rec.Faults {
					if f.Error != notRunNoT0 || f.StartNs != 0 {
						t.Errorf("fault %+v, want not run: hitgen never started sending", f)
					}
				}
				if len(got) != 0 {
					t.Errorf("the instance got %d requests, want none", len(got))
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("the instance got %d requests, want the 0.3 s fault only", len(got))
			}
			if d := got[0].at.Sub(t0.at); d < 200*time.Millisecond || d > 400*time.Millisecond {
				t.Errorf("the 0.3 s fault came %v after the sending line was read, want 0.3 s ± 100 ms", d)
			}
			if d := got[0].at.Sub(start); d < 700*time.Millisecond {
				t.Errorf("the 0.3 s fault came %v after hitgen started, want after its t0 (0.5 s) + 0.3 s", d)
			}
			if f := r.rec.Faults[0]; !f.sent() || f.Error != "" {
				t.Errorf("the 0.3 s fault's record %+v, want armed", f)
			}
			if f := r.rec.Faults[1]; f.Error != notRunHitgen || f.StartNs != 0 {
				t.Errorf("the 2.5 s fault's record %+v, want not run: hitgen exited first", f)
			}
		})
	}
}

// stopTestStore shuts down the store a test left running (Cleanup: a test that failed midway).
func stopTestStore(t *testing.T, r *runner) {
	t.Helper()
	if r.store != nil {
		if err := r.stopStore(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("shutting the test's store down: %v", err)
		}
	}
}

// TestRun_StoreMaxClients (IMPLEMENTATION_PLAN H3 step 3; DESIGN §B1): the run's store step
// refuses a store whose CONFIG GET maxclients is not 4096 once it has started it. A real
// valkey-server started from a durable profile that ends in maxclients 100 gives exit 2 with
// the reason store_maxclients, and the store is shut down: SHUTDOWN as rl_ops, so it exits 0,
// not killed, and its port is free. context.json records the refusal and the settings read.
func TestRun_StoreMaxClients(t *testing.T) {
	r, logs := testRunner(t, scenario("test", "", ""), flags{})
	conf := r.s.path("deploy/valkey-durable.conf")
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, conf, string(data)+"maxclients 100\n", 0o600)
	t.Cleanup(func() { stopTestStore(t, r) })
	code, ok := r.storeStep(t.Context())
	if ok || code != exitRefused {
		t.Fatalf("the store step = %d, %v; want exit %d, refused\n%s", code, ok, exitRefused, logs.String())
	}
	if got := refusalReasons(t, logs.String()); !slices.Equal(got, []string{reasonStoreMaxClients}) {
		t.Errorf("logged refusals %q, want %q", got, reasonStoreMaxClients)
	}
	if r.store != nil || len(r.rec.Children) != 1 {
		t.Fatalf("after the refusal: store still held %v, records %+v; want the store stopped and recorded", r.store != nil, r.rec.Children)
	}
	if rec := r.rec.Children[0]; rec.Name != "valkey-server" || rec.Exit != "exit 0" || rec.Killed || rec.Early || rec.Error != "" || !gone(rec.PID) {
		t.Errorf("the store's record %+v, gone %v; want valkey-server shut down: exit 0, not killed", rec, gone(rec.PID))
	}
	if held, why := portHeld(t.Context(), r.s.storePort); held {
		t.Errorf("the store's port %d after the refusal: %s, want it free", r.s.storePort, why)
	}
	rc := readContext(t, r.runPath)
	if rc.Outcome != "refused" || !strings.Contains(rc.Error, reasonStoreMaxClients) || rc.StoreSettings["maxclients"] != "100" {
		t.Errorf("context.json outcome %q, error %q, store maxclients %q; want refused for %s, maxclients 100 as read",
			rc.Outcome, rc.Error, rc.StoreSettings["maxclients"], reasonStoreMaxClients)
	}
}

// TestRun_ChildOutlivesCtx (AGENTS.md §2 "Child processes"; IMPLEMENTATION_PLAN H3 step 4): a
// child started on a ctx that then ends, as a signal ends the run's, keeps running until the
// orchestrator stops it: 300 ms after the ctx ended it still runs, and stop's SIGTERM lets it
// exit 0, not killed.
func TestRun_ChildOutlivesCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := startStandInCtx(ctx, t, t.TempDir(), "hitapi-1", runlog.RoleHitAPI, 1, exitOnTERM, nil)
	cancel()
	if c.waitFor(300 * time.Millisecond) {
		t.Fatalf("the child ended (%s) when its ctx ended, before its stop", exitText(c.waitErr))
	}
	if rec := c.stop(5 * time.Second); rec.Exit != "exit 0" || rec.Killed || rec.Early || rec.Error != "" {
		t.Errorf("the child's record %+v, want exit 0 after SIGTERM, not killed", rec)
	}
}

// Stand-in scripts for a run's end: an instance or the sampler that exits 0, or 1, on SIGTERM;
// an instance that exits 0 by itself 300 ms after it starts; a store that dies 200 ms after it
// starts; a hitgen that sends 100 ms after it starts and exits 0, or 1, 400 ms later, or that
// sends for 10 s; a report builder (python3) that prints ready, then runs for 30 s; a python3
// whose every report step prints its arguments and passes.
const (
	exit0OnTERM = `trap 'exit 0' TERM; echo ready >&2; while :; do sleep 0.05; done`
	exit1OnTERM = `trap 'exit 1' TERM; echo ready >&2; while :; do sleep 0.05; done`
	exitsEarly  = `echo ready >&2; sleep 0.3; exit 0`
	storeDies   = `echo ready >&2; sleep 0.2; exit 1`
	hitgenOK    = `sleep 0.1; echo '` + sendingLine + `' >&2; sleep 0.4`
	hitgenFails = hitgenOK + `; exit 1`
	hitgenLong  = `sleep 0.1; echo '` + sendingLine + `' >&2; sleep 10`
	reportLong  = "#!/bin/sh\necho ready >&2\nexec sleep 30\n"
	pythonOK    = "#!/bin/sh\necho \"python3 $*\" >&2\n"
)

// signalStepScript is a stand-in python3 for a signal while validate.py or the thresholds freeze
// runs: it appends each call's arguments, one line, to the file calls; `-m report --mark-invalid
// …` runs the real one (the program python, the repository repo on PYTHONPATH); `-m report
// <run>` copies the files report and markdown to <run>/report.json and <run>/report.md; the
// step block ("validate": report/validate.py; "thresholds": `-m report.thresholds`) prints ready,
// then sleeps 30 s (the freeze then creates the frozen file its --out names, as a freeze that
// ends would: the real python3 does both, so a kill ends it at once); every other step passes.
func signalStepScript(calls, report, markdown, python, repo, block string) string {
	validate, freeze := ":", ":"
	switch block {
	case "validate":
		validate = "echo ready >&2; exec sleep 30"
	case "thresholds":
		freeze = fmt.Sprintf(`echo ready >&2; exec '%s' -c 'import sys, time; time.sleep(30); open(sys.argv[1], "x").close()' "$5"`, python)
	}
	return fmt.Sprintf("#!/bin/sh\n"+
		"echo \"$*\" >> '%s'\n"+
		"case \"$1 $2 $3\" in\n"+
		"'-m report --mark-invalid') PYTHONPATH='%s' exec '%s' \"$@\" ;;\n"+
		"esac\n"+
		"case \"$1 $2\" in\n"+
		"'-m report') cp '%s' \"$3/report.json\"; cp '%s' \"$3/report.md\"; exit 0 ;;\n"+
		"'-m report.thresholds') %s ;;\n"+
		"esac\n"+
		"if [ \"$1\" = report/validate.py ]; then %s; fi\n",
		calls, repo, python, report, markdown, freeze, validate)
}

// checkMarked fails t unless the run's report.json and report.md were marked invalid by the
// real `python3 -m report --mark-invalid` (report/__main__.py) after the step record failed
// ("validate", or "freeze" with --freeze): evidence_validity.valid false, and
// evidence_validity.<record> saying passed false and naming the step's output file, out ("" for
// none: null).
func checkMarked(t *testing.T, runPath, record, out string) {
	t.Helper()
	var rep struct {
		Evidence map[string]json.RawMessage `json:"evidence_validity"`
	}
	data, err := os.ReadFile(filepath.Join(runPath, "report.json"))
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil {
		t.Fatalf("report.json: %v", err)
	}
	var step struct {
		Passed *bool  `json:"passed"`
		Output string `json:"output"`
	}
	if raw, ok := rep.Evidence[record]; !ok || json.Unmarshal(raw, &step) != nil || step.Passed == nil || *step.Passed || step.Output != out {
		t.Errorf("report.json evidence_validity %s, want valid false and %s {passed: false, output: %q}", data, record, out)
	}
	if string(rep.Evidence["valid"]) != "false" {
		t.Errorf("report.json evidence_validity.valid %s, want false", rep.Evidence["valid"])
	}
	md, err := os.ReadFile(filepath.Join(runPath, "report.md"))
	if err != nil || !strings.Contains(string(md), "| valid | false |") || strings.Contains(string(md), "| valid | true |") ||
		!strings.Contains(string(md), "| "+record+".output | "+cmp.Or(out, "null")+" |") {
		t.Errorf("report.md holds (%v):\n%s\nwant it marked invalid with report.json, naming %q", err, md, out)
	}
}

// standInPython puts script first on PATH as python3 for the rest of the test: the run's report
// steps (python3 -m report, report/validate.py, python3 -m report.thresholds) run it.
func standInPython(t *testing.T, script string) {
	t.Helper()
	fake := t.TempDir()
	writeFile(t, filepath.Join(fake, "python3"), script, 0o700)
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	if p, err := exec.LookPath("python3"); err != nil || p != filepath.Join(fake, "python3") {
		t.Fatalf("python3 resolves to %q (%v), want the stand-in in %s", p, err, fake)
	}
}

// TestRun_FinishRun (IMPLEMENTATION_PLAN H3 steps 3 and 4): the run once hitgen has started
// (afterHitgen: drive, the ctx check, finishRun), with stand-in children, no store and a row
// counter that reports 7 rows. A finished run, and one whose instance is killed after its grace
// (only recorded), exit 0 with db_rows.json {"db_rows":7} and one runs/index.jsonl line,
// {scenario, baseline, run_id}. Every failure exits 1, context.json's outcome is failed naming
// it, and runs/index.jsonl gets no line: hitgen exits 1; a fault is not armed (its instance
// answers 500); a faultctl fault fails (store pause 1s, run in-process with no store: plan H4
// step 7); a forced rewrite ends with aof_last_bgrewrite_status err; an instance exits
// before its stop; an instance or the sampler exits 1 after SIGTERM; the store exits during the
// run (recorded in context.json); a signal while hitgen sends; a signal once hitgen has exited,
// before finishRun; a signal while a report step runs, which stops it at once (the report steps
// run on the caller's ctx: a stand-in python3 that would run 30 s is killed, finishRun returns
// within 5 s of the signal, and context.json's error names the step and the signal): the report
// builder, report/validate.py in a limiter run, and the thresholds freeze in a baseline, none of
// which then leaves a frozen file, the last two marking the report invalid all the same (the real
// `python3 -m report --mark-invalid [--freeze]`, after the signal); a signal while the store shuts
// down, after validate.py passed a baseline's report and before its thresholds freeze (the stub
// store's SHUTDOWN brings it, then the store exits 0), which keeps the freeze from running and
// marks the report invalid all the same (the real `python3 -m report --mark-invalid --freeze`,
// the one call after validate.py, no freeze output named), leaving no frozen file; a run index
// line that cannot be appended (runs/index.jsonl is a directory). Every child is stopped, in
// order: hitgen, the instances, the sampler, then the store when there is one.
func TestRun_FinishRun(t *testing.T) {
	const (
		faultTables    = oneStep + "\n[[faults]]\nat_s = 0.1\ncmd = '{\"target\":\"db\",\"mode\":\"fail_before\",\"count\":1}'\ninstance = 1\n"
		faultctlTables = oneStep + "\n[[faults]]\nat_s = 0.1\ncmd = \"faultctl store pause 1s\"\n"
		indexLine      = `{"scenario":"pf","baseline":false,"run_id":"20261008-142233-pf-1"}` + "\n"
	)
	for _, c := range []struct {
		name        string
		top, tables string    // the scenario's top-level lines and tables
		hitgen      string    // "" runs hitgenOK
		apis        [3]string // "" runs exit0OnTERM
		shortGrace  bool      // instance 3's grace is 300 ms
		sampler     string    // "" runs exit0OnTERM
		store       string    // a store stand-in; "" runs none
		faultStatus int       // instance 1's answer to a fault
		rewriteInfo string    // the stub store's INFO persistence once the rewrite ends
		report      string    // the stand-in python3 first on PATH for the report steps; "" runs pythonOK
		block       string    // the stand-in python3 is signalStepScript's, blocking in this step (report is unused)
		baseline    bool      // a baseline run (limiter off), with no frozen thresholds file before it
		indexDir    bool      // runs/index.jsonl is a directory
		signal      string    // "while sending", "after hitgen", "during a report step", "while the store shuts down" or ""
		stepOut     string    // during a report step: the step's output file, whose "ready" brings the signal
		stepErr     string    // during a report step: the text naming the step in context.json's error
		wantErr     string    // in context.json's error; "" for a finished run
	}{
		{name: "a finished run"},
		{name: "an instance killed after its grace", apis: [3]string{2: ignoreTERM}, shortGrace: true},
		{name: "hitgen exits 1", hitgen: hitgenFails, wantErr: "hitgen ended with exit 1"},
		{name: "a fault is not armed", tables: faultTables, faultStatus: http.StatusInternalServerError,
			wantErr: "a fault at 0.1 s was not armed everywhere"},
		{name: "a faultctl fault fails", tables: faultctlTables,
			wantErr: "the fault at 0.1 s, faultctl store pause 1s, failed: the store is not running"},
		{name: "a forced rewrite ends with status err", top: "forced_aof_rewrites_s = [0.1]", rewriteInfo: infoRewriteFailed,
			wantErr: "the forced rewrite at 0.1 s failed"},
		{name: "an instance exits before its stop", apis: [3]string{1: exitsEarly},
			wantErr: "hitapi-2 exited before the orchestrator stopped it (exit 0)"},
		{name: "an instance exits 1 after SIGTERM", apis: [3]string{0: exit1OnTERM}, wantErr: "hitapi-1 ended with exit 1 after SIGTERM"},
		{name: "the sampler exits 1 after SIGTERM", sampler: exit1OnTERM, wantErr: "sampler ended with exit 1 after SIGTERM"},
		{name: "the store exits during the run", store: storeDies, wantErr: "the store exited during the run (exit 1"},
		{name: "a signal while hitgen sends", hitgen: hitgenLong, signal: "while sending", wantErr: "the run was stopped"},
		{name: "a signal once hitgen has exited", signal: "after hitgen", wantErr: "stopped by a signal"},
		{name: "a signal while the report builder runs", report: reportLong, signal: "during a report step",
			stepOut: "report.stderr", stepErr: "the report: python3 -m report (see report.stderr): python3: signal: killed", wantErr: "stopped by a signal"},
		{name: "a signal while validate.py runs", block: "validate", signal: "during a report step",
			stepOut: "validate.stderr", stepErr: "the report: report/validate.py does not pass the report (see validate.stderr): python3: signal: killed",
			wantErr: "stopped by a signal"},
		{name: "a signal while the thresholds freeze runs", block: "thresholds", baseline: true, signal: "during a report step",
			stepOut: "thresholds.stderr", stepErr: "freezing the thresholds: python3 -m report.thresholds (see thresholds.stderr): python3: signal: killed",
			wantErr: "stopped by a signal"},
		{name: "a signal while the store shuts down, before the thresholds freeze", baseline: true, signal: "while the store shuts down",
			wantErr: "stopped by a signal"},
		{name: "the run index cannot be appended", indexDir: true, wantErr: "the run index: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, logs := testRunner(t, scenario("test", c.top, c.tables), flags{baseline: c.baseline})
			indexPath := filepath.Join(r.s.root, "runs", "index.jsonl")
			if c.indexDir {
				if err := os.Mkdir(indexPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// The site holds runs/thresholds/pf-1.json (a limiter run's preflight needs it, and the
			// run has copied it): a baseline here has none, so its freeze runs, and a run signalled
			// during a report step has none either, so the run must not leave one.
			frozen := filepath.Join(r.s.root, "runs", "thresholds", "pf-1.json")
			if c.baseline || c.signal == "during a report step" {
				remove(t, frozen)
			}
			script := cmp.Or(c.report, pythonOK)
			calls := filepath.Join(t.TempDir(), "calls")
			if c.block != "" || c.signal == "while the store shuts down" { // the latter's stand-in blocks in no step
				python, repo := realPython(t)
				report, markdown := filepath.Join(t.TempDir(), "report.json"), filepath.Join(t.TempDir(), "report.md")
				writeFile(t, report, stepsReport, 0o600)
				writeFile(t, markdown, stepsMarkdown, 0o600)
				script = signalStepScript(calls, report, markdown, python, repo, c.block)
			}
			standInPython(t, script)
			if c.faultStatus != 0 {
				r.apis[0].addr = newFaultAPIWith(t, c.faultStatus, 0).addr
			}
			if c.rewriteInfo != "" {
				store := fakeresp.Start(t, "rl_ops", r.rd.pw.ops)
				store.ReplyValue("BGREWRITEAOF", resp.Value{Kind: resp.Simple, Str: rewriteStartedReply}, 1)
				store.ReplyValue("INFO", resp.Value{Kind: resp.Bulk, Str: c.rewriteInfo}, 1)
				r.storeAddr = store.Addr()
			}
			for i := range r.apis {
				r.apis[i].grace = 5 * time.Second
				if c.shortGrace && i == 2 {
					r.apis[i].grace = 300 * time.Millisecond
				}
				r.apiProcs = append(r.apiProcs, startStandIn(t, r.runPath, fmt.Sprintf("hitapi-%d", i+1), runlog.RoleHitAPI, i+1,
					cmp.Or(c.apis[i], exit0OnTERM), nil))
			}
			r.sampler = startStandIn(t, r.runPath, "sampler", runlog.RoleSampler, 0, cmp.Or(c.sampler, exit0OnTERM), nil)
			wantOrder := []string{"hitgen", "hitapi-1", "hitapi-2", "hitapi-3", "sampler"}
			if c.store != "" {
				r.store = startStandIn(t, r.runPath, "valkey-server", runlog.RoleValkey, 0, c.store, nil)
				wantOrder = append(wantOrder, "valkey-server")
			}
			// The stub store a signal while the store shuts down needs: a stand-in that runs until
			// the signal, and the fake RESP server the run sends SHUTDOWN to as rl_ops, which never
			// answers it (the connection closes once the signal has come, below).
			var stub *fakeresp.Server
			if c.signal == "while the store shuts down" {
				stub = fakeresp.Start(t, "rl_ops", r.rd.pw.ops)
				stub.Delay("SHUTDOWN", time.Minute)
				r.storeAddr = stub.Addr()
				r.store = startStandIn(t, r.runPath, "valkey-server", runlog.RoleValkey, 0, exit0OnTERM, nil)
				wantOrder = append(wantOrder, "valkey-server")
			}
			r.countRows = func(context.Context) (int64, error) { return 7, nil }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			t0 := newT0Watch()
			startHitgenStandIn(t, r, t0, cmp.Or(c.hitgen, hitgenOK))
			var code int
			switch c.signal {
			case "while sending":
				stop := time.AfterFunc(300*time.Millisecond, cancel)
				defer stop.Stop()
				code = r.afterHitgen(ctx, t0)
			case "after hitgen":
				r.drive(ctx, t0) // returns once hitgen has exited
				cancel()
				code = r.finishRun(ctx)
			case "during a report step":
				// The signal comes once the step's stand-in printed ready, or after 10 s at most.
				signalled := make(chan time.Time, 1)
				go func() {
					for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
						if data, _ := os.ReadFile(filepath.Join(r.runPath, c.stepOut)); bytes.Contains(data, []byte("ready")) {
							break
						}
					}
					signalled <- time.Now()
					cancel()
				}()
				code = r.afterHitgen(ctx, t0)
				returned, at := time.Now(), <-signalled
				if took := returned.Sub(at); took > 5*time.Second {
					t.Errorf("finishRun returned %v after the signal during %s, want within 5 s: the report steps run on the caller's ctx", took, c.stepOut)
				}
				rc := readContext(t, r.runPath)
				if !strings.Contains(rc.Error, c.stepErr) {
					t.Errorf("context.json error %q, want the step stopped by the signal, %q", rc.Error, c.stepErr)
				}
				if _, err := os.Lstat(frozen); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("a frozen thresholds file after the signal during %s (%v), want none", c.stepOut, err)
				}
				// The step the signal stopped marks the report invalid, after the signal.
				var wantCalls []string
				switch c.block {
				case "validate":
					wantCalls = []string{"-m report " + r.runPath, "report/validate.py " + r.runPath, "-m report --mark-invalid " + r.runPath}
					checkMarked(t, r.runPath, "validate", "validate.stderr")
				case "thresholds":
					wantCalls = []string{"-m report " + r.runPath, "report/validate.py " + r.runPath + " --mode baseline",
						"-m report.thresholds " + r.runPath + " --out " + frozen, "-m report --mark-invalid --freeze " + r.runPath}
					checkMarked(t, r.runPath, "freeze", "thresholds.stderr")
				}
				if c.block != "" {
					if data, err := os.ReadFile(calls); err != nil || !slices.Equal(strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), wantCalls) {
						t.Errorf("python3 calls %q (%v), want %q", data, err, wantCalls)
					}
				}
			case "while the store shuts down":
				// The signal comes once the stub store got SHUTDOWN (validate.py has passed, the
				// freeze is next), or after 10 s at most; then the store exits and the connection
				// closes with no reply, as a store that shuts down closes it.
				pid := r.store.proc.PID
				shut := make(chan struct{})
				go func() {
					defer close(shut)
					for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
						if stub.Count("SHUTDOWN") > 0 {
							break
						}
					}
					cancel()
					_ = syscall.Kill(-pid, syscall.SIGTERM)
					stub.DropConns()
				}()
				code = r.afterHitgen(ctx, t0)
				<-shut
				rc := readContext(t, r.runPath)
				if want := "freezing the thresholds: python3 -m report.thresholds did not run: context canceled"; !strings.Contains(rc.Error, want) {
					t.Errorf("context.json error %q, want the freeze the signal kept from running, %q", rc.Error, want)
				}
				if !slices.ContainsFunc(rc.Children, func(rec childRecord) bool {
					return rec.Name == "valkey-server" && rec.Exit == "exit 0" && !rec.Early && !rec.Killed && rec.Error == ""
				}) {
					t.Errorf("context.json children %+v, want the store's record: shut down, exit 0, not killed", rc.Children)
				}
				if _, err := os.Lstat(frozen); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("a frozen thresholds file after the signal (%v), want none", err)
				}
				// The freeze the signal kept from running marks the report invalid, after it.
				wantCalls := []string{"-m report " + r.runPath, "report/validate.py " + r.runPath + " --mode baseline",
					"-m report --mark-invalid --freeze " + r.runPath}
				if data, err := os.ReadFile(calls); err != nil || !slices.Equal(strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), wantCalls) {
					t.Errorf("python3 calls %q (%v), want %q", data, err, wantCalls)
				}
				checkMarked(t, r.runPath, "freeze", "")
			default:
				code = r.afterHitgen(ctx, t0)
			}

			want := exitOK
			if c.wantErr != "" {
				want = exitFailure
			}
			if code != want {
				t.Errorf("exit code %d, want %d\n%s", code, want, logs.String())
			}
			rc := readContext(t, r.runPath)
			if c.wantErr == "" && (rc.Outcome != "ok" || rc.Error != "") {
				t.Errorf("context.json outcome %q, error %q; want ok", rc.Outcome, rc.Error)
			}
			if c.wantErr != "" && (rc.Outcome != "failed" || !strings.Contains(rc.Error, c.wantErr)) {
				t.Errorf("context.json outcome %q, error %q; want failed, naming %q", rc.Outcome, rc.Error, c.wantErr)
			}
			index, err := os.ReadFile(indexPath)
			switch {
			case c.indexDir:
				if info, err := os.Stat(indexPath); err != nil || !info.IsDir() {
					t.Errorf("runs/index.jsonl after the failed append: %v, want the directory left as it was", err)
				}
			case c.wantErr == "" && string(index) != indexLine:
				t.Errorf("runs/index.jsonl holds %q (%v), want the one line %q", index, err, indexLine)
			case c.wantErr != "" && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("runs/index.jsonl after a failed run holds %q (%v), want no file", index, err)
			}
			if c.signal != "while sending" {
				if data, err := os.ReadFile(filepath.Join(r.runPath, "db_rows.json")); err != nil || string(data) != "{\"db_rows\":7}\n" {
					t.Errorf("db_rows.json holds %q (%v), want {\"db_rows\":7} from the row counter", data, err)
				}
			}
			var order []string
			for _, rec := range rc.Children {
				order = append(order, rec.Name)
				if !gone(rec.PID) {
					t.Errorf("%s (pid %d) still runs after the run's end", rec.Name, rec.PID)
				}
			}
			if !slices.Equal(order, wantOrder) {
				t.Errorf("children stopped in the order %q, want %q", order, wantOrder)
			}
			if c.store != "" && !slices.ContainsFunc(rc.Children, func(rec childRecord) bool {
				return rec.Name == "valkey-server" && rec.Early && rec.Exit == "exit 1"
			}) {
				t.Errorf("context.json children %+v, want the store's record: exit 1, exited before its stop", rc.Children)
			}
			if r.hitgen != nil || r.sampler != nil || r.store != nil || len(r.apiProcs) != 0 {
				t.Error("children still held after the run's end")
			}
		})
	}
}

// reportStepsScript is a stand-in python3 for TestRun_ReportSteps: it appends each call's
// arguments, one line, to the file calls; `-m report --mark-invalid <run>` runs the real one,
// `python3 -m report --mark-invalid <run>` (the program python, the repository repo on
// PYTHONPATH); `-m report <run>` copies the files report and markdown to <run>/report.json and
// <run>/report.md and exits build; `-m report.thresholds …` exits freeze; report/validate.py
// prints a FAIL line and its verdict line and exits validate when validate is not 0, else 0.
func reportStepsScript(calls, report, markdown, python, repo string, build, validate, freeze int) string {
	return fmt.Sprintf("#!/bin/sh\n"+
		"echo \"$*\" >> '%s'\n"+
		"case \"$1 $2 $3\" in\n"+
		"'-m report --mark-invalid') PYTHONPATH='%s' exec '%s' \"$@\" ;;\n"+
		"esac\n"+
		"case \"$1 $2\" in\n"+
		"'-m report') cp '%s' \"$3/report.json\"; cp '%s' \"$3/report.md\"; exit %d ;;\n"+
		"'-m report.thresholds') exit %d ;;\n"+
		"esac\n"+
		"if [ %d -ne 0 ]; then echo 'FAIL thresholds_hash: a detail'; echo \"validate: $2: invalid\"; exit %d; fi\n",
		calls, repo, python, report, markdown, build, freeze, validate, validate)
}

// realPython returns the python3 on PATH, before a test puts a stand-in first, and the
// repository root: the report package's home, for a stand-in that runs a real report step.
func realPython(t *testing.T) (python, repo string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3: %v", err)
	}
	if repo, err = filepath.Abs(filepath.Join("..", "..")); err != nil {
		t.Fatal(err)
	}
	return python, repo
}

// The report.json and report.md the stand-in builder writes, and the report.json `python3 -m
// report --mark-invalid` writes from it (report/__main__.py): evidence_validity.valid false and
// evidence_validity.validate naming the check of validate.py's FAIL line and its output file,
// every other member as written and in order (an integer beyond float64's exact range and text
// beyond ASCII included, the latter escaped as the report builder writes JSON), indented by two
// spaces; with --freeze, evidence_validity.freeze naming the freeze's output file instead.
// report.md, rendered again from it, says valid false.
const (
	stepsReport = `{"run_id": "20261008-142233-pf-1", "load": {"planned": 300, "big": 123456789012345678901, "rate": 0.1},
 "evidence_validity": {"checks": {"log_drops": {"ok": true, "detail": "≤ 5 s"}}, "valid": true}, "tail": ["é", null]}
`
	stepsReportInvalid = `{
  "run_id": "20261008-142233-pf-1",
  "load": {
    "planned": 300,
    "big": 123456789012345678901,
    "rate": 0.1
  },
  "evidence_validity": {
    "checks": {
      "log_drops": {
        "ok": true,
        "detail": "\u2264 5 s"
      }
    },
    "valid": false,
    "validate": {
      "passed": false,
      "failed_checks": [
        "thresholds_hash"
      ],
      "output": "validate.stderr"
    }
  },
  "tail": [
    "\u00e9",
    null
  ]
}
`
	stepsReportFreezeInvalid = `{
  "run_id": "20261008-142233-pf-1",
  "load": {
    "planned": 300,
    "big": 123456789012345678901,
    "rate": 0.1
  },
  "evidence_validity": {
    "checks": {
      "log_drops": {
        "ok": true,
        "detail": "\u2264 5 s"
      }
    },
    "valid": false,
    "freeze": {
      "passed": false,
      "output": "thresholds.stderr"
    }
  },
  "tail": [
    "\u00e9",
    null
  ]
}
`
	stepsMarkdown = "# Run report 20261008-142233-pf-1\n\n## Evidence validity (§B8)\n\n| field | value |\n|---|---|\n| valid | true |\n"
)

// TestRun_ReportSteps (IMPLEMENTATION_PLAN H5 step 7; DESIGN §B5, §B9 step 2, §C5): at a run's
// end, with stand-in children and a stand-in python3, the report steps run in order: `python3 -m
// report <run>`, then `python3 report/validate.py <run>`, with --mode baseline for a baseline
// run (limiter_enabled = false). A finished limiter run exits 0 with its report.json and report.md
// untouched and its runs/index.jsonl line. A builder that fails fails the run (exit 1,
// context.json failed, no index line) and nothing is validated. A report validate.py does not
// pass fails the run, with validate's FAIL line in context.json's error, and is marked invalid by
// the real `python3 -m report --mark-invalid <run>`: report.json as stepsReportInvalid, and
// report.md rendered again from it, saying valid false and naming the failed check and
// validate.stderr. A valid baseline then freezes its thresholds, `python3 -m
// report.thresholds <run> --out <site>/runs/thresholds/pf-1.json`, before its index line; a
// baseline whose frozen file exists leaves it untouched and runs no freeze; a baseline whose
// report does not pass runs no freeze; a freeze that fails fails the run, with no index line,
// and marks the report invalid, the real `python3 -m report --mark-invalid --freeze <run>`:
// report.json as stepsReportFreezeInvalid, report.md saying valid false and naming
// thresholds.stderr.
func TestRun_ReportSteps(t *testing.T) {
	for _, c := range []struct {
		name                    string
		baseline                bool
		frozen                  bool // runs/thresholds/pf-1.json exists before the run
		build, validate, freeze int  // the stand-in's exit codes
		wantSteps               []string
		wantErr                 string // in context.json's error; "" for a finished run
		wantMark                string // report.json and report.md marked invalid after this step: "validate" or "freeze"
		wantLog                 string // in the orchestrator's log
	}{
		{name: "a limiter run whose report passes", wantSteps: []string{"report", "validate"}},
		{name: "a limiter run whose builder fails", build: 1, wantSteps: []string{"report"},
			wantErr: "the report: python3 -m report (see report.stderr): python3: exit status 1"},
		{name: "a limiter run whose report does not pass", validate: 1, wantSteps: []string{"report", "validate", "mark invalid"},
			wantErr:  "the report: report/validate.py does not pass the report (see validate.stderr): python3: exit status 1: FAIL thresholds_hash: a detail",
			wantMark: "validate", wantLog: `"msg":"the report is not valid"`},
		{name: "a valid baseline freezes its thresholds", baseline: true, wantSteps: []string{"report", "validate baseline", "freeze"},
			wantLog: `"msg":"thresholds frozen"`},
		{name: "a baseline whose thresholds are frozen already", baseline: true, frozen: true,
			wantSteps: []string{"report", "validate baseline"}, wantLog: `"msg":"thresholds already frozen: never overwritten"`},
		{name: "a baseline whose report does not pass", baseline: true, validate: 1, wantSteps: []string{"report", "validate baseline", "mark invalid"},
			wantErr: "FAIL thresholds_hash: a detail", wantMark: "validate"},
		{name: "a baseline whose freeze fails", baseline: true, freeze: 1, wantSteps: []string{"report", "validate baseline", "freeze", "mark invalid freeze"},
			wantErr:  "freezing the thresholds: python3 -m report.thresholds (see thresholds.stderr): python3: exit status 1",
			wantMark: "freeze", wantLog: `"msg":"the report is not valid"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, logs := testRunner(t, scenario("test", "", ""), flags{baseline: c.baseline})
			dir := t.TempDir()
			calls, report, markdown := filepath.Join(dir, "calls"), filepath.Join(dir, "report.json"), filepath.Join(dir, "report.md")
			writeFile(t, report, stepsReport, 0o600)
			writeFile(t, markdown, stepsMarkdown, 0o600)
			python, repo := realPython(t)
			standInPython(t, reportStepsScript(calls, report, markdown, python, repo, c.build, c.validate, c.freeze))
			// The site holds runs/thresholds/pf-1.json (for a limiter run's preflight): a baseline
			// with no frozen file has it removed first.
			frozen := filepath.Join(r.s.root, "runs", "thresholds", "pf-1.json")
			if c.frozen {
				writeFile(t, frozen, "frozen by an earlier baseline\n", 0o444)
			} else {
				remove(t, frozen)
			}
			for i := range r.apis {
				r.apis[i].grace = 5 * time.Second
				r.apiProcs = append(r.apiProcs, startStandIn(t, r.runPath, fmt.Sprintf("hitapi-%d", i+1), runlog.RoleHitAPI, i+1, exit0OnTERM, nil))
			}
			r.sampler = startStandIn(t, r.runPath, "sampler", runlog.RoleSampler, 0, exit0OnTERM, nil)
			r.countRows = func(context.Context) (int64, error) { return 7, nil }
			t0 := newT0Watch()
			startHitgenStandIn(t, r, t0, hitgenOK)
			code := r.afterHitgen(t.Context(), t0)

			want := exitOK
			if c.wantErr != "" {
				want = exitFailure
			}
			if code != want {
				t.Errorf("exit code %d, want %d\n%s", code, want, logs.String())
			}
			rc := readContext(t, r.runPath)
			if c.wantErr == "" && (rc.Outcome != "ok" || rc.Error != "") {
				t.Errorf("context.json outcome %q, error %q; want ok", rc.Outcome, rc.Error)
			}
			if c.wantErr != "" && (rc.Outcome != "failed" || !strings.Contains(rc.Error, c.wantErr)) {
				t.Errorf("context.json outcome %q, error %q; want failed, naming %q", rc.Outcome, rc.Error, c.wantErr)
			}
			steps := map[string]string{
				"report":              "-m report " + r.runPath,
				"validate":            "report/validate.py " + r.runPath,
				"validate baseline":   "report/validate.py " + r.runPath + " --mode baseline",
				"mark invalid":        "-m report --mark-invalid " + r.runPath,
				"mark invalid freeze": "-m report --mark-invalid --freeze " + r.runPath,
				"freeze":              "-m report.thresholds " + r.runPath + " --out " + frozen,
			}
			var wantCalls []string
			for _, step := range c.wantSteps {
				wantCalls = append(wantCalls, steps[step])
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); !slices.Equal(got, wantCalls) {
				t.Errorf("python3 calls %q, want %q", got, wantCalls)
			}
			wantReport, wantMD := stepsReport, []string(nil)
			switch c.wantMark {
			case "validate":
				wantReport = stepsReportInvalid
				wantMD = []string{"| valid | false |", `| validate.failed_checks | ["thresholds_hash"] |`, "| validate.output | validate.stderr |"}
			case "freeze":
				wantReport = stepsReportFreezeInvalid
				wantMD = []string{"| valid | false |", "| freeze.passed | false |", "| freeze.output | thresholds.stderr |"}
			}
			if got, err := os.ReadFile(filepath.Join(r.runPath, "report.json")); err != nil || string(got) != wantReport {
				t.Errorf("report.json holds (%v):\n%s\nwant:\n%s", err, got, wantReport)
			}
			md, err := os.ReadFile(filepath.Join(r.runPath, "report.md"))
			switch {
			case c.wantMark == "" && string(md) != stepsMarkdown:
				t.Errorf("report.md holds %q (%v), want the builder's, untouched:\n%s", md, err, stepsMarkdown)
			case c.wantMark != "":
				for _, want := range wantMD {
					if err != nil || !strings.Contains(string(md), want) {
						t.Errorf("report.md holds (%v):\n%s\nwant a line %s: marked invalid with report.json", err, md, want)
					}
				}
				if strings.Contains(string(md), "| valid | true |") {
					t.Errorf("report.md still says valid:\n%s", md)
				}
			}
			if c.wantLog != "" && !strings.Contains(logs.String(), c.wantLog) {
				t.Errorf("no %s line in the log:\n%s", c.wantLog, logs.String())
			}
			index, err := os.ReadFile(filepath.Join(r.s.root, "runs", "index.jsonl"))
			wantIndex := fmt.Sprintf(`{"scenario":"pf","baseline":%v,"run_id":"20261008-142233-pf-1"}`+"\n", c.baseline)
			switch {
			case c.wantErr == "" && string(index) != wantIndex:
				t.Errorf("runs/index.jsonl holds %q (%v), want the one line %q", index, err, wantIndex)
			case c.wantErr != "" && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("runs/index.jsonl after a failed run holds %q (%v), want no file", index, err)
			}
			if c.frozen {
				if got, err := os.ReadFile(frozen); err != nil || string(got) != "frozen by an earlier baseline\n" {
					t.Errorf("the frozen thresholds file holds %q (%v) after the run, want it untouched", got, err)
				}
			}
		})
	}
}

// TestRun_FreezeNotRun (IMPLEMENTATION_PLAN H5 step 7): a valid baseline whose ctx ended (a
// signal) before its thresholds freeze could run fails the freeze, with no frozen file, and marks
// its report invalid all the same: the real `python3 -m report --mark-invalid --freeze <run>` is
// the one python3 call, and with no thresholds.stderr the freeze record names no output.
func TestRun_FreezeNotRun(t *testing.T) {
	r, logs := testRunner(t, scenario("test", "", ""), flags{baseline: true})
	frozen := filepath.Join(r.s.root, "runs", "thresholds", "pf-1.json")
	remove(t, frozen)
	dir := t.TempDir()
	calls, report, markdown := filepath.Join(dir, "calls"), filepath.Join(dir, "report.json"), filepath.Join(dir, "report.md")
	writeFile(t, report, stepsReport, 0o600)
	writeFile(t, markdown, stepsMarkdown, 0o600)
	writeFile(t, filepath.Join(r.runPath, "report.json"), stepsReport, 0o600)
	writeFile(t, filepath.Join(r.runPath, "report.md"), stepsMarkdown, 0o600)
	python, repo := realPython(t)
	standInPython(t, reportStepsScript(calls, report, markdown, python, repo, 0, 0, 0))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	want := "python3 -m report.thresholds did not run: context canceled"
	if err := r.freeze(ctx); err == nil || err.Error() != want {
		t.Errorf("freeze after the signal: %v, want %q\n%s", err, want, logs.String())
	}
	if data, err := os.ReadFile(calls); err != nil || string(data) != "-m report --mark-invalid --freeze "+r.runPath+"\n" {
		t.Errorf("python3 calls %q (%v), want only the mark", data, err)
	}
	checkMarked(t, r.runPath, "freeze", "")
	if _, err := os.Lstat(frozen); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a frozen thresholds file after the signal (%v), want none", err)
	}
}

// TestRun_HitgenArgs (IMPLEMENTATION_PLAN H3 steps 3 and 4; DESIGN §B4, §B7): hitgen starts from
// bin/test on the scenario file, with --targets naming the instances in instance order, --out
// the run directory and, in a limiter run whose site has the frozen thresholds file,
// --thresholds <run>/thresholds.json; a baseline run gets --baseline and no --thresholds.
// pids.json, rewritten as hitgen starts, lists it: role hitgen, its PID.
func TestRun_HitgenArgs(t *testing.T) {
	for _, c := range []struct {
		name     string
		baseline bool
	}{{"limiter run", false}, {"baseline run", true}} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := testRunner(t, scenario("test", "", ""), flags{baseline: c.baseline})
			fake := t.TempDir()
			writeFile(t, filepath.Join(fake, "hitgen"), "#!/bin/sh\necho \"ARGS $*\" >&2\n", 0o700)
			r.testBin = fake
			if err := r.startHitgen(t.Context(), newT0Watch()); err != nil {
				t.Fatal(err)
			}
			hg := r.hitgen
			t.Cleanup(func() {
				if !hg.exited() {
					_ = syscall.Kill(-hg.proc.PID, syscall.SIGKILL)
					hg.waitFor(reapWait)
				}
			})
			if !hg.waitFor(10 * time.Second) {
				t.Fatal("the stand-in hitgen did not exit")
			}
			pid := hg.proc.PID
			r.stopChild(&r.hitgen, time.Second)

			// testNow is 2026-10-08T14:22:33Z and the scenario pf, seed 1.
			run := filepath.Join(r.s.root, "runs", "20261008-142233-pf-1")
			targets := fmt.Sprintf("127.0.0.1:%d,127.0.0.1:%d,127.0.0.1:%d", r.s.apiBase, r.s.apiBase+1, r.s.apiBase+2)
			want := []string{"--scenario", r.in.scenarioPath, "--targets", targets, "--out", run}
			if c.baseline {
				want = append(want, "--baseline")
			} else {
				want = append(want, "--thresholds", filepath.Join(run, "thresholds.json"))
			}
			data, err := os.ReadFile(filepath.Join(run, "hitgen.stderr"))
			if err != nil {
				t.Fatal(err)
			}
			line, _, _ := strings.Cut(string(data), "\n")
			args, ok := strings.CutPrefix(line, "ARGS ")
			if !ok || !slices.Equal(strings.Fields(args), want) {
				t.Errorf("hitgen's arguments %q, want %q", line, want)
			}
			pids, err := runlog.ReadPIDs(filepath.Join(run, "pids.json"))
			if err != nil || !slices.Contains(pids.Procs, runlog.Proc{Role: "hitgen", PID: pid}) {
				t.Errorf("pids.json %+v (%v), want hitgen with pid %d", pids, err, pid)
			}
		})
	}
}

// TestRun_ContextJSON (IMPLEMENTATION_PLAN H3 step 5; DESIGN §B5 "Two run kinds", §B8; HARNESS
// PRF-5): context.json of a test run, written once the store has started (a real valkey-server
// from the durable profile) and the epoch has started (a stand-in rladmin), holds run_kind test
// with the label "test-build scenario, not a benchmark"; the hardware (a model, this machine's
// CPU count, its memory); the OS; the versions (this Go, valkey-server's and postgres's first
// lines); the site's .data/conformance.json object; the SHA-256 of the frozen thresholds file the
// run copied; the epoch ID rladmin printed; and the store settings CONFIG GET read. A benchmark
// run is labelled "benchmark".
func TestRun_ContextJSON(t *testing.T) {
	const conformance = `{"eligible":true,"fsync":{"durable_script_p50_us":6460.5,"f_fullfsync_floor_p50_us":5667.9},"passed":true}`
	r, logs := testRunner(t, scenario("test", "", ""), flags{})
	writeFile(t, r.s.path(".data/conformance.json"), conformance+"\n", 0o600)
	frozen, err := os.ReadFile(r.s.path("runs/thresholds/pf-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(frozen)
	t.Cleanup(func() { stopTestStore(t, r) })
	if code, ok := r.storeStep(t.Context()); !ok {
		t.Fatalf("the store step ended the run with exit %d\n%s", code, logs.String())
	}
	fake := t.TempDir()
	writeFile(t, filepath.Join(fake, "rladmin"), "#!/bin/sh\necho '{\"epoch_id\":\"5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f\",\"t_us\":1}'\n", 0o700)
	r.binDir = fake
	if err := r.startEpoch(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.gatherHost(t.Context())
	if err := r.stopStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.writeContext(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(r.runPath, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rc struct {
		RunKind  string `json:"run_kind"`
		RunLabel string `json:"run_label"`
		Hardware *struct {
			Model        string `json:"model"`
			NCPU         int    `json:"ncpu"`
			MemsizeBytes int64  `json:"memsize_bytes"`
		} `json:"hardware"`
		OS       map[string]string `json:"os"`
		Versions struct {
			Go       string `json:"go"`
			Valkey   string `json:"valkey"`
			Postgres string `json:"postgres"`
		} `json:"versions"`
		Conformance      json.RawMessage   `json:"conformance"`
		ThresholdsSHA256 *string           `json:"thresholds_sha256"`
		EpochID          string            `json:"epoch_id"`
		StoreSettings    map[string]string `json:"store_settings"`
		ReadErrors       map[string]string `json:"read_errors"`
	}
	if err := json.Unmarshal(data, &rc); err != nil {
		t.Fatalf("context.json: %v\n%s", err, data)
	}
	if rc.RunKind != "test" || rc.RunLabel != "test-build scenario, not a benchmark" {
		t.Errorf("run_kind %q, run_label %q; want test, \"test-build scenario, not a benchmark\"", rc.RunKind, rc.RunLabel)
	}
	if hw := rc.Hardware; hw == nil || hw.Model == "" || hw.NCPU != runtime.NumCPU() || hw.MemsizeBytes <= 0 {
		t.Errorf("hardware %+v (read errors %v), want a model, ncpu %d and the memory size", hw, rc.ReadErrors, runtime.NumCPU())
	}
	if rc.OS["ProductName"] == "" || rc.OS["ProductVersion"] == "" {
		t.Errorf("os %v (read errors %v), want sw_vers's ProductName and ProductVersion", rc.OS, rc.ReadErrors)
	}
	if v := rc.Versions; v.Go != runtime.Version() || !strings.HasPrefix(v.Valkey, "Valkey server v=") ||
		!strings.HasPrefix(v.Postgres, "postgres (PostgreSQL) ") {
		t.Errorf("versions %+v (read errors %v), want go %s and valkey-server's and postgres's --version lines", v, rc.ReadErrors, runtime.Version())
	}
	var got, wantConf any
	if err := json.Unmarshal(rc.Conformance, &got); err != nil {
		t.Errorf("conformance %s: %v", rc.Conformance, err)
	}
	if err := json.Unmarshal([]byte(conformance), &wantConf); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantConf) {
		t.Errorf("conformance %s, want the site's .data/conformance.json %s", rc.Conformance, conformance)
	}
	if want := hex.EncodeToString(sum[:]); rc.ThresholdsSHA256 == nil || *rc.ThresholdsSHA256 != want {
		t.Errorf("thresholds_sha256 %v, want %s (the frozen file's SHA-256)", rc.ThresholdsSHA256, want)
	}
	if rc.EpochID != "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f" {
		t.Errorf("epoch_id %q, want the one rladmin printed", rc.EpochID)
	}
	for k, want := range map[string]string{
		"maxclients": "4096", "appendonly": "yes", "appendfsync": "always", "maxmemory-policy": "noeviction",
		"port": strconv.Itoa(r.s.storePort), "aclfile": filepath.Join(r.s.root, "runs", "20261008-142233-pf-1", "store", "acl.conf"),
	} {
		if got := rc.StoreSettings[k]; got != want {
			t.Errorf("store_settings %s = %q, want %q", k, got, want)
		}
	}

	bench := newRunContext(inputs{scenario: config.Scenario{Name: "pf", Seed: 1, RunKind: config.RunBench}}, runDir{id: "20261008-142233-pf-1"})
	if bench.RunKind != "bench" || bench.RunLabel != "benchmark" {
		t.Errorf("a benchmark run's run_kind %q, run_label %q; want bench, benchmark", bench.RunKind, bench.RunLabel)
	}
}

// TestRun_HostProgramsAbsolute (IMPLEMENTATION_PLAN H3 step 5): the hardware and the OS are
// read with /usr/sbin/sysctl and /usr/bin/sw_vers by absolute path, so an orchestrator whose
// PATH lacks them (cron and launchd leave /usr/sbin out) still records both.
func TestRun_HostProgramsAbsolute(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	r, _ := bareRunner(t, "")
	r.envs = &childEnvs{plain: []string{"PATH=" + empty}}
	if hw, err := r.hardware(t.Context()); err != nil || hw.NCPU != int64(runtime.NumCPU()) || hw.Model == "" {
		t.Errorf("hardware with PATH=%s: %+v, %v; want a model and ncpu %d", empty, hw, err, runtime.NumCPU())
	}
	if m, err := r.osInfo(t.Context()); err != nil || m["ProductName"] == "" {
		t.Errorf("os with PATH=%s: %v, %v; want sw_vers's ProductName and ProductVersion", empty, m, err)
	}
}

// TestRun_BenchContextPRF5 (IMPLEMENTATION_PLAN H3 step 5; DESIGN §B8; HARNESS PRF-5): a
// benchmark run must record every context.json item of step 5. A finished benchmark run that
// recorded them all (gatherHost's real reads, the site's S1 conformance result, the store and
// PostgreSQL settings, a schedule.jsonl) exits 0 with its run index line. One that lacks any
// one of them (each case clears one, as a failed read leaves it; the conformance case removes
// .data/conformance.json before gatherHost reads it) fails: exit 1, context.json's outcome
// failed naming the item, no index line. So does one whose conformance result lacks the
// F_FULLFSYNC floor or the durable single-client script p50 (S1's fsync.f_fullfsync_floor_p50_us
// and fsync.durable_script_p50_us, each a finite number > 0): an fsync of null, as S1 writes
// when its test recorded none, or either measurement absent, null, not a number, zero or beyond
// float64, naming the measurement. The thresholds hash is a limiter run's only: a baseline run
// without it finishes. A test run without the conformance result, or with an fsync of null,
// finishes: exit 0, outcome ok, the failed read in read_errors and logged as a WARN line; the
// file with an fsync of null is still recorded in context.json.
func TestRun_BenchContextPRF5(t *testing.T) {
	bins := buildBins(t)
	const (
		floor  = "conformance.fsync.f_fullfsync_floor_p50_us"
		script = "conformance.fsync.durable_script_p50_us"
	)
	for _, c := range []struct {
		name        string
		kind        string
		baseline    bool
		conformance string          // .data/conformance.json for gatherHost to read; "" keeps the site's conformanceJSON
		clear       func(r *runner) // before gatherHost: its reads see it; after: what a failed read leaves
		after       func(r *runner)
		schedule    bool // write schedule.jsonl
		missing     string
	}{
		{name: "a benchmark run with every item", kind: config.RunBench, schedule: true},
		{name: "a benchmark baseline run, which copies no thresholds", kind: config.RunBench, baseline: true, schedule: true},
		{name: "no hardware", kind: config.RunBench, schedule: true, after: func(r *runner) { r.rec.Hardware = nil }, missing: "hardware"},
		{name: "no OS", kind: config.RunBench, schedule: true, after: func(r *runner) { r.rec.OS = nil }, missing: "os"},
		{name: "no valkey version", kind: config.RunBench, schedule: true, after: func(r *runner) { r.rec.Versions.Valkey = "" },
			missing: "versions.valkey"},
		{name: "no postgres version", kind: config.RunBench, schedule: true, after: func(r *runner) { r.rec.Versions.Postgres = "" },
			missing: "versions.postgres"},
		{name: "no store settings", kind: config.RunBench, schedule: true, after: func(r *runner) { r.rec.StoreSettings = nil },
			missing: "store_settings"},
		{name: "no PostgreSQL settings", kind: config.RunBench, schedule: true, after: func(r *runner) { r.rec.PostgresSettings = nil },
			missing: "postgres_settings"},
		{name: "no conformance result", kind: config.RunBench, schedule: true,
			clear: func(r *runner) { remove(t, r.s.path(conformanceFile)) }, missing: "conformance"},
		{name: "a conformance result whose fsync is null", kind: config.RunBench, schedule: true,
			conformance: conformanceWith("null"), missing: floor + ", " + script},
		{name: "no F_FULLFSYNC floor", kind: config.RunBench, schedule: true,
			conformance: conformanceWith(`{"plain_fsync_p50_us":21.4,"durable_script_p50_us":6460.5}`), missing: floor},
		{name: "a null F_FULLFSYNC floor", kind: config.RunBench, schedule: true,
			conformance: conformanceWith(`{"f_fullfsync_floor_p50_us":null,"plain_fsync_p50_us":21.4,"durable_script_p50_us":6460.5}`),
			missing:     floor},
		{name: "a non-numeric F_FULLFSYNC floor", kind: config.RunBench, schedule: true,
			conformance: conformanceWith(`{"f_fullfsync_floor_p50_us":"5667.9","plain_fsync_p50_us":21.4,"durable_script_p50_us":6460.5}`),
			missing:     floor},
		{name: "a zero F_FULLFSYNC floor", kind: config.RunBench, schedule: true,
			conformance: conformanceWith(`{"f_fullfsync_floor_p50_us":0,"plain_fsync_p50_us":21.4,"durable_script_p50_us":6460.5}`),
			missing:     floor},
		{name: "no durable script p50", kind: config.RunBench, schedule: true,
			conformance: conformanceWith(`{"f_fullfsync_floor_p50_us":5667.9,"plain_fsync_p50_us":21.4}`), missing: script},
		{name: "a null durable script p50", kind: config.RunBench, schedule: true,
			conformance: conformanceWith(`{"f_fullfsync_floor_p50_us":5667.9,"plain_fsync_p50_us":21.4,"durable_script_p50_us":null}`),
			missing:     script},
		{name: "a non-numeric durable script p50", kind: config.RunBench, schedule: true,
			conformance: conformanceWith(`{"f_fullfsync_floor_p50_us":5667.9,"plain_fsync_p50_us":21.4,"durable_script_p50_us":true}`),
			missing:     script},
		{name: "a durable script p50 beyond float64", kind: config.RunBench, schedule: true,
			conformance: conformanceWith(`{"f_fullfsync_floor_p50_us":5667.9,"plain_fsync_p50_us":21.4,"durable_script_p50_us":1e999}`),
			missing:     script},
		{name: "no merged settings", kind: config.RunBench, schedule: true, after: func(r *runner) { r.rec.Settings = nil },
			missing: "settings"},
		{name: "no schedule hash", kind: config.RunBench, missing: "schedule_sha256"},
		{name: "a limiter run without its thresholds hash", kind: config.RunBench, schedule: true,
			after: func(r *runner) { r.rec.ThresholdsSHA256 = nil }, missing: "thresholds_sha256"},
		{name: "a test run without the conformance result", kind: config.RunTest, schedule: true,
			clear: func(r *runner) { remove(t, r.s.path(conformanceFile)) }},
		{name: "a test run whose conformance result's fsync is null", kind: config.RunTest, schedule: true,
			conformance: conformanceWith("null")},
	} {
		t.Run(c.name, func(t *testing.T) {
			standInPython(t, pythonOK) // the report steps pass: the outcome is the PRF-5 check's
			r, logs := testRunnerIn(t, runTestSiteWith(t, bins), scenario(c.kind, "", ""), flags{baseline: c.baseline})
			if c.conformance != "" {
				writeFile(t, r.s.path(conformanceFile), c.conformance, 0o600)
			}
			if c.clear != nil {
				c.clear(r)
			}
			r.gatherHost(t.Context())
			r.rec.StoreSettings = map[string]string{"maxclients": "4096"}
			r.rec.PostgresSettings = map[string]string{"fsync": "on"}
			if c.after != nil {
				c.after(r)
			}
			if c.schedule {
				writeFile(t, filepath.Join(r.runPath, "schedule.jsonl"), `{"planned_ns":0}`+"\n", 0o600)
			}
			r.countRows = func(context.Context) (int64, error) { return 0, nil }
			t0 := newT0Watch()
			startHitgenStandIn(t, r, t0, hitgenOK)
			code := r.afterHitgen(t.Context(), t0)

			rc := readContext(t, r.runPath)
			index, indexErr := os.ReadFile(filepath.Join(r.s.root, "runs", "index.jsonl"))
			if c.missing != "" {
				if code != exitFailure || rc.Outcome != "failed" || !strings.Contains(rc.Error, c.missing) {
					t.Errorf("exit code %d, outcome %q, error %q; want %d, failed, naming %s\n%s", code, rc.Outcome, rc.Error,
						exitFailure, c.missing, logs.String())
				}
				if !errors.Is(indexErr, fs.ErrNotExist) {
					t.Errorf("runs/index.jsonl after the failed run holds %q (%v), want no file", index, indexErr)
				}
				return
			}
			if code != exitOK || rc.Outcome != "ok" || rc.Error != "" {
				t.Errorf("exit code %d, outcome %q, error %q; want %d, ok\n%s", code, rc.Outcome, rc.Error, exitOK, logs.String())
			}
			if want := fmt.Sprintf(`{"scenario":"pf","baseline":%v,"run_id":"20261008-142233-pf-1"}`+"\n", c.baseline); string(index) != want {
				t.Errorf("runs/index.jsonl holds %q (%v), want %q", index, indexErr, want)
			}
			if c.kind == config.RunTest {
				if rc.ReadErrors["conformance"] == "" {
					t.Errorf("context.json read_errors %v, want the conformance read's error", rc.ReadErrors)
				}
				if !strings.Contains(logs.String(), `"level":"WARN","msg":"context.json: a read failed","what":"conformance"`) {
					t.Errorf("no WARN line for the failed conformance read in the log:\n%s", logs.String())
				}
				if c.conformance != "" { // a file without the measurements is still recorded
					var want bytes.Buffer
					if err := json.Compact(&want, []byte(c.conformance)); err != nil {
						t.Fatal(err)
					}
					var got bytes.Buffer
					if err := json.Compact(&got, rc.Conformance); err != nil || got.String() != want.String() {
						t.Errorf("context.json's conformance %s (%v), want the site's file %s", rc.Conformance, err, want.String())
					}
				}
			}
		})
	}
}
