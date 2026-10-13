package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil"
	"ratelimiter/internal/testutil/fakeresp"
)

// The production adapter satisfies the interface that limitertest aliases (§A5.12).
var _ Adapter = (*StoreAdapter)(nil)

// The package's first six tests fail by assertion on defects that would crash or hang the
// tests after them first (DESIGN §C9: neither a crash nor a hang is a kill), so bin/mutate,
// which runs tests in file order with -failfast and without -shuffle, counts those defects as
// kills; they stay first, in this order. TestPickDecides and TestDialReturnsEachFailure call
// pick and dial directly, so a pick or a dial that returns neither a connection nor an error
// fails them before any caller dereferences the missing connection, and TestPickDecides
// takes a ready connection before any pick finds none ready; it also takes the fail-fast
// error of a dial the store refused, and TestDialReturnsEachFailure reads a refused AUTH's
// text (refusalText) and classifies a call that fails fast on it (classify), before any
// slot's failed dial or any call that sent nothing reaches either.
// TestSuccessfulDialReadiesAtOnce checks the slot's success path before any test's dial
// fails; TestDialFailuresInChild checks that the adapter uses its injected Dial and Rand
// before any test builds an adapter without them; TestStallAfterPartialBatchReply checks the
// stall rule before any reply empties a FIFO, and the send observer before any test writes
// without one; TestEntriesReturnAndCloseEnds checks Do's observer label for a two-word
// command before any test sends a one-word one. Each call and each Close there runs on a
// goroutine of its own with a bounded wait (within, waitDone), so a hang fails the test by
// assertion; nothing catches a panic, which crashes the test binary.

// TestPickDecides: pick decides in this order (DESIGN §A5.4; plan S3 step 8): a closed set is
// unavailable (errAdapterClosed), even with ctx already ended; an ended ctx returns its cause
// (context.Cause) at once; a ready connection is taken, with n commands reserved on it, even
// while the kind's last dial failed within its backoff; and with none ready during that
// backoff, a call fails fast (errFailFast), with a *refusedError that carries the store's
// error reply when the store refused that dial (plan S9 step 1). It runs pick on a bare
// connSet, with no connection goroutines; the ready connection is a net.Pipe end that nothing
// reads.
func TestPickDecides(t *testing.T) {
	const wrongPass = "WRONGPASS invalid username-password pair or user is disabled."
	pick := func(s *connSet, ctx context.Context, n int, what string) (*pipeConn, error) {
		t.Helper()
		var pc *pipeConn
		var err error
		mustWithin(t, what, time.Second, func() { pc, err = s.pick(ctx, n) })
		return pc, err
	}
	// A call ctx that its dependency deadline, start + D, has already ended.
	ended, cancel := context.WithDeadlineCause(t.Context(), time.Now().Add(-time.Millisecond), errDependencyDeadline)
	defer cancel()

	closed := newConnSet(Call, t.Context(), nil)
	closed.shutdown()
	if pc, err := pick(closed, ended, 1, "a pick on a closed set"); pc != nil || !errors.Is(err, errAdapterClosed) {
		t.Fatalf("a pick on a closed set, its ctx ended = %v, %v; want no connection and %v", pc, err, errAdapterClosed)
	}
	open := newConnSet(Call, t.Context(), nil)
	if pc, err := pick(open, ended, 1, "a pick with its ctx ended"); pc != nil || !errors.Is(err, errDependencyDeadline) {
		t.Fatalf("a pick with its ctx ended = %v, %v; want no connection and its cause, %v", pc, err, errDependencyDeadline)
	}
	c, s := net.Pipe()
	t.Cleanup(func() { _, _ = c.Close(), s.Close() })
	ready := newPipeConn(c, testD, nil, open.remove)
	if !open.add(ready) {
		t.Fatal("adding a ready connection to an open set failed")
	}
	open.dialFailed(time.Now().Add(time.Minute), "") // another slot's dial failed meanwhile
	if pc, err := pick(open, t.Context(), 2, "a pick with a connection ready"); pc != ready || err != nil || ready.load.Load() != 2 {
		t.Fatalf("a pick of 2 with a connection ready = %v, %v, %d reserved; want that connection, no error, 2", pc, err, ready.load.Load())
	}
	open.remove(ready)
	if pc, err := pick(open, t.Context(), 1, "a pick within the backoff"); pc != nil || !errors.Is(err, errFailFast) {
		t.Fatalf("a pick within the backoff of a failed dial = %v, %v; want no connection and %v", pc, err, errFailFast)
	}
	open.dialFailed(time.Now().Add(time.Minute), wrongPass) // the store refused the next dial
	pc, err := pick(open, t.Context(), 1, "a pick within the backoff of a refused dial")
	var re *refusedError
	if pc != nil || !errors.As(err, &re) || re.reply != wrongPass || !errors.Is(err, errFailFast) {
		t.Fatalf("a pick within the backoff of a dial the store refused = %v, %v; want no connection and a *refusedError carrying the reply and wrapping %v", pc, err, errFailFast)
	}
}

// TestDialReturnsEachFailure: dial connects one connection and authenticates it (DESIGN
// §A5.4). It returns the connection once AUTH succeeds; for each way it can fail it returns
// an error and no connection, and closes the connection it opened: a connect that fails (the
// dialer's error, wrapped), an AUTH the store refuses, and a connect that completes after its
// set shut down (errAdapterClosed, with nothing written). The refused AUTH's error holds the
// store's reply, which refusalText reads, and a call that fails fast on that refusal is
// unavailable, sent nothing, and keeps the reply as its StoreError (classify; plan S9 step 1).
// It calls dial directly, on an adapter with no connection slots.
func TestDialReturnsEachFailure(t *testing.T) {
	const wrongPass = "WRONGPASS invalid username-password pair or user is disabled."
	pw := newPassword(t)
	srv := fakeresp.Start(t, "rl_lib", pw)
	refused := errors.New("connection refused (test)")
	var failConnect atomic.Bool
	var writes atomic.Int64 // on every connection dial opened
	var opened tapList
	a := &StoreAdapter{
		addr: srv.Addr(), user: "rl_lib", pass: pw, d: testD, life: t.Context(),
		dialFn: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if failConnect.Load() {
				return nil, refused
			}
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := &tapConn{Conn: c, intercept: func(*tapConn, []byte) (bool, int, error) {
				writes.Add(1)
				return false, 0, nil
			}}
			opened.add(tc)
			return tc, nil
		},
	}
	set := newConnSet(Call, t.Context(), nil)

	pc, err := a.dial(set)
	if err != nil || pc == nil {
		t.Fatalf("a dial the store accepts = %v, %v; want a connection and no error", pc, err)
	}
	_ = pc.conn.Close()

	failConnect.Store(true)
	pc, err = a.dial(set)
	if pc != nil || !errors.Is(err, refused) {
		t.Fatalf("a dial that cannot connect = %v, %v; want no connection and the dialer's error", pc, err)
	}
	failConnect.Store(false)

	srv.ReplyError("AUTH", wrongPass, 1)
	pc, err = a.dial(set)
	if pc != nil || err == nil || errors.Is(err, errAdapterClosed) {
		t.Fatalf("a dial whose AUTH the store refuses = %v, %v; want no connection and the handshake's error", pc, err)
	}
	if got := refusalText(err); got != wrongPass {
		t.Fatalf("refusalText of the refused dial's error = %q, want the store's reply %q", got, wrongPass)
	}
	failFast := exchanged{err: &refusedError{reply: refusalText(err)}}
	if r, ok := classify(t.Context(), time.Now().Add(testD), "acquire", failFast); ok || r.Outcome != Unavailable || r.Sent || r.SentCommands != 0 || r.StoreError != wrongPass {
		t.Fatalf("classify of a call that failed fast on the refusal = %+v, %v; want unavailable, nothing sent, the reply as StoreError, no reply value", r, ok)
	}

	set.shutdown()
	before := writes.Load()
	pc, err = a.dial(set)
	if pc != nil || !errors.Is(err, errAdapterClosed) {
		t.Fatalf("a dial that connects after its set shut down = %v, %v; want no connection and %v", pc, err, errAdapterClosed)
	}
	conns := opened.all()
	closed := 0
	for _, tc := range conns {
		if tc.closed.Load() {
			closed++
		}
	}
	if len(conns) != 3 || closed != 3 || writes.Load() != before {
		t.Errorf("%d connections opened, %d of them closed, %d writes after the shutdown; want 3 (the test closed the first), 3, 0",
			len(conns), closed, writes.Load()-before)
	}
}

// TestSuccessfulDialReadiesAtOnce: a slot whose first dial succeeds makes that connection
// ready at once, with no backoff and no jitter draw (DESIGN §A5.4: the backoff follows a
// failed dial or a lost connection). Every dial here succeeds, so the adapter never calls
// Options.Rand; a call that comes anyway waits for the test's end, so its slot does nothing
// more until then.
func TestSuccessfulDialReadiesAtOnce(t *testing.T) {
	pw := newPassword(t)
	srv := fakeresp.Start(t, "rl_lib", pw)
	var dials, draws atomic.Int64
	a := startBounded(t, Options{
		Addr: srv.Addr(), User: "rl_lib", Password: pw, StoreConns: 1, RefundConns: 1,
		DependencyDeadline: 5 * time.Second, Limits: testLimits(), // no handshake runs out of time
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		Rand: func() float64 {
			draws.Add(1)
			<-t.Context().Done()
			return 0.5
		},
	})
	waitFor(t, "every slot ready, or a jitter draw", 2*time.Second, func() bool {
		ready := a.ReadyConns(Call) == 1 && a.ReadyConns(Refund) == 1 && a.ReadyConns(Maintenance) == 1
		return ready || draws.Load() > 0
	})
	if d, r := dials.Load(), draws.Load(); d != 3 || r != 0 {
		t.Fatalf("%d dials and %d jitter draws before every slot was ready, want 3 (one per slot) and 0", d, r)
	}
}

// TestDialFailuresInChild: a dial that cannot connect, one whose AUTH the store refuses, and
// one that completes only after Close each end without a connection (DESIGN §A5.4). After a
// failure the slot draws its jitter from Options.Rand, backs off and dials again through
// Options.Dial, closing the connection it opened; after Close it closes the late connection
// unused. It runs in a child copy of the test binary (testutil.InChild): a defect that
// crashes a connection goroutine on these paths crashes only the child, which InChild
// reports as a crash, never as a failure by assertion, and the package's other tests still
// run.
func TestDialFailuresInChild(t *testing.T) {
	if !testutil.InChild(t, 20*time.Second) {
		return
	}
	pw := newPassword(t)
	srv := fakeresp.Start(t, "rl_lib", pw)
	srv.ReplyError("AUTH", "WRONGPASS invalid username-password pair or user is disabled.", 3)
	var dials, draws atomic.Int64
	var reached tapList // the connections that reached the store
	a := startBounded(t, Options{
		Addr: srv.Addr(), User: "rl_lib", Password: pw, StoreConns: 1, RefundConns: 1,
		DependencyDeadline: testD, Limits: testLimits(),
		// The 3 slots' first dials cannot connect; the next 3 reach the store, which refuses
		// their AUTH; every later dial succeeds.
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dials.Add(1) <= 3 {
				return nil, errors.New("connection refused (test)")
			}
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := &tapConn{Conn: c}
			reached.add(tc)
			return tc, nil
		},
		Rand: func() float64 { draws.Add(1); return 0.99 }, // backoffs of 49.75 ms, then 99.5 ms
	})
	waitFor(t, "every slot ready after a failed connect and a refused AUTH", 3*time.Second, func() bool {
		return a.ReadyConns(Call) == 1 && a.ReadyConns(Refund) == 1 && a.ReadyConns(Maintenance) == 1
	})
	if d, r := dials.Load(), draws.Load(); d != 9 || r != 6 {
		t.Errorf("%d dials and %d jitter draws, want 9 (3 refused, 3 with AUTH refused, 3 ready) and 6 (one per failed dial)", d, r)
	}
	closed := 0
	for _, tc := range reached.all() {
		if tc.closed.Load() {
			closed++
		}
	}
	if n := len(reached.all()); n != 6 || closed != 3 {
		t.Errorf("%d connections reached the store and %d of them were closed, want 6 and 3 (those whose AUTH was refused)", n, closed)
	}

	// Each slot's dial completes only after Close began: it closes that connection unused.
	var dialing, writes atomic.Int64
	var late tapList
	b := startBounded(t, Options{
		Addr: srv.Addr(), User: "rl_lib", Password: pw, StoreConns: 1, RefundConns: 1,
		DependencyDeadline: time.Minute, Limits: testLimits(), // only Close ends these dials
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialing.Add(1)
			<-ctx.Done() // the adapter's lifetime ends at Close …
			c, s := net.Pipe()
			_ = s.Close() // nothing would read what the client end writes
			tc := &tapConn{Conn: c, intercept: func(*tapConn, []byte) (bool, int, error) {
				writes.Add(1)
				return false, 0, nil
			}}
			late.add(tc)
			return tc, nil // … and the dial completes all the same
		},
	})
	waitFor(t, "3 dials in progress", time.Second, func() bool { return dialing.Load() == 3 })
	mustWithin(t, "Close", 2*time.Second, b.Close)
	conns := late.all()
	closed = 0
	for _, tc := range conns {
		if tc.closed.Load() {
			closed++
		}
	}
	if len(conns) != 3 || closed != 3 || writes.Load() != 0 {
		t.Errorf("%d connections completed after Close, %d closed, %d writes on them; want 3, 3, 0", len(conns), closed, writes.Load())
	}
}

// TestStallAfterPartialBatchReply: when the store answers the first command of a batch and
// never the second, the stall rule moves to the second, the new FIFO head, and closes the
// connection once it has waited D since its write (DESIGN §A5.4; plan S3 step 4). The first
// response is ok with its reply; the second, sent and never answered, outcome_unknown. The
// store end is a net.Pipe (pipeStore), so the test decides to the byte what arrives.
func TestStallAfterPartialBatchReply(t *testing.T) {
	pw := newPassword(t)
	srv := fakeresp.Start(t, "rl_lib", pw) // the redial after the stall reaches it
	sc := scripts.New("acquire", "-- partial batch reply")
	srv.Load(sc.Text)
	srv.Handle(sc.SHA, echoAcquire)
	p := newPipeStore(t, srv.Addr())
	obs := &observer{}
	a := startBounded(t, Options{
		Addr: srv.Addr(), User: "rl_lib", Password: pw, StoreConns: 1, RefundConns: 1,
		DependencyDeadline: testD, Limits: testLimits(), Dial: p.dial, SendObserver: obs.record,
	})
	waitFor(t, "ready connections", 5*time.Second, func() bool {
		return a.ReadyConns(Call) == 1 && a.ReadyConns(Refund) == 1 && a.ReadyConns(Maintenance) == 1
	})
	end := p.callEnd(t, a)
	if err := end.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting the store end's read deadline: %v", err)
	}
	r := resp.NewReader(end)

	began := time.Now() // before the batch's write
	var rs []Response
	batch := goDone(func() {
		rs = a.CallBatch(t.Context(), []Request{acquireReq(sc, 1), acquireReq(sc, 2)})
	})
	for g := 1; g <= 2; g++ {
		v, err := r.ReadValue()
		if err != nil || len(v.Elems) != 4 || v.Elems[0].Str != "EVALSHA" || v.Elems[3].Str != strconv.Itoa(g) {
			t.Fatalf("the store read %s, %v; want the batch's EVALSHA with g %d", v, err, g)
		}
	}
	if _, err := end.Write(resp.AppendValue(nil, allowed(1))); err != nil { // the first command's reply only
		t.Fatalf("answering the first command: %v", err)
	}
	if err := end.SetReadDeadline(time.Now().Add(testD + time.Second)); err != nil {
		t.Fatalf("setting the store end's read deadline: %v", err)
	}
	if v, err := r.ReadValue(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the first reply the store end read %s, %v; want EOF: the stall rule did not close the connection for the unanswered second command", v, err)
	}
	if el := time.Since(began); el < testD {
		t.Errorf("the connection closed %v after the batch began, before D", el)
	}
	if !waitDone(t, "CallBatch", 2*time.Second, batch) {
		t.FailNow()
	}
	if rs[0].Outcome != OK || rs[0].Reply.Elems[4].Int != 1 {
		t.Errorf("response 1 = %v, want ok with its own reply", rs[0].Outcome)
	}
	if rs[1].Outcome != OutcomeUnknown || !rs[1].Sent || rs[1].SentCommands != 1 {
		t.Errorf("response 2 = %v, Sent %v, SentCommands %d; want outcome_unknown, true, 1", rs[1].Outcome, rs[1].Sent, rs[1].SentCommands)
	}
	// The observer hears each command of the batch once, with all its bytes (§A5.12).
	want := []int{cmdLen("EVALSHA", sc.SHA, "0", "1"), cmdLen("EVALSHA", sc.SHA, "0", "2")}
	if got := obs.bytesOf("acquire"); !slices.Equal(got, want) {
		t.Errorf("observer bytes for acquire = %v, want %v", got, want)
	}
}

// TestEntriesReturnAndCloseEnds: every call returns, and Close ends at once, in each state a
// call can find the adapter in: no connection ready yet, with the host's ctx already
// cancelled (cancelled at once) or live (unavailable at start + D); ready, with the host's
// ctx live or already cancelled; and closed (DESIGN §A5.4; SPEC API-1, API-22). Do labels a
// command for the send observer by its name in upper case, with SCRIPT's subcommand when it
// has one (§A5.12).
func TestEntriesReturnAndCloseEnds(t *testing.T) {
	pw := newPassword(t)
	sc := scripts.New("acquire", "-- entries return")
	var r Response
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	hang := fakeresp.Start(t, "rl_lib", pw)
	hang.HangAuth() // no connection ever becomes ready
	n := startBounded(t, Options{
		Addr: hang.Addr(), User: "rl_lib", Password: pw, StoreConns: 1, RefundConns: 1,
		DependencyDeadline: testD, Limits: testLimits(),
	})
	start := time.Now()
	mustWithin(t, "a call with no connection ready and the host's ctx cancelled", 2*time.Second, func() {
		r = n.Call(cancelled, acquireReq(sc, 1))
	})
	if el := time.Since(start); r.Outcome != Cancelled || r.Sent || el >= testD {
		t.Errorf("a call with no connection ready and the host's ctx cancelled = %v, Sent %v after %v; want cancelled, false, at once (before D)",
			r.Outcome, r.Sent, el)
	}
	start = time.Now()
	mustWithin(t, "a call with no connection ready", 2*time.Second, func() { r = n.Call(t.Context(), acquireReq(sc, 2)) })
	if el := time.Since(start); r.Outcome != Unavailable || r.Sent || el < testD {
		t.Errorf("a call with no connection ready = %v, Sent %v after %v; want unavailable, false at start + D", r.Outcome, r.Sent, el)
	}
	mustWithin(t, "Close with no connection ready", time.Second, n.Close)

	srv := fakeresp.Start(t, "rl_lib", pw)
	srv.Load(sc.Text)
	srv.Handle(sc.SHA, echoAcquire)
	obs := &observer{}
	a := startBounded(t, Options{
		Addr: srv.Addr(), User: "rl_lib", Password: pw, StoreConns: 1, RefundConns: 1,
		DependencyDeadline: testD, Limits: testLimits(), SendObserver: obs.record,
	})
	waitFor(t, "ready connections", 5*time.Second, func() bool {
		return a.ReadyConns(Call) == 1 && a.ReadyConns(Refund) == 1 && a.ReadyConns(Maintenance) == 1
	})
	mustWithin(t, "a call", 2*time.Second, func() { r = a.Call(t.Context(), acquireReq(sc, 1)) })
	if r.Outcome != OK || r.Reply.Elems[4].Int != 1 || r.SentCommands != 1 {
		t.Fatalf("a call = %v, SentCommands %d; want ok with its own reply, 1", r.Outcome, r.SentCommands)
	}
	mustWithin(t, "a call with the host's ctx cancelled", 2*time.Second, func() { r = a.Call(cancelled, acquireReq(sc, 2)) })
	if r.Outcome != Cancelled || r.Sent {
		t.Errorf("a call with the host's ctx cancelled = %v, Sent %v; want cancelled, false", r.Outcome, r.Sent)
	}
	before := len(obs.all())
	mustWithin(t, "Do(ping hello)", 2*time.Second, func() { _, r = a.Do(t.Context(), Call, "ping", "hello") })
	if r.Outcome != OK {
		t.Errorf("Do(ping hello) = %v, want ok", r.Outcome)
	}
	// Checked before the next Do is sent: a two-word command other than SCRIPT is labelled
	// by its name alone.
	if got, want := obs.all()[before:], []obsRec{{"PING", cmdLen("ping", "hello")}}; !slices.Equal(got, want) {
		t.Fatalf("Do(ping hello) records = %v, want %v", got, want)
	}
	mustWithin(t, "Do(SCRIPT)", 2*time.Second, func() { _, r = a.Do(t.Context(), Call, "SCRIPT") }) // no subcommand
	if !r.Sent {
		t.Errorf("Do(SCRIPT): Sent false, want true")
	}
	if got, want := obs.all()[before+1:], []obsRec{{"SCRIPT", cmdLen("SCRIPT")}}; !slices.Equal(got, want) {
		t.Errorf("Do(SCRIPT) records = %v, want %v", got, want)
	}
	mustWithin(t, "Close", time.Second, a.Close)
	mustWithin(t, "a call after Close", time.Second, func() { r = a.Call(t.Context(), acquireReq(sc, 3)) })
	if r.Outcome != Unavailable || r.Sent {
		t.Errorf("a call after Close = %v, Sent %v; want unavailable, false", r.Outcome, r.Sent)
	}
}

func TestSendObserver(t *testing.T) {
	e := newEnv(t)
	sc := scripts.New("acquire", "-- send observer")
	e.srv.Handle(sc.SHA, echoAcquire) // not loaded, so the call reloads it
	e.start()
	e.warm(8, 2)
	r := e.a.Call(t.Context(), Request{Kind: Call, Script: sc, Keys: []string{"rl:e:x:k:abc"}, Args: []string{"3"}})
	if r.Outcome != OK || r.SentCommands != 2 {
		t.Fatalf("Call = %v, SentCommands %d; want ok after one reload, 2", r.Outcome, r.SentCommands)
	}
	v, dr := e.a.Do(t.Context(), Call, "PING")
	if dr.Outcome != OK || v.Kind != resp.Simple || v.Str != "PONG" {
		t.Fatalf("Do(PING) = %s, %v; want +PONG, ok", v, dr.Outcome)
	}

	eval := cmdLen("EVALSHA", sc.SHA, "1", "rl:e:x:k:abc", "3")
	load := cmdLen("SCRIPT", "LOAD", sc.Text)
	auth := cmdLen("AUTH", "rl_lib", e.pw)
	ping := len("*1\r\n$4\r\nPING\r\n")
	if got, want := e.obs.bytesOf("acquire"), []int{eval, eval}; !slices.Equal(got, want) {
		t.Errorf("acquire records = %v, want %v (one per EVALSHA)", got, want)
	}
	if got, want := e.obs.bytesOf("SCRIPT LOAD"), []int{load}; !slices.Equal(got, want) {
		t.Errorf("SCRIPT LOAD records = %v, want %v", got, want)
	}
	if got, want := e.obs.bytesOf("PING"), []int{ping}; !slices.Equal(got, want) {
		t.Errorf("PING records = %v, want %v", got, want)
	}
	auths := e.obs.bytesOf("AUTH")
	if len(auths) != e.srv.Accepted() || len(auths) != 11 {
		t.Errorf("%d AUTH records for %d accepted connections, want one per dial (11)", len(auths), e.srv.Accepted())
	}
	for _, n := range auths {
		if n != auth {
			t.Errorf("AUTH record of %d bytes, want %d", n, auth)
		}
	}
	for _, rec := range e.obs.all() {
		switch rec.cmd {
		case "acquire", "SCRIPT LOAD", "PING", "AUTH":
		default:
			t.Errorf("unexpected observer record %q", rec.cmd)
		}
	}

	// Do labels a command by its name in upper case, SCRIPT with its subcommand.
	before := len(e.obs.all())
	if _, r := e.a.Do(t.Context(), Call, "script", "load", "return 1"); r.Outcome != OK {
		t.Errorf("Do(script load) = %v, want ok", r.Outcome)
	}
	if _, r := e.a.Do(t.Context(), Call, "ping", "hello"); r.Outcome != OK {
		t.Errorf("Do(ping hello) = %v, want ok", r.Outcome)
	}
	got := e.obs.all()[before:]
	want := []obsRec{{"SCRIPT LOAD", cmdLen("script", "load", "return 1")}, {"PING", cmdLen("ping", "hello")}}
	if !slices.Equal(got, want) {
		t.Errorf("Do records = %v, want %v", got, want)
	}
}

func TestCancelledBeforeSend(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	e.start()
	e.warm(8, 2)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	for _, ctx := range []context.Context{cancelled, expired} {
		if r := e.a.Call(ctx, acquireReq(sc, 1)); r.Outcome != Cancelled || r.Sent {
			t.Errorf("Call = %v, Sent %v; want cancelled, false", r.Outcome, r.Sent)
		}
		if rs := e.a.CallBatch(ctx, []Request{acquireReq(sc, 2)}); rs[0].Outcome != Cancelled || rs[0].Sent {
			t.Errorf("CallBatch = %v, Sent %v; want cancelled, false", rs[0].Outcome, rs[0].Sent)
		}
		if _, r := e.a.Do(ctx, Call, "PING"); r.Outcome != Cancelled || r.Sent {
			t.Errorf("Do = %v, Sent %v; want cancelled, false", r.Outcome, r.Sent)
		}
	}
	if recs := e.obs.bytesOf("acquire"); len(recs) != 0 {
		t.Errorf("observer heard %d EVALSHA writes, want none", len(recs))
	}
	if n := e.srv.Count("EVALSHA") + e.srv.Count("PING"); n != 0 {
		t.Errorf("the server received %d commands, want none", n)
	}
}

// TestNotSentHostOrDependency: a call that sends nothing (the store accepts TCP and never
// answers AUTH) is unavailable when its dependency deadline, start + D, comes first, even with
// the host's deadline only microseconds later, and cancelled when the host's own deadline or
// cancellation comes first (SPEC API-1; plan S3 step 8). Per case, 201 concurrent calls, a
// third each through Call, CallBatch (2 requests) and Do, each from its own start. No slot
// dials before every call has taken its start (Options.Start): a failed dial wakes the
// waiting calls to fail fast (DESIGN §A5.4), so each AUTH timeout, D after its dial began,
// comes after every call's start + D, and only the two deadlines decide.
func TestNotSentHostOrDependency(t *testing.T) {
	cases := []struct {
		name string
		host func(parent context.Context, start time.Time) (context.Context, context.CancelFunc)
		want Outcome
		lo   time.Duration // no call may end earlier than this after its start
	}{
		{"host deadline 5 µs after start + D", hostDeadline(testD + 5*time.Microsecond), Unavailable, testD},
		{"host deadline 1 ms after start + D", hostDeadline(testD + time.Millisecond), Unavailable, testD},
		{"host deadline 100 ms after start", hostDeadline(100 * time.Millisecond), Cancelled, 100 * time.Millisecond},
		{"host cancels 50 ms after start", hostCancel(50 * time.Millisecond), Cancelled, 50 * time.Millisecond},
	}
	entries := []string{"Call", "CallBatch", "Do"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.HangAuth() // no connection becomes ready: every call waits until a deadline
			sc := e.script("acquire", echoAcquire)
			gate := make(chan struct{}) // closed once every call has taken its start
			e.start(func(o *Options) { o.Start = gate })
			type ended struct {
				rs []Response
				el time.Duration
			}
			res := make([]ended, 201)
			var wg, started sync.WaitGroup
			started.Add(len(res))
			for i := range res {
				wg.Add(1)
				go func() {
					defer wg.Done()
					start := time.Now()
					started.Done()
					ctx, cancel := tc.host(t.Context(), start)
					defer cancel()
					var rs []Response
					switch i % 3 {
					case 0:
						rs = []Response{e.a.call(ctx, start, acquireReq(sc, i))}
					case 1:
						rs = e.a.callBatch(ctx, start, []Request{acquireReq(sc, i), acquireReq(sc, i)})
					default:
						_, r := e.a.do(ctx, start, Call, []string{"PING"})
						rs = []Response{r}
					}
					res[i] = ended{rs, time.Since(start)}
				}()
			}
			started.Wait()
			close(gate) // the slots dial from now on, after every call's start
			wg.Wait()
			for k, entry := range entries {
				outcomes := map[Outcome]int{}
				early, sent := 0, 0
				for i := k; i < len(res); i += 3 {
					for _, r := range res[i].rs {
						outcomes[r.Outcome]++
						if r.Sent || r.SentCommands != 0 {
							sent++
						}
					}
					if res[i].el < tc.lo {
						early++
					}
				}
				if len(outcomes) != 1 || outcomes[tc.want] == 0 {
					t.Errorf("%s: outcomes %v, want every one %v", entry, outcomes, tc.want)
				}
				if sent != 0 {
					t.Errorf("%s: %d responses report a sent command, want none", entry, sent)
				}
				if early != 0 {
					t.Errorf("%s: %d calls ended less than %v after their start", entry, early, tc.lo)
				}
			}
			if n := e.srv.Count("EVALSHA") + e.srv.Count("PING"); n != 0 {
				t.Errorf("the server received %d commands, want none", n)
			}
		})
	}
}

// TestDeadlineAfterLateWrite: CallBatch and Do end at their deadline, start + D, also when
// their command left late and its reply is due after start + D but before the stall rule
// (the command's write + D) could end it (plan S3 step 8: CallBatch runs within one
// deadline min(ctx deadline, start + D), and Do the same way). Here the call connection
// becomes ready 150 ms after the call starts, and the store answers each command 250 ms
// after it arrives: at 400 ms, while the stall rule waits until 450 ms.
func TestDeadlineAfterLateWrite(t *testing.T) {
	for _, entry := range []string{"CallBatch", "Do"} {
		t.Run(entry, func(t *testing.T) {
			e := newEnv(t)
			sc := e.script("acquire", echoAcquire)
			e.srv.Delay("EVALSHA", 250*time.Millisecond)
			e.srv.Delay("PING", 250*time.Millisecond)
			began := time.Now() // before the call starts, so its deadline comes D after this or later
			e.start(withConns(1, 1), func(o *Options) {
				o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
					select { // no connection is ready until 150 ms after began
					case <-time.After(time.Until(began.Add(150 * time.Millisecond))):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				}
			})
			var rs []Response
			label, sent := "EVALSHA", 2
			switch entry {
			case "CallBatch":
				rs = e.a.CallBatch(t.Context(), []Request{acquireReq(sc, 1), acquireReq(sc, 2)})
			default:
				_, r := e.a.Do(t.Context(), Call, "PING")
				rs, label, sent = []Response{r}, "PING", 1
			}
			el := time.Since(began)
			for i, r := range rs {
				if r.Outcome != OutcomeUnknown || !r.Sent || r.SentCommands != 1 {
					t.Errorf("%s response %d = %v, Sent %v, SentCommands %d; want outcome_unknown, true, 1",
						entry, i+1, r.Outcome, r.Sent, r.SentCommands)
				}
			}
			if el < testD || el > testD+150*time.Millisecond {
				t.Errorf("%s ended after %v, want 300–450 ms (its deadline, start + D)", entry, el)
			}
			if n := e.srv.Count(label); n != sent {
				t.Errorf("the server received %d %s, want %d", n, label, sent)
			}
		})
	}
}

// hostDeadline gives the host's ctx the deadline d after the call's start.
func hostDeadline(d time.Duration) func(context.Context, time.Time) (context.Context, context.CancelFunc) {
	return func(parent context.Context, start time.Time) (context.Context, context.CancelFunc) {
		return context.WithDeadline(parent, start.Add(d))
	}
}

// hostCancel gives the host's ctx no deadline; the host cancels it d after the call's start.
func hostCancel(d time.Duration) func(context.Context, time.Time) (context.Context, context.CancelFunc) {
	return func(parent context.Context, start time.Time) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		timer := time.AfterFunc(time.Until(start.Add(d)), cancel)
		return ctx, func() { timer.Stop(); cancel() }
	}
}

// TestZeroByteWriteAfterDependencyDeadline: a write that sends nothing and fails only after
// the call's dependency deadline passed, the host cancelling later still, is unavailable:
// start + D ended the call first, the same rule as before a write (SPEC API-1; plan S3
// step 8). Each call starts 250 ms in the past (it spent them waiting), so start + D
// comes 50 ms after the write starts and the host cancels 150 ms after that.
func TestZeroByteWriteAfterDependencyDeadline(t *testing.T) {
	cases := []struct {
		entry, label string
		writes       int // commands in the write
	}{
		{"Call", "acquire", 1},
		{"CallBatch", "acquire", 2},
		{"Do", "PING", 1},
	}
	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			e := newEnv(t)
			sc := e.script("acquire", echoAcquire)
			host, cancelHost := context.WithCancel(t.Context())
			defer cancelHost()
			var start time.Time // set before the call; read only by its own write
			var taps tapList
			e.start(withConns(1, 1), withTap(&taps, func(_ *tapConn, p []byte) (bool, int, error) {
				if !bytes.Contains(p, []byte("EVALSHA")) && !bytes.Contains(p, []byte("PING")) {
					return false, 0, nil
				}
				time.Sleep(time.Until(start.Add(testD + 150*time.Millisecond))) // start + D has passed
				cancelHost()                                                    // the host gives up only now
				return true, 0, injectedError{}
			}))
			e.warm(1, 1)
			start = time.Now().Add(50*time.Millisecond - testD)
			var rs []Response
			switch tc.entry {
			case "Call":
				rs = []Response{e.a.call(host, start, acquireReq(sc, 1))}
			case "CallBatch":
				rs = e.a.callBatch(host, start, []Request{acquireReq(sc, 1), acquireReq(sc, 2)})
			default:
				_, r := e.a.do(host, start, Call, []string{"PING"})
				rs = []Response{r}
			}
			for i, r := range rs {
				if r.Outcome != Unavailable || r.Sent || r.SentCommands != 0 {
					t.Errorf("%s response %d = %v, Sent %v, SentCommands %d; want unavailable, false, 0",
						tc.entry, i+1, r.Outcome, r.Sent, r.SentCommands)
				}
			}
			if got, want := e.obs.bytesOf(tc.label), make([]int, tc.writes); !slices.Equal(got, want) {
				t.Errorf("observer bytes for %s = %v, want %v: the write was reached and sent nothing", tc.label, got, want)
			}
			if host.Err() == nil {
				t.Error("the host's ctx is live: the write did not fail after the host cancelled")
			}
		})
	}
}

// lateCancelHost is a host ctx with no deadline that its host cancels just after at, the
// call's start + D: the cancel lands on the first read of Err at or after at. Nothing reads
// Err before the call has ended; the adapter reads it to classify a call that sent nothing.
// So the cancel lands after start + D ended the call and before the call is classified,
// every time: the window a real cancel just after start + D can hit.
type lateCancelHost struct {
	context.Context
	cancel context.CancelFunc
	at     time.Time
}

// lateCancel returns a lateCancelHost under parent that cancels at at, and its cancel.
func lateCancel(parent context.Context, at time.Time) (lateCancelHost, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	return lateCancelHost{Context: ctx, cancel: cancel, at: at}, cancel
}

func (h lateCancelHost) Err() error {
	if !time.Now().Before(h.at) {
		h.cancel()
	}
	return h.Context.Err()
}

// startEachEntry starts one call each through Call, CallBatch (2 requests) and Do, all from
// start, each with a host that cancels just after start + D (lateCancelHost). The function
// it returns waits for them and checks that every response is unavailable with nothing sent
// and that no call ended before its start + D.
func startEachEntry(t *testing.T, e *env, sc scripts.Script, start time.Time) (wait func()) {
	entries := []string{"Call", "CallBatch", "Do"}
	rs := make([][]Response, len(entries))
	early := make([]bool, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host, cancel := lateCancel(t.Context(), start.Add(testD))
			defer cancel()
			switch entry {
			case "Call":
				rs[i] = []Response{e.a.call(host, start, acquireReq(sc, 1))}
			case "CallBatch":
				rs[i] = e.a.callBatch(host, start, []Request{acquireReq(sc, 2), acquireReq(sc, 3)})
			default:
				_, r := e.a.do(host, start, Call, []string{"PING"})
				rs[i] = []Response{r}
			}
			early[i] = time.Now().Before(start.Add(testD))
		}()
	}
	return func() {
		t.Helper()
		// A call stuck for good fails the test here, not by the test binary's timeout.
		if !within(t, "the 3 calls", 2*time.Second, wg.Wait) {
			t.FailNow()
		}
		for i, entry := range entries {
			for j, r := range rs[i] {
				if r.Outcome != Unavailable || r.Sent || r.SentCommands != 0 {
					t.Errorf("%s response %d = %v, Sent %v, SentCommands %d; want unavailable, false, 0",
						entry, j+1, r.Outcome, r.Sent, r.SentCommands)
				}
			}
			if early[i] {
				t.Errorf("%s ended before its start + D", entry)
			}
		}
	}
}

// TestHostCancelAfterDependencyDeadline: a call that start + D ended while it picked a
// connection sent nothing and is unavailable, also when the host cancels right after
// start + D, before the call is classified (SPEC API-1; plan S3 step 8: cancelled only if
// the host's ctx ended first). So picking returns the call ctx's cause. The store accepts
// TCP and never answers AUTH, so no connection becomes ready. In one case the calls start
// now and wait for a connection until start + D; in the other they reach the pick with
// start + D already passed (they start D + 1 ms in the past).
func TestHostCancelAfterDependencyDeadline(t *testing.T) {
	cases := []struct {
		name string
		ago  time.Duration // how far in the past the calls start
	}{
		{"start + D comes while waiting for a connection", 0},
		{"start + D passed before the pick", testD + time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.HangAuth() // no connection ever becomes ready
			sc := e.script("acquire", echoAcquire)
			e.start()
			startEachEntry(t, e, sc, time.Now().Add(-tc.ago))()
			if n := e.srv.Count("EVALSHA") + e.srv.Count("PING"); n != 0 {
				t.Errorf("the server received %d commands, want none", n)
			}
		})
	}
}

// TestHostCancelWhileWaitingForWriteLock: a call that start + D ended while it waited for a
// connection's write lock sent nothing and is unavailable, also when the host cancels right
// after start + D, before the call is classified (SPEC API-1; plan S3 step 8). So waiting
// for the write lock returns the call ctx's cause. On the one call connection, call A's
// write hangs, sending nothing and holding the write lock, while a call each through Call,
// CallBatch and Do waits for it. Their start lies 150 ms before A's, so their start + D
// comes 150 ms before A's own deadline and before the stall rule (A's write + D) could close
// the connection, which would send them back to picking.
func TestHostCancelWhileWaitingForWriteLock(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	release := make(chan struct{})
	var held atomic.Bool
	var taps tapList
	e.start(withConns(1, 1), withTap(&taps, func(_ *tapConn, p []byte) (bool, int, error) {
		if !bytes.Contains(p, []byte("EVALSHA")) || !held.CompareAndSwap(false, true) {
			return false, 0, nil
		}
		select { // A's write sends nothing and hangs until release
		case <-release:
		case <-t.Context().Done():
		}
		return true, 0, injectedError{}
	}))
	e.warm(1, 1)
	set := e.a.sets[Call]
	set.mu.Lock()
	pc := set.ready[0]
	set.mu.Unlock()
	accepted := e.srv.Accepted()

	began := time.Now() // before A starts: A's deadline and its write's stall deadline come D after this or later
	doneA := make(chan Response, 1)
	go func() { doneA <- e.a.Call(t.Context(), acquireReq(sc, 1)) }()
	waitFor(t, "A's write to start", time.Second, held.Load)
	wait := startEachEntry(t, e, sc, began.Add(-150*time.Millisecond))
	waitFor(t, "the 3 calls to wait for the write lock", time.Second, func() bool {
		return pc.load.Load() == 5 // A's command, then 1 + 2 + 1 reserved by Call, CallBatch and Do
	})
	wait()
	close(release)
	select { // A stuck for good fails the test here, not by the test binary's timeout
	case a := <-doneA:
		if a.Outcome != Unavailable || a.Sent {
			t.Errorf("A = %v, Sent %v; want unavailable, false: its write sent nothing", a.Outcome, a.Sent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A still running 2 s after its write was released")
	}
	select {
	case <-pc.done:
		t.Error("the call connection closed: the calls did not end waiting for its write lock")
	default:
	}
	if got := e.srv.Accepted(); got != accepted {
		t.Errorf("accepted connections went from %d to %d, want no redial", accepted, got)
	}
	if got := e.obs.bytesOf("acquire"); !slices.Equal(got, []int{0}) {
		t.Errorf("observer bytes for acquire = %v, want [0]: only A's write began, and it sent nothing", got)
	}
	if n := e.srv.Count("EVALSHA") + e.srv.Count("PING"); n != 0 {
		t.Errorf("the server received %d commands, want none", n)
	}
}

// doneCancelHost is a host ctx with no deadline whose Done channel never closes and whose Err
// turns context.Canceled once Done has been asked for: a host cancel that lands after pick,
// which takes a ready connection asking only for Err, and while lockWrite's select, which asks
// for Done, takes the free write lock. asked records that Done was asked for.
type doneCancelHost struct {
	context.Context
	never chan struct{} // never closed
	asked atomic.Bool
}

func (h *doneCancelHost) Deadline() (time.Time, bool) { return time.Time{}, false }

func (h *doneCancelHost) Done() <-chan struct{} {
	h.asked.Store(true)
	return h.never
}

func (h *doneCancelHost) Err() error {
	if h.asked.Load() {
		return context.Canceled
	}
	return nil
}

// TestHostCancelAfterWriteLock: a call whose host cancels after it picked a connection, while
// it took the write lock, never starts its write: the check between the two finds the call
// ended, so it sent nothing and its error is the host's cancel (SPEC API-1, API-22; DESIGN
// §A5.4: no write starts after the call's deadline or its host's end). exchange runs directly
// on a warmed adapter with one call connection, with a doneCancelHost: pick takes the ready
// connection while the host is live, and lockWrite takes the free lock just as the host
// cancels.
func TestHostCancelAfterWriteLock(t *testing.T) {
	e := newEnv(t)
	e.start(withConns(1, 1))
	e.warm(1, 1)
	host := &doneCancelHost{Context: t.Context(), never: make(chan struct{})}
	var out []exchanged
	mustWithin(t, "exchange(PING)", 2*time.Second, func() {
		out = e.a.exchange(host, e.a.sets[Call], []command{{label: "PING", args: []string{"PING"}}})
	})
	if !host.asked.Load() {
		t.Fatal("exchange never asked the host ctx for Done: lockWrite's select did not run")
	}
	if len(out) != 1 || out[0].sent || !errors.Is(out[0].err, context.Canceled) {
		t.Fatalf("exchange = %+v; want one command, not sent, with context.Canceled", out)
	}
	if got := e.obs.bytesOf("PING"); len(got) != 0 {
		t.Errorf("observer heard PING writes of %v bytes, want none", got)
	}
	if n := e.srv.Count("PING"); n != 0 {
		t.Errorf("the server received %d PING, want none", n)
	}
}

// pendingHost is a live host ctx whose deadline has passed: how classification finds it
// while the timer goroutine that ends it has not run yet.
type pendingHost struct {
	context.Context
	deadline time.Time
}

func (h pendingHost) Deadline() (time.Time, bool) { return h.deadline, true }

// TestNotSentOutcome: the outcome of a command that never left, for each state the host's
// ctx and the call's end can be in when it is classified (SPEC API-1, API-22; plan S3 step
// 8: cancelled only if the host's ctx ended first). The goroutines that end the two
// contexts run in either order, so the rows include a host deadline after start + D whose
// end arrived first, and a host deadline before it that has passed but not yet arrived.
func TestNotSentOutcome(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second) // the call's start + D, already passed
	deadlineAt := func(d time.Time) context.Context {
		ctx, cancel := context.WithDeadline(t.Context(), d)
		t.Cleanup(cancel)
		return ctx
	}
	cancelled := func(parent context.Context) context.Context {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx
	}
	rows := []struct {
		name string
		host context.Context
		dep  time.Time // start + D
		err  error     // what ended the exchange: the call ctx's cause, or a store failure
		want Outcome
	}{
		{"start + D ended the call, then the host cancelled", cancelled(t.Context()), past, errDependencyDeadline, Unavailable},
		{"host deadline 5 µs after start + D, its end arrived first", deadlineAt(past.Add(5 * time.Microsecond)), past, context.DeadlineExceeded, Unavailable},
		{"host deadline equal to start + D", deadlineAt(past), past, context.DeadlineExceeded, Unavailable},
		{"host deadline 1 ms before start + D", deadlineAt(past.Add(-time.Millisecond)), past, context.DeadlineExceeded, Cancelled},
		{"host deadline before start + D passed, its end not yet arrived", pendingHost{t.Context(), past.Add(-time.Millisecond)}, past, injectedError{}, Cancelled},
		{"host deadline before start + D not yet reached", deadlineAt(now.Add(time.Hour)), now.Add(2 * time.Hour), injectedError{}, Unavailable},
		{"host cancelled", cancelled(t.Context()), past, context.Canceled, Cancelled},
		{"host cancelled, its deadline after start + D", cancelled(deadlineAt(now.Add(time.Hour))), past, context.Canceled, Cancelled},
		{"adapter closed, host cancelled", cancelled(t.Context()), past, errAdapterClosed, Unavailable},
		{"no connection within the backoff, host cancelled", cancelled(t.Context()), past, errFailFast, Unavailable},
		{"the store refused the last dial, host cancelled", cancelled(t.Context()), past, &refusedError{reply: "WRONGPASS invalid username-password pair or user is disabled."}, Unavailable},
		{"nothing written, host live", t.Context(), now.Add(time.Hour), injectedError{}, Unavailable},
	}
	for _, r := range rows {
		if got := notSent(r.host, r.dep, r.err); got != r.want {
			t.Errorf("%s: notSent = %v, want %v", r.name, got, r.want)
		}
	}
}

func TestLifetimeEnds(t *testing.T) {
	e := newEnv(t)
	e.srv.HangAuth() // no connection ever becomes ready, so calls wait
	sc := e.script("acquire", echoAcquire)
	life, end := context.WithCancel(t.Context())
	a := New(life, Options{
		Addr: e.srv.Addr(), User: "rl_lib", Password: e.pw, StoreConns: 8, RefundConns: 2,
		DependencyDeadline: testD, Limits: testLimits(),
	})
	t.Cleanup(a.Close)
	done := make(chan Response, 1)
	go func() { done <- a.Call(t.Context(), acquireReq(sc, 1)) }()
	time.Sleep(50 * time.Millisecond)
	ended := time.Now()
	end()
	r := <-done
	if el := time.Since(ended); el >= 150*time.Millisecond {
		t.Errorf("the waiting call ended %v after the lifetime, want at once", el)
	}
	if r.Outcome != Unavailable || r.Sent {
		t.Errorf("the waiting call = %v, Sent %v; want unavailable, false", r.Outcome, r.Sent)
	}
	if r := a.Call(t.Context(), acquireReq(sc, 2)); r.Outcome != Unavailable || r.Sent {
		t.Errorf("a later call = %v, Sent %v; want unavailable, false", r.Outcome, r.Sent)
	}
}

// TestCallSecondsObserved (plan S3 Tests (a), S9 step 2; DESIGN §A5.11): the count of
// rl_store_call_seconds{script="acquire"} rises by exactly N after N calls; then one call of
// each of the 7 scripts rl_lib runs (§A2), each on its production kind and answered with a
// valid reply (the §A4.9 shapes of TestDecodeLayouts), raises its own series' count by exactly
// 1 and leaves the other six unchanged, so no script is observed under another's label.
func TestCallSecondsObserved(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	const fp = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	constant := func(v resp.Value) fakeresp.Handler { return func(_, _ []string) resp.Value { return v } }
	rows := []struct {
		label string // the script label value, as DESIGN §A5.11 lists it
		kind  Kind
		sc    scripts.Script
		h     fakeresp.Handler
	}{
		{"acquire", Call, scripts.Acquire, echoAcquire},
		{"refund", Refund, scripts.Refund, constant(rep(3, tNow, "", 1_000_000_000))},
		{"due_entries", Maintenance, scripts.DueEntries, constant(rep(0, tNow, "", 0))},
		{"cleanup_entry", Maintenance, scripts.CleanupEntry, constant(rep(30, tNow, "", 0))},
		{"reclaim", Maintenance, scripts.Reclaim, constant(rep(40, 0, "", 1))},
		{"read_epoch", Call, scripts.ReadEpoch, constant(rep(0, 0, "", id, fp, tNow, 3, tNow+7, 10, 2, 0))},
		{"hold", Call, scripts.Hold, constant(rep(40, 0, "", 0))},
	}
	e := newEnv(t)
	for _, r := range rows {
		e.srv.Load(r.sc.Text)
		e.srv.Handle(r.sc.SHA, r.h)
	}
	e.start()
	e.warm(8, 2)
	counts := func() map[string]uint64 {
		out := map[string]uint64{}
		for _, f := range e.reg.Snapshot().Families {
			if f.Name != "rl_store_call_seconds" {
				continue
			}
			for _, s := range f.Series {
				out[s.LabelValue] = s.Count
			}
		}
		for _, r := range rows {
			if _, ok := out[r.label]; !ok {
				t.Fatalf("no rl_store_call_seconds{script=%q} series", r.label)
			}
		}
		return out
	}
	// rose checks that between before and after the series label rose by n and every other
	// series of the 7 stayed put.
	rose := func(what string, before, after map[string]uint64, label string, n uint64) {
		t.Helper()
		for _, r := range rows {
			want := uint64(0)
			if r.label == label {
				want = n
			}
			if got := after[r.label] - before[r.label]; got != want {
				t.Errorf("%s: rl_store_call_seconds{script=%q} count rose by %d, want %d", what, r.label, got, want)
			}
		}
	}

	const n = 7
	before := counts()
	for i := range n {
		if r := e.a.Call(t.Context(), Request{Kind: Call, Script: scripts.Acquire, Args: []string{strconv.Itoa(i + 1)}}); r.Outcome != OK || r.Reply.Elems[4].Int != int64(i+1) {
			t.Fatalf("call %d = %v, want ok with its own reply", i+1, r.Outcome)
		}
	}
	rose(strconv.Itoa(n)+" acquire calls", before, counts(), "acquire", n)

	for _, r := range rows {
		before := counts()
		if got := e.a.Call(t.Context(), Request{Kind: r.kind, Script: r.sc, Args: []string{"1"}}); got.Outcome != OK || got.SentCommands != 1 {
			t.Fatalf("%s call = %v, SentCommands %d; want ok, 1", r.label, got.Outcome, got.SentCommands)
		}
		rose("one "+r.label+" call", before, counts(), r.label, 1)
	}
}

func TestCloseFailsInFlight(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	defer close(release)
	sc := e.script("acquire", blockUntil(t.Context(), release))
	e.start()
	e.warm(8, 2)
	var wg sync.WaitGroup
	results := make([]Response, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.a.Call(t.Context(), acquireReq(sc, i+1))
		}()
	}
	waitFor(t, "3 commands written", 2*time.Second, func() bool { return len(e.obs.bytesOf("acquire")) == 3 })
	closed := time.Now()
	ended := make(chan time.Duration, 1)
	go func() { // a Close that never returns fails the select below, not the test's timeout
		e.a.Close()
		wg.Wait()
		ended <- time.Since(closed)
	}()
	select {
	case el := <-ended:
		if el >= 200*time.Millisecond {
			t.Errorf("Close and the calls in flight took %v, want them to end at once", el)
		}
	case <-time.After(time.Second):
		// Returning ends t.Context(), the adapter's lifetime, which ends the stuck Close.
		t.Fatal("Close and the calls in flight still running 1 s after Close began")
	}
	for i, r := range results {
		if r.Outcome != OutcomeUnknown || !r.Sent {
			t.Errorf("call %d = %v, Sent %v; want outcome_unknown, true", i+1, r.Outcome, r.Sent)
		}
	}

	records := len(e.obs.all())
	if r := e.a.Call(t.Context(), acquireReq(sc, 4)); r.Outcome != Unavailable || r.Sent {
		t.Errorf("Call after Close = %v, Sent %v; want unavailable, false", r.Outcome, r.Sent)
	}
	if rs := e.a.CallBatch(t.Context(), []Request{acquireReq(sc, 5)}); rs[0].Outcome != Unavailable || rs[0].Sent {
		t.Errorf("CallBatch after Close = %v, Sent %v; want unavailable, false", rs[0].Outcome, rs[0].Sent)
	}
	if _, r := e.a.Do(t.Context(), Call, "PING"); r.Outcome != Unavailable || r.Sent {
		t.Errorf("Do after Close = %v, Sent %v; want unavailable, false", r.Outcome, r.Sent)
	}
	// The closed adapter decides before the host's ctx (plan S3 step 8: after Close, calls
	// return unavailable): also with a ctx already cancelled or past its deadline.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	for _, h := range []struct {
		name string
		ctx  context.Context
	}{{"cancelled", cancelled}, {"expired", expired}} {
		if r := e.a.Call(h.ctx, acquireReq(sc, 6)); r.Outcome != Unavailable || r.Sent {
			t.Errorf("Call after Close, %s ctx = %v, Sent %v; want unavailable, false", h.name, r.Outcome, r.Sent)
		}
		if rs := e.a.CallBatch(h.ctx, []Request{acquireReq(sc, 7)}); rs[0].Outcome != Unavailable || rs[0].Sent {
			t.Errorf("CallBatch after Close, %s ctx = %v, Sent %v; want unavailable, false", h.name, rs[0].Outcome, rs[0].Sent)
		}
		if _, r := e.a.Do(h.ctx, Call, "PING"); r.Outcome != Unavailable || r.Sent {
			t.Errorf("Do after Close, %s ctx = %v, Sent %v; want unavailable, false", h.name, r.Outcome, r.Sent)
		}
	}
	if n := len(e.obs.all()); n != records {
		t.Errorf("%d observer records after Close, want none: nothing is sent", n-records)
	}
	for _, k := range []Kind{Call, Refund, Maintenance} {
		if n := e.a.ReadyConns(k); n != 0 {
			t.Errorf("ReadyConns(%v) = %d after Close, want 0", k, n)
		}
	}
	e.a.Close() // a second Close is harmless
}

// TestStartHoldsDials: with Options.Start set, no slot dials until it is closed, and then
// every slot connects; when the lifetime ends first (Close), the slots end without dialing
// and Close returns at once (plan S5 step 2: core holds the dials while a test's store
// wrapper runs, so a wrapper that returns no adapter leaves no connection).
func TestStartHoldsDials(t *testing.T) {
	e := newEnv(t)
	start := make(chan struct{})
	e.start(func(o *Options) { o.Start = start })
	time.Sleep(100 * time.Millisecond) // far longer than 11 dials to 127.0.0.1 take
	if n := e.srv.Accepted(); n != 0 {
		t.Fatalf("the store accepted %d connections before Start was closed, want 0", n)
	}
	close(start)
	e.warm(8, 2) // every slot connects

	held := newEnv(t)
	held.start(func(o *Options) { o.Start = make(chan struct{}) })
	done := goDone(held.a.Close)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close waited for slots that never dialed")
	}
	if n := held.srv.Accepted(); n != 0 {
		t.Fatalf("the store accepted %d connections from an adapter closed before Start, want 0", n)
	}
}
