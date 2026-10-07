package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"ratelimiter/internal/receipt"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// The lifecycle tests build limiters through newLimiter with a hookSet, so they run in the
// untagged build too. Their order in this file matters to bin/mutate --sweep, which runs
// without -shuffle and with -failfast: TestNewSignerWrapper builds no limiter, so a
// newSigner mutant fails it before a limiter without a signer wrapper reaches newSigner;
// TestCloseRefundsAndListener, the first limiter with a refund listener and a refund pending
// at Close, fails a mutant of Close's drain or listener stop, or of Health's ListenerDrops,
// before TestCloseStopsWatchThenAdapter's limiter without a listener, or its settings_invalid
// instance, can crash on it; and TestCloseStopsWatchThenAdapter fails a shutdown mutant that
// TestCloseRefundsAndListener misses before any later Close. effects_test.go's tests, which
// build no limiter, run before all of them (file name order).

// lifecycleWait bounds every wait of the lifecycle tests.
const lifecycleWait = 5 * time.Second

// lifecycleSettings are valid settings with the harness limits (DESIGN §A5.10), 4 slots of
// each kind and a 1 h re-read interval, so the epoch watch makes only its startup read.
func lifecycleSettings() (Policy, Instance) {
	p := settings.DefaultPolicy()
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 100_000, 50_000
	in := settings.DefaultInstance(p)
	in.MaxReleaseDelayNs, in.MarginNs = 5_000_000_000, 1_000_000_000
	in.MaxHeldReceipts, in.MaxPendingRefunds = 4, 4
	in.EpochRereadIntervalNs = 3_600_000_000_000
	return p, in
}

// stubAdapter is the store adapter of the lifecycle tests (hookSet.storeWrapper): every
// request is unavailable and sends nothing. It keeps the Done channel of the first call's
// context, which is the instance lifetime context (the epoch watch's startup read), and
// counts Close calls, noting whether that context had ended when the first one came.
type stubAdapter struct {
	firstCall chan struct{} // closed by the first call

	mu        sync.Mutex
	calls     int
	lifeDone  <-chan struct{} // the first call's ctx.Done()
	closes    int
	lifeEnded bool // the first call's context had ended when Close first came
}

func newStubAdapter() *stubAdapter { return &stubAdapter{firstCall: make(chan struct{})} }

// wrap is the store wrapper: it closes the real adapter, which newLimiter holds back from
// dialing until the wrapper returns, and puts the stub in its place.
func (a *stubAdapter) wrap(inner store.Adapter) store.Adapter {
	inner.Close()
	return a
}

func (a *stubAdapter) Call(ctx context.Context, _ store.Request) store.Response {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.calls == 1 {
		a.lifeDone = ctx.Done()
		close(a.firstCall)
	}
	return store.Response{Outcome: store.Unavailable}
}

func (a *stubAdapter) CallBatch(_ context.Context, reqs []store.Request) []store.Response {
	out := make([]store.Response, len(reqs))
	for i := range out {
		out[i] = store.Response{Outcome: store.Unavailable}
	}
	return out
}

func (a *stubAdapter) Do(context.Context, store.Kind, ...string) (resp.Value, store.Response) {
	return resp.Value{}, store.Response{Outcome: store.Unavailable}
}

func (a *stubAdapter) ReadyConns(store.Kind) int { return 0 }

func (a *stubAdapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closes++
	if a.closes == 1 && a.lifeDone != nil {
		select {
		case <-a.lifeDone:
			a.lifeEnded = true
		default:
		}
	}
}

// closeState returns how often Close came and whether the lifetime context had ended by
// the first.
func (a *stubAdapter) closeState() (closes int, lifeEnded bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closes, a.lifeEnded
}

// closeWithin calls l.Close and fails the test unless it returns within d. When it does not,
// the test ends the lifetime context itself, so the stuck Close can finish and leave no
// goroutine behind, then fails.
func closeWithin(t *testing.T, l *Limiter, d time.Duration) {
	t.Helper()
	timer := time.NewTimer(d) // started before Close, so the bound covers all of it
	defer timer.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Close()
	}()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	if l.stop != nil {
		l.stop()
	}
	select {
	case <-done:
		t.Fatalf("Close did not return within %v; it returned once the test ended the lifetime context", d)
	case <-time.After(d):
		t.Fatalf("Close did not return within %v, nor within %v after the test ended the lifetime context", d, d)
	}
}

// wrappedSigner is what the signer wrapper of TestNewSignerWrapper returns.
type wrappedSigner struct{ receipt.Signer }

// TestNewSignerWrapper: newSigner makes the HMAC signer from the key reader's 32 bytes
// (RCP-4). A signer wrapper, when set, gets that signer once and newSigner returns what the
// wrapper returns (DESIGN §A5.12); a wrapper that returns nil, or a key reader that fails, is
// an error and no signer. With no wrapper, the HMAC signer itself is returned.
func TestNewSignerWrapper(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	rc := receipt.New([16]byte{1, 2, 3}, "aw", 1, 2, 1_760_000_000_000_000, 60_000_000)
	mac := hmac.New(sha256.New, key)
	mac.Write(receipt.MACInput(rc))
	wantMAC := mac.Sum(nil)
	signsWithKey := func(s receipt.Signer) bool {
		got, err := s.Sign(rc)
		return err == nil && bytes.Equal(got.MAC[:], wantMAC)
	}

	// First, with a wrapper: nothing here calls a nil wrapper.
	var gave []receipt.Signer
	var made *wrappedSigner
	s, err := newSigner(bytes.NewReader(key), func(in receipt.Signer) receipt.Signer {
		gave = append(gave, in)
		made = &wrappedSigner{in}
		return made
	})
	if err != nil || len(gave) != 1 || made == nil || s != receipt.Signer(made) {
		t.Fatalf("with a signer wrapper: err %v, wrapper called %d times, the wrapper's signer returned %v; want no error, once, true",
			err, len(gave), made != nil && s == receipt.Signer(made))
	}
	if !gave[0].Usable() || !signsWithKey(gave[0]) {
		t.Fatal("the signer wrapper got a signer that is not usable or does not sign with the key reader's bytes")
	}
	if s, err := newSigner(bytes.NewReader(key), func(receipt.Signer) receipt.Signer { return nil }); err == nil || s != nil {
		t.Fatalf("a signer wrapper that returns nil: a signer %v, err %v; want no signer and an error", s != nil, err)
	}
	errNoKey := errors.New("test: no key bytes")
	calls := 0
	counting := func(in receipt.Signer) receipt.Signer {
		calls++
		return in
	}
	if s, err := newSigner(iotest.ErrReader(errNoKey), counting); !errors.Is(err, errNoKey) || s != nil || calls != 0 {
		t.Fatalf("a failing key reader: a signer %v, err %v, wrapper called %d times; want no signer, the reader's error, no call", s != nil, err, calls)
	}

	// Then without a wrapper.
	s, err = newSigner(bytes.NewReader(key), nil)
	if err != nil {
		t.Fatalf("no wrapper: %v", err)
	}
	if _, ok := s.(*receipt.HMACSigner); !ok || !signsWithKey(s) {
		t.Fatalf("no wrapper: got a %T, want the HMAC signer over the key reader's bytes", s)
	}
	if s, err := newSigner(iotest.ErrReader(errNoKey), nil); !errors.Is(err, errNoKey) || s != nil {
		t.Fatalf("no wrapper, a failing key reader: a signer %v, err %v; want no signer, the reader's error", s != nil, err)
	}
}

// TestCloseRefundsAndListener (plan S6 step 7; SPEC API-1d, API-1e; DESIGN §A5.2, §A5.11):
// Close on a running instance with a refund listener and one refund pending that cannot end
// (the instance never joins an epoch, so each attempt sends nothing and is retried) returns no
// earlier than its 100 ms drain timeout and within 5 s, the refund ended unresolved (closed),
// and the listener's dispatcher stopped: once it has returned, the refund's event is delivered
// or, the drain deadline having passed, dropped and counted. A release() after Close ends
// unresolved (closed), and its event is dropped and counted, never delivered; Health's
// ListenerDrops is the listener's count, and every slot is free.
func TestCloseRefundsAndListener(t *testing.T) {
	p, in := lifecycleSettings()
	in.DrainTimeoutNs = 100_000_000
	sink := &eventSink{}
	l, err := newLimiter(p, in, StoreOptions{}, hookSet{storeWrapper: newStubAdapter().wrap}, []Option{WithRefundListener(sink.listen)})
	t.Cleanup(func() { l.Close() })
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	// Two allowed permits as Acquire makes them: a signed receipt and one slot of each kind.
	permit := func(s uint64) *Permit {
		t.Helper()
		rc, err := l.signer.Sign(receipt.New([16]byte{1}, "aw", 1, s, 1_760_000_000_000_000, 60_000_000))
		if err != nil || !l.slots.tryAcquire() {
			t.Fatalf("permit %d: signing error %v, or no free slot", s, err)
		}
		return newPermit(l.slots, l.refunds, rc, receipt.DeductionID(rc), time.Now())
	}
	pending, late := permit(1), permit(2)
	pending.Release()

	start := time.Now()
	closeWithin(t, l, lifecycleWait)
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("Close returned after %v with a refund pending, before its 100 ms drain timeout", d)
	}
	if st := pending.life.load(); st != stateFinal {
		t.Fatalf("the pending refund's permit is %v after Close, want FINAL: the drain's end ends it unresolved (closed)", st)
	}
	select {
	case <-l.lis.exited:
	case <-time.After(lifecycleWait):
		t.Fatalf("the listener's dispatcher had not returned %v after Close", lifecycleWait)
	}
	evs, drops := sink.untimed(t), l.lis.drops.Load()
	if uint64(len(evs))+drops != 1 {
		t.Fatalf("%d event(s) delivered and %d dropped, want 1 in all: the drain's end emits one", len(evs), drops)
	}
	if want := (RefundEvent{DeductionID: pending.DeductionID(), Result: Unresolved, Reason: UnresolvedClosed}); len(evs) == 1 && evs[0] != want {
		t.Fatalf("event %+v, want %+v (0 attempts: nothing was sent)", evs[0], want)
	}
	if h := l.Health(); h.ListenerDrops != drops {
		t.Fatalf("Health().ListenerDrops %d, want the listener's %d", h.ListenerDrops, drops)
	}

	late.Release()
	if st := late.life.load(); st != stateFinal {
		t.Fatalf("a release() after Close left the permit %v, want FINAL (unresolved, closed)", st)
	}
	if n, d := len(sink.untimed(t)), l.lis.drops.Load(); n != len(evs) || d != drops+1 {
		t.Fatalf("after a release() after Close: %d event(s) delivered and %d dropped, want %d and %d: its event is dropped and counted, never delivered",
			n, d, len(evs), drops+1)
	}
	if h := l.Health(); h.ListenerDrops != drops+1 {
		t.Fatalf("Health().ListenerDrops %d after the dropped event, want %d", h.ListenerDrops, drops+1)
	}
	if u := gaugeValue(t, l.Metrics(), "rl_refund_results_total", "unresolved"); u != 2 {
		t.Fatalf("rl_refund_results_total{unresolved} %v, want 2", u)
	}
	if rs, fs := l.slots.inUse(); rs != 0 || fs != 0 {
		t.Fatalf("slots in use %d and %d after both permits ended, want 0 and 0", rs, fs)
	}
}

// TestCloseStopsWatchThenAdapter: Close on a running instance ends the lifetime context,
// waits for the epoch watch, then closes the store adapter once, and returns within 5 s;
// Health then reports not_ready (closing), and a second Close returns and closes nothing
// more. A settings_invalid instance has no watch and no adapter: Close returns there too, and
// the instance stays settings_invalid (DESIGN §A5.2, plan S5 steps 2 and 8).
func TestCloseStopsWatchThenAdapter(t *testing.T) {
	p, in := lifecycleSettings()
	a := newStubAdapter()
	l, err := newLimiter(p, in, StoreOptions{}, hookSet{storeWrapper: a.wrap}, nil)
	t.Cleanup(func() { l.Close() })
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	select {
	case <-a.firstCall:
	case <-time.After(lifecycleWait):
		t.Fatalf("no startup read_epoch within %v", lifecycleWait)
	}
	closeWithin(t, l, lifecycleWait)
	if closes, ended := a.closeState(); closes != 1 || !ended {
		t.Fatalf("Close closed the store adapter %d times, the lifetime context ended first: %v; want once, true", closes, ended)
	}
	if h := l.Health(); h.Status != statusNotReady || h.Reason != reasonClosing {
		t.Errorf("health after Close: %s/%s, want %s/%s", h.Status, h.Reason, statusNotReady, reasonClosing)
	}
	closeWithin(t, l, lifecycleWait)
	if closes, _ := a.closeState(); closes != 1 {
		t.Errorf("a second Close: the store adapter closed %d times in all, want 1", closes)
	}

	in.MarginNs = 59_000_000 // RCP-8: 59 ms × 10^6 < 60 s × 1,000 ppm
	inv, err := New(p, in, StoreOptions{})
	t.Cleanup(func() { inv.Close() })
	if err == nil {
		t.Fatal("settings that fail RCP-8 were accepted")
	}
	closeWithin(t, inv, lifecycleWait)
	closeWithin(t, inv, lifecycleWait)
	if h := inv.Health(); h.Status != statusNotReady || h.Reason != reasonSettingsInvalid {
		t.Errorf("a settings_invalid instance after Close: health %s/%s, want %s/%s", h.Status, h.Reason, statusNotReady, reasonSettingsInvalid)
	}
}

// TestNewAppliesOptions: newLimiter, the body of New and NewWithHooks, applies each non-nil
// Option once, in the order given, and skips nil ones (DESIGN §A5.2).
func TestNewAppliesOptions(t *testing.T) {
	p, in := lifecycleSettings()
	var applied []string
	opt := func(name string) Option { return func(*options) { applied = append(applied, name) } }
	l, err := newLimiter(p, in, StoreOptions{}, hookSet{storeWrapper: newStubAdapter().wrap}, []Option{opt("a")})
	t.Cleanup(func() { l.Close() })
	if err != nil || !slices.Equal(applied, []string{"a"}) {
		t.Fatalf("one Option: err %v, applied %q; want no error, [a]", err, applied)
	}
	applied = nil
	l2, err := newLimiter(p, in, StoreOptions{}, hookSet{storeWrapper: newStubAdapter().wrap}, []Option{nil, opt("b"), nil, opt("c"), nil})
	t.Cleanup(func() { l2.Close() })
	if err != nil || !slices.Equal(applied, []string{"b", "c"}) {
		t.Fatalf("Options with nil ones between: err %v, applied %q; want no error, [b c]", err, applied)
	}
}
