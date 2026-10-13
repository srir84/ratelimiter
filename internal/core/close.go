package core

import "time"

// listenerGrace is how long past the drain's end Close still lets the listener's dispatcher
// deliver the events already queued, when the drain deadline leaves it less (plan S6 step 7),
// to meet SPEC API-1e (Close waits, within this window, for the listener to finish handling
// the queued events (each callback has returned)), API-1d (the promise holds while no event is
// dropped), STA-15 (the refunds still pending after the drain are emitted as unresolved
// (closed)) and T-REF-6 (b).
// The drain's end queues at most one event per pending refund (MaxPendingRefunds, 1,152 at
// harness values) besides the odd release() after Close began, and a listener that hands
// events off (HOST-8) takes microseconds per event, so the queue empties and the last callback
// returns within milliseconds, even under the race detector or behind a scheduling delay;
// Close moves on as soon as the queue is empty and no callback is running. The whole grace is
// spent only on a slow or blocked listener: it adds up to 100 ms of shutdown overhead
// (HOST-6). hitapi's timed worst case, P 5 s + drain_timeout 35 s + this 0.1 s + its 4 s
// run-log drain = 44.1 s, leaves 0.9 s of the harness's 45 s kill grace (DESIGN §B10) for the
// close steps hitapi does not time, which take milliseconds unless the disk or PostgreSQL
// stalls (cmd/hitapi's logDrainTimeout lists them in order); TestListenerGraceBudget caps the
// grace at 100 ms, and cmd/hitapi's TestShutdownBudget pins the timed sum.
const listenerGrace = 100 * time.Millisecond

// listenerDeadline is the end of the time Close gives the listener to deliver the events
// queued by a drain that ended at now: the drain deadline, or listenerGrace past now when that
// is later, as it is when the drain ended less than the grace before its deadline, at it, or
// past it (the drain waits for the attempts in flight, which end after their cancel).
func listenerDeadline(now, drainDeadline time.Time) time.Time {
	return now.Add(max(drainDeadline.Sub(now), listenerGrace))
}

// WithDrainTimeout makes Close drain for d instead of the instance's DrainTimeoutNs setting
// (SPEC API-1e, CFG-8). A d of 0 or less drains nothing: every pending refund ends as
// unresolved (closed) at once (plan choice for a negative d).
func WithDrainTimeout(d time.Duration) CloseOption {
	return func(o *closeOptions) { o.drain = d }
}

// Close is the instance's graceful stop (SPEC API-1e, STA-15; DESIGN §A5.2). From its start a
// new Acquire returns not_ready and sends nothing, Health reports closing, and a release() on
// an OPEN permit ends as unresolved (closed) with nothing sent; the refund workers keep
// retrying the pending refunds, with normal store checks, until none is pending or the drain
// timeout (WithDrainTimeout, else DrainTimeoutNs) passes. Then the workers and every retention
// and backoff timer stop: the attempts still in flight are cancelled and counted
// (refunds.drain), and the refunds still pending end as unresolved (closed), their events
// queued for the listener (SPEC API-1d, API-1e, STA-15). The listener gets until the drain
// deadline, or listenerGrace past the drain's end when that is later, to deliver the queued
// events, and Close moves on as soon as the queue is empty and no callback is running; then
// its dispatcher is stopped without waiting for a callback that is blocked, and the events
// still queued behind such a callback, and later events, are dropped: when Close returns,
// every event is delivered, dropped and counted, or in a callback still running. Last, the
// epoch watch and the cleanup runner stop (a timed pass from Close's start on returns
// unavailable, sending nothing), and then every connection closes. Close returns as soon as
// that is done; a second Close returns at once (plan choice).
func (l *Limiter) Close(opts ...CloseOption) {
	o := closeOptions{drain: time.Duration(l.in.DrainTimeoutNs)}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if l.closeStarted.Swap(true) {
		return
	}
	l.closing.Store(true)
	deadline := time.Now().Add(max(o.drain, 0))
	if l.refunds != nil {
		l.refunds.drain(deadline)
	}
	if l.lis != nil {
		// The drain has ended and queued its events.
		lisDeadline := listenerDeadline(time.Now(), deadline)
		l.lis.waitIdle(lisDeadline)
		l.lis.stop()
		l.lis.waitExit(lisDeadline)
	}
	l.shutdown()
}
