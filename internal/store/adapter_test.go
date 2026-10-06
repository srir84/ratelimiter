package store

import (
	"bytes"
	"context"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
)

// The production adapter satisfies the interface that limitertest aliases (§A5.12).
var _ Adapter = (*StoreAdapter)(nil)

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
// third each through Call, CallBatch (2 requests) and Do, each from its own start.
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
			e.start()
			type ended struct {
				rs []Response
				el time.Duration
			}
			res := make([]ended, 201)
			var wg sync.WaitGroup
			for i := range res {
				wg.Add(1)
				go func() {
					defer wg.Done()
					start := time.Now()
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
		wg.Wait()
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
	if a := <-doneA; a.Outcome != Unavailable || a.Sent {
		t.Errorf("A = %v, Sent %v; want unavailable, false: its write sent nothing", a.Outcome, a.Sent)
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

func TestCallSecondsObserved(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	e.start()
	e.warm(8, 2)
	count := func() uint64 {
		for _, f := range e.reg.Snapshot().Families {
			if f.Name != "rl_store_call_seconds" {
				continue
			}
			for _, s := range f.Series {
				if s.LabelValue == "acquire" {
					return s.Count
				}
			}
		}
		t.Fatal("no rl_store_call_seconds{script=\"acquire\"} series")
		return 0
	}
	const n = 7
	before := count()
	for i := range n {
		if r := e.a.Call(t.Context(), acquireReq(sc, i+1)); r.Outcome != OK {
			t.Fatalf("call %d = %v, want ok", i+1, r.Outcome)
		}
	}
	if got := count() - before; got != n {
		t.Errorf("rl_store_call_seconds{script=\"acquire\"} count rose by %d after %d calls, want %d", got, n, n)
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
