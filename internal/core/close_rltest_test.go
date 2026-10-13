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

// TestCloseAndKill: Close (nothing pending to drain) and Kill each stop the epoch watch and close
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
// reaches the real adapter only after the shutdown began closing it, so nothing is sent. The
// store was never found unavailable; the instance's own shutdown failed the call: the result
// is not_ready (closing) in both modes, never degraded or unavailable, with no permit and both
// slots free. TestAcquireUnavailableDuringDrain covers a store refusal before that. The
// control: the same limiter, not closing, whose acquire is refused before the send (the
// refuse fault) is still degraded in Open and unavailable in Closed (API-20a, API-20b).
func TestCloseDuringSend(t *testing.T) {
	const u = 19_000_000_000 // an in-range E_ALLOWED u, which no acquire here may reach
	for _, c := range []struct {
		name string
		mode FailureMode
		stop func(*Limiter)
	}{
		{"Open, Close", Open, func(l *Limiter) { l.Close() }},
		{"Open, Kill", Open, Kill},
		{"Closed, Close", Closed, func(l *Limiter) { l.Close() }},
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

// drainGate is a store wrapper around the real adapter for TestAcquireUnavailableDuringDrain:
// it passes the first acquire through, answers every refund unavailable (nothing sent), so a
// released permit's refund stays pending, and holds the second acquire at a before_send
// barrier until the test sends its response.
type drainGate struct {
	store.Adapter
	acquires atomic.Int32
	reached  chan struct{}       // closed when the second acquire waits at the barrier
	answer   chan store.Response // the held acquire's response
}

func newDrainGate() *drainGate {
	return &drainGate{reached: make(chan struct{}), answer: make(chan store.Response, 1)}
}

// wrap is the store wrapper hook.
func (g *drainGate) wrap(inner store.Adapter) store.Adapter {
	g.Adapter = inner
	return g
}

func (g *drainGate) Call(ctx context.Context, req store.Request) store.Response {
	switch req.Script.Name {
	case "refund":
		return store.Response{Outcome: store.Unavailable}
	case "acquire":
		if g.acquires.Add(1) == 2 {
			close(g.reached)
			select {
			case r := <-g.answer:
				return r
			case <-ctx.Done():
				return store.Response{Outcome: store.Cancelled}
			}
		}
	}
	return g.Adapter.Call(ctx, req)
}

// TestAcquireUnavailableDuringDrain (SPEC API-8a, API-20a, API-20b, API-21, API-1e): an acquire
// that passed the local checks before Close began, and that the store refuses with nothing sent
// while Close drains, takes the failure-mode branch like any unavailable acquire: degraded with
// a permit and no deduction_id in Open, unavailable in Closed, with the store's error in Diag.
// Closing refuses new acquires only (API-8a), and the adapter stays open through the drain;
// only the shutdown after it fails calls itself (TestCloseDuringSend). The store wrapper
// answers every refund unavailable, so the first permit's refund stays pending and
// Close(WithDrainTimeout(1.5 s)) drains until its deadline; it holds the second acquire at its
// before_send barrier until Health reads closing, then answers it unavailable. Close has not
// returned when that acquire does; once it has, both slots are free and the store ran only the
// first acquire.
func TestAcquireUnavailableDuringDrain(t *testing.T) {
	const (
		u        = 19_000_000_000 // the first acquire's in-range E_ALLOWED u
		drain    = 1500 * time.Millisecond
		storeErr = "dial tcp: connect: connection refused"
	)
	for _, c := range []struct {
		mode  FailureMode
		kind  ResultKind
		label string
	}{{Open, Degraded, "degraded"}, {Closed, Unavailable, "unavailable"}} {
		t.Run(c.mode.String(), func(t *testing.T) {
			g := newDrainGate()
			l, calls := wireLimiterWith(t, c.mode, u, Hooks{StoreWrapper: g.wrap})
			first := l.Acquire(t.Context(), "k")
			if first.Kind != Allowed {
				t.Fatalf("first acquire: %v %+v, want allowed", first.Kind, first.Diag)
			}
			first.Permit.Release() // refused at the store: pending until the drain's end
			before := l.Metrics()
			done := make(chan Result, 1)
			go func() { done <- l.Acquire(t.Context(), "k") }()
			select {
			case <-g.reached:
			case <-time.After(waitMax):
				t.Fatalf("the second acquire did not reach the before_send barrier within %v", waitMax)
			}
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				l.Close(WithDrainTimeout(drain))
			}()
			waitReason(t, l, reasonClosing)
			g.answer <- store.Response{Outcome: store.Unavailable, StoreError: storeErr}
			var r Result
			select {
			case r = <-done:
			case <-time.After(waitMax):
				t.Fatalf("the second acquire did not return within %v", waitMax)
			}
			select {
			case <-closed:
				t.Errorf("Close returned before the held acquire did: want the acquire to end during the %v drain", drain)
			default:
			}
			if r.Kind != c.kind || (r.Permit != nil) != (c.kind == Degraded) || r.Diag != (Diag{StoreError: storeErr}) || r.DecisionNs != 0 || r.RetryAfterNs != 0 {
				t.Errorf("acquire refused during the drain: %v %+v (permit %v); want %v with the store's error, a permit only when degraded", r.Kind, r.Diag, r.Permit != nil, c.kind)
			}
			if r.Permit != nil {
				if id := r.Permit.DeductionID(); id != "" {
					t.Errorf("degraded permit has deduction_id %q, want none", id)
				}
				r.Permit.Done() // a no-op for a DEGRADED permit
			}
			if label := risenLabel(t, before, l.Metrics()); label != c.label {
				t.Errorf("counted as %q, want %s", label, c.label)
			}
			select {
			case <-closed:
			case <-time.After(waitMax):
				t.Fatalf("Close did not return within %v", waitMax)
			}
			if got := slotsOf(l); got != (slotCounts{0, 0}) {
				t.Errorf("slots in use %+v after Close, want 0 and 0", got)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("the store ran %d acquires, want 1 (the first)", n)
			}
		})
	}
}

// killGate is a store wrapper around the real adapter for TestAcquireUnavailableDuringKill: it
// passes the first acquire through and holds the second at a before_send barrier until the test
// sends its response. It holds the first refund attempt until the test lets it go, and reports
// when that attempt's context ends: Kill's refund stop has cancelled it and waits for the
// worker. Any later refund is answered unavailable (nothing sent).
type killGate struct {
	store.Adapter
	acquires  atomic.Int32
	refunds   atomic.Int32
	reached   chan struct{}       // closed when the second acquire waits at the barrier
	answer    chan store.Response // the held acquire's response
	refunding chan struct{}       // closed when the first refund attempt waits
	cancelled chan struct{}       // closed once that attempt's context has ended
	letGo     chan struct{}       // closed by let: the held refund attempt returns
	letOnce   sync.Once
}

func newKillGate() *killGate {
	return &killGate{
		reached: make(chan struct{}), answer: make(chan store.Response, 1),
		refunding: make(chan struct{}), cancelled: make(chan struct{}), letGo: make(chan struct{}),
	}
}

// wrap is the store wrapper hook.
func (g *killGate) wrap(inner store.Adapter) store.Adapter {
	g.Adapter = inner
	return g
}

// let ends the held refund attempt; a later call does nothing.
func (g *killGate) let() { g.letOnce.Do(func() { close(g.letGo) }) }

func (g *killGate) Call(ctx context.Context, req store.Request) store.Response {
	switch req.Script.Name {
	case "refund":
		if g.refunds.Add(1) != 1 {
			return store.Response{Outcome: store.Unavailable}
		}
		close(g.refunding)
		select {
		case <-ctx.Done():
			close(g.cancelled)
		case <-g.letGo:
			return store.Response{Outcome: store.Unavailable}
		}
		<-g.letGo
		return store.Response{Outcome: store.Cancelled}
	case "acquire":
		if g.acquires.Add(1) == 2 {
			close(g.reached)
			select {
			case r := <-g.answer:
				return r
			case <-ctx.Done():
				return store.Response{Outcome: store.Cancelled}
			}
		}
	}
	return g.Adapter.Call(ctx, req)
}

// TestAcquireUnavailableDuringKill (SPEC API-21, API-1e; DESIGN §A5.12): Kill is a shutdown
// from its first step. An acquire that passed the local checks before Kill began, and that comes
// back unavailable (nothing sent) while Kill is still in its first steps, reads not_ready
// (closing) in both modes, with no permit, never degraded or unavailable. The store wrapper
// holds the first permit's refund attempt, so Kill's refund stop, which cancels that attempt
// and waits for its worker, holds Kill before its shutdown: the wrapper reports the cancel, and
// only then does the test answer the second acquire, held at its before_send barrier,
// unavailable with a store error. Kill has not returned when that acquire does; it returns once
// the test lets the refund attempt go. The store ran only the first acquire.
func TestAcquireUnavailableDuringKill(t *testing.T) {
	const (
		u        = 19_000_000_000 // the first acquire's in-range E_ALLOWED u
		storeErr = "dial tcp: connect: connection refused"
	)
	for _, mode := range []FailureMode{Open, Closed} {
		t.Run(mode.String(), func(t *testing.T) {
			g := newKillGate()
			l, calls := wireLimiterWith(t, mode, u, Hooks{StoreWrapper: g.wrap})
			t.Cleanup(g.let) // registered after the limiter's Close, so it runs first
			timeout := time.NewTimer(waitMax)
			defer timeout.Stop()
			wait := func(ch <-chan struct{}, what string) {
				t.Helper()
				select {
				case <-ch:
				case <-timeout.C:
					t.Fatalf("%s within %v", what, waitMax)
				}
			}
			first := l.Acquire(t.Context(), "k")
			if first.Kind != Allowed {
				t.Fatalf("first acquire: %v %+v, want allowed", first.Kind, first.Diag)
			}
			first.Permit.Release() // its refund attempt waits in the store wrapper
			wait(g.refunding, "no refund attempt reached the store wrapper")
			before := l.Metrics()
			done := make(chan Result, 1)
			go func() { done <- l.Acquire(t.Context(), "k") }()
			wait(g.reached, "the second acquire did not reach the before_send barrier")
			killed := make(chan struct{})
			go func() {
				defer close(killed)
				Kill(l)
			}()
			wait(g.cancelled, "Kill did not cancel the held refund attempt")
			g.answer <- store.Response{Outcome: store.Unavailable, StoreError: storeErr}
			var r Result
			select {
			case r = <-done:
			case <-timeout.C:
				t.Fatalf("the second acquire did not return within %v", waitMax)
			}
			select {
			case <-killed:
				t.Errorf("Kill returned while its refund stop still waited for the held attempt")
			default:
			}
			if r.Kind != NotReady || r.Diag != (Diag{Reason: reasonClosing}) || r.Permit != nil || r.DecisionNs != 0 || r.RetryAfterNs != 0 {
				t.Errorf("acquire refused during Kill's refund stop: %v %+v (permit %v); want not_ready (closing), no permit", r.Kind, r.Diag, r.Permit != nil)
			}
			if label := risenLabel(t, before, l.Metrics()); label != "not_ready" {
				t.Errorf("counted as %q, want not_ready", label)
			}
			if got := slotsOf(l); got != (slotCounts{1, 1}) {
				t.Errorf("slots in use %+v after the second acquire returned, want 1 and 1 (the first permit's only)", got)
			}
			g.let()
			wait(killed, "Kill did not return")
			if n := calls.Load(); n != 1 {
				t.Errorf("the store ran %d acquires, want 1 (the first)", n)
			}
		})
	}
}
