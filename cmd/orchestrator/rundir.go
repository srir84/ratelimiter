package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
)

// A run directory's files (DESIGN §B1, §B8; IMPLEMENTATION_PLAN H3 step 2).
const (
	runsDir        = "runs"
	configFile     = "config.toml"
	thresholdsFile = "thresholds.json"
	storeDir       = "store"
	aclFile        = "acl.conf"
	aclTemplate    = "deploy/acl.conf.tmpl"

	// runIDLayout is the run ID's time part, YYYYMMDD-HHMMSS in UTC (plan choice).
	runIDLayout = "20060102-150405"
	// maxThresholdsSize bounds the frozen thresholds file a run copies, in bytes (plan choice:
	// one entry per step needs far less).
	maxThresholdsSize = 4 << 20
	// passwordBytes is a generated store password's length before hex encoding (plan H3 step 2).
	passwordBytes = 32
)

// baselineOverride turns the limiter off in a --baseline run's config.toml (HAR-17; plan H3
// step 2).
const baselineOverride = "[api]\nlimiter_enabled = false\n"

// The store's ACL users (DESIGN §A2).
const (
	userLib     = config.LibUser
	userAdmin   = "rl_admin"
	userFixture = "rl_fixture" // test runs only
	userOps     = "rl_ops"
)

const (
	// envStoreAddr is rladmin's store address (cmd/rladmin).
	envStoreAddr = "RL_STORE_ADDR"
	// envPrefix starts every variable the orchestrator sets for its children; childEnv drops
	// the orchestrator's own ones, so no child inherits a stale password or setting.
	envPrefix = "RL_"
)

// runDir is a created run directory (plan H3 step 2).
type runDir struct {
	id               string     // YYYYMMDD-HHMMSS-<name>-<seed>
	path             string     // runs/<id>
	pw               *passwords // the store's passwords, also in store/acl.conf (a pointer: see passwords)
	thresholdsSHA256 string     // hex SHA-256 of the copied thresholds.json; "" when the run copied none
}

// runConfig returns a run's config.toml and its decoded settings (plan H3 step 2):
// config.Merge(harness.toml, the scenario's overrides), then [api] limiter_enabled = false for
// a --baseline run. The result never holds a password: the config format has no field for one.
func runConfig(base, overrides []byte, baseline bool) ([]byte, config.File, error) {
	merged, err := config.Merge(base, overrides)
	if err != nil {
		return nil, config.File{}, fmt.Errorf("merging the scenario's overrides: %w", err)
	}
	if baseline {
		if merged, err = config.Merge(merged, []byte(baselineOverride)); err != nil {
			return nil, config.File{}, fmt.Errorf("turning the limiter off for the baseline: %w", err)
		}
	}
	f, err := config.Parse(merged)
	if err != nil {
		return nil, config.File{}, err
	}
	return merged, f, nil
}

// runID is a run's ID (plan choice): its start time in UTC, YYYYMMDD-HHMMSS, then the
// scenario's name and seed.
func runID(now time.Time, sc config.Scenario) string {
	return now.UTC().Format(runIDLayout) + "-" + sc.Name + "-" + strconv.FormatUint(sc.Seed, 10)
}

// createRunDir creates runs/<id>/ for a run that passed preflight (plan H3 step 2;
// DESIGN §B8): config.toml, thresholds.json when the run copies the frozen file, and
// store/acl.conf rendered with fresh passwords. A run ID already taken is an error. On any
// failure the new directory is removed.
func createRunDir(s site, in inputs, now time.Time) (rd runDir, err error) {
	id := runID(now, in.scenario)
	if err := os.MkdirAll(s.path(runsDir), 0o750); err != nil {
		return runDir{}, fmt.Errorf("creating %s: %w", runsDir, err)
	}
	dir := s.path(runsDir + "/" + id)
	if err := os.Mkdir(dir, 0o750); err != nil {
		return runDir{}, fmt.Errorf("creating the run directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir) // only this call created it; the creation error is reported
		}
	}()
	if err := writeNewFile(filepath.Join(dir, configFile), in.config); err != nil {
		return runDir{}, err
	}
	sum := ""
	if in.thresholds != "" {
		if sum, err = copyThresholds(in.thresholds, filepath.Join(dir, thresholdsFile)); err != nil {
			return runDir{}, err
		}
	}
	if err := os.Mkdir(filepath.Join(dir, storeDir), 0o700); err != nil {
		return runDir{}, fmt.Errorf("creating the store directory: %w", err)
	}
	pw, err := renderACL(s.path(aclTemplate), filepath.Join(dir, storeDir, aclFile), in.scenario.RunKind == config.RunTest)
	if err != nil {
		return runDir{}, err
	}
	return runDir{id: id, path: dir, pw: pw, thresholdsSHA256: sum}, nil
}

// writeNewFile writes data to a file that must not exist yet, mode 0600.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	return nil
}

// copyThresholds copies the frozen thresholds file src to dst and returns the hex SHA-256 of
// its bytes, which context.json records (DESIGN §B5 "Thresholds", §B9 step 3).
func copyThresholds(src, dst string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("copying the frozen thresholds: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxThresholdsSize+1))
	_ = f.Close() // read-only: the bytes read are what counts
	if err != nil {
		return "", fmt.Errorf("copying the frozen thresholds %s: %w", src, err)
	}
	if len(data) > maxThresholdsSize {
		return "", fmt.Errorf("copying the frozen thresholds %s: more than %d bytes", src, maxThresholdsSize)
	}
	if err := writeNewFile(dst, data); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// passwords are a run's store passwords, one per ACL user (DESIGN §A2): drawn fresh for every
// run and kept in this process, in the rendered ACL file (mode 0600) and in the RL_*_PASSWORD
// variables of the children that use them, nowhere else. fixture is "" in a benchmark run,
// whose store has no rl_fixture user. Formatting or logging a passwords value shows none of
// them; a struct holds it through a pointer, which fmt prints as an address, never the fields.
type passwords struct {
	lib, admin, fixture, ops string
}

func (passwords) String() string       { return "passwords{redacted}" }
func (passwords) GoString() string     { return "passwords{redacted}" }
func (passwords) LogValue() slog.Value { return slog.StringValue("redacted") }

// of returns user's password and the variable that carries it to a child (DESIGN §A2); "" for
// a user the run has no password for.
func (pw *passwords) of(user string) (pass, variable string) {
	switch user {
	case userLib:
		return pw.lib, config.LibPasswordEnv
	case userAdmin:
		return pw.admin, "RL_ADMIN_PASSWORD"
	case userFixture:
		return pw.fixture, "RL_FIXTURE_PASSWORD"
	case userOps:
		return pw.ops, "RL_OPS_PASSWORD"
	}
	return "", ""
}

// childEnv returns a child's environment (DESIGN §A2; plan H3 step 2): base, the orchestrator's
// own environment from main, less its RL_ variables; then the RL_*_PASSWORD variable of each of
// users, and only those (hitapi uses rl_lib, rladmin rl_admin, the sampler rl_admin and rl_ops);
// then RL_STORE_ADDR=storeAddr when storeAddr is not "" (rladmin's store address). A user the
// run has no password for, such as rl_fixture in a benchmark run, is an error.
func (pw *passwords) childEnv(base []string, storeAddr string, users ...string) ([]string, error) {
	env := make([]string, 0, len(base)+len(users)+1)
	for _, kv := range base {
		if !strings.HasPrefix(kv, envPrefix) {
			env = append(env, kv)
		}
	}
	for _, u := range users {
		pass, name := pw.of(u)
		if pass == "" {
			return nil, fmt.Errorf("no store password for user %q in this run", u)
		}
		env = append(env, name+"="+pass)
	}
	if storeAddr != "" {
		env = append(env, envStoreAddr+"="+storeAddr)
	}
	return env, nil
}

// childEnvs are the environments of a run's children (DESIGN §A2; plan H3 step 2), each made by
// childEnv with only the passwords that child uses. The orchestrator uses rl_fixture's (the
// controlled clock) and rl_ops's (CONFIG GET, BGREWRITEAOF, SHUTDOWN) itself, in-process.
// Formatting or logging a childEnvs value shows none of it.
type childEnvs struct {
	plain   []string // valkey-server, pg_ctl, hitgen, the report steps (python3): no password
	hitapi  []string // RL_LIB_PASSWORD (harness/config)
	rladmin []string // RL_ADMIN_PASSWORD and RL_STORE_ADDR (cmd/rladmin)
	sampler []string // RL_ADMIN_PASSWORD for its rladmin status, RL_OPS_PASSWORD for INFO, RL_STORE_ADDR
}

func (childEnvs) String() string       { return "childEnvs{redacted}" }
func (childEnvs) GoString() string     { return "childEnvs{redacted}" }
func (childEnvs) LogValue() slog.Value { return slog.StringValue("redacted") }

// envs builds the run's child environments from base, the orchestrator's own environment, and
// storeAddr, the store address of the run's config.toml, which every child must agree on.
func (pw *passwords) envs(base []string, storeAddr string) (*childEnvs, error) {
	var e childEnvs
	var err error
	for _, c := range []struct {
		env   *[]string
		addr  string
		users []string
	}{
		{&e.plain, "", nil},
		{&e.hitapi, "", []string{userLib}},
		{&e.rladmin, storeAddr, []string{userAdmin}},
		{&e.sampler, storeAddr, []string{userAdmin, userOps}},
	} {
		if *c.env, err = pw.childEnv(base, c.addr, c.users...); err != nil {
			return nil, err
		}
	}
	return &e, nil
}

// renderACL renders the ACL template at tmplPath (deploy/acl.conf.tmpl, text/template with the
// fields LibPassword, AdminPassword, FixturePassword and OpsPassword) into a new file at outPath,
// mode 0600, with a fresh password per user: 32 crypto/rand bytes, hex-encoded (DESIGN §A2;
// plan H3 step 2). Without fixture (a benchmark run) the template's rl_fixture line is dropped
// first, so the store has no fixture user and no fixture password exists. The result must start
// with "user default off" and define exactly the run's users. Errors never hold a password.
func renderACL(tmplPath, outPath string, fixture bool) (*passwords, error) {
	src, err := os.ReadFile(tmplPath)
	if err != nil {
		return nil, fmt.Errorf("rendering the ACL file: %w", err)
	}
	users := []string{userLib, userAdmin, userFixture, userOps}
	if !fixture {
		users = []string{userLib, userAdmin, userOps}
		if src, err = dropUser(src, userFixture); err != nil {
			return nil, fmt.Errorf("rendering the ACL file: %s: %w", tmplPath, err)
		}
	}
	var pw passwords
	for _, p := range []*string{&pw.lib, &pw.admin, &pw.ops} {
		if *p, err = newPassword(); err != nil {
			return nil, err
		}
	}
	if fixture {
		if pw.fixture, err = newPassword(); err != nil {
			return nil, err
		}
	}
	tmpl, err := template.New(filepath.Base(tmplPath)).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("rendering the ACL file: parsing %s: %w", tmplPath, err)
	}
	var out bytes.Buffer
	data := struct{ LibPassword, AdminPassword, FixturePassword, OpsPassword string }{pw.lib, pw.admin, pw.fixture, pw.ops}
	if err := tmpl.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("rendering the ACL file: executing %s: %w", tmplPath, err)
	}
	if err := checkACL(out.String(), users); err != nil {
		return nil, fmt.Errorf("rendering the ACL file from %s: %w", tmplPath, err)
	}
	if err := writeNewFile(outPath, out.Bytes()); err != nil {
		return nil, err
	}
	return &pw, nil
}

// dropUser removes the template line that defines user, which must appear exactly once.
func dropUser(src []byte, user string) ([]byte, error) {
	lines := strings.SplitAfter(string(src), "\n")
	kept := make([]string, 0, len(lines))
	found := 0
	for _, line := range lines {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "user" && f[1] == user {
			found++
			continue
		}
		kept = append(kept, line)
	}
	if found != 1 {
		return nil, fmt.Errorf("%d lines define user %s, want 1", found, user)
	}
	return []byte(strings.Join(kept, "")), nil
}

// checkACL checks a rendered ACL file: the first line is "user default off" and every other
// non-empty line, "user <name> on >…", defines one of users, each exactly once. Errors name
// lines and users only, never a line's text, which holds a password.
func checkACL(text string, users []string) error {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if lines[0] != "user default off" {
		return errors.New(`the first line is not "user default off"`)
	}
	seen := map[string]int{}
	for i, line := range lines[1:] {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != "user" || f[2] != "on" || !strings.HasPrefix(f[3], ">") {
			return fmt.Errorf("line %d does not define a user as `user <name> on >password …`", i+2)
		}
		seen[f[1]]++
	}
	for _, u := range users {
		if seen[u] != 1 {
			return fmt.Errorf("%d lines define user %s, want 1", seen[u], u)
		}
		delete(seen, u)
	}
	if len(seen) > 0 {
		extra := slices.Sorted(maps.Keys(seen))
		return fmt.Errorf("the file defines users this run does not have: %s", strings.Join(extra, ", "))
	}
	return nil
}

// newPassword draws a store password: passwordBytes crypto/rand bytes, hex-encoded.
func newPassword() (string, error) {
	b := make([]byte, passwordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("drawing a store password: %w", err)
	}
	return hex.EncodeToString(b), nil
}
