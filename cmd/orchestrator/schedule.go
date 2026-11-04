package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
)

const (
	// sendingMsg is the msg of hitgen's log line written right before its t0 (cmd/hitgen: the
	// "sending" line, then the sender starts and takes t0).
	sendingMsg = "sending"
	// faultPostTimeout bounds one /_test/faults request (plan choice: arming is immediate).
	faultPostTimeout = 10 * time.Second
	// notRunHitgen is the error of a fault or rewrite whose time had not come when hitgen exited
	// or the run stopped.
	notRunHitgen = "not run: hitgen exited, or the run stopped, before its time"
	// notRunNoT0 is the error of every fault and rewrite of a run whose hitgen never started
	// sending.
	notRunNoT0 = "not run: hitgen never started sending"
)

// t0Watch catches hitgen's "sending" line as the orchestrator copies hitgen's output (plan
// choice; H2's review): hitgen writes it right before it takes its t0, which comes about 0.9 s
// after the process starts on an S1-sized schedule. The [[faults]] at_s and the
// forced_aof_rewrites_s offsets count from the moment the line is read, not from the process
// start.
type t0Watch struct {
	once sync.Once
	seen chan struct{} // closed once the line was read
	at   time.Time     // when it was read (monotonic); set before seen closes
}

func newT0Watch() *t0Watch { return &t0Watch{seen: make(chan struct{})} }

// line reads one line of hitgen's output: a JSON log record whose msg is "sending" marks t0.
func (w *t0Watch) line(b []byte) {
	if len(b) == 0 || b[0] != '{' {
		return
	}
	var rec struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(b, &rec) != nil || rec.Msg != sendingMsg {
		return
	}
	w.once.Do(func() {
		w.at = time.Now()
		close(w.seen)
	})
}

// faultRecord is one [[faults]] entry as context.json records it (DESIGN §B5 "Faults"; plan H3
// step 3): its offset and cmd, the instance it named (null when it went to every instance),
// the wall-clock ns when the orchestrator began sending it and when every target had answered,
// and each target's answer. A fault that did not run has an error and no times. A faultctl
// cmd's record (plan H4 step 7, runFaultctl) has no targets; its instance is the one an api
// kill names (null for the store, tamper and clock commands, which act on the shared store);
// its times are the command's own start and end, as faultctl prints them. A store fault whose
// outage was measured adds outage_ns, the outage's length (DESIGN §B6: from SHUTDOWN or
// SIGSTOP to the first PING answered, on the monotonic clock), and outage_start_ns (plan
// choice), the wall-clock ns just before SHUTDOWN or SIGSTOP, which places it; both are null
// for every other fault.
type faultRecord struct {
	AtS           float64       `json:"at_s"`
	Cmd           string        `json:"cmd"`
	Instance      *int          `json:"instance"`
	StartNs       int64         `json:"start_ns"`
	EndNs         int64         `json:"end_ns"`
	OutageStartNs *int64        `json:"outage_start_ns"`
	OutageNs      *int64        `json:"outage_ns"`
	Targets       []faultTarget `json:"targets"`
	Error         string        `json:"error,omitempty"`

	faultctl bool // Cmd runs faultctl (config.Fault.Faultctl)
}

// faultTarget is one instance's answer to a fault: its HTTP status, or why there was none.
type faultTarget struct {
	Instance int    `json:"instance"`
	Status   int    `json:"status"`
	Error    string `json:"error,omitempty"`
}

// sent reports whether every target of the fault armed it (200).
func (f faultRecord) sent() bool {
	if f.StartNs == 0 || len(f.Targets) == 0 {
		return false
	}
	for _, t := range f.Targets {
		if t.Status != http.StatusOK || t.Error != "" {
			return false
		}
	}
	return true
}

// rewriteRecord is one forced rewrite as context.json's rewrites records it (plan H3 step 3):
// its offset, the wall-clock ns just before BGREWRITEAOF and when INFO persistence showed it
// ended, and aof_last_bgrewrite_status then, or "joined" when BGREWRITEAOF found a rewrite
// already running, whose window it records (forceRewrite). A status other than ok comes with
// an error.
type rewriteRecord struct {
	AtS     float64 `json:"at_s"`
	StartNs int64   `json:"start_ns"`
	EndNs   int64   `json:"end_ns"`
	Status  string  `json:"status,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// offset is a scenario offset in seconds as a duration, rounded to whole ns (the scenario
// loader keeps offsets finite and inside the run, at most a day).
func offset(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }

// newFaultRecord is f's record before it runs.
func newFaultRecord(f config.Fault) faultRecord {
	rec := faultRecord{AtS: f.AtS, Cmd: f.Cmd, faultctl: f.Faultctl()}
	if len(f.Targets) == 1 {
		n := f.Targets[0]
		rec.Instance = &n
	}
	return rec
}

// drive runs the faults and forced rewrites while hitgen sends (DESIGN §B5; plan H3 step 3):
// it waits for hitgen's t0, then fires each at its offset from t0 until hitgen exits or ctx
// ends; those not yet due then are recorded as not run, and those running finish first. The
// records go into context.json.
func (r *runner) drive(ctx context.Context, t0 *t0Watch) {
	sc := r.in.scenario
	select {
	case <-t0.seen:
	case <-r.hitgen.done:
		r.rec.Faults, r.rec.Rewrites = notRunFaults(sc.Faults, notRunNoT0), notRunRewrites(sc.ForcedAOFRewritesS, notRunNoT0)
		return
	case <-ctx.Done():
		r.rec.Faults, r.rec.Rewrites = notRunFaults(sc.Faults, notRunNoT0), notRunRewrites(sc.ForcedAOFRewritesS, notRunNoT0)
		return
	}
	r.logger.Info("hitgen is sending: t0", "faults", len(sc.Faults), "forced_rewrites", len(sc.ForcedAOFRewritesS))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.rec.Faults = r.runFaults(ctx, t0.at, sc.Faults, stop)
	}()
	go func() {
		defer wg.Done()
		r.rec.Rewrites = r.runRewrites(ctx, t0.at, rewriteSchedule(sc.ForcedAOFRewritesS), stop)
	}()
	select {
	case <-r.hitgen.done:
	case <-ctx.Done():
	}
	close(stop)
	wg.Wait()
}

// notRunFaults records every fault as not run, for reason.
func notRunFaults(faults []config.Fault, reason string) []faultRecord {
	recs := make([]faultRecord, len(faults))
	for i, f := range faults {
		recs[i] = newFaultRecord(f)
		recs[i].Error = reason
	}
	return recs
}

// notRunRewrites records every forced rewrite as not run, for reason.
func notRunRewrites(offsets []float64, reason string) []rewriteRecord {
	recs := make([]rewriteRecord, len(offsets))
	for i, at := range offsets {
		recs[i] = rewriteRecord{AtS: at, Error: reason}
	}
	return recs
}

// waitUntil waits until at (monotonic) and reports true, or reports false once stop closes or
// ctx ends first.
func waitUntil(ctx context.Context, at time.Time, stop <-chan struct{}) bool {
	t := time.NewTimer(time.Until(at))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-stop:
		return false
	case <-ctx.Done():
		return false
	}
}

// runFaults runs each fault at t0 + at_s (DESIGN §B4, §B5; plan H3 step 3): a JSON body is
// posted to /_test/faults of its targets, the one instance it names or every instance (§B4),
// all at once; a faultctl cmd runs in-process (runFaultctl, plan H4 step 7). Each fault waits
// for its own time on its own goroutine, one per [[faults]] entry, so targets that answer late,
// or a store outage, delay no other fault (runFault); faults due at the same offset go out
// together, in no set order. It returns once every one has ended, with the records in the
// scenario's order.
func (r *runner) runFaults(ctx context.Context, t0 time.Time, faults []config.Fault, stop <-chan struct{}) []faultRecord {
	recs := make([]faultRecord, len(faults))
	var wg sync.WaitGroup
	for i, f := range faults {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs[i] = r.runFault(ctx, t0, f, stop)
		}()
	}
	wg.Wait()
	return recs
}

// runFault runs f at t0 + its at_s, a faultctl cmd in-process (runFaultctl) and a body through
// sendFault, and returns its record once it has ended: the command, or every target's answer.
// A fault not yet due when stop closes or ctx ends is recorded as not run.
func (r *runner) runFault(ctx context.Context, t0 time.Time, f config.Fault, stop <-chan struct{}) faultRecord {
	rec := newFaultRecord(f)
	if !waitUntil(ctx, t0.Add(offset(f.AtS)), stop) {
		rec.Error = notRunHitgen
		return rec
	}
	if f.Faultctl() {
		rec = r.runFaultctl(ctx, f, rec)
		r.logger.Info("faultctl command ran", "at_s", f.AtS, "cmd", f.Cmd, "start_ns", rec.StartNs, "end_ns", rec.EndNs,
			"outage_start_ns", rec.OutageStartNs, "outage_ns", rec.OutageNs, "error", rec.Error)
		return rec
	}
	rec = r.sendFault(ctx, f, rec)
	r.logger.Info("fault sent", "at_s", f.AtS, "targets", f.Targets, "armed", rec.sent())
	return rec
}

// sendFault posts f's body to each of its targets at once and records the start, the end and
// every answer.
func (r *runner) sendFault(ctx context.Context, f config.Fault, rec faultRecord) faultRecord {
	rec.StartNs = r.now().UnixNano()
	rec.Targets = make([]faultTarget, len(f.Targets))
	var wg sync.WaitGroup
	for i, n := range f.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.Targets[i] = r.postFault(ctx, n, f.Cmd)
		}()
	}
	wg.Wait()
	rec.EndNs = r.now().UnixNano()
	return rec
}

// postFault posts body to /_test/faults of instance n (1-based), within faultPostTimeout.
func (r *runner) postFault(ctx context.Context, n int, body string) faultTarget {
	t := faultTarget{Instance: n}
	if n < 1 || n > len(r.apis) {
		t.Error = fmt.Sprintf("no instance %d", n)
		return t
	}
	ctx, cancel := context.WithTimeout(ctx, faultPostTimeout)
	defer cancel()
	reply, status, err := r.call(ctx, http.MethodPost, r.apis[n-1].addr, "/_test/faults", body)
	t.Status = status
	switch {
	case err != nil:
		t.Error = err.Error()
	case status != http.StatusOK:
		t.Error = fmt.Sprintf("status %d: %.200s", status, reply)
	}
	return t
}

// rewriteSchedule is when the forced rewrites run (DESIGN §B5 "Forced rewrites"; plan H3 step
// 3): each forced_aof_rewrites_s offset as a duration from hitgen's t0 (plan choice), in the
// scenario's order, which the loader keeps increasing.
func rewriteSchedule(offsets []float64) []time.Duration {
	at := make([]time.Duration, len(offsets))
	for i, s := range offsets {
		at[i] = offset(s)
	}
	return at
}

// runRewrites runs each forced rewrite at t0 + its offset (rewriteSchedule), one at a time, in
// order: BGREWRITEAOF as rl_ops, then INFO persistence every rewritePoll until it ends
// (forceRewrite). A rewrite already running when stop closes finishes; those not yet due are
// recorded as not run.
func (r *runner) runRewrites(ctx context.Context, t0 time.Time, schedule []time.Duration, stop <-chan struct{}) []rewriteRecord {
	recs := make([]rewriteRecord, 0, len(schedule))
	for _, d := range schedule {
		at := d.Seconds()
		rec := rewriteRecord{AtS: at}
		if !waitUntil(ctx, t0.Add(d), stop) {
			rec.Error = notRunHitgen
			recs = append(recs, rec)
			continue
		}
		res, err := forceRewrite(ctx, r.storeAddr, r.rd.pw.ops, rewritePoll, r.now)
		rec.StartNs, rec.EndNs, rec.Status = res.startNs, res.endNs, res.status
		if err != nil {
			rec.Error = err.Error()
		}
		r.logger.Info("forced rewrite", "at_s", at, "start_ns", rec.StartNs, "end_ns", rec.EndNs, "status", rec.Status, "error", rec.Error)
		recs = append(recs, rec)
	}
	return recs
}
