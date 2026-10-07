package core

import (
	"context"
	"math/rand/v2"
	"slices"
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

// nextWithin takes the next ready job from r as a worker does (next), and fails the test when
// none comes within 5 s, so an empty queue fails by assertion instead of blocking until the
// test binary's timeout. The wait runs in a goroutine; when the test gives up on it, r's
// cleanup stop ends it.
func nextWithin(t *testing.T, r *refunds) *Permit {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second) // started before the wait it bounds
	defer timeout.Stop()
	type job struct {
		p  *Permit
		ok bool
	}
	got := make(chan job, 1) // buffered: a wait the test gave up on still ends
	go func() {
		p, ok := r.next()
		got <- job{p, ok}
	}()
	select {
	case j := <-got:
		if !j.ok {
			t.Fatal("next() reported the refunds stopped, want a job")
		}
		return j.p
	case <-timeout.C:
		t.Fatal("next() returned no job within 5 s")
	}
	return nil
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
	if p := nextWithin(t, r); p != a {
		t.Fatal("the worker did not take A, the earliest job, first")
	}
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
	if p := nextWithin(t, r); p != a {
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
	if p := nextWithin(t, r); p != released {
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
