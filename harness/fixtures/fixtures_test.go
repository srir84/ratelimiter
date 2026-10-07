//go:build rltest

package fixtures_test

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// sends records what a store adapter's send observer hears.
type sends struct {
	mu   sync.Mutex
	recs []send
}

type send struct {
	cmd   string
	bytes int
	at    time.Time // when the observer heard it
}

func (s *sends) observe(cmd string, n int) {
	at := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, send{cmd, n, at})
}

// since returns the records after the first n.
func (s *sends) since(n int) []send {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.recs[n:])
}

func (s *sends) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

// written reports whether recs hold a record of cmd with bytes written.
func written(recs []send, cmd string) bool {
	return slices.ContainsFunc(recs, func(r send) bool { return r.cmd == cmd && r.bytes > 0 })
}

// wrapEnv is a fresh store, a real store adapter as rl_lib with a send observer, and a
// store wrapper in front of it.
type wrapEnv struct {
	w    *fixtures.StoreWrapper
	a    store.Adapter
	sent *sends
}

func newWrapEnv(t *testing.T) *wrapEnv {
	t.Helper()
	st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
	p := settings.DefaultPolicy()
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 1000, 1000
	e := &wrapEnv{w: fixtures.NewStoreWrapper(), sent: &sends{}}
	inner := store.New(t.Context(), store.Options{
		Addr: st.Addr, User: "rl_lib", Password: st.PW.Lib,
		StoreConns: 1, RefundConns: 1, DependencyDeadline: 2 * time.Second,
		Limits: store.LimitsFor(p), SendObserver: e.sent.observe,
	})
	e.a = e.w.Wrap(inner)
	t.Cleanup(e.a.Close)
	// Every connection authenticated, so later records belong to the calls alone.
	deadline := time.Now().Add(5 * time.Second)
	for inner.ReadyConns(store.Call) < 1 || inner.ReadyConns(store.Refund) < 1 || inner.ReadyConns(store.Maintenance) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the adapter's connections were not ready within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return e
}

// readEpoch is a read_epoch call: on a store without an epoch it gets a decodable reply.
func readEpoch() store.Request {
	return store.Request{
		Kind: store.Call, Script: scripts.ReadEpoch,
		Keys: []string{keys.EpochKey, keys.CtlTimeKey, keys.QuarantinePlaceholder},
	}
}

func (e *wrapEnv) arm(t *testing.T, f fixtures.Fault) {
	t.Helper()
	if err := e.w.Arm(f); err != nil {
		t.Fatal(err)
	}
}

// holdReq is a hold call for an epoch the store does not have: hold refuses with
// epoch_changed and writes nothing.
func holdReq() store.Request {
	return store.Request{
		Kind: store.Call, Script: scripts.Hold,
		Keys: []string{keys.EpochKey}, Args: []string{"0123456789abcdef0123456789abcdef", "store_rollback"},
	}
}

// warm loads read_epoch and hold into the store: Call reloads a script after NOSCRIPT, while
// CallBatch does not.
func (e *wrapEnv) warm(t *testing.T) {
	t.Helper()
	for _, req := range []store.Request{readEpoch(), holdReq()} {
		if r := e.a.Call(t.Context(), req); r.Outcome != store.OK {
			t.Fatalf("loading %s: outcome %v", req.Script.Name, r.Outcome)
		}
	}
}

// first returns when the first record of cmd with bytes written was heard, or the zero time.
func first(recs []send, cmd string) time.Time {
	for _, r := range recs {
		if r.cmd == cmd && r.bytes > 0 {
			return r.at
		}
	}
	return time.Time{}
}

// TestClockRejectsNonMicro (DESIGN §B6; plan S5 step 9): the clock takes ns and writes
// rl:ctl:time in µs as rl_fixture. It refuses a negative value or step and one that is not a
// whole µs, writing nothing and keeping its reading; both sides of each bound are pinned (0
// and −1,000; 1,000 and 999 or 1,001; back to exactly 0 and 1 µs past it).
func TestClockRejectsNonMicro(t *testing.T) {
	st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
	conn, err := st.Conn("rl_fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	read, err := st.Conn("rl_fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	var heard []int64
	c := fixtures.NewClock(conn, func(ns int64) { heard = append(heard, ns) })
	stored := func() string {
		t.Helper()
		r, err := read.Do("GET", keys.CtlTimeKey)
		if err != nil {
			t.Fatal(err)
		}
		if r.Nil {
			return "<nil>"
		}
		return r.Str
	}

	if err := c.Advance(1000); !errors.Is(err, fixtures.ErrUnset) {
		t.Errorf("Advance before Set: %v, want ErrUnset", err)
	}
	if err := c.Back(1000); !errors.Is(err, fixtures.ErrUnset) {
		t.Errorf("Back before Set: %v, want ErrUnset", err)
	}
	for _, ns := range []int64{-1000, -1, 1_760_000_000_000_000_500, 999, 1001} {
		want := fixtures.ErrNotMicro
		if ns < 0 {
			want = fixtures.ErrNegative
		}
		if err := c.Set(ns); !errors.Is(err, want) {
			t.Errorf("Set(%d): %v, want %v", ns, err, want)
		}
	}
	if got := stored(); got != "<nil>" {
		t.Fatalf("refused Sets wrote rl:ctl:time = %s", got)
	}

	steps := []struct {
		name string
		do   func() error
		want error  // nil: accepted
		now  int64  // NowNs afterwards
		raw  string // rl:ctl:time afterwards (µs)
	}{
		{"set 0", func() error { return c.Set(0) }, nil, 0, "0"},
		{"set 1000", func() error { return c.Set(1000) }, nil, 1000, "1"},
		{"set T", func() error { return c.Set(1_760_000_000_000_001_000) }, nil, 1_760_000_000_000_001_000, "1760000000000001"},
		{"advance 1", func() error { return c.Advance(1) }, fixtures.ErrNotMicro, 1_760_000_000_000_001_000, "1760000000000001"},
		{"advance 999", func() error { return c.Advance(999) }, fixtures.ErrNotMicro, 1_760_000_000_000_001_000, "1760000000000001"},
		{"advance -1000", func() error { return c.Advance(-1000) }, fixtures.ErrNegative, 1_760_000_000_000_001_000, "1760000000000001"},
		{"advance 1000", func() error { return c.Advance(1000) }, nil, 1_760_000_000_000_002_000, "1760000000000002"},
		{"advance 0", func() error { return c.Advance(0) }, nil, 1_760_000_000_000_002_000, "1760000000000002"},
		{"back 1500", func() error { return c.Back(1500) }, fixtures.ErrNotMicro, 1_760_000_000_000_002_000, "1760000000000002"},
		{"back -1000", func() error { return c.Back(-1000) }, fixtures.ErrNegative, 1_760_000_000_000_002_000, "1760000000000002"},
		{"back 2000", func() error { return c.Back(2000) }, nil, 1_760_000_000_000_000_000, "1760000000000000"},
		{"back past 0", func() error { return c.Back(1_760_000_000_000_001_000) }, fixtures.ErrNegative, 1_760_000_000_000_000_000, "1760000000000000"},
		{"back to 0", func() error { return c.Back(1_760_000_000_000_000_000) }, nil, 0, "0"},
		{"set max µs", func() error { return c.Set(9_223_372_036_854_775_000) }, nil, 9_223_372_036_854_775_000, "9223372036854775"},
		{"advance past int64", func() error { return c.Advance(1000) }, fixtures.ErrOverflow, 9_223_372_036_854_775_000, "9223372036854775"},
	}
	var accepted []int64
	for _, s := range steps {
		err := s.do()
		if (s.want == nil && err != nil) || (s.want != nil && !errors.Is(err, s.want)) {
			t.Errorf("%s: %v, want %v", s.name, err, s.want)
		}
		if s.want == nil {
			accepted = append(accepted, s.now)
		}
		if got := c.NowNs(); got != s.now {
			t.Errorf("%s: NowNs %d, want %d", s.name, got, s.now)
		}
		if got := stored(); got != s.raw {
			t.Errorf("%s: rl:ctl:time %s, want %s", s.name, got, s.raw)
		}
	}
	if !slices.Equal(heard, accepted) {
		t.Errorf("onSet heard %v, want the accepted values %v", heard, accepted)
	}
}

// TestRefuseSendsNothing (DESIGN §B6 FLT-1 b): a refused call is unavailable and writes no
// byte; the fault covers exactly Count matching calls and only its script.
func TestRefuseSendsNothing(t *testing.T) {
	e := newWrapEnv(t)
	e.arm(t, fixtures.Fault{Mode: fixtures.Refuse, Match: "read_epoch", Count: 1})
	n := e.sent.len()
	r := e.a.Call(t.Context(), readEpoch())
	if r.Outcome != store.Unavailable || r.Sent || r.SentCommands != 0 {
		t.Errorf("refused call: outcome %v, sent %v (%d), want unavailable, nothing sent", r.Outcome, r.Sent, r.SentCommands)
	}
	if recs := e.sent.since(n); len(recs) != 0 {
		t.Errorf("a refused call wrote %v, want 0 bytes", recs)
	}
	// Count 1 is used up: the next call reaches the store.
	n = e.sent.len()
	if r := e.a.Call(t.Context(), readEpoch()); r.Outcome != store.OK || !r.Sent {
		t.Errorf("the call after the refusal: outcome %v, sent %v, want ok", r.Outcome, r.Sent)
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") {
		t.Errorf("the call after the refusal wrote %v, want read_epoch bytes", recs)
	}
	// A refusal armed for another script leaves this one alone.
	e.arm(t, fixtures.Fault{Mode: fixtures.Refuse, Match: "acquire"})
	n = e.sent.len()
	if r := e.a.Call(t.Context(), readEpoch()); r.Outcome != store.OK {
		t.Errorf("read_epoch with acquire refused: outcome %v, want ok", r.Outcome)
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") {
		t.Errorf("read_epoch with acquire refused wrote %v, want read_epoch bytes", recs)
	}
}

// TestDropReplySends (DESIGN §B6 FLT-3): the call is written (bytes > 0) and its reply
// discarded, so it ends outcome_unknown; the next call gets its reply.
func TestDropReplySends(t *testing.T) {
	e := newWrapEnv(t)
	e.arm(t, fixtures.Fault{Mode: fixtures.DropReply, Match: "read_epoch", Count: 1})
	n := e.sent.len()
	r := e.a.Call(t.Context(), readEpoch())
	if r.Outcome != store.OutcomeUnknown || !r.Sent {
		t.Errorf("dropped reply: outcome %v, sent %v, want outcome_unknown, sent", r.Outcome, r.Sent)
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") {
		t.Errorf("a dropped-reply call wrote %v, want read_epoch bytes", recs)
	}
	if r := e.a.Call(t.Context(), readEpoch()); r.Outcome != store.OK || r.Reply.Code != 22 || r.Reply.Reason != "epoch_record_missing" {
		t.Errorf("the next call: outcome %v, reply %d %q, want ok 22 epoch_record_missing", r.Outcome, r.Reply.Code, r.Reply.Reason)
	}
}

// TestBarrierBlocks (DESIGN §B6 Barriers): a before_send barrier holds the call before any
// byte is written until Release; an after_reply barrier holds it after the reply came back;
// a held call whose ctx ends returns without sending.
func TestBarrierBlocks(t *testing.T) {
	e := newWrapEnv(t)
	call := func(ctx context.Context) <-chan store.Response {
		ch := make(chan store.Response, 1)
		go func() { ch <- e.a.Call(ctx, readEpoch()) }()
		return ch
	}
	waitReached := func(name string) {
		t.Helper()
		select {
		case <-e.w.Reached(name):
		case <-time.After(5 * time.Second):
			t.Fatalf("no call reached barrier %s within 5s", name)
		}
	}
	stillHeld := func(name string, ch <-chan store.Response) {
		t.Helper()
		select {
		case r := <-ch:
			t.Fatalf("barrier %s: the call returned %v before Release", name, r.Outcome)
		case <-time.After(100 * time.Millisecond):
		}
	}
	done := func(name string, ch <-chan store.Response) store.Response {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(5 * time.Second):
			t.Fatalf("barrier %s: the call did not return within 5s", name)
			return store.Response{}
		}
	}

	// before_send: nothing is written while it holds.
	e.arm(t, fixtures.Fault{Mode: fixtures.Barrier, Match: "read_epoch", Count: 1, At: fixtures.BeforeSend, Name: "b1"})
	n := e.sent.len()
	ch := call(t.Context())
	waitReached("b1")
	stillHeld("b1", ch)
	if recs := e.sent.since(n); len(recs) != 0 {
		t.Errorf("before_send: %v written while held, want nothing", recs)
	}
	e.w.Release("b1")
	if r := done("b1", ch); r.Outcome != store.OK {
		t.Errorf("before_send after Release: outcome %v, want ok", r.Outcome)
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") {
		t.Errorf("before_send after Release wrote %v, want read_epoch bytes", recs)
	}

	// after_reply: the command is written, the reply held.
	e.arm(t, fixtures.Fault{Mode: fixtures.Barrier, Match: "read_epoch", Count: 1, At: fixtures.AfterReply, Name: "b2"})
	n = e.sent.len()
	ch = call(t.Context())
	waitReached("b2")
	stillHeld("b2", ch)
	if recs := e.sent.since(n); !written(recs, "read_epoch") {
		t.Errorf("after_reply: %v written while held, want read_epoch bytes", recs)
	}
	e.w.Release("b2")
	if r := done("b2", ch); r.Outcome != store.OK {
		t.Errorf("after_reply after Release: outcome %v, want ok", r.Outcome)
	}

	// A held call whose ctx ends goes on without the barrier, and sends nothing.
	e.arm(t, fixtures.Fault{Mode: fixtures.Barrier, Match: "read_epoch", Count: 1, At: fixtures.BeforeSend, Name: "b3"})
	ctx, cancel := context.WithCancel(t.Context())
	n = e.sent.len()
	ch = call(ctx)
	waitReached("b3")
	cancel()
	if r := done("b3", ch); r.Outcome != store.Cancelled || r.Sent {
		t.Errorf("held call cancelled: outcome %v, sent %v, want cancelled, nothing sent", r.Outcome, r.Sent)
	}
	if recs := e.sent.since(n); len(recs) != 0 {
		t.Errorf("held call cancelled: %v written, want nothing", recs)
	}
}

// TestBatchFaultsPerRequest (DESIGN §B6; plan S7 step 11): inside CallBatch each request takes
// its own fault. A refusal matching one request refuses that one alone (unavailable, no byte
// written) while the rest of the batch is sent; a before_send barrier holds its request alone,
// which is sent on its own after Release, while the rest of the batch goes ahead. A drop_reply
// loses its own request's reply alone (outcome_unknown, sent, bytes written); a delayed
// request is written alone at least Delay after the call while the rest is written at once;
// an after_reply barrier on one request lets every request be written and holds the batch's
// return until Release. The responses keep the batch's order.
func TestBatchFaultsPerRequest(t *testing.T) {
	e := newWrapEnv(t)
	e.warm(t)
	isEpochMissing := func(r store.Response) bool {
		return r.Outcome == store.OK && r.Sent && r.Reply.Code == 22 && r.Reply.Reason == "epoch_record_missing"
	}
	isHoldRefused := func(r store.Response) bool {
		return r.Outcome == store.OK && r.Sent && r.Reply.Code == 41 && r.Reply.Reason == "epoch_changed"
	}

	// A refusal of hold: read_epoch is sent, hold is not.
	e.arm(t, fixtures.Fault{Mode: fixtures.Refuse, Match: "hold", Count: 1})
	n := e.sent.len()
	rs := e.a.CallBatch(t.Context(), []store.Request{readEpoch(), holdReq()})
	if len(rs) != 2 {
		t.Fatalf("refusal: %d responses, want 2", len(rs))
	}
	if !isEpochMissing(rs[0]) {
		t.Errorf("refusal of hold: read_epoch outcome %v, sent %v, reply %d %q; want ok 22 epoch_record_missing",
			rs[0].Outcome, rs[0].Sent, rs[0].Reply.Code, rs[0].Reply.Reason)
	}
	if rs[1].Outcome != store.Unavailable || rs[1].Sent || rs[1].SentCommands != 0 {
		t.Errorf("refusal of hold: hold outcome %v, sent %v (%d); want unavailable, nothing sent", rs[1].Outcome, rs[1].Sent, rs[1].SentCommands)
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") || written(recs, "hold") {
		t.Errorf("refusal of hold wrote %v, want read_epoch bytes and no hold byte", recs)
	}

	// A barrier on hold, first in the batch: read_epoch goes ahead, hold is sent after Release.
	e.arm(t, fixtures.Fault{Mode: fixtures.Barrier, Match: "hold", Count: 1, At: fixtures.BeforeSend, Name: "batch"})
	n = e.sent.len()
	ch := make(chan []store.Response, 1)
	go func() { ch <- e.a.CallBatch(t.Context(), []store.Request{holdReq(), readEpoch()}) }()
	select {
	case <-e.w.Reached("batch"):
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the barrier within 5s")
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") || written(recs, "hold") {
		t.Errorf("held hold: %v written, want read_epoch bytes and no hold byte", recs)
	}
	select {
	case rs := <-ch:
		t.Fatalf("the batch returned %v before Release", rs)
	case <-time.After(100 * time.Millisecond):
	}
	released := time.Now()
	e.w.Release("batch")
	select {
	case rs = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the batch did not return within 5s of Release")
	}
	if len(rs) != 2 || !isHoldRefused(rs[0]) || !isEpochMissing(rs[1]) {
		t.Fatalf("batch after Release: %+v, want hold's refusal (41 epoch_changed), then read_epoch's 22 epoch_record_missing", rs)
	}
	if at := first(e.sent.since(n), "hold"); at.IsZero() || at.Before(released) {
		t.Errorf("hold written at %v, want after Release (%v)", at, released)
	}

	// A drop_reply of hold: both are written; read_epoch keeps its reply, hold's is lost.
	e.arm(t, fixtures.Fault{Mode: fixtures.DropReply, Match: "hold", Count: 1})
	n = e.sent.len()
	rs = e.a.CallBatch(t.Context(), []store.Request{readEpoch(), holdReq()})
	if len(rs) != 2 {
		t.Fatalf("drop_reply: %d responses, want 2", len(rs))
	}
	if !isEpochMissing(rs[0]) {
		t.Errorf("drop_reply of hold: read_epoch outcome %v, sent %v, reply %d %q; want ok 22 epoch_record_missing",
			rs[0].Outcome, rs[0].Sent, rs[0].Reply.Code, rs[0].Reply.Reason)
	}
	if rs[1].Outcome != store.OutcomeUnknown || !rs[1].Sent {
		t.Errorf("drop_reply of hold: hold outcome %v, sent %v; want outcome_unknown, sent", rs[1].Outcome, rs[1].Sent)
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") || !written(recs, "hold") {
		t.Errorf("drop_reply of hold wrote %v, want read_epoch and hold bytes", recs)
	}

	// A delay on hold, first in the batch: read_epoch is written at once, hold alone at least
	// Delay after the call; hold's response stays first.
	const delay = 300 * time.Millisecond
	e.arm(t, fixtures.Fault{Mode: fixtures.Delay, Match: "hold", Count: 1, Delay: delay})
	n = e.sent.len()
	start := time.Now()
	rs = e.a.CallBatch(t.Context(), []store.Request{holdReq(), readEpoch()})
	if len(rs) != 2 || !isHoldRefused(rs[0]) || !isEpochMissing(rs[1]) {
		t.Errorf("delayed hold: %+v, want hold's refusal (41 epoch_changed), then read_epoch's 22 epoch_record_missing", rs)
	}
	recs := e.sent.since(n)
	if at := first(recs, "read_epoch"); at.IsZero() || at.Sub(start) >= delay {
		t.Errorf("delayed hold: read_epoch written %v after the call (zero: never), want at once, before the %v delay", at.Sub(start), delay)
	}
	if at := first(recs, "hold"); at.IsZero() || at.Sub(start) < delay {
		t.Errorf("delayed hold: hold written %v after the call (zero: never), want at least %v", at.Sub(start), delay)
	}

	// An after_reply barrier on read_epoch, first in the batch: both requests are written, and
	// the batch returns only after Release, its responses in order.
	e.arm(t, fixtures.Fault{Mode: fixtures.Barrier, Match: "read_epoch", Count: 1, At: fixtures.AfterReply, Name: "batch_reply"})
	n = e.sent.len()
	ch = make(chan []store.Response, 1)
	go func() { ch <- e.a.CallBatch(t.Context(), []store.Request{readEpoch(), holdReq()}) }()
	select {
	case <-e.w.Reached("batch_reply"):
	case rs := <-ch:
		t.Fatalf("after_reply: the batch returned %+v without reaching the barrier", rs)
	case <-time.After(5 * time.Second):
		t.Fatal("after_reply: no request reached the barrier within 5s")
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") || !written(recs, "hold") {
		t.Errorf("after_reply: %v written while held, want read_epoch and hold bytes", recs)
	}
	select {
	case rs := <-ch:
		t.Fatalf("after_reply: the batch returned %+v before Release", rs)
	case <-time.After(100 * time.Millisecond):
	}
	e.w.Release("batch_reply")
	select {
	case rs = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("after_reply: the batch did not return within 5s of Release")
	}
	if len(rs) != 2 || !isEpochMissing(rs[0]) || !isHoldRefused(rs[1]) {
		t.Errorf("after_reply after Release: %+v, want read_epoch's 22 epoch_record_missing, then hold's refusal (41 epoch_changed)", rs)
	}
}

// TestDelayHoldsTheSend (DESIGN §B6 delay): a delay fault writes no byte of its call before
// Delay has passed, then sends it and returns the reply; a call whose ctx ends before Delay
// returns cancelled at its deadline with nothing written.
func TestDelayHoldsTheSend(t *testing.T) {
	e := newWrapEnv(t)
	const delay = 300 * time.Millisecond
	e.arm(t, fixtures.Fault{Mode: fixtures.Delay, Match: "read_epoch", Count: 1, Delay: delay})
	n := e.sent.len()
	start := time.Now()
	if r := e.a.Call(t.Context(), readEpoch()); r.Outcome != store.OK || !r.Sent || r.Reply.Code != 22 {
		t.Errorf("delayed call: outcome %v, sent %v, reply %d; want ok, sent, 22", r.Outcome, r.Sent, r.Reply.Code)
	}
	if at := first(e.sent.since(n), "read_epoch"); at.IsZero() || at.Sub(start) < delay {
		t.Errorf("delayed call: read_epoch written %v after the call (zero: never), want at least %v", at.Sub(start), delay)
	}

	e.arm(t, fixtures.Fault{Mode: fixtures.Delay, Match: "read_epoch", Count: 1, Delay: 2 * time.Second})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	n = e.sent.len()
	start = time.Now()
	r := e.a.Call(ctx, readEpoch())
	if el := time.Since(start); r.Outcome != store.Cancelled || r.Sent || el >= time.Second {
		t.Errorf("delay past the ctx deadline: outcome %v, sent %v after %v; want cancelled, nothing sent, at the 100ms deadline", r.Outcome, r.Sent, el)
	}
	if recs := e.sent.since(n); written(recs, "read_epoch") {
		t.Errorf("delay past the ctx deadline wrote %v, want no read_epoch byte", recs)
	}
}

// TestFaultDuration (DESIGN §B6; FLT-1 b): a fault armed with a Duration (and no Count)
// applies to every matching call until Duration has passed since Arm, and to none after.
func TestFaultDuration(t *testing.T) {
	e := newWrapEnv(t)
	const dur = 300 * time.Millisecond
	armed := time.Now()
	e.arm(t, fixtures.Fault{Mode: fixtures.Refuse, Match: "read_epoch", Duration: dur})
	n := e.sent.len()
	for k := range 2 {
		if r := e.a.Call(t.Context(), readEpoch()); r.Outcome != store.Unavailable || r.Sent {
			t.Errorf("call %d within the duration: outcome %v, sent %v; want unavailable, nothing sent", k+1, r.Outcome, r.Sent)
		}
	}
	if el := time.Since(armed); el >= dur {
		t.Fatalf("the refused calls ended %v after Arm, past the %v duration", el, dur)
	}
	if recs := e.sent.since(n); written(recs, "read_epoch") {
		t.Errorf("refused calls wrote %v, want no read_epoch byte", recs)
	}
	time.Sleep(time.Until(armed.Add(dur + 50*time.Millisecond)))
	n = e.sent.len()
	if r := e.a.Call(t.Context(), readEpoch()); r.Outcome != store.OK || !r.Sent {
		t.Errorf("call after the duration: outcome %v, sent %v; want ok, sent", r.Outcome, r.Sent)
	}
	if recs := e.sent.since(n); !written(recs, "read_epoch") {
		t.Errorf("call after the duration wrote %v, want read_epoch bytes", recs)
	}
}

// TestSignerUnusable (DESIGN §B6 FLT-10; SPEC API-1a): an armed unusable fault makes Usable
// report false (with Count 0, on every call until Disarm) and the signer is usable again after
// Disarm; through a limiter, Count 1 makes the next acquire config_error with no permit and no
// acquire written, and the following acquire allowed.
func TestSignerUnusable(t *testing.T) {
	inner, err := receipt.NewHMACSigner(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w := fixtures.NewSignerWrapper()
	s := w.Wrap(inner)
	if !s.Usable() {
		t.Fatal("no fault armed: Usable false, want true")
	}
	if err := w.Arm(fixtures.SignerFault{Mode: fixtures.SignUnusable}); err != nil {
		t.Fatal(err)
	}
	for k := range 3 {
		if s.Usable() {
			t.Errorf("unusable armed with Count 0, call %d: Usable true, want false", k+1)
		}
	}
	w.Disarm()
	if !s.Usable() {
		t.Error("after Disarm: Usable false, want true")
	}

	d := driver.New(t, driver.Options{Instances: 1})
	if err := d.SignerWrapper(0).Arm(fixtures.SignerFault{Mode: fixtures.SignUnusable, Count: 1}); err != nil {
		t.Fatal(err)
	}
	n := len(d.Sent(0))
	if op := d.Acquire(0, "k"); op.Kind != limiter.ConfigError || op.Permit != nil {
		t.Errorf("acquire with the signer unusable: %v (a permit: %v), want config_error and no permit", op.Kind, op.Permit != nil)
	}
	if sent := d.Sent(0)[n:]; driver.Count(sent, "acquire") != 0 {
		t.Errorf("acquire with the signer unusable wrote %v, want no acquire byte", sent)
	}
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Errorf("the next acquire: %v, want allowed", op.Kind)
	}
	if op.Permit != nil {
		op.Permit.Done()
	}
}

// refundReq is a refund call (DESIGN §A4.1) under an epoch the store does not have: on a store
// without an epoch it gets {22, 0, 'epoch_record_missing'} and writes nothing.
func refundReq(t *testing.T) store.Request {
	t.Helper()
	enc, err := keys.Encode("k")
	if err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef"
	return store.Request{
		Kind: store.Refund, Script: scripts.Refund,
		Keys: []string{
			keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(id), keys.QuarantineKey(id),
			keys.BucketKey(id, enc), keys.MarkerIndexKey(id, enc),
		},
		Args:            []string{id, "fp", enc, id, "1", "1", "2000000", "20000000000", "1000000000", "10", "1000", "1000", "0", "0"},
		ReceiptExpiryUs: 2_000_000,
	}
}

// refundOK reports whether r is the reply refundReq gets on a store without an epoch.
func refundOK(r store.Response) bool {
	return r.Outcome == store.OK && r.Reply.Code == 22 && r.Reply.Reason == "epoch_record_missing"
}

// TestRefundMatch (plan S6 step 10; DESIGN §B6 FLT-1 b, FLT-4, Barriers): each store fault
// mode takes match "refund" and then acts on refund calls (sent on the refund connections) and
// on nothing else: refuse sends nothing (unavailable); drop_reply sends and loses the reply
// (outcome_unknown); delay holds the send for its wait; a before_send barrier holds it until
// Release. A refund fault leaves read_epoch alone and an acquire fault leaves refunds alone.
func TestRefundMatch(t *testing.T) {
	e := newWrapEnv(t)
	req := refundReq(t)
	if r := e.a.Call(t.Context(), req); !refundOK(r) {
		t.Fatalf("unfaulted refund: outcome %v, reply %d %q, want ok 22 epoch_record_missing", r.Outcome, r.Reply.Code, r.Reply.Reason)
	}

	e.arm(t, fixtures.Fault{Mode: fixtures.Refuse, Match: "acquire"})
	n := e.sent.len()
	if r := e.a.Call(t.Context(), req); !refundOK(r) {
		t.Errorf("refund with acquire refused: outcome %v, want ok", r.Outcome)
	}
	if !written(e.sent.since(n), "refund") {
		t.Errorf("refund with acquire refused wrote %v, want refund bytes", e.sent.since(n))
	}
	e.w.Disarm()

	e.arm(t, fixtures.Fault{Mode: fixtures.Refuse, Match: "refund", Count: 1})
	n = e.sent.len()
	if r := e.a.Call(t.Context(), readEpoch()); r.Outcome != store.OK {
		t.Errorf("read_epoch with refund refused: outcome %v, want ok", r.Outcome)
	}
	if r := e.a.Call(t.Context(), req); r.Outcome != store.Unavailable || r.Sent || r.SentCommands != 0 {
		t.Errorf("refused refund: outcome %v, sent %v (%d), want unavailable, nothing sent", r.Outcome, r.Sent, r.SentCommands)
	}
	if written(e.sent.since(n), "refund") {
		t.Errorf("a refused refund wrote %v, want 0 refund bytes", e.sent.since(n))
	}

	e.arm(t, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
	n = e.sent.len()
	if r := e.a.Call(t.Context(), req); r.Outcome != store.OutcomeUnknown || !r.Sent || r.SentCommands != 1 {
		t.Errorf("dropped refund reply: outcome %v, sent %v (%d), want outcome_unknown, 1 sent", r.Outcome, r.Sent, r.SentCommands)
	}
	if !written(e.sent.since(n), "refund") {
		t.Errorf("a dropped-reply refund wrote %v, want refund bytes", e.sent.since(n))
	}
	if r := e.a.Call(t.Context(), req); !refundOK(r) {
		t.Errorf("the refund after the dropped reply: outcome %v, want ok", r.Outcome)
	}

	const wait = 150 * time.Millisecond
	e.arm(t, fixtures.Fault{Mode: fixtures.Delay, Match: "refund", Count: 1, Delay: wait})
	n = e.sent.len()
	start := time.Now()
	if r := e.a.Call(t.Context(), req); !refundOK(r) {
		t.Errorf("delayed refund: outcome %v, want ok", r.Outcome)
	}
	if at := first(e.sent.since(n), "refund"); at.IsZero() || at.Sub(start) < wait {
		t.Errorf("delayed refund written %v after the call began, want at least %v", at.Sub(start), wait)
	}

	e.arm(t, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.BeforeSend, Name: "refund-held"})
	n = e.sent.len()
	ch := make(chan store.Response, 1)
	go func() { ch <- e.a.Call(t.Context(), req) }()
	select {
	case <-e.w.Reached("refund-held"):
	case <-time.After(5 * time.Second):
		t.Fatal("the refund did not reach the barrier within 5s")
	}
	select {
	case r := <-ch:
		t.Fatalf("the held refund returned %v before Release", r.Outcome)
	case <-time.After(100 * time.Millisecond):
	}
	if written(e.sent.since(n), "refund") {
		t.Errorf("a refund held before its send wrote %v, want 0 refund bytes", e.sent.since(n))
	}
	e.w.Release("refund-held")
	select {
	case r := <-ch:
		if !refundOK(r) {
			t.Errorf("the released refund: outcome %v, want ok", r.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the released refund did not return within 5s")
	}
}

// stubAdapter is a store adapter that answers every script call with resp's response.
type stubAdapter struct {
	resp func(store.Request) store.Response
}

func (s stubAdapter) Call(_ context.Context, req store.Request) store.Response { return s.resp(req) }

func (s stubAdapter) CallBatch(_ context.Context, reqs []store.Request) []store.Response {
	out := make([]store.Response, len(reqs))
	for i, r := range reqs {
		out[i] = s.resp(r)
	}
	return out
}

func (stubAdapter) Do(context.Context, store.Kind, ...string) (resp.Value, store.Response) {
	return resp.Value{}, store.Response{Outcome: store.Unavailable}
}

func (stubAdapter) ReadyConns(store.Kind) int { return 1 }

func (stubAdapter) Close() {}

// TestNotReadyRefunds (DESIGN §A5.4, §C4; plan S6 step 13): the wrapper counts the refund
// responses the store refused with LOADING or BUSY (unavailable with the error reply kept), by
// Call and by CallBatch, and nothing else: a refund unavailable with nothing sent, an acquire
// refused with BUSY, and a refund answered with an error reply that is not LOADING or BUSY
// (outcome_unknown).
func TestNotReadyRefunds(t *testing.T) {
	answers := map[string]store.Response{
		"refund-loading": {Outcome: store.Unavailable, StoreError: "LOADING Valkey is loading the dataset in memory"},
		"refund-busy":    {Outcome: store.Unavailable, StoreError: "BUSY Valkey is busy running a script"},
		"refund-unsent":  {Outcome: store.Unavailable},
		"refund-error":   {Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR something"},
		"acquire-busy":   {Outcome: store.Unavailable, StoreError: "BUSY Valkey is busy running a script"},
	}
	w := fixtures.NewStoreWrapper()
	a := w.Wrap(stubAdapter{resp: func(r store.Request) store.Response { return answers[r.Args[0]] }})
	req := func(script scripts.Script, answer string) store.Request {
		return store.Request{Kind: store.Refund, Script: script, Args: []string{answer}}
	}
	if got := w.NotReadyRefunds(); got != 0 {
		t.Fatalf("NotReadyRefunds before any call = %d, want 0", got)
	}
	for _, name := range []string{"refund-unsent", "refund-error"} {
		a.Call(t.Context(), req(scripts.Refund, name))
	}
	a.Call(t.Context(), req(scripts.Acquire, "acquire-busy"))
	if got := w.NotReadyRefunds(); got != 0 {
		t.Errorf("NotReadyRefunds after no LOADING or BUSY refund = %d, want 0", got)
	}
	a.Call(t.Context(), req(scripts.Refund, "refund-loading"))
	if got := w.NotReadyRefunds(); got != 1 {
		t.Errorf("NotReadyRefunds after a LOADING refund = %d, want 1", got)
	}
	a.CallBatch(t.Context(), []store.Request{
		req(scripts.Refund, "refund-busy"), req(scripts.Acquire, "acquire-busy"), req(scripts.Refund, "refund-unsent"),
	})
	if got := w.NotReadyRefunds(); got != 2 {
		t.Errorf("NotReadyRefunds after a batch with one BUSY refund = %d, want 2", got)
	}
	if got := w.NoScripts(); got != 0 {
		t.Errorf("NoScripts = %d, want 0: no response had NoScript", got)
	}
}

// TestNotReadyRefundsAfterReload (DESIGN §A5.4, §C4; plan S6 step 13): a refund whose resend
// after a NOSCRIPT reload the store refused with LOADING or BUSY comes back outcome_unknown,
// SentCommands 1, with the error reply in StoreError (the adapter's NOSCRIPT path), while the
// send observer heard two refund writes; the wrapper counts it as not ready, by Call and by
// CallBatch. The same response with BUSYKEY, or with the NOSCRIPT reply itself, does not count.
func TestNotReadyRefundsAfterReload(t *testing.T) {
	reloaded := func(storeErr string) store.Response {
		return store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, NoScript: true, StoreError: storeErr}
	}
	answers := map[string]store.Response{
		"busy":     reloaded("BUSY Valkey is busy running a script"),
		"loading":  reloaded("LOADING Valkey is loading the dataset in memory"),
		"busykey":  reloaded("BUSYKEY Target key name already exists."),
		"noscript": reloaded("NOSCRIPT No matching script. Please use EVAL."),
	}
	w := fixtures.NewStoreWrapper()
	a := w.Wrap(stubAdapter{resp: func(r store.Request) store.Response { return answers[r.Args[0]] }})
	req := func(answer string) store.Request {
		return store.Request{Kind: store.Refund, Script: scripts.Refund, Args: []string{answer}}
	}
	a.Call(t.Context(), req("busykey"))
	a.Call(t.Context(), req("noscript"))
	if got := w.NotReadyRefunds(); got != 0 {
		t.Errorf("NotReadyRefunds after BUSYKEY and NOSCRIPT replies = %d, want 0", got)
	}
	a.Call(t.Context(), req("busy"))
	if got := w.NotReadyRefunds(); got != 1 {
		t.Errorf("NotReadyRefunds after a resend refused with BUSY = %d, want 1", got)
	}
	a.CallBatch(t.Context(), []store.Request{req("loading"), req("busykey")})
	if got := w.NotReadyRefunds(); got != 2 {
		t.Errorf("NotReadyRefunds after a batch with one resend refused with LOADING = %d, want 2", got)
	}
	if got := w.NoScripts(); got != 5 {
		t.Errorf("NoScripts = %d, want 5: every response had NoScript", got)
	}
}

// TestSignerCorruptField (DESIGN §A5.9, §B6 FLT-10 corrupt; plan S6 step 10): with
// CorruptField armed for any of the 7 authenticated fields, a correctly signed receipt no
// longer verifies (the alteration happens after signing, on the receipt Verify gets), while
// signing is unaffected; Disarm restores verification. A corrupt fault with an unknown or empty
// field, or a field on another mode, is refused; Count bounds the Verify calls it answers.
func TestSignerCorruptField(t *testing.T) {
	inner, err := receipt.NewHMACSigner(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w := fixtures.NewSignerWrapper()
	s := w.Wrap(inner)
	var id [16]byte
	id[3] = 7
	signed, err := s.Sign(receipt.New(id, "enc-of-k", 3, 9, 1_760_000_000_000_000, 6_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Verify(signed) {
		t.Fatal("no fault armed: the signed receipt does not verify")
	}
	fields := fixtures.ReceiptFields()
	if len(fields) != 7 {
		t.Fatalf("ReceiptFields() = %q, want the 7 fields of §A5.9", fields)
	}
	for _, f := range fields {
		if err := w.CorruptField(f); err != nil {
			t.Fatalf("CorruptField(%q): %v", f, err)
		}
		for k := range 2 {
			if s.Verify(signed) {
				t.Errorf("%s corrupted, Verify call %d: true, want false", f, k+1)
			}
		}
		again, err := s.Sign(receipt.New(id, "enc-of-k", 3, 9, 1_760_000_000_000_000, 6_000_000))
		if err != nil || again.MAC != signed.MAC {
			t.Errorf("%s corrupted: Sign changed (err %v): the fault acts on Verify only", f, err)
		}
		w.Disarm()
		if !s.Verify(signed) {
			t.Errorf("%s: after Disarm the receipt does not verify", f)
		}
	}
	for _, bad := range []fixtures.SignerFault{
		{Mode: fixtures.SignCorrupt, Field: "mac"},
		{Mode: fixtures.SignCorrupt},
		{Mode: fixtures.SignFail, Field: fixtures.FieldG},
		{Mode: fixtures.SignCorrupt, Field: fixtures.FieldG, Count: -1},
	} {
		if err := w.Arm(bad); err == nil {
			t.Errorf("Arm(%+v) = nil, want an error", bad)
		}
	}
	if err := w.Arm(fixtures.SignerFault{Mode: fixtures.SignCorrupt, Field: fixtures.FieldS, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if s.Verify(signed) {
		t.Error("corrupt fault with Count 1, first Verify: true, want false")
	}
	if !s.Verify(signed) {
		t.Error("corrupt fault with Count 1, second Verify: false, want true (the fault was used up)")
	}
}
