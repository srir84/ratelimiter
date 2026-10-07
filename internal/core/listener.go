package core

import (
	"sync"
	"sync/atomic"
	"time"

	"ratelimiter/internal/metrics"
)

// WithRefundListener has f receive each released permit's one final refund event (SPEC
// API-1d; DESIGN §A5.2, §A5.11). Events pass through a queue of ListenerQueueSize to one
// dispatcher goroutine, which calls f inside recover(): f may block or panic without
// affecting accounting, retries or slots, and an event that finds the queue full is dropped
// and counted (Health.ListenerDrops, rl_listener_drops_total). f must not call back into the
// limiter (HOST-8). A nil f sets no listener.
func WithRefundListener(f func(RefundEvent)) Option {
	return func(o *options) { o.listener = f }
}

// listener is the refund listener's queue and dispatcher (DESIGN §A5.11). The channel is never
// closed: stop closes done instead, and from then on emit drops every event under the closed
// flag, which mu guards; events still queued when the dispatcher stops are never delivered and
// are counted as drops, so drops + delivered = emitted (T-REF-7).
type listener struct {
	f          func(RefundEvent)
	ch         chan RefundEvent
	done       chan struct{} // closed by stop: the dispatcher returns before its next callback
	exited     chan struct{} // closed when the dispatcher has returned
	reg        *metrics.Registry
	drops      atomic.Uint64 // Health.ListenerDrops
	delivering atomic.Bool   // the dispatcher is in the host's callback; set under mu, never once closed

	mu     sync.Mutex
	closed bool          // stop ran
	queued int           // events sent on ch and not yet delivered or dropped
	idle   chan struct{} // closed when queued drops to 0 while waitIdle waits
}

// newListener returns the listener for f with a queue of size events; start starts its
// dispatcher.
func newListener(f func(RefundEvent), size int, reg *metrics.Registry) *listener {
	return &listener{
		f: f, ch: make(chan RefundEvent, max(size, 1)),
		done: make(chan struct{}), exited: make(chan struct{}), reg: reg,
	}
}

// start starts the dispatcher goroutine; stop ends it.
func (s *listener) start() { go s.dispatch() }

// emit queues ev for the listener without waiting: a full queue, or a stopped listener, drops
// it and counts the drop (API-1d).
func (s *listener) emit(ev RefundEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.drop()
		return
	}
	select {
	case s.ch <- ev:
		s.queued++
	default:
		s.drop()
	}
}

// drop counts one dropped event.
func (s *listener) drop() {
	s.drops.Add(1)
	s.reg.Inc(metrics.ListenerDrops, 0)
}

// dispatch delivers queued events one at a time until stop closes done. An event taken from
// the queue once stop ran is dropped, not delivered; the check and the start of a callback
// share one hold of mu, so once stop ran no callback starts.
func (s *listener) dispatch() {
	defer close(s.exited)
	for {
		select {
		case <-s.done:
			return
		case ev := <-s.ch:
			s.mu.Lock()
			if s.closed {
				s.drop()
				s.doneOne()
				s.mu.Unlock()
				return
			}
			s.delivering.Store(true)
			s.mu.Unlock()
			s.deliver(ev)
			s.mu.Lock()
			s.delivering.Store(false)
			s.doneOne()
			s.mu.Unlock()
		}
	}
}

// deliver calls the host's listener; a panic in it is recovered and ignored (DESIGN §A5.11:
// host code may panic, and listener errors never affect the limiter, API-1d).
func (s *listener) deliver(ev RefundEvent) {
	defer func() { _ = recover() }()
	s.f(ev)
}

// doneOne marks one queued event delivered or dropped; the caller holds mu.
func (s *listener) doneOne() {
	s.queued--
	if s.queued == 0 && s.idle != nil {
		close(s.idle)
		s.idle = nil
	}
}

// waitIdle waits until every queued event was delivered or dropped, or the deadline passes,
// or the listener is stopped.
func (s *listener) waitIdle(deadline time.Time) {
	s.mu.Lock()
	if s.closed || s.queued == 0 {
		s.mu.Unlock()
		return
	}
	if s.idle == nil {
		s.idle = make(chan struct{})
	}
	idle := s.idle
	s.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
	case <-s.done:
	}
}

// stop stops the dispatcher without waiting for it (a callback may be blocked): later emits
// are dropped, and so are the events still queued.
func (s *listener) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

// stopLocked is stop with mu held.
func (s *listener) stopLocked() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
	for {
		select {
		case <-s.ch:
			s.drop()
			s.doneOne()
		default:
			return
		}
	}
}

// waitExit runs after stop: it waits until the dispatcher has returned, but past the deadline
// only while the dispatcher is not in the host's callback, so a callback still running is never
// waited for past it. A stopped dispatcher outside a callback returns at once (it drops an
// event it had taken first), so when waitExit returns every event emitted is delivered,
// dropped and counted, or in that callback.
func (s *listener) waitExit(deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-s.exited:
		return
	case <-timer.C:
	}
	select {
	case <-s.done: // stopped: past the deadline, wait only for a dispatcher in no callback
		if !s.delivering.Load() {
			<-s.exited
		}
	default: // not stopped: nothing is waited for past the deadline
	}
}

// listenerDrops is Health.ListenerDrops: the events the listener queue dropped (0 without a
// listener).
func (l *Limiter) listenerDrops() uint64 {
	if l.lis == nil {
		return 0
	}
	return l.lis.drops.Load()
}
