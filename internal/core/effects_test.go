package core

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
)

// The tests of this file build no limiter and run before limiter_test.go's: go test runs a
// package's test files in name order, and bin/mutate --sweep runs without -shuffle and with
// -failfast (DESIGN §C9). Each drives one part of the refund machinery directly, so a mutant
// there fails one of them with an assertion before a test that reaches the same code through a
// limiter's workers or Close, or through a permit without refunds (TestPermitDoneOnce), can
// crash on it. Their order matters too: TestReadyQueueBasics first (the queue alone), then
// TestRefundTimersAndStop (the queue through refunds, next, the timers and stop: no worker, no
// event and no settle of a pending refund), TestListenerWaitIdleWakes (the first listener that
// delivers an event), TestRefundSettle (the first settle of a pending refund and the first
// event emitted) and TestRefundWorkerAttempt (the file's first worker). limiter_test.go's
// TestCloseRefundsAndListener, the first test to close a limiter with a listener and a refund
// pending, relies on all of them. Since S7, cleanup_test.go sorts before this file, and its
// limiters start refund workers and Close: background_test.go's tests, which run first in the
// untagged build (the build bin/mutate runs first), fail the mutants of the worker loop and of
// Close's nil guards that those limiters crash on.

// effectsWait bounds every wait of this file's tests.
const effectsWait = 5 * time.Second

// pollUntil polls cond every millisecond and fails the test unless it holds within
// effectsWait.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(effectsWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within %v", what, effectsWait)
		}
		time.Sleep(time.Millisecond)
	}
}

// eventSink is a refund listener that keeps every event, in order.
type eventSink struct {
	mu  sync.Mutex
	evs []RefundEvent
}

func (s *eventSink) listen(ev RefundEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

// untimed returns the events kept so far without their monotonic times, after checking that
// each has a release time (release() records one, at least 1 ns) no later than its end.
func (s *eventSink) untimed(t *testing.T) []RefundEvent {
	t.Helper()
	s.mu.Lock()
	evs := slices.Clone(s.evs)
	s.mu.Unlock()
	for i, ev := range evs {
		if ev.ReleaseMonoNs < 1 || ev.FinalMonoNs < ev.ReleaseMonoNs {
			t.Fatalf("event %+v: ReleaseMonoNs %d, FinalMonoNs %d; want 1 ≤ release ≤ final", ev, ev.ReleaseMonoNs, ev.FinalMonoNs)
		}
		evs[i].ReleaseMonoNs, evs[i].FinalMonoNs = 0, 0
	}
	return evs
}

// TestReadyQueueBasics (plan S6 step 4; DESIGN §A5.6): the ready queue alone. Three jobs
// queued in the reverse of their release order pop earliest release first, each leaving with
// index -1, and a fourth is refused (capacity 3); pop on the empty queue returns nil; remove
// takes out a queued job at the root and one below it, keeping every other job's index its
// position, and is a no-op for a job that is not queued.
func TestReadyQueueBasics(t *testing.T) {
	var mu sync.Mutex
	q := readyQueue{cap: 3, cond: sync.NewCond(&mu)}
	job := func(name string, release int64, seq uint64) *Permit {
		p := &Permit{id: name, index: -1, seq: seq}
		p.releaseMono.Store(release)
		return p
	}
	a, b, c := job("A", 10, 3), job("B", 20, 2), job("C", 30, 1) // released A, B, C; queued C, B, A
	name := func(p *Permit) string {
		if p == nil {
			return "nil"
		}
		return p.id
	}
	positions := func() bool {
		for i, p := range q.h {
			if p.index != i {
				return false
			}
		}
		return true
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range []*Permit{c, b, a} {
		if !q.push(p) {
			t.Fatalf("push of %s refused with %d of 3 queued", p.id, len(q.h))
		}
	}
	if !positions() {
		t.Fatal("after 3 pushes a queued job's index is not its position")
	}
	if q.push(job("D", 5, 4)) {
		t.Fatal("a full queue (3 of 3) accepted a fourth job")
	}
	for _, want := range []*Permit{a, b, c} {
		got := q.pop()
		if got != want {
			t.Fatalf("pop returned %s, want %s: earliest release first", name(got), want.id)
		}
		if got.index != -1 {
			t.Fatalf("popped %s keeps index %d, want -1", got.id, got.index)
		}
	}
	if got := q.pop(); got != nil {
		t.Fatalf("pop on the empty queue returned %s, want nil", name(got))
	}

	for _, p := range []*Permit{c, b, a} {
		if !q.push(p) {
			t.Fatalf("push of %s refused with %d of 3 queued", p.id, len(q.h))
		}
	}
	q.remove(a) // the root: the earliest release
	if a.index != -1 || len(q.h) != 2 || slices.Contains(q.h, a) || !positions() {
		t.Fatalf("after removing the root A: A at index %d, %d queued, A still queued %v; want -1, 2, false, every index its position",
			a.index, len(q.h), slices.Contains(q.h, a))
	}
	below := q.h[1] // not the root
	q.remove(below)
	if below.index != -1 || len(q.h) != 1 || slices.Contains(q.h, below) || !positions() {
		t.Fatalf("after removing %s below the root: index %d, %d queued; want -1 and 1, every index its position", below.id, below.index, len(q.h))
	}
	q.remove(a) // not queued: nothing changes
	if len(q.h) != 1 || a.index != -1 {
		t.Fatalf("removing a job that is not queued: %d queued, its index %d; want 1 and -1", len(q.h), a.index)
	}
}

// TestRefundTimersAndStop (plan S6 steps 3–5 and 8; DESIGN §A5.5, §A5.6): an OPEN permit's
// retention timer is armed (REF-4a); release() makes it pending and queued with no backoff
// timer and the same retention timer still armed; a worker's next() takes it, and retry arms
// its backoff timer; stop then stops both timers, so neither fires after it, keeps the refund
// pending (Close's drain ends it next; under Kill it is lost), and next() reports the end.
func TestRefundTimersAndStop(t *testing.T) {
	reg := metrics.NewRegistry()
	var closing atomic.Bool
	r := testRefunds(t, 2, reg, &closing, time.Hour, time.Hour) // no backoff fires in the test
	sl := newSlots(2, 2, reg)
	if !sl.tryAcquire() {
		t.Fatal("no free slot")
	}
	p := newPermit(sl, r, receipt.Receipt{}, "d1_stop", time.Now())
	r.mu.Lock()
	retention := p.retention
	_, armed := r.timers[p]
	r.mu.Unlock()
	if retention == nil || !armed {
		t.Fatalf("a new OPEN permit: retention timer set %v, armed %v; want both (REF-4a)", retention != nil, armed)
	}

	p.Release()
	r.mu.Lock()
	_, pending := r.pending[p]
	queued, backoffSet, sameRetention := p.index >= 0, p.backoff != nil, p.retention == retention
	_, armed = r.timers[p]
	r.mu.Unlock()
	if st := p.life.load(); st != stateReleased || !pending || !queued || backoffSet {
		t.Fatalf("after release(): %v, pending %v, queued %v, backoff timer set %v; want RELEASED, pending, queued, no backoff timer",
			st, pending, queued, backoffSet)
	}
	if !sameRetention || !armed {
		t.Fatalf("after release(): the same retention timer %v, armed %v; want both (it ends a pending refund, REF-4a)", sameRetention, armed)
	}
	if got := gaugeValue(t, reg.Snapshot(), "rl_pending_refunds", ""); got != 1 {
		t.Fatalf("rl_pending_refunds %v after one release(), want 1", got)
	}

	// A worker takes the job, and its attempt is retried: the backoff timer is armed.
	if got, _ := nextWithin(t, r); got != p {
		t.Fatal("next() did not return the one queued job")
	}
	r.endAttempt(p)
	r.retry(p)
	r.mu.Lock()
	backoff := p.backoff
	r.mu.Unlock()
	if backoff == nil {
		t.Fatal("retry armed no backoff timer")
	}

	r.stop()
	r.mu.Lock()
	retentionLeft, backoffLeft, timersLeft := p.retention, p.backoff, len(r.timers)
	_, pending = r.pending[p]
	r.mu.Unlock()
	// Stop reports true only when the timer was still running: stop must have stopped both.
	if running := retention.Stop(); running || retentionLeft != nil || timersLeft != 0 {
		t.Fatalf("after stop: the retention timer still running %v, still set %v, %d armed; want stopped, unset, none",
			running, retentionLeft != nil, timersLeft)
	}
	if running := backoff.Stop(); running || backoffLeft != nil {
		t.Fatalf("after stop: the backoff timer still running %v, still set %v; want stopped and unset", running, backoffLeft != nil)
	}
	if st := p.life.load(); st != stateReleased || !pending {
		t.Fatalf("after stop: %v, pending %v; want RELEASED and pending (Close's drain ends it; under Kill it is lost)", st, pending)
	}
	timeout := time.NewTimer(effectsWait) // started before the wait it bounds
	defer timeout.Stop()
	ended := make(chan bool, 1) // buffered: a wait the test gave up on still ends
	go func() {
		q, ctx, ok := r.next()
		ended <- q == nil && ctx == nil && !ok
	}()
	select {
	case e := <-ended:
		if !e {
			t.Fatal("next() after stop returned a job, want none and false: the workers end")
		}
	case <-timeout.C:
		t.Fatalf("next() after stop did not return within %v", effectsWait)
	}
}

// TestListenerWaitIdleWakes (plan S6 steps 6 and 7; DESIGN §A5.11): Close's wait for the
// listener (waitIdle) returns as soon as the last queued event is delivered, not at its
// deadline. The one event is held in the callback while waitIdle starts waiting; once the
// callback returns, waitIdle returns before its 5 s deadline, with nothing queued or waited
// for, the event delivered once and nothing dropped.
func TestListenerWaitIdleWakes(t *testing.T) {
	reg := metrics.NewRegistry()
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	release := func() { unblockOnce.Do(func() { close(unblock) }) }
	var delivered atomic.Int32
	s := newListener(func(RefundEvent) {
		delivered.Add(1)
		<-unblock
	}, 4, reg)
	s.start()
	t.Cleanup(func() {
		release()
		s.stop()
		s.waitExit(time.Now().Add(effectsWait))
	})
	s.emit(RefundEvent{DeductionID: "d1_idle"})
	pollUntil(t, "event in the callback", func() bool { return delivered.Load() == 1 })

	timeout := time.NewTimer(2 * effectsWait) // started before the deadline: it bounds the whole wait
	defer timeout.Stop()
	deadline := time.Now().Add(effectsWait)
	returned := make(chan time.Time, 1)
	go func() {
		s.waitIdle(deadline)
		returned <- time.Now()
	}()
	pollUntil(t, "waitIdle waiting", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.idle != nil
	})
	release()
	select {
	case at := <-returned:
		if !at.Before(deadline) {
			t.Fatalf("waitIdle returned %v after its deadline, want at once when the queue emptied", at.Sub(deadline))
		}
	case <-timeout.C:
		t.Fatalf("waitIdle did not return within %v", 2*effectsWait)
	}
	s.mu.Lock()
	waiting, queued := s.idle != nil, s.queued
	s.mu.Unlock()
	if waiting || queued != 0 || delivered.Load() != 1 || s.drops.Load() != 0 {
		t.Fatalf("after waitIdle: still waited for %v, %d queued, %d delivered, %d dropped; want false, 0, 1, 0",
			waiting, queued, delivered.Load(), s.drops.Load())
	}
}

// TestRefundSettle (plan S6 steps 3, 5 and 7; DESIGN §A5.5, §A5.6, §A5.11; SPEC API-1d,
// API-1e): with two refunds pending and Close's drain waiting for them (waitIdle), the
// retention timer of A, which waits out a backoff, fires (RELEASED → FINAL, unresolved
// (retention)): A leaves the pending set and the queue, its backoff and retention timers stop,
// its slots are freed and its one event reaches the listener, while the drain keeps waiting
// for B; B's final result (applied) then wakes the drain at once, nothing pending.
func TestRefundSettle(t *testing.T) {
	const creditNs = 1_760_000_001_000_000_000
	reg := metrics.NewRegistry()
	sink := &eventSink{}
	lis := newListener(sink.listen, 4, reg)
	lis.start()
	t.Cleanup(func() {
		lis.stop()
		lis.waitExit(time.Now().Add(effectsWait))
	})
	var closing atomic.Bool
	r := newRefunds(refundsConfig{
		capacity: 2, lifetime: time.Hour, backoffInitial: time.Hour, backoffMax: time.Hour,
		born: time.Now(), closing: &closing, reg: reg, lis: lis,
	}, t.Context())
	t.Cleanup(r.stop)
	sl := newSlots(2, 2, reg)
	for i := range 2 {
		if !sl.tryAcquire() {
			t.Fatalf("cannot reserve slot pair %d of 2", i+1)
		}
	}
	a := newPermit(sl, r, receipt.Receipt{}, "d1_a", time.Now())
	b := newPermit(sl, r, receipt.Receipt{}, "d1_b", time.Now())
	a.Release()
	b.Release()
	if got, _ := nextWithin(t, r); got != a {
		t.Fatal("next() did not return A, the earlier release")
	}
	r.endAttempt(a)
	r.retry(a) // A's attempt was retried: it waits out a 1 h backoff, and B stays queued
	r.mu.Lock()
	aRetention, aBackoff := a.retention, a.backoff
	r.mu.Unlock()
	if aRetention == nil || aBackoff == nil {
		t.Fatalf("A waiting out its backoff: retention timer set %v, backoff timer set %v; want both", aRetention != nil, aBackoff != nil)
	}

	// Close's drain waits until nothing is pending.
	timeout := time.NewTimer(2 * effectsWait) // started before the deadline: it bounds the whole wait
	defer timeout.Stop()
	deadline := time.Now().Add(effectsWait)
	idle := make(chan bool, 1) // buffered: a wait the test gave up on still ends (stop aborts it)
	go func() { idle <- r.waitIdle(deadline) }()
	pollUntil(t, "drain waiting", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.idle != nil
	})

	a.expire() // A's retention timer fires during its backoff
	r.mu.Lock()
	_, aPending := r.pending[a]
	aQueued, backoffLeft, retentionLeft := a.index >= 0, a.backoff, a.retention
	_, aArmed := r.timers[a]
	pendingLeft, drainWaiting := len(r.pending), r.idle != nil
	r.mu.Unlock()
	if st := a.life.load(); st != stateFinal || aPending || aQueued {
		t.Fatalf("A after its retention: %v, pending %v, queued %v; want FINAL, neither", st, aPending, aQueued)
	}
	// Stop reports true only when the timer was still running: A's transition must have stopped both.
	if running := aBackoff.Stop(); running || backoffLeft != nil {
		t.Fatalf("A's backoff timer still running %v, still set %v; want stopped and unset", running, backoffLeft != nil)
	}
	if running := aRetention.Stop(); running || retentionLeft != nil || aArmed {
		t.Fatalf("A's retention timer still running %v, still set %v, armed %v; want stopped, unset, unarmed", running, retentionLeft != nil, aArmed)
	}
	if pendingLeft != 1 || !drainWaiting {
		t.Fatalf("%d refund(s) pending, the drain still waited for %v; want B pending and the drain waiting", pendingLeft, drainWaiting)
	}
	if rs, fs := sl.inUse(); rs != 1 || fs != 1 {
		t.Fatalf("slots in use %d and %d after A ended, want 1 and 1 (B holds its pair)", rs, fs)
	}
	snap := reg.Snapshot()
	if pend, unres := gaugeValue(t, snap, "rl_pending_refunds", ""), gaugeValue(t, snap, "rl_refund_results_total", "unresolved"); pend != 1 || unres != 1 {
		t.Fatalf("rl_pending_refunds %v, rl_refund_results_total{unresolved} %v; want 1 and 1", pend, unres)
	}
	lis.waitIdle(time.Now().Add(effectsWait))
	want := []RefundEvent{{DeductionID: "d1_a", Result: Unresolved, Reason: UnresolvedRetention}}
	if got := sink.untimed(t); !slices.Equal(got, want) {
		t.Fatalf("listener events %+v, want %+v: A's one event", got, want)
	}

	if got, _ := nextWithin(t, r); got != b {
		t.Fatal("next() did not return B")
	}
	r.endAttempt(b) // B's attempt got its final result: the worker ends the attempt, then the job
	b.finish(attemptResult{final: true, result: Applied, creditNs: creditNs})
	select {
	case ok := <-idle:
		if !ok {
			t.Fatal("the drain's wait ended at its deadline with a refund pending: B's final result, the last pending refund, must wake it at once")
		}
	case <-timeout.C:
		t.Fatalf("the drain's wait did not return within %v", 2*effectsWait)
	}
	r.mu.Lock()
	pendingLeft, drainWaiting = len(r.pending), r.idle != nil
	r.mu.Unlock()
	if st := b.life.load(); st != stateFinal || pendingLeft != 0 || drainWaiting {
		t.Fatalf("B after its final result: %v; %d pending, the drain still waited for %v; want FINAL, 0, false", st, pendingLeft, drainWaiting)
	}
	if rs, fs := sl.inUse(); rs != 0 || fs != 0 {
		t.Fatalf("slots in use %d and %d after B ended, want 0 and 0", rs, fs)
	}
	snap = reg.Snapshot()
	if pend, applied := gaugeValue(t, snap, "rl_pending_refunds", ""), gaugeValue(t, snap, "rl_refund_results_total", "applied"); pend != 0 || applied != 1 {
		t.Fatalf("rl_pending_refunds %v, rl_refund_results_total{applied} %v; want 0 and 1", pend, applied)
	}
	lis.waitIdle(time.Now().Add(effectsWait))
	want = append(want, RefundEvent{DeductionID: "d1_b", Result: Applied, CreditDecisionNs: creditNs})
	if got := sink.untimed(t); !slices.Equal(got, want) {
		t.Fatalf("listener events %+v, want %+v", got, want)
	}
}

// TestRefundWorkerAttempt (plan S6 steps 4 and 5; DESIGN §A5.6; SPEC API-1d): one refund
// worker takes a released job off the ready queue, runs one attempt for it, adds the refund
// commands that attempt sent to the permit's attempts, and ends the job on its final result:
// FINAL, its slots freed, nothing pending, one applied result counted.
func TestRefundWorkerAttempt(t *testing.T) {
	reg := metrics.NewRegistry()
	attempted := make(chan *Permit, 1)
	var closing atomic.Bool
	r := newRefunds(refundsConfig{
		capacity: 1, lifetime: time.Hour, backoffInitial: time.Hour, backoffMax: time.Hour,
		born: time.Now(), closing: &closing, reg: reg,
		attempt: func(_ context.Context, p *Permit) attemptResult {
			select {
			case attempted <- p:
			default: // never blocks the worker
			}
			return attemptResult{final: true, result: Applied, creditNs: 1_760_000_001_000_000_000, sent: 1}
		},
	}, t.Context())
	t.Cleanup(r.stop)
	r.startWorkers(1)
	sl := newSlots(1, 1, reg)
	if !sl.tryAcquire() {
		t.Fatal("no free slot")
	}
	p := newPermit(sl, r, receipt.Receipt{}, "d1_worker", time.Now())
	timeout := time.NewTimer(effectsWait) // started before the wait it bounds
	defer timeout.Stop()
	p.Release()
	select {
	case got := <-attempted:
		if got != p {
			t.Fatal("the worker's attempt was for another permit")
		}
	case <-timeout.C:
		t.Fatalf("the worker ran no attempt within %v of release()", effectsWait)
	}
	// Counting the result is the last effect of the worker's RELEASED → FINAL transition: once it
	// shows, the state, the pending set and the slots are settled.
	pollUntil(t, "applied result counted", func() bool {
		return gaugeValue(t, reg.Snapshot(), "rl_refund_results_total", "applied") == 1
	})
	if st := p.life.load(); st != stateFinal {
		t.Fatalf("the job is %v after its final result, want FINAL", st)
	}
	if n := p.attempts.Load(); n != 1 {
		t.Fatalf("attempts %d, want 1: the one command the attempt sent (API-1d)", n)
	}
	if rs, fs := sl.inUse(); rs != 0 || fs != 0 {
		t.Fatalf("slots in use %d and %d after the final result, want 0 and 0", rs, fs)
	}
	if pend := gaugeValue(t, reg.Snapshot(), "rl_pending_refunds", ""); pend != 0 {
		t.Fatalf("rl_pending_refunds %v after the final result, want 0", pend)
	}
}
