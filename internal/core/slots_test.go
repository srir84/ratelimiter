package core

import (
	"testing"

	"ratelimiter/internal/metrics"
)

// TestSlotsPairedTry: the paired try reserves one slot of each kind or neither, so the
// effective capacity is the smaller of the two (DESIGN §A5.5, SPEC RCP-9); release frees one
// of each and never takes a count below 0; rl_slots_in_use follows.
func TestSlotsPairedTry(t *testing.T) {
	for _, c := range []struct{ receipts, refunds, want int }{
		{3, 5, 3}, {5, 2, 2}, {4, 4, 4}, {1, 1, 1}, {0, 4, 0}, {4, 0, 0}, {-1, 4, 0},
	} {
		reg := metrics.NewRegistry()
		s := newSlots(c.receipts, c.refunds, reg)
		n := 0
		for n <= 10 && s.tryAcquire() {
			n++
		}
		r, f := s.inUse()
		snap := reg.Snapshot()
		gr, gf := gaugeValue(t, snap, "rl_slots_in_use", "receipt"), gaugeValue(t, snap, "rl_slots_in_use", "refund")
		if n != c.want || r != c.want || f != c.want || gr != float64(c.want) || gf != float64(c.want) {
			t.Fatalf("capacities %d and %d: reserved %d, in use %d and %d, gauges %v and %v; want %d each",
				c.receipts, c.refunds, n, r, f, gr, gf, c.want)
		}
		if c.want == 0 {
			continue
		}
		s.release()
		if r, f := s.inUse(); r != c.want-1 || f != c.want-1 {
			t.Fatalf("capacities %d and %d: after one release %d and %d in use, want %d each", c.receipts, c.refunds, r, f, c.want-1)
		}
		again := s.tryAcquire()
		if more := s.tryAcquire(); !again || more {
			t.Fatalf("capacities %d and %d: a freed pair is not reserved exactly once more", c.receipts, c.refunds)
		}
	}
	s := newSlots(1, 1, metrics.NewRegistry())
	s.release()
	r, f := s.inUse()
	first := s.tryAcquire()
	if second := s.tryAcquire(); r != 0 || f != 0 || !first || second {
		t.Fatalf("a release with nothing reserved: %d and %d in use, or the capacity changed", r, f)
	}
}
