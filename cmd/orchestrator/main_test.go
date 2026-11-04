//go:build rltest

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/runlog"
)

// TestParseFlags (plan H3 step 8): --scenario is required; --baseline, --bin-dir and
// --instance-config N=FILE, once per instance N from 1 to 3 with a FILE, are parsed; anything
// else is refused.
func TestParseFlags(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want *flags // nil: refused
	}{
		{"scenario only", []string{"--scenario", "s.toml"}, &flags{scenario: "s.toml", storeProfile: "durable"}},
		{"every flag", []string{"--scenario", "s.toml", "--baseline", "--instance-config", "3=x.toml",
			"--instance-config", "1=y.toml", "--bin-dir", "d", "--store-profile", "barrier"},
			&flags{scenario: "s.toml", baseline: true, instanceConfigs: map[int]string{1: "y.toml", 3: "x.toml"}, binDir: "d",
				storeProfile: "barrier"}},
		{"store profile everysec", []string{"--scenario", "s.toml", "--store-profile", "everysec"},
			&flags{scenario: "s.toml", storeProfile: "everysec"}},
		{"store profile buffered", []string{"--scenario", "s.toml", "--store-profile", "buffered"},
			&flags{scenario: "s.toml", storeProfile: "buffered"}},
		{"unknown store profile", []string{"--scenario", "s.toml", "--store-profile", "fast"}, nil},
		{"empty store profile", []string{"--scenario", "s.toml", "--store-profile", ""}, nil},
		{"no --scenario", []string{"--baseline"}, nil},
		{"empty --scenario", []string{"--scenario", ""}, nil},
		{"positional argument", []string{"--scenario", "s.toml", "extra"}, nil},
		{"unknown flag", []string{"--scenario", "s.toml", "--bogus"}, nil},
		{"instance 0", []string{"--scenario", "s.toml", "--instance-config", "0=x.toml"}, nil},
		{"instance 4", []string{"--scenario", "s.toml", "--instance-config", "4=x.toml"}, nil},
		{"instance named twice", []string{"--scenario", "s.toml", "--instance-config", "3=x.toml", "--instance-config", "3=y.toml"}, nil},
		{"no file after the =", []string{"--scenario", "s.toml", "--instance-config", "3="}, nil},
		{"no =", []string{"--scenario", "s.toml", "--instance-config", "3"}, nil},
		{"no instance number", []string{"--scenario", "s.toml", "--instance-config", "x=y.toml"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFlags(c.args, io.Discard)
			switch {
			case c.want == nil && err == nil:
				t.Errorf("parseFlags(%q) = %+v, want an error", c.args, got)
			case c.want != nil && err != nil:
				t.Errorf("parseFlags(%q): %v", c.args, err)
			case c.want != nil && (got.scenario != c.want.scenario || got.baseline != c.want.baseline ||
				got.binDir != c.want.binDir || !maps.Equal(got.instanceConfigs, c.want.instanceConfigs) ||
				got.storeProfile != c.want.storeProfile):
				t.Errorf("parseFlags(%q) = %+v, want %+v", c.args, got, *c.want)
			}
		})
	}
	if _, err := parseFlags([]string{"--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("parseFlags(--help) error %v, want flag.ErrHelp", err)
	}
}

// TestOrchestrate_PIDFile (plan H3 step 4): a run that passes preflight replaces a stale pid
// file (one naming a dead process) with its own and removes it at exit. The test site's store
// port is not harness.toml's 6390, so the run sequence fails at its first check, exit 1, once
// runs/<id>/ exists, and records the failure in context.json.
func TestOrchestrate_PIDFile(t *testing.T) {
	s := newTestSite(t, testBins{})
	writeFile(t, s.path(pidFileName), strconv.Itoa(deadPID(t))+"\n", 0o600)
	path := filepath.Join(t.TempDir(), "scenario.toml")
	writeFile(t, path, scenario("test", "", ""), 0o600)
	var stderr bytes.Buffer
	if code := orchestrate(t.Context(), []string{"--scenario", path}, s, nil, os.Getpid(), testNow, &stderr); code != exitFailure {
		t.Fatalf("exit code %d, want %d (the store address check)\nstderr:\n%s", code, exitFailure, stderr.String())
	}
	// testNow is 2026-10-08T14:22:33Z; the scenario is pf, seed 1.
	dir := filepath.Join(s.root, "runs", "20261008-142233-pf-1")
	if _, err := os.Stat(filepath.Join(dir, configFile)); err != nil {
		t.Errorf("the run directory was not created: %v\nstderr:\n%s", err, stderr.String())
	}
	var rc struct {
		Outcome, Error string
	}
	if data, err := os.ReadFile(filepath.Join(dir, "context.json")); err != nil {
		t.Errorf("no context.json after the failed run: %v", err)
	} else if err := json.Unmarshal(data, &rc); err != nil || rc.Outcome != "failed" || !strings.Contains(rc.Error, "127.0.0.1:6390") {
		t.Errorf("context.json outcome %q, error %q (%v); want failed, naming config.toml's store address 127.0.0.1:6390",
			rc.Outcome, rc.Error, err)
	}
	if data, err := os.ReadFile(s.path(pidFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pid file left behind after the run (%q, %v), want none", data, err)
	}
}

// TestOrchestrate_PIDFileHeld (IMPLEMENTATION_PLAN H3 steps 1 and 4): a run that starts while
// another holds the pid file, still empty because that run has not written its PID yet, passes
// preflight (an empty file is stale by its content) and is then refused as
// orchestrator_running, exit 2, with no run directory and the file left as it was.
func TestOrchestrate_PIDFileHeld(t *testing.T) {
	s := newTestSite(t, testBins{})
	writeFile(t, s.path(pidFileName), "", 0o600)
	flockHeld(t, s.path(pidFileName))
	path := filepath.Join(t.TempDir(), "scenario.toml")
	writeFile(t, path, scenario("test", "", ""), 0o600)
	var stderr bytes.Buffer
	if code := orchestrate(t.Context(), []string{"--scenario", path}, s, nil, os.Getpid(), testNow, &stderr); code != exitRefused {
		t.Fatalf("exit code %d, want %d\nstderr:\n%s", code, exitRefused, stderr.String())
	}
	if got := refusalReasons(t, stderr.String()); !slices.Equal(got, []string{reasonRunning}) {
		t.Errorf("refusal reasons %q, want [%s]\nstderr:\n%s", got, reasonRunning, stderr.String())
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
	if data, err := os.ReadFile(s.path(pidFileName)); err != nil || len(data) != 0 {
		t.Errorf("the held pid file became %q (%v), want it left empty", data, err)
	}
}

// noneIgnored is signal.Ignored for a process that ignores no signal.
func noneIgnored(os.Signal) bool { return false }

// TestOrchestrate_StopSignals (IMPLEMENTATION_PLAN H3 step 4; H4 step 7, the orchestrator
// decision on SIGQUIT): in a process that started with none of them ignored
// (TestOrchestrate_StopSignalsIgnored covers the other case), SIGINT, SIGTERM, SIGHUP and
// SIGQUIT stop a run, each by ending the ctx main gives it (the teardown, exit 1). A SIGHUP
// sent to this process while those signals are caught, as main catches them, ends that ctx
// instead of killing the process. A SIGQUIT sent while an in-process store pause has the run's
// store stopped (ps state T) ends the run as the others do: the pause's recovery sends SIGCONT,
// so the store runs again (a state other than T) before the teardown, which shuts it down
// (SHUTDOWN: exit 0, not killed); the run exits 1, context.json's outcome failed, and the
// pause's record shows it cut short (an error, an outage under its 4.5 s).
func TestOrchestrate_StopSignals(t *testing.T) {
	got := stopSignals(noneIgnored)
	for _, want := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		if !slices.Contains(got, want) {
			t.Fatalf("stop signals %v, want %v among them", got, want)
		}
	}
	if len(got) != 4 {
		t.Errorf("stop signals %v, want SIGINT, SIGTERM, SIGHUP and SIGQUIT only", got)
	}
	ctx, stop := signal.NotifyContext(t.Context(), got...)
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	wait := time.NewTimer(10 * time.Second)
	defer wait.Stop()
	select {
	case <-ctx.Done():
	case <-wait.C:
		t.Fatal("a SIGHUP did not end the run's ctx within 10 s")
	}

	t.Run("SIGQUIT during a store pause", func(t *testing.T) {
		const pauseCmd = "faultctl store pause 4.5s"
		r, logs := testRunner(t, scenario("test", "instances = 1", oneStep+"\n[[faults]]\nat_s = 0.1\ncmd = \""+pauseCmd+"\"\n"), flags{})
		t.Cleanup(func() { stopTestStore(t, r) })
		if err := r.startStore(t.Context()); err != nil {
			t.Fatal(err)
		}
		store := r.store
		t.Cleanup(func() { // registered last, so it runs first: a failed test never leaves the store stopped
			if !store.exited() {
				_ = syscall.Kill(store.proc.PID, syscall.SIGCONT)
			}
		})
		r.apis[0].grace = 5 * time.Second
		// The instance takes 300 ms to exit on SIGTERM, so the store runs that long after the
		// pause's recovery before the teardown reaches it.
		r.apiProcs = []*child{startStandIn(t, r.runPath, "hitapi-1", runlog.RoleHitAPI, 1, exitOnTERM, nil)}
		r.sampler = startStandIn(t, r.runPath, "sampler", runlog.RoleSampler, 0, exit0OnTERM, nil)
		r.countRows = func(context.Context) (int64, error) { return 0, nil } // a run that did reach its end (the test failed)
		t0 := newT0Watch()
		startHitgenStandIn(t, r, t0, hitgenLong)
		ctx, stop := signal.NotifyContext(t.Context(), stopSignals(noneIgnored)...)
		defer stop()

		// The watcher: once the store is stopped, SIGQUIT to this process, caught as main
		// catches it; then the store must run again. Each wait is bounded.
		type watch struct {
			stopped, resumed bool
			err              error
		}
		watched := make(chan watch, 1)
		go func() {
			var w watch
			defer func() { watched <- w }()
			for deadline := time.Now().Add(3 * time.Second); !w.stopped && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				w.stopped = procState(t, store.proc.PID) == "T"
			}
			if !w.stopped {
				return
			}
			if w.err = syscall.Kill(os.Getpid(), syscall.SIGQUIT); w.err != nil {
				return
			}
			for deadline := time.Now().Add(5 * time.Second); !w.resumed && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				st := procState(t, store.proc.PID)
				w.resumed = st != "" && st != "T"
			}
		}()
		code := r.afterHitgen(ctx, t0)
		w := <-watched
		if !w.stopped || w.err != nil || !w.resumed {
			t.Errorf("store stopped by the pause %v, SIGQUIT sent %v (%v), store running again after it %v; want all three",
				w.stopped, w.stopped && w.err == nil, w.err, w.resumed)
		}
		if code != exitFailure {
			t.Errorf("exit code %d after the SIGQUIT, want %d\n%s", code, exitFailure, logs.String())
		}
		rc := readContext(t, r.runPath)
		if rc.Outcome != "failed" || !strings.Contains(rc.Error, "the run was stopped") {
			t.Errorf("context.json outcome %q, error %q; want failed: the run was stopped", rc.Outcome, rc.Error)
		}
		if len(rc.Faults) != 1 || rc.Faults[0].Cmd != pauseCmd || rc.Faults[0].Error == "" || rc.Faults[0].OutageNs == nil ||
			*rc.Faults[0].OutageNs >= int64(4500*time.Millisecond) {
			t.Errorf("context.json faults %+v, want the pause's record cut short: an error, an outage_ns under 4,500 ms", rc.Faults)
		}
		var storeRec []childRecord
		for _, rec := range rc.Children {
			if rec.Name == nameStore {
				storeRec = append(storeRec, rec)
			}
		}
		if len(storeRec) != 1 || storeRec[0].PID != store.proc.PID || storeRec[0].Exit != "exit 0" || storeRec[0].Killed ||
			storeRec[0].Early || storeRec[0].Error != "" || !gone(store.proc.PID) {
			t.Errorf("the store's records %+v, want one: process %d shut down by the teardown, exit 0, not killed", storeRec, store.proc.PID)
		}
	})
}

// procState returns the first letter of process pid's state as ps shows it (T: stopped), or
// "" when ps shows no such process. ps runs within 5 s.
func procState(t *testing.T, pid int) string {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	state := strings.TrimSpace(string(out))
	if err != nil || state == "" {
		return ""
	}
	return state[:1]
}

// TestOrchestrate_StopSignalsIgnored (IMPLEMENTATION_PLAN H3 step 4, H4 step 7; HARNESS HAR-5): a
// SIGINT or SIGHUP the orchestrator started with ignored (nohup ignores SIGHUP; a shell ignores
// SIGINT for a background job) is not caught, so it stays ignored; SIGTERM and SIGQUIT always
// are (Go's runtime replaces an inherited SIGQUIT ignore with its own handler, so
// signal.Ignored never reports SIGQUIT and the table has no such row). A process started
// under nohup, as `nohup make run …` starts the orchestrator, is checked end to end: this test
// binary, re-run under /usr/bin/nohup (TestMain, ignoredHUPCheck), catches the signals main
// catches and sends itself SIGHUP, which must neither end the run's ctx nor kill it.
func TestOrchestrate_StopSignalsIgnored(t *testing.T) {
	for _, c := range []struct {
		ignored []os.Signal
		want    []os.Signal
	}{
		{[]os.Signal{syscall.SIGHUP}, []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT}},
		{[]os.Signal{syscall.SIGINT}, []os.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}},
		{[]os.Signal{syscall.SIGINT, syscall.SIGHUP}, []os.Signal{syscall.SIGTERM, syscall.SIGQUIT}},
		{[]os.Signal{syscall.SIGTERM}, []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}},
	} {
		got := stopSignals(func(sig os.Signal) bool { return slices.Contains(c.ignored, sig) })
		if len(got) != len(c.want) || slices.ContainsFunc(c.want, func(sig os.Signal) bool { return !slices.Contains(got, sig) }) {
			t.Errorf("stop signals with %v ignored: %v, want %v", c.ignored, got, c.want)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	// -test.run=^$: were TestMain not to run the check, the re-run would run no test either.
	cmd := exec.CommandContext(ctx, "/usr/bin/nohup", os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), ignoredHUPEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte(ignoredHUPPassed)) {
		t.Errorf("the check in a process started under nohup: %v, want exit 0 after %q; its output:\n%s", err, ignoredHUPPassed, out)
	}
}

// ignoredHUPEnv makes this test binary run ignoredHUPCheck in place of the tests (TestMain):
// TestOrchestrate_StopSignalsIgnored re-runs it so, under nohup.
const ignoredHUPEnv = "RL_ORCH_TEST_IGNORED_HUP"

// ignoredHUPPassed is what ignoredHUPCheck prints once every check passed.
const ignoredHUPPassed = "an ignored SIGHUP stays ignored"

// mainSignalWaits are TestMainStopSignals's waits from the signal to the scenario's write: the
// first try's, then each retry's.
var mainSignalWaits = []time.Duration{300 * time.Millisecond, 1200 * time.Millisecond, 4800 * time.Millisecond}

const (
	// mainFIFOWait bounds the wait for a child to open its scenario, and mainExitWait the wait
	// for its exit once the scenario is written.
	mainFIFOWait = 10 * time.Second
	mainExitWait = 10 * time.Second
	// mainScenario is the scenario a child reads: it does not load (no seed, run_kind, steps or
	// users), so preflight returns at once, before it reads harness.toml or probes a port.
	mainScenario = "name = \"stopped\"\n"
)

// mainEnv makes this test binary run main on its own command line in place of the tests
// (TestMain): TestMainStopSignals starts it so.
const mainEnv = "RL_ORCH_TEST_MAIN"

// TestMainStopSignals (IMPLEMENTATION_PLAN H3 step 4): main itself catches SIGHUP, SIGQUIT,
// SIGTERM and SIGINT, each ending the ctx it gives orchestrate. A child process runs main
// (this test binary, TestMain) as orchestrator --scenario <FIFO>, in its own temporary
// directory: main resolves harness.toml, bin/, .data/, runs/ and the pid file there, from
// ".". Preflight blocks reading the FIFO, after main has caught its signals and before
// anything else; once the test's open of the FIFO for writing succeeds (only a reader lets it),
// the test signals the child (its own process, never a group), waits, then writes a scenario
// that does not load. The child must exit 1, not killed by the signal, with "stopped during
// preflight" on stderr, leaving its directory as it was: the signal ended the ctx before
// preflight returned. A child that exits 2 with the scenario refusal instead had the signal
// handled only after the scenario was read: that case is tried again with a longer wait, at
// most twice. A death by the signal (its default action, as for SIGHUP) or Go's SIGQUIT dump
// and exit 2 fails at once. Each child starts with SIGHUP and SIGINT at their default action,
// whatever this process inherited (go test under nohup ignores SIGHUP, a shell's background
// job SIGINT, and an ignored signal stays ignored across exec, where main would leave it out):
// while this process catches them, exec gives a child the default action. Go's runtime never
// keeps an inherited ignore of SIGQUIT or SIGTERM. One more child starts through /usr/bin/nohup,
// so SIGHUP is ignored when main starts (`nohup make run`), and main must leave it so: sent
// SIGHUP, it waits as long as the longest try above, then the scenario is written, and the
// child must exit 2 with the scenario's refusal as its only output, neither stopped nor killed
// by the signal. That case is never tried again: a late signal would give the same outcome.
func TestMainStopSignals(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, syscall.SIGHUP, syscall.SIGINT)
	t.Cleanup(func() { signal.Stop(caught) }) // once the parallel subtests have ended too
	for _, c := range []struct {
		name string
		sig  syscall.Signal
	}{
		{"SIGHUP", syscall.SIGHUP},
		{"SIGQUIT", syscall.SIGQUIT},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGINT", syscall.SIGINT},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for try, wait := range mainSignalWaits {
				r := runMainSignalled(t, exe, c.sig, wait, false)
				ws, _ := r.state.Sys().(syscall.WaitStatus)
				stopped := strings.Contains(r.stderr, `"msg":"stopped during preflight"`)
				late := !ws.Signaled() && ws.ExitStatus() == exitRefused && scenarioRefused(r.stderr)
				switch {
				case !ws.Signaled() && ws.ExitStatus() == exitFailure && stopped:
					if len(r.left) != 1 || r.left[0] != "scenario.toml" {
						t.Errorf("main's directory after it stopped holds %q, want only its scenario.toml", r.left)
					}
					return
				case late && try < len(mainSignalWaits)-1:
					t.Logf("main, sent %s %v before its scenario was written, exited 2 with the scenario refusal: the signal "+
						"was handled after the scenario was read; trying again with a longer wait", c.name, wait)
				default:
					t.Fatalf("main, sent %s %v before its scenario was written: %v, stderr:\n%s\nwant exit 1, not killed by the "+
						"signal, after \"stopped during preflight\"", c.name, wait, r.state, r.stderr)
				}
			}
		})
	}
	t.Run("SIGHUP ignored at startup", func(t *testing.T) {
		t.Parallel()
		wait := mainSignalWaits[len(mainSignalWaits)-1]
		r := runMainSignalled(t, exe, syscall.SIGHUP, wait, true)
		ws, _ := r.state.Sys().(syscall.WaitStatus)
		if ws.Signaled() || ws.ExitStatus() != exitRefused || !onlyScenarioRefused(r.stderr) {
			t.Fatalf("main, started under nohup and sent SIGHUP %v before its scenario was written: %v, stderr:\n%s\nwant exit 2 "+
				"with only the scenario refusal: the ignored SIGHUP neither stops the run nor kills it", wait, r.state, r.stderr)
		}
		if len(r.left) != 1 || r.left[0] != "scenario.toml" {
			t.Errorf("main's directory after it refused the scenario holds %q, want only its scenario.toml", r.left)
		}
	})
}

// onlyScenarioRefused reports whether stderr is one line, the orchestrator's refusal of a
// scenario that does not load (scenarioRefused): nothing else, "stopped during preflight"
// included.
func onlyScenarioRefused(stderr string) bool {
	return !strings.Contains(strings.TrimSpace(stderr), "\n") && scenarioRefused(stderr)
}

// scenarioRefused reports whether stderr holds only the orchestrator's JSON log lines, the
// refusal of a scenario that does not load among them: not Go's SIGQUIT dump, nor a race report.
func scenarioRefused(stderr string) bool {
	refused := false
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var rec struct{ Msg, Reason string }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return false
		}
		refused = refused || (rec.Msg == "refusing the run" && rec.Reason == reasonScenario)
	}
	return refused
}

// mainRun is how a child running main ended: its state, its stderr and what its directory
// held then.
type mainRun struct {
	state  *os.ProcessState
	stderr string
	left   []string
}

// runMainSignalled runs main in a child process (TestMainStopSignals), started through
// /usr/bin/nohup when nohup is true, sends it sig once it reads its scenario FIFO, waits for
// wait, then writes mainScenario and waits for the child's exit. Every wait is bounded; a child
// still running at the end of the test is killed.
func runMainSignalled(t *testing.T, exe string, sig syscall.Signal, wait time.Duration, nohup bool) mainRun {
	t.Helper()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "scenario.toml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// The child is killed once the test ends, if it still runs then; its stderr is read while it
	// may still write, on a failure.
	stderr := &lockedBuffer{}
	cmd := exec.CommandContext(t.Context(), exe, "--scenario", fifo)
	if nohup { // nohup ignores SIGHUP, then execs main in its own process: the pid stays
		cmd = exec.CommandContext(t.Context(), "/usr/bin/nohup", exe, "--scenario", fifo)
	}
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", mainEnv + "=1"}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // its outcome is cmd.ProcessState, read once exited is closed
		close(exited)
	}()
	t.Cleanup(func() { <-exited }) // t.Context() has ended by then, so the child was killed
	f := openFIFOWriter(t, fifo, exited)
	if f == nil {
		select {
		case <-exited:
			t.Fatalf("main exited before it opened its scenario: %v, stderr:\n%s", cmd.ProcessState, stderr.String())
		default:
			t.Fatalf("main did not open its scenario within %v", mainFIFOWait)
		}
	}
	if err := syscall.Kill(cmd.Process.Pid, sig); err != nil {
		_ = f.Close() // the test ends here either way
		t.Fatalf("sending %v to main's process %d: %v", sig, cmd.Process.Pid, err)
	}
	timer := time.NewTimer(wait)
	select {
	case <-exited: // ended by the signal: the checks report how
	case <-timer.C:
	}
	timer.Stop()
	_, _ = f.Write([]byte(mainScenario)) // fails only when the child has gone: its exit tells
	_ = f.Close()                        // the child reads to the end of the file
	timer = time.NewTimer(mainExitWait)
	defer timer.Stop()
	select {
	case <-exited:
	case <-timer.C:
		t.Fatalf("main did not exit within %v of reading its scenario; stderr so far:\n%s", mainExitWait, stderr.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	return mainRun{state: cmd.ProcessState, stderr: stderr.String(), left: left}
}

// openFIFOWriter opens the FIFO at path for writing once a reader has it open: O_NONBLOCK, so
// the open fails (ENXIO) instead of waiting while there is none. It tries every millisecond, at
// most mainFIFOWait, and returns nil when that passes or exited closes first.
func openFIFOWriter(t *testing.T, path string, exited <-chan struct{}) *os.File {
	t.Helper()
	for limit := time.Now().Add(mainFIFOWait); time.Now().Before(limit); time.Sleep(time.Millisecond) {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return f
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("opening the scenario FIFO for writing: %v", err)
		}
		select {
		case <-exited:
			return nil
		default:
		}
	}
	return nil
}

// TestMain runs the tests; ignoredHUPCheck in the re-run TestOrchestrate_StopSignalsIgnored
// starts under nohup; or main, on its own command line, in the children TestMainStopSignals
// starts.
func TestMain(m *testing.M) {
	if os.Getenv(ignoredHUPEnv) == "1" {
		os.Exit(ignoredHUPCheck())
	}
	if os.Getenv(mainEnv) == "1" {
		main() // it exits
	}
	os.Exit(m.Run())
}

// ignoredHUPCheck runs in a process started with SIGHUP ignored: SIGHUP must be left out of
// the stop signals (stopSignals(signal.Ignored), as main catches them) and still be ignored once
// they are caught; a SIGHUP the process sends itself must neither end the run's ctx within
// 300 ms nor kill it. It prints ignoredHUPPassed and returns 0, or prints what failed and
// returns 1.
func ignoredHUPCheck() int {
	fail := func(format string, args ...any) int {
		_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
		return 1
	}
	if !signal.Ignored(syscall.SIGHUP) {
		return fail("SIGHUP is not ignored at the start, as nohup leaves it")
	}
	sigs := stopSignals(signal.Ignored)
	if slices.Contains(sigs, os.Signal(syscall.SIGHUP)) {
		return fail("stop signals %v with SIGHUP ignored at the start, want SIGHUP left out", sigs)
	}
	ctx, stop := signal.NotifyContext(context.Background(), sigs...)
	defer stop()
	if !signal.Ignored(syscall.SIGHUP) {
		return fail("SIGHUP is no longer ignored once the stop signals are caught")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		return fail("sending SIGHUP: %v", err)
	}
	wait := time.NewTimer(300 * time.Millisecond)
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return fail("an ignored SIGHUP ended the run's ctx")
	case <-wait.C:
	}
	_, _ = fmt.Fprintln(os.Stdout, ignoredHUPPassed)
	return 0
}
