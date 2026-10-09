//go:build rltest

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/fixtures"
	"ratelimiter/harness/runlog"
)

// The orchestrator's faultctl runner (IMPLEMENTATION_PLAN H4 step 7; DESIGN §B4 [[faults]], §B5
// "Faults", §B6): a scenario's "faultctl …" cmd runs here, in-process, through the functions
// faultctl itself runs (harness/fixtures: ParseFaultCommand, StoreDown, StorePause, KillAPI,
// Tamper.Apply, Clock.Apply). A store or an instance it restarts is the orchestrator's child,
// in the child table and in pids.json: the sampler samples it and the run's end stops it, like
// the process it replaced, whose record names the fault that ended it.
const (
	// reasonFaultctlInvalid is preflight's refusal of a faultctl cmd this build could not run
	// (faultctlRefusals; plan choice for the code).
	reasonFaultctlInvalid = "faultctl_invalid"
	// storePIDFile is the store fault's PID file in the run directory (StoreTarget.PIDFile; plan
	// choice): the store child's PID, written before the fault and removed after it.
	storePIDFile = "valkey-server.pid"
	// prekillSlack is how long past P an api kill's pre-kill call may take: P + 2 s (plan H4
	// step 2).
	prekillSlack = 2 * time.Second
)

// The ends a faultctl command intends for the process it replaces (plan H4 step 7), as
// exitText words them: Valkey exits 0 once it accepted store down's SHUTDOWN, and api kill
// ends hitapi with SIGKILL. A replaced process that ended otherwise (it crashed, or exited on
// its own before the kill) fails the fault (retire).
const (
	exitShutdown = "exit 0"
	exitKilled   = "signal: killed"
)

// faultctlRefusals is the rltest build's check of the scenario's faultctl cmds in preflight
// (buildRefusals; plan choice). It refuses, before anything starts, each cmd that would fail,
// or do nothing, mid-run: one that does not parse as faultctl's command line
// (fixtures.ParseFaultCommand); an api kill of an instance the scenario lacks; a clock command
// on a run whose clock_source is not controlled, whose limiter never reads that time; and a
// tamper --epoch, since a run starts a fresh epoch whose ID no scenario can know (the runner
// tampers in the run's epoch).
func faultctlRefusals(sc config.Scenario, controlled bool) []refusal {
	var refs refusals
	for i, f := range sc.Faults {
		if !f.Faultctl() {
			continue
		}
		cmd, err := parseFaultctl(f.Cmd)
		switch {
		case err != nil:
			refs.add(reasonFaultctlInvalid, "faults[%d].cmd %q: %v", i, f.Cmd, err)
		case cmd.Op == fixtures.OpAPIKill && cmd.Instance > sc.Instances:
			refs.add(reasonFaultctlInvalid, "faults[%d].cmd %q: the scenario has %d instances", i, f.Cmd, sc.Instances)
		case clockOp(cmd.Op) && !controlled:
			refs.add(reasonFaultctlInvalid, "faults[%d].cmd %q: a clock command needs clock_source = \"controlled\"", i, f.Cmd)
		case cmd.Op == fixtures.OpTamper && cmd.Tamper.EpochID != "":
			refs.add(reasonFaultctlInvalid, "faults[%d].cmd %q: the run starts a fresh epoch, whose ID no scenario can know: "+
				"leave --epoch out, the orchestrator tampers in the run's epoch", i, f.Cmd)
		}
	}
	return refs
}

// parseFaultctl parses a [[faults]] cmd that runs faultctl: the words after "faultctl ", split
// at white space, as faultctl parses its own command line (fixtures.ParseFaultCommand).
func parseFaultctl(cmd string) (fixtures.FaultCommand, error) {
	return fixtures.ParseFaultCommand(strings.Fields(strings.TrimPrefix(cmd, config.FaultctlPrefix)))
}

// clockOp reports whether op is a clock command.
func clockOp(op fixtures.FaultOp) bool {
	switch op {
	case fixtures.OpClockSet, fixtures.OpClockAdvance, fixtures.OpClockBack:
		return true
	case fixtures.OpStoreDown, fixtures.OpStorePause, fixtures.OpAPIKill, fixtures.OpTamper:
	}
	return false
}

// runFaultctl runs the faultctl cmd of f at its time (runFault) and returns rec completed: no
// targets; instance N for api kill N, null for the store, tamper and clock commands, which act
// on the shared store; start_ns and end_ns the command's own start and end (DESIGN §B5
// "Faults": its actual start and end), for every command; for a store fault whose outage was
// measured, outage_start_ns, where the outage started (just before SHUTDOWN or SIGSTOP), and
// outage_ns, its length (as faultctl prints them); the error, when the command failed. A
// store down or a pause the run's ctx ends early still brings the store back before it
// returns (fixtures.StoreDown, StorePause).
func (r *runner) runFaultctl(ctx context.Context, f config.Fault, rec faultRecord) faultRecord {
	rec.Instance, rec.Targets = nil, []faultTarget{}
	cmd, err := parseFaultctl(f.Cmd)
	if err != nil { // preflight refused it already (faultctlRefusals)
		rec.Error = err.Error()
		return rec
	}
	if cmd.Op == fixtures.OpAPIKill {
		n := cmd.Instance
		rec.Instance = &n
	}
	start := r.now()
	var out fixtures.Outage
	switch cmd.Op {
	case fixtures.OpStoreDown, fixtures.OpStorePause:
		out, err = r.storeFault(ctx, cmd, f.Cmd)
	case fixtures.OpAPIKill:
		err = r.apiKill(ctx, cmd, f.Cmd)
	case fixtures.OpTamper:
		// The run's epoch (context.json's epoch_id): preflight refuses a tamper --epoch.
		err = withFixtureConn(ctx, r.storeAddr, r.rd.pw.fixture, func(c fixtureConn) error {
			return fixtures.NewTamper(c, r.rec.EpochID).Apply(cmd.Tamper)
		})
	case fixtures.OpClockSet, fixtures.OpClockAdvance, fixtures.OpClockBack:
		err = r.clockStep(ctx, cmd)
	}
	rec.StartNs, rec.EndNs = start.UnixNano(), r.now().UnixNano()
	if out.EndNs != 0 { // the outage was measured: where it started, and how long it lasted
		outageStart, outage := out.StartNs, out.OutageNs
		rec.OutageStartNs, rec.OutageNs = &outageStart, &outage
	}
	if err != nil {
		rec.Error = err.Error()
	}
	return rec
}

// clockStep runs clock set, advance or back on the controlled clock as rl_fixture (DESIGN §B6
// "Controlled time"), as faultctl does: a step starts from the time the store holds
// (Clock.Load), which the run set before its epoch started.
func (r *runner) clockStep(ctx context.Context, cmd fixtures.FaultCommand) error {
	return withFixtureConn(ctx, r.storeAddr, r.rd.pw.fixture, func(c fixtureConn) error {
		clk := fixtures.NewClock(c, nil)
		if cmd.Op != fixtures.OpClockSet {
			if err := clk.Load(); err != nil {
				return err
			}
		}
		return clk.Apply(cmd.Op, cmd.Ns)
	})
}

// storeFault runs store down or store pause on the run's store as rl_ops (FLT-1 a, FLT-2;
// DESIGN §B6) with fixtures.StoreDown or StorePause, on the PID file runs/<id>/valkey-server.pid,
// which holds the store child's PID for the fault's length. store down restarts the store
// through restartStore, a Restart hook that starts it as the orchestrator's child; once the
// store is back, the PID it reports, which StoreDown wrote into the PID file, must be that
// child's. A shut-down store that ended other than with exit 0 still gets its restart and its
// record, and the fault, its outage measured, fails (restartStore).
func (r *runner) storeFault(ctx context.Context, cmd fixtures.FaultCommand, line string) (fixtures.Outage, error) {
	r.mu.Lock()
	st := r.store
	r.mu.Unlock()
	if st == nil || st.exited() {
		return fixtures.Outage{}, errors.New("the store is not running")
	}
	pidFile := filepath.Join(r.runPath, storePIDFile)
	if err := replaceFile(pidFile, []byte(strconv.Itoa(st.proc.PID)+"\n")); err != nil {
		return fixtures.Outage{}, fmt.Errorf("the store's PID file: %w", err)
	}
	defer func() { _ = os.Remove(pidFile) }() // the fault's own file: it names nothing once the fault ends
	t := fixtures.StoreTarget{Addr: r.storeAddr, OpsUser: userOps, OpsPassword: r.rd.pw.ops, PIDFile: pidFile}
	if cmd.Op == fixtures.OpStorePause {
		return fixtures.StorePause(ctx, t, cmd.Duration)
	}
	// ended is the shut-down store's end when it was not SHUTDOWN's: StoreDown calls the hook
	// on this goroutine, and still waits for the restarted store's first PING.
	var ended error
	t.Restart = func(rctx context.Context) (err error) {
		ended, err = r.restartStore(rctx, st, line)
		return err
	}
	out, err := fixtures.StoreDown(ctx, t, cmd.Duration)
	if err != nil {
		return out, errors.Join(ended, err)
	}
	r.mu.Lock()
	cur := r.store
	r.mu.Unlock()
	pid, err := readPIDFile(pidFile)
	switch {
	case err != nil:
		return out, errors.Join(ended, err)
	case cur == nil || cur == st || pid != cur.proc.PID:
		return out, errors.Join(ended, fmt.Errorf("the store at %s is process %d, not the store the orchestrator restarted", r.storeAddr, pid))
	}
	return out, ended
}

// readPIDFile reads the PID in the store fault's PID file (decimal text).
func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading the store's PID file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("the store's PID file %s holds %.40q, not a PID", path, data)
	}
	return pid, nil
}

// restartStore is store down's Restart hook (plan H4 step 7): the store accepted SHUTDOWN and
// its port is closed. It waits up to storeExitWait for that child, old, to exit, records it as
// ended by the fault (retire), and starts the store again from the run's command line
// (storeSpec: the same port, data directory, ACL file and rewrite setting), its output
// appended to valkey-server.stderr, as the orchestrator's child, which takes old's place in the
// child table and in pids.json: the sampler and the run's SHUTDOWN reach it. StoreDown then
// waits for its first PING. err is why the store was not restarted, or not recorded in
// pids.json; ended is old's end when it was not SHUTDOWN's, exit 0 (retire): the store is
// restarted all the same, and the fault fails once StoreDown has measured the outage.
func (r *runner) restartStore(ctx context.Context, old *child, line string) (ended, err error) {
	if !old.waitFor(storeExitWait) {
		return nil, fmt.Errorf("the store, process %d, still ran %v after SHUTDOWN", old.proc.PID, storeExitWait)
	}
	sp := r.storeSpec()
	sp.appendOut = true
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store != old {
		return nil, errors.New("the store's child changed during store down")
	}
	ended = r.retire(old, line, exitShutdown)
	r.store = nil
	c, err := startChild(ctx, r.runPath, sp)
	if err != nil {
		return ended, errors.Join(err, r.setProcLocked(runlog.RoleValkey, 0, 0))
	}
	r.store = c
	r.logger.Info("store restarted", "cmd", line, "old_pid", old.proc.PID, "pid", c.proc.PID)
	return ended, r.setProcLocked(runlog.RoleValkey, 0, c.proc.PID)
}

// apiKill runs api kill N [--restart] on instance N (FLT-6; DESIGN §B6) with fixtures.KillAPI:
// the pre-kill call through the kill client within P + 2 s (P from the instance's config
// file), the pre_kill line in runs/<id>/api-N.jsonl, then SIGKILL; with --restart, a Start
// hook that starts hitapi N again as the orchestrator's child (restartAPI), and the wait for
// its /health to be ready. Without --restart the killed instance leaves the child table and
// pids.json (retireAPI): nothing runs in its place. A killed instance that ended other than by
// the SIGKILL (it exited before it) is still restarted or taken out, and recorded, and the
// fault fails (retire).
func (r *runner) apiKill(ctx context.Context, cmd fixtures.FaultCommand, line string) error {
	n, i := cmd.Instance, cmd.Instance-1
	if i < 0 || i >= len(r.apis) {
		return fmt.Errorf("the run has no instance %d", n)
	}
	var old *child
	r.mu.Lock()
	if i < len(r.apiProcs) {
		old = r.apiProcs[i]
	}
	r.mu.Unlock()
	if old == nil || old.exited() {
		return fmt.Errorf("hitapi %d is not running", n)
	}
	a := r.apis[i]
	t := fixtures.APITarget{
		BaseURL:        "http://" + a.addr,
		PID:            old.proc.PID,
		LogPath:        filepath.Join(r.runPath, fmt.Sprintf("api-%d.jsonl", n)),
		PrekillTimeout: a.p + prekillSlack,
		Client:         r.killClient,
	}
	switch c := r.killClient; {
	case c == nil:
		return fmt.Errorf("hitapi %d: no HTTP client for the kill: nothing was done", n)
	case c.Timeout != 0 && c.Timeout < t.PrekillTimeout:
		return fmt.Errorf("hitapi %d: the pre-kill call may take %v, P + %v, longer than the kill client's %v: nothing was done",
			n, t.PrekillTimeout, prekillSlack, c.Timeout)
	}
	// ended is the killed instance's end when it was not SIGKILL's: KillAPI calls the Start
	// hook on this goroutine, and still waits for the restarted instance to be ready.
	var ended error
	if cmd.Restart {
		t.Start = func(sctx context.Context) (pid int, err error) {
			pid, ended, err = r.restartAPI(sctx, i, old, line)
			return pid, err
		}
	}
	_, err := fixtures.KillAPI(ctx, t, cmd.Restart)
	if !cmd.Restart {
		err = errors.Join(err, r.retireAPI(i, old, line))
	}
	return errors.Join(err, ended)
}

// restartAPI is api kill --restart's Start hook (plan H4 step 7): old, instance i+1's child, got
// SIGKILL and is gone. Once it is reaped (at most reapWait) it records old as ended by the
// fault (retire) and starts hitapi i+1 again from the run's command line (apiSpec: the same
// address, config file, run log and run ID), its output appended to hitapi-N.stderr, as the
// orchestrator's child, which takes old's place in the child table and in pids.json: the
// sampler and the run's end (SIGTERM, then the instance's grace) reach it. It returns the new
// process's PID; err when it was not restarted, or not recorded in pids.json; and ended, old's
// end when it was not SIGKILL's (retire): the instance is restarted all the same, and the
// fault fails once KillAPI has seen it ready.
func (r *runner) restartAPI(ctx context.Context, i int, old *child, line string) (pid int, ended, err error) {
	if !old.waitFor(reapWait) {
		return 0, nil, fmt.Errorf("hitapi %d, process %d, was not reaped %v after SIGKILL", i+1, old.proc.PID, reapWait)
	}
	sp := r.apiSpec(i)
	sp.appendOut = true
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.apiProcs[i] != old {
		return 0, nil, fmt.Errorf("hitapi %d's child changed during api kill", i+1)
	}
	ended = r.retire(old, line, exitKilled)
	r.apiProcs[i] = nil
	c, err := startChild(ctx, r.runPath, sp)
	if err != nil {
		return 0, ended, errors.Join(err, r.setProcLocked(runlog.RoleHitAPI, i+1, 0))
	}
	r.apiProcs[i] = c
	r.logger.Info("instance restarted", "cmd", line, "instance", i+1, "old_pid", old.proc.PID, "pid", c.proc.PID)
	if err = r.setProcLocked(runlog.RoleHitAPI, i+1, c.proc.PID); err != nil {
		return 0, ended, err // the child runs, in the table: the run's end stops it
	}
	return c.proc.PID, ended, nil
}

// retireAPI takes instance i+1's child out of the child table and pids.json once api kill
// without --restart has killed it, recording it as ended by the fault: nothing runs in its
// place, and its PID, which the system may give another process, is not sampled any more. A
// child that has not exited reapWait after KillAPI returned (the kill failed, or never
// happened) stays, for the run's end to stop and judge. A child that ended other than by the
// SIGKILL is taken out all the same, and the error says so (retire).
func (r *runner) retireAPI(i int, old *child, line string) error {
	if !old.waitFor(reapWait) {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.apiProcs[i] != old {
		return nil
	}
	ended := r.retire(old, line, exitKilled)
	r.apiProcs[i] = nil
	return errors.Join(ended, r.setProcLocked(runlog.RoleHitAPI, i+1, 0))
}

// retire records old, a child a faultctl command ended on purpose and that has exited (plan H4
// step 7): stop's record (old reaped, and what is left of its process group killed), with the
// command in fault, and exited_before_stop false, since the orchestrator ended it. The record
// keeps old's own end; when that is not want, the end the command intended (exitShutdown,
// exitKilled), it returns an error that fails the fault, so a process that crashed, or exited
// on its own, is not passed off as the command's deliberate end. r.mu is held.
func (r *runner) retire(old *child, line, want string) error {
	rec := old.stop(0)
	rec.Fault, rec.Early = line, false
	r.record(rec)
	if rec.Exit != want {
		return fmt.Errorf("%s, process %d, ended with %s, not with %s as the command ends it (see %s%s)",
			old.name, rec.PID, rec.Exit, want, old.name, stderrSuffix)
	}
	return nil
}

// setProcLocked sets the PID of role's process (and instance's, hitapi only) in pids.json, or
// with pid 0 takes its entry out, and rewrites the file, which the sampler re-reads on every
// tick: a restarted process is sampled under its new PID. r.mu is held.
func (r *runner) setProcLocked(role string, instance, pid int) error {
	procs := make([]runlog.Proc, 0, len(r.procs))
	found := false
	for _, p := range r.procs {
		if p.Role != role || p.Instance != instance {
			procs = append(procs, p)
			continue
		}
		found = true
		if pid != 0 {
			p.PID = pid
			procs = append(procs, p)
		}
	}
	if !found {
		return fmt.Errorf("%s has no %s %d", runlog.PIDsFile, role, instance)
	}
	r.procs = procs
	if err := runlog.WritePIDs(filepath.Join(r.runPath, runlog.PIDsFile), runlog.PIDs{Procs: r.procs}); err != nil {
		return fmt.Errorf("writing %s: %w", runlog.PIDsFile, err)
	}
	return nil
}
