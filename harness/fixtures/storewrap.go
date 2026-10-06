//go:build rltest

package fixtures

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
	// delayed request is sent alone after the wait, while the rest of the batch goes ahead.
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
	// Match is the script name (Request.Script.Name, e.g. "acquire") or, for Do, the command
	// (its first argument, e.g. "SCAN"), compared case-insensitively; "" matches every call.
	Match string
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
// applies the armed faults. It also counts the responses that came back with NoScript set,
// as the store adapter reported them, for the driver's allowed-commands check (§C4). One
// wrapper serves one limiter; it is safe for concurrent use.
type StoreWrapper struct {
	mu       sync.Mutex
	faults   []*armed
	barriers map[string]*barrier
	noScript int
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
// name or position, and a delay without a wait.
func (w *StoreWrapper) Arm(f Fault) error {
	if f.Count < 0 || f.Duration < 0 || f.Delay < 0 {
		return fmt.Errorf("fixtures: fault %q: negative count or time", f.Mode)
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
	a := &armed{f: f, left: f.Count}
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

// NoScripts returns how many responses came back with NoScript set.
func (w *StoreWrapper) NoScripts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.noScript
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

// take returns the first armed fault matching name and uses up one of its calls, or nil.
func (w *StoreWrapper) take(name string) *Fault {
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

// saw counts the NoScript responses among rs.
func (w *StoreWrapper) saw(rs ...store.Response) {
	n := 0
	for _, r := range rs {
		if r.NoScript {
			n++
		}
	}
	if n == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.noScript += n
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

// Call applies the first armed fault matching the script's name.
func (a *wrapped) Call(ctx context.Context, req store.Request) store.Response {
	f := a.w.take(req.Script.Name)
	if !a.before(ctx, f) {
		return store.Response{Outcome: store.Unavailable}
	}
	r := a.next.Call(ctx, req)
	a.w.saw(r)
	a.after(ctx, f)
	if f != nil && f.Mode == DropReply {
		return dropped(r)
	}
	return r
}

// CallBatch applies faults per request (IMPLEMENTATION_PLAN S7 step 11): each request takes
// the first armed fault matching its script. A refused request is not sent (unavailable). The
// requests with no fault, a drop_reply or an after_reply barrier go out together as one inner
// CallBatch, in their order; then each request held at a before_send barrier or delayed is
// sent alone, in order, once released or after its wait. A drop_reply request's reply is
// discarded, and each after_reply barrier holds the return once every reply is in. The
// responses keep the batch's order.
func (a *wrapped) CallBatch(ctx context.Context, reqs []store.Request) []store.Response {
	faults := make([]*Fault, len(reqs))
	for i, req := range reqs {
		faults[i] = a.w.take(req.Script.Name)
	}
	out := make([]store.Response, len(reqs))
	var together, alone []int // request indexes
	for i, f := range faults {
		switch {
		case f == nil:
			together = append(together, i)
		case f.Mode == Refuse:
			out[i] = store.Response{Outcome: store.Unavailable}
		case f.Mode == Delay || (f.Mode == Barrier && f.At == BeforeSend):
			alone = append(alone, i)
		default: // drop_reply, or an after_reply barrier
			together = append(together, i)
		}
	}
	if len(together) > 0 {
		batch := make([]store.Request, len(together))
		for j, i := range together {
			batch[j] = reqs[i]
		}
		a.place(out, together, a.next.CallBatch(ctx, batch))
	}
	for _, i := range alone {
		a.wait(ctx, faults[i])
		a.place(out, []int{i}, a.next.CallBatch(ctx, reqs[i:i+1]))
	}
	for i, f := range faults {
		a.after(ctx, f)
		if f != nil && f.Mode == DropReply {
			out[i] = dropped(out[i])
		}
	}
	return out
}

// place counts the NoScript responses of an inner CallBatch and stores them at the indexes of
// the requests it sent: the inner adapter returns one response per request, in order.
func (a *wrapped) place(out []store.Response, idx []int, rs []store.Response) {
	a.w.saw(rs...)
	for j, r := range rs {
		if j < len(idx) {
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
	f := a.w.take(cmd)
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
