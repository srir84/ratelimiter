package core

import (
	"container/heap"
	"context"
	"encoding/hex"
	"math"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
)

// attemptResult is how one refund attempt ended (DESIGN §A5.6, §A4.9). final marks codes 3,
// 4, 10 and 11, with the result and, for applied and already_applied, the credit's
// decision_ns; every other outcome is retried. sent is the number of refund commands the
// attempt sent (Response.SentCommands, API-1d); epochWide asks the epoch watch for an early
// read (§A5.7).
type attemptResult struct {
	final     bool
	result    RefundResult
	creditNs  int64
	sent      int
	epochWide bool
}

// classifyRefund maps one refund response to its attemptResult (§A5.6; SPEC API-13 … API-19).
// Final: E_APPLIED (credit time t), E_ALREADY_APPLIED (the original credit time credit_us),
// E_RECEIPT_INVALID and E_RECEIPT_EXPIRED. Retried: codes 20–25 (20 and 22 also ask for an
// early epoch read), unavailable, cancelled, outcome_unknown and capacity_error. The adapter
// has checked the reply's layout and ranges (§A4.9, with the receipt's expiry expiryUs); a
// reply that fails them here all the same is retried as outcome_unknown, never final. The
// switch names every refund code of §A4.9; any other code is retried as outcome_unknown.
func classifyRefund(resp store.Response, ratePeriodNs, expiryUs int64) attemptResult {
	out := attemptResult{sent: max(resp.SentCommands, 0)}
	rep := resp.Reply
	if resp.Outcome != store.OK || rep.TUs < 0 || rep.TUs >= tBound {
		return out
	}
	switch rep.Code {
	case 3: // E_APPLIED (applied, API-13): {3, t, '', credited_units}; final
		if c, ok := elemInt(rep.Elems, 3); ok && len(rep.Elems) == 4 && c >= 0 && c <= ratePeriodNs {
			out.final, out.result, out.creditNs = true, Applied, DecisionNs(rep.TUs)
		}
	case 4: // E_ALREADY_APPLIED (already_applied, API-14): {4, t, '', credit_us}, credit_us ≤ t and before the receipt's expiry; final
		if c, ok := elemInt(rep.Elems, 3); ok && len(rep.Elems) == 4 && c >= 0 && c <= rep.TUs && c < expiryUs {
			out.final, out.result, out.creditNs = true, AlreadyApplied, DecisionNs(c)
		}
	case 10: // E_RECEIPT_INVALID (receipt_invalid, API-15): final
		out.final, out.result = true, ReceiptInvalid
	case 11: // E_RECEIPT_EXPIRED (receipt_expired, API-16): final
		out.final, out.result = true, ReceiptExpired
	case 20: // E_CONFIG (config_error, API-19): epoch-wide, retried after an early epoch read
		out.epochWide = true
	case 21: // E_CLOCK (clock_error, API-19): retried
	case 22: // E_INTEGRITY_EPOCH (epoch-wide state_integrity_error, API-19): retried after an early epoch read
		out.epochWide = true
	case 23: // E_INTEGRITY_KEY (per-key state_integrity_error, API-19): retried
	case 24: // E_CAPACITY (capacity_error, API-18): retried
	case 25: // E_BAD_CALL: a library defect, reported as outcome_unknown and retried
	}
	return out
}

// refundAttempt is one refund attempt for p's receipt (§A5.6): a receipt whose MAC does not
// verify is final receipt_invalid with no store call (API-2a step 1); otherwise one
// Adapter.Call on the refund connections within D (the adapter's deadline on ctx, the refund
// workers' context), under the epoch the watch joined last, with its floors (§A4.1, §A5.7).
// A store error reply is kept for Health.
func (l *Limiter) refundAttempt(ctx context.Context, p *Permit) attemptResult {
	rc := p.receipt
	if !l.signer.Verify(rc) {
		return attemptResult{final: true, result: ReceiptInvalid}
	}
	st := l.epoch.Load()
	if st.id == "" { // never joined: no receipt exists before a join; nothing is sent
		return attemptResult{}
	}
	resp := l.adapter.Call(ctx, l.refundRequest(rc, st))
	if resp.StoreError != "" {
		l.setStoreError(resp.StoreError)
	}
	res := classifyRefund(resp, l.p.RatePeriodNs, rc.ExpiryUs)
	if res.epochWide {
		l.watch.trigger()
	}
	return res
}

// refundRequest is the refund call of §A4.1 under the joined epoch st: KEYS epoch, ctl, ix, q,
// k:<enc>, mi:<enc>; ARGV epoch_id, fp, enc, the receipt's epoch_id (hex), g and s (canonical
// decimal), expiry_us, cap_units, rate_period_ns, rate_tokens, max_markers,
// max_markers_per_key, floor_hwm, floor_gen. The receipt's expiry also goes to the decoder
// (ReceiptExpiryUs, §A4.9).
func (l *Limiter) refundRequest(rc receipt.Receipt, st *epochState) store.Request {
	num := func(v int64) string { return strconv.FormatInt(v, 10) }
	return store.Request{
		Kind:   store.Refund,
		Script: scripts.Refund,
		Keys: []string{
			keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(st.id), keys.QuarantineKey(st.id),
			keys.BucketKey(st.id, rc.Enc), keys.MarkerIndexKey(st.id, rc.Enc),
		},
		Args: []string{
			st.id, l.fp, rc.Enc, hex.EncodeToString(rc.EpochID[:]),
			strconv.FormatUint(rc.G, 10), strconv.FormatUint(rc.S, 10), num(rc.ExpiryUs),
			num(l.capUnits), num(l.p.RatePeriodNs), num(l.p.RateTokens),
			num(l.p.MaxLiveMarkers), num(l.p.MaxMarkersPerKey), num(st.floorHwm), num(st.floorGen),
		},
		ReceiptExpiryUs: rc.ExpiryUs,
	}
}

// backoffDelay is min(maxD, initial × 2^n) × (0.5 + 0.5 × u) for u in [0, 1): the refund
// retry delay of §A5.6, with its random factor in [0.5, 1.0].
func backoffDelay(n int, initial, maxD time.Duration, u float64) time.Duration {
	d := min(initial, maxD)
	for range n {
		d = min(2*d, maxD) // d ≤ maxD, a validated setting far below 2^62 ns: no overflow
	}
	return time.Duration(float64(d) * (0.5 + 0.5*u))
}

// readyQueue is the refund ready queue (§A5.6): a min-heap (container/heap) of RELEASED
// permits ordered by release time, earliest first, equal times in queuing order; a retry keeps
// its original release time. Its capacity is MaxPendingRefunds. refunds.mu guards it, and its
// sync.Cond (on that mutex) wakes the workers.
type readyQueue struct {
	h    permitHeap
	cap  int
	cond *sync.Cond
}

// push queues p, or returns false when the queue is full.
func (q *readyQueue) push(p *Permit) bool {
	if len(q.h) >= q.cap {
		return false
	}
	heap.Push(&q.h, p)
	q.cond.Signal()
	return true
}

// pop removes and returns the earliest job; nil when the queue is empty.
func (q *readyQueue) pop() *Permit {
	if len(q.h) == 0 {
		return nil
	}
	p, _ := heap.Pop(&q.h).(*Permit)
	return p
}

// remove takes p out of the queue when it is queued (index ≥ 0: Push and Swap keep each
// queued permit's index, Pop sets -1).
func (q *readyQueue) remove(p *Permit) {
	if p.index >= 0 {
		heap.Remove(&q.h, p.index)
	}
}

// permitHeap is heap.Interface over permits, keyed by (release time, queuing order).
type permitHeap []*Permit

func (h permitHeap) Len() int { return len(h) }

func (h permitHeap) Less(i, j int) bool {
	ri, rj := h[i].releaseMono.Load(), h[j].releaseMono.Load()
	if ri != rj {
		return ri < rj
	}
	return h[i].seq < h[j].seq
}

func (h permitHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}

func (h *permitHeap) Push(x any) {
	p, _ := x.(*Permit)
	p.index = len(*h)
	*h = append(*h, p)
}

func (h *permitHeap) Pop() any {
	old := *h
	n := len(old)
	p := old[n-1]
	old[n-1] = nil
	p.index = -1
	*h = old[:n-1]
	return p
}

// refundsConfig is what newRefunds needs.
type refundsConfig struct {
	capacity                   int           // MaxPendingRefunds: the ready queue's capacity
	lifetime                   time.Duration // receipt_lifetime: the retention timer
	backoffInitial, backoffMax time.Duration // §A5.6
	born                       time.Time     // the limiter's construction: the origin of the event times
	closing                    *atomic.Bool  // the limiter's closing flag
	reg                        *metrics.Registry
	lis                        *listener // nil without a refund listener
	// attempt runs one refund attempt; nil for refunds without workers (tests).
	attempt func(context.Context, *Permit) attemptResult
}

// refunds is an instance's refund machinery (DESIGN §A5.5, §A5.6): the pending refunds (the
// RELEASED permits), the ready queue, the refund workers, the retention and backoff timers,
// and the final events. One mutex guards the queue, the sets and every timer field of a
// permit, so a permit that turns FINAL leaves the queue and stops its timers in one step.
type refunds struct {
	refundsConfig

	mu      sync.Mutex
	queue   readyQueue
	pending map[*Permit]struct{} // RELEASED permits, until FINAL
	timers  map[*Permit]struct{} // permits whose retention timer is armed
	stopped bool                 // stop ran: no timer is armed and no job queued again; workers end
	drained bool                 // Close's drain ended: a refund queued later ends unresolved (closed) at once
	idle    chan struct{}        // closed when pending empties while waitIdle waits
	abort   chan struct{}        // closed by stop: waitIdle returns
	seq     uint64

	ctx    context.Context    // the workers' context, a child of the lifetime context
	cancel context.CancelFunc // ends the attempts in flight (stop)
	wg     sync.WaitGroup     // the workers
}

// newRefunds returns refunds whose workers (startWorkers) run on a child of life.
func newRefunds(c refundsConfig, life context.Context) *refunds {
	r := &refunds{
		refundsConfig: c,
		pending:       map[*Permit]struct{}{},
		timers:        map[*Permit]struct{}{},
		abort:         make(chan struct{}),
	}
	r.queue = readyQueue{cap: max(c.capacity, 0), cond: sync.NewCond(&r.mu)}
	r.ctx, r.cancel = context.WithCancel(life)
	return r
}

// startWorkers starts n refund workers (§A5.6); stop ends them.
func (r *refunds) startWorkers(n int) {
	for range n {
		r.wg.Add(1)
		go r.work()
	}
}

// work is one refund worker: it takes the earliest ready job, whose attempt next has already
// put in flight, runs that attempt, counts the refund commands it sent, ends the attempt, then
// ends the job on a final result or schedules its retry; a backoff never holds the worker (§A7
// step 2). The attempt runs on its own context, which the retention timer cancels when it ends
// the permit meanwhile; the timer then leaves the permit's slots and its event to this worker,
// which frees the slots and then emits the event once the commands are counted (endAttempt;
// API-1d, API-1h).
func (r *refunds) work() {
	defer r.wg.Done()
	for {
		p, ctx, ok := r.next()
		if !ok {
			return
		}
		res := r.attempt(ctx, p)
		if res.sent > 0 {
			p.attempts.Add(int64(res.sent)) // only commands that were sent count (API-1d)
		}
		r.endAttempt(p)
		if res.final {
			p.finish(res)
			continue
		}
		r.retry(p)
	}
}

// endAttempt ends p's attempt once the worker has added the refund commands it sent: the
// attempt's context is released, and the effects a transition left to this worker while the
// attempt was in flight (settle) follow in apply's order: the slots are freed, then the event
// is emitted, counting those commands (API-1d), so the permit holds its slots until its final
// event (API-1h). When no transition left any (held is the zero transition), nothing more
// happens: emit does nothing for emitNone.
func (r *refunds) endAttempt(p *Permit) {
	r.mu.Lock()
	cancel, held := p.cancelAttempt, p.held
	p.attempting, p.cancelAttempt, p.held = false, noAttempt, transition{}
	r.mu.Unlock()
	cancel()
	if held.freeSlots && p.slots != nil {
		p.slots.release()
	}
	r.emit(p, held.emit, attemptResult{})
}

// noAttempt is a permit's cancelAttempt while no refund attempt is in flight: there is nothing
// to cancel.
func noAttempt() {}

// next waits for a ready job and returns it with the context of its refund attempt (§A5.6),
// which is in flight from then until endAttempt: the job leaves the queue and its attempt
// starts under one hold of mu, so no transition's effects (settle) can run in between: a
// transition that ends the permit meanwhile, or later, finds the attempt in flight and cancels
// that context; the attempt then sends nothing more. The context is a child of the workers'
// context. next returns false once stop ran. A job already FINAL is skipped (its transition
// removed it from the queue; this is a second guard).
func (r *refunds) next() (*Permit, context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		for !r.stopped && len(r.queue.h) == 0 {
			r.queue.cond.Wait()
		}
		if r.stopped {
			return nil, nil, false
		}
		if p := r.queue.pop(); p.life.load() == stateReleased {
			ctx, cancel := context.WithCancel(r.ctx)
			p.attempting, p.cancelAttempt = true, cancel
			return p, ctx, true
		}
	}
}

// retry arms p's backoff timer (§A5.6): min(RefundBackoffMax, RefundBackoffInitial × 2^n) ×
// U[0.5, 1.0]; when it fires, p goes back to the queue with its original release time. A job
// already FINAL, or one whose refunds stopped, is dropped.
func (r *refunds) retry(p *Permit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || p.life.load() != stateReleased {
		return
	}
	d := r.backoff(p.retries)
	p.retries++
	p.backoff = time.AfterFunc(d, func() { r.requeue(p) })
}

// backoff draws the delay of retry n (§A5.6) from the global math/rand/v2 source (AGENTS.md §1).
func (r *refunds) backoff(n int) time.Duration {
	return backoffDelay(n, r.backoffInitial, r.backoffMax, rand.Float64())
}

// requeue is a backoff timer's callback: p goes back to the ready queue unless it is FINAL or
// the refunds stopped.
func (r *refunds) requeue(p *Permit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.backoff = nil
	if r.stopped || p.life.load() != stateReleased {
		return
	}
	r.queueLocked(p)
}

// queueLocked puts a pending p on the ready queue; the caller holds mu. The queue holds only
// pending permits, at most one entry each, and each pending permit holds a refund slot, so it
// cannot be full (§A5.5); if it ever were, p would retry after a backoff instead of being lost.
func (r *refunds) queueLocked(p *Permit) {
	if r.queue.push(p) {
		return
	}
	d := r.backoff(p.retries)
	p.backoff = time.AfterFunc(d, func() { r.requeue(p) })
}

// enqueue is OPEN → RELEASED's effect: p becomes pending and goes on the ready queue. A
// permit some other transition already ended is left alone; after Close's drain ended, or
// once the refunds stopped, the refund ends at once as unresolved (closed).
func (r *refunds) enqueue(p *Permit) {
	r.mu.Lock()
	if p.life.load() != stateReleased {
		r.mu.Unlock()
		return
	}
	if r.stopped || r.drained {
		r.mu.Unlock()
		p.drainEnd()
		return
	}
	r.pending[p] = struct{}{}
	r.reg.Set(metrics.PendingRefunds, 0, float64(len(r.pending)))
	r.seq++
	p.seq = r.seq
	r.queueLocked(p)
	r.mu.Unlock()
}

// settle runs first in the effects of every transition t but OPEN → RELEASED: p's retention
// timer is disarmed, and a pending p leaves the pending set, the ready queue and its backoff
// timer, before its slots are freed (apply). When a refund attempt for p is in flight (from
// next to endAttempt), the attempt is cancelled and the rest of t's effects, the slot release
// and the event, are held for the worker running it: settle reports true, and apply performs
// nothing more. The worker adds the commands the attempt sent, then frees the slots, then
// emits the event (endAttempt), so the permit holds its slots until its final event (API-1h)
// and the event's Attempts counts a command that was sent (API-1d). Only the retention timer
// can end a permit while an attempt is in flight: the worker's own final result comes after
// endAttempt, and the drain's end after the workers stopped. The timer is still the
// transition's winner, as §A5.5 says; it hands the slot release and the event to that worker.
func (r *refunds) settle(p *Permit, t transition) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.retention != nil {
		p.retention.Stop()
		p.retention = nil
	}
	delete(r.timers, p)
	held := p.attempting
	if held {
		p.held = t
		p.cancelAttempt()
	}
	if _, ok := r.pending[p]; !ok {
		return held
	}
	r.queue.remove(p)
	if p.backoff != nil {
		p.backoff.Stop()
		p.backoff = nil
	}
	delete(r.pending, p)
	r.reg.Set(metrics.PendingRefunds, 0, float64(len(r.pending)))
	if len(r.pending) == 0 && r.idle != nil {
		close(r.idle)
		r.idle = nil
	}
	return held
}

// armRetention starts p's retention timer (§A5.5, REF-4a): it fires receipt_lifetime after
// the instant just before the acquire was sent. Once the refunds stopped no timer is armed.
func (r *refunds) armRetention(p *Permit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	p.retention = time.AfterFunc(time.Until(p.sentAt.Add(r.lifetime)), p.expire)
	r.timers[p] = struct{}{}
}

// timersRunning reports whether stop has not run yet: a retention timer that fires after it
// does nothing.
func (r *refunds) timersRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.stopped
}

// monoNs is the time since the limiter was built, in ns, at least 1 (the event times,
// plan S6 step 6).
func (r *refunds) monoNs() int64 { return max(time.Since(r.born).Nanoseconds(), 1) }

// emit builds p's one final event (API-1d), counts it in rl_refund_results_total and hands it
// to the listener, if any.
func (r *refunds) emit(p *Permit, kind emission, res attemptResult) {
	ev := RefundEvent{
		DeductionID:   p.id,
		Attempts:      attemptCount(p.attempts.Load()),
		ReleaseMonoNs: p.releaseMono.Load(),
		FinalMonoNs:   r.monoNs(),
	}
	switch kind {
	case emitResult:
		ev.Result, ev.CreditDecisionNs = res.result, res.creditNs
	case emitClosed:
		ev.Result, ev.Reason = Unresolved, UnresolvedClosed
	case emitRetention:
		ev.Result, ev.Reason = Unresolved, UnresolvedRetention
	case emitExpired:
		ev.Result, ev.Reason = Unresolved, UnresolvedExpired
	case emitNone:
		return
	}
	r.reg.Inc(metrics.RefundResults, int(ev.Result))
	if r.lis != nil {
		r.lis.emit(ev)
	}
}

// attemptCount converts an attempt count for RefundEvent.Attempts, saturating at 2^31 − 1 so
// it fits an int on every platform.
func attemptCount(n int64) int {
	if n > math.MaxInt32 {
		n = math.MaxInt32
	}
	return int(n)
}

// waitIdle waits until no refund is pending, the deadline passes, or stop runs; it reports
// whether none is pending.
func (r *refunds) waitIdle(deadline time.Time) bool {
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return true
	}
	if r.idle == nil {
		r.idle = make(chan struct{})
	}
	idle := r.idle
	r.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
	case <-r.abort:
	}
	return false
}

// drain is Close's part of the refunds (SPEC API-1e; plan S6 step 7): it waits until no refund
// is pending or the deadline passes; then it stops the workers, cancelling the attempts in
// flight and waiting for them, so each adds the refund commands it sent before any event is
// built (API-1d: an attempt whose command was sent counts), one that ends with a final result
// keeps it, and one whose retention timer left it the slots and the event frees the slots and
// then emits the event (endAttempt); last, the refunds still pending end as unresolved
// (closed). The cancel ends a store call at once, so drain returns soon after the deadline.
func (r *refunds) drain(deadline time.Time) {
	r.waitIdle(deadline)
	r.stop()
	r.drainEnd()
}

// drainEnd ends Close's drain (API-1e): every refund still pending ends as unresolved
// (closed), and any queued later ends so at once.
func (r *refunds) drainEnd() {
	r.mu.Lock()
	r.drained = true
	left := make([]*Permit, 0, len(r.pending))
	for p := range r.pending {
		left = append(left, p)
	}
	r.mu.Unlock()
	for _, p := range left {
		p.drainEnd()
	}
}

// stop ends the refund machinery at once (Close after its drain, Kill): no timer fires and no
// job is queued again, the attempts in flight end, and stop returns once every worker has.
// Refunds still pending stay so: Close ends them next (drainEnd); under Kill they are lost.
func (r *refunds) stop() {
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		close(r.abort)
		for p := range r.timers {
			if p.retention != nil {
				p.retention.Stop()
				p.retention = nil
			}
		}
		clear(r.timers)
		for p := range r.pending {
			if p.backoff != nil {
				p.backoff.Stop()
				p.backoff = nil
			}
		}
		r.queue.cond.Broadcast()
	}
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}
