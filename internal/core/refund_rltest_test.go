//go:build rltest

package core

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
)

// refundInstance is testInstance with a 1 ms → 4 ms refund backoff, so retries come quickly
// (§A5.6 inequality: 4 ms + 1 s + (ceil(4 / 128) + 3) × 2 s ≤ 30 s), and a 2 s drain, so a
// cleanup Close with a refund still pending ends soon.
func refundInstance(p Policy) Instance {
	in := testInstance(p)
	in.RefundBackoffInitialNs, in.RefundBackoffMaxNs = 1_000_000, 4_000_000
	in.DrainTimeoutNs = 2_000_000_000
	return in
}

// eventLog is a refund listener that keeps every event, in order.
type eventLog struct {
	mu  sync.Mutex
	evs []RefundEvent
}

func (e *eventLog) listen(ev RefundEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evs = append(e.evs, ev)
}

func (e *eventLog) events() []RefundEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.evs)
}

// waitEvents waits, at most waitMax, until log holds n events, and returns them.
func waitEvents(t *testing.T, log *eventLog, n int) []RefundEvent {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d refund event(s)", n), func() bool { return len(log.events()) >= n })
	return log.events()
}

// sequence is an acquire responder that allows with generation 1 and sequences 1, 2, 3, ….
func sequence() func(store.Request) store.Response {
	var s atomic.Int64
	return func(store.Request) store.Response { return allowedResp(1, s.Add(1)) }
}

// replies is a refund responder that answers rs in order, then the last one again.
func replies(rs ...store.Response) func(store.Request) store.Response {
	var i atomic.Int64
	return func(store.Request) store.Response {
		n := i.Add(1) - 1
		return rs[min(n, int64(len(rs)-1))]
	}
}

// appliedAt is an E_APPLIED refund response at budget time t, crediting one token.
func appliedAt(t int64) store.Response { return okResp(3, t, "", int64(1_000_000_000)) }

// refundLimiter builds a ready limiter of mode on a fake store whose acquire allows (sequences
// 1, 2, …) and whose refunds answer refund, with refundInstance's settings changed by edit
// and log as the refund listener.
func (s *testStore) refundLimiter(t *testing.T, mode FailureMode, edit func(*Instance), refund func(store.Request) store.Response, log *eventLog) (*Limiter, *fakeStore) {
	t.Helper()
	p := testPolicy(mode)
	in := refundInstance(p)
	if edit != nil {
		edit(&in)
	}
	fake := newFake(joined(p), sequence())
	fake.setRefund(refund)
	return s.ready(t, p, in, fake, Hooks{}, reasonReady, WithRefundListener(log.listen)), fake
}

// acquireOK acquires "k" and fails the test unless it is allowed.
func acquireOK(t *testing.T, l *Limiter) *Permit {
	t.Helper()
	r := l.Acquire(t.Context(), "k")
	if r.Kind != Allowed || r.Permit == nil {
		t.Fatalf("Acquire = %v %+v, want allowed with a permit", r.Kind, r.Diag)
	}
	return r.Permit
}

// counter returns the value of a counter or gauge series in l's metrics.
func counter(t *testing.T, l *Limiter, name, label string) float64 {
	t.Helper()
	return gaugeValue(t, l.Metrics(), name, label)
}

// verifyFails is a signer whose Verify rejects every receipt (a corrupted receipt).
type verifyFails struct{ receipt.Signer }

func (verifyFails) Verify(receipt.Receipt) bool { return false }

// TestRefundAttemptsCountOnlySent (plan S6, L1; SPEC API-1d; DESIGN §A5.4 "sent", §A5.6): a
// refund's Attempts counts only the refund commands that were sent: 3 unavailable attempts
// (nothing sent) then applied → 1; 2 outcome_unknown attempts (sent) then applied → 3; a
// NOSCRIPT reply, the reload and the resend that applies, through the real adapter → 2; a
// receipt whose MAC does not verify → receipt_invalid with 0 attempts and no store call. Every
// refund request goes to the refund connections with the §A4.1 KEYS and ARGV.
func TestRefundAttemptsCountOnlySent(t *testing.T) {
	s := newTestStore(t)
	const applyAt = tNow + 1_000
	unavailable := store.Response{Outcome: store.Unavailable}
	unknownSent := store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR lost"}
	for _, c := range []struct {
		name  string
		first []store.Response
		want  int
	}{
		{"3 unavailable, then applied", []store.Response{unavailable, unavailable, unavailable}, 1},
		{"2 outcome_unknown, then applied", []store.Response{unknownSent, unknownSent}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &eventLog{}
			l, fake := s.refundLimiter(t, Closed, nil, replies(append(c.first, appliedAt(applyAt))...), log)
			acquireOK(t, l).Release()
			ev := waitEvents(t, log, 1)[0]
			if ev.Result != Applied || ev.Attempts != c.want || ev.CreditDecisionNs != applyAt*1000 || ev.Reason != 0 {
				t.Errorf("event %+v, want applied, %d attempts, credit %d", ev, c.want, int64(applyAt*1000))
			}
			reqs := fake.requests("refund")
			if len(reqs) != len(c.first)+1 {
				t.Fatalf("%d refund calls, want %d", len(reqs), len(c.first)+1)
			}
			p := testPolicy(Closed)
			wantKeys := []string{"rl:epoch", "rl:ctl:time", "rl:e:" + epochA + ":ix", "rl:e:" + epochA + ":q",
				"rl:e:" + epochA + ":k:aw", "rl:e:" + epochA + ":mi:aw"}
			// The receipt: epoch epochA, enc "aw" (key "k"), g 1, s 1, expiry tNow + 60 s; the
			// default policy; floors hwm tNow and gen 1 from the joined read.
			wantArgs := []string{epochA, fingerprint.Of(p), "aw", epochA, "1", "1", "1760000060000000",
				"20000000000", "1000000000", "10", "50000", "1000", "1760000000000000", "1"}
			for i, r := range reqs {
				if r.Kind != store.Refund || r.Script != scripts.Refund || !slices.Equal(r.Keys, wantKeys) ||
					!slices.Equal(r.Args, wantArgs) || r.ReceiptExpiryUs != tNow+lifeUs {
					t.Errorf("refund request %d: kind %v, script %s, keys %q, args %q, receipt expiry %d", i, r.Kind, r.Script.Name, r.Keys, r.Args, r.ReceiptExpiryUs)
				}
			}
		})
	}

	t.Run("NOSCRIPT, reload and resend, through the real adapter", func(t *testing.T) {
		ts := newTestStore(t)
		p := testPolicy(Closed)
		fp := fingerprint.Of(p)
		arr := func(vs ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: vs} }
		ts.srv.Load(scripts.ReadEpoch.Text)
		ts.srv.Handle(scripts.ReadEpoch.SHA, func(_, _ []string) resp.Value {
			return arr(intVal(0), intVal(0), strVal(""), strVal(epochA), strVal(fp), intVal(tNow), intVal(1),
				intVal(tNow), intVal(0), intVal(0), intVal(0))
		})
		ts.srv.Load(scripts.Acquire.Text)
		ts.srv.Handle(scripts.Acquire.SHA, func(_, _ []string) resp.Value {
			return arr(intVal(1), intVal(tNow), strVal(""), intVal(19_000_000_000), intVal(1), intVal(1), intVal(tNow+lifeUs))
		})
		// Not loaded: the first EVALSHA refund gets NOSCRIPT.
		ts.srv.Handle(scripts.Refund.SHA, func(_, _ []string) resp.Value {
			return arr(intVal(3), intVal(applyAt), strVal(""), intVal(1_000_000_000))
		})
		in := refundInstance(p)
		in.EpochRereadIntervalNs = wireInterval
		log := &eventLog{}
		l := ts.ready(t, p, in, nil, Hooks{}, reasonReady, WithRefundListener(log.listen))
		acquireOK(t, l).Release()
		ev := waitEvents(t, log, 1)[0]
		if ev.Result != Applied || ev.Attempts != 2 || ev.CreditDecisionNs != applyAt*1000 {
			t.Errorf("event %+v, want applied with 2 attempts (the EVALSHA and its resend)", ev)
		}
		if n := ts.srv.Count("SCRIPT LOAD"); n != 1 {
			t.Errorf("%d SCRIPT LOAD, want 1", n)
		}
		// One Call, its reload and resend included: one rl_store_call_seconds{script="refund"}
		// observation; the two refund connections are counted as ready.
		calls := uint64(0)
		for _, f := range l.Metrics().Families {
			if f.Name == "rl_store_call_seconds" {
				for _, sr := range f.Series {
					if sr.LabelValue == "refund" {
						calls = sr.Count
					}
				}
			}
		}
		if calls != 1 {
			t.Errorf("rl_store_call_seconds{script=\"refund\"} counted %d calls, want 1", calls)
		}
		waitFor(t, "2 ready refund connections", func() bool { return counter(t, l, "rl_store_connections", "refund") == 2 })
	})

	t.Run("a receipt that fails Verify", func(t *testing.T) {
		p := testPolicy(Closed)
		fake := newFake(joined(p), sequence())
		fake.setRefund(replies(appliedAt(applyAt)))
		log := &eventLog{}
		l := s.ready(t, p, refundInstance(p), fake, Hooks{SignerWrapper: func(in receipt.Signer) receipt.Signer { return verifyFails{in} }},
			reasonReady, WithRefundListener(log.listen))
		acquireOK(t, l).Release()
		ev := waitEvents(t, log, 1)[0]
		if ev.Result != ReceiptInvalid || ev.Attempts != 0 || ev.CreditDecisionNs != 0 {
			t.Errorf("event %+v, want receipt_invalid, 0 attempts, no credit time", ev)
		}
		if n := fake.count("refund"); n != 0 {
			t.Errorf("%d refund calls, want 0 (API-2a step 1 fails before the store)", n)
		}
		if got := slotsOf(l); got != (slotCounts{0, 0}) {
			t.Errorf("slots in use %+v after the final event, want 0 and 0", got)
		}
	})
}

// TestBackoffDoesNotHoldWorker (plan S6, L1; DESIGN §A5.6, §A7 step 2): with one refund worker
// and a 0.5–1 s backoff, job A's first attempt is refused (unavailable) and job B, released
// after A, is applied during A's backoff, before A's retry: the refund calls are A, B, A.
func TestBackoffDoesNotHoldWorker(t *testing.T) {
	s := newTestStore(t)
	var mu sync.Mutex
	var calls []string
	refund := func(req store.Request) store.Response {
		mu.Lock()
		defer mu.Unlock()
		job := "B"
		if req.Args[5] == "1" { // the receipt's sequence: A holds 1, B 2
			job = "A"
		}
		calls = append(calls, job)
		if job == "A" && len(calls) == 1 {
			return store.Response{Outcome: store.Unavailable}
		}
		return appliedAt(tNow + 1_000)
	}
	log := &eventLog{}
	l, _ := s.refundLimiter(t, Closed, func(in *Instance) {
		in.RefundWorkers = 1
		in.RefundBackoffInitialNs, in.RefundBackoffMaxNs = 1_000_000_000, 1_000_000_000
	}, refund, log)
	a, b := acquireOK(t, l), acquireOK(t, l)
	a.Release()
	b.Release()
	evs := waitEvents(t, log, 2)
	mu.Lock()
	got := slices.Clone(calls)
	mu.Unlock()
	if !slices.Equal(got, []string{"A", "B", "A"}) {
		t.Fatalf("refund calls %v, want [A B A]: B must run while A waits out its backoff", got)
	}
	if evs[0].DeductionID != b.DeductionID() || evs[1].DeductionID != a.DeductionID() {
		t.Errorf("events for %s then %s, want B (%s) then A (%s)", evs[0].DeductionID, evs[1].DeductionID, b.DeductionID(), a.DeductionID())
	}
	// B ended less than A's shortest backoff (500 ms) after A was released.
	if d := time.Duration(evs[0].FinalMonoNs - evs[1].ReleaseMonoNs); d >= 500*time.Millisecond {
		t.Errorf("B ended %v after A's release, want < 500 ms", d)
	}
	if evs[1].Attempts != 1 || evs[0].Attempts != 1 {
		t.Errorf("attempts A %d, B %d; want 1 each (the refused attempt sent nothing)", evs[1].Attempts, evs[0].Attempts)
	}
}

// TestListenerPanicAndDrops (plan S6, L1; DESIGN §A5.11; SPEC API-1d, HOST-8): a listener that
// panics on every event gets every event and the dispatcher survives; with a queue of 4 and a
// blocked listener, 10 events → the one in the callback, 4 queued, 5 dropped, and once it is
// unblocked, delivered + dropped = 10, with rl_listener_drops_total equal to the drops; events
// still queued when the listener stops, and every event after, are dropped and counted, and so
// is an event the dispatcher had taken, before waitExit returns. In a limiter, a panicking
// listener changes no refund result, slot or count.
func TestListenerPanicAndDrops(t *testing.T) {
	deadline := func() time.Time { return time.Now().Add(waitMax) }
	t.Run("panics", func(t *testing.T) {
		reg := metrics.NewRegistry()
		var calls atomic.Int32
		s := newListener(func(RefundEvent) {
			calls.Add(1)
			panic("host listener")
		}, 16, reg)
		s.start()
		for i := range 10 {
			s.emit(RefundEvent{DeductionID: fmt.Sprint("d1_", i)})
		}
		s.waitIdle(deadline())
		if n := calls.Load(); n != 10 || s.drops.Load() != 0 {
			t.Errorf("%d of 10 events delivered, %d dropped; want 10 and 0", n, s.drops.Load())
		}
		s.stop()
		s.waitExit(deadline())
	})

	t.Run("blocked, queue 4", func(t *testing.T) {
		reg := metrics.NewRegistry()
		unblock := make(chan struct{})
		var delivered atomic.Int32
		s := newListener(func(RefundEvent) {
			delivered.Add(1)
			<-unblock
		}, 4, reg)
		s.start()
		t.Cleanup(func() {
			s.stop()
			s.waitExit(deadline())
		})
		s.emit(RefundEvent{DeductionID: "d1_0"})
		waitFor(t, "the first event in the callback", func() bool { return delivered.Load() == 1 })
		for i := 1; i < 10; i++ {
			s.emit(RefundEvent{DeductionID: fmt.Sprint("d1_", i)})
		}
		if d := s.drops.Load(); d != 5 {
			t.Errorf("%d drops with one event in the callback and 4 queued, want 5", d)
		}
		close(unblock)
		start := time.Now()
		s.waitIdle(deadline())
		if w := time.Since(start); w > time.Second {
			t.Errorf("waitIdle returned %v after the listener was unblocked, want once the queue emptied (< 1 s)", w)
		}
		d, n := s.drops.Load(), delivered.Load()
		if n != 5 || int(d)+int(n) != 10 {
			t.Errorf("delivered %d, dropped %d; want 5 and 5, 10 in all", n, d)
		}
		if m := gaugeValue(t, reg.Snapshot(), "rl_listener_drops_total", ""); m != float64(d) {
			t.Errorf("rl_listener_drops_total %v, want %d", m, d)
		}
	})

	t.Run("stopped with events queued", func(t *testing.T) {
		reg := metrics.NewRegistry()
		unblock := make(chan struct{})
		var delivered atomic.Int32
		s := newListener(func(RefundEvent) {
			delivered.Add(1)
			<-unblock
		}, 4, reg)
		s.start()
		s.emit(RefundEvent{DeductionID: "d1_0"})
		waitFor(t, "the first event in the callback", func() bool { return delivered.Load() == 1 })
		for i := 1; i <= 3; i++ {
			s.emit(RefundEvent{DeductionID: fmt.Sprint("d1_", i)})
		}
		s.stop() // the callback is still blocked: stop does not wait for it
		s.emit(RefundEvent{DeductionID: "d1_late"})
		close(unblock)
		s.waitExit(deadline())
		if n, d := delivered.Load(), s.drops.Load(); n != 1 || d != 4 {
			t.Errorf("delivered %d, dropped %d; want 1 (the one in the callback) and 4 (3 queued, 1 after stop)", n, d)
		}
	})

	t.Run("an event taken when the listener stops", func(t *testing.T) {
		// After one delivered event, the dispatcher takes a second from the queue while the test
		// holds the mutex, so stop runs before the dispatcher checks it. Past the deadline,
		// waitExit still waits for the dispatcher, which is in no callback: it returns only once
		// the taken event is dropped and counted, never delivered.
		reg := metrics.NewRegistry()
		var delivered atomic.Int32
		s := newListener(func(RefundEvent) { delivered.Add(1) }, 4, reg)
		s.start()
		s.emit(RefundEvent{DeductionID: "d1_first"})
		s.waitIdle(deadline())
		if n := delivered.Load(); n != 1 {
			t.Fatalf("%d of 1 event delivered", n)
		}
		s.mu.Lock()
		s.ch <- RefundEvent{DeductionID: "d1_taken"} // emit's send, with the mutex held
		s.queued++
		for limit := time.Now().Add(waitMax); len(s.ch) > 0; time.Sleep(time.Millisecond) {
			if time.Now().After(limit) {
				s.mu.Unlock()
				t.Fatalf("the dispatcher did not take the event within %v", waitMax)
			}
		}
		s.stopLocked() // the dispatcher holds the event and waits for the mutex
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			s.waitExit(time.Now()) // the deadline has passed
		}()
		select {
		case <-returned:
			s.mu.Unlock()
			t.Fatal("waitExit returned past the deadline while the dispatcher, in no callback, still held a taken event")
		case <-time.After(50 * time.Millisecond):
		}
		s.mu.Unlock()
		select {
		case <-returned:
		case <-time.After(waitMax):
			t.Fatalf("waitExit did not return within %v of the dispatcher's release", waitMax)
		}
		if n, d := delivered.Load(), s.drops.Load(); n != 1 || d != 1 {
			t.Errorf("when waitExit returned: delivered %d, dropped %d; want 1 (the first) and 1 (the taken event)", n, d)
		}
	})

	t.Run("a limiter with a panicking listener", func(t *testing.T) {
		s := newTestStore(t)
		var calls atomic.Int32
		p := testPolicy(Closed)
		fake := newFake(joined(p), sequence())
		fake.setRefund(replies(appliedAt(tNow + 1_000)))
		l := s.ready(t, p, refundInstance(p), fake, Hooks{}, reasonReady, WithRefundListener(func(RefundEvent) {
			calls.Add(1)
			panic("host listener")
		}))
		for range 3 {
			acquireOK(t, l).Release()
		}
		waitFor(t, "3 applied refunds", func() bool { return counter(t, l, "rl_refund_results_total", "applied") == 3 })
		waitFor(t, "3 listener calls", func() bool { return calls.Load() == 3 })
		if got := slotsOf(l); got != (slotCounts{0, 0}) {
			t.Errorf("slots in use %+v, want 0 and 0", got)
		}
		if h := l.Health(); h.ListenerDrops != 0 || h.Status != statusReady {
			t.Errorf("health %+v, want ready with 0 listener drops", h)
		}
		acquireOK(t, l).Done() // the instance still serves
	})
}

// TestListenerCloseBlocked (plan S6, L1; DESIGN §A5.11; SPEC API-1e): a listener blocked in
// its callback holds Close no longer than the listener's window, here its drain deadline;
// after Close, a release() on an OPEN permit ends unresolved (closed) with nothing sent, does
// not panic, and its event is dropped and counted, never delivered.
func TestListenerCloseBlocked(t *testing.T) {
	s := newTestStore(t)
	unblock := make(chan struct{})
	var calls atomic.Int32
	p := testPolicy(Closed)
	fake := newFake(joined(p), sequence())
	fake.setRefund(replies(appliedAt(tNow + 1_000)))
	l := s.ready(t, p, refundInstance(p), fake, Hooks{}, reasonReady, WithRefundListener(func(RefundEvent) {
		calls.Add(1)
		<-unblock
	}))
	t.Cleanup(func() { close(unblock) }) // runs before the limiter's own cleanup Close
	first, second := acquireOK(t, l), acquireOK(t, l)
	first.Release()
	waitFor(t, "the listener blocked in its first callback", func() bool { return calls.Load() == 1 })

	const drain = 300 * time.Millisecond
	closed := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(closed)
		l.Close(WithDrainTimeout(drain))
	}()
	select {
	case <-closed:
	case <-time.After(waitMax):
		t.Fatalf("Close did not return within %v with a blocked listener", waitMax) // the cleanup unblocks it
	}
	if d := time.Since(start); d > drain+700*time.Millisecond {
		t.Errorf("Close took %v with a blocked listener, want at most the %v drain deadline (+ 700 ms)", d, drain)
	}
	refunds := fake.count("refund")
	second.Release()
	if st := second.life.load(); st != stateFinal {
		t.Errorf("a release() after Close left the permit %v, want FINAL (unresolved closed)", st)
	}
	if n := fake.count("refund"); n != refunds {
		t.Errorf("a release() after Close sent %d refund(s), want none", n-refunds)
	}
	if got := counter(t, l, "rl_refund_results_total", "unresolved"); got != 1 {
		t.Errorf("rl_refund_results_total{unresolved} = %v, want 1", got)
	}
	if h := l.Health(); h.ListenerDrops != 1 {
		t.Errorf("listener drops %d, want 1: the event after Close", h.ListenerDrops)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the listener was called %d times, want 1: nothing is delivered after Close", n)
	}
}

// TestRetentionTimer (plan S6 step 3; DESIGN §A5.5; SPEC REF-4a, REF-6, API-1h): with a 1.5 s
// receipt lifetime, an OPEN permit expires 1.5 s after its acquire was sent (its slots freed),
// a later release() ends it unresolved (expired) with nothing sent, and a done() after that is
// silent; a released permit whose refunds the store refuses ends unresolved (retention) at its
// lifetime, with 0 attempts, its slots freed and rl_pending_refunds back to 0.
func TestRetentionTimer(t *testing.T) {
	s := newTestStore(t)
	p := testPolicy(Closed)
	p.ReceiptLifetimeNs, p.RefundDeadlineNs, p.OutageToleranceNs = 1_500_000_000, 1_100_000_000, 100_000_000
	in := refundInstance(p)
	// RCP-8: 0.1 + 1.1 + 0.1 + 0.1 = 1.4 s ≤ 1.5 s; §A5.6: 4 ms + 1 s + (1 + 3) × 1 ms ≤ 1.1 s.
	in.MaxReleaseDelayNs, in.MarginNs, in.DependencyDeadlineNs = 100_000_000, 100_000_000, 1_000_000
	fake := newFake(joined(p), sequence()) // refunds unanswered: unavailable, nothing sent
	log := &eventLog{}
	l := s.ready(t, p, in, fake, Hooks{}, reasonReady, WithRefundListener(log.listen))

	start := time.Now()
	open, released := acquireOK(t, l), acquireOK(t, l)
	released.Release()
	if got := counter(t, l, "rl_pending_refunds", ""); got != 1 {
		t.Errorf("rl_pending_refunds %v after one release, want 1", got)
	}
	evs := waitEvents(t, log, 1)
	waitFor(t, "the OPEN permit to expire", func() bool { return open.life.load() == stateExpired })
	elapsed := time.Since(start)
	if ev := evs[0]; ev.Result != Unresolved || ev.Reason != UnresolvedRetention || ev.Attempts != 0 || ev.DeductionID != released.DeductionID() {
		t.Errorf("released permit's event %+v, want unresolved (retention), 0 attempts", ev)
	}
	if d := time.Duration(evs[0].FinalMonoNs - evs[0].ReleaseMonoNs); d < 1_300*time.Millisecond || d > 2_500*time.Millisecond {
		t.Errorf("unresolved (retention) %v after release(), want about the 1.5 s lifetime", d)
	}
	if elapsed < 1_400*time.Millisecond {
		t.Errorf("the OPEN permit expired %v after its acquire, before the 1.5 s lifetime", elapsed)
	}
	if got := slotsOf(l); got != (slotCounts{0, 0}) {
		t.Errorf("slots in use %+v after both retentions, want 0 and 0", got)
	}
	if got := counter(t, l, "rl_pending_refunds", ""); got != 0 {
		t.Errorf("rl_pending_refunds %v after the retention, want 0", got)
	}
	open.Release()
	evs = waitEvents(t, log, 2)
	if ev := evs[1]; ev.Result != Unresolved || ev.Reason != UnresolvedExpired || ev.Attempts != 0 || ev.DeductionID != open.DeductionID() {
		t.Errorf("expired permit's release() event %+v, want unresolved (expired), 0 attempts", ev)
	}
	open.Done()
	time.Sleep(50 * time.Millisecond)
	if n := len(log.events()); n != 2 {
		t.Errorf("%d events after done() on a finished permit, want 2", n)
	}
	if n := fake.count("refund"); n < 2 {
		t.Errorf("%d refund attempts before the retention ended them, want the workers to keep retrying", n)
	}
}

// TestCloseDrain (plan S6 step 7; SPEC API-1d, API-1e, STA-15, T-REF-6 at L1): (a) refunds the
// store refuses: Close(WithDrainTimeout(150 ms)) returns after about 150 ms with the refund
// ended unresolved (closed), its event delivered before Close returns and nothing dropped
// (API-1d drops an event only on overflow), its slots freed, Acquire not_ready with nothing
// sent and Health closing;
// (b) a refund still in flight when Close begins is applied, and its event delivered, before
// Close returns, and a release() during the drain ends unresolved (closed) at once, its event
// delivered too; a second Close returns at once; (c) with nothing pending, a slow listener
// still gets until the drain deadline to take the queued events: all 4 delivered, none
// dropped, before Close returns.
func TestCloseDrain(t *testing.T) {
	s := newTestStore(t)
	t.Run("refused refunds, 150 ms drain", func(t *testing.T) {
		log := &eventLog{}
		l, fake := s.refundLimiter(t, Closed, nil, replies(store.Response{Outcome: store.Unavailable}), log)
		acquireOK(t, l).Release()
		waitFor(t, "a refused refund attempt", func() bool { return fake.count("refund") > 0 })
		const drain = 150 * time.Millisecond
		start := time.Now()
		l.Close(WithDrainTimeout(drain))
		if d := time.Since(start); d < drain || d > drain+700*time.Millisecond {
			t.Errorf("Close took %v, want the %v drain (+ at most 700 ms)", d, drain)
		}
		// The drain used its whole timeout; its end queued the event, which Close lets the
		// dispatcher deliver before it stops it (SPEC API-1d, API-1e: the remaining refunds emit
		// unresolved (closed), and only an overflow drops an event).
		evs, drops := log.events(), l.Health().ListenerDrops
		if len(evs) != 1 || drops != 0 {
			t.Fatalf("when Close returned: %d events delivered and %d dropped, want 1 and 0", len(evs), drops)
		}
		if evs[0].Result != Unresolved || evs[0].Reason != UnresolvedClosed || evs[0].Attempts != 0 {
			t.Errorf("event %+v, want unresolved (closed) with 0 attempts", evs[0])
		}
		if got := counter(t, l, "rl_refund_results_total", "unresolved"); got != 1 {
			t.Errorf("rl_refund_results_total{unresolved} = %v, want 1", got)
		}
		if got := slotsOf(l); got != (slotCounts{0, 0}) {
			t.Errorf("slots in use %+v, want 0 and 0", got)
		}
		acquires := fake.count("acquire")
		if r := l.Acquire(t.Context(), "k"); r.Kind != NotReady || r.Diag.Reason != reasonClosing || fake.count("acquire") != acquires {
			t.Errorf("Acquire after Close = %v %+v (%d sent), want not_ready (closing), nothing sent", r.Kind, r.Diag, fake.count("acquire")-acquires)
		}
		if h := l.Health(); h.Reason != reasonClosing {
			t.Errorf("health reason %q, want closing", h.Reason)
		}
	})

	t.Run("a refund in flight, and a release during the drain", func(t *testing.T) {
		log := &eventLog{}
		gate, reached := make(chan struct{}), make(chan struct{})
		var once sync.Once
		l, fake := s.refundLimiter(t, Closed, nil, func(store.Request) store.Response {
			once.Do(func() { close(reached) })
			<-gate
			return appliedAt(tNow + 1_000)
		}, log)
		inFlight, late := acquireOK(t, l), acquireOK(t, l)
		inFlight.Release()
		<-reached
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			l.Close() // the instance's drain, 2 s
		}()
		waitFor(t, "Close to begin", func() bool { return l.Health().Reason == reasonClosing })
		late.Release() // during the drain: unresolved (closed), nothing sent
		replied := time.Now()
		close(gate)
		select {
		case <-closed:
		case <-time.After(waitMax):
			t.Fatalf("Close did not return within %v of the refund's reply", waitMax)
		}
		// As soon as nothing is pending, well before the 2 s drain timeout.
		if d := time.Since(replied); d > time.Second {
			t.Errorf("Close returned %v after the last refund's reply, want at once (< 1 s)", d)
		}
		evs := log.events()
		want := map[string]RefundEvent{
			inFlight.DeductionID(): {Result: Applied, Attempts: 1, CreditDecisionNs: (tNow + 1_000) * 1000},
			late.DeductionID():     {Result: Unresolved, Reason: UnresolvedClosed},
		}
		if len(evs) != 2 {
			t.Fatalf("events %+v, want 2", evs)
		}
		for _, ev := range evs {
			w := want[ev.DeductionID]
			if ev.Result != w.Result || ev.Reason != w.Reason || ev.Attempts != w.Attempts || ev.CreditDecisionNs != w.CreditDecisionNs {
				t.Errorf("event %+v, want %+v", ev, w)
			}
		}
		if n := fake.count("refund"); n != 1 {
			t.Errorf("%d refund calls, want 1", n)
		}
		again := time.Now()
		l.Close()
		if d := time.Since(again); d > 100*time.Millisecond {
			t.Errorf("a second Close took %v", d)
		}
	})

	t.Run("queued events delivered until the drain deadline", func(t *testing.T) {
		p := testPolicy(Closed)
		fake := newFake(joined(p), sequence())
		fake.setRefund(replies(appliedAt(tNow + 1_000)))
		gate := make(chan struct{})
		var gateOnce sync.Once
		openGate := func() { gateOnce.Do(func() { close(gate) }) }
		log := &eventLog{}
		// A listener held at the gate, then 20 ms per event.
		l := s.ready(t, p, refundInstance(p), fake, Hooks{}, reasonReady, WithRefundListener(func(ev RefundEvent) {
			<-gate
			time.Sleep(20 * time.Millisecond)
			log.listen(ev)
		}))
		t.Cleanup(openGate) // registered after the limiter: runs before its cleanup Close
		var permits []*Permit
		for range 4 {
			permits = append(permits, acquireOK(t, l))
		}
		for _, pm := range permits {
			pm.Release()
		}
		waitFor(t, "4 applied refunds", func() bool { return counter(t, l, "rl_refund_results_total", "applied") == 4 })
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			l.Close(WithDrainTimeout(2 * time.Second))
		}()
		waitFor(t, "Close to begin", func() bool { return l.Health().Reason == reasonClosing })
		time.Sleep(50 * time.Millisecond) // nothing is pending: Close now waits for the listener only
		openGate()
		select {
		case <-closed:
		case <-time.After(waitMax):
			t.Fatalf("Close did not return within %v of the listener's release", waitMax)
		}
		if evs, drops := log.events(), l.Health().ListenerDrops; len(evs) != 4 || drops != 0 {
			t.Errorf("when Close returned: %d events delivered and %d dropped, want 4 and 0 (the listener has until the 2 s drain deadline)", len(evs), drops)
		}
	})
}

// TestCloseDeliversDrainEndEvents (SPEC API-1d, API-1e, STA-15, T-REF-6 (b); FLT-11 at L1):
// after an epoch-wide state loss (the epoch record lost its hwm), every refund attempt is sent
// and answered code 22 (hwm_missing), so it is retried, and the epoch watch is lost. With 32
// refunds pending, Close(WithDrainTimeout(100 ms)) drains its whole timeout and ends them
// unresolved (closed). Their 32 events, queued at the drain's end in a listener queue with room
// for them, are all delivered before Close returns, though the listener takes 20 ms over the
// first while the rest wait in the queue; each counts its sent attempts (≥ 1), and none is
// dropped: API-1d drops an event only on overflow. Close returns within the drain plus the
// listener's short grace.
func TestCloseDeliversDrainEndEvents(t *testing.T) {
	const (
		n     = 32
		drain = 100 * time.Millisecond
	)
	s := newTestStore(t)
	p := testPolicy(Closed)
	in := refundInstance(p)
	in.MaxHeldReceipts, in.MaxPendingRefunds = n, n
	fake := newFake(joined(p), sequence())
	fake.setRefund(replies(okResp(22, 0, "hwm_missing")))
	log := &eventLog{}
	var slow sync.Once
	l := s.ready(t, p, in, fake, Hooks{}, reasonReady, WithRefundListener(func(ev RefundEvent) {
		slow.Do(func() { time.Sleep(20 * time.Millisecond) }) // a hand-off that is slow once
		log.listen(ev)
	}))
	permits := make([]*Permit, 0, n)
	for range n {
		permits = append(permits, acquireOK(t, l))
	}
	fake.setEpoch(epochErr("hwm_missing")) // the early read that code 22 asks for finds the loss
	for _, pm := range permits {
		pm.Release()
	}
	waitFor(t, "the epoch watch lost (hwm_missing)", func() bool { return l.epoch.Load().state == wsLost })
	waitFor(t, "a refund command sent for each of the 32 permits", func() bool {
		seqs := map[string]bool{}
		for _, r := range fake.requests("refund") {
			seqs[r.Args[5]] = true // the receipt's sequence (§A4.1)
		}
		return len(seqs) == n
	})

	start := time.Now()
	l.Close(WithDrainTimeout(drain))
	took := time.Since(start)
	evs, drops := log.events(), l.Health().ListenerDrops
	if len(evs) != n || drops != 0 {
		t.Fatalf("when Close returned: %d events delivered and %d dropped, want %d and 0: the drain's end queued them in a queue with room for them", len(evs), drops, n)
	}
	got := map[string]bool{}
	for _, ev := range evs {
		if ev.Result != Unresolved || ev.Reason != UnresolvedClosed || ev.Attempts < 1 {
			t.Errorf("event %+v, want unresolved (closed) with its sent attempts counted (≥ 1)", ev)
		}
		got[ev.DeductionID] = true
	}
	for _, pm := range permits {
		if !got[pm.DeductionID()] {
			t.Errorf("no event for permit %s", pm.DeductionID())
		}
	}
	if took < drain || took > drain+700*time.Millisecond {
		t.Errorf("Close took %v, want its %v drain (+ at most 700 ms)", took, drain)
	}
	if v := counter(t, l, "rl_refund_results_total", "unresolved"); v != n {
		t.Errorf("rl_refund_results_total{unresolved} = %v, want %d", v, n)
	}
	if got := slotsOf(l); got != (slotCounts{0, 0}) {
		t.Errorf("slots in use %+v after Close, want 0 and 0", got)
	}
}

// TestCloseListenerGrace (SPEC API-1d, API-1e, HOST-6; the 100 ms grace of plan S6 step 7, to
// meet SPEC API-1d, API-1e, STA-15 and T-REF-6 (b)): both ends of the listener's grace past
// a drain that used its whole timeout. The listener is blocked in its callback for an applied
// refund, and a second refund, which the store refuses, is pending.
// Close(WithDrainTimeout(100 ms)) ends it unresolved (closed) at the drain's end and waits for
// the queue to empty for the whole 100 ms grace (no less), then stops the dispatcher without
// waiting for the blocked callback (at most 100 ms more; the measured overhead is about 2 ms,
// so a grace about 100 ms longer fails here; TestListenerGraceBudget fails any grace over
// 100 ms): its event, still queued, is dropped and counted, and the listener was called once.
func TestCloseListenerGrace(t *testing.T) {
	const (
		drain = 100 * time.Millisecond
		grace = 100 * time.Millisecond
	)
	s := newTestStore(t)
	unblock := make(chan struct{})
	var calls atomic.Int32
	p := testPolicy(Closed)
	fake := newFake(joined(p), sequence())
	fake.setRefund(func(req store.Request) store.Response {
		if req.Args[5] == "1" { // the first permit's receipt: sequence 1
			return appliedAt(tNow + 1_000)
		}
		return store.Response{Outcome: store.Unavailable}
	})
	l := s.ready(t, p, refundInstance(p), fake, Hooks{}, reasonReady, WithRefundListener(func(RefundEvent) {
		calls.Add(1)
		<-unblock
	}))
	t.Cleanup(func() { close(unblock) }) // runs before the limiter's own cleanup Close
	applied, refused := acquireOK(t, l), acquireOK(t, l)
	applied.Release()
	waitFor(t, "the listener blocked in its first callback", func() bool { return calls.Load() == 1 })
	refused.Release()
	waitFor(t, "a refused refund attempt", func() bool { return fake.count("refund") > 1 })

	closed := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(closed)
		l.Close(WithDrainTimeout(drain))
	}()
	select {
	case <-closed:
	case <-time.After(waitMax):
		t.Fatalf("Close did not return within %v with a blocked listener", waitMax) // the cleanup unblocks it
	}
	if took := time.Since(start); took < drain+grace || took > drain+grace+100*time.Millisecond {
		t.Errorf("Close took %v with a blocked listener, want its %v drain + the %v grace (+ at most 100 ms)", took, drain, grace)
	}
	if st := refused.life.load(); st != stateFinal {
		t.Errorf("the refused refund's permit is %v after Close, want FINAL (unresolved closed)", st)
	}
	if h := l.Health(); h.ListenerDrops != 1 {
		t.Errorf("listener drops %d, want 1: the drain's end event, still queued behind the blocked callback", h.ListenerDrops)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the listener was called %d times, want 1", n)
	}
}

// TestCloseListenerGraceFromDrainEnd (SPEC API-1d, API-1e; the F1 decision on Close's listener
// grace): the listener's grace counts from the drain's end, which can come after the drain
// deadline, since the drain ends only once the attempts in flight have. The listener is
// blocked in its callback for an applied refund, and the attempt of a second refund is in a
// store call that ignores its cancel and returns 250 ms after Close began, 200 ms past the
// 50 ms drain deadline. Close then ends that refund unresolved (closed) and gives the blocked
// listener the whole 100 ms grace past that return (no less: a window that ends at the
// drain deadline, or at the deadline + the grace, 150 ms, has passed by then, and Close would
// return at once); then its dispatcher is stopped without waiting for the blocked callback,
// so the event, still queued, is dropped and counted, and the listener was called once.
// TestListenerDeadline pins both sides of the window exactly; this test pins that Close
// measures the grace from the drain's end.
func TestCloseListenerGraceFromDrainEnd(t *testing.T) {
	const (
		drain = 50 * time.Millisecond
		grace = 100 * time.Millisecond
		late  = 250 * time.Millisecond // when the store call returns, from Close's start
	)
	s := newTestStore(t)
	unblock, hold := make(chan struct{}), make(chan struct{})
	var holdOnce sync.Once
	release := func() { holdOnce.Do(func() { close(hold) }) }
	returned := make(chan time.Time, 1)
	var calls atomic.Int32
	p := testPolicy(Closed)
	fake := newFake(joined(p), sequence())
	fake.setRefund(func(req store.Request) store.Response {
		if req.Args[5] == "1" { // the first permit's receipt: sequence 1
			return appliedAt(tNow + 1_000)
		}
		<-hold // the attempt's cancel does not end this call
		select {
		case returned <- time.Now():
		default:
		}
		return store.Response{Outcome: store.Unavailable}
	})
	l := s.ready(t, p, refundInstance(p), fake, Hooks{}, reasonReady, WithRefundListener(func(RefundEvent) {
		calls.Add(1)
		<-unblock
	}))
	t.Cleanup(func() { close(unblock) }) // these two run before the limiter's own cleanup Close
	t.Cleanup(release)
	applied, slow := acquireOK(t, l), acquireOK(t, l)
	applied.Release()
	waitFor(t, "the listener blocked in its first callback", func() bool { return calls.Load() == 1 })
	slow.Release()
	waitFor(t, "the second refund's store call in flight", func() bool { return fake.count("refund") == 2 })

	closed := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(closed)
		l.Close(WithDrainTimeout(drain))
	}()
	time.Sleep(late)
	release()
	select {
	case <-closed:
	case <-time.After(waitMax):
		t.Fatalf("Close did not return within %v with a blocked listener", waitMax) // the cleanup unblocks it
	}
	took := time.Since(start)
	var ret time.Time
	select {
	case ret = <-returned:
	default:
		t.Fatal("Close returned before the second refund's store call did")
	}
	if d := ret.Sub(start); d < drain+grace {
		t.Fatalf("the store call returned %v after Close began, want past the %v drain deadline + the %v grace", d, drain, grace)
	}
	if d := took - ret.Sub(start); d < grace || d > grace+700*time.Millisecond {
		t.Errorf("Close returned %v after the drain's end (the store call's return), want the %v grace past it (+ at most 700 ms)", d, grace)
	}
	if st := slow.life.load(); st != stateFinal {
		t.Errorf("the second refund's permit is %v after Close, want FINAL (unresolved closed)", st)
	}
	if h := l.Health(); h.ListenerDrops != 1 {
		t.Errorf("listener drops %d, want 1: the drain's end event, still queued behind the blocked callback", h.ListenerDrops)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the listener was called %d times, want 1", n)
	}
}

// TestKillStopsRefunds (plan S6 step 8; DESIGN §A5.12; SPEC REF-8): Kill with a refund pending
// (the store refuses it) and the listener running returns within 100 ms; no event is delivered
// afterwards, the pending refund is lost (still RELEASED, no more attempts), a release() after
// Kill delivers nothing, and Close then returns at once and changes nothing: the lost refund
// stays RELEASED, and no result or listener drop is counted.
func TestKillStopsRefunds(t *testing.T) {
	s := newTestStore(t)
	log := &eventLog{}
	l, fake := s.refundLimiter(t, Closed, nil, replies(store.Response{Outcome: store.Unavailable}), log)
	pending, open := acquireOK(t, l), acquireOK(t, l)
	pending.Release()
	waitFor(t, "a refused refund attempt", func() bool { return fake.count("refund") > 0 })
	start := time.Now()
	Kill(l)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Kill took %v, want at most 100 ms", d)
	}
	attempts := fake.count("refund")
	open.Release()
	time.Sleep(50 * time.Millisecond) // more than 10 backoffs of at most 4 ms
	if n := fake.count("refund"); n != attempts {
		t.Errorf("%d refund attempts after Kill, want none", n-attempts)
	}
	if st := pending.life.load(); st != stateReleased {
		t.Errorf("the pending refund's permit is %v after Kill, want RELEASED (lost, no event)", st)
	}
	if evs := log.events(); len(evs) != 0 {
		t.Errorf("events after Kill: %+v, want none", evs)
	}
	results, drops := refundEvents(t, l.reg), l.Health().ListenerDrops
	again := time.Now()
	l.Close()
	if d := time.Since(again); d > 100*time.Millisecond {
		t.Errorf("Close after Kill took %v, want it to return at once", d)
	}
	// Close after Kill does nothing: the lost refund stays RELEASED, with no result counted and
	// no event emitted.
	if st := pending.life.load(); st != stateReleased {
		t.Errorf("the lost refund's permit is %v after Close, want RELEASED (Close after Kill does nothing)", st)
	}
	if n := refundEvents(t, l.reg); n != results {
		t.Errorf("rl_refund_results_total rose from %d to %d in a Close after Kill, want unchanged", results, n)
	}
	if d := l.Health().ListenerDrops; d != drops {
		t.Errorf("listener drops rose from %d to %d in a Close after Kill, want unchanged", drops, d)
	}
}

// wireRefundLimiter builds a ready Closed limiter on ts's fake RESP server, through the real
// adapter, with testPolicy and refundInstance (wireRefundLimiterWith).
func wireRefundLimiter(t *testing.T, ts *testStore, refund fakeresp.Handler, log *eventLog) *Limiter {
	t.Helper()
	p := testPolicy(Closed)
	in := refundInstance(p)
	in.EpochRereadIntervalNs = wireInterval
	return wireRefundLimiterWith(t, ts, p, in, refund, log)
}

// wireRefundLimiterWith builds a ready limiter with p and in on ts's fake RESP server, through
// the real adapter: the store reports epochA under p's fingerprint, allows every acquire
// (generation 1, sequence 1, expiring p's receipt lifetime after tNow) and answers each refund
// through refund; log is the refund listener.
func wireRefundLimiterWith(t *testing.T, ts *testStore, p Policy, in Instance, refund fakeresp.Handler, log *eventLog) *Limiter {
	t.Helper()
	fp := fingerprint.Of(p)
	arr := func(vs ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: vs} }
	ts.srv.Load(scripts.ReadEpoch.Text)
	ts.srv.Handle(scripts.ReadEpoch.SHA, func(_, _ []string) resp.Value {
		return arr(intVal(0), intVal(0), strVal(""), strVal(epochA), strVal(fp), intVal(tNow), intVal(1),
			intVal(tNow), intVal(0), intVal(0), intVal(0))
	})
	ts.srv.Load(scripts.Acquire.Text)
	ts.srv.Handle(scripts.Acquire.SHA, func(_, _ []string) resp.Value {
		return arr(intVal(1), intVal(tNow), strVal(""), intVal(19_000_000_000), intVal(1), intVal(1),
			intVal(tNow+p.ReceiptLifetimeNs/1000))
	})
	ts.srv.Load(scripts.Refund.Text)
	ts.srv.Handle(scripts.Refund.SHA, refund)
	return ts.ready(t, p, in, nil, Hooks{}, reasonReady, WithRefundListener(log.listen))
}

// TestRetentionCountsSentAttempt (SPEC API-1d, REF-4a, REF-5; DESIGN §A5.4 "sent", §A5.5,
// §A5.6), through the real adapter and the fake RESP server: a release() 150 ms before the
// receipt lifetime ends (late, and still attempted, REF-5) sends its first refund command,
// which the store holds unanswered. When the retention timer fires, the permit ends unresolved
// (retention) with Attempts 1, the EVALSHA written counted, and its event comes within 300 ms
// of the timer while the store still holds the reply: the timer cancels the attempt instead of
// waiting out its 750 ms D. The store got exactly one refund command, and none follows.
func TestRetentionCountsSentAttempt(t *testing.T) {
	const (
		lifetime = 4_500 * time.Millisecond
		early    = 150 * time.Millisecond // the release comes this long before the lifetime ends
	)
	ts := newTestStore(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // before the server's Stop, which waits for the handler
	reached := make(chan struct{})
	var once sync.Once
	var refunds atomic.Int32
	p := testPolicy(Closed)
	p.ReceiptLifetimeNs, p.RefundDeadlineNs, p.OutageToleranceNs = int64(lifetime), 4_200_000_000, 100_000_000
	in := refundInstance(p)
	// RCP-8: 0.1 + 4.2 + 0.1 + 0.1 = 4.5 s ≤ 4.5 s, and the margin is above 4.5 s × 1,000 ppm;
	// §A5.6: 4 ms + 1 s + (1 + 3) × 0.75 s = 4.004 s ≤ 4.2 s.
	in.MaxReleaseDelayNs, in.MarginNs, in.DependencyDeadlineNs = 100_000_000, 100_000_000, 750_000_000
	in.EpochRereadIntervalNs = wireInterval
	log := &eventLog{}
	l := wireRefundLimiterWith(t, ts, p, in, func(_, _ []string) resp.Value {
		refunds.Add(1)
		once.Do(func() { close(reached) })
		<-block
		return resp.Value{Kind: resp.Array, Elems: []resp.Value{intVal(3), intVal(tNow + 1_000), strVal(""), intVal(1_000_000_000)}}
	}, log)
	waitFor(t, "2 ready refund connections", func() bool { return counter(t, l, "rl_store_connections", "refund") == 2 })

	before := time.Now() // the acquire is sent after this
	permit := acquireOK(t, l)
	acquired := time.Now() // and before this: the timer fires between before and acquired + lifetime
	time.Sleep(time.Until(before.Add(lifetime - early)))
	permit.Release()
	select {
	case <-reached:
	case <-time.After(waitMax):
		t.Fatalf("the refund did not reach the store within %v", waitMax)
	}
	ev := waitEvents(t, log, 1)[0]
	arrived := time.Now()
	if ev.DeductionID != permit.DeductionID() || ev.Result != Unresolved || ev.Reason != UnresolvedRetention || ev.Attempts != 1 {
		t.Errorf("event %+v, want unresolved (retention) for %s with 1 attempt: the EVALSHA was written (API-1d)", ev, permit.DeductionID())
	}
	if arrived.Before(before.Add(lifetime)) {
		t.Errorf("the event came %v before the lifetime ended", before.Add(lifetime).Sub(arrived))
	}
	if late := arrived.Sub(acquired.Add(lifetime)); late > 300*time.Millisecond {
		t.Errorf("the event came %v after the retention timer, want at most 300 ms: the timer cancels the attempt in flight instead of waiting out its 750 ms D", late)
	}
	time.Sleep(50 * time.Millisecond) // more than 10 backoffs of at most 4 ms
	if n := refunds.Load(); n != 1 {
		t.Errorf("%d refund commands reached the store, want 1: the retention ended the refund (REF-4a)", n)
	}
	if got := slotsOf(l); got != (slotCounts{0, 0}) {
		t.Errorf("slots in use %+v after the retention, want 0 and 0", got)
	}
}

// TestRefundInFlightAtStop (plan S6 steps 7 and 8; DESIGN §A5.12; SPEC API-1d, API-1e): a
// refund command sent through the real adapter, whose reply never comes, is cancelled when
// the workers stop. Kill returns within 500 ms instead of waiting out the attempt's D (2 s),
// and emits nothing. Close(WithDrainTimeout(100 ms)) returns within 500 ms of its deadline
// and ends the refund unresolved (closed) with Attempts 1, the cancelled command counted; its
// event is delivered before Close returns, and nothing is dropped (API-1d).
func TestRefundInFlightAtStop(t *testing.T) {
	for _, kill := range []bool{true, false} {
		name := "Close at the drain deadline"
		if kill {
			name = "Kill"
		}
		t.Run(name, func(t *testing.T) {
			ts := newTestStore(t)
			block := make(chan struct{})
			t.Cleanup(func() { close(block) }) // before the server's Stop, which waits for the handler
			reached := make(chan struct{})
			var once sync.Once
			log := &eventLog{}
			l := wireRefundLimiter(t, ts, func(_, _ []string) resp.Value {
				once.Do(func() { close(reached) })
				<-block
				return resp.Value{Kind: resp.Array, Elems: []resp.Value{intVal(3), intVal(tNow + 1_000), strVal(""), intVal(1_000_000_000)}}
			}, log)
			timeout := time.NewTimer(waitMax) // started before the waits it bounds
			defer timeout.Stop()
			permit := acquireOK(t, l)
			permit.Release()
			select {
			case <-reached:
			case <-timeout.C:
				t.Fatalf("the refund did not reach the store within %v", waitMax)
			}
			const drain = 100 * time.Millisecond
			stopped := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(stopped)
				if kill {
					Kill(l)
				} else {
					l.Close(WithDrainTimeout(drain))
				}
			}()
			select {
			case <-stopped:
			case <-timeout.C:
				t.Fatalf("%s did not return within %v with a refund in flight", name, waitMax)
			}
			d := time.Since(start)
			if kill {
				if d >= 500*time.Millisecond {
					t.Errorf("Kill took %v with a refund in flight, want < 500 ms: it cancels the attempt", d)
				}
				if evs := log.events(); len(evs) != 0 || permit.life.load() != stateReleased {
					t.Errorf("after Kill: events %+v, permit %v; want none, RELEASED (lost)", evs, permit.life.load())
				}
				return
			}
			if d < drain || d > drain+500*time.Millisecond {
				t.Errorf("Close took %v, want its %v drain (+ at most 500 ms): the cancel ends the attempt in flight", d, drain)
			}
			evs, drops := log.events(), l.Health().ListenerDrops
			if len(evs) != 1 || drops != 0 {
				t.Fatalf("when Close returned: %d events delivered and %d dropped, want 1 and 0", len(evs), drops)
			}
			if evs[0].Result != Unresolved || evs[0].Reason != UnresolvedClosed || evs[0].Attempts != 1 {
				t.Errorf("event %+v, want unresolved (closed) with 1 attempt: the command in flight was sent (API-1d)", evs[0])
			}
			if got := counter(t, l, "rl_refund_results_total", "unresolved"); got != 1 {
				t.Errorf("rl_refund_results_total{unresolved} = %v, want 1", got)
			}
			if got := slotsOf(l); got != (slotCounts{0, 0}) {
				t.Errorf("slots in use %+v after Close, want 0 and 0", got)
			}
		})
	}
}

// TestHeldRetentionEventAtStop (SPEC API-1d, API-1h, T-REF-7; DESIGN §A5.5, §A5.12): a
// released permit whose refund attempt is in flight when its retention timer fires (the test
// fires it) ends FINAL at the timer, out of the pending set, but keeps both slots and leaves its
// event to the attempt's worker; the store applies the refund and holds the reply past the
// cancel. Close, whose drain then finds nothing pending, or Kill stops the workers and waits for
// that attempt: until the store answers, the slots stay in use and no event is emitted. Then the
// worker frees the permit's slots, once (the test holds a second reservation, so a second free
// would show), and emits the one event, unresolved (retention) with the sent command counted
// (Attempts 1). Close delivers it before it returns; Kill, whose listener stopped first, drops
// it and counts the drop. Either way delivered + dropped = emitted.
func TestHeldRetentionEventAtStop(t *testing.T) {
	for _, kill := range []bool{false, true} {
		name := "Close"
		if kill {
			name = "Kill"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			reached, gate := make(chan struct{}), make(chan struct{})
			var reachOnce, gateOnce sync.Once
			openGate := func() { gateOnce.Do(func() { close(gate) }) }
			log := &eventLog{}
			l, fake := s.refundLimiter(t, Closed, nil, func(store.Request) store.Response {
				// The store applies the refund and holds its reply until the test opens gate;
				// the fake does not watch the call's context, so the attempt stays in flight
				// past the cancel.
				reachOnce.Do(func() { close(reached) })
				<-gate
				return appliedAt(tNow + 1_000)
			}, log)
			t.Cleanup(openGate)        // runs before the limiter's own cleanup Close
			if !l.slots.tryAcquire() { // a second reservation, so a double free shows as 0 in use, not 1
				t.Fatal("no free slot pair for the second reservation")
			}
			permit := acquireOK(t, l)
			permit.Release()
			select {
			case <-reached:
			case <-time.After(waitMax):
				t.Fatalf("the refund did not reach the store within %v", waitMax)
			}
			permit.expire() // the retention timer fires while the attempt is in flight
			if st := permit.life.load(); st != stateFinal {
				t.Fatalf("permit %v after the timer, want FINAL", st)
			}
			if got := slotsOf(l); got != (slotCounts{2, 2}) {
				t.Fatalf("slots in use %+v after the timer, want 2 and 2 (the permit's pair and the other reservation): the permit's pair waits with the event for the attempt in flight (API-1h)", got)
			}
			if n := refundEvents(t, l.reg); n != 0 {
				t.Fatalf("rl_refund_results_total sums to %d after the timer, want 0: the event waits for the attempt", n)
			}

			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				if kill {
					Kill(l)
				} else {
					l.Close(WithDrainTimeout(time.Second))
				}
			}()
			waitFor(t, name+" stopping the refund workers", func() bool {
				l.refunds.mu.Lock()
				defer l.refunds.mu.Unlock()
				return l.refunds.stopped
			})
			time.Sleep(20 * time.Millisecond) // an effect that did not wait for the attempt would show by now
			if got, n := slotsOf(l), refundEvents(t, l.reg); got != (slotCounts{2, 2}) || n != 0 {
				t.Fatalf("while %s waits for the attempt: slots in use %+v, rl_refund_results_total %d; want 2 and 2, 0", name, got, n)
			}
			openGate()
			select {
			case <-stopped:
			case <-time.After(waitMax):
				t.Fatalf("%s did not return within %v of the attempt's return", name, waitMax)
			}

			evs, drops := log.events(), l.Health().ListenerDrops
			emitted := counter(t, l, "rl_refund_results_total", "unresolved")
			if emitted != 1 || refundEvents(t, l.reg) != 1 {
				t.Fatalf("rl_refund_results_total{unresolved} = %v (all results %d), want 1 and 1: one final event", emitted, refundEvents(t, l.reg))
			}
			wantDelivered, wantDrops := 1, uint64(0)
			if kill {
				wantDelivered, wantDrops = 0, 1 // Kill stopped the listener before the workers
			}
			if len(evs) != wantDelivered || drops != wantDrops || float64(len(evs))+float64(drops) != emitted {
				t.Fatalf("when %s returned: %d delivered and %d dropped of %v emitted, want %d and %d (T-REF-7: delivered + dropped = emitted)",
					name, len(evs), drops, emitted, wantDelivered, wantDrops)
			}
			if !kill {
				if ev := evs[0]; ev.DeductionID != permit.DeductionID() || ev.Result != Unresolved || ev.Reason != UnresolvedRetention || ev.Attempts != 1 {
					t.Errorf("event %+v, want unresolved (retention) for %s with 1 attempt: the held call sent its command (API-1d)", ev, permit.DeductionID())
				}
			}
			if got := slotsOf(l); got != (slotCounts{1, 1}) {
				t.Errorf("slots in use %+v after %s, want 1 and 1 (the other reservation): the worker frees the permit's pair once", got, name)
			}
			if n := fake.count("refund"); n != 1 {
				t.Errorf("%d refund commands reached the store, want 1", n)
			}
		})
	}
}

// TestRefundEventTimes (plan S6 step 6; DESIGN §A5.2, §A5.6; SPEC REF-5): an event's
// ReleaseMonoNs is the time of the permit's first release() since the limiter was built. A,
// released 100 ms after construction, and B, 150 ms after A, carry release times inside the
// instants measured around each call (the construction instant lies between the readings
// taken before and after it), and a second release() of A while its refund is held at the
// store changes nothing.
func TestRefundEventTimes(t *testing.T) {
	s := newTestStore(t)
	gate, reached := make(chan struct{}), make(chan struct{})
	var gateOnce, reachOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }
	refund := func(req store.Request) store.Response {
		if req.Args[5] == "1" { // A's receipt: sequence 1
			reachOnce.Do(func() { close(reached) })
			<-gate
		}
		return appliedAt(tNow + 1_000)
	}
	log := &eventLog{}
	before := time.Now() // the limiter is built after this
	l, _ := s.refundLimiter(t, Closed, nil, refund, log)
	built := time.Now() // and before this
	t.Cleanup(openGate) // registered after the limiter: runs before its cleanup Close
	a, b := acquireOK(t, l), acquireOK(t, l)
	timeout := time.NewTimer(waitMax) // started before the waits it bounds
	defer timeout.Stop()

	time.Sleep(time.Until(built.Add(100 * time.Millisecond)))
	sA := time.Now()
	a.Release()
	eA := time.Now()
	select {
	case <-reached:
	case <-timeout.C:
		t.Fatalf("A's refund did not reach the store within %v", waitMax)
	}
	time.Sleep(time.Until(eA.Add(150 * time.Millisecond)))
	sB := time.Now()
	b.Release()
	eB := time.Now()
	evB := waitEvents(t, log, 1)[0] // B's: A's refund is held
	a.Release()                     // A is RELEASED: a no-op
	openGate()
	evs := waitEvents(t, log, 2)
	evA := evs[1]
	if evB.DeductionID != b.DeductionID() || evA.DeductionID != a.DeductionID() {
		t.Fatalf("events for %s then %s, want B (%s) then A (%s)", evB.DeductionID, evA.DeductionID, b.DeductionID(), a.DeductionID())
	}
	relA, relB := time.Duration(evA.ReleaseMonoNs), time.Duration(evB.ReleaseMonoNs)
	if lo, hi := sA.Sub(built), eA.Sub(before); relA < lo || relA > hi || relA < 100*time.Millisecond {
		t.Errorf("A's ReleaseMonoNs %v, want its first release() time since construction, in [%v, %v] (≥ 100 ms)", relA, lo, hi)
	}
	if lo, hi := sB.Sub(eA), eB.Sub(sA); relB-relA < lo || relB-relA > hi {
		t.Errorf("B − A = %v (A %v, B %v), want the time between the two first releases, in [%v, %v] (about 150 ms)", relB-relA, relA, relB, lo, hi)
	}
	if evA.Result != Applied || evB.Result != Applied {
		t.Errorf("results A %v, B %v; want applied", evA.Result, evB.Result)
	}
}

// TestRetentionStartsAtSend (plan S6 step 3; DESIGN §A5.5; SPEC REF-4a): the retention timer
// starts at the instant just before the acquire was sent, not at its reply. The store takes
// 400 ms to answer the acquire and the receipt lifetime is 1.5 s, so the OPEN permit expires
// about 1.1 s after Acquire returns: at least 1.5 s after Acquire was called (the send came
// later), and at most 1.3 s after it returned (1.1 s, plus 200 ms for scheduling and polling).
func TestRetentionStartsAtSend(t *testing.T) {
	s := newTestStore(t)
	p := testPolicy(Closed)
	p.ReceiptLifetimeNs, p.RefundDeadlineNs, p.OutageToleranceNs = 1_500_000_000, 1_100_000_000, 100_000_000
	in := refundInstance(p)
	// RCP-8: 0.1 + 1.1 + 0.1 + 0.1 = 1.4 s ≤ 1.5 s; §A5.6: 4 ms + 1 s + (1 + 3) × 1 ms ≤ 1.1 s.
	in.MaxReleaseDelayNs, in.MarginNs, in.DependencyDeadlineNs = 100_000_000, 100_000_000, 1_000_000
	fake := newFake(joined(p), func(store.Request) store.Response {
		time.Sleep(400 * time.Millisecond)
		return allowedResp(1, 1)
	})
	l := s.ready(t, p, in, fake, Hooks{}, reasonReady)
	called := time.Now()
	permit := acquireOK(t, l)
	returned := time.Now()
	waitFor(t, "the OPEN permit to expire", func() bool { return permit.life.load() == stateExpired })
	expired := time.Now()
	if d := expired.Sub(called); d < 1_500*time.Millisecond {
		t.Errorf("expired %v after Acquire was called, before the 1.5 s lifetime from the send", d)
	}
	if d := expired.Sub(returned); d > 1_300*time.Millisecond {
		t.Errorf("expired %v after Acquire returned, want about 1.1 s (≤ 1.3 s): the timer starts at the send, 400 ms before the reply", d)
	}
	// The timer's goroutine frees the slots right after the state changes.
	waitFor(t, "the expired permit's slots freed", func() bool { return slotsOf(l) == (slotCounts{0, 0}) })
}

// TestReleaseNoOp (plan S6 step 3; SPEC API-1h, API-5; DESIGN §A5.5): Release() on a degraded
// permit (an Open limiter whose acquire the store refused before the send) and on a nil
// *Permit does nothing: the degraded permit stays DEGRADED, nothing is counted and no event is
// emitted, so a later refund's event is the listener's only one.
func TestReleaseNoOp(t *testing.T) {
	s := newTestStore(t)
	p := testPolicy(Open)
	fake := newFake(joined(p), nil) // acquires refused: unavailable, nothing sent
	fake.setRefund(replies(appliedAt(tNow + 1_000)))
	log := &eventLog{}
	l := s.ready(t, p, refundInstance(p), fake, Hooks{}, reasonReady, WithRefundListener(log.listen))
	r := l.Acquire(t.Context(), "k")
	if r.Kind != Degraded || r.Permit == nil {
		t.Fatalf("Acquire = %v (permit %v), want degraded with a permit", r.Kind, r.Permit != nil)
	}
	degraded := r.Permit
	degraded.Release()
	var none *Permit
	none.Release()
	if st := degraded.life.load(); st != stateDegraded {
		t.Errorf("a degraded permit after Release(): %v, want DEGRADED", st)
	}
	if n := refundEvents(t, l.reg); n != 0 {
		t.Errorf("rl_refund_results_total sums to %d after Release() on a degraded and a nil permit, want 0", n)
	}
	// An allowed permit's refund after them: its event is the first and only one delivered.
	fake.setAcquire(sequence())
	allowed := acquireOK(t, l)
	allowed.Release()
	waitFor(t, "the allowed permit's event", func() bool {
		evs := log.events()
		return len(evs) > 0 && evs[len(evs)-1].DeductionID == allowed.DeductionID()
	})
	if evs := log.events(); len(evs) != 1 {
		t.Errorf("events %+v, want only the allowed permit's", evs)
	}
	if n := fake.count("refund"); n != 1 {
		t.Errorf("%d refund calls, want 1 (the allowed permit's)", n)
	}
	if h := l.Health(); h.ListenerDrops != 0 {
		t.Errorf("listener drops %d, want 0", h.ListenerDrops)
	}
	if got := slotsOf(l); got != (slotCounts{0, 0}) {
		t.Errorf("slots in use %+v, want 0 and 0", got)
	}
}

// TestRefundInstanceSettings (plan S6 steps 4–6; DESIGN §A5.6, §A5.11): the Instance settings
// reach the refund machinery. ListenerQueueSize 2: with the listener blocked in its first
// callback, 4 more events leave 2 queued and 2 dropped. RefundWorkers 2: of three refunds held
// at the store, exactly 2 are there at once and the third is still unsent 100 ms later (the
// default 128 workers would send all three). RefundBackoffInitialNs 300 ms (maximum 1 s): the
// second attempt comes 150–400 ms after the refused first one (the 50 ms default would give
// 25–50 ms, the maximum 0.5–1 s).
func TestRefundInstanceSettings(t *testing.T) {
	s := newTestStore(t)
	p := testPolicy(Closed)
	t.Run("ListenerQueueSize 2", func(t *testing.T) {
		in := refundInstance(p)
		in.ListenerQueueSize = 2
		fake := newFake(joined(p), sequence())
		fake.setRefund(replies(appliedAt(tNow + 1_000)))
		gate := make(chan struct{})
		var gateOnce sync.Once
		openGate := func() { gateOnce.Do(func() { close(gate) }) }
		var calls atomic.Int32
		l := s.ready(t, p, in, fake, Hooks{}, reasonReady, WithRefundListener(func(RefundEvent) {
			calls.Add(1)
			<-gate
		}))
		t.Cleanup(openGate) // registered after the limiter: runs before its cleanup Close
		acquireOK(t, l).Release()
		waitFor(t, "the listener blocked in its first callback", func() bool { return calls.Load() == 1 })
		var permits []*Permit
		for range 4 {
			permits = append(permits, acquireOK(t, l))
		}
		for _, pm := range permits {
			pm.Release()
		}
		// Every event emitted is queued (the one in the callback included) or dropped.
		waitFor(t, "5 events queued or dropped", func() bool {
			l.lis.mu.Lock()
			defer l.lis.mu.Unlock()
			return l.lis.queued+int(l.lis.drops.Load()) == 5
		})
		if d := l.Health().ListenerDrops; d != 2 {
			t.Errorf("%d listener drops, want 2: 1 event in the callback, 2 queued, 2 dropped", d)
		}
		openGate()
		waitFor(t, "the 2 queued events delivered", func() bool { return calls.Load() == 3 })
	})

	t.Run("RefundWorkers 2", func(t *testing.T) {
		in := refundInstance(p)
		in.RefundWorkers = 2
		gate := make(chan struct{})
		var gateOnce sync.Once
		openGate := func() { gateOnce.Do(func() { close(gate) }) }
		var atStore atomic.Int32
		fake := newFake(joined(p), sequence())
		fake.setRefund(func(store.Request) store.Response {
			atStore.Add(1)
			<-gate
			return appliedAt(tNow + 1_000)
		})
		log := &eventLog{}
		l := s.ready(t, p, in, fake, Hooks{}, reasonReady, WithRefundListener(log.listen))
		t.Cleanup(openGate) // registered after the limiter: runs before its cleanup Close
		permits := []*Permit{acquireOK(t, l), acquireOK(t, l), acquireOK(t, l)}
		for _, pm := range permits {
			pm.Release()
		}
		waitFor(t, "2 refunds at the store at once (2 workers)", func() bool { return atStore.Load() >= 2 })
		time.Sleep(100 * time.Millisecond)
		if n := atStore.Load(); n != 2 {
			t.Errorf("%d refunds at the store 100 ms after the second arrived, want exactly 2: 2 workers, both held, so the third waits in the queue", n)
		}
		openGate()
		waitEvents(t, log, 3)
		if n := atStore.Load(); n != 3 {
			t.Errorf("%d refund calls after the gate opened, want 3", n)
		}
	})

	t.Run("RefundBackoffInitialNs 300 ms", func(t *testing.T) {
		in := refundInstance(p)
		in.RefundBackoffInitialNs, in.RefundBackoffMaxNs = 300_000_000, 1_000_000_000
		var mu sync.Mutex
		var at []time.Time
		fake := newFake(joined(p), sequence())
		fake.setRefund(func(store.Request) store.Response {
			mu.Lock()
			defer mu.Unlock()
			at = append(at, time.Now())
			if len(at) == 1 {
				return store.Response{Outcome: store.Unavailable}
			}
			return appliedAt(tNow + 1_000)
		})
		log := &eventLog{}
		l := s.ready(t, p, in, fake, Hooks{}, reasonReady, WithRefundListener(log.listen))
		acquireOK(t, l).Release()
		waitEvents(t, log, 1)
		mu.Lock()
		times := slices.Clone(at)
		mu.Unlock()
		if len(times) != 2 {
			t.Fatalf("%d refund calls, want 2", len(times))
		}
		// The first backoff is min(1 s, 300 ms × 2^0) × U[0.5, 1.0] = 150–300 ms; 100 ms of slack
		// above it for scheduling. The 50 ms default gives 25–50 ms and the 1 s maximum 0.5–1 s.
		if gap := times[1].Sub(times[0]); gap < 150*time.Millisecond || gap > 400*time.Millisecond {
			t.Errorf("second attempt %v after the first, want 150–400 ms: min(1 s, 300 ms × 2^0) × [0.5, 1.0]", gap)
		}
	})
}
