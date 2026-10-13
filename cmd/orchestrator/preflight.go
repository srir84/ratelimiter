package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/dbclient"
	"ratelimiter/limiter"
)

// The limits preflight enforces (DESIGN §B4, §A7, §B9).
const (
	// maxRefuseBP is the largest refuse_after_acquire_bp a run may set: 5 %, the supported
	// per-key failure load (§B4, §B10).
	maxRefuseBP = 500
	// bpPerUnit is refuse_after_acquire_bp's denominator: refusals per 10,000 requests (§B3).
	bpPerUnit = 10_000
	// refusalsPerInstance is the most ERR-15 refusals per second per instance the refund path
	// is sized for (§A7 sizing, §B4: 50/s per instance).
	refusalsPerInstance = 50
	// minRecoveryStepS is the shortest step after a spike, in s (§B9 "Spike recovery").
	minRecoveryStepS = 90
	// rltestTag is the build tag of the test binaries (DESIGN §B1).
	rltestTag = "rltest"
	// portDialTimeout bounds the connection attempt that tells whether a port is taken.
	portDialTimeout = 250 * time.Millisecond
)

// The reasons preflight refuses a run for (plan choice for the codes): each "refusing the
// run" line carries one, with a detail naming what was found.
const (
	reasonScenario           = "scenario_invalid"          // the scenario file does not load (§B4)
	reasonConfig             = "config_invalid"            // harness.toml, or it merged with the overrides, does not load
	reasonBenchFaults        = "bench_faults"              // a benchmark run with [[faults]] (HAR-24)
	reasonBenchClockOverride = "bench_clock_override"      // a benchmark scenario that overrides clock_source (HAR-24)
	reasonBenchClock         = "bench_clock_controlled"    // a benchmark run whose merged clock_source is controlled (HAR-24)
	reasonInstanceConfBench  = "instance_config_bench"     // --instance-config on a benchmark run (§B5)
	reasonInstanceConf       = "instance_config_invalid"   // an --instance-config instance or file that cannot be used
	reasonInstanceConfDSN    = "instance_config_db_dsn"    // an --instance-config whose [api] db_dsn is not the run's
	reasonDSNPassword        = "db_dsn_password"           // an [api] db_dsn that carries a password (AGENTS.md §1 Secrets)
	reasonLimiterOverride    = "limiter_enabled_override"  // a scenario that overrides [api] limiter_enabled (HAR-17)
	reasonDSNOverride        = "db_dsn_override"           // a scenario that overrides [api] db_dsn (§B4)
	reasonRefuseBP           = "refuse_bp_over_limit"      // refuse_after_acquire_bp above 500 (§B4)
	reasonRefusalRate        = "refusal_rate_over_limit"   // refusals/s above 50 × instances (§B4, §A7)
	reasonSpikeStep          = "spike_step_too_short"      // the step after the spike shorter than 90 s (§B9)
	reasonOutageSegments     = "store_outage_segments"     // more than one store outage segment (§A7, §B4)
	reasonRefuseUnbounded    = "store_refuse_unbounded"    // a store refuse body with neither count nor duration_ms (§B6)
	reasonRefuseTooLong      = "store_refuse_too_long"     // a store refuse body longer than outage_tolerance (§A7, FLT-1)
	reasonClockUntagged      = "controlled_clock_untagged" // a controlled clock in a !rltest build (clock_stub.go)
	reasonFaultctlUntagged   = "faultctl_untagged"         // a faultctl cmd in a !rltest build (clock_stub.go)
	reasonBinaryMissing      = "binary_missing"            // hitapi, rladmin, hitgen or sampler missing (§B1)
	reasonBenchRltest        = "bench_binary_rltest"       // a benchmark binary built with -tags=rltest (HAR-24)
	reasonBenchUnverified    = "bench_binary_unverified"   // a benchmark binary whose build info cannot be read (HAR-24)
	reasonPortBound          = "port_bound"                // 6390 or one of 8081…808N taken (§B1)
	reasonRunning            = "orchestrator_running"      // .data/orchestrator.pid names a live process
	reasonThresholds         = "thresholds_missing"        // a benchmark limiter run without its frozen thresholds (§B5, §B9)
	reasonConformance        = "conformance_missing"       // a benchmark run without .data/conformance.json (§B8)
)

// The files preflight reads, relative to the repository root (DESIGN §B1, §B5, §B9).
const (
	harnessConfigFile = "harness.toml"
	binDirName        = "bin"
	conformanceFile   = ".data/conformance.json"
	thresholdsDir     = "runs/thresholds"
)

// site is where the orchestrator finds and puts a run's files and which ports it uses (DESIGN
// §B1): main sets the repository root and the fixed ports; tests use a temporary tree and free
// ports.
type site struct {
	root      string // the repository root, "." in main
	storePort int    // the store's port, 6390 in main
	apiBase   int    // instance n listens on apiBase+n-1, 8081… in main
}

// path is the root-relative slash path rel on disk.
func (s site) path(rel string) string { return filepath.Join(s.root, filepath.FromSlash(rel)) }

// refusal is one reason preflight refuses a run (exit 2): a fixed code and what was found.
type refusal struct {
	reason, detail string
}

// refusals collects a run's refusals in the order the checks find them.
type refusals []refusal

func (r *refusals) add(reason, format string, args ...any) {
	*r = append(*r, refusal{reason: reason, detail: fmt.Sprintf(format, args...)})
}

// inputs is a run that passed preflight: what the run directory and the run sequence are built
// from.
type inputs struct {
	scenario        config.Scenario
	scenarioPath    string
	baseline        bool
	config          []byte         // the run's config.toml (runConfig)
	settings        config.File    // config decoded, without the store password
	binDir          string         // hitapi and rladmin
	testBinDir      string         // hitgen and the sampler: always bin/test (§B1)
	instanceConfigs map[int]string // --instance-config: instance number → its config file (test runs)
	thresholds      string         // the frozen thresholds file the run copies; "" when it copies none
	storePort       int            // the store's port
	apiPorts        []int          // instance n listens on apiPorts[n-1]
}

// preflight checks a run before anything starts (IMPLEMENTATION_PLAN H3 step 1; DESIGN §B4,
// §B5, §B9): it loads the scenario and harness.toml and returns the run's inputs, or every
// refusal it finds. It starts nothing and writes nothing: the benchmark guard (HAR-24) runs
// here, before the store starts, except the /health check, which needs running instances.
func preflight(ctx context.Context, s site, fl flags) (inputs, []refusal) {
	sc, err := config.LoadScenario(fl.scenario)
	if err != nil {
		return inputs{}, []refusal{{reason: reasonScenario, detail: err.Error()}}
	}
	bench := sc.RunKind == config.RunBench
	in := inputs{
		scenario:        sc,
		scenarioPath:    fl.scenario,
		baseline:        fl.baseline,
		binDir:          fl.binDir,
		testBinDir:      s.path(binDirName + "/" + config.RunTest),
		instanceConfigs: fl.instanceConfigs,
		storePort:       s.storePort,
	}
	if in.binDir == "" {
		in.binDir = s.path(binDirName + "/" + sc.RunKind)
	}
	for n := 1; n <= sc.Instances; n++ {
		in.apiPorts = append(in.apiPorts, s.apiBase+n-1)
	}

	var refs refusals
	limiterRun := !fl.baseline
	controlled := false
	runDSN := "" // the merged config's db_dsn; "" when it does not load
	if base, err := os.ReadFile(s.path(harnessConfigFile)); err != nil {
		refs.add(reasonConfig, "reading the harness config: %v", err)
	} else if data, f, err := runConfig(base, sc.Overrides, fl.baseline); err != nil {
		refs.add(reasonConfig, "%v", err)
	} else {
		in.config, in.settings = data, f
		limiterRun = f.API.LimiterEnabled
		controlled = f.Policy.ClockSource == limiter.Controlled
		refusalRules(&refs, sc, f.API.RefuseAfterAcquireBP)
		storeRefusalRule(&refs, sc, f.Policy.OutageToleranceNs)
		dsnRule(&refs, sc, f.API.DBDSN)
		runDSN = f.API.DBDSN
	}
	if bench {
		benchRules(&refs, sc, controlled)
	}
	overrideRules(&refs, sc)
	scenarioRules(&refs, sc)
	refs = append(refs, buildRefusals(sc, controlled)...)
	instanceConfigRules(&refs, sc, fl.instanceConfigs, runDSN)
	binaryRules(&refs, in.binDir, in.testBinDir, bench)
	for _, p := range append([]int{s.storePort}, in.apiPorts...) {
		if held, why := portHeld(ctx, p); held {
			refs.add(reasonPortBound, "127.0.0.1:%d is taken: %s", p, why)
		}
	}
	pidPath := s.path(pidFileName)
	if pid, live, err := livePIDFile(pidPath); err != nil {
		refs.add(reasonRunning, "%v", err)
	} else if live {
		refs.add(reasonRunning, "%s names process %d, which is running: another run is in progress "+
			"(delete the file only if that process is not an orchestrator)", pidPath, pid)
	}
	if limiterRun {
		path := s.path(fmt.Sprintf("%s/%s-%d.json", thresholdsDir, sc.Name, sc.Seed))
		switch err := checkRegular(path); {
		case err == nil:
			in.thresholds = path
		case bench:
			refs.add(reasonThresholds, "a benchmark limiter run needs its frozen thresholds (run the baseline first, DESIGN §B9): %v", err)
		}
	}
	if bench {
		if err := checkRegular(s.path(conformanceFile)); err != nil {
			refs.add(reasonConformance, "a benchmark run needs the latest store conformance result (make test-store): %v", err)
		}
	}
	return in, refs
}

// refusalRules checks refuse_after_acquire_bp, the merged config's (DESIGN §B4): at most 500,
// and the refusals it makes at the highest step rate at most 50/s per instance (§A7). The
// rate rule is exact: bp / 10,000 × the rate's exact value as a rational, so no float rounding
// lets a rate just above the limit pass.
func refusalRules(refs *refusals, sc config.Scenario, bp int) {
	if bp > maxRefuseBP {
		refs.add(reasonRefuseBP, "refuse_after_acquire_bp = %d: want at most %d (5 %%, the supported per-key failure load)", bp, maxRefuseBP)
	}
	maxRate := 0.0
	for _, st := range sc.Steps {
		maxRate = max(maxRate, st.RatePerS)
	}
	rate := new(big.Rat).SetFloat64(maxRate)
	if rate == nil { // not finite; the scenario loader refuses such a rate first
		refs.add(reasonScenario, "the highest step rate %v/s is not a finite rate", maxRate)
		return
	}
	perS := rate.Mul(rate, big.NewRat(int64(bp), bpPerUnit)) // refusals per second
	if perS.Cmp(big.NewRat(int64(refusalsPerInstance*sc.Instances), 1)) > 0 {
		shown := strings.TrimSuffix(strings.TrimRight(perS.FloatString(20), "0"), ".")
		refs.add(reasonRefusalRate, "refuse_after_acquire_bp %d / %d × the highest step rate %v/s = %s refusals/s: want at most %d/s × %d instances",
			bp, bpPerUnit, maxRate, shown, refusalsPerInstance, sc.Instances)
	}
}

// benchRules is the benchmark guard's check of the scenario and the merged config (HAR-24;
// DESIGN §B5): no [[faults]], no clock_source override (whatever its value), no controlled
// clock.
func benchRules(refs *refusals, sc config.Scenario, controlled bool) {
	if len(sc.Faults) > 0 {
		refs.add(reasonBenchFaults, "a benchmark run with %d [[faults]] entries: faults need run_kind = \"test\"", len(sc.Faults))
	}
	switch set, err := sc.OverridesKey("policy", "clock_source"); {
	case err != nil:
		refs.add(reasonScenario, "%v", err)
	case set:
		refs.add(reasonBenchClockOverride, "a benchmark scenario overrides [policy] clock_source: clock overrides need run_kind = \"test\"")
	}
	if controlled {
		refs.add(reasonBenchClock, "a benchmark run with clock_source = \"controlled\": benchmarks use the store clock")
	}
}

// overrideRules refuses, whatever the run kind, the two scenario overrides DESIGN §B4's list
// leaves out and a run takes only from its command line and harness.toml: [api]
// limiter_enabled, since --baseline is the switch (HAR-17) that context.json, the run index
// and hitgen's --baseline record; and [api] db_dsn, since the run empties and counts the
// hits table of harness.toml's database (TRUNCATE hits, db_rows.json).
func overrideRules(refs *refusals, sc config.Scenario) {
	for _, o := range []struct{ key, reason, why string }{
		{"limiter_enabled", reasonLimiterOverride, "--baseline is the switch that turns the limiter off (HAR-17)"},
		{"db_dsn", reasonDSNOverride, "every run uses harness.toml's database, whose hits table it empties and counts"},
	} {
		switch set, err := sc.OverridesKey("api", o.key); {
		case err != nil:
			refs.add(reasonScenario, "%v", err)
		case set:
			refs.add(o.reason, "the scenario overrides [api] %s: %s", o.key, o.why)
		}
	}
}

// dsnRule checks the run's [api] db_dsn, the merged config's, before anything is written:
// config.toml and context.json would hold it (AGENTS.md §1 Secrets). It refuses what
// dbclient.Open would (dsnRefusal); a DSN that does not parse is config_invalid.
func dsnRule(refs *refusals, sc config.Scenario, dsn string) {
	source := harnessConfigFile + "'s [api] db_dsn"
	if set, err := sc.OverridesKey("api", "db_dsn"); err == nil && set {
		source = "the scenario's [api] db_dsn override"
	}
	dsnRefusal(refs, dsn, source, reasonConfig)
}

// dsnRefusal adds the refusal dbclient.CheckDSN finds in dsn, if any: db_dsn_password for a
// DSN that carries a password, parseReason for one that does not parse (a password in it may
// be in a form the password check cannot read). The detail names source and dbclient's fixed
// error, never the DSN's text.
func dsnRefusal(refs *refusals, dsn, source, parseReason string) {
	switch err := dbclient.CheckDSN(dsn); {
	case err == nil:
	case errors.Is(err, dbclient.ErrDSNPassword):
		refs.add(reasonDSNPassword, "%s: %v", source, err)
	default:
		refs.add(parseReason, "%s: %v", source, err)
	}
}

// scenarioRules checks the scenario's own values: the step after the spike, the store outage
// segments among the faults, and the store refusals without a limit. Each build checks the
// faultctl cmds itself (buildRefusals): the untagged one refuses them, the rltest one refuses
// those it could not run.
func scenarioRules(refs *refusals, sc config.Scenario) {
	if sc.Verdict == config.VerdictSpike && sc.SpikeStep != nil {
		if i := *sc.SpikeStep + 1; i < len(sc.Steps) && sc.Steps[i].DurationS < minRecoveryStepS {
			refs.add(reasonSpikeStep, "steps[%d], the step after spike_step = %d, lasts %v s: the recovery window needs at least %d s",
				i, *sc.SpikeStep, sc.Steps[i].DurationS, minRecoveryStepS)
		}
	}
	if n := outageSegments(sc.Faults); n > 1 {
		refs.add(reasonOutageSegments, "the [[faults]] arm %d store outage segments: a run arms at most one", n)
	}
	for i, f := range sc.Faults {
		if b, ok := storeRefusal(f.Cmd); ok && b.unbounded() {
			refs.add(reasonRefuseUnbounded, "faults[%d] %s: a store refusal with neither a count nor a duration_ms refuses until "+
				"DELETE /_test/faults, which a scenario cannot send: give it a duration_ms of at most outage_tolerance", i, f.Cmd)
		}
	}
}

// storeRefusalRule refuses each store refusal (storeRefusal) whose duration_ms is above
// toleranceNs, the merged config's [policy] outage_tolerance_ns: REF-5 allows one outage segment
// of at most outage_tolerance (DESIGN §A7), and HARNESS FLT-1 arms a 5 s outage. A duration_ms
// of exactly the tolerance passes.
func storeRefusalRule(refs *refusals, sc config.Scenario, toleranceNs int64) {
	maxMs := toleranceNs / int64(time.Millisecond) // duration_ms × 1e6 > toleranceNs exactly when duration_ms > maxMs
	for i, f := range sc.Faults {
		if b, ok := storeRefusal(f.Cmd); ok && b.DurationMs > 0 && b.DurationMs > maxMs {
			refs.add(reasonRefuseTooLong, "faults[%d] %s: a store refusal of duration_ms %d is longer than [policy] outage_tolerance_ns %d",
				i, f.Cmd, b.DurationMs, toleranceNs)
		}
	}
}

// outageSegments counts the store outage segments the faults arm (plan choice; DESIGN §A7,
// §B4, §B6): one per faultctl "store down" or "store pause" entry, and one per store refusal
// body (storeRefusal) that is timed (a duration_ms above 0) or unbounded (neither a count nor
// a duration_ms), however many instances it goes to. A refusal with a count and no duration_ms
// ends after that many calls: it arms no segment.
func outageSegments(faults []config.Fault) int {
	n := 0
	for _, f := range faults {
		if f.Faultctl() {
			args := strings.Fields(f.Cmd) // faultctl store down|pause <d>
			if len(args) >= 3 && args[1] == "store" && (args[2] == "down" || args[2] == "pause") {
				n++
			}
		} else if b, ok := storeRefusal(f.Cmd); ok && (b.DurationMs > 0 || b.unbounded()) {
			n++
		}
	}
	return n
}

// faultBody is a /_test/faults body with the fields and types hitapi decodes it into
// (harness/httpapi faultRequest), so that preflight reads a body the way hitapi arms it.
type faultBody struct {
	Target     string `json:"target"`
	Mode       string `json:"mode"`
	Match      string `json:"match"`
	Count      *int   `json:"count"`
	DurationMs int64  `json:"duration_ms"`
	DelayMs    int64  `json:"delay_ms"`
}

// storeRefusal decodes a /_test/faults body into hitapi's field types (one JSON value, so a
// repeated or case-variant key and a null resolve as they do there) and returns it, with ok
// true when it arms a store refusal (FLT-1 (b), DESIGN §B6): target "store", mode "refuse".
// Unknown keys are ignored, so the checks on it fail closed: a key hitapi gains later (H4's
// "at") still counts, and a key it does not know can only add to the segments and refusals of
// a body hitapi answers with 400.
func storeRefusal(body string) (faultBody, bool) {
	dec := json.NewDecoder(strings.NewReader(body))
	var b faultBody
	if err := dec.Decode(&b); err != nil {
		return faultBody{}, false // hitapi answers such a body with 400 and arms nothing
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return faultBody{}, false // a second value: hitapi answers 400 too
	}
	return b, b.Target == "store" && b.Mode == "refuse"
}

// unbounded reports whether a store fault body has neither a count nor a duration_ms: hitapi
// reads an absent, null or 0 count or duration_ms as no limit, so such a refusal refuses every
// matching call until DELETE /_test/faults. A body with a negative count or duration_ms is not
// unbounded: hitapi answers it with 400.
func (b faultBody) unbounded() bool { return (b.Count == nil || *b.Count == 0) && b.DurationMs == 0 }

// instanceConfigRules checks the --instance-config files (plan H3 step 6): only a test run
// takes them, each for an instance the scenario has, each a config file that loads, with an
// [api] db_dsn dbclient takes (dsnRefusal; the run copies the file into its directory and
// context.json) and that is runDSN, the merged config's, since the run empties and counts one
// database's hits table. runDSN "" (the merged config did not load) skips that comparison.
func instanceConfigRules(refs *refusals, sc config.Scenario, configs map[int]string, runDSN string) {
	ns := make([]int, 0, len(configs))
	for n := range configs {
		ns = append(ns, n)
	}
	slices.Sort(ns)
	for _, n := range ns {
		path := configs[n]
		if sc.RunKind == config.RunBench {
			refs.add(reasonInstanceConfBench, "--instance-config %d=%s: a benchmark run gives every instance the run's config.toml", n, path)
			continue
		}
		if n > sc.Instances {
			refs.add(reasonInstanceConf, "--instance-config %d=%s: the scenario has %d instances", n, path, sc.Instances)
			continue
		}
		var f config.File
		data, err := os.ReadFile(path)
		if err == nil {
			f, err = config.Parse(data)
		}
		if err != nil {
			refs.add(reasonInstanceConf, "--instance-config %d=%s: %v", n, path, err)
			continue
		}
		dsnRefusal(refs, f.API.DBDSN, fmt.Sprintf("--instance-config %d=%s: [api] db_dsn", n, path), reasonInstanceConf)
		if runDSN != "" && f.API.DBDSN != runDSN {
			refs.add(reasonInstanceConfDSN, "--instance-config %d=%s: its [api] db_dsn is not the run's config.toml's: "+
				"the run empties and counts one database's hits table", n, path)
		}
	}
}

// binaryRules checks the binaries a run starts (DESIGN §B1): hitapi and rladmin in binDir,
// hitgen and the sampler in bin/test, each an executable regular file. In a benchmark run
// binDir's two binaries, whichever directory that is, must be readable Go binaries whose build
// info does not list the rltest tag (HAR-24).
func binaryRules(refs *refusals, binDir, testBinDir string, bench bool) {
	for _, name := range []string{"hitapi", "rladmin"} {
		path := filepath.Join(binDir, name)
		if err := checkExecutable(path); err != nil {
			refs.add(reasonBinaryMissing, "%v", err)
			continue
		}
		if !bench {
			continue
		}
		switch text, err := buildInfoText(path); {
		case err != nil:
			refs.add(reasonBenchUnverified, "%s: no readable Go build info, so it cannot be shown to be a benchmark build: %v", path, err)
		case listsRltest(text):
			refs.add(reasonBenchRltest, "%s: its build info lists -tags=%s: a test binary in a benchmark run", path, rltestTag)
		}
	}
	for _, name := range []string{"hitgen", "sampler"} {
		if err := checkExecutable(filepath.Join(testBinDir, name)); err != nil {
			refs.add(reasonBinaryMissing, "%v", err)
		}
	}
}

// checkExecutable fails unless path is a regular file with an execute bit.
func checkExecutable(path string) error {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s: not a regular file", path)
	case info.Mode().Perm()&0o111 == 0:
		return fmt.Errorf("%s: not executable", path)
	}
	return nil
}

// checkRegular fails unless path is a regular file.
func checkRegular(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return &fs.PathError{Op: "stat", Path: path, Err: errors.New("not a regular file")}
	}
	return nil
}

// buildInfoText returns a Go binary's build info as `go version -m` prints it, less that
// command's first line and indentation: `go version -m` reads it with debug/buildinfo too.
func buildInfoText(path string) (string, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", err
	}
	return bi.String(), nil
}

// listsRltest reports whether build info text, `go version -m` output or debug/buildinfo's
// form of it, lists the rltest tag: a "build" line whose -tags setting holds rltest among its
// comma-separated tags (plan H3 step 1).
func listsRltest(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		kind, setting, ok := strings.Cut(strings.TrimLeft(line, " \t"), "\t")
		if !ok || kind != "build" {
			continue
		}
		key, value, ok := strings.Cut(setting, "=")
		if !ok || key != "-tags" {
			continue
		}
		if uq, err := strconv.Unquote(value); err == nil {
			value = uq // a value with special characters is quoted
		}
		if slices.Contains(strings.Split(value, ","), rltestTag) {
			return true
		}
	}
	return false
}

// portHeld reports whether 127.0.0.1:port is taken, and how (plan choice for the check): a
// connection to it succeeds, so something listens there or on every address; or it cannot be
// bound. Neither leaves anything open.
func portHeld(ctx context.Context, port int) (bool, string) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dctx, cancel := context.WithTimeout(ctx, portDialTimeout)
	defer cancel()
	var d net.Dialer
	if c, err := d.DialContext(dctx, "tcp", addr); err == nil {
		_ = c.Close() // only the connection's success mattered
		return true, "a connection to it succeeded"
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return true, fmt.Sprintf("it cannot be bound: %v", err)
	}
	_ = l.Close() // only the bind's success mattered
	return false, ""
}
