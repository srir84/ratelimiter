package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// judged reports whether ev has stopped the run, and after which step.
func judged(ev *evaluator) (stopped bool, step int) {
	select {
	case <-ev.stop():
		return true, ev.stoppedAfter()
	default:
		return false, ev.stoppedAfter()
	}
}

// stepEntries returns n entries for each step of bounds, spread evenly over the step.
func stepEntries(bounds []int64, n int) []Entry {
	var out []Entry
	for k := range len(bounds) - 1 {
		for j := range n {
			out = append(out, Entry{I: len(out), PlannedNs: bounds[k] + (bounds[k+1]-bounds[k])*int64(j)/int64(n), Step: k,
				Instance: 1, Kind: "initial", DuplicateOf: -1})
		}
	}
	return out
}

// sentAfter is a 200 written lag ns after e's planned time, answered latency ns later.
func sentAfter(e Entry, lag, latency int64) result {
	sent := e.PlannedNs + lag
	return result{sentNs: &sent, latency: &latency, status: 200}
}

// TestStopEarly_Judge checks the step judgement of stop_early (plan H2 step 7, DESIGN §B9):
// generator-limited means a nearest-rank p99 of sent_ns − planned_ns above 5 ms, with first
// attempts never written (drops among them) counting as unbounded lag; with thresholds, over
// the step's observation window (first attempts of entries planned after the step's first
// 5 s), the p99 latency of the first attempts with an HTTP response (status 0 left out, though
// a written timeout has a latency), and the unexpected-error rate (5xx other than
// service_aborted_after_acquire, and no response, over the window's first attempts), each
// above its threshold, miss. Steps are judged in order once their first attempts have all
// finished; the last step is never judged; a miss after the schedule is sent stops nothing.
// Step 0's 100 entries are spread over [0 s, 10 s): entries 50 to 99 make its window.
func TestStopEarly_Judge(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	bounds := []int64{0, 10 * sec, 20 * sec, 30 * sec}
	th := []stepThreshold{{set: true, latencyNs: 100_000_000, errorRate: 0.02}, {set: true, latencyNs: 100_000_000, errorRate: 0.02}, {}}
	// loose allows an unexpected-error rate up to 50 %, so that only the latency can miss.
	loose := []stepThreshold{{set: true, latencyNs: 100_000_000, errorRate: 0.5}, {}, {}}
	ms := int64(1_000_000)

	// run feeds step 0's 100 first attempts (outcome i gives attempt i's result; nil: dropped)
	// and reports whether step 0 stopped the run.
	run := func(thresholds []stepThreshold, outcome func(i int, e Entry) *result) (bool, int) {
		entries := stepEntries(bounds, 100)
		ev := newEvaluator(logger, entries, bounds, thresholds)
		for i, e := range entries[:100] {
			if r := outcome(i, e); r != nil {
				ev.initialDone(e, *r)
			} else {
				ev.dropped(e)
			}
		}
		return judged(ev)
	}
	lagged := func(n int, lag int64) func(int, Entry) *result {
		return func(i int, e Entry) *result {
			r := sentAfter(e, 1*ms, 10*ms)
			if i < n {
				r = sentAfter(e, lag, 10*ms)
			}
			return &r
		}
	}
	for _, c := range []struct {
		name       string
		thresholds []stepThreshold
		outcome    func(i int, e Entry) *result
		stop       bool
	}{
		{"every lag at 5 ms", nil, lagged(100, 5*ms), false},
		{"every lag at 5 ms + 1 ns", nil, lagged(100, 5*ms+1), true},
		{"one lag of 1 s in 100: p99 is the 99th value", nil, lagged(1, 1_000*ms), false},
		{"two lags of 1 s in 100", nil, lagged(2, 1_000*ms), true},
		{"one drop in 100", nil, func(i int, e Entry) *result {
			if i == 0 {
				return nil
			}
			r := sentAfter(e, ms, 10*ms)
			return &r
		}, false},
		{"two drops in 100", nil, func(i int, e Entry) *result {
			if i < 2 {
				return nil
			}
			r := sentAfter(e, ms, 10*ms)
			return &r
		}, true},
		{"two attempts in 100 never written", nil, func(i int, e Entry) *result {
			r := sentAfter(e, ms, 10*ms)
			if i < 2 {
				r = result{status: 0, errCode: ptr("transport_error")}
			}
			return &r
		}, true},
		{"latency at the threshold", th, func(_ int, e Entry) *result { r := sentAfter(e, ms, 100*ms); return &r }, false},
		{"latency above the threshold", th, func(_ int, e Entry) *result { r := sentAfter(e, ms, 100*ms+1); return &r }, true},
		{"slow only in the first 5 s", th, func(_ int, e Entry) *result {
			lat := 10 * ms
			if e.PlannedNs < 5*sec {
				lat = 900 * ms
			}
			r := sentAfter(e, ms, lat)
			return &r
		}, false},
		{"slow at the window's start", th, func(_ int, e Entry) *result {
			lat := 10 * ms
			if e.PlannedNs == 5*sec { // the window's first entry: p99 of its 50 is their largest
				lat = 900 * ms
			}
			r := sentAfter(e, ms, lat)
			return &r
		}, true},
		{"latency above the threshold without thresholds", nil, func(_ int, e Entry) *result {
			r := sentAfter(e, ms, 900*ms)
			return &r
		}, false},
		{"written timeouts with a 30 s latency in the window: not in the p99", loose, func(i int, e Entry) *result {
			r := sentAfter(e, ms, 10*ms)
			if i >= 98 { // 2 of the window's 50: in the p99 (its largest value) they would miss
				r.status, r.latency, r.errCode = 0, ptr(30_000*ms), ptr("timeout")
			}
			return &r
		}, false},
		{"1 unexpected error (503) in the window's 50: 2 %", th, errorOutcomes(1, 503, "limiter_unavailable"), false},
		{"2 unexpected errors (503) in the window's 50: 4 %", th, errorOutcomes(2, 503, "limiter_unavailable"), true},
		{"503s only in the warm-up, every one of its 50", th, func(_ int, e Entry) *result {
			r := sentAfter(e, ms, 10*ms)
			if e.PlannedNs < 5*sec {
				r.status, r.errCode = 503, ptr("limiter_unavailable")
			}
			return &r
		}, false},
		{"2 500s without an error code in the window's 50", th, errorOutcomes(2, 500, ""), true},
		{"2 service_aborted_after_acquire in the window's 50", th, errorOutcomes(2, 503, "service_aborted_after_acquire"), false},
		{"2 429s in the window's 50", th, errorOutcomes(2, 429, "rate_limited"), false},
		{"2 400s in the window's 50", th, errorOutcomes(2, 400, "invalid_request"), false},
		{"3 timeouts after the write, 2 of them in the window's 50", th, func(i int, e Entry) *result {
			r := sentAfter(e, ms, 10*ms)
			if i%33 == 0 && i > 0 && i < 100 {
				r.status, r.latency, r.errCode = 0, nil, ptr("timeout")
			}
			return &r
		}, true},
		{"errors without thresholds", nil, errorOutcomes(50, 503, "limiter_unavailable"), false},
		{"a step with no thresholds (generator-limited baseline)", []stepThreshold{{}, {}, {}}, errorOutcomes(50, 503, "limiter_unavailable"), false},
	} {
		if stopped, step := run(c.thresholds, c.outcome); stopped != c.stop || (stopped && step != 0) {
			t.Errorf("%s: stopped %v after step %d, want stopped %v after step 0", c.name, stopped, step, c.stop)
		}
	}

	// Order: step 1 finishes first, but is judged only after step 0; then a miss of step 1 stops.
	entries := stepEntries(bounds, 10)
	ev := newEvaluator(logger, entries, bounds, nil)
	for _, e := range entries[10:20] {
		ev.initialDone(e, sentAfter(e, 50*ms, 10*ms))
	}
	if stopped, _ := judged(ev); stopped {
		t.Error("step 1 was judged before step 0 had finished")
	}
	for _, e := range entries[:10] {
		ev.initialDone(e, sentAfter(e, ms, 10*ms))
	}
	if stopped, step := judged(ev); !stopped || step != 1 {
		t.Errorf("once step 0 finished: stopped %v after step %d, want the lagging step 1 judged and stopping", stopped, step)
	}

	// The observation window is each step minus its own first 5 s (§B9): with thresholds on
	// step 1, latencies of 900 ms over step 1's first 5 s ([10 s, 15 s)) do not count, while the
	// same latencies inside its window ([15 s, 20 s)) miss.
	for _, c := range []struct {
		name       string
		slow       func(e Entry) bool
		stop       bool
		stopsAfter int
	}{
		{"slow only in step 1's first 5 s", func(e Entry) bool { return e.Step == 1 && e.PlannedNs < 15*sec }, false, -1},
		{"slow in step 1's window", func(e Entry) bool { return e.Step == 1 && e.PlannedNs >= 15*sec }, true, 1},
	} {
		entries := stepEntries(bounds, 100)
		ev := newEvaluator(logger, entries, bounds, th)
		for _, e := range entries[:200] {
			lat := 10 * ms
			if c.slow(e) {
				lat = 900 * ms
			}
			ev.initialDone(e, sentAfter(e, ms, lat))
		}
		if stopped, step := judged(ev); stopped != c.stop || step != c.stopsAfter {
			t.Errorf("%s: stopped %v after step %d, want stopped %v after step %d", c.name, stopped, step, c.stop, c.stopsAfter)
		}
	}

	// The last step is never judged, and a miss once launching ended stops nothing.
	ev = newEvaluator(logger, entries, bounds, nil)
	for _, e := range entries[:20] {
		ev.initialDone(e, sentAfter(e, ms, 10*ms))
	}
	for _, e := range entries[20:] {
		ev.initialDone(e, sentAfter(e, 900*ms, 10*ms))
	}
	if stopped, _ := judged(ev); stopped {
		t.Error("the last step was judged and stopped the run, want it never judged")
	}
	ev = newEvaluator(logger, entries, bounds, nil)
	ev.finish()
	for _, e := range entries[:10] {
		ev.initialDone(e, sentAfter(e, 900*ms, 10*ms))
	}
	if stopped, step := judged(ev); stopped || step != -1 {
		t.Errorf("a miss after launching ended: stopped %v after step %d, want no stop", stopped, step)
	}
}

// TestStopEarly_FirstAttemptsOnly (plan H2 step 7; DESIGN §B9): only first attempts reach
// stop_early. Against a server that answers each request ID 429 (retry_after_ms 100) and then
// 200, a sender whose evaluator covers one step of 3 entries makes 3 retries, while the
// evaluator sees 3 first attempts and 3 lags, with nothing left pending. The step is given as
// starting 5 s before the run, so that its observation window (§B9) holds the 3 entries and
// they count in the error denominator.
func TestStopEarly_FirstAttemptsOnly(t *testing.T) {
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, q stubReq) {
		if q.seen == 0 {
			answerLimitedMs(w, r, 100)
			return
		}
		answerOK(w, r)
	})
	entries := []Entry{
		{I: 0, PlannedNs: 10_000_000, User: "u000000", RequestID: "r000000000", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 1, PlannedNs: 20_000_000, User: "u000001", RequestID: "r000000001", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 2, PlannedNs: 30_000_000, User: "u000002", RequestID: "r000000002", Instance: 1, Kind: "initial", DuplicateOf: -1},
	}
	ev := newEvaluator(slog.New(slog.DiscardHandler), entries, []int64{-5 * sec, sec}, nil)
	recs := attempts(t, sendAll(t, entries, []string{st.URL + "/hit"}, func(s *sender) { s.eval = ev }))
	retries := 0
	for _, r := range recs {
		if r.Attempt > 1 {
			retries++
		}
	}
	ev.mu.Lock()
	st0 := ev.steps[0]
	ev.mu.Unlock()
	if retries != 3 || st0.attempts != 3 || len(st0.lags) != 3 || st0.pending != 0 {
		t.Errorf("%d retries; the step saw %d first attempts and %d lags, %d pending; want 3 retries, 3, 3 and 0 pending",
			retries, st0.attempts, len(st0.lags), st0.pending)
	}
}

// errorOutcomes returns outcomes where the first n first attempts of step 0's observation
// window (entries 50 to 99 of its 100, planned from 5 s) got status with error code (none when
// ""), and the rest a 200.
func errorOutcomes(n, status int, code string) func(int, Entry) *result {
	return func(i int, e Entry) *result {
		r := sentAfter(e, 1_000_000, 10_000_000)
		if i >= 50 && i < 50+n {
			r.status = status
			if code != "" {
				r.errCode = ptr(code)
			}
		}
		return &r
	}
}

// TestStopEarly_ThresholdsFile checks loadThresholds (plan H2 step 7; the file shape is a plan
// choice): per-step thresholds by step index, generator_limited steps and steps the file
// leaves out get none, other keys are ignored; a file that is not that JSON, has no steps,
// names a step outside the scenario or twice, or gives a step a missing or out-of-range
// threshold is refused, naming the field.
func TestStopEarly_ThresholdsFile(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "thresholds.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	got, err := loadThresholds(write(t, `{"created_ns": 5, "definitions": {"latency": "p99 + 50 ms"}, "steps": [
		{"step": 2, "latency_threshold_ns": 150000000, "error_rate_threshold": 0.001, "baseline_p99_ns": 100000000},
		{"step": 0, "generator_limited": true},
		{"step": 1, "latency_threshold_ns": 1, "error_rate_threshold": 0}]}`), 4)
	if err != nil {
		t.Fatalf("loadThresholds: %v", err)
	}
	want := []stepThreshold{{}, {set: true, latencyNs: 1, errorRate: 0}, {set: true, latencyNs: 150_000_000, errorRate: 0.001}, {}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("loadThresholds = %+v, want %+v", got, want)
	}
	for _, c := range []struct{ name, body, field string }{
		{"not JSON", `steps = []`, "thresholds file"},
		{"no steps", `{"steps": []}`, "steps: none"},
		{"no step key", `{"steps": [{"latency_threshold_ns": 1, "error_rate_threshold": 0}]}`, "steps[0].step: missing"},
		{"a negative step", `{"steps": [{"step": -1, "generator_limited": true}]}`, "steps[0].step"},
		{"a step past the last", `{"steps": [{"step": 4, "generator_limited": true}]}`, "steps[0].step"},
		{"a step twice", `{"steps": [{"step": 1, "generator_limited": true}, {"step": 1, "generator_limited": true}]}`, "steps[1].step"},
		{"no latency threshold", `{"steps": [{"step": 0, "error_rate_threshold": 0.1}]}`, "latency_threshold_ns and error_rate_threshold"},
		{"no error-rate threshold", `{"steps": [{"step": 0, "latency_threshold_ns": 5}]}`, "latency_threshold_ns and error_rate_threshold"},
		{"latency threshold 0", `{"steps": [{"step": 0, "latency_threshold_ns": 0, "error_rate_threshold": 0.1}]}`, "steps[0].latency_threshold_ns"},
		{"error rate above 1", `{"steps": [{"step": 0, "latency_threshold_ns": 5, "error_rate_threshold": 1.5}]}`, "steps[0].error_rate_threshold"},
		{"error rate below 0", `{"steps": [{"step": 0, "latency_threshold_ns": 5, "error_rate_threshold": -0.1}]}`, "steps[0].error_rate_threshold"},
		{"a latency threshold that is not an integer", `{"steps": [{"step": 0, "latency_threshold_ns": 1.5, "error_rate_threshold": 0.1}]}`, "thresholds file"},
	} {
		if _, err := loadThresholds(write(t, c.body), 4); err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: error %v, want one naming %q", c.name, err, c.field)
		}
	}
	if _, err := loadThresholds(filepath.Join(t.TempDir(), "missing.json"), 4); err == nil {
		t.Error("a missing file loaded, want an error")
	}
}
