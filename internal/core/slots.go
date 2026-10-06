package core

import (
	"sync"

	"ratelimiter/internal/metrics"
)

// slots are the two counting semaphores of DESIGN §A5.5 (SPEC RCP-9): held receipts
// (MaxHeldReceipts) and pending refunds (MaxPendingRefunds). An acquire reserves one of each
// with a non-blocking paired try before it sends anything, so the effective concurrency is
// the smaller of the two, and a permit frees both together. rl_slots_in_use follows every
// change.
type slots struct {
	mu                      sync.Mutex
	receipts, refunds       int // in use
	maxReceipts, maxRefunds int
	reg                     *metrics.Registry
}

// newSlots returns empty slots with the given capacities; a capacity below 0 counts as 0.
func newSlots(maxReceipts, maxRefunds int, reg *metrics.Registry) *slots {
	return &slots{maxReceipts: max(maxReceipts, 0), maxRefunds: max(maxRefunds, 0), reg: reg}
}

// tryAcquire reserves one receipt slot and one refund slot, or neither: it returns false at
// once, without waiting, when either kind is full (busy, API-7b).
func (s *slots) tryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receipts >= s.maxReceipts || s.refunds >= s.maxRefunds {
		return false
	}
	s.receipts++
	s.refunds++
	s.publish()
	return true
}

// release frees one slot of each kind. Callers free a reservation once (the permit's
// compare-and-swap, or a non-allowed Acquire); a count never goes below 0.
func (s *slots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = max(s.receipts-1, 0)
	s.refunds = max(s.refunds-1, 0)
	s.publish()
}

// inUse returns the slots in use of each kind.
func (s *slots) inUse() (receipts, refunds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receipts, s.refunds
}

// publish sets rl_slots_in_use; the caller holds mu.
func (s *slots) publish() {
	s.reg.Set(metrics.SlotsInUse, metrics.SlotReceipt, float64(s.receipts))
	s.reg.Set(metrics.SlotsInUse, metrics.SlotRefund, float64(s.refunds))
}
