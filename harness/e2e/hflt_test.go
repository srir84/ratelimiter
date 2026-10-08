//go:build rltest

package e2e

import (
	"net/http"
	"testing"
	"time"

	"ratelimiter/harness/config"
)

// TestT_HFLT_1 (HARNESS §8): with max_in_flight = 2 and two /hit requests blocked on keys
// held by open uncommitted inserts, a third gets 503 service_overloaded at once with no
// acquire sent (the acquire total is unchanged by it) and writes nothing (HAR-14, ERR-7,
// HOST-7); once the inserts roll back, the two finish with 200.
func TestT_HFLT_1(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.API.MaxInFlight = 2 }})
	api := s.APIs[0]
	rollbackA := s.holdKey(t, "fa", "r1")
	rollbackB := s.holdKey(t, "fb", "r1")
	heldA := api.Go(t, http.MethodGet, "/hit", IDs("fa", "r1"), "")
	heldB := api.Go(t, http.MethodGet, "/hit", IDs("fb", "r1"), "")
	s.waitLockWaiters(t, 2)

	if h := api.Health(t); Int(t, h, "in_flight") != 2 {
		t.Errorf("/health in_flight %v with both requests held, want 2", h["in_flight"])
	}
	before := api.AcquireTotal(t)
	if before != 2 {
		t.Errorf("acquire total %d with both requests held, want 2", before)
	}
	third := api.Hit(t, "fc", "r1")
	wantError(t, "the third /hit", third, http.StatusServiceUnavailable, "service_overloaded")
	if took := third.After.Sub(third.Before); took > time.Second {
		t.Errorf("the third /hit answered after %v, want at once: there is no queue", took)
	}
	if after := api.AcquireTotal(t); after != before {
		t.Errorf("acquire total went from %d to %d over the refused /hit, want unchanged: no acquire sent", before, after)
	}
	if n := s.keyRows(t, "fc", "r1"); n != 0 {
		t.Errorf("rows for (fc, r1) = %d, want 0", n)
	}

	for _, rb := range []func() error{rollbackA, rollbackB} {
		if err := rb(); err != nil {
			t.Fatalf("rolling back a held key: %v", err)
		}
	}
	wantCreated(t, "the first held /hit", Wait(t, heldA, 10*time.Second, "the first held /hit"), "fa", "r1")
	wantCreated(t, "the second held /hit", Wait(t, heldB, 10*time.Second, "the second held /hit"), "fb", "r1")
	if n := s.rows(t); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}
