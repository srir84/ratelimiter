//go:build rltest

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/runlog"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storeproc"
)

// faultctlFaults is the [[faults]] tables of cmds, each at 1 s, to every instance.
func faultctlFaults(cmds ...string) string {
	var b strings.Builder
	for _, c := range cmds {
		fmt.Fprintf(&b, "\n[[faults]]\nat_s = 1\ncmd = \"%s\"\n", c)
	}
	return b.String()
}

// TestFaultctl_Preflight (IMPLEMENTATION_PLAN H4 step 7; plan choice): the rltest orchestrator
// passes a scenario whose faultctl cmds it can run, each command of faultctl's grammar among
// them, and refuses as faultctl_invalid, before anything starts, each cmd that would fail or
// do nothing mid-run: one that does not parse (an unknown command or tamper operation, a store
// fault over 5 minutes, a clock value that is not a whole number of µs), an api kill of an
// instance the scenario lacks, a clock command on the store clock, and a tamper --epoch (a
// run's epoch is fresh). The interim refusal of every faultctl cmd is gone.
func TestFaultctl_Preflight(t *testing.T) {
	const controlled = "[policy]\nclock_source = \"controlled\"\n"
	for _, c := range []struct {
		name, top, tables string
		want              []string
	}{
		{name: "every command, runnable", top: "instances = 3", tables: controlled + oneStep + faultctlFaults(
			"faultctl store down 4s", "faultctl api kill 3 --restart", "faultctl api kill 1", "faultctl tamper del-ix --key u000001",
			"faultctl tamper corrupt-field --key u000002 --field u --value x", "faultctl tamper wipe", "faultctl tamper del-hwm",
			"faultctl clock set 1791374400000000000", "faultctl clock advance 1000", "faultctl clock back 1000")},
		{name: "store pause", top: "instances = 1", tables: oneStep + faultctlFaults("faultctl store pause 4.5s")},
		{name: "unknown command", tables: oneStep + faultctlFaults("faultctl reboot"), want: []string{reasonFaultctlInvalid}},
		{name: "store down over 5 minutes", tables: oneStep + faultctlFaults("faultctl store down 10m"), want: []string{reasonFaultctlInvalid}},
		{name: "unknown tamper operation", tables: oneStep + faultctlFaults("faultctl tamper scramble --key u1"), want: []string{reasonFaultctlInvalid}},
		{name: "api kill of an instance the scenario lacks", top: "instances = 1", tables: oneStep + faultctlFaults("faultctl api kill 2 --restart"),
			want: []string{reasonFaultctlInvalid}},
		{name: "clock on the store clock", tables: oneStep + faultctlFaults("faultctl clock set 1791374400000000000"),
			want: []string{reasonFaultctlInvalid}},
		{name: "clock value not whole µs", tables: controlled + oneStep + faultctlFaults("faultctl clock advance 1500"),
			want: []string{reasonFaultctlInvalid}},
		{name: "tamper --epoch", tables: oneStep + faultctlFaults("faultctl tamper del-ix --key u1 --epoch 5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f"),
			want: []string{reasonFaultctlInvalid}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestSite(t, testBins{})
			path := filepath.Join(t.TempDir(), "scenario.toml")
			writeFile(t, path, scenario("test", c.top, c.tables), 0o600)
			_, refs := preflight(t.Context(), s, flags{scenario: path})
			var got []string
			for _, ref := range refs {
				got = append(got, ref.reason)
				if !strings.Contains(ref.detail, "faultctl ") {
					t.Errorf("refusal %+v: its detail does not name the cmd", ref)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("refusals %+v, want the reasons %q", refs, c.want)
			}
		})
	}
}

// prekillAPI is a stub instance for api kill: POST /_test/prekill runs before (when set), then
// answers 200 with both drop counts and log_queue_len 0, delay after the call arrived (nothing
// when the call ends first); GET /health answers status ready; it counts each.
type prekillAPI struct {
	addr  string
	delay time.Duration // set before the server serves
	mu    sync.Mutex
	hits  map[string]int // "METHOD /path" → requests
	// before runs when a pre-kill call arrives, before its answer; nil runs nothing.
	before func()
}

func newPrekillAPI(t *testing.T, delay time.Duration) *prekillAPI {
	t.Helper()
	a := &prekillAPI{hits: map[string]int{}, delay: delay}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		a.mu.Lock()
		a.hits[r.Method+" "+r.URL.Path]++
		before := a.before
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /_test/prekill":
			if before != nil {
				before()
			}
			wait := time.NewTimer(a.delay)
			defer wait.Stop()
			select {
			case <-wait.C:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, `{"listener_drops":0,"log_drops":0,"log_queue_len":0}`)
		case "GET /health":
			_, _ = io.WriteString(w, `{"status":"ready"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	a.addr = srv.Listener.Addr().String()
	return a
}

func (a *prekillAPI) count(route string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hits[route]
}

// setBefore makes each later pre-kill call run fn before its answer.
func (a *prekillAPI) setBefore(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.before = fn
}

// pidsOf returns the PIDs runs/<id>/pids.json names, by role and instance ("hitapi 1").
func pidsOf(t *testing.T, r *runner) map[string]int {
	t.Helper()
	p, err := runlog.ReadPIDs(filepath.Join(r.runPath, runlog.PIDsFile))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]int{}
	for _, pr := range p.Procs {
		name := pr.Role
		if pr.Instance != 0 {
			name = fmt.Sprintf("%s %d", pr.Role, pr.Instance)
		}
		m[name] = pr.PID
	}
	return m
}

// runFaultctlCmd runs cmd as a [[faults]] entry at 1 s to instance 1 through the runner.
func runFaultctlCmd(t *testing.T, r *runner, cmd string) faultRecord {
	t.Helper()
	f := config.Fault{AtS: 1, Cmd: cmd, Targets: []int{1}}
	return r.runFaultctl(t.Context(), f, newFaultRecord(f))
}

// runTimedFaultctlCmd is runFaultctlCmd, plus the clock adjustment seen around it
// (clockAdjustment), for checkOutageBounds.
func runTimedFaultctlCmd(t *testing.T, r *runner, cmd string) (faultRecord, int64) {
	t.Helper()
	before := time.Now()
	rec := runFaultctlCmd(t, r, cmd)
	return rec, clockAdjustment(before, time.Now())
}

// clockAdjustment is how far the wall clock moved beyond the monotonic clock between before
// and after, two time.Now readings: the system clock's steps and slews in between (timed on
// macOS slews it by up to a few ms a second and sometimes steps it by tens of ms).
func clockAdjustment(before, after time.Time) int64 {
	return (after.UnixNano() - before.UnixNano()) - after.Sub(before).Nanoseconds()
}

// TestFaultctl_APIKill (IMPLEMENTATION_PLAN H4 steps 2 and 7; DESIGN §B6 FLT-6): the runner
// runs a [[faults]] api kill in-process on an orchestrator child, a stand-in for hitapi 1
// started by the run's own step (startAPIs) and a stub instance's /_test/prekill and /health.
// With --restart: one pre-kill call, a pre_kill line read_ok:true at the end of api-1.jsonl, the
// child killed (SIGKILL) and recorded with the command as its fault, not as an early exit, and
// a new child started by the run's own spec in its place: in the child table and in pids.json
// under its new PID, its output appended to hitapi-1.stderr, and stopped by the run's end
// (stopAPIs: SIGTERM, exit 0) with nothing for judgeStops to fail. The fault's record has
// instance 1, its times, no outage_ns and no targets. Without --restart the killed child leaves
// the table and pids.json, and the run's end has nothing to stop. The pre-kill call may take
// P + 2 s, P the instance's max_release_delay_ns as prepare reads it from the config file the
// instance starts with: with P = 1 s in the run's config, a pre-kill answer 2.5 s after the
// call (past P + 1 s, within P + 2 s = 3 s) is waited for (no error, read_ok:true); and with
// P = 1 s in instance 1's own file (--instance-config) and harness.toml's 5 s in the run's, a
// 4 s kill client is enough and that 2.5 s answer is waited for too. With a kill client whose
// timeout is shorter than P + 2 s nothing is done: no pre-kill call, the child still runs (1 s
// at P = 5 s; 2.5 s, between P + 1 s and P + 2 s, at P = 1 s). A stand-in that exits 0 during
// the pre-kill call, before the SIGKILL, is still restarted or taken out, and recorded as it
// ended (exit 0), but the fault fails, naming that end.
func TestFaultctl_APIKill(t *testing.T) {
	const (
		pOne       = "[instance]\nmax_release_delay_ns = 1_000_000_000\n" // P = 1 s: the pre-kill call may take 3 s
		slow       = 2500 * time.Millisecond                              // past P + 1 s, within P + 2 s, at P = 1 s
		exitedSelf = "ended with exit 0, not with signal: killed"
	)
	for _, c := range []struct {
		name         string
		cmd          string
		tables       string        // the scenario's tables; "" for oneStep, harness.toml's P = 5 s
		ownP         bool          // instance 1 starts with its own config file (--instance-config), P = 1 s
		killTimeout  time.Duration // the kill client's timeout; 0 for main's (newKillClient, 1 min)
		prekillDelay time.Duration // the stub answers the pre-kill call this long after it arrives
		exitsFirst   bool          // the stand-in exits 0 during the pre-kill call, before the SIGKILL
		restarted    bool
		wantErrSub   string
	}{
		{name: "restart", cmd: "faultctl api kill 1 --restart", restarted: true},
		{name: "kill", cmd: "faultctl api kill 1"},
		{name: "kill client too short", cmd: "faultctl api kill 1 --restart", killTimeout: time.Second, wantErrSub: "nothing was done"},
		{name: "slow pre-kill", cmd: "faultctl api kill 1 --restart", tables: pOne + oneStep, prekillDelay: slow, restarted: true},
		{name: "kill client between P + 1 s and P + 2 s", cmd: "faultctl api kill 1 --restart", tables: pOne + oneStep,
			killTimeout: slow, wantErrSub: "nothing was done"},
		{name: "slow pre-kill, the instance's own P", cmd: "faultctl api kill 1 --restart", ownP: true, killTimeout: 4 * time.Second,
			prekillDelay: slow, restarted: true},
		{name: "exits 0 before the kill, restart", cmd: "faultctl api kill 1 --restart", exitsFirst: true, restarted: true, wantErrSub: exitedSelf},
		{name: "exits 0 before the kill", cmd: "faultctl api kill 1", exitsFirst: true, wantErrSub: exitedSelf},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := runTestSite(t)
			var fl flags
			if c.ownP {
				base, err := os.ReadFile(s.path(harnessConfigFile))
				if err != nil {
					t.Fatal(err)
				}
				own, err := config.Merge(base, []byte(pOne))
				if err != nil {
					t.Fatal(err)
				}
				ownPath := filepath.Join(t.TempDir(), "own.toml")
				writeFile(t, ownPath, string(own), 0o600)
				fl.instanceConfigs = map[int]string{1: ownPath}
			}
			r, logs := testRunnerIn(t, s, scenario("test", "instances = 1", c.tables), fl)
			api := newPrekillAPI(t, c.prekillDelay)
			r.apis[0].addr = api.addr
			fake := t.TempDir()
			writeFile(t, filepath.Join(fake, "hitapi"), "#!/bin/sh\ntrap 'exit 0' USR1\n"+exit0OnTERM+"\n", 0o755)
			r.binDir = fake
			r.killClient = newKillClient()
			if c.killTimeout != 0 {
				r.killClient.Timeout = c.killTimeout
			}
			logPath := filepath.Join(r.runPath, "api-1.jsonl")
			writeFile(t, logPath, `{"kind":"lifecycle","event":"start"}`+"\n", 0o600)
			if err := r.startAPIs(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, ch := range r.apiProcs {
					if ch != nil && !ch.exited() {
						ch.stop(5 * time.Second)
					}
				}
			})
			old := r.apiProcs[0]
			waitOutput(t, r.runPath, "hitapi-1", 1)
			wantExit := "signal: killed"
			if c.exitsFirst {
				wantExit = "exit 0"
				api.setBefore(func() { // the stand-in's trap: exit 0, reaped before the call is answered
					_ = syscall.Kill(old.proc.PID, syscall.SIGUSR1)
					old.waitFor(5 * time.Second)
				})
			}

			rec := runFaultctlCmd(t, r, c.cmd)
			if c.wantErrSub == "nothing was done" {
				if !strings.Contains(rec.Error, c.wantErrSub) || api.count("POST /_test/prekill") != 0 || old.exited() || r.apiProcs[0] != old {
					t.Fatalf("record %+v, pre-kill calls %d, child exited %v; want the error %q, no call, the child running",
						rec, api.count("POST /_test/prekill"), old.exited(), c.wantErrSub)
				}
				return
			}
			if (rec.Error == "") != (c.wantErrSub == "") || !strings.Contains(rec.Error, c.wantErrSub) || rec.Instance == nil || *rec.Instance != 1 ||
				rec.StartNs <= 0 || rec.EndNs < rec.StartNs || rec.OutageNs != nil || rec.OutageStartNs != nil || rec.Targets == nil || len(rec.Targets) != 0 {
				t.Fatalf("record %+v, want instance 1, start_ns <= end_ns, no outage_ns or outage_start_ns, no targets, the error %q (none when empty)\n%s",
					rec, c.wantErrSub, logs.String())
			}
			if c.exitsFirst && !strings.Contains(rec.Error, fmt.Sprintf("hitapi-1, process %d, %s", old.proc.PID, exitedSelf)) {
				t.Errorf("record error %q, want it to name the stand-in, process %d, and how it ended", rec.Error, old.proc.PID)
			}
			if n := api.count("POST /_test/prekill"); n != 1 {
				t.Errorf("%d pre-kill calls, want 1", n)
			}
			if !old.exited() || exitText(old.waitErr) != wantExit {
				t.Fatalf("the killed child: exited %v, %s; want ended with %s", old.exited(), exitText(old.waitErr), wantExit)
			}
			lines := bytes.Split(bytes.TrimSpace(readFile(t, logPath)), []byte("\n"))
			var last map[string]any
			if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
				t.Fatal(err)
			}
			if want := map[string]any{"kind": "lifecycle", "event": "pre_kill", "listener_drops": 0.0, "log_drops": 0.0, "read_ok": true}; !mapsEqual(last, want) {
				t.Errorf("api-1.jsonl's last line %v, want %v", last, want)
			}
			wantOld := childRecord{Name: "hitapi-1", Role: runlog.RoleHitAPI, Instance: 1, PID: old.proc.PID, Exit: wantExit, Fault: c.cmd}
			if len(r.rec.Children) != 1 || r.rec.Children[0] != wantOld {
				t.Fatalf("records %+v, want only the killed child's %+v", r.rec.Children, wantOld)
			}

			pids := pidsOf(t, r)
			if !c.restarted {
				if r.apiProcs[0] != nil || len(pids) != 0 || api.count("GET /health") != 0 {
					t.Errorf("after api kill: table %v, pids.json %v, /health reads %d; want the instance out of both, never read", r.apiProcs, pids, api.count("GET /health"))
				}
				r.stopAPIs()
				r.judgeStops()
				if len(r.rec.Children) != 1 || len(r.failure) != 0 {
					t.Errorf("after the run's end: records %+v, failures %q; want nothing stopped, no failure", r.rec.Children, r.failure)
				}
				return
			}
			cur := r.apiProcs[0]
			if cur == nil || cur == old || cur.exited() || pids["hitapi 1"] != cur.proc.PID || cur.proc.PID == old.proc.PID {
				t.Fatalf("after api kill --restart: child %+v, pids.json %v (old %d); want a new running child under its new PID", cur, pids, old.proc.PID)
			}
			if api.count("GET /health") == 0 {
				t.Error("the restarted instance's /health was never read")
			}
			waitOutput(t, r.runPath, "hitapi-1", 2) // both processes' output in one file
			r.stopAPIs()
			r.judgeStops()
			wantNew := childRecord{Name: "hitapi-1", Role: runlog.RoleHitAPI, Instance: 1, PID: cur.proc.PID, Exit: "exit 0",
				GraceNs: r.apis[0].grace.Nanoseconds()}
			if len(r.rec.Children) != 2 || r.rec.Children[1] != wantNew || len(r.failure) != 0 {
				t.Errorf("after the run's end: records %+v, failures %q; want the restarted child's %+v, no failure", r.rec.Children, r.failure, wantNew)
			}
		})
	}
}

// waitOutput waits up to 10 s until runs/<id>/<name>.stderr holds n "ready" lines, one per
// stand-in process that started.
func waitOutput(t *testing.T, dir, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(filepath.Join(dir, name+".stderr"))
		got := bytes.Count(data, []byte("ready"))
		if got == n {
			return
		}
		if got > n || time.Now().After(deadline) {
			t.Fatalf("%s.stderr holds %d ready lines, want %d: %q", name, got, n, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readFile returns the file's content.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mapsEqual compares two decoded JSON objects.
func mapsEqual(a, b map[string]any) bool {
	ja, erra := json.Marshal(a)
	jb, errb := json.Marshal(b)
	return erra == nil && errb == nil && bytes.Equal(ja, jb)
}

// TestFaultctl_Store (IMPLEMENTATION_PLAN H4 step 7; DESIGN §B5 "Faults", §B6 FLT-1 (a),
// FLT-2): the runner runs a [[faults]] store pause and store down in-process on the run's own
// store, started by the run's step (startStore). Each record's start_ns and end_ns are the
// command's own times, the runner's clock readings around it (a paddedClock as r.now, which
// waits 100 ms around each reading), and its outage, outage_ns long from outage_start_ns,
// lasts more than the commanded 1 s and lies inside them (checkOutageBounds), at least those
// 100 ms from each end (checkOutageTimes). store pause 1s measures an outage of more than
// 1,000 ms, at most 5,000 ms, and leaves the same child in the table and pids.json. store down
// 1s, on the store grown to the §A7 size (storeproc.PaddingA7, as TestStoreCtlDownAndPause's
// down_padded), measures one of more than 1,000 ms, at most 5,000 ms, that holds the 1 s and
// the AOF load the store's log shows (checkLoadInOutage); the old child exits 0 after SHUTDOWN
// and is recorded with the command as its fault, not as an early exit; the restarted store,
// started by the run's own spec, is the child in the table and pids.json under its new PID,
// answers PING as rl_ops, appends to valkey-server.stderr, and the run's end shuts it down
// (SHUTDOWN, exit 0). Neither fault leaves the PID file behind, and the instance field stays
// null.
func TestFaultctl_Store(t *testing.T) {
	const d = time.Second // each fault's commanded duration
	r, logs := testRunner(t, scenario("test", "instances = 1", ""), flags{})
	t.Cleanup(func() { stopTestStore(t, r) })
	if err := r.startStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := r.store
	outPath := filepath.Join(r.runPath, nameStore+stderrSuffix)
	checkOutage := func(cmd string, rec faultRecord, adj int64, clk *paddedClock) {
		t.Helper()
		if rec.Error != "" || rec.OutageNs == nil || *rec.OutageNs <= int64(d) || *rec.OutageNs > int64(5*time.Second) ||
			rec.Instance != nil || rec.StartNs <= 0 || rec.EndNs <= rec.StartNs {
			t.Fatalf("%s: record %+v (outage_ns %v), want an outage of more than 1,000 ms, at most 5,000 ms, between start_ns and end_ns, no instance, no error\n%s",
				cmd, rec, rec.OutageNs, logs.String())
		}
		checkOutageBounds(t, cmd, rec, d, adj)
		clk.checkOutageTimes(t, cmd, rec, adj)
		if _, err := os.Stat(filepath.Join(r.runPath, storePIDFile)); !os.IsNotExist(err) {
			t.Errorf("%s: the PID file is still there (%v), want it removed", cmd, err)
		}
	}

	clk := &paddedClock{pad: clockPad}
	r.now = clk.now
	rec, adj := runTimedFaultctlCmd(t, r, "faultctl store pause 1s")
	checkOutage("store pause 1s", rec, adj, clk)
	if r.store != first || pidsOf(t, r)[runlog.RoleValkey] != first.proc.PID || len(r.rec.Children) != 0 {
		t.Fatalf("after store pause: child %v, pids.json %v, records %+v; want the same store, nothing recorded", r.store, pidsOf(t, r), r.rec.Children)
	}

	// The store grows to the §A7 size before its store down, so the restarted store's AOF load
	// takes long enough for the store's log (valkey-server.stderr, which the restarted store
	// appends to; before is its size before the fault) to time it.
	if err := storeproc.LoadPaddingAt(r.storeAddr, r.rd.pw.fixture, storeproc.PaddingA7); err != nil {
		t.Fatal(err)
	}
	before := len(readFile(t, outPath))
	clk = &paddedClock{pad: clockPad}
	r.now = clk.now
	rec, adj = runTimedFaultctlCmd(t, r, "faultctl store down 1s")
	checkOutage("store down 1s", rec, adj, clk)
	checkLoadInOutage(t, "store down 1s", outPath, before, d, *rec.OutageNs, adj)
	cur := r.store
	if cur == nil || cur == first || cur.exited() || pidsOf(t, r)[runlog.RoleValkey] != cur.proc.PID {
		t.Fatalf("after store down: child %+v, pids.json %v; want the restarted store under its new PID", cur, pidsOf(t, r))
	}
	wantOld := childRecord{Name: nameStore, Role: runlog.RoleValkey, PID: first.proc.PID, Exit: "exit 0", Fault: "faultctl store down 1s"}
	if len(r.rec.Children) != 1 || r.rec.Children[0] != wantOld {
		t.Fatalf("records %+v, want only the shut-down store's %+v", r.rec.Children, wantOld)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := pingStore(ctx, r.storeAddr, r.rd.pw.ops); err != nil {
		t.Errorf("PING as rl_ops after store down: %v", err)
	}
	if after := len(readFile(t, outPath)); after <= before {
		t.Errorf("valkey-server.stderr held %d bytes before the restart and %d after, want the restarted store's output appended", before, after)
	}
	if err := r.stopStore(t.Context()); err != nil {
		t.Fatalf("shutting the restarted store down: %v", err)
	}
	wantNew := childRecord{Name: nameStore, Role: runlog.RoleValkey, PID: cur.proc.PID, Exit: "exit 0", GraceNs: storeGrace.Nanoseconds()}
	if len(r.rec.Children) != 2 || r.rec.Children[1] != wantNew || !gone(cur.proc.PID) {
		t.Errorf("records %+v, want the restarted store's %+v after its SHUTDOWN", r.rec.Children, wantNew)
	}
}

// checkOutageBounds checks the record of a store fault commanded for d (DESIGN §B5 "Faults",
// §B6; plan H4 step 7): start_ns and end_ns are the command's own times, and the outage it
// measured, outage_ns long (from SHUTDOWN or SIGSTOP to the first PING answered) from
// outage_start_ns, lasts more than d and at most 5 s and lies inside them. More than d: it
// ends at the first PING answered after SIGCONT or the restart, d after it began, so an
// outage_ns that was not measured, d itself among them, fails. Inside: start_ns <=
// outage_start_ns and outage_start_ns + outage_ns <= end_ns, each within |adj|, the wall
// clock's adjustment the test saw around the fault (clockAdjustment): outage_ns is a
// monotonic length and the others wall-clock readings, so a step or slew of the system clock
// meanwhile moves them apart by that much.
func checkOutageBounds(t *testing.T, cmd string, rec faultRecord, d time.Duration, adj int64) {
	t.Helper()
	if rec.OutageNs == nil || rec.OutageStartNs == nil {
		t.Errorf("%s: record %+v without outage_ns or outage_start_ns", cmd, rec)
		return
	}
	from, length, slack := *rec.OutageStartNs, *rec.OutageNs, max(adj, -adj)
	if length <= int64(d) || from < rec.StartNs-slack || from+length > rec.EndNs+slack || length > int64(5*time.Second) {
		t.Errorf("%s: an outage of outage_ns %d from outage_start_ns %d, start_ns %d, end_ns %d (the wall clock adjusted by %d ns meanwhile): "+
			"want more than the commanded %v and at most 5 s, inside the command's times: start_ns <= outage_start_ns and "+
			"outage_start_ns + outage_ns <= end_ns, within that adjustment",
			cmd, length, from, rec.StartNs, rec.EndNs, adj, d)
	}
}

// The store's log lines that time its AOF load after a store down (Valkey 9.1, the lines
// TestStoreCtlDownAndPause reads in store.log; a run's store logs to its output file): SHUTDOWN
// arriving, then the restarted store's load ("…: 0.165 seconds", from Valkey's wall clock,
// rounded to the ms).
const (
	shutdownLogLine = "User requested shutdown"
	loadLogLine     = "DB loaded from append only file: "
	// loadLogWait bounds the wait for the restarted store to log its load.
	loadLogWait = 10 * time.Second
	// loadResolution is the resolution of the logged load time.
	loadResolution = time.Millisecond
)

// checkLoadInOutage checks the outage, outage ns long, of a store down commanded for d against
// the store's log at path (runs/<id>/valkey-server.stderr, which the restarted store appends
// to) from offset, its size before the fault: the outage ends at the first PING answered after
// the restarted store loaded its AOF (DESIGN §B6 FLT-1 (a): LOADING replies count inside it),
// so it holds d and the load time the log shows (aofLoadTime), within the log's resolution and
// |adj| (clockAdjustment: the load time is a wall-clock length, outage_ns a monotonic one). On a
// store whose load takes long (storeproc.PaddingA7), an outage_ns that was not measured, d or a
// little more, fails.
func checkLoadInOutage(t *testing.T, cmd, path string, offset int, d time.Duration, outage, adj int64) {
	t.Helper()
	load := aofLoadTime(t, path, offset)
	t.Logf("%s: an outage of %v, the AOF load %v", cmd, time.Duration(outage), load)
	if least := int64(d+load-loadResolution) - max(adj, -adj); outage < least {
		t.Errorf("%s: outage_ns %d (%v), want at least %d: the commanded %v plus the AOF load of %v that %s shows (the wall clock adjusted by %d ns meanwhile)",
			cmd, outage, time.Duration(outage), least, d, load, path, adj)
	}
}

// aofLoadTime reads the store's log at path from offset (the lines of one store down) until
// the restarted store has logged its AOF load after the SHUTDOWN line, at most loadLogWait,
// and returns that load time.
func aofLoadTime(t *testing.T, path string, offset int) time.Duration {
	t.Helper()
	limit := time.Now().Add(loadLogWait)
	for {
		data := readFile(t, path)
		if len(data) < offset {
			t.Fatalf("%s has %d bytes, fewer than the %d it had before the fault", path, len(data), offset)
		}
		shutdown := false
		for _, line := range strings.SplitAfter(string(data[offset:]), "\n") {
			_, rest, found := strings.Cut(line, loadLogLine)
			switch {
			case !strings.HasSuffix(line, "\n"): // the last one: empty, or still being written
			case !shutdown:
				shutdown = strings.Contains(line, shutdownLogLine)
			case found:
				secs, ok := strings.CutSuffix(strings.TrimSuffix(rest, "\n"), " seconds")
				load, err := time.ParseDuration(secs + "s")
				if !ok || err != nil || load < 0 {
					t.Fatalf("%s: no load time in %q", path, line)
				}
				return load
			}
		}
		if !time.Now().Before(limit) {
			t.Fatalf("%s: no %q line after %q within %v", path, loadLogLine, shutdownLogLine, loadLogWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// clockPad is how long a paddedClock waits before and after each of its readings.
const clockPad = 100 * time.Millisecond

// paddedClock is a clock for the runner (its now, as r.now) that records each reading and
// waits pad before and after taking it: whatever a faultctl command does between the runner's
// readings around it starts at least pad after the first and ends at least pad before the last.
type paddedClock struct {
	pad time.Duration

	mu    sync.Mutex
	reads []time.Time
}

func (c *paddedClock) now() time.Time {
	time.Sleep(c.pad)
	now := time.Now()
	c.mu.Lock()
	c.reads = append(c.reads, now)
	c.mu.Unlock()
	time.Sleep(c.pad)
	return now
}

// checkOutageTimes checks a store fault's record against the paddedClock that timed it as the
// runner's now: start_ns and end_ns are the command's own times (DESIGN §B5 "Faults": a
// fault's actual start and end), the clock's first and last readings, and the outage,
// outage_ns long from outage_start_ns, lies at least pad inside them, within |adj|
// (clockAdjustment). A start or an end taken from anything else, the outage's own bounds among
// them, or an outage placed at the command's start or end fails.
func (c *paddedClock) checkOutageTimes(t *testing.T, cmd string, rec faultRecord, adj int64) {
	t.Helper()
	c.mu.Lock()
	reads := slices.Clone(c.reads)
	c.mu.Unlock()
	if len(reads) < 2 || rec.StartNs != reads[0].UnixNano() || rec.EndNs != reads[len(reads)-1].UnixNano() {
		t.Errorf("%s: start_ns %d, end_ns %d; want the command's own start and end, the runner's first and last clock readings %v", cmd, rec.StartNs, rec.EndNs, reads)
	}
	if rec.OutageStartNs == nil || rec.OutageNs == nil {
		t.Errorf("%s: record %+v without outage_start_ns or outage_ns", cmd, rec)
		return
	}
	pad, slack := c.pad.Nanoseconds(), max(adj, -adj)
	if head, tail := *rec.OutageStartNs-rec.StartNs, rec.EndNs-(*rec.OutageStartNs+*rec.OutageNs); head < pad-slack || tail < pad-slack {
		t.Errorf("%s: the outage starts %v after start_ns and ends %v before end_ns (the wall clock adjusted by %d ns meanwhile): "+
			"want each at least %v, the clock's wait after its first reading and before its last", cmd, time.Duration(head), time.Duration(tail), adj, c.pad)
	}
}

// waitStorePing waits up to 10 s until r's store answers PING as rl_ops, one try every 20 ms
// within 1 s each, as the run's store step waits for it.
func waitStorePing(t *testing.T, r *runner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := pingStore(ctx, r.storeAddr, r.rd.pw.ops)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store did not answer PING as rl_ops within 10 s: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFaultctl_StoreDownCrash (IMPLEMENTATION_PLAN H4 step 7; DESIGN §B6 FLT-1 (a)): a store
// that ends other than with exit 0 at store down's SHUTDOWN is not passed off as the fault's
// deliberate end. The run's store is a stand-in: a shell that runs valkey-server from the
// run's own command line and, once it has exited, kills itself (SIGKILL), so it dies by a
// signal at SHUTDOWN. store down 1s still restarts the store from the run's own spec as the
// orchestrator's child, in the child table and pids.json under its new PID and answering PING
// as rl_ops, and still measures the outage (more than the commanded 1,000 ms, at most 5,000
// ms, from outage_start_ns inside the command's start_ns and end_ns, checkOutageBounds); the
// stand-in is recorded as it ended, signal: killed, with the command as its fault and not as
// an early exit; and the fault fails, its error naming the stand-in and that end. The run's
// end shuts the restarted store down (exit 0).
func TestFaultctl_StoreDownCrash(t *testing.T) {
	const (
		cmd = "faultctl store down 1s"
		d   = time.Second // its commanded duration
	)
	r, logs := testRunner(t, scenario("test", "instances = 1", ""), flags{})
	t.Cleanup(func() { stopTestStore(t, r) })
	sp := r.storeSpec()
	sp.path, sp.args = "/bin/sh", append([]string{"-c", `"$0" "$@"; kill -KILL $$`, sp.path}, sp.args...)
	standIn, err := startChild(t.Context(), r.runPath, sp)
	if err != nil {
		t.Fatal(err)
	}
	r.store = standIn
	if err := r.addProc(standIn); err != nil {
		t.Fatal(err)
	}
	waitStorePing(t, r)

	rec, adj := runTimedFaultctlCmd(t, r, cmd)
	want := fmt.Sprintf("valkey-server, process %d, ended with signal: killed, not with exit 0", standIn.proc.PID)
	if !strings.Contains(rec.Error, want) || rec.Instance != nil || rec.OutageNs == nil ||
		*rec.OutageNs <= int64(d) || *rec.OutageNs > int64(5*time.Second) {
		t.Fatalf("record %+v (outage_ns %v), want the error %q, no instance, an outage of more than 1,000 ms, at most 5,000 ms\n%s",
			rec, rec.OutageNs, want, logs.String())
	}
	checkOutageBounds(t, cmd, rec, d, adj)
	wantOld := childRecord{Name: nameStore, Role: runlog.RoleValkey, PID: standIn.proc.PID, Exit: "signal: killed", Fault: cmd}
	if len(r.rec.Children) != 1 || r.rec.Children[0] != wantOld {
		t.Fatalf("records %+v, want only the stand-in's %+v", r.rec.Children, wantOld)
	}
	cur := r.store
	if cur == nil || cur == standIn || cur.exited() || pidsOf(t, r)[runlog.RoleValkey] != cur.proc.PID {
		t.Fatalf("after store down: child %+v, pids.json %v; want the restarted store under its new PID", cur, pidsOf(t, r))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := pingStore(ctx, r.storeAddr, r.rd.pw.ops); err != nil {
		t.Errorf("PING as rl_ops after store down: %v", err)
	}
	if err := r.stopStore(t.Context()); err != nil {
		t.Errorf("shutting the restarted store down: %v", err)
	}
	if n := len(r.rec.Children); n != 2 || r.rec.Children[1].PID != cur.proc.PID || r.rec.Children[1].Exit != "exit 0" {
		t.Errorf("records %+v, want the restarted store's after its SHUTDOWN: exit 0", r.rec.Children)
	}
}

// TestFaultctl_TamperClock (IMPLEMENTATION_PLAN H4 step 7; DESIGN §B6 "Controlled time",
// FLT-7, FLT-11): the runner runs [[faults]] clock and tamper commands in-process as
// rl_fixture on the run's store: clock set writes rl:ctl:time in µs, advance and back step from
// the time the store holds; tamper del-ix removes the key's member from the bucket index of the
// run's epoch (context.json's epoch_id) and del-hwm the epoch record's hwm; a deletion that
// finds nothing fails the fault, with its error in the record. Each record has its times, and
// neither an instance nor an outage.
func TestFaultctl_TamperClock(t *testing.T) {
	r, logs := testRunner(t, scenario("test", "instances = 1", "[policy]\nclock_source = \"controlled\"\n"+oneStep), flags{})
	t.Cleanup(func() { stopTestStore(t, r) })
	if err := r.startStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.rec.EpochID = "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f"
	enc, err := keys.Encode("tk")
	if err != nil {
		t.Fatal(err)
	}
	ix := keys.IndexKey(r.rec.EpochID)
	fixture := func(args ...string) string {
		t.Helper()
		var got string
		err := withFixtureConn(t.Context(), r.storeAddr, r.rd.pw.fixture, func(c fixtureConn) error {
			v, err := c.Do(args...)
			got = v.String()
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", args[0], err)
		}
		return got
	}
	fixture("ZADD", ix, "0", enc)
	fixture("HSET", keys.EpochKey, "hwm", "7")

	for _, c := range []struct {
		cmd       string
		failed    bool
		check     []string // a command whose reply, as storeproc.Reply prints it, must be want
		wantReply string
	}{
		{cmd: "faultctl clock set 1791374400000000000", check: []string{"GET", keys.CtlTimeKey}, wantReply: `"1791374400000000"`},
		{cmd: "faultctl clock advance 5000", check: []string{"GET", keys.CtlTimeKey}, wantReply: `"1791374400000005"`},
		{cmd: "faultctl clock back 1000", check: []string{"GET", keys.CtlTimeKey}, wantReply: `"1791374400000004"`},
		{cmd: "faultctl tamper del-ix --key tk", check: []string{"TYPE", ix}, wantReply: "+none"},
		{cmd: "faultctl tamper del-hwm", check: []string{"TYPE", keys.EpochKey}, wantReply: "+none"},
		{cmd: "faultctl tamper del-ix --key tk", failed: true},
	} {
		rec := runFaultctlCmd(t, r, c.cmd)
		if (rec.Error != "") != c.failed || rec.Instance != nil || rec.OutageNs != nil || rec.OutageStartNs != nil || rec.StartNs <= 0 || rec.EndNs < rec.StartNs {
			t.Errorf("%s: record %+v, want failed %v, its times, no instance, no outage\n%s", c.cmd, rec, c.failed, logs.String())
			continue
		}
		if c.check != nil {
			if got := fixture(c.check...); got != c.wantReply {
				t.Errorf("%s: %s replies %s, want %s", c.cmd, strings.Join(c.check, " "), got, c.wantReply)
			}
		}
	}
}
