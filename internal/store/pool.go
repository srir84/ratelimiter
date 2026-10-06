package store

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"ratelimiter/internal/metrics"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/store/resp"
)

// connSet is the connections of one Kind (DESIGN §A5.4). Each slot runs its own dial loop
// (runSlot); calls pick the ready connection with the fewest commands in flight.
type connSet struct {
	kind    Kind
	life    context.Context // the adapter's lifetime
	metrics *metrics.Registry

	mu        sync.Mutex
	ready     []*pipeConn
	readyCh   chan struct{} // closed and replaced when a connection becomes ready, and at shutdown
	failed    bool          // the kind's last finished dial failed …
	failUntil time.Time     // … and its backoff runs until then
	dialing   map[net.Conn]struct{}
	closed    bool
}

func newConnSet(kind Kind, life context.Context, m *metrics.Registry) *connSet {
	return &connSet{
		kind:    kind,
		life:    life,
		metrics: m,
		readyCh: make(chan struct{}),
		dialing: make(map[net.Conn]struct{}),
	}
}

// pick returns the ready connection with the fewest commands in flight and reserves n
// commands on it, so concurrent picks spread out (DESIGN §A5.4). With none ready it fails
// fast (errFailFast) while the kind's last dial failed within its backoff; otherwise it
// waits for a connection to become ready until ctx ends, and returns ctx's cause
// (context.Cause), which tells the dependency deadline from the host's end. A closed set
// decides before ctx: after Close every call is unavailable, even with ctx already ended
// (plan S3 step 8).
func (s *connSet) pick(ctx context.Context, n int) (*pipeConn, error) {
	for {
		s.mu.Lock()
		if s.closed || s.life.Err() != nil {
			s.mu.Unlock()
			return nil, errAdapterClosed
		}
		if ctx.Err() != nil {
			s.mu.Unlock()
			return nil, context.Cause(ctx)
		}
		if len(s.ready) > 0 {
			best := s.ready[0]
			for _, pc := range s.ready[1:] {
				if pc.load.Load() < best.load.Load() {
					best = pc
				}
			}
			best.load.Add(int64(n))
			s.mu.Unlock()
			return best, nil
		}
		if s.failed && time.Now().Before(s.failUntil) {
			s.mu.Unlock()
			return nil, errFailFast
		}
		wait := s.readyCh
		s.mu.Unlock()
		select { // each wakeup is decided at the top, in the same order
		case <-wait:
		case <-ctx.Done():
		case <-s.life.Done():
		}
	}
}

// add makes pc ready; it reports false, adding nothing, once the set is shut down.
func (s *connSet) add(pc *pipeConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.ready = append(s.ready, pc)
	s.failed = false
	s.wakeLocked()
	s.gaugeLocked()
	return true
}

// remove takes pc out of the ready list (pipeConn.close calls it).
func (s *connSet) remove(pc *pipeConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.Index(s.ready, pc); i >= 0 {
		s.ready = slices.Delete(s.ready, i, i+1)
	}
	s.gaugeLocked()
}

// dialFailed records a failed dial whose backoff runs until until.
func (s *connSet) dialFailed(until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = true
	s.failUntil = until
}

// startDial registers a connection in its handshake, so shutdown can cut it short; it
// reports false once the set is shut down.
func (s *connSet) startDial(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.dialing[c] = struct{}{}
	return true
}

func (s *connSet) endDial(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dialing, c)
}

// shutdown marks the set closed, wakes every waiting pick, ends handshakes in progress and
// returns the ready connections for the caller to close. It closes each connection still in
// its handshake rather than moving its deadline, which the handshake could re-arm, so Close
// never waits out a handshake deadline (up to D) against a store that does not answer AUTH.
func (s *connSet) shutdown() []*pipeConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.wakeLocked()
	for c := range s.dialing {
		_ = c.Close() // the handshake fails at once; the dial loop's own Close is then a no-op
	}
	return slices.Clone(s.ready)
}

func (s *connSet) readyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ready)
}

func (s *connSet) wakeLocked() {
	close(s.readyCh)
	s.readyCh = make(chan struct{})
}

// gaugeLocked sets rl_store_connections{kind} to the ready count (§A5.11).
func (s *connSet) gaugeLocked() {
	if s.metrics != nil {
		s.metrics.Set(metrics.StoreConnections, connLabel(s.kind), float64(len(s.ready)))
	}
}

// connLabel is the rl_store_connections label index of a kind.
func connLabel(k Kind) int {
	switch k {
	case Call:
		return metrics.ConnCall
	case Refund:
		return metrics.ConnRefund
	case Maintenance:
		return metrics.ConnMaintenance
	}
	return metrics.ConnCall
}

// reconnectBackoff is the wait before redialing after n consecutive failed dials (DESIGN
// §A5.4): min(1 s, 50 ms × 2^n) × (0.5 + 0.5r), with r drawn from [0, 1).
func reconnectBackoff(n int, r float64) time.Duration {
	limit := time.Duration(settings.ReconnectBackoffMaxNs)
	base := 50 * time.Millisecond
	for i := 0; i < n && base < limit; i++ {
		base *= 2
	}
	base = min(base, limit)
	r = min(max(r, 0), 1)
	return time.Duration(float64(base) * (0.5 + 0.5*r))
}

// runSlot keeps one connection of set (DESIGN §A5.4): it dials and authenticates, makes the
// connection ready, waits for it to close, and redials, with backoff after a failed dial or
// a lost connection, until the adapter's lifetime ends. It owns the connection's reader.
func (a *StoreAdapter) runSlot(set *connSet) {
	defer a.wg.Done()
	failures := 0
	for {
		pc, err := a.dial(set)
		if err != nil {
			if a.life.Err() != nil {
				return
			}
			wait := reconnectBackoff(failures, a.rand())
			failures = min(failures+1, 32)
			set.dialFailed(time.Now().Add(wait))
			if !a.sleep(wait) {
				return
			}
			continue
		}
		failures = 0
		if !set.add(pc) {
			_ = pc.conn.Close() // shut down meanwhile; never used
			return
		}
		go pc.readLoop()
		select {
		case <-pc.done:
		case <-a.life.Done():
			pc.close()
		}
		<-pc.readerDone
		if a.life.Err() != nil || !a.sleep(reconnectBackoff(0, a.rand())) {
			return
		}
	}
}

// dial connects one connection of set and authenticates it; connecting and the handshake
// share one deadline, D from now (DESIGN §A5.4). A failure anywhere sent nothing that can
// change limiter state.
func (a *StoreAdapter) dial(set *connSet) (*pipeConn, error) {
	deadline := time.Now().Add(a.d)
	ctx, cancel := context.WithDeadline(a.life, deadline)
	defer cancel()
	c, err := a.dialFn(ctx, "tcp", a.addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to the store: %w", err)
	}
	if !set.startDial(c) {
		_ = c.Close()
		return nil, errAdapterClosed
	}
	err = resp.Handshake(c, a.user, a.pass, deadline, a.obs)
	set.endDial(c)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("store handshake: %w", err)
	}
	return newPipeConn(c, a.d, a.obs, set.remove), nil
}

// sleep waits d, or reports false when the adapter's lifetime ends first.
func (a *StoreAdapter) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-a.life.Done():
		return false
	}
}
