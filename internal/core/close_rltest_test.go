//go:build rltest

package core

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// TestCloseAndKill: Close (S5: no drain yet) and Kill each stop the epoch watch and close
// every connection before they return, Kill within 100 ms; afterwards Acquire returns
// not_ready and Health reports closing, and a second call returns at once (DESIGN §A5.12,
// plan S5 steps 3 and 8).
func TestCloseAndKill(t *testing.T) {
	for _, kill := range []bool{false, true} {
		ts := newTestStore(t)
		p := testPolicy(Closed)
		fp := fingerprint.Of(p)
		reads := &atomic.Int32{}
		ts.srv.Load(scripts.ReadEpoch.Text)
		ts.srv.Handle(scripts.ReadEpoch.SHA, func(_, _ []string) resp.Value {
			reads.Add(1)
			return resp.Value{Kind: resp.Array, Elems: []resp.Value{intVal(0), intVal(0), strVal(""), strVal(epochA), strVal(fp),
				intVal(tNow), intVal(1), intVal(tNow), intVal(0), intVal(0), intVal(0)}}
		})
		in := testInstance(p)
		in.EpochRereadIntervalNs = 20_000_000 // 20 ms
		l := ts.ready(t, p, in, nil, Hooks{}, reasonReady)
		// Each precondition gets its own bounded wait: under load, dials and reads need not
		// all complete within one window.
		waitFor(t, "two epoch reads", func() bool { return reads.Load() >= 2 })
		for _, c := range []struct {
			kind store.Kind
			n    int
		}{{store.Call, 8}, {store.Refund, 2}, {store.Maintenance, 1}} {
			waitFor(t, fmt.Sprintf("%d ready %v connections", c.n, c.kind), func() bool { return l.adapter.ReadyConns(c.kind) == c.n })
		}
		start := time.Now()
		if kill {
			Kill(l)
			if d := time.Since(start); d > 100*time.Millisecond {
				t.Errorf("Kill took %v, want at most 100 ms", d)
			}
		} else {
			l.Close()
		}
		after := reads.Load()
		for _, k := range []store.Kind{store.Call, store.Refund, store.Maintenance} {
			if n := l.adapter.ReadyConns(k); n != 0 {
				t.Errorf("kill %v: %d ready %v connections after it returned, want 0", kill, n, k)
			}
		}
		time.Sleep(5 * 20 * time.Millisecond) // 5 re-read intervals
		if n := reads.Load(); n != after {
			t.Errorf("kill %v: %d epoch reads after it returned, want none", kill, n-after)
		}
		if r := l.Acquire(t.Context(), "k"); r.Kind != NotReady || r.Diag.Reason != reasonClosing {
			t.Errorf("kill %v: Acquire = %v %+v, want not_ready (closing)", kill, r.Kind, r.Diag)
		}
		if h := l.Health(); h.Status != statusNotReady || h.Reason != reasonClosing || h.EpochID != epochA {
			t.Errorf("kill %v: health %+v, want not_ready, closing, epoch %s", kill, h, epochA)
		}
		again := time.Now()
		l.Close()
		Kill(l)
		if d := time.Since(again); d > 100*time.Millisecond {
			t.Errorf("kill %v: a second Close and Kill took %v", kill, d)
		}
	}
}

// gate is a store wrapper around the real adapter for the tests below, with DESIGN §B6's
// refuse fault and before_send barrier (core's tests do not import the harness). With refuse
// set, every acquire is answered unavailable and never reaches the adapter (nothing sent).
// Otherwise every acquire waits before its send until Close releases it, which then closes
// the adapter, in fixtures.StoreWrapper.Close's order. Every other call passes through.
type gate struct {
	store.Adapter
	refuse    bool
	reached   chan struct{} // closed when the first acquire waits at the barrier
	release   chan struct{} // closed by Close
	arrive    sync.Once
	closeOnce sync.Once
}

func newGate(refuse bool) *gate {
	return &gate{refuse: refuse, reached: make(chan struct{}), release: make(chan struct{})}
}

// wrap is the store wrapper hook.
func (g *gate) wrap(inner store.Adapter) store.Adapter {
	g.Adapter = inner
	return g
}

func (g *gate) Call(ctx context.Context, req store.Request) store.Response {
	if req.Script.Name != "acquire" {
		return g.Adapter.Call(ctx, req)
	}
	if g.refuse {
		return store.Response{Outcome: store.Unavailable}
	}
	g.arrive.Do(func() { close(g.reached) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return g.Adapter.Call(ctx, req)
}

// Close releases the barrier, then closes the adapter.
func (g *gate) Close() {
	g.closeOnce.Do(func() { close(g.release) })
	g.Adapter.Close()
}

// TestCloseDuringSend (SPEC API-21, API-1e, API-8a; plan S5 steps 3 and 8): an acquire that
// passed the local checks and waits at a before_send barrier when Close or Kill begins
// reaches the real adapter only after the shutdown closed it, so nothing is sent. The store
// was never found unavailable and the instance is closing: the result is not_ready (closing)
// in both modes, never degraded or unavailable, with no permit and both slots free. The
// control: the same limiter, not closing, whose acquire is refused before the send (the
// refuse fault) is still degraded in Open and unavailable in Closed (API-20a, API-20b).
func TestCloseDuringSend(t *testing.T) {
	const u = 19_000_000_000 // an in-range E_ALLOWED u, which no acquire here may reach
	for _, c := range []struct {
		name string
		mode FailureMode
		stop func(*Limiter)
	}{
		{"Open, Close", Open, (*Limiter).Close},
		{"Open, Kill", Open, Kill},
		{"Closed, Close", Closed, (*Limiter).Close},
		{"Closed, Kill", Closed, Kill},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newGate(false)
			l, calls := wireLimiterWith(t, c.mode, u, Hooks{StoreWrapper: g.wrap})
			before := l.Metrics()
			timeout := time.NewTimer(waitMax) // started before the acquire, so the bound covers all of it
			defer timeout.Stop()
			done := make(chan Result, 1)
			go func() { done <- l.Acquire(t.Context(), "k") }()
			select {
			case <-g.reached:
			case <-timeout.C:
				t.Fatalf("the acquire did not reach the before_send barrier within %v", waitMax)
			}
			c.stop(l) // returns once the adapter is closed; its Close released the barrier
			var r Result
			select {
			case r = <-done:
			case <-timeout.C:
				t.Fatalf("the acquire did not return within %v", waitMax)
			}
			if r.Kind != NotReady || r.Diag != (Diag{Reason: "closing"}) || r.Permit != nil || r.DecisionNs != 0 || r.RetryAfterNs != 0 {
				t.Errorf("acquire held at the barrier: %v %+v (permit %v); want not_ready (closing), no permit", r.Kind, r.Diag, r.Permit != nil)
			}
			if got := slotsOf(l); got != (slotCounts{0, 0}) {
				t.Errorf("slots in use %+v after the acquire returned, want 0 and 0", got)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("the store ran %d acquires, want 0 (nothing sent)", n)
			}
			if label := risenLabel(t, before, l.Metrics()); label != "not_ready" {
				t.Errorf("counted as %q, want not_ready", label)
			}
		})
	}
	for _, c := range []struct {
		mode  FailureMode
		kind  ResultKind
		label string
	}{{Open, Degraded, "degraded"}, {Closed, Unavailable, "unavailable"}} {
		t.Run(fmt.Sprintf("%v, refuse", c.mode), func(t *testing.T) {
			l, calls := wireLimiterWith(t, c.mode, u, Hooks{StoreWrapper: newGate(true).wrap})
			before := l.Metrics()
			r := l.Acquire(t.Context(), "k")
			if r.Kind != c.kind || (r.Permit != nil) != (c.kind == Degraded) || r.Diag != (Diag{}) || r.DecisionNs != 0 || r.RetryAfterNs != 0 {
				t.Fatalf("refused acquire: %v %+v (permit %v); want %v, permit only when degraded", r.Kind, r.Diag, r.Permit != nil, c.kind)
			}
			if r.Permit != nil {
				if id := r.Permit.DeductionID(); id != "" {
					t.Errorf("degraded permit has deduction_id %q, want none", id)
				}
				r.Permit.Done() // a no-op for a DEGRADED permit
			}
			if got := slotsOf(l); got != (slotCounts{0, 0}) {
				t.Errorf("slots in use %+v after the acquire returned, want 0 and 0", got)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("the store ran %d acquires, want 0 (refused before the send)", n)
			}
			if label := risenLabel(t, before, l.Metrics()); label != c.label {
				t.Errorf("counted as %q, want %s", label, c.label)
			}
			if h := l.Health(); h.Reason != "ready" {
				t.Errorf("health reason %q, want ready: the instance is not closing", h.Reason)
			}
		})
	}
}
