package core

import (
	"context"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// testRefunds returns refunds without workers (no attempt function) for the unit tests: a
// ready queue of capacity, a 1 h retention timer, the given backoff and the closing flag
// closing. t's cleanup stops it.
func testRefunds(t *testing.T, capacity int, reg *metrics.Registry, closing *atomic.Bool, backoffInitial, backoffMax time.Duration) *refunds {
	t.Helper()
	r := newRefunds(refundsConfig{
		capacity: capacity, lifetime: time.Hour, backoffInitial: backoffInitial, backoffMax: backoffMax,
		born: time.Now(), closing: closing, reg: reg,
	}, t.Context())
	t.Cleanup(r.stop)
	return r
}

// nextWithin takes the next ready job from r as a worker does (next), which puts its refund
// attempt in flight, and returns it with the attempt's context; a test that goes on as the
// worker would ends the attempt (endAttempt) before it retries or finishes the job. It fails
// the test when no job comes within 5 s, so an empty queue fails by assertion instead of
// blocking until the test binary's timeout. The wait runs in a goroutine; when the test gives
// up on it, r's cleanup stop ends it.
func nextWithin(t *testing.T, r *refunds) (*Permit, context.Context) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second) // started before the wait it bounds
	defer timeout.Stop()
	type job struct {
		p   *Permit
		ctx context.Context
		ok  bool
	}
	got := make(chan job, 1) // buffered: a wait the test gave up on still ends
	go func() {
		p, ctx, ok := r.next()
		got <- job{p, ctx, ok}
	}()
	select {
	case j := <-got:
		if !j.ok {
			t.Fatal("next() reported the refunds stopped, want a job")
		}
		return j.p, j.ctx
	case <-timeout.C:
		t.Fatal("next() returned no job within 5 s")
	}
	return nil, nil
}

// TestReadyQueueOrder (plan S6, L1; DESIGN §A5.6): 1,000 jobs with random release times
// (many equal), queued in random order into a queue of capacity 1,000, pop earliest release
// first, equal times in queuing order; a 1,001st is refused (full). Then through refunds: a job
// that a worker took and retried goes back with its original release time, so it pops before a
// job released later, and a job that turned FINAL leaves the queue.
func TestReadyQueueOrder(t *testing.T) {
	const n = 1_000
	rng := rand.New(rand.NewPCG(6, 1))
	var mu sync.Mutex
	q := readyQueue{cap: n, cond: sync.NewCond(&mu)}
	jobs := make([]*Permit, n)
	for i := range jobs {
		p := &Permit{index: -1, seq: uint64(i + 1)}
		p.releaseMono.Store(1 + rng.Int64N(300)) // 1,000 jobs over 300 values: many ties
		jobs[i] = p
	}
	mu.Lock()
	for _, i := range rng.Perm(n) {
		if !q.push(jobs[i]) {
			t.Fatalf("push %d of %d refused", i, n)
		}
	}
	if q.push(&Permit{index: -1}) {
		t.Error("a full queue accepted a job")
	}
	var got []*Permit
	for p := q.pop(); p != nil; p = q.pop() {
		got = append(got, p)
	}
	mu.Unlock()
	want := slices.Clone(jobs)
	slices.SortFunc(want, func(a, b *Permit) int {
		if ra, rb := a.releaseMono.Load(), b.releaseMono.Load(); ra != rb {
			return int(ra - rb)
		}
		return int(a.seq) - int(b.seq)
	})
	if !slices.Equal(got, want) {
		t.Fatalf("pop order differs from (release time, queuing order): %d jobs popped, want %d", len(got), n)
	}
	for _, p := range got {
		if p.index != -1 {
			t.Fatalf("a popped job keeps index %d, want -1", p.index)
		}
	}

	// Through refunds: A then B released; a worker takes A, whose attempt fails, and retries it
	// after a 1 ms backoff; C is released meanwhile. A comes back before B and C.
	reg := metrics.NewRegistry()
	var closing atomic.Bool
	r := testRefunds(t, 4, reg, &closing, time.Millisecond, time.Millisecond)
	sl := newSlots(4, 4, reg)
	release := func() *Permit {
		if !sl.tryAcquire() {
			t.Fatal("no free slot")
		}
		p := newPermit(sl, r, receipt.Receipt{}, "d1_job", time.Now())
		p.Release()
		time.Sleep(time.Millisecond) // a distinct, later release time for the next job
		return p
	}
	a, b := release(), release()
	if p, _ := nextWithin(t, r); p != a {
		t.Fatal("the worker did not take A, the earliest job, first")
	}
	r.endAttempt(a) // the attempt sent nothing, and the worker retries the job
	r.retry(a)
	c := release()
	queuedAgain := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return a.index >= 0
	}
	for deadline := time.Now().Add(5 * time.Second); !queuedAgain(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("A's 1 ms backoff did not put it back on the queue within 5 s")
		}
	}
	if p, _ := nextWithin(t, r); p != a {
		t.Fatal("the retried A did not come back first: a retry must keep its release time")
	}
	if a.retries != 1 {
		t.Errorf("A's backoff exponent after one retry: %d, want 1", a.retries)
	}
	// C turns FINAL (the drain ends for it): it leaves the queue, so only B is left.
	c.drainEnd()
	r.mu.Lock()
	queued := slices.Clone(r.queue.h)
	r.mu.Unlock()
	if len(queued) != 1 || queued[0] != b {
		t.Errorf("queue after C turned FINAL holds %d jobs (B first: %v), want only B", len(queued), len(queued) > 0 && queued[0] == b)
	}
}

// TestReadyQueueSkipsFinal (plan S6 step 5; DESIGN §A5.5): a job that is FINAL while still on
// the ready queue is skipped. With a FINAL job (the earlier release time) and a RELEASED one
// queued, a worker takes the RELEASED one, and the FINAL one has left the queue.
func TestReadyQueueSkipsFinal(t *testing.T) {
	reg := metrics.NewRegistry()
	var closing atomic.Bool
	r := testRefunds(t, 2, reg, &closing, time.Millisecond, time.Millisecond)
	final := &Permit{life: newLifecycle(stateFinal), index: -1, seq: 1}
	final.releaseMono.Store(1)
	released := &Permit{life: newLifecycle(stateReleased), index: -1, seq: 2}
	released.releaseMono.Store(2)
	r.mu.Lock()
	pushed := r.queue.push(final) && r.queue.push(released)
	r.mu.Unlock()
	if !pushed {
		t.Fatal("the queue of capacity 2 refused one of 2 jobs")
	}
	if p, _ := nextWithin(t, r); p != released {
		t.Fatalf("a worker took the %v job, want the RELEASED one: dequeue skips jobs already FINAL", p.life.load())
	}
	r.mu.Lock()
	left, finalIndex := len(r.queue.h), final.index
	r.mu.Unlock()
	if left != 0 || finalIndex != -1 {
		t.Errorf("after the dequeue: %d job(s) queued, the FINAL job at index %d; want none and -1", left, finalIndex)
	}
}

// TestRefundBackoffBounds (plan S6, L1; DESIGN §A5.6): 10,000 backoff delays for each n = 0 … 6
// lie in [0.5, 1.0] × min(1 s, 50 ms × 2^n) and spread over that range; the factor's ends give
// half the cap and the cap.
func TestRefundBackoffBounds(t *testing.T) {
	reg := metrics.NewRegistry()
	var closing atomic.Bool
	r := testRefunds(t, 1, reg, &closing, 50*time.Millisecond, time.Second)
	caps := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond, time.Second, time.Second}
	for n, cp := range caps {
		lo, hi := cp/2, cp
		minSeen, maxSeen := hi, lo
		for range 10_000 {
			d := r.backoff(n)
			if d < lo || d > hi {
				t.Fatalf("n = %d: delay %v outside [%v, %v]", n, d, lo, hi)
			}
			minSeen, maxSeen = min(minSeen, d), max(maxSeen, d)
		}
		if tenth := (hi - lo) / 10; minSeen > lo+tenth || maxSeen < hi-tenth {
			t.Errorf("n = %d: delays span [%v, %v], want them to reach within %v of %v and %v", n, minSeen, maxSeen, tenth, lo, hi)
		}
		if got := backoffDelay(n, 50*time.Millisecond, time.Second, 0); got != lo {
			t.Errorf("n = %d, factor 0.5: %v, want %v", n, got, lo)
		}
		if got := backoffDelay(n, 50*time.Millisecond, time.Second, 1); got != hi {
			t.Errorf("n = %d, factor 1.0: %v, want %v", n, got, hi)
		}
	}
	if got := backoffDelay(0, 2*time.Second, time.Second, 1); got != time.Second {
		t.Errorf("an initial delay above the maximum: %v, want the maximum, 1 s", got)
	}
}

// TestRefundResultClassification (plan S6, L1; DESIGN §A5.6, §A4.9; SPEC API-13 … API-19):
// every §A4.9 refund code and every transport class maps to final or retry: codes 3, 4, 10, 11
// are final (applied with the reply's t, already_applied with its credit_us); codes 20–25,
// unavailable, cancelled, outcome_unknown and capacity_error are retried, codes 20 and 22 with
// an early epoch read. A reply that fails the §A4.9 checks after the adapter (a wrapped
// adapter's) is retried, never final. sent is the response's SentCommands.
func TestRefundResultClassification(t *testing.T) {
	const (
		tUs    = 1_760_000_001_000_000
		expiry = 1_760_000_060_000_000
		period = 1_000_000_000
	)
	ok := func(code, t int64, reason string, vals ...int64) store.Response {
		elems := []resp.Value{{Kind: resp.Int, Int: code}, {Kind: resp.Int, Int: t}, {Kind: resp.Bulk, Str: reason}}
		for _, v := range vals {
			elems = append(elems, resp.Value{Kind: resp.Int, Int: v})
		}
		return store.Response{Outcome: store.OK, Sent: true, SentCommands: 1,
			Reply: store.Reply{Code: code, TUs: t, Reason: reason, Elems: elems}}
	}
	retry := func(sent int) attemptResult { return attemptResult{sent: sent} }
	cases := []struct {
		name string
		resp store.Response
		want attemptResult
	}{
		{"3 applied", ok(3, tUs, "", period), attemptResult{final: true, result: Applied, creditNs: 1_760_000_001_000_000_000, sent: 1}},
		{"3 applied, credited 0", ok(3, tUs, "", 0), attemptResult{final: true, result: Applied, creditNs: 1_760_000_001_000_000_000, sent: 1}},
		{"3 applied at t 0", ok(3, 0, "", period), attemptResult{final: true, result: Applied, sent: 1}},
		{"3 credited one past the period", ok(3, tUs, "", period+1), retry(1)},
		{"3 credited negative", ok(3, tUs, "", -1), retry(1)},
		{"3 without its value", ok(3, tUs, ""), retry(1)},
		{"3 with a fifth element", ok(3, tUs, "", period, 1), retry(1)},
		{"3 at TBOUND", ok(3, tBound, "", period), retry(1)},
		{"4 already applied", ok(4, tUs, "", tUs-5), attemptResult{final: true, result: AlreadyApplied, creditNs: 1_760_000_000_999_995_000, sent: 1}},
		{"4 credit time equal to t", ok(4, tUs, "", tUs), attemptResult{final: true, result: AlreadyApplied, creditNs: 1_760_000_001_000_000_000, sent: 1}},
		{"4 credit time 0", ok(4, tUs, "", 0), attemptResult{final: true, result: AlreadyApplied, creditNs: 0, sent: 1}},
		{"4 with a fifth element", ok(4, tUs, "", tUs-5, 1), retry(1)},
		{"4 credit time after t", ok(4, tUs, "", tUs+1), retry(1)},
		{"4 credit time at the receipt's expiry", ok(4, expiry+1, "", expiry), retry(1)},
		{"4 credit time just before the receipt's expiry", ok(4, expiry+1, "", expiry-1), attemptResult{final: true, result: AlreadyApplied, creditNs: 1_760_000_059_999_999_000, sent: 1}},
		{"10 receipt invalid (epoch)", ok(10, tUs, "epoch"), attemptResult{final: true, result: ReceiptInvalid, sent: 1}},
		{"10 receipt invalid (generation)", ok(10, tUs, "generation_or_sequence"), attemptResult{final: true, result: ReceiptInvalid, sent: 1}},
		{"11 receipt expired", ok(11, tUs, "expired"), attemptResult{final: true, result: ReceiptExpired, sent: 1}},
		{"20 config", ok(20, 0, "epoch_or_fingerprint"), attemptResult{sent: 1, epochWide: true}},
		{"21 clock", ok(21, 0, "clock"), retry(1)},
		{"22 epoch-wide integrity", ok(22, 0, "integrity_hold"), attemptResult{sent: 1, epochWide: true}},
		{"23 per-key integrity", ok(23, tUs, "refund_bucket_missing"), retry(1)},
		{"24 capacity (live)", ok(24, tUs, "live_markers"), retry(1)},
		{"24 capacity (per key)", ok(24, tUs, "key_markers"), retry(1)},
		{"25 bad call", ok(25, 0, "keys_mismatch"), retry(1)},
		{"an acquire code", ok(1, tUs, "", 1, 1, 1, 1), retry(1)},
		{"unavailable, nothing sent", store.Response{Outcome: store.Unavailable}, retry(0)},
		{"cancelled, nothing sent", store.Response{Outcome: store.Cancelled}, retry(0)},
		{"outcome_unknown, sent", store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1}, retry(1)},
		{"outcome_unknown after a NOSCRIPT reload", store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 2, NoScript: true}, retry(2)},
		{"capacity_error (OOM)", store.Response{Outcome: store.CapacityError, Sent: true, SentCommands: 1, StoreError: "OOM command not allowed"}, retry(1)},
	}
	applied2 := ok(3, tUs, "", period)
	applied2.SentCommands, applied2.NoScript = 2, true
	cases = append(cases, struct {
		name string
		resp store.Response
		want attemptResult
	}{"3 applied after a NOSCRIPT reload", applied2, attemptResult{final: true, result: Applied, creditNs: 1_760_000_001_000_000_000, sent: 2}})
	for _, c := range cases {
		if got := classifyRefund(c.resp, period, expiry); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestDrainCountsAttemptInFlight (plan S6 step 7; SPEC API-1d, API-1e): a refund whose command
// was sent and is still in flight at the drain deadline is cancelled, and counted, before the
// drain's end builds its event. The event, taken by a listener that is still running, is
// unresolved (closed) with Attempts 1; drain returns soon after its deadline, the permit FINAL
// with its slots freed.
func TestDrainCountsAttemptInFlight(t *testing.T) {
	const wait = 5 * time.Second
	reg := metrics.NewRegistry()
	var mu sync.Mutex
	var evs []RefundEvent
	lis := newListener(func(ev RefundEvent) {
		mu.Lock()
		defer mu.Unlock()
		evs = append(evs, ev)
	}, 4, reg)
	lis.start()
	t.Cleanup(func() {
		lis.stop()
		lis.waitExit(time.Now().Add(wait))
	})
	reached := make(chan struct{})
	var once sync.Once
	var closing atomic.Bool
	r := newRefunds(refundsConfig{
		capacity: 1, lifetime: time.Hour, backoffInitial: time.Millisecond, backoffMax: time.Millisecond,
		born: time.Now(), closing: &closing, reg: reg, lis: lis,
		// A refund command written whose reply never comes: the adapter's outcome_unknown with
		// one command sent (§A5.4), which is retried, once the call is cancelled or, at the
		// latest, its D (2 s, the default) passes.
		attempt: func(ctx context.Context, _ *Permit) attemptResult {
			once.Do(func() { close(reached) })
			d := time.NewTimer(2 * time.Second)
			defer d.Stop()
			select {
			case <-ctx.Done():
			case <-d.C:
			}
			return attemptResult{sent: 1}
		},
	}, t.Context())
	t.Cleanup(r.stop)
	r.startWorkers(1)
	sl := newSlots(1, 1, reg)
	if !sl.tryAcquire() {
		t.Fatal("no free slot")
	}
	p := newPermit(sl, r, receipt.Receipt{}, "d1_inflight", time.Now())
	timeout := time.NewTimer(wait) // started before the wait it bounds
	defer timeout.Stop()
	p.Release()
	select {
	case <-reached:
	case <-timeout.C:
		t.Fatalf("the refund attempt did not start within %v", wait)
	}

	const drain = 100 * time.Millisecond
	start := time.Now()
	r.drain(start.Add(drain))
	if d := time.Since(start); d < drain || d > drain+500*time.Millisecond {
		t.Errorf("drain returned after %v, want soon after its %v deadline (+ at most 500 ms)", d, drain)
	}
	lis.waitIdle(time.Now().Add(wait))
	mu.Lock()
	got := slices.Clone(evs)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("%d events %+v, want 1", len(got), got)
	}
	ev := got[0]
	ev.ReleaseMonoNs, ev.FinalMonoNs = 0, 0
	if want := (RefundEvent{DeductionID: "d1_inflight", Result: Unresolved, Reason: UnresolvedClosed, Attempts: 1}); ev != want {
		t.Errorf("event %+v, want %+v: the command in flight was sent, so it counts (API-1d)", got[0], want)
	}
	if st := p.life.load(); st != stateFinal {
		t.Errorf("permit %v after the drain, want FINAL", st)
	}
	if rs, fs := sl.inUse(); rs != 0 || fs != 0 {
		t.Errorf("slots in use %d and %d after the drain, want 0 and 0", rs, fs)
	}
}

// TestListenerGraceBudget (SPEC HOST-6; DESIGN §B10; the F1 decision on Close's listener
// grace): listenerGrace, the time Close still gives the listener past a drain that used its
// whole timeout, is at most 100 ms, its share of the harness's shutdown budget: the
// orchestrator kills a hitapi 45 s after its SIGTERM (P 5 s + drain_timeout 35 s + 5 s,
// cmd/orchestrator killGrace), and hitapi's timed waits add up to P 5 s + drain 35 s +
// listenerGrace + the run log's 4 s drain (cmd/hitapi logDrainTimeout), 44.1 s at 100 ms,
// which leaves 0.9 s for the steps hitapi does not time (cmd/hitapi TestShutdownBudget pins
// the sum). TestCloseListenerGrace pins the other side: Close gives a blocked listener the
// whole 100 ms.
func TestListenerGraceBudget(t *testing.T) {
	const budget = 100 * time.Millisecond
	if grace := listenerGrace; grace > budget {
		t.Errorf("listenerGrace = %v, want at most %v: hitapi's timed shutdown waits (P 5 s + drain 35 s + the grace + its 4 s run-log drain) would leave less than 0.9 s of the 45 s kill grace", grace, budget)
	}
}

// TestListenerDeadline (SPEC API-1d, API-1e; the F1 decision on Close's listener grace): the
// end of the listener's window after the drain, by hand from a fixed instant now, the drain's
// end, far from the test's own clock. A drain deadline later than now + 100 ms ends the
// window; an earlier one, now itself, or one the drain has already passed gives now + 100 ms.
// The window ends at the later of the two, never their sum or the earlier one. Both sides of
// the boundary are pinned 1 ns from it; at it the two agree.
func TestListenerDeadline(t *testing.T) {
	now := time.Date(2001, time.February, 3, 4, 5, 6, 7, time.UTC)
	for _, c := range []struct {
		name  string
		drain time.Duration // the drain deadline, from now
		want  time.Duration // the window's end, from now
	}{
		{"drain deadline 1 s away: the deadline", time.Second, time.Second},
		{"drain deadline 1 ns past the grace: the deadline", 100*time.Millisecond + 1, 100*time.Millisecond + 1},
		{"drain deadline at the grace: either", 100 * time.Millisecond, 100 * time.Millisecond},
		{"drain deadline 1 ns short of the grace: the grace", 100*time.Millisecond - 1, 100 * time.Millisecond},
		{"drain deadline 1 ns away: the grace", 1, 100 * time.Millisecond},
		{"drain ended at its deadline: the grace", 0, 100 * time.Millisecond},
		{"drain ended 30 ms past its deadline: the grace", -30 * time.Millisecond, 100 * time.Millisecond},
		{"drain ended 1 h past its deadline: the grace", -time.Hour, 100 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listenerDeadline(now, now.Add(c.drain)); !got.Equal(now.Add(c.want)) {
				t.Errorf("listenerDeadline(now, now + %v) = now + %v, want now + %v", c.drain, got.Sub(now), c.want)
			}
		})
	}
}

// deliveredEvent is a refund event as a test listener received it, with the slots of each
// kind its permit's slots had in use at that moment.
type deliveredEvent struct {
	ev                RefundEvent
	receipts, refunds int
}

// slotListener returns a listener for the slots sl that hands each event to events with the
// slots in use when it arrives. events has room for every event the test could see, so the
// callback never waits on it; t's cleanup stops the listener.
func slotListener(t *testing.T, sl *slots, events chan deliveredEvent, reg *metrics.Registry) *listener {
	t.Helper()
	lis := newListener(func(ev RefundEvent) {
		rs, fs := sl.inUse()
		events <- deliveredEvent{ev: ev, receipts: rs, refunds: fs}
	}, cap(events), reg)
	lis.start()
	t.Cleanup(func() {
		lis.stop()
		lis.waitExit(time.Now().Add(5 * time.Second))
	})
	return lis
}

// retentionRig is one subtest of TestRetentionCountsAttemptInFlight: refunds with one worker
// whose attempts run attempt, a receipt lifetime, a listener that hands each event to events
// with the slots in use at that moment, and one released permit, whose retention timer fires
// a lifetime after sent.
type retentionRig struct {
	r      *refunds
	reg    *metrics.Registry
	sl     *slots
	p      *Permit
	sent   time.Time
	events chan deliveredEvent
}

// newRetentionRig builds the rig and releases its permit; t's cleanup stops the workers, then
// the listener.
func newRetentionRig(t *testing.T, lifetime time.Duration, attempt func(context.Context, *Permit) attemptResult) *retentionRig {
	t.Helper()
	reg := metrics.NewRegistry()
	sl := newSlots(1, 1, reg)
	events := make(chan deliveredEvent, 4)
	lis := slotListener(t, sl, events, reg)
	var closing atomic.Bool
	r := newRefunds(refundsConfig{
		capacity: 1, lifetime: lifetime, backoffInitial: time.Millisecond, backoffMax: time.Millisecond,
		born: time.Now(), closing: &closing, reg: reg, lis: lis, attempt: attempt,
	}, t.Context())
	t.Cleanup(r.stop)
	r.startWorkers(1)
	if !sl.tryAcquire() {
		t.Fatal("no free slot")
	}
	sent := time.Now()
	p := newPermit(sl, r, receipt.Receipt{}, "d1_retention", sent)
	p.Release()
	return &retentionRig{r: r, reg: reg, sl: sl, p: p, sent: sent, events: events}
}

// TestRetentionCountsAttemptInFlight (SPEC API-1d, API-1h, REF-4a; DESIGN §A5.5, §A5.6): a
// refund whose first command was sent and is still in flight when its retention timer fires
// ends unresolved (retention) with Attempts 1, not 0: the command was sent, so it counts.
//   - The timer cancels the attempt, and the worker counts the command before the one event
//     goes out, soon after the timer, not when the attempt's 2 s D would have ended it.
//   - While the cancelled attempt has not returned yet (the test fires the timer, then lets the
//     attempt return), the timer has ended the permit as far as the F1 hand-off lets it: FINAL,
//     out of the pending set, its timers stopped. The slots and the event wait for the attempt:
//     API-1h keeps the slots until the final event (DESIGN §A5.5's retention hand-off). Once
//     the attempt returns, its worker frees the slots and then emits the one event, counted:
//     while the test holds the slots' lock the worker cannot emit, and the listener reads no
//     slot in use when the event arrives.
func TestRetentionCountsAttemptInFlight(t *testing.T) {
	const (
		wait     = 5 * time.Second
		lifetime = 300 * time.Millisecond
	)
	want := RefundEvent{DeductionID: "d1_retention", Result: Unresolved, Reason: UnresolvedRetention, Attempts: 1}
	t.Run("the attempt ends at its cancel", func(t *testing.T) {
		reached := make(chan struct{})
		var once sync.Once
		rig := newRetentionRig(t, lifetime, func(ctx context.Context, _ *Permit) attemptResult {
			// A refund command written whose reply never comes: the adapter's outcome_unknown
			// with one command sent (§A5.4), once the call is cancelled or, at the latest, its D
			// passes.
			once.Do(func() { close(reached) })
			d := time.NewTimer(2 * time.Second)
			defer d.Stop()
			select {
			case <-ctx.Done():
			case <-d.C:
			}
			return attemptResult{sent: 1}
		})
		timeout := time.NewTimer(wait) // started before the waits it bounds
		defer timeout.Stop()
		select {
		case <-reached:
		case <-timeout.C:
			t.Fatalf("the refund attempt did not start within %v", wait)
		}
		var got deliveredEvent
		select {
		case got = <-rig.events:
		case <-timeout.C:
			t.Fatalf("no event within %v", wait)
		}
		arrived := time.Since(rig.sent)
		ev := got.ev
		ev.ReleaseMonoNs, ev.FinalMonoNs = 0, 0
		if ev != want {
			t.Errorf("event %+v, want %+v: the command in flight was sent, so it counts (API-1d)", got.ev, want)
		}
		if got.receipts != 0 || got.refunds != 0 {
			t.Errorf("slots in use %d and %d when the listener received the event, want 0 and 0: they are freed before the event (API-1h)", got.receipts, got.refunds)
		}
		if arrived < lifetime || arrived > lifetime+500*time.Millisecond {
			t.Errorf("the event came %v after the acquire's send, want soon after the %v lifetime (+ at most 500 ms): the timer cancels the attempt in flight instead of waiting out its 2 s D", arrived, lifetime)
		}
		select {
		case extra := <-rig.events:
			t.Errorf("a second event %+v, want exactly one (API-1d)", extra.ev)
		case <-time.After(100 * time.Millisecond):
		}
		if st := rig.p.life.load(); st != stateFinal {
			t.Errorf("permit %v after its retention, want FINAL", st)
		}
		if rs, fs := rig.sl.inUse(); rs != 0 || fs != 0 {
			t.Errorf("slots in use %d and %d after the retention, want 0 and 0", rs, fs)
		}
	})

	t.Run("the slots and the event wait for the cancelled attempt", func(t *testing.T) {
		reached, cancelled, gate := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var reachOnce, cancelOnce, gateOnce sync.Once
		openGate := func() { gateOnce.Do(func() { close(gate) }) }
		// A 1 h lifetime: the test fires the retention timer itself (expire is its callback),
		// so the timer's effects are all done when the call returns.
		rig := newRetentionRig(t, time.Hour, func(ctx context.Context, _ *Permit) attemptResult {
			// A sent command whose call returns only once the test opens gate after the cancel.
			reachOnce.Do(func() { close(reached) })
			<-ctx.Done()
			cancelOnce.Do(func() { close(cancelled) })
			<-gate
			return attemptResult{sent: 1}
		})
		t.Cleanup(openGate)            // registered after the rig: runs before its workers are waited for
		timeout := time.NewTimer(wait) // started before the waits it bounds
		defer timeout.Stop()
		select {
		case <-reached:
		case <-timeout.C:
			t.Fatalf("the refund attempt did not start within %v", wait)
		}
		rig.p.expire() // the retention timer fires while the attempt is in flight
		select {
		case <-cancelled:
		case <-timeout.C:
			t.Fatalf("the retention timer did not cancel the attempt in flight within %v", wait)
		}
		rig.r.mu.Lock()
		_, pending := rig.r.pending[rig.p]
		_, armed := rig.r.timers[rig.p]
		retention, queued := rig.p.retention, rig.p.index >= 0
		rig.r.mu.Unlock()
		if st := rig.p.life.load(); st != stateFinal || pending || queued || armed || retention != nil {
			t.Fatalf("after the timer: %v, pending %v, queued %v, retention timer armed %v (set %v); want FINAL, none of them (DESIGN §A5.5)",
				st, pending, queued, armed, retention != nil)
		}
		if pend := gaugeValue(t, rig.reg.Snapshot(), "rl_pending_refunds", ""); pend != 0 {
			t.Fatalf("rl_pending_refunds %v after the timer, want 0", pend)
		}
		if rs, fs := rig.sl.inUse(); rs != 1 || fs != 1 {
			t.Fatalf("slots in use %d and %d after the timer, want 1 and 1: a RELEASED permit holds both until its final event (API-1h), which waits for the attempt in flight", rs, fs)
		}
		if n := refundEvents(t, rig.reg); n != 0 {
			t.Fatalf("rl_refund_results_total sums to %d after the timer, want 0: the event waits until the worker has counted the attempt (API-1d)", n)
		}

		// The attempt returns while the test holds the slots' lock: its worker counts the
		// command and ends the attempt, which frees the slots first, so it cannot emit yet.
		func() {
			rig.sl.mu.Lock()
			defer rig.sl.mu.Unlock()
			openGate()
			pollUntil(t, "end of the worker's attempt", func() bool {
				rig.r.mu.Lock()
				defer rig.r.mu.Unlock()
				return !rig.p.attempting
			})
			time.Sleep(50 * time.Millisecond) // an emit that did not wait for the slots would have counted by now
			if n := refundEvents(t, rig.reg); n != 0 {
				t.Errorf("rl_refund_results_total sums to %d while the permit's slots were still in use, want 0: the worker frees the slots, then emits the event", n)
			}
		}()
		var got deliveredEvent
		select {
		case got = <-rig.events:
		case <-timeout.C:
			t.Fatalf("no event within %v of the attempt's return", wait)
		}
		ev := got.ev
		ev.ReleaseMonoNs, ev.FinalMonoNs = 0, 0
		if ev != want {
			t.Errorf("event %+v, want %+v: the cancelled command was sent, so it counts (API-1d)", got.ev, want)
		}
		if got.receipts != 0 || got.refunds != 0 {
			t.Errorf("slots in use %d and %d when the listener received the event, want 0 and 0: the worker frees them before it emits", got.receipts, got.refunds)
		}
		select {
		case extra := <-rig.events:
			t.Errorf("a second event %+v, want exactly one (API-1d)", extra.ev)
		case <-time.After(100 * time.Millisecond):
		}
		if rs, fs := rig.sl.inUse(); rs != 0 || fs != 0 {
			t.Errorf("slots in use %d and %d after the event, want 0 and 0", rs, fs)
		}
	})
}

// TestSlotsFreedBeforeEvent (SPEC API-1d, API-1h; DESIGN §A5.5): a transition that frees a
// permit's slots and emits its final event itself, with no refund attempt in flight to hand
// them to, frees the slots first and emits the event after (apply), so the permit holds its
// slots until its final event (API-1h) and the listener never finds them still in use. There
// are four such transitions:
//   - a worker's final result (RELEASED → FINAL, applied);
//   - the retention timer with no attempt in flight (RELEASED → FINAL, unresolved (retention));
//   - the end of Close's drain (RELEASED → FINAL, unresolved (closed));
//   - a release() after Close began (OPEN → FINAL, unresolved (closed)).
//
// Each fires while the test holds the slots' lock: the permit turns FINAL, but its transition
// cannot free the slots yet, so no final event may be counted for 50 ms. Once the lock is
// released, exactly one event comes, and the listener reads no slot in use when it arrives.
// TestRetentionCountsAttemptInFlight pins the same order for a retention that hands its
// effects to the attempt in flight.
func TestSlotsFreedBeforeEvent(t *testing.T) {
	const (
		wait     = 5 * time.Second
		creditNs = 1_000
	)
	// rig is one subtest's refunds and its permit, which holds the only slot pair; the worker's
	// attempt (the final-result case's) closes reached when it starts and returns once
	// openGate is called.
	type rig struct {
		r        *refunds
		p        *Permit
		closing  *atomic.Bool
		reached  chan struct{}
		openGate func()
	}
	cases := []struct {
		name string
		// start puts the permit in the state the transition starts from and returns the call
		// that fires the transition.
		start func(t *testing.T, x *rig) func()
		want  RefundEvent
	}{
		{
			name: "a worker's final result",
			start: func(t *testing.T, x *rig) func() {
				x.r.startWorkers(1)
				x.p.Release()
				select {
				case <-x.reached:
				case <-time.After(wait):
					t.Fatalf("the refund attempt did not start within %v", wait)
				}
				return x.openGate // the attempt returns applied, and its worker fires the final result
			},
			want: RefundEvent{DeductionID: "d1_order", Result: Applied, CreditDecisionNs: creditNs, Attempts: 1},
		},
		{
			name: "a retention with no attempt in flight",
			start: func(_ *testing.T, x *rig) func() {
				x.p.Release()     // pending and queued; no worker runs, so no attempt is in flight
				return x.p.expire // the retention timer's callback
			},
			want: RefundEvent{DeductionID: "d1_order", Result: Unresolved, Reason: UnresolvedRetention},
		},
		{
			name: "the drain's end",
			start: func(_ *testing.T, x *rig) func() {
				x.p.Release()
				return func() { x.r.drain(time.Now()) } // the drain deadline has passed with the refund still pending
			},
			want: RefundEvent{DeductionID: "d1_order", Result: Unresolved, Reason: UnresolvedClosed},
		},
		{
			name: "a release after Close began",
			start: func(_ *testing.T, x *rig) func() {
				x.closing.Store(true) // what Close does first
				return x.p.Release
			},
			want: RefundEvent{DeductionID: "d1_order", Result: Unresolved, Reason: UnresolvedClosed},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := metrics.NewRegistry()
			sl := newSlots(1, 1, reg)
			events := make(chan deliveredEvent, 4)
			lis := slotListener(t, sl, events, reg)
			var closing atomic.Bool
			reached, gate := make(chan struct{}), make(chan struct{})
			var reachOnce, gateOnce sync.Once
			openGate := func() { gateOnce.Do(func() { close(gate) }) }
			r := newRefunds(refundsConfig{
				capacity: 1, lifetime: time.Hour, backoffInitial: time.Millisecond, backoffMax: time.Millisecond,
				born: time.Now(), closing: &closing, reg: reg, lis: lis,
				// Run only by the final-result case's worker: the store applies the refund, and
				// the call returns once the test opens gate.
				attempt: func(context.Context, *Permit) attemptResult {
					reachOnce.Do(func() { close(reached) })
					<-gate
					return attemptResult{final: true, result: Applied, creditNs: creditNs, sent: 1}
				},
			}, t.Context())
			t.Cleanup(r.stop)
			t.Cleanup(openGate) // registered after r's: runs before stop waits for the worker
			if !sl.tryAcquire() {
				t.Fatal("no free slot")
			}
			x := &rig{r: r, p: newPermit(sl, r, receipt.Receipt{}, "d1_order", time.Now()), closing: &closing, reached: reached, openGate: openGate}
			fire := c.start(t, x)

			// The transition fires while the test holds the slots' lock: it turns the permit
			// FINAL, then waits for the lock to free the slots, so it cannot emit yet.
			fired := make(chan struct{})
			func() {
				sl.mu.Lock()
				defer sl.mu.Unlock()
				go func() {
					defer close(fired)
					fire()
				}()
				pollUntil(t, "transition to FINAL", func() bool { return x.p.life.load() == stateFinal })
				time.Sleep(50 * time.Millisecond) // an emit that did not wait for the slots would have counted by now
				if n := refundEvents(t, reg); n != 0 {
					t.Errorf("rl_refund_results_total sums to %d while the permit's slots could not be freed yet, want 0: the transition frees the slots, then emits the event (API-1h)", n)
				}
			}()
			timeout := time.NewTimer(wait) // started before the waits it bounds
			defer timeout.Stop()
			var got deliveredEvent
			select {
			case got = <-events:
			case <-timeout.C:
				t.Fatalf("no event within %v of the slots' lock being released", wait)
			}
			ev := got.ev
			ev.ReleaseMonoNs, ev.FinalMonoNs = 0, 0
			if ev != c.want {
				t.Errorf("event %+v, want %+v", got.ev, c.want)
			}
			if got.receipts != 0 || got.refunds != 0 {
				t.Errorf("slots in use %d and %d when the listener received the event, want 0 and 0: they are freed before the event (API-1h)", got.receipts, got.refunds)
			}
			select {
			case <-fired:
			case <-timeout.C:
				t.Fatalf("the transition's call did not return within %v", wait)
			}
			select {
			case extra := <-events:
				t.Errorf("a second event %+v, want exactly one (API-1d)", extra.ev)
			case <-time.After(100 * time.Millisecond):
			}
			if rs, fs := sl.inUse(); rs != 0 || fs != 0 {
				t.Errorf("slots in use %d and %d after the event, want 0 and 0", rs, fs)
			}
		})
	}
}

// TestNextStartsAttempt (SPEC API-1d, API-1h; DESIGN §A5.5, §A5.6): the job next() hands a
// worker comes with its refund attempt already in flight, and with that attempt's context:
// both are set under the hold of the refunds' lock that takes the job off the queue, so no
// transition's effects (settle) can run in between: a transition that ends the permit
// meanwhile finds the attempt in flight and cancels it. A retention that wins right after
// next() returned finds the attempt in flight too: it cancels that context before anything is
// written and leaves the slots and the event to the worker. The adapter returns at once on a
// cancelled context with nothing sent, so the worker counts nothing, and the one event it
// emits once it ends the attempt carries Attempts 0 (API-1d counts only sent commands); the
// slots stay in use until then (API-1h).
//   - By hand: the test takes the job as a worker does (next) and ends the attempt itself.
//   - Through a worker, whose attempt returns only once the test has fired the retention, as
//     the adapter does on the cancelled context.
func TestNextStartsAttempt(t *testing.T) {
	const wait = 5 * time.Second
	t.Run("by hand", func(t *testing.T) {
		reg := metrics.NewRegistry()
		sl := newSlots(2, 2, reg)
		events := make(chan deliveredEvent, 4)
		lis := slotListener(t, sl, events, reg)
		var closing atomic.Bool
		r := newRefunds(refundsConfig{capacity: 1, lifetime: time.Hour, born: time.Now(), closing: &closing, reg: reg, lis: lis}, t.Context())
		t.Cleanup(r.stop)
		sl.tryAcquire() // a second reservation, so a double free shows as 0 in use, not 1
		if !sl.tryAcquire() {
			t.Fatal("no free slot")
		}
		p := newPermit(sl, r, receipt.Receipt{}, "d1_next", time.Now())
		p.Release()
		job, ctx := nextWithin(t, r) // as a worker takes its job
		r.mu.Lock()
		inFlight := p.attempting
		r.mu.Unlock()
		if job != p || !inFlight || ctx == nil || ctx.Err() != nil {
			t.Fatalf("next() returned the released permit %v, its attempt in flight %v, with a live context %v; want all three: a retention that wins now must find the attempt in flight",
				job == p, inFlight, ctx != nil && ctx.Err() == nil)
		}
		p.expire() // the retention timer wins right after next() returned
		if st := p.life.load(); st != stateFinal || ctx.Err() == nil {
			t.Fatalf("after the timer: %v, the attempt's context cancelled %v; want FINAL and cancelled, before anything is written", st, ctx.Err() != nil)
		}
		if rs, fs := sl.inUse(); rs != 2 || fs != 2 {
			t.Fatalf("slots in use %d and %d after the timer, want 2 and 2: the permit keeps its pair until its final event (API-1h)", rs, fs)
		}
		if n := refundEvents(t, reg); n != 0 {
			t.Fatalf("rl_refund_results_total sums to %d after the timer, want 0: the event waits for the attempt", n)
		}
		// The attempt on the cancelled context sends nothing (the adapter returns before it
		// writes), so the worker adds no command and ends the attempt.
		r.endAttempt(p)
		timeout := time.NewTimer(wait) // started before the wait it bounds
		defer timeout.Stop()
		var got deliveredEvent
		select {
		case got = <-events:
		case <-timeout.C:
			t.Fatalf("no event within %v of the attempt's end", wait)
		}
		ev := got.ev
		ev.ReleaseMonoNs, ev.FinalMonoNs = 0, 0
		if want := (RefundEvent{DeductionID: "d1_next", Result: Unresolved, Reason: UnresolvedRetention}); ev != want {
			t.Errorf("event %+v, want %+v: the cancelled attempt sent nothing, so it is not counted (API-1d)", got.ev, want)
		}
		if got.receipts != 1 || got.refunds != 1 {
			t.Errorf("slots in use %d and %d when the listener received the event, want 1 and 1 (the other reservation): the permit's pair is freed before its event", got.receipts, got.refunds)
		}
		if rs, fs := sl.inUse(); rs != 1 || fs != 1 {
			t.Errorf("slots in use %d and %d after the event, want 1 and 1: the permit's pair is freed once", rs, fs)
		}
	})

	t.Run("through a worker", func(t *testing.T) {
		started := make(chan context.Context, 1)
		proceed := make(chan struct{})
		var startOnce, proceedOnce sync.Once
		letReturn := func() { proceedOnce.Do(func() { close(proceed) }) }
		rig := newRetentionRig(t, time.Hour, func(ctx context.Context, _ *Permit) attemptResult {
			startOnce.Do(func() { started <- ctx })
			<-proceed
			if ctx.Err() != nil {
				return attemptResult{} // the adapter returns at once on a cancelled context: nothing sent
			}
			return attemptResult{sent: 1}
		})
		t.Cleanup(letReturn)           // registered after the rig: runs before its workers are waited for
		timeout := time.NewTimer(wait) // started before the waits it bounds
		defer timeout.Stop()
		var ctx context.Context
		select {
		case ctx = <-started:
		case <-timeout.C:
			t.Fatalf("the refund attempt did not start within %v", wait)
		}
		if ctx.Err() != nil {
			t.Fatal("the attempt began with a cancelled context")
		}
		rig.p.expire() // the retention timer wins before the attempt writes anything
		if ctx.Err() == nil {
			t.Fatal("the retention timer did not cancel the attempt's context")
		}
		if rs, fs := rig.sl.inUse(); rs != 1 || fs != 1 {
			t.Fatalf("slots in use %d and %d after the timer, want 1 and 1 (API-1h)", rs, fs)
		}
		letReturn()
		var got deliveredEvent
		select {
		case got = <-rig.events:
		case <-timeout.C:
			t.Fatalf("no event within %v", wait)
		}
		ev := got.ev
		ev.ReleaseMonoNs, ev.FinalMonoNs = 0, 0
		if want := (RefundEvent{DeductionID: "d1_retention", Result: Unresolved, Reason: UnresolvedRetention}); ev != want || got.receipts != 0 || got.refunds != 0 {
			t.Errorf("event %+v with %d and %d slots in use, want %+v with 0 and 0: nothing was sent (API-1d), and the slots are freed first", got.ev, got.receipts, got.refunds, want)
		}
		select {
		case extra := <-rig.events:
			t.Errorf("a second event %+v, want exactly one (API-1d)", extra.ev)
		case <-time.After(100 * time.Millisecond):
		}
	})
}

// TestRetentionRacesAttemptEnd (SPEC API-1d, API-1h; DESIGN §A5.5, §A5.6): a released permit's
// retention timer raced 1,000 times against the end of its refund attempt, which sent one
// command (the worker took the job, which put the attempt in flight, adds the command, then
// ends the attempt). In every order exactly one event goes out, unresolved (retention) with
// Attempts 1; the slots are freed once; the attempt's context is cancelled and nothing stays in
// flight or held.
func TestRetentionRacesAttemptEnd(t *testing.T) {
	const iterations = 1_000
	lisReg := metrics.NewRegistry()
	evs := make(chan RefundEvent, 2*iterations) // room for a second event per permit, so emit never waits
	lis := newListener(func(ev RefundEvent) { evs <- ev }, 2*iterations, lisReg)
	lis.start()
	t.Cleanup(func() {
		lis.stop()
		lis.waitExit(time.Now().Add(5 * time.Second))
	})
	var closing atomic.Bool
	for i := range iterations {
		reg := metrics.NewRegistry()
		r := newRefunds(refundsConfig{capacity: 1, lifetime: time.Hour, born: time.Now(), closing: &closing, reg: reg, lis: lis}, t.Context())
		t.Cleanup(r.stop) // stop is idempotent: this one ends a failed iteration's waits
		sl := newSlots(2, 2, reg)
		sl.tryAcquire() // a second reservation, so a double free shows as 0 in use
		sl.tryAcquire()
		p := newPermit(sl, r, receipt.Receipt{}, "d1_race_"+strconv.Itoa(i), time.Now())
		p.Release()
		job, ctx := nextWithin(t, r) // the worker takes the job: its attempt is in flight
		if job != p || ctx == nil {
			t.Fatalf("next() returned the released permit %v, with its attempt's context %v; want both", job == p, ctx != nil)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { // the worker: its attempt sent one command
			defer wg.Done()
			<-start
			p.attempts.Add(1)
			r.endAttempt(p)
		}()
		go func() { // the retention timer
			defer wg.Done()
			<-start
			p.expire()
		}()
		close(start)
		wg.Wait()
		r.mu.Lock()
		attempting, held := p.attempting, p.held
		r.mu.Unlock()
		if st := p.life.load(); st != stateFinal || attempting || held != (transition{}) || ctx.Err() == nil {
			t.Fatalf("iteration %d: %v, attempt in flight %v, held %+v, attempt context ended %v; want FINAL, false, none, true",
				i, st, attempting, held, ctx.Err() != nil)
		}
		if inUse, _ := sl.inUse(); inUse != 1 {
			t.Fatalf("iteration %d: %d receipt slots in use, want 1: the permit's freed once", i, inUse)
		}
		r.stop()
	}
	lis.waitIdle(time.Now().Add(5 * time.Second))
	if d := lis.drops.Load(); d != 0 || len(evs) != iterations {
		t.Fatalf("%d events and %d drops, want %d and 0: exactly one per permit", len(evs), d, iterations)
	}
	seen := map[string]bool{}
	for range iterations {
		ev := <-evs
		if ev.Result != Unresolved || ev.Reason != UnresolvedRetention || ev.Attempts != 1 || seen[ev.DeductionID] {
			t.Fatalf("event %+v (seen before: %v), want one unresolved (retention) event per permit with 1 attempt", ev, seen[ev.DeductionID])
		}
		seen[ev.DeductionID] = true
	}
}

// refundEvents counts the final events a registry saw: the sum of rl_refund_results_total.
func refundEvents(t *testing.T, reg *metrics.Registry) int {
	t.Helper()
	n := 0
	for _, f := range reg.Snapshot().Families {
		if f.Name == "rl_refund_results_total" {
			for _, s := range f.Series {
				n += int(s.Value)
			}
		}
	}
	return n
}

// fireEvent delivers ev to p the way its source does: the host (done, release, release after
// Close began), the retention timer, a worker's final result, or the end of Close's drain.
func fireEvent(p *Permit, ev permitEvent) {
	switch ev {
	case evDone:
		p.Done()
	case evRelease:
		p.release(false)
	case evReleaseClosing:
		p.release(true)
	case evRetention:
		p.expire()
	case evFinalResult:
		p.finish(attemptResult{final: true, result: Applied, creditNs: 1})
	case evDrainEnd:
		p.drainEnd()
	}
}

// TestPermitEffects_Race (plan S6, L1; DESIGN §A5.5; SPEC API-1d, API-1h): from OPEN, RELEASED
// and EXPIRED, every pair of events (each source of §A5.5's transitions: done, release, release
// after Close began, retention, a worker's final result, the drain's end), raced 1,000 times on
// a real permit with its effects, frees its slots at most once and emits at most one event,
// exactly as one of the two serial orders would; the permit is pending and queued, and its
// retention timer armed, exactly while it is RELEASED (or OPEN, for the timer).
func TestPermitEffects_Race(t *testing.T) {
	const iterations = 1_000
	want := wantTable()
	effects := func(o pairOutcome) (frees, emits int) {
		for _, f := range []fired{o.e1, o.e2} {
			if f.ok && f.t.freeSlots {
				frees++
			}
			if f.ok && f.t.emit != emitNone {
				emits++
			}
		}
		return frees, emits
	}
	var closing atomic.Bool
	for _, from := range []permitState{stateOpen, stateReleased, stateExpired} {
		for _, e1 := range allEvents() {
			for _, e2 := range allEvents() {
				order12 := serialOutcome(want, from, e1, e2, false)
				order21 := serialOutcome(want, from, e1, e2, true)
				for range iterations {
					reg := metrics.NewRegistry()
					sl := newSlots(2, 2, reg)
					sl.tryAcquire() // a second reservation, so a double free shows as 0 in use
					sl.tryAcquire()
					r := newRefunds(refundsConfig{capacity: 2, lifetime: time.Hour, born: time.Now(), closing: &closing, reg: reg}, t.Context())
					p := newPermit(sl, r, receipt.Receipt{}, "d1_race", time.Now())
					switch from {
					case stateReleased:
						p.release(false)
					case stateExpired:
						p.expire()
					case stateOpen, stateDone, stateFinal, stateDegraded:
					}
					if got := p.life.load(); got != from {
						t.Fatalf("setup reached %v, want %v", got, from)
					}
					inUse, _ := sl.inUse()
					var wg sync.WaitGroup
					start := make(chan struct{})
					for _, ev := range []permitEvent{e1, e2} {
						wg.Add(1)
						go func() {
							defer wg.Done()
							<-start
							fireEvent(p, ev)
						}()
					}
					close(start)
					wg.Wait()
					final := p.life.load()
					after, _ := sl.inUse()
					frees, emits := inUse-after, refundEvents(t, reg)
					matched := false
					for _, o := range []pairOutcome{order12, order21} {
						f, e := effects(o)
						if o.final == final && f == frees && e == emits {
							matched = true
						}
					}
					if !matched || frees > 1 || emits > 1 {
						t.Fatalf("from %v, %v ‖ %v: ended %v with slots freed %d times and %d events; want one serial order: %v %v or %v %v",
							from, e1, e2, final, frees, emits, order12.final, order12, order21.final, order21)
					}
					r.mu.Lock()
					_, pending := r.pending[p]
					_, armed := r.timers[p]
					queued, timerSet := p.index >= 0, p.retention != nil
					r.mu.Unlock()
					released := final == stateReleased
					if pending != released || queued != released {
						t.Fatalf("from %v, %v ‖ %v: ended %v, pending %v, queued %v; want both only while RELEASED", from, e1, e2, final, pending, queued)
					}
					live := final == stateOpen || final == stateReleased
					if armed != live || timerSet != live {
						t.Fatalf("from %v, %v ‖ %v: ended %v, retention timer armed %v (set %v); want it only while OPEN or RELEASED", from, e1, e2, final, armed, timerSet)
					}
					r.stop()
				}
			}
		}
	}
}
