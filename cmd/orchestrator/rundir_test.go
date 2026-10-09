//go:build rltest

package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/limiter"
)

// preparedRun runs preflight on scenario text in a fresh test site (newTestSite, shell-script
// binaries: test runs only) and fails the test on any refusal.
func preparedRun(t *testing.T, text string, baseline bool) (site, inputs) {
	t.Helper()
	s := newTestSite(t, testBins{})
	path := filepath.Join(t.TempDir(), "scenario.toml")
	writeFile(t, path, text, 0o600)
	in, refs := preflight(t.Context(), s, flags{scenario: path, baseline: baseline})
	if len(refs) > 0 {
		t.Fatalf("preflight refused the run: %+v", refs)
	}
	return s, in
}

// aclLine is one user line of a rendered ACL file: `user <name> on ><password> …`.
var aclLine = regexp.MustCompile(`^user (\S+) on >(\S+) `)

// hex64 is a generated password: 32 bytes, hex-encoded.
var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// aclPasswords checks the rendered ACL file of a run directory (mode 0600, first line
// `user default off`) and returns its users, in file order, and their passwords.
func aclPasswords(t *testing.T, rd runDir) (users []string, pws map[string]string) {
	t.Helper()
	path := filepath.Join(rd.path, storeDir, aclFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s has mode %o, want 600", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if lines[0] != "user default off" {
		t.Errorf("first line %q, want %q", lines[0], "user default off")
	}
	pws = map[string]string{}
	for i, line := range lines[1:] {
		m := aclLine.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("line %d does not define a user with a password", i+2)
			continue
		}
		users = append(users, m[1])
		pws[m[1]] = m[2]
	}
	return users, pws
}

// assertNoPasswords fails the test when any of the run directory's files names holds one of
// pws (AGENTS.md §1 Secrets): config.toml, context.json and the children's saved output.
func assertNoPasswords(t *testing.T, rd runDir, pws map[string]string, files ...string) {
	t.Helper()
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(rd.path, name))
		if err != nil {
			t.Fatal(err)
		}
		for user, pw := range pws {
			if bytes.Contains(data, []byte(pw)) {
				t.Errorf("%s holds %s's password", name, user)
			}
		}
	}
}

// redacted is env as a failing assertion prints it: each of pws's passwords replaced by
// "<user's password>", so no generated password reaches the test's output (AGENTS.md §1
// Secrets) and a diff still shows whose password went where.
func redacted(env []string, pws map[string]string) []string {
	shown := make([]string, len(env))
	for i, kv := range env {
		for user, pw := range pws {
			kv = strings.ReplaceAll(kv, pw, "<"+user+"'s password>")
		}
		shown[i] = kv
	}
	return shown
}

// TestRunDir_ACLRender (plan H3 step 2; DESIGN §A2): the run's store/acl.conf has mode 0600
// and starts with `user default off`; a test run has 4 users, a bench run 3, without
// rl_fixture; every password is 64 hex characters and distinct from the others, none is in
// config.toml, and a second run draws fresh ones, sharing none with the first. childEnv hands
// a child only the passwords of the users it names, without the orchestrator's own RL_
// variables, and a password a run lacks is an error; envs gives each child role its
// passwords; formatting or logging the passwords or the environments shows none; and none is
// in context.json or in any child's saved output, rladmin's included (runFiles: the run's own
// arguments and environments, stand-in programs that print their arguments and the names of
// their RL_ variables, which must be exactly the child's own, pg_ctl's among them).
func TestRunDir_ACLRender(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("..", "..", "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := runConfig(base, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind  string
		users []string
	}{
		{config.RunTest, []string{"rl_lib", "rl_admin", "rl_fixture", "rl_ops"}},
		{config.RunBench, []string{"rl_lib", "rl_admin", "rl_ops"}},
	} {
		t.Run(c.kind, func(t *testing.T) {
			s := newTestSite(t, testBins{})
			in := inputs{scenario: config.Scenario{Name: "acl", Seed: 3, RunKind: c.kind}, config: cfg}
			rd, err := createRunDir(s, in, testNow())
			if err != nil {
				t.Fatal(err)
			}
			users, pws := aclPasswords(t, rd)
			if !slices.Equal(users, c.users) {
				t.Fatalf("users %q, want %q", users, c.users)
			}
			seen := map[string]bool{}
			for user, pw := range pws {
				if !hex64.MatchString(pw) {
					t.Errorf("%s's password is not 64 hex characters", user)
				}
				if seen[pw] {
					t.Errorf("%s's password repeats another user's", user)
				}
				seen[pw] = true
			}
			assertNoPasswords(t, rd, pws, configFile)
			rd2, err := createRunDir(newTestSite(t, testBins{}), in, testNow())
			if err != nil {
				t.Fatal(err)
			}
			_, pws2 := aclPasswords(t, rd2)
			for user, pw := range pws2 {
				if seen[pw] {
					t.Errorf("the second run's %s password is one the first run drew", user)
				}
			}
			info, err := os.Stat(filepath.Join(rd.path, storeDir))
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Errorf("store directory mode %o, want 700", perm)
			}

			environ := []string{"PATH=/usr/bin:/bin", "RL_LIB_PASSWORD=stale", "RL_ADMIN_USER=someone", "HOME=/home/x"}
			env, err := rd.pw.childEnv(environ, "127.0.0.1:6390", userAdmin)
			if err != nil {
				t.Fatal(err)
			}
			wantEnv := []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "RL_ADMIN_PASSWORD=" + pws["rl_admin"], "RL_STORE_ADDR=127.0.0.1:6390"}
			if !slices.Equal(env, wantEnv) {
				t.Errorf("rladmin's environment %q, want %q", redacted(env, pws), redacted(wantEnv, pws))
			}
			env, err = rd.pw.childEnv(environ, "", userLib)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "RL_LIB_PASSWORD=" + pws["rl_lib"]}; !slices.Equal(env, want) {
				t.Errorf("hitapi's environment %q, want %q", redacted(env, pws), redacted(want, pws))
			}
			env, err = rd.pw.childEnv(environ, "", userAdmin, userOps)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "RL_ADMIN_PASSWORD=" + pws["rl_admin"],
				"RL_OPS_PASSWORD=" + pws["rl_ops"]}; !slices.Equal(env, want) {
				t.Errorf("the environment with rl_admin and rl_ops %q, want %q", redacted(env, pws), redacted(want, pws))
			}
			env, err = rd.pw.childEnv(environ, "", userFixture)
			if c.kind == config.RunBench {
				if err == nil {
					t.Errorf("childEnv with rl_fixture in a bench run = %q, want an error", redacted(env, pws))
				}
			} else if want := []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "RL_FIXTURE_PASSWORD=" + pws["rl_fixture"]}; err != nil || !slices.Equal(env, want) {
				t.Errorf("childEnv with rl_fixture = %q, %v; want %q", redacted(env, pws), err, redacted(want, pws))
			}
			if _, err := rd.pw.childEnv(environ, "", "default"); err == nil {
				t.Error("childEnv with the default user: no error")
			}
			envs, err := rd.pw.envs(environ, "127.0.0.1:6390")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range []struct {
				role      string
				got, want []string
			}{
				{"plain", envs.plain, []string{"PATH=/usr/bin:/bin", "HOME=/home/x"}},
				{"hitapi", envs.hitapi, []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "RL_LIB_PASSWORD=" + pws["rl_lib"]}},
				{"rladmin", envs.rladmin, []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "RL_ADMIN_PASSWORD=" + pws["rl_admin"],
					"RL_STORE_ADDR=127.0.0.1:6390"}},
				{"sampler", envs.sampler, []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "RL_ADMIN_PASSWORD=" + pws["rl_admin"],
					"RL_OPS_PASSWORD=" + pws["rl_ops"], "RL_STORE_ADDR=127.0.0.1:6390"}},
			} {
				if !slices.Equal(e.got, e.want) {
					t.Errorf("the %s environment %q, want %q", e.role, redacted(e.got, pws), redacted(e.want, pws))
				}
			}
			// What redacted shows is checked without printing it: a broken redaction would show the
			// passwords it should hide.
			if want := []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "RL_ADMIN_PASSWORD=<rl_admin's password>",
				"RL_OPS_PASSWORD=<rl_ops's password>", "RL_STORE_ADDR=127.0.0.1:6390"}; !slices.Equal(redacted(envs.sampler, pws), want) {
				t.Errorf("redacted(the sampler's environment) is not %q: diagnostics must name each password's user, never show it", want)
			}

			var shown bytes.Buffer
			fmt.Fprintf(&shown, "%v %+v %#v %s %q", rd.pw, rd.pw, rd.pw, rd.pw, rd.pw)
			fmt.Fprintf(&shown, "%v %+v %#v", rd, rd, rd)
			fmt.Fprintf(&shown, "%v %+v %#v %v", envs, envs, envs, *envs)
			slog.New(slog.NewJSONHandler(&shown, nil)).Info("run", "pw", rd.pw, "rd", rd, "envs", envs)
			slog.New(slog.NewTextHandler(&shown, nil)).Info("run", "pw", rd.pw, "rd", rd, "envs", envs)
			for user, pw := range pws {
				if strings.Contains(shown.String(), pw) {
					t.Errorf("formatting or logging the passwords shows %s's", user)
				}
			}

			var logs bytes.Buffer
			r := newRunner(slog.New(slog.NewJSONHandler(&logs, nil)), s, in, rd, envs, newHTTPClient(), time.Now)
			assertNoPasswords(t, rd, pws, runFiles(t, r)...)
			for user, pw := range pws {
				if strings.Contains(logs.String(), pw) {
					t.Errorf("the run's log shows %s's password", user)
				}
			}
		})
	}
}

// TestRunDir_ConfigMerge (plan H3 step 2): the run directory is runs/<YYYYMMDD-HHMMSS in
// UTC>-<name>-<seed>/, and its config.toml is harness.toml with the scenario's [policy],
// [instance] and [api] overrides merged in, plus limiter_enabled = false with --baseline; every
// key the scenario leaves alone keeps harness.toml's value (DESIGN §B2).
func TestRunDir_ConfigMerge(t *testing.T) {
	t.Setenv(config.LibPasswordEnv, "")
	const overrides = "[policy]\nfailure_mode = \"open\"\nclock_source = \"controlled\"\n" +
		"[instance]\nmax_held_receipts = 64\n[api]\nrefuse_after_acquire_bp = 100\n"
	const dsn = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64"
	// The wall clock 07:22:33 at UTC−7 is 14:22:33 UTC.
	now := time.Date(2026, 10, 8, 7, 22, 33, 0, time.FixedZone("UTC-7", -7*3600))
	for _, c := range []struct {
		name      string
		overrides string
		baseline  bool
		edit      func(*config.File)
	}{
		{"no overrides", "", false, func(*config.File) {}},
		{"overrides", overrides, false, func(f *config.File) {
			f.Policy.FailureMode = limiter.Open
			f.Policy.ClockSource = limiter.Controlled
			f.Instance.MaxHeldReceipts = 64
			f.API.RefuseAfterAcquireBP = 100
		}},
		{"overrides with --baseline", overrides, true, func(f *config.File) {
			f.Policy.FailureMode = limiter.Open
			f.Policy.ClockSource = limiter.Controlled
			f.Instance.MaxHeldReceipts = 64
			f.API.RefuseAfterAcquireBP = 100
			f.API.LimiterEnabled = false
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := "name = \"merge\"\nseed = 7\nrun_kind = \"test\"\n" + c.overrides + oneStep
			s, in := preparedRun(t, text, c.baseline)
			rd, err := createRunDir(s, in, now)
			if err != nil {
				t.Fatal(err)
			}
			if rd.id != "20261008-142233-merge-7" || rd.path != filepath.Join(s.root, "runs", "20261008-142233-merge-7") {
				t.Errorf("run %s in %s, want 20261008-142233-merge-7 in runs/ under %s", rd.id, rd.path, s.root)
			}
			got, err := config.Load(filepath.Join(rd.path, configFile))
			if err != nil {
				t.Fatal(err)
			}
			want := config.File{
				Policy: limiter.Policy{
					RateTokens: 10, RatePeriodNs: 1_000_000_000, Capacity: 20, FailureMode: limiter.Closed,
					MaxTrackedBuckets: 100_000, MaxLiveMarkers: 50_000, MaxMarkersPerKey: 1_000,
					ReceiptLifetimeNs: 60_000_000_000, RefundDeadlineNs: 30_000_000_000,
					OutageToleranceNs: 5_000_000_000, CleanupIntervalNs: 5_000_000_000, ClockSource: limiter.Store,
				},
				Instance: limiter.Instance{
					MaxReleaseDelayNs: 5_000_000_000, MarginNs: 1_000_000_000,
					ClockRateBoundPPM: 1000, DependencyDeadlineNs: 2_000_000_000,
					EpochRereadIntervalNs: 1_000_000_000, DrainTimeoutNs: 35_000_000_000,
					MaxHeldReceipts: 1152, MaxPendingRefunds: 1152, ListenerQueueSize: 10_000, RefundWorkers: 128,
					RefundBackoffInitialNs: 50_000_000, RefundBackoffMaxNs: 1_000_000_000,
					StoreConns: 8, RefundConns: 2, CleanupBatch: 1000, CleanupPipelines: 4, ReclaimBatch: 1000,
				},
				Store: limiter.StoreOptions{Addr: "127.0.0.1:6390", User: "rl_lib"},
				API:   config.API{MaxInFlight: 512, LimiterEnabled: true, RefuseAfterAcquireBP: 0, DBDSN: dsn},
			}
			c.edit(&want)
			if got != want {
				t.Errorf("config.toml loads as\n%+v\nwant\n%+v", got, want)
			}
			if in.settings != want {
				t.Errorf("preflight's settings\n%+v\ndiffer from config.toml's\n%+v", in.settings, want)
			}
		})
	}
	t.Run("a taken run ID", func(t *testing.T) {
		s, in := preparedRun(t, "name = \"merge\"\nseed = 7\nrun_kind = \"test\"\n"+oneStep, false)
		first, err := createRunDir(s, in, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := createRunDir(s, in, now); err == nil {
			t.Error("a second run directory with the same run ID: no error")
		}
		for _, name := range []string{configFile, filepath.Join(storeDir, aclFile)} {
			if _, err := os.Stat(filepath.Join(first.path, name)); err != nil {
				t.Errorf("the first run's %s is gone after the refused second createRunDir: %v", name, err)
			}
		}
	})
}

// TestRunDir_ThresholdsCopy (plan H3 step 2; DESIGN §B5 "Thresholds"): a limiter run copies
// its frozen runs/thresholds/<name>-<seed>.json to thresholds.json and records the file's
// SHA-256; a baseline run, and a test run without the file, copy nothing.
func TestRunDir_ThresholdsCopy(t *testing.T) {
	text := scenario("test", "", "")
	t.Run("limiter run", func(t *testing.T) {
		s, in := preparedRun(t, text, false)
		rd, err := createRunDir(s, in, testNow())
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(rd.path, thresholdsFile))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != thresholdsJSON || rd.thresholdsSHA256 != thresholdsSHA256 {
			t.Errorf("thresholds.json %q with SHA-256 %s, want %q with %s", data, rd.thresholdsSHA256, thresholdsJSON, thresholdsSHA256)
		}
	})
	for _, c := range []struct {
		name     string
		baseline bool
		edit     func(t *testing.T, s site)
	}{
		{"baseline run", true, func(*testing.T, site) {}},
		{"test run without frozen thresholds", false, func(t *testing.T, s site) { remove(t, s.path(thresholdsDir+"/pf-1.json")) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestSite(t, testBins{})
			c.edit(t, s)
			path := filepath.Join(t.TempDir(), "scenario.toml")
			writeFile(t, path, text, 0o600)
			in, refs := preflight(t.Context(), s, flags{scenario: path, baseline: c.baseline})
			if len(refs) > 0 {
				t.Fatalf("preflight refused the run: %+v", refs)
			}
			rd, err := createRunDir(s, in, testNow())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(rd.path, thresholdsFile)); !os.IsNotExist(err) || rd.thresholdsSHA256 != "" {
				t.Errorf("thresholds.json stat %v, SHA-256 %q; want no file and no hash", err, rd.thresholdsSHA256)
			}
		})
	}
}
