package core

import (
	"sync"
	"testing"
	"time"

	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
)

func allStates() []permitState {
	return []permitState{stateOpen, stateReleased, stateExpired, stateDone, stateFinal, stateDegraded}
}

func allEvents() []permitEvent {
	return []permitEvent{evDone, evRelease, evReleaseClosing, evRetention, evFinalResult, evDrainEnd}
}

type statePair struct {
	from permitState
	ev   permitEvent
}

// wantTable is DESIGN §A5.5's transition table, written out: 7 rows as 10 (state, event)
// entries. Every other pair is a no-op.
func wantTable() map[statePair]transition {
	return map[statePair]transition{
		{stateOpen, evDone}:              {to: stateDone, freeSlots: true},
		{stateOpen, evRelease}:           {to: stateReleased, enqueue: true},
		{stateOpen, evRetention}:         {to: stateExpired, freeSlots: true},
		{stateOpen, evReleaseClosing}:    {to: stateFinal, freeSlots: true, emit: emitClosed},
		{stateExpired, evDone}:           {to: stateDone},
		{stateExpired, evRelease}:        {to: stateFinal, emit: emitExpired},
		{stateExpired, evReleaseClosing}: {to: stateFinal, emit: emitExpired},
		{stateReleased, evFinalResult}:   {to: stateFinal, freeSlots: true, emit: emitResult},
		{stateReleased, evRetention}:     {to: stateFinal, freeSlots: true, emit: emitRetention},
		{stateReleased, evDrainEnd}:      {to: stateFinal, freeSlots: true, emit: emitClosed},
	}
}

func TestPermitTransitionTable(t *testing.T) {
	want := wantTable()
	entries, liveNoops, terminalNoops := 0, 0, 0
	for _, s := range allStates() {
		for _, e := range allEvents() {
			w, wok := want[statePair{s, e}]
			if got, ok := transitions(s, e); ok != wok || got != w {
				t.Errorf("transitions(%v, %v) = %+v, %v; want %+v, %v", s, e, got, ok, w, wok)
			}
			l := newLifecycle(s)
			got, ok := l.fire(e)
			wantState := s
			if wok {
				wantState = w.to
			}
			if ok != wok || got != w || l.load() != wantState {
				t.Errorf("fire(%v) from %v = %+v, %v, now %v; want %+v, %v, now %v", e, s, got, ok, l.load(), w, wok, wantState)
			}
			switch {
			case wok:
				entries++
			case s == stateOpen || s == stateReleased || s == stateExpired:
				liveNoops++
			default:
				terminalNoops++
			}
		}
	}
	if entries != 10 || liveNoops != 8 || terminalNoops != 18 {
		t.Errorf("%d entries, %d no-ops from OPEN/RELEASED/EXPIRED, %d from DONE/FINAL/DEGRADED; want 10, 8, 18", entries, liveNoops, terminalNoops)
	}
	if _, ok := transitions(permitState(99), evDone); ok {
		t.Error("an unknown state has a transition")
	}
	if _, ok := transitions(stateOpen, permitEvent(99)); ok {
		t.Error("an unknown event has a transition")
	}
}

func TestPermitSequencesInvariants(t *testing.T) {
	evs := allEvents()
	n := 0
	for code := range 6 * 6 * 6 * 6 { // every sequence of 4 events, in base 6
		seq := []permitEvent{evs[code%6], evs[code/6%6], evs[code/36%6], evs[code/216%6]}
		l := newLifecycle(stateOpen)
		frees, emits, enqueues := 0, 0, 0
		for _, e := range seq {
			tr, ok := l.fire(e)
			if !ok {
				continue
			}
			if tr.freeSlots {
				frees++
			}
			if tr.emit != emitNone {
				emits++
			}
			if tr.enqueue {
				enqueues++
			}
		}
		final := l.load()
		slotsGone := final == stateDone || final == stateFinal || final == stateExpired
		if frees > 1 || (frees == 1) != slotsGone {
			t.Errorf("%v: slots freed %d times, ending %v", seq, frees, final)
		}
		if emits > 1 || (emits == 1) != (final == stateFinal) {
			t.Errorf("%v: %d events, ending %v", seq, emits, final)
		}
		if enqueues > 1 {
			t.Errorf("%v: %d enqueues", seq, enqueues)
		}
		n++
	}
	if n != 1296 {
		t.Errorf("%d sequences, want 6^4 = 1296", n)
	}
}

// fired is one fire call's result.
type fired struct {
	t  transition
	ok bool
}

// pairOutcome is what firing e1 and e2 on one permit did: the final state and each call's
// result.
type pairOutcome struct {
	final  permitState
	e1, e2 fired
}

// serialOutcome fires e1 then e2 from s (e2 first when e2First), by wantTable.
func serialOutcome(want map[statePair]transition, s permitState, e1, e2 permitEvent, e2First bool) pairOutcome {
	step := func(s permitState, e permitEvent) (permitState, fired) {
		if tr, ok := want[statePair{s, e}]; ok {
			return tr.to, fired{tr, true}
		}
		return s, fired{}
	}
	var o pairOutcome
	if e2First {
		s, o.e2 = step(s, e2)
		o.final, o.e1 = step(s, e1)
	} else {
		s, o.e1 = step(s, e1)
		o.final, o.e2 = step(s, e2)
	}
	return o
}

func TestPermitTransitionPairsRace(t *testing.T) {
	const iterations = 1_000
	want := wantTable()
	for _, from := range []permitState{stateOpen, stateReleased, stateExpired} {
		for _, e1 := range allEvents() {
			for _, e2 := range allEvents() {
				order12 := serialOutcome(want, from, e1, e2, false)
				order21 := serialOutcome(want, from, e1, e2, true)
				for range iterations {
					l := newLifecycle(from)
					var got pairOutcome
					var wg sync.WaitGroup
					start := make(chan struct{})
					wg.Add(2)
					go func() {
						defer wg.Done()
						<-start
						got.e1.t, got.e1.ok = l.fire(e1)
					}()
					go func() {
						defer wg.Done()
						<-start
						got.e2.t, got.e2.ok = l.fire(e2)
					}()
					close(start)
					wg.Wait()
					got.final = l.load()
					if got != order12 && got != order21 {
						t.Fatalf("from %v, %v ‖ %v concurrently = %+v\nwant one serial order: %+v\nor %+v", from, e1, e2, got, order12, order21)
					}
					frees, emits := 0, 0
					for _, f := range []fired{got.e1, got.e2} {
						if f.ok && f.t.freeSlots {
							frees++
						}
						if f.ok && f.t.emit != emitNone {
							emits++
						}
					}
					if frees > 1 || emits > 1 {
						t.Fatalf("from %v, %v ‖ %v: slots freed %d times, %d events", from, e1, e2, frees, emits)
					}
				}
			}
		}
	}
}

// gaugeValue returns the value of the series of the named family with the given label value
// in a snapshot.
func gaugeValue(t *testing.T, snap metrics.MetricsSnapshot, name, label string) float64 {
	t.Helper()
	for _, f := range snap.Families {
		if f.Name != name {
			continue
		}
		for _, s := range f.Series {
			if s.LabelValue == label {
				return s.Value
			}
		}
	}
	t.Fatalf("no series %s{%q}", name, label)
	return 0
}

// TestPermitDoneOnce (L1): 100 concurrent Done() calls on one OPEN permit free its slots
// exactly once. Two reservations are held, so a second free would show as 0 slots in use;
// each round also checks DONE, the gauges, and that later calls stay no-ops.
func TestPermitDoneOnce(t *testing.T) {
	const rounds, callers = 50, 100
	for round := range rounds {
		reg := metrics.NewRegistry()
		sl := newSlots(2, 2, reg)
		for i := range 2 {
			if !sl.tryAcquire() {
				t.Fatalf("cannot reserve slot pair %d of 2", i+1)
			}
		}
		if sl.tryAcquire() {
			t.Fatal("a third reservation of 2 slots succeeded")
		}
		p := newPermit(sl, nil, receipt.Receipt{}, "d1_test", time.Now())
		if id := p.DeductionID(); id != "d1_test" {
			t.Fatalf("round %d: an OPEN permit's DeductionID = %q, want d1_test", round, id)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				p.Done()
			}()
		}
		close(start)
		wg.Wait()
		if r, f := sl.inUse(); r != 1 || f != 1 {
			t.Fatalf("round %d: after %d concurrent Done(): %d receipt and %d refund slots in use, want 1 and 1", round, callers, r, f)
		}
		if s := p.life.load(); s != stateDone {
			t.Fatalf("round %d: permit state %v, want DONE", round, s)
		}
		snap := reg.Snapshot()
		if r, f := gaugeValue(t, snap, "rl_slots_in_use", "receipt"), gaugeValue(t, snap, "rl_slots_in_use", "refund"); r != 1 || f != 1 {
			t.Fatalf("round %d: rl_slots_in_use = %v receipt, %v refund; want 1, 1", round, r, f)
		}
		p.Done()
		if r, f := sl.inUse(); r != 1 || f != 1 {
			t.Fatalf("round %d: a later Done() freed slots: %d and %d in use, want 1 and 1", round, r, f)
		}
	}
	// A degraded permit and a nil one hold nothing: Done is a no-op, DeductionID is "".
	d := newDegradedPermit()
	if st := d.life.load(); st != stateDegraded || d.slots != nil {
		t.Fatalf("a new degraded permit: state %v, slots held %v; want DEGRADED, no slots", st, d.slots != nil)
	}
	d.Done()
	var nilPermit *Permit
	nilPermit.Done()
	if d.life.load() != stateDegraded || d.DeductionID() != "" || nilPermit.DeductionID() != "" {
		t.Fatalf("degraded permit: state %v, DeductionID %q; nil permit DeductionID %q", d.life.load(), d.DeductionID(), nilPermit.DeductionID())
	}
}
