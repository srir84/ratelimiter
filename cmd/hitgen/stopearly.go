package main

// stop_early (DESIGN §B4 "Scenario file", §B9; HARNESS PRF-8; IMPLEMENTATION_PLAN H2 step 7):
// each step is judged while the next one runs, once every initial attempt of the step has
// finished. A step whose p99 scheduling lag is above 5 ms is generator-limited; with a
// thresholds file (and not a baseline run) a step also misses when its p99 latency over its
// observation window or its unexpected-error rate is above the file's value for it. The first
// step that is generator-limited or misses stops the launching at once.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"
)

const (
	// allowedLagNs is the largest step p99 of sent_ns − planned_ns that is not generator-limited
	// (§B9 "Allowed scheduling lag").
	allowedLagNs = int64(5 * time.Millisecond)
	// warmupNs is the start of each step left out of its observation window (§B9).
	warmupNs = int64(5 * time.Second)
	// unsentLag is the lag of a first attempt that was never written (a generator drop, or a
	// failure before WroteRequest): no lag bound covers it (plan choice, PRF-8).
	unsentLag = int64(math.MaxInt64)
	// errAbortedAfterAcquire is the 5xx that is not an unexpected error (§B9, ERR-15).
	errAbortedAfterAcquire = "service_aborted_after_acquire"
)

// stepThreshold is one step's frozen thresholds (§B9): set false when the file gives the step
// none, because its baseline step is missing or generator-limited.
type stepThreshold struct {
	set       bool
	latencyNs int64   // the most a p99 latency may be, ns
	errorRate float64 // the most an unexpected-error rate may be, a fraction
}

// thresholdsFile is the part of a frozen thresholds file hitgen reads (plan choice for its
// shape; H5's report/thresholds.py writes it): a "steps" list with one object per step that has
// thresholds, each naming its 0-based "step" and either "generator_limited": true (no
// thresholds, §B9 "Missing baseline step") or both "latency_threshold_ns" (an integer) and
// "error_rate_threshold" (a fraction in [0, 1]). Other keys are not hitgen's and are ignored.
type thresholdsFile struct {
	Steps []struct {
		Step               *int     `json:"step"`
		GeneratorLimited   bool     `json:"generator_limited"`
		LatencyThresholdNs *int64   `json:"latency_threshold_ns"`
		ErrorRateThreshold *float64 `json:"error_rate_threshold"`
	} `json:"steps"`
}

// loadThresholds reads the thresholds file at path for a scenario of steps steps and returns
// each step's thresholds. It refuses a file that is not that JSON, has no steps, names a step
// outside the scenario or twice, or gives a step that is not generator-limited no threshold or
// one out of range (a latency threshold ≤ 0, an error-rate threshold outside [0, 1]).
func loadThresholds(path string, steps int) ([]stepThreshold, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the thresholds file: %w", err)
	}
	var f thresholdsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("thresholds file %s: %w", path, err)
	}
	if len(f.Steps) == 0 {
		return nil, fmt.Errorf("thresholds file %s: steps: none", path)
	}
	out := make([]stepThreshold, steps)
	seen := make([]bool, steps)
	var problems []error
	for n, st := range f.Steps {
		switch {
		case st.Step == nil:
			problems = append(problems, fmt.Errorf("steps[%d].step: missing", n))
			continue
		case *st.Step < 0 || *st.Step >= steps:
			problems = append(problems, fmt.Errorf("steps[%d].step: %d is not a step of the scenario (0 to %d)", n, *st.Step, steps-1))
			continue
		case seen[*st.Step]:
			problems = append(problems, fmt.Errorf("steps[%d].step: step %d appears twice", n, *st.Step))
			continue
		}
		seen[*st.Step] = true
		if st.GeneratorLimited {
			continue
		}
		lat, rate := st.LatencyThresholdNs, st.ErrorRateThreshold
		switch {
		case lat == nil || rate == nil:
			problems = append(problems, fmt.Errorf("steps[%d]: step %d needs latency_threshold_ns and error_rate_threshold, or generator_limited", n, *st.Step))
		case *lat <= 0:
			problems = append(problems, fmt.Errorf("steps[%d].latency_threshold_ns: %d, want more than 0", n, *lat))
		case !(*rate >= 0 && *rate <= 1):
			problems = append(problems, fmt.Errorf("steps[%d].error_rate_threshold: %v, want 0 to 1", n, *rate))
		default:
			out[*st.Step] = stepThreshold{set: true, latencyNs: *lat, errorRate: *rate}
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("thresholds file %s: %w", path, errors.Join(problems...))
	}
	return out, nil
}

// evaluator judges the steps in order as their first attempts finish (stop_early). Its methods
// are safe for concurrent use: every entry's goroutine reports to it.
type evaluator struct {
	logger     *slog.Logger
	bounds     []int64         // step k is [bounds[k], bounds[k+1]) ns from the run's start
	thresholds []stepThreshold // nil: compare the lag only (no file, or a baseline run)
	stopCh     chan struct{}   // closed by the first miss while launching goes on

	mu       sync.Mutex
	steps    []stepStats
	next     int  // the next step to judge
	stopped  int  // the step whose miss stopped the run; -1 while none has
	finished bool // launching ended: a miss no longer stops anything
	// clock is the sender's stop clock (bind): it records a stop's time and returns it, with
	// false once launching is over. nil: a stop at time 0 whenever launching has not ended.
	clock func() (int64, bool)
}

// stepStats collects one step's first attempts. Latencies and errors cover its observation
// window only, the first attempts of entries planned once its first 5 s have passed (§B9).
type stepStats struct {
	pending    int     // entries whose first attempt has not finished, launched or not
	lags       []int64 // sent_ns − planned_ns of each finished first attempt; unsentLag if never written
	latencies  []int64 // latency of each first attempt in the window with an HTTP response
	attempts   int     // first attempts made in the window (the error denominator, §B9)
	unexpected int     // of them, 5xx other than ERR-15, timeouts and transport errors (§B9)
}

// newEvaluator returns the evaluator for a run of entries over steps with the given bounds
// (len(steps)+1 of them); thresholds is nil, or one per step.
func newEvaluator(logger *slog.Logger, entries []Entry, bounds []int64, thresholds []stepThreshold) *evaluator {
	ev := &evaluator{logger: logger, bounds: bounds, thresholds: thresholds, stopCh: make(chan struct{}),
		steps: make([]stepStats, len(bounds)-1), stopped: -1}
	for _, e := range entries {
		ev.steps[e.Step].pending++
	}
	return ev
}

// stop is closed once a step misses while launching goes on.
func (ev *evaluator) stop() <-chan struct{} { return ev.stopCh }

// bind gives the evaluator the sender's stop clock (sender.stopClock), before any attempt
// reports to it.
func (ev *evaluator) bind(clock func() (int64, bool)) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.clock = clock
}

// stopTime records a stop on the clock and returns its time, or returns false once launching
// has ended (finish, or the clock says so). Called with mu held.
func (ev *evaluator) stopTime() (int64, bool) {
	switch {
	case ev.finished:
		return 0, false
	case ev.clock == nil:
		return 0, true
	}
	return ev.clock()
}

// stoppedAfter returns the step whose miss stopped the run, or -1.
func (ev *evaluator) stoppedAfter() int {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return ev.stopped
}

// finish tells the evaluator that launching has ended (the schedule is done, or stopped): a
// later miss stops nothing.
func (ev *evaluator) finish() {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.finished = true
}

// dropped records an entry the dispatcher did not launch (a generator drop): its first attempt
// is over, never written.
func (ev *evaluator) dropped(e Entry) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	st := &ev.steps[e.Step]
	st.lags = append(st.lags, unsentLag)
	ev.done(st)
}

// initialDone records the finished first attempt of entry e.
func (ev *evaluator) initialDone(e Entry, r result) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	st := &ev.steps[e.Step]
	lag := unsentLag
	if r.sentNs != nil {
		lag = *r.sentNs - e.PlannedNs
	}
	st.lags = append(st.lags, lag)
	// The observation window (§B9): each step minus its first 5 s, for the error rate's
	// denominator and numerator alike, and for the latencies.
	if e.PlannedNs >= ev.bounds[e.Step]+warmupNs {
		st.attempts++
		if unexpectedError(r) {
			st.unexpected++
		}
		// The latency population is the first attempts with an HTTP response (§B9), chosen by
		// status: a timeout or a transport error (status 0) stays out, although it has a latency
		// too, to its failure. A response follows the request's write, so it has a latency.
		if r.status != 0 && r.latency != nil {
			st.latencies = append(st.latencies, *r.latency)
		}
	}
	ev.done(st)
}

// unexpectedError reports whether an attempt's outcome is an unexpected error (§B9): a 5xx
// other than service_aborted_after_acquire (ERR-15), or no response at all (a timeout or a
// transport error).
func unexpectedError(r result) bool {
	if r.status == 0 {
		return true
	}
	if r.status < http.StatusInternalServerError || r.status > 599 {
		return false
	}
	return r.errCode == nil || *r.errCode != errAbortedAfterAcquire
}

// done counts one finished first attempt of st's step, then judges every step, in order, whose
// first attempts have all finished. The last step is never judged: there is no step after it
// to stop. A miss stops the launching at the time the clock records, unless launching is over
// by then (the whole schedule dispatched, or ctx ended): that miss is only logged. Called with
// mu held.
func (ev *evaluator) done(st *stepStats) {
	st.pending--
	for ev.stopped < 0 && ev.next < len(ev.steps)-1 && ev.steps[ev.next].pending == 0 {
		k := ev.next
		ev.next++
		reason, missed := ev.judge(k)
		if !missed {
			continue
		}
		stopNs, ok := ev.stopTime()
		if !ok {
			ev.logger.Info("a step missed after the schedule was sent; nothing left to stop", "step", k, "reason", reason)
			return
		}
		ev.stopped = k
		close(ev.stopCh)
		ev.logger.Warn("stopping early", "step", k, "reason", reason, "stop_ns", stopNs)
	}
}

// judge returns why step k misses, if it does: generator-limited (its p99 lag above
// allowedLagNs), or, with thresholds for it, its p99 latency or its unexpected-error rate above
// them. A step with no first attempts of a kind is not judged on it.
func (ev *evaluator) judge(k int) (reason string, missed bool) {
	st := &ev.steps[k]
	if len(st.lags) > 0 {
		if lag := p99(st.lags); lag > allowedLagNs {
			return "generator-limited: p99 scheduling lag above 5 ms", true
		}
	}
	if ev.thresholds == nil || !ev.thresholds[k].set {
		return "", false
	}
	th := ev.thresholds[k]
	if len(st.latencies) > 0 && p99(st.latencies) > th.latencyNs {
		return "p99 latency above the threshold", true
	}
	if st.attempts > 0 && float64(st.unexpected)/float64(st.attempts) > th.errorRate {
		return "unexpected-error rate above the threshold", true
	}
	return "", false
}

// p99 is the nearest-rank 99th percentile of vs (not empty): the smallest value with at least
// 99 % of vs at or below it. It sorts vs.
func p99(vs []int64) int64 {
	slices.Sort(vs)
	rank := (99*len(vs) + 99) / 100 // ⌈0.99 n⌉
	return vs[rank-1]
}
