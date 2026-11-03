package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ratelimiter/harness/config"
	"ratelimiter/harness/dbclient"
	"ratelimiter/harness/runlog"
	"ratelimiter/limiter"
)

// The run sequence's waits (IMPLEMENTATION_PLAN H3 steps 3 and 4; plan choices unless noted).
const (
	// killSlack is the kill grace past P + drain_timeout (DESIGN §B10: 45 s at harness values).
	killSlack = 5 * time.Second
	// hitgenGrace is hitgen's: on SIGTERM it waits at most its 30 s HTTP timeout for the
	// attempts in flight, then writes its summary.
	hitgenGrace = 35 * time.Second
	// samplerGrace is the sampler's: it finishes the tick in progress (under 1 s) and drains
	// samples.jsonl (at most 5 s).
	samplerGrace = 10 * time.Second
	// storeGrace bounds the store's own shutdown on SIGTERM, when SHUTDOWN failed.
	storeGrace = 30 * time.Second
	// storeExitWait bounds the store's exit after it accepted SHUTDOWN.
	storeExitWait = 30 * time.Second
	// rladminTimeout bounds rladmin start-epoch: its own 30 s deadline plus 15 s.
	rladminTimeout = 45 * time.Second
	// pgStartTimeout bounds pg_ctl -w start, which waits up to 60 s itself.
	pgStartTimeout = 90 * time.Second
	// dbTimeout bounds each of TRUNCATE hits, the settings read and the row count.
	dbTimeout = 30 * time.Second
	// commandTimeout bounds the short commands: pg_ctl status, sysctl, sw_vers, --version.
	commandTimeout = 10 * time.Second
	// reportTimeout bounds the report builder: twice its budget, 300 s for a run of 12,000,000
	// run-log lines (IMPLEMENTATION_PLAN H5 step 10).
	reportTimeout = 10 * time.Minute
	// checkTimeout bounds report/validate.py and the thresholds freeze, which read report.json,
	// context.json, the scenario and the thresholds files, never the run logs.
	checkTimeout = 2 * time.Minute
)

// The files and programs the run uses, relative to the repository root or the run directory
// (DESIGN §B1, §B8; IMPLEMENTATION_PLAN H3 steps 3 and 5).
const (
	pgData        = ".data/pg"
	pgLog         = ".data/pg.log"
	runIndex      = "runs/index.jsonl"
	contextFile   = "context.json"
	dbRowsFile    = "db_rows.json"
	scheduleFile  = "schedule.jsonl"
	reportFile    = "report.json"
	valkeyProgram = "valkey-server"
)

// The store profiles (SPEC STA-16, STA-16a; docs/operator-guide.md "Store profiles"): durable
// is the default and the only one STA-16 calls durable; the others are opt-in and trade loss
// on an OS crash or power cut for latency.
const (
	profileDurable  = "durable"  // appendfsync always
	profileEverysec = "everysec" // appendfsync everysec
	profileBarrier  = "barrier"  // appendfsync always with F_BARRIERFSYNC: the patched program
	profileBuffered = "buffered" // appendfsync no
	// barrierProgram is the patched valkey-server deploy/build-valkey-barrier.sh builds.
	barrierProgram = ".data/valkey-barrier/valkey-server"
)

// storeProfileFiles returns a store profile's config file, a root-relative slash path, and its
// program: valkeyProgram, looked up on PATH, or barrierProgram, root-relative. "" is durable.
func storeProfileFiles(profile string) (conf, program string, err error) {
	switch profile {
	case "", profileDurable:
		return "deploy/valkey-durable.conf", valkeyProgram, nil
	case profileEverysec:
		return "deploy/valkey-everysec.conf", valkeyProgram, nil
	case profileBarrier:
		return "deploy/valkey-barrier.conf", barrierProgram, nil
	case profileBuffered:
		return "deploy/valkey-buffered.conf", valkeyProgram, nil
	default:
		return "", "", fmt.Errorf("--store-profile %q: want %s, %s, %s or %s", profile,
			profileDurable, profileEverysec, profileBarrier, profileBuffered)
	}
}

// The report steps at a run's end (IMPLEMENTATION_PLAN H5 steps 2, 5, 6 and 7; DESIGN §B5,
// §B9, §C5), each `python3 …` in the repository root.
const (
	pythonProgram  = "python3"
	reportModule   = "report"             // python3 -m report runs/<id>
	validateScript = "report/validate.py" // python3 report/validate.py runs/<id> [--mode baseline]
	freezeModule   = "report.thresholds"  // python3 -m report.thresholds runs/<id> --out <frozen file>
	markInvalid    = "--mark-invalid"     // python3 -m report --mark-invalid [--freeze] runs/<id>
	markFreeze     = "--freeze"           // the mark of a baseline whose freeze failed
	// maxFailedChecks is the most bytes of validate.py's FAIL lines a failure's message carries;
	// validate.stderr holds them all.
	maxFailedChecks = 400
)

// The child output files' stems (plan choice): runs/<id>/<stem>.stderr.
const (
	nameStore      = "valkey-server"
	nameSampler    = "sampler"
	nameHitgen     = "hitgen"
	nameRladmin    = "rladmin"
	nameReport     = "report"
	nameValidate   = "validate"
	nameInvalid    = "mark-invalid"
	nameThresholds = "thresholds"
)

// The reasons the run sequence refuses a run once it has started something (exit 2, after the
// run is torn down): checks that need a running store or running instances.
const (
	reasonStoreMaxClients     = "store_maxclients"        // CONFIG GET maxclients is not 4096 (§B1)
	reasonBenchHealthFixtures = "bench_health_fixtures"   // a benchmark instance's /health has fixtures: true (HAR-24)
	reasonBenchHealthClock    = "bench_health_clock"      // a benchmark instance's /health has clock_source controlled (HAR-24)
	reasonBenchHealthUnread   = "bench_health_unverified" // a benchmark instance's /health cannot be read (HAR-24)
)

// runSequence runs the §B5 sequence once preflight has passed and the run directory exists
// (IMPLEMENTATION_PLAN H3 steps 3 to 6) and returns the exit code: 0 after a finished run; 1
// when a step fails or ctx ends (a signal: stopSignals); 2 when a check that needs the running
// store or instances refuses the run. Every failure and refusal stops whatever runs first. envs holds
// each child's environment, with only the store passwords it uses; client is the HTTP client
// main built, and killClient the one main built for a faultctl api kill's pre-kill call and
// /health reads (plan H4 step 7); now reads the wall clock (context.json, the fault and rewrite
// records).
func runSequence(ctx context.Context, logger *slog.Logger, s site, in inputs, rd runDir, envs *childEnvs,
	client, killClient *http.Client, now func() time.Time) int {
	r := newRunner(logger, s, in, rd, envs, client, now)
	r.killClient = killClient
	return r.run(ctx)
}

// apiInstance is one hitapi instance's setup: its address, its config file (the run's
// config.toml, or its own with --instance-config), the kill grace and P from that file.
type apiInstance struct {
	addr   string
	config string
	grace  time.Duration
	p      time.Duration // max_release_delay_ns: a faultctl api kill's pre-kill call takes up to P + 2 s
	own    bool          // started with --instance-config: ready once its /health reason is not no_epoch
}

// runner is one run's sequence: what it was given, the children it started (each stopped in
// order at the end, or by stopAll on a failure), and context.json as it fills.
type runner struct {
	logger *slog.Logger
	s      site
	in     inputs
	rd     runDir
	envs   *childEnvs
	client *http.Client
	// killClient makes a faultctl api kill's requests (plan H4 step 7): its timeout must not cut
	// the pre-kill call short, which takes up to P + 2 s. nil: an api kill fails, doing nothing.
	killClient *http.Client
	now        func() time.Time

	runPath   string // the run directory, absolute: children get absolute paths
	binDir    string // hitapi and rladmin, absolute
	testBin   string // hitgen and the sampler, absolute
	storeAddr string // 127.0.0.1:<store port>, config.toml's [store] addr
	storeConf string // the store profile's config file, root-relative (storeProfileFiles)
	apis      []apiInstance

	// mu guards the child table (store, apiProcs, procs and pids.json, rec.Children) while the
	// faults run, each on its own goroutine: a faultctl command that restarts the store or an
	// instance replaces its child there (plan H4 step 7). Outside drive one goroutine runs the
	// sequence.
	mu                     sync.Mutex
	store, sampler, hitgen *child
	apiProcs               []*child      // apiProcs[i] is instance i+1; nil once a fault killed it for good
	procs                  []runlog.Proc // pids.json

	// countRows counts the hits table's rows for db_rows.json: the database client's
	// CountRows, set once the client is open.
	countRows func(ctx context.Context) (int64, error)

	rec     runContext
	failure []string // what went wrong in a run that went on to its end: exit 1
}

func newRunner(logger *slog.Logger, s site, in inputs, rd runDir, envs *childEnvs, client *http.Client, now func() time.Time) *runner {
	return &runner{logger: logger, s: s, in: in, rd: rd, envs: envs, client: client, now: now,
		runPath: absPath(rd.path), binDir: absPath(in.binDir), testBin: absPath(in.testBinDir),
		rec: newRunContext(in, rd)}
}

// run is the sequence (DESIGN §B5; HARNESS §4).
func (r *runner) run(ctx context.Context) int {
	r.rec.StartNs = r.now().UnixNano()
	if err := r.prepare(); err != nil {
		return r.abort(ctx, "preparing the run", err)
	}
	r.gatherHost(ctx)

	// The store (§A2, §B1), fresh in runs/<id>/store, with the maxclients it must have.
	if code, ok := r.storeStep(ctx); !ok {
		return code
	}

	// PostgreSQL: started unless it runs, and the hits table emptied (HARNESS §4 step 1).
	if err := r.startPostgres(ctx); err != nil {
		return r.abort(ctx, "starting PostgreSQL", err)
	}
	db, err := dbclient.Open(ctx, r.in.settings.API.DBDSN)
	if err != nil {
		return r.abort(ctx, "opening the database client", err)
	}
	defer db.Close()
	r.countRows = db.CountRows
	if err := r.prepareDB(ctx, db); err != nil {
		return r.abort(ctx, "emptying the hits table", err)
	}

	// The controlled clock is set before the epoch starts (HARNESS §4 step 2; DESIGN §B6).
	if r.in.settings.Policy.ClockSource == limiter.Controlled {
		ns, err := setControlledClock(ctx, r.storeAddr, r.rd.pw.fixture, r.now())
		if err != nil {
			return r.abort(ctx, "setting the controlled clock", err)
		}
		r.rec.ControlledClockNs = &ns
		r.logger.Info("controlled clock set", "ns", ns)
	}
	if err := r.startEpoch(ctx); err != nil {
		return r.abort(ctx, "starting the epoch", err)
	}
	r.logger.Info("epoch started", "epoch_id", r.rec.EpochID)

	// The instances, ready, then the benchmark guard's /health check (HAR-24).
	if code, ok := r.instancesStep(ctx); !ok {
		return code
	}

	// The sampler, then hitgen; the faults and forced rewrites count from hitgen's t0.
	if err := r.startSampler(ctx); err != nil {
		return r.abort(ctx, "starting the sampler", err)
	}
	t0 := newT0Watch()
	if err := r.startHitgen(ctx, t0); err != nil {
		return r.abort(ctx, "starting hitgen", err)
	}
	return r.afterHitgen(ctx, t0)
}

// storeStep starts the store (startStore) and refuses the run unless CONFIG GET maxclients as
// rl_ops shows 4096 (IMPLEMENTATION_PLAN H3 step 3; DESIGN §B1): Valkey lowers it by itself
// when its file limit is too low. The settings it read go into context.json. ok is false when
// the run ended here, torn down: code is then 1 for a failure, 2 for the refusal.
func (r *runner) storeStep(ctx context.Context) (code int, ok bool) {
	if err := r.startStore(ctx); err != nil {
		return r.abort(ctx, "starting the store", err), false
	}
	set, err := configGet(ctx, r.storeAddr, r.rd.pw.ops, storeSettingKeys()...)
	if err != nil {
		return r.abort(ctx, "reading the store's settings", err), false
	}
	r.rec.StoreSettings = set
	if got := set["maxclients"]; got != storeMaxClients {
		return r.refuse(ctx, refusal{reason: reasonStoreMaxClients, detail: fmt.Sprintf(
			"CONFIG GET maxclients = %q, want %s: the store lowered it, its file limit is too low (DESIGN §B1)", got, storeMaxClients)}), false
	}
	r.logger.Info("store started", "addr", r.storeAddr, "pid", r.store.proc.PID)
	return exitOK, true
}

// instancesStep starts the instances (startAPIs), waits until they are ready (waitReady) and,
// in a benchmark run, refuses the run when an instance's /health cannot show production
// construction (guardHealth; HAR-24, DESIGN §B5 "Benchmark guard"). ok is false when the run
// ended here, torn down: code is then 1 for a failure, 2 for the refusal.
func (r *runner) instancesStep(ctx context.Context) (code int, ok bool) {
	if err := r.startAPIs(ctx); err != nil {
		return r.abort(ctx, "starting hitapi", err), false
	}
	if err := r.waitReady(ctx, readyWait); err != nil {
		return r.abort(ctx, "waiting for the instances", err), false
	}
	if r.in.scenario.RunKind == config.RunBench {
		if refs := r.guardHealth(ctx); len(refs) > 0 {
			return r.refuse(ctx, refs...), false
		}
	}
	r.logger.Info("instances ready", "addrs", strings.Join(r.apiAddrs(), ","))
	return exitOK, true
}

// afterHitgen is the run once hitgen has started (IMPLEMENTATION_PLAN H3 steps 3 and 4): the
// faults and forced rewrites while it sends (drive), then finishRun. When ctx ends first (a
// signal), the run is torn down in order (abort: hitgen, the instances, the sampler, the
// store) and exits 1, with no run index line.
func (r *runner) afterHitgen(ctx context.Context, t0 *t0Watch) int {
	r.drive(ctx, t0)
	if err := ctx.Err(); err != nil {
		return r.abort(ctx, "the run was stopped", err)
	}
	return r.finishRun(ctx)
}

// finishRun is the end of a run whose hitgen has exited (IMPLEMENTATION_PLAN H3 steps 3 and
// 4, H5 step 7): the instances stop gracefully, db_rows.json, the sampler stops, context.json,
// the report and its validation, the store's shutdown, a valid baseline's thresholds freeze and
// the run index. A failure noted along the way makes it exit 1 without the run index line (and
// without the freeze), and context.json's outcome says failed with the reasons: hitgen's exit; a
// fault or rewrite that failed; a store that exited during the run; in a benchmark run, a
// context.json item of PRF-5 it could not record (missingPRF5); an instance or the sampler that
// exited before it was stopped, or ended other than with exit 0 after SIGTERM (judgeStops); the
// report builder; a report that report/validate.py does not pass (marked invalid in report.json
// and report.md); the store's shutdown; the freeze (the report then marked invalid too); ctx
// ending (a signal) before the end, the freeze included; and a run index line that cannot be
// appended. The teardown's bounded steps (the instances' grace, db_rows, the sampler's stop,
// SHUTDOWN) run even after a signal, so the evidence is complete; the report steps (the builder,
// validate.py, the freeze) run on ctx, so a signal stops them, and are skipped once ctx has
// ended; only marking a report invalid, when validate.py did not pass it or its baseline's
// freeze did not complete, runs after a signal too (report, freeze).
func (r *runner) finishRun(ctx context.Context) int {
	bounded := context.WithoutCancel(ctx)
	capture := r.hitgen.capture
	hg := r.stopChild(&r.hitgen, hitgenGrace)
	if hg.Exit != "exit 0" {
		r.fail("hitgen ended with %s (see %s%s)", hg.Exit, nameHitgen, stderrSuffix)
	}
	if err := capture.writeErr(); err != nil {
		r.fail("saving hitgen's output in %s%s: %v", nameHitgen, stderrSuffix, err)
	}
	for _, f := range r.rec.Faults {
		switch {
		case f.faultctl && (f.Error != "" || f.StartNs == 0):
			r.fail("the fault at %v s, %s, failed: %s", f.AtS, f.Cmd, f.Error)
		case !f.faultctl && (f.Error != "" || !f.sent()):
			r.fail("a fault at %v s was not armed everywhere", f.AtS)
		}
	}
	for _, rw := range r.rec.Rewrites {
		if rw.Error != "" {
			r.fail("the forced rewrite at %v s failed", rw.AtS)
		}
	}
	r.stopAPIs()
	if err := r.writeDBRows(bounded); err != nil {
		r.fail("db_rows.json: %v", err)
	}
	r.stopChild(&r.sampler, samplerGrace)
	if sum, err := fileSHA256(filepath.Join(r.runPath, scheduleFile)); err != nil {
		r.readErr("schedule_sha256", err)
	} else {
		r.rec.ScheduleSHA256 = &sum
	}
	// A store that exited during the run fails it before the outcome is set, with its record,
	// so context.json and the exit code agree; stopStore has nothing left to stop then.
	if c := r.store; c != nil && c.exited() {
		r.store = nil
		rec := c.stop(storeGrace)
		r.record(rec)
		r.fail("the store exited during the run (%s; see %s%s)", rec.Exit, nameStore, stderrSuffix)
	}
	if r.in.scenario.RunKind == config.RunBench {
		if missing := r.missingPRF5(); len(missing) > 0 {
			r.fail("context.json lacks %s, which a benchmark run must record (PRF-5; see read_errors)", strings.Join(missing, ", "))
		}
	}
	r.judgeStops()
	r.rec.EndNs = r.now().UnixNano()
	r.writeOutcome()
	written := len(r.failure)
	reported := false // the report was built and validate.py passed it
	if ctx.Err() == nil {
		if err := r.report(ctx); err != nil {
			r.fail("the report: %v", err)
		} else {
			reported = true
		}
	}
	if err := r.stopStore(bounded); err != nil {
		r.fail("shutting the store down: %v", err)
	}
	// A baseline that ended with no failure, its report valid in baseline mode, freezes its
	// thresholds when the frozen file does not exist yet (DESIGN §B9 step 2), before its run
	// index line: a freeze that fails, that a signal stops, or that a signal keeps from running,
	// fails the run and marks its report invalid.
	if reported && len(r.failure) == 0 && !r.in.settings.API.LimiterEnabled {
		if err := r.freeze(ctx); err != nil {
			r.fail("freezing the thresholds: %v", err)
		}
	}
	if ctx.Err() != nil {
		r.fail("stopped by a signal")
	}
	// Only a run with no failure gets its run index line; a line that cannot be appended fails
	// the run, which context.json then says too.
	if len(r.failure) == 0 {
		if err := appendRunIndex(r.s.path(runIndex), r.in.scenario.Name, r.in.baseline, r.rd.id); err != nil {
			r.fail("the run index: %v", err)
		}
	}
	if len(r.failure) > written {
		r.writeOutcome() // a failure after context.json was written: it says so too
	}
	if len(r.failure) > 0 {
		r.logger.Error("the run failed", "run_id", r.rd.id, "error", strings.Join(r.failure, "; "))
		return exitFailure
	}
	r.logger.Info("run finished", "run_id", r.rd.id, "dir", r.rd.path)
	return exitOK
}

// judgeStops fails the run for each instance and the sampler, once stopped, that exited before
// the orchestrator stopped it, or ended other than with exit 0 after SIGTERM: hitapi exits 1
// when its run log was not written whole, the sampler when samples.jsonl was not. A SIGKILL
// after the grace is only recorded (IMPLEMENTATION_PLAN H3 step 3). hitgen's end is judged by
// its exit, the store's on its own. An instance a faultctl api kill ended on purpose is judged
// by that fault's record, and the one that replaced it here like any other (plan H4 step 7).
func (r *runner) judgeStops() {
	for _, c := range r.rec.Children {
		if (c.Role != runlog.RoleHitAPI && c.Role != runlog.RoleSampler) || c.Fault != "" {
			continue
		}
		switch {
		case c.Early:
			r.fail("%s exited before the orchestrator stopped it (%s)", c.Name, c.Exit)
		case !c.Killed && c.Exit != "exit 0":
			r.fail("%s ended with %s after SIGTERM (see %s%s)", c.Name, c.Exit, c.Name, stderrSuffix)
		}
	}
}

// writeOutcome sets context.json's outcome from the failures noted so far (ok when there are
// none) and writes the file; a write that fails is a failure too.
func (r *runner) writeOutcome() {
	r.rec.Outcome, r.rec.Error = outcomeOK, ""
	if len(r.failure) > 0 {
		r.rec.Outcome, r.rec.Error = outcomeFailed, strings.Join(r.failure, "; ")
	}
	if err := r.writeContext(); err != nil {
		r.fail("context.json: %v", err)
	}
}

// fail notes a failure of a run that goes on to its end.
func (r *runner) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.logger.Error("run failure", "error", msg)
	r.failure = append(r.failure, msg)
}

// abort ends a run that failed after it began (plan H3 step 4): it logs why, stops every
// child that runs (stopAll), writes context.json with the failure and returns exit 1.
func (r *runner) abort(ctx context.Context, step string, err error) int {
	r.logger.Error("the run failed", "run_id", r.rd.id, "step", step, "error", err)
	r.rec.Outcome, r.rec.Error = outcomeFailed, step+": "+err.Error()
	r.stopAll(ctx)
	r.rec.EndNs = r.now().UnixNano()
	if err := r.writeContext(); err != nil {
		r.logger.Error("writing context.json", "error", err)
	}
	return exitFailure
}

// refuse ends a run that a check of the running store or instances refused (DESIGN §B1
// maxclients, §B5 benchmark guard): one "refusing the run" line per reason, as preflight
// logs them, then the run is torn down like a failure, and the exit code is 2.
func (r *runner) refuse(ctx context.Context, refs ...refusal) int {
	details := make([]string, 0, len(refs))
	for _, ref := range refs {
		r.logger.Error("refusing the run", "reason", ref.reason, "detail", ref.detail)
		details = append(details, ref.reason+": "+ref.detail)
	}
	r.rec.Outcome, r.rec.Error = outcomeRefused, strings.Join(details, "; ")
	r.stopAll(ctx)
	r.rec.EndNs = r.now().UnixNano()
	if err := r.writeContext(); err != nil {
		r.logger.Error("writing context.json", "error", err)
	}
	return exitRefused
}

// stopAll stops every child that still runs, in order: hitgen, the instances (each with its
// grace), the sampler, then the store (SHUTDOWN as rl_ops, else SIGTERM and SIGKILL). It runs
// even after ctx ended: each stop has its own bound.
func (r *runner) stopAll(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	r.stopChild(&r.hitgen, hitgenGrace)
	r.stopAPIs()
	r.stopChild(&r.sampler, samplerGrace)
	if err := r.stopStore(ctx); err != nil {
		r.logger.Error("shutting the store down", "error", err)
	}
}

// stopChild stops *c, when it was started and not yet stopped, records how it ended and clears
// *c, so no child is stopped twice.
func (r *runner) stopChild(c **child, grace time.Duration) childRecord {
	if *c == nil {
		return childRecord{}
	}
	rec := (*c).stop(grace)
	*c = nil
	r.record(rec)
	return rec
}

// record adds a child's end to context.json and logs it.
func (r *runner) record(rec childRecord) {
	r.rec.Children = append(r.rec.Children, rec)
	if rec.Killed || rec.Error != "" {
		r.logger.Warn("child killed after its grace", "name", rec.Name, "pid", rec.PID, "grace_ns", rec.GraceNs,
			"exit", rec.Exit, "error", rec.Error)
		return
	}
	r.logger.Info("child stopped", "name", rec.Name, "pid", rec.PID, "exit", rec.Exit)
}

// stopAPIs stops every running instance at once (HAR-15): SIGTERM, then each instance's own
// grace, P + drain_timeout + 5 s from the config file it was started with, then SIGKILL,
// which is recorded (plan H3 step 3). An instance a fault restarted is stopped so too; one it
// killed for good has left the table already.
func (r *runner) stopAPIs() {
	procs := r.apiProcs
	r.apiProcs = nil
	recs := make([]childRecord, len(procs))
	var wg sync.WaitGroup
	for i, c := range procs {
		if c == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs[i] = c.stop(r.apis[i].grace)
		}()
	}
	wg.Wait()
	for i, rec := range recs {
		if procs[i] != nil {
			r.record(rec)
		}
	}
}

// stopStore shuts the store down (plan H3 step 3): SHUTDOWN as rl_ops, which keeps the AOF in
// runs/<id>/store, and a wait for the process to exit; when that fails, SIGTERM (a graceful
// shutdown too) and SIGKILL after storeGrace.
func (r *runner) stopStore(ctx context.Context) error {
	c := r.store
	if c == nil {
		return nil
	}
	r.store = nil
	c.stopRequested.Store(true)
	var err error
	if !c.exited() {
		if err = shutdownStore(ctx, r.storeAddr, r.rd.pw.ops); err == nil && !c.waitFor(storeExitWait) {
			err = fmt.Errorf("the store still ran %v after SHUTDOWN", storeExitWait)
		}
	}
	rec := c.stop(storeGrace)
	r.record(rec)
	switch {
	case err != nil:
		return err
	case rec.Killed || rec.Error != "":
		return fmt.Errorf("the store had to be killed: %s %s", rec.Exit, rec.Error)
	case rec.Early:
		return fmt.Errorf("the store exited before it was shut down (%s)", rec.Exit)
	}
	return nil
}

// prepare checks what the sequence relies on: config.toml names the store the orchestrator
// starts; the store profile's program is on PATH, or built for the barrier profile; each instance's address, config file (an
// --instance-config file is copied into the run directory as instance-N.toml) and kill grace.
func (r *runner) prepare() error {
	r.storeAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(r.in.storePort))
	if a := r.in.settings.Store.Addr; a != r.storeAddr {
		return fmt.Errorf("config.toml's [store] addr is %s, but the orchestrator starts the store on %s", a, r.storeAddr)
	}
	conf, prog, err := storeProfileFiles(r.in.storeProfile)
	if err != nil {
		return err
	}
	if prog != valkeyProgram {
		prog = absPath(r.s.path(prog))
	}
	bin, err := exec.LookPath(prog)
	if err != nil {
		if prog != valkeyProgram {
			return fmt.Errorf("finding the store program: %w (run deploy/build-valkey-barrier.sh)", err)
		}
		return fmt.Errorf("finding the store program: %w", err)
	}
	r.storeConf = conf
	r.rec.StoreProfile = r.in.storeProfile
	if r.rec.StoreProfile == "" {
		r.rec.StoreProfile = profileDurable
	}
	r.rec.Versions.ValkeyPath = bin
	cfg := filepath.Join(r.runPath, configFile)
	for n, port := range r.in.apiPorts {
		a := apiInstance{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), config: cfg, grace: killGrace(r.in.settings),
			p: time.Duration(r.in.settings.Instance.MaxReleaseDelayNs)}
		if src, ok := r.in.instanceConfigs[n+1]; ok {
			info, f, err := copyInstanceConfig(src, r.runPath, n+1)
			if err != nil {
				return err
			}
			a.config, a.grace, a.p, a.own = filepath.Join(r.runPath, info.File), killGrace(f), time.Duration(f.Instance.MaxReleaseDelayNs), true
			r.rec.InstanceConfigs = append(r.rec.InstanceConfigs, info)
		}
		r.apis = append(r.apis, a)
	}
	return nil
}

// killGrace is how long the orchestrator waits after SIGTERM before it kills an instance
// started with f (DESIGN §B10, HARNESS HAR-15): P + drain_timeout + 5 s, 45 s at harness
// values. Killing earlier would lose refund events (HAR-13).
func killGrace(f config.File) time.Duration {
	return time.Duration(f.Instance.MaxReleaseDelayNs+f.Instance.DrainTimeoutNs) + killSlack
}

// apiAddrs returns the instances' addresses in instance order.
func (r *runner) apiAddrs() []string {
	addrs := make([]string, len(r.apis))
	for i, a := range r.apis {
		addrs[i] = a.addr
	}
	return addrs
}

// addProc adds a started child to pids.json and rewrites the file (plan H3 step 4): the
// sampler re-reads it on every tick.
func (r *runner) addProc(c *child) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.procs = append(r.procs, c.proc)
	if err := runlog.WritePIDs(filepath.Join(r.runPath, runlog.PIDsFile), runlog.PIDs{Procs: r.procs}); err != nil {
		return fmt.Errorf("writing %s: %w", runlog.PIDsFile, err)
	}
	return nil
}

// startStore starts valkey-server with the run's store profile through the file-limit shell
// (storeArgs, runlog.FileLimitCommand) and waits until it answers PING as rl_ops, at most
// storeStartWait; a store that exits first fails the run.
func (r *runner) startStore(ctx context.Context) error {
	c, err := startChild(ctx, r.runPath, r.storeSpec())
	if err != nil {
		return err
	}
	r.store = c
	if err := r.addProc(c); err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, storeStartWait)
	defer cancel()
	tick := time.NewTicker(storeStartPoll)
	defer tick.Stop()
	for {
		if c.exited() {
			return fmt.Errorf("valkey-server ended with %s before it answered (see %s%s)", exitText(c.waitErr), nameStore, stderrSuffix)
		}
		pctx, pcancel := context.WithTimeout(wctx, time.Second)
		err := pingStore(pctx, r.storeAddr, r.rd.pw.ops)
		pcancel()
		if err == nil {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("the store did not answer PING as %s within %v: %w", userOps, storeStartWait, err)
		case <-tick.C:
		}
	}
}

// storeSpec is the store's child: valkey-server through the file-limit shell, with no
// password in its environment (it reads the rendered ACL file).
func (r *runner) storeSpec() childSpec {
	return childSpec{name: nameStore, role: runlog.RoleValkey, path: r.rec.Versions.ValkeyPath, args: r.storeArgs(),
		env: r.envs.plain, ulimit: true}
}

// storeArgs returns the run's valkey-server arguments (runlog.StoreArgs, which faultctl's store
// down rebuilds to restart the store, plan H4 step 3): the run's store profile file, the
// store port, runs/<id>/store and its acl.conf; production rewrite settings only when the
// scenario forces rewrites.
func (r *runner) storeArgs() []string {
	dir := filepath.Join(r.runPath, storeDir)
	return runlog.StoreArgs(absPath(r.s.path(r.storeConf)), r.in.storePort, dir, filepath.Join(dir, aclFile),
		len(r.in.scenario.ForcedAOFRewritesS) > 0)
}

// startPostgres runs `pg_ctl -D .data/pg status || pg_ctl -D .data/pg -l .data/pg.log -w start`
// (plan H3 step 3). A start that fails while the server came up anyway counts as started. The
// cluster is shared and never stopped by the orchestrator.
func (r *runner) startPostgres(ctx context.Context) error {
	data, logf := absPath(r.s.path(pgData)), absPath(r.s.path(pgLog))
	status := func() error {
		sctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		_, err := runCommand(sctx, "", r.envs.plain, nil, "pg_ctl", "-D", data, "status")
		return err
	}
	if status() == nil {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, pgStartTimeout)
	defer cancel()
	_, err := runCommand(sctx, "", r.envs.plain, nil, "pg_ctl", "-D", data, "-l", logf, "-w", "start")
	if err == nil || status() == nil {
		r.logger.Info("PostgreSQL started", "data", data)
		return nil
	}
	return fmt.Errorf("pg_ctl start (see %s): %w", logf, err)
}

// prepareDB empties the hits table (HARNESS §4 step 1) and reads the PostgreSQL settings
// context.json records (DESIGN §B10).
func (r *runner) prepareDB(ctx context.Context, db *dbclient.Client) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err := db.Truncate(ctx); err != nil {
		return err
	}
	set, err := db.Settings(ctx, postgresSettingKeys()...)
	if err != nil {
		r.readErr("postgres_settings", err)
		return nil
	}
	r.rec.PostgresSettings = set
	return nil
}

// startEpoch runs `rladmin start-epoch --policy runs/<id>/config.toml --json` from the bin
// directory as rl_admin (DESIGN §A6; plan H3 step 3: no flag, the store is fresh) and records
// the epoch ID it printed in context.json. Its stderr goes to runs/<id>/rladmin.stderr.
func (r *runner) startEpoch(ctx context.Context) error {
	errOut, err := os.OpenFile(filepath.Join(r.runPath, nameRladmin+stderrSuffix), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = errOut.Close() }() // rladmin has exited; its lines are on the file
	ctx, cancel := context.WithTimeout(ctx, rladminTimeout)
	defer cancel()
	out, err := runCommand(ctx, "", r.envs.rladmin, errOut, filepath.Join(r.binDir, "rladmin"),
		"start-epoch", "--policy", filepath.Join(r.runPath, configFile), "--json")
	if err != nil {
		return fmt.Errorf("rladmin start-epoch (see %s%s): %w", nameRladmin, stderrSuffix, err)
	}
	var res struct {
		EpochID string `json:"epoch_id"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("rladmin start-epoch printed %.200q: %w", out, err)
	}
	if b, err := hex.DecodeString(res.EpochID); err != nil || len(b) != 16 || strings.ToLower(res.EpochID) != res.EpochID {
		return fmt.Errorf("rladmin start-epoch printed epoch_id %.80q, want 32 lowercase hex characters", res.EpochID)
	}
	r.rec.EpochID = res.EpochID
	return nil
}

// startAPIs starts the instances (DESIGN §B5; plan H3 step 3): hitapi N from the bin
// directory, on 127.0.0.1:8080+N, with its config file, its run log runs/<id>/api-N.jsonl and
// the run ID, through the file-limit shell, with RL_LIB_PASSWORD only (apiSpec).
func (r *runner) startAPIs(ctx context.Context) error {
	for i := range r.apis {
		c, err := startChild(ctx, r.runPath, r.apiSpec(i))
		if err != nil {
			return err
		}
		r.apiProcs = append(r.apiProcs, c)
		if err := r.addProc(c); err != nil {
			return err
		}
	}
	return nil
}

// apiSpec is instance i's (0-based) child: hitapi N = i+1 from the bin directory.
func (r *runner) apiSpec(i int) childSpec {
	a, n := r.apis[i], i+1
	return childSpec{
		name: fmt.Sprintf("%s-%d", runlog.RoleHitAPI, n), role: runlog.RoleHitAPI, instance: n,
		path: filepath.Join(r.binDir, "hitapi"),
		args: []string{"--config", a.config, "--listen", a.addr, "--log", filepath.Join(r.runPath, fmt.Sprintf("api-%d.jsonl", n)),
			"--instance", strconv.Itoa(n), "--run-id", r.rd.id},
		env: r.envs.hitapi, ulimit: true,
	}
}

// startSampler starts the sampler (samplerSpec).
func (r *runner) startSampler(ctx context.Context) error {
	c, err := startChild(ctx, r.runPath, r.samplerSpec())
	if err != nil {
		return err
	}
	r.sampler = c
	return r.addProc(c)
}

// samplerSpec is the sampler's child: bin/test/sampler (DESIGN §B7) on the run directory, the
// instances, the bin directory's rladmin and the PostgreSQL data directory, with its
// environment (RL_ADMIN_PASSWORD, RL_OPS_PASSWORD, RL_STORE_ADDR).
func (r *runner) samplerSpec() childSpec {
	return childSpec{name: nameSampler, role: runlog.RoleSampler, path: filepath.Join(r.testBin, "sampler"),
		args: []string{"--run-dir", r.runPath, "--apis", strings.Join(r.apiAddrs(), ","),
			"--rladmin", filepath.Join(r.binDir, "rladmin"), "--pg-data", absPath(r.s.path(pgData))},
		env: r.envs.sampler}
}

// hitgenSpec is hitgen's child: bin/test/hitgen (DESIGN §B4) on the scenario, the instances and
// the run directory, with the run's frozen thresholds when it copied them and --baseline for a
// baseline run, and no password. Its output goes through a captureWriter, which hands its
// lines to t0 (its "sending" line).
func (r *runner) hitgenSpec(t0 *t0Watch) childSpec {
	args := []string{"--scenario", absPath(r.in.scenarioPath), "--targets", strings.Join(r.apiAddrs(), ","), "--out", r.runPath}
	if r.rd.thresholdsSHA256 != "" {
		args = append(args, "--thresholds", filepath.Join(r.runPath, thresholdsFile))
	}
	if r.in.baseline {
		args = append(args, "--baseline")
	}
	return childSpec{name: nameHitgen, role: runlog.RoleHitgen, path: filepath.Join(r.testBin, "hitgen"), args: args,
		env: r.envs.plain, capture: &captureWriter{onLine: t0.line}}
}

// startHitgen starts hitgen (hitgenSpec).
func (r *runner) startHitgen(ctx context.Context, t0 *t0Watch) error {
	c, err := startChild(ctx, r.runPath, r.hitgenSpec(t0))
	if err != nil {
		return err
	}
	r.hitgen = c
	if err := r.addProc(c); err != nil {
		return err
	}
	r.logger.Info("hitgen started", "pid", c.proc.PID)
	return nil
}

// report is the run's report (IMPLEMENTATION_PLAN H5 step 7; DESIGN §B5, §C5): the builder,
// `python3 -m report runs/<id>`, then `python3 report/validate.py runs/<id>`, with --mode
// baseline for a run with the limiter off. Each runs on ctx, so a signal stops it, with its
// output in runs/<id>/report.stderr and runs/<id>/validate.stderr. A builder that fails leaves
// nothing to validate. A report that validate.py does not pass (it exits other than 0, a signal
// included) is marked invalid, and the run fails (make run exits 1): `python3 -m report
// --mark-invalid runs/<id>` sets evidence_validity.valid = false (plan choice: the report's one
// validity flag) with the checks validate.py failed, and writes report.json and report.md again
// together, so neither says valid. That step runs even after a signal, bounded by checkTimeout,
// with its output in runs/<id>/mark-invalid.stderr (markReportInvalid).
func (r *runner) report(ctx context.Context) error {
	if _, err := r.python(ctx, nameReport, reportTimeout, "-m", reportModule, r.runPath); err != nil {
		return fmt.Errorf("python3 -m report (see %s%s): %w", nameReport, stderrSuffix, err)
	}
	args := []string{validateScript, r.runPath}
	if !r.in.settings.API.LimiterEnabled {
		args = append(args, "--mode", "baseline")
	}
	out, err := r.python(ctx, nameValidate, checkTimeout, args...)
	if err == nil {
		return nil
	}
	err = fmt.Errorf("%s does not pass the report (see %s%s): %w%s", validateScript, nameValidate, stderrSuffix, err, failedChecks(out))
	return r.markReportInvalid(ctx, err)
}

// markReportInvalid marks the run's report invalid after the failure err of a report step
// (IMPLEMENTATION_PLAN H5 step 7: a failure sets valid: false): `python3 -m report
// --mark-invalid [flags…] runs/<id>` (flags: --freeze for a freeze that did not complete), on a
// ctx that a signal does not end, bounded by checkTimeout, its output in
// runs/<id>/mark-invalid.stderr. It returns err, naming the mark's own failure too when the
// mark fails.
func (r *runner) markReportInvalid(ctx context.Context, err error, flags ...string) error {
	args := append([]string{"-m", reportModule, markInvalid}, flags...)
	if _, markErr := r.python(context.WithoutCancel(ctx), nameInvalid, checkTimeout, append(args, r.runPath)...); markErr != nil {
		return fmt.Errorf("%w; marking the report invalid, python3 %s (see %s%s): %w", err, strings.Join(args, " "), nameInvalid, stderrSuffix, markErr)
	}
	r.logger.Error("the report is not valid", "run_id", r.rd.id, "report", filepath.Join(r.runPath, reportFile))
	return err
}

// failedChecks is the "FAIL <check>: <detail>" lines of validate.py's output, joined, as an
// error's tail: "" when there are none; at most maxFailedChecks bytes of them.
func failedChecks(out []byte) string {
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "FAIL ") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	text := strings.Join(lines, "; ")
	if len(text) > maxFailedChecks {
		cut := maxFailedChecks
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut-- // never inside a character
		}
		text = text[:cut] + "…"
	}
	return ": " + text
}

// freeze freezes a valid baseline run's thresholds (IMPLEMENTATION_PLAN H5 steps 5 and 7; DESIGN
// §B9 step 2): when runs/thresholds/<scenario>-<seed>.json does not exist yet, `python3 -m
// report.thresholds runs/<id> --out <that file>` on ctx, its output in
// runs/<id>/thresholds.stderr. The freeze checks the baseline again (validate.py in baseline
// mode) and creates the file once, mode 0444. A frozen file that exists is never overwritten or
// edited (PRF-7): the step only logs it. A freeze that does not complete (it fails, a signal
// stops it, or ctx ended before it could run) marks the report invalid, `python3 -m report
// --mark-invalid --freeze runs/<id>` (markReportInvalid), and fails the run.
func (r *runner) freeze(ctx context.Context) error {
	out := absPath(r.s.path(fmt.Sprintf("%s/%s-%d.json", thresholdsDir, r.in.scenario.Name, r.in.scenario.Seed)))
	switch _, err := os.Lstat(out); {
	case err == nil:
		r.logger.Info("thresholds already frozen: never overwritten", "run_id", r.rd.id, "path", out)
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return r.markReportInvalid(ctx, err, markFreeze)
	}
	if err := ctx.Err(); err != nil {
		return r.markReportInvalid(ctx, fmt.Errorf("python3 -m %s did not run: %w", freezeModule, err), markFreeze)
	}
	if _, err := r.python(ctx, nameThresholds, checkTimeout, "-m", freezeModule, r.runPath, "--out", out); err != nil {
		return r.markReportInvalid(ctx, fmt.Errorf("python3 -m %s (see %s%s): %w", freezeModule, nameThresholds, stderrSuffix, err), markFreeze)
	}
	r.logger.Info("thresholds frozen", "run_id", r.rd.id, "path", out)
	return nil
}

// python runs `python3 args…` in the repository root on ctx, bounded by timeout, with the plain
// environment (no password, DESIGN §A2), and saves its output in runs/<id>/<name>.stderr, a new
// file: its stderr as it runs, then its stdout, which it also returns.
func (r *runner) python(ctx context.Context, name string, timeout time.Duration, args ...string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(r.runPath, name+stderrSuffix), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := runCommand(ctx, r.s.root, r.envs.plain, f, pythonProgram, args...)
	_, werr := f.Write(out)
	cerr := f.Close()
	if err == nil {
		err = errors.Join(werr, cerr)
	}
	return out, err
}

// absPath is path made absolute: children get absolute paths, whatever their working
// directory. A path that cannot be made absolute is returned as it is (the child then fails on
// it and says so).
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
