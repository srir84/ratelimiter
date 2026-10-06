package core

import (
	"strconv"
	"sync/atomic"
	"time"

	"ratelimiter/internal/receipt"
)

// permitState is a permit's lifecycle state (§A5.5, SPEC API-1h). DONE, FINAL and DEGRADED
// are terminal: every event there is a no-op.
type permitState int32

const (
	stateOpen     permitState = iota // allowed; holds a receipt and both slots
	stateReleased                    // release() queued a refund; slots held until its final event
	stateExpired                     // retention ended while OPEN; slots freed
	stateDone                        // finished with done()
	stateFinal                       // the one final event was emitted
	stateDegraded                    // degraded acquire: no receipt, no slots
)

func (s permitState) String() string {
	switch s {
	case stateOpen:
		return "OPEN"
	case stateReleased:
		return "RELEASED"
	case stateExpired:
		return "EXPIRED"
	case stateDone:
		return "DONE"
	case stateFinal:
		return "FINAL"
	case stateDegraded:
		return "DEGRADED"
	}
	return "permitState(" + strconv.Itoa(int(s)) + ")"
}

// permitEvent is what can happen to a permit.
type permitEvent int8

const (
	evDone           permitEvent = iota // the host calls done()
	evRelease                           // the host calls release()
	evReleaseClosing                    // the host calls release() after Close began
	evRetention                         // the retention timer fires (receipt_lifetime after the send, REF-4a)
	evFinalResult                       // a refund worker gets a final result
	evDrainEnd                          // Close's drain ends with the refund still pending
)

func (e permitEvent) String() string {
	switch e {
	case evDone:
		return "done"
	case evRelease:
		return "release"
	case evReleaseClosing:
		return "release_closing"
	case evRetention:
		return "retention"
	case evFinalResult:
		return "final_result"
	case evDrainEnd:
		return "drain_end"
	}
	return "permitEvent(" + strconv.Itoa(int(e)) + ")"
}

// emission is the listener event a transition emits (API-1d: at most one per permit).
type emission int8

const (
	emitNone      emission = iota
	emitResult             // the refund worker's final result
	emitClosed             // unresolved, reason closed
	emitRetention          // unresolved, reason retention
	emitExpired            // unresolved, reason expired
)

func (m emission) String() string {
	switch m {
	case emitNone:
		return "none"
	case emitResult:
		return "result"
	case emitClosed:
		return "unresolved_closed"
	case emitRetention:
		return "unresolved_retention"
	case emitExpired:
		return "unresolved_expired"
	}
	return "emission(" + strconv.Itoa(int(m)) + ")"
}

// transition is one row of §A5.5's table: the new state and the effects that only the
// goroutine that made the change performs.
type transition struct {
	to        permitState
	freeSlots bool     // free both the receipt and the refund slot
	emit      emission // the final listener event, if any
	enqueue   bool     // put the refund job on the ready queue
}

// transitions is §A5.5's table, its 7 rows expanded to 10 (state, event) entries. ok is
// false for every other pair, a no-op.
func transitions(from permitState, ev permitEvent) (transition, bool) {
	switch from {
	case stateOpen:
		switch ev {
		case evDone:
			return transition{to: stateDone, freeSlots: true}, true
		case evRelease: // the queue cannot be full: its capacity is the slot count
			return transition{to: stateReleased, enqueue: true}, true
		case evReleaseClosing: // nothing is sent
			return transition{to: stateFinal, freeSlots: true, emit: emitClosed}, true
		case evRetention:
			return transition{to: stateExpired, freeSlots: true}, true
		case evFinalResult, evDrainEnd: // no refund is pending
		}
	case stateExpired: // the slots are already free
		switch ev {
		case evDone: // silent
			return transition{to: stateDone}, true
		case evRelease, evReleaseClosing: // nothing is sent
			return transition{to: stateFinal, emit: emitExpired}, true
		case evRetention, evFinalResult, evDrainEnd:
		}
	case stateReleased: // the first finish was release(): later finishes are no-ops
		switch ev {
		case evFinalResult:
			return transition{to: stateFinal, freeSlots: true, emit: emitResult}, true
		case evRetention:
			return transition{to: stateFinal, freeSlots: true, emit: emitRetention}, true
		case evDrainEnd:
			return transition{to: stateFinal, freeSlots: true, emit: emitClosed}, true
		case evDone, evRelease, evReleaseClosing:
		}
	case stateDone, stateFinal, stateDegraded:
	}
	return transition{}, false
}

// Permit is what an allowed or degraded Acquire returns (DESIGN §A5.5, SPEC API-1h). An
// allowed permit holds the signed receipt and one slot of each kind until its first finish;
// a degraded one holds neither, and every call on it is a no-op. Release and the retention
// timer arrive in S6.
type Permit struct {
	life    *lifecycle
	slots   *slots          // nil for a degraded permit
	receipt receipt.Receipt // the signed receipt; zero for a degraded permit
	id      string          // deduction_id; "" for a degraded permit
	sentAt  time.Time       // monotonic instant just before the acquire was sent: the retention timer's start (S6)
}

// newPermit returns an OPEN permit that holds one reservation of sl.
func newPermit(sl *slots, rc receipt.Receipt, id string, sentAt time.Time) *Permit {
	return &Permit{life: newLifecycle(stateOpen), slots: sl, receipt: rc, id: id, sentAt: sentAt}
}

// newDegradedPermit returns a DEGRADED permit: no receipt, no slots, no deduction_id (API-5).
func newDegradedPermit() *Permit {
	return &Permit{life: newLifecycle(stateDegraded)}
}

// Done is the host's finish on success or an unknown outcome (API-1c): OPEN → DONE keeps the
// charge and frees both slots. In every other state, and on a nil permit, it is a no-op; the
// compare-and-swap lets only one caller free the slots.
func (p *Permit) Done() {
	if p == nil {
		return
	}
	if t, ok := p.life.fire(evDone); ok && t.freeSlots {
		p.slots.release()
	}
}

// DeductionID returns the permit's deduction_id (§A5.9): "" for a degraded or nil permit.
func (p *Permit) DeductionID() string {
	if p == nil {
		return ""
	}
	return p.id
}

// lifecycle holds a permit's state; Permit is built on it.
type lifecycle struct {
	state atomic.Int32
}

func newLifecycle(s permitState) *lifecycle {
	l := &lifecycle{}
	l.state.Store(int32(s))
	return l
}

func (l *lifecycle) load() permitState { return permitState(l.state.Load()) }

// fire applies ev with a compare-and-swap loop: it reads the state, looks up (state, ev)
// and swaps in the new state; when another event changed the state first, it reads it
// again. It returns true only to the caller whose swap succeeded, which alone performs the
// returned transition's effects, and false when ev is a no-op in the state it found.
func (l *lifecycle) fire(ev permitEvent) (transition, bool) {
	for {
		cur := l.state.Load()
		t, ok := transitions(permitState(cur), ev)
		if !ok {
			return transition{}, false
		}
		if l.state.CompareAndSwap(cur, int32(t.to)) {
			return t, true
		}
	}
}
