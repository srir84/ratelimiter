package main

// Sending the schedule and recording it (DESIGN §B4 "Sending", "HTTP client", "Record"; HARNESS
// TRF-3, TRF-5, TRF-10, PRF-1, PRF-8; IMPLEMENTATION_PLAN H2 steps 4 and 6). The run is open
// loop: a dispatcher wakes every 1 ms and launches one goroutine for each entry whose planned
// time has passed, and never waits for a response. That goroutine makes the entry's first
// attempt in an in-flight slot and records it in gen.jsonl when it ends. A retry after 429
// (retry.go) waits out its hint on a timer, holding no in-flight slot, so waiting retries never
// hold back initial arrivals (TRF-3); when the timer fires, the retry takes a slot again. Every
// time is monotonic, an offset from the run's start t0.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"ratelimiter/harness/config"
)

// maxInFlight is the most attempts in flight at once, each in its own goroutine (plan choice):
// an entry due while that many are running is not launched and is a generator drop (PRF-1),
// and a retry due then is a retry drop. Tests lower it; it is the one package variable they may
// write (bin/trace testglobals).
var maxInFlight = 20_000

const (
	kindRetry   = "retry"   // gen.jsonl: an attempt after a 429 (TRF-5)
	kindSummary = "summary" // gen.jsonl: the final line

	// The error of an attempt that got no response (plan choice): the client's timeout ended
	// it, or the connection failed.
	errTimeout   = "timeout"
	errTransport = "transport_error"

	// tickEvery is the dispatcher's period (§B4 "Sending", §D1).
	tickEvery = time.Millisecond
	// maxPendingRetries is the most retries waiting out their hint at once (plan choice; AGENTS.md
	// §1 Queues): a retry due to wait while that many wait is not made, a retry drop, and its
	// entry's attempts end. A waiting retry costs a timer, not a goroutine; at S1's 10,000/s with
	// every entry limited, about 20,500 wait at once (2 retries of ≥ 1 s each, HTTP-9).
	maxPendingRetries = 100_000
	// maxBodyBytes bounds the response body read: hitapi's are well under 1 KiB.
	maxBodyBytes = 64 << 10
)

// lineWriter takes run-log records (harness/runlog.Writer): Enqueue never blocks.
type lineWriter interface {
	Enqueue(rec any)
}

// attemptLine is one gen.jsonl line per attempt (§B4 "Record", TRF-10). Fields that do not
// apply are null: duplicate_of on a non-duplicate entry's attempts, the hint fields and
// jitter_ns on first attempts, sent_ns before WroteRequest and got_conn_wait_ns before GotConn,
// latency on an attempt never written (sent_ns null), arrival_ns and decision_ns without a
// response that carried them, and error on a 200.
type attemptLine struct {
	Kind          string  `json:"kind"` // the entry's kind on attempt 1; retry after it
	PlannedNs     int64   `json:"planned_ns"`
	SentNs        *int64  `json:"sent_ns"`
	GotConnWaitNs *int64  `json:"got_conn_wait_ns"`
	Instance      int     `json:"instance"`
	User          string  `json:"user"`
	RequestID     string  `json:"request_id"`
	DuplicateOf   *int    `json:"duplicate_of"`
	Attempt       int     `json:"attempt"`
	HintSource    *string `json:"hint_source"`
	HintMs        *int64  `json:"hint_ms"`
	JitterNs      *int64  `json:"jitter_ns"`
	Status        int     `json:"status"` // 0 when there was no response
	Error         *string `json:"error"`
	// Latency is the attempt's end − WroteRequest, ns, monotonic: its end is the response body
	// read (plan choice) or, with no response, the failure (a timeout or a transport error).
	Latency    *int64 `json:"latency"`
	ArrivalNs  *int64 `json:"arrival_ns"`
	DecisionNs *int64 `json:"decision_ns"`
}

// summary is gen.jsonl's final line (plan H2 step 6, PRF-1): planned entries, those launched,
// those never written (planned − written, counting WroteRequest events), the run log's drops,
// the step whose miss stopped the run and the partly run step it cut (null when the run was
// not stopped early), and the arrivals the schedule left out for want of an eligible user.
type summary struct {
	Kind              string `json:"kind"`
	Planned           int    `json:"planned"`
	Launched          int    `json:"launched"`
	Unsent            int    `json:"unsent"`
	LogDrops          int64  `json:"log_drops"`
	StoppedAfterStep  *int   `json:"stopped_after_step"`
	ExcludedStep      *int   `json:"excluded_step"`
	SkippedNoEligible int    `json:"skipped_no_eligible"`
}

// result is what one attempt saw. Times are offsets from t0; nil means it did not happen.
type result struct {
	sentNs        *int64
	gotConnWaitNs *int64
	latency       *int64
	status        int
	errCode       *string
	arrivalNs     *int64
	decisionNs    *int64
	retryAfterMs  *int64    // the body's retry_after_ms
	retryAfter    []string  // the Retry-After header's values
	ended         time.Time // when the response was read or the attempt failed
}

// emptyBody is every /hit request's body: empty, and neither nil nor http.NoBody, with no
// GetBody, so net/http never replays a request whose reused connection failed (plan H2 step 4;
// AGENTS.md §6): such a resend would be an unrecorded second attempt (TRF-5). net/http reads it,
// finds it empty and sends no body.
type emptyBody struct{}

func (emptyBody) Read([]byte) (int, error) { return 0, io.EOF }
func (emptyBody) Close() error             { return nil }

// sendConfig is what a sender needs besides the schedule; main builds it.
type sendConfig struct {
	urls    []string     // urls[n-1] is instance n's /hit URL
	client  *http.Client // the one client (main's newClient)
	timeout time.Duration
	seed    uint64
	retries config.Retries
	eval    *evaluator // nil without stop_early
	log     lineWriter
	logger  *slog.Logger
	// runNs is the scenario's run length, the sum of its steps' durations (Scenario.RunNs): a run
	// that sends its whole schedule lasts that long from t0 (plan H2 step 1: the steps fix the
	// run length). 0: no longer than the schedule.
	runNs int64
}

// sender sends one schedule.
type sender struct {
	sendConfig
	entries    []Entry
	capacity   int64            // maxInFlight when the sender was made
	pendingCap int              // maxPendingRetries; tests lower it
	ticks      <-chan time.Time // the dispatcher's wake-ups: nil for a tickEvery ticker; tests drive it
	t0         time.Time        // the run's start, monotonic
	wg         sync.WaitGroup
	inFlight   atomic.Int64 // attempts in flight, each holding a slot (takeSlot)
	written    atomic.Int64 // entries whose first attempt was written (WroteRequest, no error)
	next       int          // the dispatcher's only: the next entry to reach
	launched   int          // the dispatcher's only
	drops      int          // the dispatcher's only: entries due while maxInFlight were running

	// stopMu orders a stop_early stop (stopClock) against the dispatcher's batches: each batch,
	// reading the time and launching every entry due by then, holds it.
	stopMu   sync.Mutex
	frontier int64 // the time of the last batch, an offset from t0; -1 before the first
	stopNs   int64 // the stop_early stop's time, an offset from t0; -1 while there is none
	over     bool  // launching is over: every entry reached, ctx ended, or a stop

	mu         sync.Mutex
	pending    map[*pendingRetry]struct{} // retries waiting out their hint
	abandoned  bool                       // launching stopped: no retry is made after this
	retryDrops int                        // retries not made: pendingCap waiting, or no slot when due
}

// pendingRetry is one retry waiting out its hint: its timer starts the attempt.
type pendingRetry struct {
	timer *time.Timer
}

func newSender(cfg sendConfig, entries []Entry) *sender {
	return &sender{sendConfig: cfg, entries: entries, capacity: int64(maxInFlight), pendingCap: maxPendingRetries,
		frontier: -1, stopNs: -1, pending: map[*pendingRetry]struct{}{}}
}

// stopReason is why the dispatcher stopped launching, or that it goes on.
type stopReason int

const (
	launching  stopReason = iota // launching goes on
	stopNone                     // every entry was dispatched
	stopSignal                   // ctx ended (SIGTERM)
	stopEarly                    // a step missed (stop_early)
)

// run sends the schedule and returns the summary, all but log_drops and skipped_no_eligible.
// It ends once every attempt has been recorded: with the schedule sent, once the scenario's run
// length has passed since t0 (rate-0 steps and a quiet tail last too), after the last attempts
// and retries; once launching stops (ctx ends, or a step misses), after the attempts in flight,
// which get at most the client timeout before they are cancelled, while retries waiting for
// their hint are dropped (plan choice). Attempts are not tied to ctx: SIGTERM stops the
// launching, or the wait for the run's end, only (plan H2 step 8).
func (s *sender) run(ctx context.Context) summary {
	reqCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	s.t0 = time.Now()
	if s.eval != nil {
		s.eval.bind(s.stopClock)
	}
	why := s.dispatch(ctx, reqCtx)
	if s.eval != nil {
		s.eval.finish()
	}
	if why == stopNone {
		why = s.waitRunEnd(ctx)
	}
	idle := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(idle)
	}()
	if why == stopNone {
		select {
		case <-idle:
		case <-ctx.Done():
			why = stopSignal
		}
	}
	if why != stopNone {
		s.abandonRetries()
		s.logger.Info("launching stopped: waiting for the attempts in flight", "in_flight", s.inFlight.Load())
		grace := time.NewTimer(s.timeout)
		select {
		case <-idle:
		case <-grace.C:
			s.logger.Warn("attempts still in flight after the client timeout: cancelling them")
			cancelRequests()
			<-idle
		}
		grace.Stop()
	}

	sum := summary{Kind: kindSummary, Planned: len(s.entries), Launched: s.launched}
	if why == stopEarly {
		k := s.eval.stoppedAfter()
		planned, excluded := stopFields(s.entries, s.eval.bounds, k, s.stoppedAt())
		sum.Planned = planned
		sum.StoppedAfterStep, sum.ExcludedStep = &k, &excluded
	}
	sum.Unsent = sum.Planned - int(s.written.Load())
	s.mu.Lock()
	retryDrops := s.retryDrops
	s.mu.Unlock()
	s.logger.Info("sending done", "planned", sum.Planned, "launched", sum.Launched, "unsent", sum.Unsent,
		"generator_drops", s.drops, "retry_drops", retryDrops)
	if retryDrops > 0 {
		s.logger.Warn("retries dropped: too many waiting, or no in-flight slot when due", "retry_drops", retryDrops)
	}
	return sum
}

// stopFields returns the summary's planned count and excluded_step for a run stopped at stopNs
// (an offset from t0) after step k's miss (plan H2 step 7), with entries in planned order and
// step j being [bounds[j], bounds[j+1]). planned counts exactly the entries planned before the
// stop, those already due that the dispatcher had not reached yet included. excluded_step is
// the partly run step: the one whose interval holds the stop (the last step for a stop after
// the run's length), and at least k+1. Step k is judged only once its first attempts have all
// finished, up to the client timeout after it ends, so the stop can land after a short step k+1
// has run in full, or inside step k when its last first attempts finish early.
func stopFields(entries []Entry, bounds []int64, k int, stopNs int64) (planned, excluded int) {
	planned = sort.Search(len(entries), func(i int) bool { return entries[i].PlannedNs >= stopNs })
	steps := len(bounds) - 1
	step := min(sort.Search(steps, func(j int) bool { return bounds[j+1] > stopNs }), steps-1)
	return planned, max(k+1, step)
}

// stoppedAt returns the stop_early stop's time, an offset from t0, or -1 without one.
func (s *sender) stoppedAt() int64 {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	return s.stopNs
}

// stopClock is the evaluator's clock for a stop_early stop (evaluator.bind): it records the
// stop's time, an offset from t0, and returns it with true, or returns false once launching is
// over (the dispatcher has reached every entry, or ctx ended), when a miss stops nothing.
// Holding stopMu, it comes after any batch the dispatcher is launching and is later than that
// batch's time, so every entry the dispatcher reached was planned before the stop, and the
// batches after it see the stop and launch nothing.
func (s *sender) stopClock() (int64, bool) {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.over {
		return 0, false
	}
	s.stopNs = max(time.Since(s.t0).Nanoseconds(), s.frontier+1)
	return s.stopNs, true
}

// waitRunEnd waits, once every entry has been dispatched, until the scenario's run length has
// passed since t0 (plan H2 step 1: the steps fix the run length, so rate-0 steps and a quiet
// tail last too), and returns stopNone; it returns stopSignal when ctx ends first (SIGTERM).
func (s *sender) waitRunEnd(ctx context.Context) stopReason {
	left := time.Duration(s.runNs - time.Since(s.t0).Nanoseconds())
	if left <= 0 {
		return stopNone
	}
	end := time.NewTimer(left)
	defer end.Stop()
	select {
	case <-end.C:
		return stopNone
	case <-ctx.Done():
		return stopSignal
	}
}

// dispatch launches every entry whose planned time has passed, checking every tickEvery
// (TRF-3), until all are launched or dropped, ctx ends or a step misses, and returns which. A
// stop recorded before a batch reads its time wins (batch), so no entry planned at or after the
// stop is launched. The drops of a batch reach the evaluator once stopMu is free: the step one
// of them finishes may miss and take stopMu for the stop's time.
func (s *sender) dispatch(ctx, reqCtx context.Context) stopReason {
	var early <-chan struct{} // nil without stop_early: never ready
	if s.eval != nil {
		early = s.eval.stop()
	}
	ticks := s.ticks
	if ticks == nil {
		tick := time.NewTicker(tickEvery)
		defer tick.Stop()
		ticks = tick.C
	}
	var dropped []Entry
	for {
		select {
		case <-ctx.Done():
		case <-early:
		case <-ticks:
		}
		var why stopReason
		dropped, why = s.batch(ctx, reqCtx, dropped[:0])
		for _, e := range dropped {
			s.eval.dropped(e)
		}
		if why != launching {
			return why
		}
	}
}

// batch, holding stopMu, ends the launching when a stop_early stop has been recorded or ctx has
// ended, or else launches every entry planned by now; launching is over too once it has reached
// every entry. It returns why launching is over, or launching, and appends to dropped the
// entries it dropped, for the evaluator (with stop_early).
func (s *sender) batch(ctx, reqCtx context.Context, dropped []Entry) ([]Entry, stopReason) {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	why := launching
	switch {
	case s.stopNs >= 0:
		why = stopEarly
	case ctx.Err() != nil:
		why = stopSignal
	default:
		now := time.Since(s.t0).Nanoseconds()
		s.frontier = now
		for ; s.next < len(s.entries) && s.entries[s.next].PlannedNs <= now; s.next++ {
			if !s.launch(reqCtx, s.entries[s.next]) && s.eval != nil {
				dropped = append(dropped, s.entries[s.next])
			}
		}
		if s.next == len(s.entries) {
			why = stopNone
		}
	}
	s.over = why != launching
	return dropped, why
}

// launch starts entry e's goroutine in an in-flight slot and returns true, or, when maxInFlight
// are taken, counts it a generator drop and returns false.
func (s *sender) launch(ctx context.Context, e Entry) bool {
	if !s.takeSlot() {
		s.drops++
		return false
	}
	s.launched++
	s.wg.Add(1)
	go s.send(ctx, e, 1, hint{}, 0)
	return true
}

// takeSlot takes an in-flight slot unless maxInFlight are taken. It never waits: the
// dispatcher and the retry timers both call it.
func (s *sender) takeSlot() bool {
	for {
		n := s.inFlight.Load()
		if n >= s.capacity {
			return false
		}
		if s.inFlight.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// send makes attempt `attempt` of entry e in the slot taken for it, frees the slot and records
// the attempt; h and jitter are what a retry waited for. After a 429 with a hint it schedules
// the next attempt, at most retries.MaxExtra of them, to the same instance with the same IDs,
// for once the hint plus the jitter has passed since the 429 arrived (TRF-5). Any other outcome
// ends the entry's attempts.
func (s *sender) send(ctx context.Context, e Entry, attempt int, h hint, jitter int64) {
	defer s.wg.Done()
	r := s.attempt(ctx, e)
	s.inFlight.Add(-1)
	if attempt == 1 && r.sentNs != nil {
		s.written.Add(1)
	}
	s.record(e, attempt, h, jitter, r)
	if attempt == 1 && s.eval != nil {
		s.eval.initialDone(e, r)
	}
	if r.status != http.StatusTooManyRequests || attempt > s.retries.MaxExtra {
		return
	}
	next, ok := chooseHint(r.retryAfterMs, r.retryAfter)
	if !ok {
		return
	}
	nextJitter := jitterNs(s.seed, e.I, attempt+1, s.retries.JitterMaxMs)
	wait := time.Duration(next.ms)*time.Millisecond + time.Duration(nextJitter)
	s.retryAt(ctx, r.ended.Add(wait), e, attempt+1, next, nextJitter)
}

// retryAt schedules attempt `attempt` of entry e for the monotonic time at, on a timer that
// holds no in-flight slot. Once launching has stopped the retry is not made (plan choice), and
// with pendingCap retries already waiting it is a retry drop.
func (s *sender) retryAt(ctx context.Context, at time.Time, e Entry, attempt int, h hint, jitter int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.abandoned:
		return
	case len(s.pending) >= s.pendingCap:
		s.retryDrops++
		return
	}
	p := &pendingRetry{}
	s.pending[p] = struct{}{}
	s.wg.Add(1)
	p.timer = time.AfterFunc(time.Until(at), func() { s.fire(ctx, p, e, attempt, h, jitter) })
}

// fire is retry p's timer: unless launching stopped meanwhile (abandonRetries has dropped it),
// the retry takes an in-flight slot and is made, or, with none free, is a retry drop. Whoever
// removes p from pending ends its WaitGroup count.
func (s *sender) fire(ctx context.Context, p *pendingRetry, e Entry, attempt int, h hint, jitter int64) {
	s.mu.Lock()
	_, waiting := s.pending[p]
	delete(s.pending, p)
	slot := waiting && s.takeSlot()
	if waiting && !slot {
		s.retryDrops++
	}
	s.mu.Unlock()
	switch {
	case !waiting:
	case !slot:
		s.wg.Done()
	default:
		s.send(ctx, e, attempt, h, jitter)
	}
}

// abandonRetries drops every retry still waiting for its hint, and every one an attempt in
// flight would schedule later: launching has stopped.
func (s *sender) abandonRetries() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abandoned = true
	for p := range s.pending {
		p.timer.Stop()
		delete(s.pending, p)
		s.wg.Done()
	}
}

// attempt makes one GET /hit for entry e and returns what it saw.
func (s *sender) attempt(ctx context.Context, e Entry) result {
	tr := &attemptTrace{t0: s.t0}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, tr.hooks()), http.MethodGet,
		s.urls[e.Instance-1], nil)
	if err != nil { // main checked every target: a request to it can always be made
		r := s.ended(tr)
		r.errCode = ptr(errTransport)
		return r
	}
	req.Body = emptyBody{}
	req.Header.Set("X-User-ID", e.User)
	req.Header.Set("X-Request-ID", e.RequestID)
	resp, err := s.client.Do(req)
	if err != nil {
		r := s.ended(tr)
		r.errCode = failure(err)
		return r
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	r := s.ended(tr)
	_ = resp.Body.Close() // read or failed: closing changes nothing recorded
	if err != nil {
		// The response broke off: no response recorded, the latency running to the failure.
		r.errCode = failure(err)
		return r
	}
	r.status = resp.StatusCode
	if values := resp.Header.Values("Retry-After"); len(values) > 0 {
		r.retryAfter = values
	}
	f := parseBody(body)
	r.arrivalNs, r.decisionNs, r.retryAfterMs = f.arrivalNs, f.decisionNs, f.retryAfterMs
	if r.status != http.StatusOK {
		r.errCode = f.errCode
	}
	return r
}

// ended returns the attempt's trace as a result ended now, as its response has been read or it
// has failed. An attempt that was written (WroteRequest, sent_ns) gets its latency, from
// WroteRequest to now: to its response, or to its timeout or transport failure (DESIGN §B4
// latency, monotonic ns). One never written keeps a null latency. The clock is read after the
// trace, so the latency is never negative.
func (s *sender) ended(tr *attemptTrace) result {
	r := tr.result()
	if r.sentNs != nil {
		latency := time.Since(s.t0).Nanoseconds() - *r.sentNs
		r.latency = &latency
	}
	return r
}

// failure classifies an attempt that got no response: the client's timeout, or a connection
// failure (plan choice).
func failure(err error) *string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return ptr(errTimeout)
	}
	return ptr(errTransport)
}

// ptr returns a pointer to a copy of v.
func ptr[T any](v T) *T { return &v }

// isErrorCode reports whether s has the shape of HTTP-7's stable error codes, 1 to 64 of
// [a-z0-9_]; a response's error that does not is recorded as null.
func isErrorCode(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// bodyFields are the values gen.jsonl takes from a response body (HTTP-6, HTTP-7, HTTP-9);
// nil when absent or malformed.
type bodyFields struct {
	errCode      *string
	retryAfterMs *int64
	arrivalNs    *int64
	decisionNs   *int64
}

// parseBody reads a response body: a JSON object whose error is a code (isErrorCode),
// retry_after_ms a whole number ≥ 0, and arrival_ns and decision_ns decimal strings of a
// positive int64. A field that breaks its rule is absent; a body that is not a JSON object has
// none (input from the API is checked, AGENTS.md §1).
func parseBody(body []byte) bodyFields {
	var m map[string]json.RawMessage
	if len(body) > maxBodyBytes || json.Unmarshal(body, &m) != nil {
		return bodyFields{}
	}
	var f bodyFields
	var s string
	if raw, ok := m["error"]; ok && json.Unmarshal(raw, &s) == nil && isErrorCode(s) {
		f.errCode = ptr(s)
	}
	if raw, ok := m["retry_after_ms"]; ok {
		if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil && n >= 0 {
			f.retryAfterMs = &n
		}
	}
	f.arrivalNs = decimalField(m, "arrival_ns")
	f.decisionNs = decimalField(m, "decision_ns")
	return f
}

// decimalField returns m[key] when it is a JSON string holding a positive int64 in decimal.
func decimalField(m map[string]json.RawMessage, key string) *int64 {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" || s[0] == '+' || s[0] == '-' {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

// record enqueues attempt `attempt` of entry e (TRF-10); h and jitter are the hint and jitter
// waited for before a retry.
func (s *sender) record(e Entry, attempt int, h hint, jitter int64, r result) {
	line := attemptLine{Kind: e.Kind, PlannedNs: e.PlannedNs, SentNs: r.sentNs, GotConnWaitNs: r.gotConnWaitNs,
		Instance: e.Instance, User: e.User, RequestID: e.RequestID, Attempt: attempt, Status: r.status,
		Error: r.errCode, Latency: r.latency, ArrivalNs: r.arrivalNs, DecisionNs: r.decisionNs}
	if e.DuplicateOf >= 0 {
		of := e.DuplicateOf
		line.DuplicateOf = &of
	}
	if attempt > 1 {
		line.Kind = kindRetry
		source, ms := h.source, h.ms
		line.HintSource, line.HintMs, line.JitterNs = &source, &ms, &jitter
	}
	s.log.Enqueue(line)
}

// attemptTrace holds one attempt's httptrace times, as offsets from t0. Its hooks may run on
// the Transport's own goroutines, hence the lock.
type attemptTrace struct {
	t0 time.Time

	mu      sync.Mutex
	getConn *int64
	gotConn *int64
	wrote   *int64 // WroteRequest without an error: sent_ns
}

func (a *attemptTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { a.mark(&a.getConn) },
		GotConn: func(httptrace.GotConnInfo) { a.mark(&a.gotConn) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				a.mark(&a.wrote)
			}
		},
	}
}

// mark sets *at to now, once.
func (a *attemptTrace) mark(at **int64) {
	now := time.Since(a.t0).Nanoseconds()
	a.mu.Lock()
	defer a.mu.Unlock()
	if *at == nil {
		*at = &now
	}
}

// result returns the attempt's trace so far, as a result with no response, ended now.
func (a *attemptTrace) result() result {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := result{sentNs: a.wrote, ended: time.Now()}
	if a.getConn != nil && a.gotConn != nil {
		wait := *a.gotConn - *a.getConn
		r.gotConnWaitNs = &wait
	}
	return r
}
