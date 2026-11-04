package core

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/metrics"
	"github.com/srir84/ratelimiter/internal/receipt"
)

// The tests of this file run first in the package's untagged build, which bin/mutate --sweep
// runs first: go test runs a package's test files in name order, and bin/mutate runs without
// -shuffle and with -failfast (DESIGN §C9), and it runs a mutant's -tags rltest build, where
// acquire_rltest_test.go sorts first, only when every untagged test passed. The first untagged
// test to build limiters would otherwise be cleanup_test.go's TestCleanupPassProcedure (S7),
// which sorts before effects_test.go and limiter_test.go: its limiters, which have no refund
// listener and no signer wrapper, reach newSigner, start refund workers on an empty queue,
// report Health and stop at Close, and it closes a settings_invalid one, which has no refund
// machinery. A mutant of the worker loop (work, next), of Close's two nil guards, of Health's
// ListenerDrops or of newSigner's wrapper check crashes there before any test asserts, and a
// mutant of newLimiter's Option loop or listener construction crashes the first limiter given a
// listener, TestCloseRefundsAndListener's, which dereferences it; so each must fail a test here
// first. These tests are built so that any other mutant of the code they reach either fails
// one of their assertions before anything crashes or leaves them passing, for a later test to
// fail, and their order matters: TestBackgroundNextTakesQueuedJob first (the ready queue and
// next alone, with no worker and no permit transition; pop is checked before next uses what it
// returns), then TestBackgroundCloseStopsRefundsAndListener (Close with nothing pending, with no
// worker and no permit transition, then ListenerDrops, with a listener first; it fails a stop
// that never wakes the workers, under which the next test's stop would hang),
// TestBackgroundWorkerRetriesQueuedJob (the first worker and the first permit transitions: the
// attempt is retried, so no transition ends the permit; the test waits for the worker's last
// step for the job before it checks the job, then runs the job's backoff and stop itself),
// TestBackgroundNewSignerWrapper (newSigner alone, with a wrapper first) and last
// TestBackgroundNewBuildsRefundListener (the first limiters that newLimiter starts, the one
// given a listener first: every mutant above that such a limiter, its workers or its Close
// would crash on fails an earlier test).

// TestBackgroundNextTakesQueuedJob (plan S6 step 4; DESIGN §A5.6): the ready queue pops the one
// job it holds, and next() on refunds that have not stopped returns a RELEASED job already on
// the queue, instead of waiting for a push or reporting the end, with its attempt's context and
// the attempt in flight, leaving the queue empty. The job is put on the queue directly, so no
// permit transition runs (enqueue's part is TestBackgroundWorkerRetriesQueuedJob's).
func TestBackgroundNextTakesQueuedJob(t *testing.T) {
	var mu sync.Mutex
	q := readyQueue{cap: 1, cond: sync.NewCond(&mu)}
	j := &Permit{id: "d1_pop", index: -1}
	mu.Lock()
	pushed := q.push(j)
	popped := q.pop()
	mu.Unlock()
	if !pushed || popped != j {
		t.Fatalf("one job in an empty queue of capacity 1: push accepted it %v, pop returned it %v; want both", pushed, popped == j)
	}

	var closing atomic.Bool
	r := testRefunds(t, 1, metrics.NewRegistry(), &closing, time.Hour, time.Hour)
	p := &Permit{life: newLifecycle(stateReleased), id: "d1_next", index: -1, cancelAttempt: noAttempt}
	r.mu.Lock()
	pushed = r.queue.push(p)
	r.mu.Unlock()
	if !pushed {
		t.Fatal("the empty ready queue of capacity 1 refused one job")
	}
	got, ctx := nextWithin(t, r)
	if got != p || ctx == nil {
		t.Fatalf("next() returned the queued job %v, with a context %v; want both", got == p, ctx != nil)
	}
	r.mu.Lock()
	attempting, left := p.attempting, len(r.queue.h)
	r.mu.Unlock()
	if !attempting || left != 0 {
		t.Fatalf("after next(): the job's attempt in flight %v, %d job(s) queued; want true and 0", attempting, left)
	}
}

// TestBackgroundCloseStopsRefundsAndListener (plan S6 step 7; SPEC API-1d, API-1e; DESIGN
// §A5.2, §A5.11): Close on an instance that has refund machinery and a refund listener, with
// nothing pending, ends the lifetime context, closes the store adapter once, stops the refunds
// (stopped: no worker or timer runs on), ends the drain (drained: a release() from then on ends
// unresolved (closed) at once) and stops the listener (closed): when Close returns, its
// dispatcher has returned, and a later event is dropped and counted, never delivered; Health's
// ListenerDrops (listenerDrops) is that listener's count, and 0 on an instance without a
// listener. The instance is built bare: a lifetime context and a stub adapter, set as
// newLimiter sets them so that shutdown's nil guards take their production branch, and no
// workers, epoch watch or cleanup runner, so only Close's own steps run.
func TestBackgroundCloseStopsRefundsAndListener(t *testing.T) {
	reg := metrics.NewRegistry()
	var delivered atomic.Int32
	lis := newListener(func(RefundEvent) { delivered.Add(1) }, 1, reg)
	lis.start()
	t.Cleanup(func() { // both return at once after a Close that stopped the listener
		lis.stop()
		lis.waitExit(time.Now().Add(effectsWait))
	})
	life, stop := context.WithCancel(t.Context())
	t.Cleanup(stop)
	a := newStubAdapter()
	l := &Limiter{reg: reg, lis: lis, adapter: a, stop: stop}
	l.refunds = newRefunds(refundsConfig{
		capacity: 1, lifetime: time.Hour, backoffInitial: time.Hour, backoffMax: time.Hour,
		born: time.Now(), closing: &l.closing, reg: reg, lis: lis,
	}, life)
	t.Cleanup(l.refunds.stop)

	l.Close()
	if closes, _ := a.closeState(); life.Err() == nil || closes != 1 {
		t.Fatalf("after Close: the lifetime context ended %v, the store adapter closed %d time(s); want true and once", life.Err() != nil, closes)
	}
	l.refunds.mu.Lock()
	stopped, drained := l.refunds.stopped, l.refunds.drained
	l.refunds.mu.Unlock()
	if !stopped || !drained {
		t.Fatalf("after Close: the refunds stopped %v, the drain ended %v; want both (API-1e)", stopped, drained)
	}
	lis.mu.Lock()
	closed := lis.closed
	lis.mu.Unlock()
	if !closed {
		t.Fatal("after Close: the refund listener is not stopped")
	}
	select {
	case <-lis.exited:
	default:
		t.Fatal("when Close returned, the listener's dispatcher, in no callback, had not returned")
	}
	lis.emit(RefundEvent{DeductionID: "d1_late"})
	if n, d := delivered.Load(), lis.drops.Load(); n != 0 || d != 1 {
		t.Fatalf("an event after Close: %d delivered, %d dropped; want 0 and 1 (API-1d)", n, d)
	}
	if got := l.listenerDrops(); got != 1 {
		t.Fatalf("Health's ListenerDrops %d after the dropped event, want the listener's 1", got)
	}
	if got := (&Limiter{}).listenerDrops(); got != 0 {
		t.Fatalf("Health's ListenerDrops %d on an instance without a refund listener, want 0", got)
	}
}

// TestBackgroundWorkerRetriesQueuedJob (plan S6 steps 4 and 5; DESIGN §A5.6; SPEC API-1d,
// API-1h): a refund worker started with a released job already queued takes it, runs one
// attempt for it, adds the one refund command that attempt sent to the permit's attempts, ends
// the attempt and, the result not being final, arms the job's backoff timer. The job stays
// RELEASED and pending, off the queue, with both its slots held (API-1h) and no event emitted,
// and the worker keeps running until stop: it has not returned when the job's backoff fires
// (the test stops the timer and runs its callback, requeue, itself), so it takes the job again
// and runs its next attempt, and it has returned once stop returns. TestRefundWorkerAttempt
// covers the other side of next()'s wait: a worker waiting on an empty queue is woken by a
// release.
func TestBackgroundWorkerRetriesQueuedJob(t *testing.T) {
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
			return attemptResult{sent: 1} // not final: the job is retried after its backoff
		},
	}, t.Context())
	t.Cleanup(r.stop)
	sl := newSlots(1, 1, reg)
	if !sl.tryAcquire() {
		t.Fatal("no free slot")
	}
	p := newPermit(sl, r, receipt.Receipt{}, "d1_retry", time.Now())
	timeout := time.NewTimer(effectsWait) // started before the wait it bounds
	defer timeout.Stop()
	p.Release()
	r.startWorkers(1)
	exited := make(chan struct{}) // closed once the worker has returned (by stop, at the latest)
	go func() {
		r.wg.Wait()
		close(exited)
	}()
	select {
	case got := <-attempted:
		if got != p {
			t.Fatal("the worker's attempt was for another permit")
		}
	case <-exited:
		t.Fatal("the worker returned without running an attempt, with a job queued and the refunds not stopped")
	case <-timeout.C:
		t.Fatalf("the worker ran no attempt within %v of its start", effectsWait)
	}
	// Arming the backoff timer is the worker's last step for the job, under r.mu.
	pollUntil(t, "the job's retry scheduled", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return p.retries == 1
	})
	r.mu.Lock()
	_, pending := r.pending[p]
	queued, attempting, backoffSet := p.index >= 0, p.attempting, p.backoff != nil
	r.mu.Unlock()
	if n := p.attempts.Load(); n != 1 {
		t.Fatalf("attempts %d, want 1: the one command the attempt sent (API-1d)", n)
	}
	if st := p.life.load(); st != stateReleased || !pending || queued || attempting || !backoffSet {
		t.Fatalf("after a retried attempt: %v, pending %v, queued %v, attempt in flight %v, backoff timer set %v; want RELEASED, pending, not queued, no attempt, a backoff timer",
			st, pending, queued, attempting, backoffSet)
	}
	if rs, fs := sl.inUse(); rs != 1 || fs != 1 {
		t.Fatalf("slots in use %d and %d while the refund is pending, want 1 and 1 (API-1h)", rs, fs)
	}
	if n := refundEvents(t, reg); n != 0 {
		t.Fatalf("%d final event(s) counted for a refund still pending, want 0", n)
	}

	// The job's 1 h backoff fires now: the test stops the timer and runs its callback.
	r.mu.Lock()
	armed := p.backoff.Stop()
	r.mu.Unlock()
	if !armed {
		t.Fatal("the job's 1 h backoff timer had fired or stopped already")
	}
	again := time.NewTimer(effectsWait) // started before the wait it bounds
	defer again.Stop()
	r.requeue(p)
	select {
	case got := <-attempted:
		if got != p {
			t.Fatal("the worker's second attempt was for another permit")
		}
	case <-exited:
		t.Fatal("the worker returned after its retry, with the refunds not stopped")
	case <-again.C:
		t.Fatalf("the worker ran no attempt within %v of the job's requeue", effectsWait)
	}
	r.stop() // it returns once every worker has
	select {
	case <-exited:
	case <-time.After(effectsWait):
		t.Fatal("the worker had not returned once stop returned")
	}
}

// TestBackgroundNewSignerWrapper (DESIGN §A5.12; SPEC RCP-4): with a signer wrapper set,
// newSigner hands the HMAC signer it makes from the key reader to the wrapper, once, and
// returns what the wrapper returns; with none, it returns the HMAC signer itself. It builds no
// limiter, and it checks the wrapper side first: TestBackgroundNewBuildsRefundListener's
// limiters, the first that newLimiter builds in the untagged build, which bin/mutate runs
// first, call newSigner with no wrapper. TestNewSignerWrapper covers the key and the error
// cases.
func TestBackgroundNewSignerWrapper(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	var gave []receipt.Signer
	var made *wrappedSigner
	s, err := newSigner(bytes.NewReader(key), func(in receipt.Signer) receipt.Signer {
		gave = append(gave, in)
		made = &wrappedSigner{in}
		return made
	})
	if err != nil || len(gave) != 1 || s != receipt.Signer(made) {
		t.Fatalf("with a signer wrapper: err %v, wrapper called %d time(s), the wrapper's signer returned %v; want no error, once, true",
			err, len(gave), len(gave) == 1 && s == receipt.Signer(made))
	}
	if _, ok := gave[0].(*receipt.HMACSigner); !ok {
		t.Fatalf("the signer wrapper got a %T, want the HMAC signer", gave[0])
	}
	s, err = newSigner(bytes.NewReader(key), nil)
	if _, ok := s.(*receipt.HMACSigner); err != nil || !ok {
		t.Fatalf("no signer wrapper: a %T, err %v; want the HMAC signer and no error", s, err)
	}
}

// TestBackgroundNewBuildsRefundListener (plan S6 step 6; DESIGN §A5.2, §A5.11):
// newLimiter applies a non-nil Option, so an instance given WithRefundListener has a refund
// listener, the one its refunds emit to; an instance given no listener, here only a nil Option,
// which newLimiter skips, has none. Each limiter is closed in its cleanup, which the cleanup
// tests' closeInCleanup bounds; the 100 ms drain timeout bounds Close's drain too, though
// nothing is pending.
func TestBackgroundNewBuildsRefundListener(t *testing.T) {
	p, in := lifecycleSettings()
	in.DrainTimeoutNs = 100_000_000
	l, err := newLimiter(p, in, StoreOptions{}, hookSet{storeWrapper: newStubAdapter().wrap, cleanupDraw: jitterDraw},
		[]Option{WithRefundListener(func(RefundEvent) {})})
	t.Cleanup(func() { closeInCleanup(t, l) })
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	if l.lis == nil || l.refunds.lis != l.lis {
		t.Fatalf("given WithRefundListener: a refund listener %v, the one the refunds emit to %v; want both",
			l.lis != nil, l.lis != nil && l.refunds.lis == l.lis)
	}
	none, err := newLimiter(p, in, StoreOptions{}, hookSet{storeWrapper: newStubAdapter().wrap, cleanupDraw: jitterDraw}, []Option{nil})
	t.Cleanup(func() { closeInCleanup(t, none) })
	if err != nil {
		t.Fatalf("newLimiter with a nil Option: %v", err)
	}
	if none.lis != nil || none.refunds.lis != nil {
		t.Fatalf("given no listener: a refund listener %v, the refunds emit to one %v; want neither", none.lis != nil, none.refunds.lis != nil)
	}
}
