package core

import "time"

// WithDrainTimeout makes Close drain for d instead of the instance's DrainTimeoutNs setting
// (SPEC API-1e, CFG-8). A d of 0 or less drains nothing: every pending refund ends as
// unresolved (closed) at once (plan choice for a negative d).
func WithDrainTimeout(d time.Duration) CloseOption {
	return func(o *closeOptions) { o.drain = d }
}

// Close is the instance's graceful stop (SPEC API-1e, STA-15; DESIGN §A5.2). From its start
// Acquire returns not_ready and sends nothing, Health reports closing, and a release() on an
// OPEN permit ends as unresolved (closed) with nothing sent; the refund workers keep retrying
// the pending refunds, with normal store checks, until none is pending or the drain timeout
// (WithDrainTimeout, else DrainTimeoutNs) passes. Then the workers and every retention and
// backoff timer stop: the attempts still in flight are cancelled and counted (refunds.drain),
// and the refunds still pending end as unresolved (closed). The listener gets until the same
// drain deadline to take the queued events; then its dispatcher is stopped without waiting
// for a callback that is blocked, and later events are dropped: when Close returns, every
// event is delivered, dropped and counted, or in a callback still running. Last, the epoch
// watch stops and every connection closes. Close returns as soon as that is done; a second
// Close returns at once (plan choice).
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
		l.lis.waitIdle(deadline)
		l.lis.stop()
		l.lis.waitExit(deadline)
	}
	l.shutdown()
}
