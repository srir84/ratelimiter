package httpapi

import (
	"context"
	"fmt"
	"sync"
)

// slots is the in-flight cap (HAR-14, HOST-7): at most max /hit requests hold a slot at once,
// and there is no queue, so a request that finds every slot taken is refused at once (ERR-7).
type slots struct {
	mu      sync.Mutex
	n, max  int
	stopped bool          // stop ran: no new slot
	idle    chan struct{} // closed when n drops to 0 while waitIdle waits
}

func newSlots(maxInFlight int) *slots { return &slots{max: maxInFlight} }

// take reserves a slot without waiting. It returns false when every slot is taken or the
// API stopped accepting /hit.
func (s *slots) take() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.n >= s.max {
		return false
	}
	s.n++
	return true
}

// put frees a slot that take reserved.
func (s *slots) put() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n--
	if s.n == 0 && s.idle != nil {
		close(s.idle)
		s.idle = nil
	}
}

// inUse returns how many slots are taken (/health in_flight).
func (s *slots) inUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// stop refuses every later take.
func (s *slots) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
}

// waitIdle waits until no slot is taken, or ctx ends.
func (s *slots) waitIdle(ctx context.Context) error {
	s.mu.Lock()
	if s.n == 0 {
		s.mu.Unlock()
		return nil
	}
	if s.idle == nil {
		s.idle = make(chan struct{})
	}
	idle := s.idle
	s.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for in-flight /hit requests: %w", ctx.Err())
	}
}
