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
	// Delay waits Fault.Delay before sending (or until the call's ctx ends). In a CallBatch the
	// wait of every delayed request starts when the batch enters the wrapper and runs while the
	// rest of the batch goes ahead; then the delayed requests whose waits have passed go out
	// together.
	Delay Mode = "delay"
	// Barrier blocks before the send or after the reply until Release (§B6 Barriers). In a
	// CallBatch a request held before the send is sent alone after Release, while the rest of
	// the batch goes ahead.
	Barrier Mode = "barrier"
)

// At is where a barrier blocks.
type At string

// The barrier positions.
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
	// At and Name place a Barrier fault: Reached(Name) and Release(Name) address it.
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

// StoreWrapper stands between the limiter and its store adapter (DESIGN §A5.12, §B6) and
// applies the armed faults. It also counts, as the store adapter reported them, the responses
// that came back with NoScript set, for the driver's allowed-commands check, and the refund
// responses the store refused with LOADING or BUSY, for its refund-sends check (§C4). One
// wrapper serves one limiter; it is safe for concurrent use.
type StoreWrapper struct {
	mu       sync.Mutex
	faults   []*armed
	barriers map[string]*barrier
	noScript int
	notReady int // refund responses refused with LOADING or BUSY
}

// NewStoreWrapper returns a wrapper with no fault armed.
func NewStoreWrapper() *StoreWrapper {
	return &StoreWrapper{barriers: map[string]*barrier{}}
}

// Wrap returns inner behind the wrapper; limitertest.WithStoreWrapper takes it.
func (w *StoreWrapper) Wrap(inner store.Adapter) store.Adapter {
	return &wrapped{w: w, next: inner}
}

// Arm arms f. It fails on an unknown mode, a negative count or time, a barrier without a
// name or position, a delay without a wait, and a Key that is no valid budget key (API-1).
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
	case Delay:
		if f.Delay == 0 {
			return fmt.Errorf("fixtures: fault %q: no delay", f.Mode)
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
	if f.Mode == Barrier {
		if b, ok := w.barriers[f.Name]; !ok || b.isReleased {
			w.barriers[f.Name] = newBarrier()
		}
	}
	w.faults = append(w.faults, a)
	return nil
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
// count).
func (w *StoreWrapper) DisarmBarrier(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.faults = slices.DeleteFunc(w.faults, func(a *armed) bool {
		return a.f.Mode == Barrier && a.f.Name == name
	})
	w.barrierLocked(name).open()
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
// "" for a call without one, keyOf) and uses up one of its calls, or nil.
func (w *StoreWrapper) take(name, enc string) *Fault {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for i := 0; i < len(w.faults); i++ {
		a := w.faults[i]
		if !a.until.IsZero() && !now.Before(a.until) {
			w.faults = append(w.faults[:i], w.faults[i+1:]...)
			i--
			continue
		}
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

// arrive marks the barrier name reached and returns its release channel.
func (w *StoreWrapper) arrive(name string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
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

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
	case <-ctx.Done():
	}
}

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

// before runs the part of fault f that comes before the send; it reports false when the
// call must not reach the store (Refuse).
func (a *wrapped) before(ctx context.Context, f *Fault) bool {
	if f == nil {
		return true
	}
	switch f.Mode {
	case Refuse:
		return false
	case Delay, Barrier:
		a.wait(ctx, f)
	case DropReply:
	}
	return true
}

// wait holds a call that fault f delays (Delay) or stops before its send (a before_send
// Barrier) until the wait has passed, the barrier is released or ctx ends.
func (a *wrapped) wait(ctx context.Context, f *Fault) {
	switch {
	case f.Mode == Delay:
		sleep(ctx, f.Delay)
	case f.Mode == Barrier && f.At == BeforeSend:
		hold(ctx, a.w.arrive(f.Name))
	}
}

// after runs the part of fault f that comes after the reply.
func (a *wrapped) after(ctx context.Context, f *Fault) {
	if f != nil && f.Mode == Barrier && f.At == AfterReply {
		hold(ctx, a.w.arrive(f.Name))
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
	if !a.before(ctx, f) {
		return store.Response{Outcome: store.Unavailable}
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
// (unavailable). The requests with no fault, a drop_reply or an after_reply barrier go out
// together as one inner CallBatch, in their order. The wait of each delayed request starts at
// the batch's entry, so it runs while those go out; then the delayed requests go out, each
// once its Fault.Delay has passed since the entry (sendDelayed). Last, each request held at a
// before_send barrier is sent alone, in order, once released. A drop_reply request's reply is
// discarded, and each after_reply barrier holds the return once every reply is in. The
// responses keep the batch's order.
func (a *wrapped) CallBatch(ctx context.Context, reqs []store.Request) []store.Response {
	entry := time.Now() // where every delayed request's wait starts
	faults := make([]*Fault, len(reqs))
	for i, req := range reqs {
		faults[i] = a.w.take(req.Script.Name, keyOf(req))
	}
	out := make([]store.Response, len(reqs))
	var together, delayed, held []int // request indexes
	for i, f := range faults {
		switch {
		case f == nil:
			together = append(together, i)
		case f.Mode == Refuse:
			out[i] = store.Response{Outcome: store.Unavailable}
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
		a.wait(ctx, faults[i])
		a.send(ctx, reqs, out, []int{i})
	}
	for i, f := range faults {
		a.after(ctx, f)
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

// sendDelayed sends the delayed requests of reqs at the indexes idx, each once its fault's
// Delay has passed since entry, or at once when ctx has ended: it sleeps until the earliest
// end among the requests left, then sends every request whose wait is over as one inner
// CallBatch, in idx order, until none is left.
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
			if ctx.Err() != nil || !now.Before(entry.Add(faults[i].Delay)) {
				due = append(due, i)
			} else {
				left = append(left, i)
			}
		}
		a.send(ctx, reqs, out, due)
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
	if !a.before(ctx, f) {
		return resp.Value{}, store.Response{Outcome: store.Unavailable}
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
