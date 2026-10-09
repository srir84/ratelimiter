//go:build rltest

package fixtures

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// Mode is a store fault (DESIGN §B6; the names are HARNESS's fault modes).
type Mode string

// The store fault modes.
const (
	// Refuse answers unavailable without calling the store: nothing is sent (FLT-1 b).
	Refuse Mode = "refuse"
	// DropReply makes the real call and discards its reply: outcome_unknown (FLT-3, FLT-4).
	// A call that sent nothing passes through unchanged: there was no reply to lose.
	DropReply Mode = "drop_reply"
	// Delay waits Fault.Delay, at its Fault.At (IMPLEMENTATION_PLAN H4 step 6). Before the send
	// (At before_send, or empty), it waits Delay or until the call's ctx ends; when ctx ends
	// first, the call returns cancelled without reaching the store adapter. After the reply
	// (At after_reply), the call goes to the store at once, and its return waits Delay or until
	// ctx ends. In a CallBatch the before_send wait of every delayed request starts when the
	// batch enters the wrapper and runs while the rest of the batch goes ahead; then the delayed
	// requests whose waits have passed go out together, and those whose ctx ended first are
	// cancelled. The after_reply requests go out with the rest, and the batch returns once the
	// longest of their waits has passed after the replies.
	Delay Mode = "delay"
	// Barrier blocks before the send or after the reply until Release (§B6 Barriers). In a
	// CallBatch a request held before the send is sent alone after Release, while the rest of
	// the batch goes ahead.
	Barrier Mode = "barrier"
)

// At is where a barrier blocks, or where a delay waits.
type At string

// The barrier and delay positions.
const (
	BeforeSend At = "before_send"
	AfterReply At = "after_reply"
)

// Fault is one armed store fault (plan choice).
type Fault struct {
	Mode Mode
	// Match is the script name (Request.Script.Name: "acquire", "refund", …) or, for Do, the
	// command (its first argument, e.g. "SCAN"), compared case-insensitively; "" matches every
	// call. Every mode takes every match: match "refund" puts the fault on the refund calls
	// alone (plan S6 step 10).
	Match string
	// Key, when not empty, narrows the match to the calls for that budget key (plan S7 step
	// 11): an acquire, refund or cleanup_entry request whose enc (ARGV 3, §A4.1) is the key's
	// encoding. No other call matches it: not another key's, not a Do, and not a script that
	// takes no enc. So a barrier can hold one key's cleanup_entry step inside a pipeline.
	Key string
	// Count is how many matching calls the fault applies to; 0 is every matching call while
	// it is armed.
	Count int
	// Duration is how long the fault stays armed after Arm; 0 is no limit.
	Duration time.Duration
	// Delay is the wait of a Delay fault.
	Delay time.Duration
	// At places a Barrier fault (before_send or after_reply) or a Delay fault (before_send,
	// the default when empty, or after_reply); Refuse and DropReply take none. Name names a
	// Barrier fault: Reached(Name) and Release(Name) address it.
	At   At
	Name string
}

// armed is a fault with what is left of it.
type armed struct {
	f     Fault
	enc   string    // keys.Encode(f.Key) when f.Key is set
	left  int       // calls left when f.Count > 0
	until time.Time // the zero time: no limit
}

// barrier is one named barrier: reached closes when the first call arrives, release when
// the test releases it.
type barrier struct {
	reached, release      chan struct{}
	isReached, isReleased bool
}

// maxStoreFaults is how many armed store faults may wait at once, and maxBarriers how many
// barrier names the wrapper holds at once (plan choice: the database wrapper's cap of 64,
// maxDBFaults; AGENTS.md §1 Queues). Arm refuses a fault past either cap, and DisarmBarrier
// never adds a name, so hitapi's POST /_test/faults and /_test/barrier routes cannot grow
// either without bound.
const (
	maxStoreFaults = 64
	maxBarriers    = 64
)

// StoreWrapper stands between the limiter and its store adapter (DESIGN §A5.12, §B6) and
// applies the armed faults: at most maxStoreFaults at once, under at most maxBarriers barrier
// names. It also counts, as the store adapter reported them, the responses that came back
// with NoScript set, for the driver's allowed-commands check, and the refund responses the
// store refused with LOADING or BUSY, for its refund-sends check (§C4). One wrapper serves
// one limiter; it is safe for concurrent use.
type StoreWrapper struct {
	mu       sync.Mutex
	faults   []*armed // at most maxStoreFaults
	barriers map[string]*barrier
	// pending counts, per barrier name, the calls that took one of its faults and have yet to
	// reach the barrier, so a name is never dropped while a call is on its way to it.
	pending  map[string]int
	noScript int
	notReady int // refund responses refused with LOADING or BUSY
}

// NewStoreWrapper returns a wrapper with no fault armed.
func NewStoreWrapper() *StoreWrapper {
	return &StoreWrapper{barriers: map[string]*barrier{}, pending: map[string]int{}}
}

// Wrap returns inner behind the wrapper; limitertest.WithStoreWrapper takes it.
func (w *StoreWrapper) Wrap(inner store.Adapter) store.Adapter {
	return &wrapped{w: w, next: inner}
}

// Arm arms f. It fails on an unknown mode, a negative count or time, a barrier without a name
// or position, a delay without a wait or with an unknown position, a position on refuse or
// drop_reply, and a Key that is no valid budget key (API-1). It also refuses f while
// maxStoreFaults faults are armed (a fault used up, disarmed or past its Duration frees its
// slot), and a Barrier fault under a new name while maxBarriers names are held: a name is
// dropped first once it is finished, that is released, named by no armed fault, and with no
// call on its way to it. A refused fault arms nothing.
func (w *StoreWrapper) Arm(f Fault) error {
	if f.Count < 0 || f.Duration < 0 || f.Delay < 0 {
		return fmt.Errorf("fixtures: fault %q: negative count or time", f.Mode)
	}
	var enc string
	if f.Key != "" {
		var err error
		if enc, err = keys.Encode(f.Key); err != nil {
			return fmt.Errorf("fixtures: fault %q: key: %w", f.Mode, err)
		}
	}
	switch f.Mode {
	case Refuse, DropReply:
		if f.At != "" {
			return fmt.Errorf("fixtures: fault %q takes no position", f.Mode)
		}
	case Delay:
		if f.Delay == 0 {
			return fmt.Errorf("fixtures: fault %q: no delay", f.Mode)
		}
		if f.At != "" && f.At != BeforeSend && f.At != AfterReply {
			return fmt.Errorf("fixtures: fault %q: position %q, want before_send or after_reply", f.Mode, f.At)
		}
	case Barrier:
		if f.Name == "" || (f.At != BeforeSend && f.At != AfterReply) {
			return fmt.Errorf("fixtures: fault %q: needs a name and a position (before_send or after_reply)", f.Mode)
		}
	default:
		return fmt.Errorf("fixtures: unknown fault mode %q", f.Mode)
	}
	a := &armed{f: f, enc: enc, left: f.Count}
	if f.Duration > 0 {
		a.until = time.Now().Add(f.Duration)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropEndedLocked(time.Now())
	if len(w.faults) >= maxStoreFaults {
		return fmt.Errorf("fixtures: fault %q refused: %d faults are armed already, the most that may wait", f.Mode, maxStoreFaults)
	}
	if f.Mode == Barrier {
		b, ok := w.barriers[f.Name]
		if !ok && !w.roomForBarrierLocked() {
			return fmt.Errorf("fixtures: barrier %q refused: %d barrier names are held already, the most at once", f.Name, maxBarriers)
		}
		if !ok || b.isReleased {
			w.barriers[f.Name] = newBarrier()
		}
	}
	w.faults = append(w.faults, a)
	return nil
}

// Armed returns how many faults are armed: those not yet used up, disarmed or past their
// Duration.
func (w *StoreWrapper) Armed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	n := 0
	for _, a := range w.faults {
		if !a.ended(now) {
			n++
		}
	}
	return n
}

// ended reports whether the fault's Duration has passed at now.
func (a *armed) ended(now time.Time) bool {
	return !a.until.IsZero() && !now.Before(a.until)
}

// dropEndedLocked removes the faults whose Duration has passed; w.mu is held.
func (w *StoreWrapper) dropEndedLocked(now time.Time) {
	w.faults = slices.DeleteFunc(w.faults, func(a *armed) bool { return a.ended(now) })
}

// roomForBarrierLocked reports whether a new barrier name fits under maxBarriers, after
// dropping every finished name: released, named by no armed fault, and with no call on its way
// to it (pending). Such a barrier has nothing left to block or release; a later Reached or
// Arm under its name starts a new one. w.mu is held.
func (w *StoreWrapper) roomForBarrierLocked() bool {
	if len(w.barriers) < maxBarriers {
		return true
	}
	named := make(map[string]bool, len(w.faults))
	for _, a := range w.faults {
		if a.f.Mode == Barrier {
			named[a.f.Name] = true
		}
	}
	for name, b := range w.barriers {
		if b.isReleased && !named[name] && w.pending[name] == 0 {
			delete(w.barriers, name)
		}
	}
	return len(w.barriers) < maxBarriers
}

// Disarm removes every armed fault and releases every barrier.
func (w *StoreWrapper) Disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.faults = nil
	for _, b := range w.barriers {
		b.open()
	}
}

// Reached returns a channel that is closed once a call reaches the barrier name. It may be
// taken before the barrier is armed.
func (w *StoreWrapper) Reached(name string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.barrierLocked(name).reached
}

// Release lets every call blocked at the barrier name go on, and every later one pass.
func (w *StoreWrapper) Release(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.barrierLocked(name).open()
}

// DisarmBarrier removes every Barrier fault named name, then releases that barrier, so the
// calls blocked at it go on (as do calls that took one of its faults and have yet to reach
// it). Unlike Release, which leaves the faults armed (each still takes its matching calls,
// letting them pass), later calls no longer meet the barrier, so a fault armed afterwards on
// the same calls takes effect (hitapi's POST /_test/barrier/release, whose barriers have no
// count). A name with no barrier is left without one: DisarmBarrier never adds a name, so the
// route cannot grow the names past maxBarriers.
func (w *StoreWrapper) DisarmBarrier(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.faults = slices.DeleteFunc(w.faults, func(a *armed) bool {
		return a.f.Mode == Barrier && a.f.Name == name
	})
	if b, ok := w.barriers[name]; ok {
		b.open()
	}
}

// NoScripts returns how many responses came back with NoScript set.
func (w *StoreWrapper) NoScripts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.noScript
}

// NotReadyRefunds returns how many refund responses the store refused with LOADING or BUSY:
// written, so the send observer heard bytes, but refused before running, so not sent and not
// an attempt under API-1d (DESIGN §A5.4, §C4). A response counts when the error reply in its
// StoreError is one by the adapter's first-word rule (store.NotReadyReply), whatever its
// outcome: unavailable for the first EVALSHA, outcome_unknown for the resend after a NOSCRIPT
// reload.
func (w *StoreWrapper) NotReadyRefunds() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.notReady
}

func newBarrier() *barrier {
	return &barrier{reached: make(chan struct{}), release: make(chan struct{})}
}

// barrierLocked returns the barrier name, made when missing; w.mu is held.
func (w *StoreWrapper) barrierLocked(name string) *barrier {
	b, ok := w.barriers[name]
	if !ok {
		b = newBarrier()
		w.barriers[name] = b
	}
	return b
}

// open releases the barrier once.
func (b *barrier) open() {
	if !b.isReleased {
		b.isReleased = true
		close(b.release)
	}
}

// take returns the first armed fault matching name and enc (the call's budget key, encoded;
// "" for a call without one, keyOf) and uses up one of its calls, or nil. A Barrier fault it
// returns counts its call as on its way to the barrier (pending) until the call reaches it
// (arrive).
func (w *StoreWrapper) take(name, enc string) *Fault {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropEndedLocked(time.Now())
	for i, a := range w.faults {
		if a.f.Match != "" && !strings.EqualFold(a.f.Match, name) {
			continue
		}
		if a.f.Key != "" && (enc == "" || enc != a.enc) {
			continue
		}
		if a.f.Count > 0 {
			if a.left--; a.left == 0 {
				w.faults = append(w.faults[:i], w.faults[i+1:]...)
			}
		}
		if a.f.Mode == Barrier {
			w.pending[a.f.Name]++
		}
		f := a.f
		return &f
	}
	return nil
}

// saw counts r, the inner adapter's response to a call of script: NoScript set, and a refund
// refused with LOADING or BUSY.
func (w *StoreWrapper) saw(script string, r store.Response) {
	notReady := script == scripts.Refund.Name && store.NotReadyReply(r.StoreError)
	if !r.NoScript && !notReady {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.NoScript {
		w.noScript++
	}
	if notReady {
		w.notReady++
	}
}

// arrive marks the barrier name reached by a call that took one of its faults (take), so that
// call is no longer on its way to it, and returns the barrier's release channel.
func (w *StoreWrapper) arrive(name string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := w.pending[name]; n > 1 {
		w.pending[name] = n - 1
	} else {
		delete(w.pending, name)
	}
	b := w.barrierLocked(name)
	if !b.isReached {
		b.isReached = true
		close(b.reached)
	}
	return b.release
}

// hold blocks until ch closes or ctx ends.
func hold(ctx context.Context, ch <-chan struct{}) {
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// sleep waits d or until ctx ends; it reports whether the wait ran to its end (false: ctx
// ended first).
func sleep(ctx context.Context, d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// delaysAfter reports whether f is a Delay fault that waits after the reply.
func delaysAfter(f *Fault) bool { return f != nil && f.Mode == Delay && f.At == AfterReply }

// wrapped is the adapter Wrap returns.
type wrapped struct {
	w    *StoreWrapper
	next store.Adapter
}

// dropped is what DropReply returns for a call that was sent: its reply is lost.
func dropped(r store.Response) store.Response {
	if !r.Sent {
		return r // nothing was sent, so no reply was lost
	}
	return store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: r.SentCommands, NoScript: r.NoScript}
}

// before runs the part of fault f that comes before the send. It reports stop when the call
// must not reach the store adapter, with the outcome to return: unavailable for Refuse, and
// cancelled for a before_send Delay whose ctx ended before its wait did (plan H4 step 6). A
// before_send Barrier holds the call until its release or the end of ctx, then lets it go on.
func (a *wrapped) before(ctx context.Context, f *Fault) (out store.Outcome, stop bool) {
	if f == nil {
		return store.OK, false
	}
	switch f.Mode {
	case Refuse:
		return store.Unavailable, true
	case Delay:
		if f.At != AfterReply && !sleep(ctx, f.Delay) {
			return store.Cancelled, true
		}
	case Barrier:
		if f.At == BeforeSend {
			hold(ctx, a.w.arrive(f.Name))
		}
	case DropReply:
	}
	return store.OK, false
}

// after runs the part of fault f that comes after the reply: an after_reply Barrier holds the
// return until its release, and an after_reply Delay waits Delay; either ends with ctx.
func (a *wrapped) after(ctx context.Context, f *Fault) {
	switch {
	case f == nil:
	case f.Mode == Barrier && f.At == AfterReply:
		hold(ctx, a.w.arrive(f.Name))
	case delaysAfter(f):
		sleep(ctx, f.Delay)
	}
}

// keyOf is the budget key a request is for, encoded: ARGV 3 (§A4.1) of an acquire, refund or
// cleanup_entry, the scripts whose third argument is an enc; "" for every other request.
func keyOf(req store.Request) string {
	switch req.Script.Name {
	case scripts.Acquire.Name, scripts.Refund.Name, scripts.CleanupEntry.Name:
		if len(req.Args) > 2 {
			return req.Args[2]
		}
	}
	return ""
}

// Call applies the first armed fault matching the script's name (and key, keyOf).
func (a *wrapped) Call(ctx context.Context, req store.Request) store.Response {
	f := a.w.take(req.Script.Name, keyOf(req))
	if out, stop := a.before(ctx, f); stop {
		return store.Response{Outcome: out}
	}
	r := a.next.Call(ctx, req)
	a.w.saw(req.Script.Name, r)
	a.after(ctx, f)
	if f != nil && f.Mode == DropReply {
		return dropped(r)
	}
	return r
}

// CallBatch applies faults per request (IMPLEMENTATION_PLAN S7 step 11): each request takes
// the first armed fault matching its script (and key, keyOf). A refused request is not sent
// (unavailable). The requests with no fault, a drop_reply, an after_reply barrier or an
// after_reply delay go out together as one inner CallBatch, in their order. The wait of each
// before_send delayed request starts at the batch's entry, so it runs while those go out; then
// the delayed requests go out, each once its Fault.Delay has passed since the entry, and those
// whose ctx ends first are cancelled without being sent (sendDelayed; plan H4 step 6). Then
// each request held at a before_send barrier is sent alone, in order, once released. Once every
// reply is in, the batch waits the longest after_reply delay of its requests (or until ctx
// ends), each after_reply barrier holds the return, and a drop_reply request's reply is
// discarded. The responses keep the batch's order.
func (a *wrapped) CallBatch(ctx context.Context, reqs []store.Request) []store.Response {
	entry := time.Now() // where every delayed request's wait starts
	faults := make([]*Fault, len(reqs))
	for i, req := range reqs {
		faults[i] = a.w.take(req.Script.Name, keyOf(req))
	}
	out := make([]store.Response, len(reqs))
	var together, delayed, held []int // request indexes
	var longest time.Duration         // the longest after_reply delay
	for i, f := range faults {
		switch {
		case f == nil:
			together = append(together, i)
		case f.Mode == Refuse:
			out[i] = store.Response{Outcome: store.Unavailable}
		case delaysAfter(f):
			together = append(together, i)
			longest = max(longest, f.Delay)
		case f.Mode == Delay:
			delayed = append(delayed, i)
		case f.Mode == Barrier && f.At == BeforeSend:
			held = append(held, i)
		default: // drop_reply, or an after_reply barrier
			together = append(together, i)
		}
	}
	a.send(ctx, reqs, out, together)
	a.sendDelayed(ctx, entry, reqs, faults, out, delayed)
	for _, i := range held {
		hold(ctx, a.w.arrive(faults[i].Name))
		a.send(ctx, reqs, out, []int{i})
	}
	if longest > 0 {
		sleep(ctx, longest)
	}
	for i, f := range faults {
		if f != nil && f.Mode == Barrier && f.At == AfterReply {
			hold(ctx, a.w.arrive(f.Name))
		}
		if f != nil && f.Mode == DropReply {
			out[i] = dropped(out[i])
		}
	}
	return out
}

// send sends the requests of reqs at the indexes idx, if any, as one inner CallBatch in idx
// order, and places their responses in out (place).
func (a *wrapped) send(ctx context.Context, reqs []store.Request, out []store.Response, idx []int) {
	if len(idx) == 0 {
		return
	}
	batch := make([]store.Request, len(idx))
	for j, i := range idx {
		batch[j] = reqs[i]
	}
	a.place(reqs, out, idx, a.next.CallBatch(ctx, batch))
}

// sendDelayed sends the before_send delayed requests of reqs at the indexes idx, each once
// its fault's Delay has passed since entry: it sleeps until the earliest end among the
// requests left, then sends every request whose wait is over as one inner CallBatch, in idx
// order, until none is left. Once ctx has ended, the requests whose waits are not over are
// cancelled, never sent (plan H4 step 6).
func (a *wrapped) sendDelayed(ctx context.Context, entry time.Time, reqs []store.Request, faults []*Fault, out []store.Response, idx []int) {
	for len(idx) > 0 {
		next := entry.Add(faults[idx[0]].Delay)
		for _, i := range idx[1:] {
			if end := entry.Add(faults[i].Delay); end.Before(next) {
				next = end
			}
		}
		sleep(ctx, time.Until(next))
		now := time.Now()
		var due, left []int
		for _, i := range idx {
			if !now.Before(entry.Add(faults[i].Delay)) {
				due = append(due, i)
			} else {
				left = append(left, i)
			}
		}
		a.send(ctx, reqs, out, due)
		if ctx.Err() != nil {
			for _, i := range left {
				out[i] = store.Response{Outcome: store.Cancelled}
			}
			return
		}
		idx = left
	}
}

// place counts the responses of an inner CallBatch (saw) and stores them at the indexes of
// the requests of reqs it sent: the inner adapter returns one response per request, in order.
func (a *wrapped) place(reqs []store.Request, out []store.Response, idx []int, rs []store.Response) {
	for j, r := range rs {
		if j < len(idx) {
			a.w.saw(reqs[idx[j]].Script.Name, r)
			out[idx[j]] = r
		}
	}
}

// Do applies the first armed fault matching the command (args[0]).
func (a *wrapped) Do(ctx context.Context, kind store.Kind, args ...string) (resp.Value, store.Response) {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	f := a.w.take(cmd, "")
	if out, stop := a.before(ctx, f); stop {
		return resp.Value{}, store.Response{Outcome: out}
	}
	v, r := a.next.Do(ctx, kind, args...)
	a.after(ctx, f)
	if f != nil && f.Mode == DropReply && r.Sent {
		return resp.Value{}, dropped(r)
	}
	return v, r
}

// ReadyConns passes through.
func (a *wrapped) ReadyConns(kind store.Kind) int { return a.next.ReadyConns(kind) }

// Close releases every barrier, so no call stays blocked, and closes the adapter.
func (a *wrapped) Close() {
	a.w.Disarm()
	a.next.Close()
}
