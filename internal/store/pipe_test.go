package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/store/resp"
)

func TestPipelinedRepliesMatchInOrder(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	e.start()
	e.warm(8, 2)
	var matched atomic.Int64
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				g := i*100 + j + 1
				r := e.a.Call(t.Context(), acquireReq(sc, g))
				if r.Outcome == OK && r.Reply.Code == 1 && r.Reply.Elems[4].Int == int64(g) {
					matched.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := matched.Load(); got != 6400 {
		t.Errorf("%d of 6,400 replies matched their calls", got)
	}
}

// earlyReplyConn is a store connection whose EVALSHA writes return only after the reply to
// their bytes has been read and handled: once the reader, holding replyLen more bytes than
// when the write began, asks for more, or the connection closes. So each reply arrives
// before its Write returns. A write that waits 2 s gives up and sets late.
type earlyReplyConn struct {
	net.Conn
	replyLen int
	late     atomic.Bool

	mu      sync.Mutex
	read    int  // bytes the reader has received
	reading bool // a Read is in progress
	closed  bool
}

func (c *earlyReplyConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	c.reading = true
	c.mu.Unlock()
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.reading = false
	c.read += n
	c.mu.Unlock()
	return n, err
}

func (c *earlyReplyConn) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte("EVALSHA")) {
		return c.Conn.Write(p)
	}
	c.mu.Lock()
	before := c.read
	c.mu.Unlock()
	n, err := c.Conn.Write(p)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(100 * time.Microsecond) {
		c.mu.Lock()
		handled := c.closed || (c.read >= before+c.replyLen && c.reading)
		c.mu.Unlock()
		if handled {
			break
		}
		if time.Now().After(deadline) {
			c.late.Store(true)
			break
		}
	}
	return n, err
}

func (c *earlyReplyConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.Conn.Close()
}

// TestReplyBeforeWriteReturns: a call joins the FIFO before its bytes are written, so a
// reply that arrives before the Write returns still finds its call (plan S3 Watch out 1).
func TestReplyBeforeWriteReturns(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	// The reply to g = 1, counted by hand; g = 2 and g = 3 give replies of the same length.
	const reply = "*7\r\n:1\r\n:1760000000000000\r\n$0\r\n\r\n:0\r\n:1\r\n:1\r\n:1760000060000000\r\n"
	var mu sync.Mutex
	var conns []*earlyReplyConn
	e.start(withConns(1, 1), func(o *Options) {
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			ec := &earlyReplyConn{Conn: c, replyLen: len(reply)}
			mu.Lock()
			conns = append(conns, ec)
			mu.Unlock()
			return ec, nil
		}
	})
	e.warm(1, 1)
	accepted := e.srv.Accepted()
	for g := 1; g <= 3; g++ {
		if r := e.a.Call(t.Context(), acquireReq(sc, g)); r.Outcome != OK || r.Reply.Elems[4].Int != int64(g) {
			t.Errorf("call %d = %v, want ok with its own reply", g, r.Outcome)
		}
	}
	if got := e.srv.Accepted(); got != accepted {
		t.Errorf("accepted connections went from %d to %d, want no redial", accepted, got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range conns {
		if c.late.Load() {
			t.Error("a write waited 2 s for its reply: the reply did not arrive before the write returned")
		}
	}
}

func TestOwnDeadlineAfterSend(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", func(keys, argv []string) resp.Value {
		if argv[0] == "1" { // A's reply comes 200 ms late, still below D = 300 ms
			select {
			case <-time.After(200 * time.Millisecond):
			case <-t.Context().Done():
			}
		}
		return echoAcquire(keys, argv)
	})
	e.start(withConns(1, 1))
	e.warm(1, 1)
	accepted := e.srv.Accepted()

	start := time.Now() // before the ctx's deadline is set, so it is start + 100 ms
	ctx, cancel := context.WithDeadline(t.Context(), start.Add(100*time.Millisecond))
	defer cancel()
	a := e.a.Call(ctx, acquireReq(sc, 1))
	el := time.Since(start)
	if a.Outcome != OutcomeUnknown || !a.Sent || a.SentCommands != 1 || a.Raw != "" {
		t.Errorf("A = %v, Sent %v, SentCommands %d, Raw %q; want outcome_unknown, true, 1, no reply",
			a.Outcome, a.Sent, a.SentCommands, a.Raw)
	}
	if el < 100*time.Millisecond || el >= 150*time.Millisecond {
		t.Errorf("A ended after %v, want ≈ 100 ms (its own deadline)", el)
	}
	// B goes on the same, only, call connection while A's reply is still due.
	b := e.a.Call(t.Context(), acquireReq(sc, 2))
	if b.Outcome != OK || b.Reply.Elems[4].Int != 2 {
		t.Errorf("B = %v with g %v, want ok with its own reply (g 2)", b.Outcome, b.Reply.Elems)
	}
	if got := e.srv.Accepted(); got != accepted {
		t.Errorf("accepted connections went from %d to %d, want no redial", accepted, got)
	}
}

func TestStallClosesConnection(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	e.start(withConns(1, 1))
	e.warm(1, 1)
	accepted := e.srv.Accepted()
	e.srv.Stall()

	// Call 1 is the oldest write; calls 2 and 3 follow 100 ms later, so their own deadlines
	// come at 400 ms or after, and only the stall rule (oldest write + D) can end them at
	// 300 ms.
	start := time.Now() // before the oldest write
	var wg sync.WaitGroup
	results := make([]Response, 3)
	began := make([]time.Duration, 3)
	ended := make([]time.Duration, 3)
	launch := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			began[i] = time.Since(start)
			results[i] = e.a.Call(t.Context(), acquireReq(sc, i+1))
			ended[i] = time.Since(start)
		}()
	}
	launch(0)
	waitFor(t, "call 1 written", time.Second, func() bool { return len(e.obs.bytesOf("acquire")) == 1 })
	time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
	launch(1)
	launch(2)
	wg.Wait()
	for i, r := range results {
		if r.Outcome != OutcomeUnknown || !r.Sent {
			t.Errorf("call %d = %v, Sent %v; want outcome_unknown, true", i+1, r.Outcome, r.Sent)
		}
		if ended[i] < 300*time.Millisecond || ended[i] > 450*time.Millisecond {
			t.Errorf("call %d ended %v after the oldest write, want 300–450 ms", i+1, ended[i])
		}
		if i > 0 && ended[i] > began[i]+testD-50*time.Millisecond {
			t.Errorf("call %d began at %v and ended at %v: not before its own deadline, so the stall rule did not close the connection",
				i+1, began[i], ended[i])
		}
	}
	waitFor(t, "the stalled call connection redialed", 2*time.Second, func() bool {
		return e.srv.Accepted() > accepted && e.a.ReadyConns(Call) == 1
	})
	if r := e.a.Call(t.Context(), acquireReq(sc, 9)); r.Outcome != OK || r.Reply.Elems[4].Int != 9 {
		t.Errorf("the next call = %v, want ok", r.Outcome)
	}
}

// TestStallAfterReply: when a reply pops the FIFO head, the stall rule moves to the new
// head: the connection closes once that command has waited D since its own write, not
// since an older command's write and not since the reply (plan S3 step 4; DESIGN §A5.4).
// On the one call connection, call 1 is answered 200 ms after its write; call 2, written
// 100 ms after call 1, is never answered.
func TestStallAfterReply(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	defer close(release)
	answered := make(chan time.Time, 1) // when the store sends call 1's reply
	sc := e.script("acquire", func(keys, argv []string) resp.Value {
		switch argv[0] {
		case "1":
			select {
			case <-time.After(200 * time.Millisecond):
			case <-t.Context().Done():
			}
			answered <- time.Now() // the reply is written after this
		case "2":
			select { // never answered while the test runs
			case <-release:
			case <-t.Context().Done():
			}
		}
		return echoAcquire(keys, argv)
	})
	var stalled atomic.Pointer[tapConn] // the call connection: the one EVALSHA is written on
	var taps tapList
	e.start(withConns(1, 1), withTap(&taps, func(c *tapConn, p []byte) (bool, int, error) {
		if bytes.Contains(p, []byte("EVALSHA")) {
			stalled.CompareAndSwap(nil, c)
		}
		return false, 0, nil
	}))
	e.warm(1, 1)
	accepted := e.srv.Accepted()

	start := time.Now() // before call 1's write
	var wg sync.WaitGroup
	var r1, r2 Response
	wg.Add(1)
	go func() {
		defer wg.Done()
		r1 = e.a.Call(t.Context(), acquireReq(sc, 1))
	}()
	waitFor(t, "call 1 written", time.Second, func() bool { return len(e.obs.bytesOf("acquire")) == 1 })
	time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
	before2 := time.Now() // before call 2's write
	wg.Add(1)
	go func() {
		defer wg.Done()
		r2 = e.a.Call(t.Context(), acquireReq(sc, 2))
	}()
	waitFor(t, "the connection with the unanswered head to close", time.Second, func() bool {
		c := stalled.Load()
		return c != nil && c.closed.Load()
	})
	closed := time.Now()
	wg.Wait()
	var replied time.Time
	select {
	case replied = <-answered:
	default:
		t.Fatal("call 1 was never answered")
	}
	if closed.Before(before2.Add(testD)) {
		t.Errorf("the connection closed %v after call 2's write, before D: the stall was measured from an older write",
			closed.Sub(before2))
	}
	if !closed.Before(replied.Add(testD)) {
		t.Errorf("the connection closed %v after call 1's reply, not before D: the stall was measured from the reply",
			closed.Sub(replied))
	}
	if r1.Outcome != OK || r1.Reply.Elems[4].Int != 1 {
		t.Errorf("call 1 = %v, want ok with its own reply", r1.Outcome)
	}
	if r2.Outcome != OutcomeUnknown || !r2.Sent {
		t.Errorf("call 2 = %v, Sent %v; want outcome_unknown, true", r2.Outcome, r2.Sent)
	}
	waitFor(t, "the call connection redialed", 2*time.Second, func() bool {
		return e.srv.Accepted() > accepted && e.a.ReadyConns(Call) == 1
	})
	if r := e.a.Call(t.Context(), acquireReq(sc, 9)); r.Outcome != OK || r.Reply.Elems[4].Int != 9 {
		t.Errorf("the next call = %v, want ok", r.Outcome)
	}
}

func TestPartialWrite(t *testing.T) {
	cases := []struct {
		name         string
		written      int  // bytes the intercepted EVALSHA write sends
		cancelCtx    bool // the host's ctx ends during the write
		deadlineFail bool // instead, setting the write deadline fails, so nothing is written
		want         Outcome
		sentCmds     int
		closed       bool
	}{
		{name: "0 bytes", want: Unavailable},
		{name: "0 bytes with ctx done", cancelCtx: true, want: Cancelled},
		{name: "0 bytes: the write deadline cannot be set", deadlineFail: true, want: Unavailable},
		{name: "5 bytes", written: 5, want: OutcomeUnknown, sentCmds: 1, closed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sc := e.script("acquire", echoAcquire)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var hit atomic.Pointer[tapConn]
			var taps tapList
			e.start(withConns(1, 1), withTap(&taps, func(c *tapConn, p []byte) (bool, int, error) {
				if tc.deadlineFail || !bytes.Contains(p, []byte("EVALSHA")) || !hit.CompareAndSwap(nil, c) {
					return false, 0, nil
				}
				n := 0
				if tc.written > 0 {
					n, _ = c.Conn.Write(p[:tc.written])
				}
				if tc.cancelCtx {
					cancel()
				}
				return true, n, injectedError{}
			}))
			e.warm(1, 1)
			accepted := e.srv.Accepted()
			if tc.deadlineFail {
				for _, c := range taps.all() { // only the call connection writes
					c.failDeadline.Store(true)
				}
			}
			r := e.a.Call(ctx, acquireReq(sc, 1))
			if r.Outcome != tc.want || r.SentCommands != tc.sentCmds || r.Sent != (tc.sentCmds > 0) {
				t.Errorf("Call = %v, SentCommands %d, Sent %v; want %v, %d, %v",
					r.Outcome, r.SentCommands, r.Sent, tc.want, tc.sentCmds, tc.sentCmds > 0)
			}
			conn := hit.Load()
			for _, c := range taps.all() {
				if c.deadlineFailed.Load() {
					conn = c
				}
			}
			if conn == nil {
				t.Fatal("no write to intercept")
			}
			if conn.closed.Load() != tc.closed {
				t.Errorf("connection closed = %v, want %v", conn.closed.Load(), tc.closed)
			}
			if tc.closed {
				// The partly written command is never sent again (plan S3 Watch out; AGENTS.md
				// §6): once the slot has redialed, and a little later, the observer still has
				// only its 5 bytes and the store has received no whole EVALSHA.
				waitFor(t, "the call connection redialed", 2*time.Second, func() bool {
					return e.srv.Accepted() > accepted && e.a.ReadyConns(Call) == 1
				})
				time.Sleep(50 * time.Millisecond)
				if got := e.obs.bytesOf("acquire"); !slices.Equal(got, []int{5}) {
					t.Errorf("observer bytes for acquire = %v, want [5]: the partly written command was sent again", got)
				}
				if n := e.srv.Count("EVALSHA"); n != 0 {
					t.Errorf("the store received %d whole EVALSHA, want 0", n)
				}
				return
			}
			// Nothing of the command left, so nothing of it stays on the connection: no stall
			// deadline closes it later, and the next call gets its own reply.
			time.Sleep(testD + 100*time.Millisecond)
			if conn.closed.Load() {
				t.Error("the connection closed after a write that sent nothing")
			}
			if r := e.a.Call(t.Context(), acquireReq(sc, 2)); r.Outcome != OK || r.Reply.Elems[4].Int != 2 {
				t.Errorf("the next call = %v, want ok with its own reply", r.Outcome)
			}
		})
	}
}

// TestCancelDuringBlockedWrite: when the host cancels while the call's write is blocked
// (the store stopped reading, as with full socket buffers), the write is cut short at once,
// not at its deadline (start + D), and the bytes written decide the outcome (SPEC API-1,
// API-22; DESIGN §A5.4): none → cancelled, and the connection stays usable; some →
// outcome_unknown, the connection closed and the command never sent again; a batch splits
// by its byte ranges (plan S3 step 4). The store end is a net.Pipe, which buffers nothing,
// so the bytes that left are exactly the ones the test read.
func TestCancelDuringBlockedWrite(t *testing.T) {
	cases := []struct {
		name     string
		reqs     int                  // 1 goes through Call, more through CallBatch
		read     func(len1 int) int   // bytes the store reads before the host cancels
		want     []Outcome            // per request
		sentCmds []int                // per request
		obs      func(len1 int) []int // observer bytes per command
		closed   bool
	}{
		{name: "nothing written", reqs: 1, read: func(int) int { return 0 },
			want: []Outcome{Cancelled}, sentCmds: []int{0}, obs: func(int) []int { return []int{0} }},
		{name: "5 bytes written", reqs: 1, read: func(int) int { return 5 },
			want: []Outcome{OutcomeUnknown}, sentCmds: []int{1}, obs: func(int) []int { return []int{5} }, closed: true},
		{name: "batch: the first command and 3 bytes written", reqs: 3, read: func(len1 int) int { return len1 + 3 },
			want: []Outcome{OutcomeUnknown, OutcomeUnknown, Cancelled}, sentCmds: []int{1, 1, 0},
			obs: func(len1 int) []int { return []int{len1, 3, 0} }, closed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sc := e.script("acquire", echoAcquire)
			p := newPipeStore(t, e.srv.Addr())
			e.start(withConns(1, 1), withPipes(p))
			e.warm(1, 1)
			end := p.callEnd(t, e.a)
			if err := end.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("setting the store end's read deadline: %v", err)
			}
			len1 := cmdLen("EVALSHA", sc.SHA, "0", "1") // each request's command: g has one digit
			reqs := make([]Request, tc.reqs)
			for i := range reqs {
				reqs[i] = acquireReq(sc, i+1)
			}

			host, cancel := context.WithCancel(t.Context()) // no deadline: the write's is start + D
			defer cancel()
			done := make(chan []Response, 1)
			go func() {
				if len(reqs) == 1 {
					done <- []Response{e.a.Call(host, reqs[0])}
					return
				}
				done <- e.a.CallBatch(host, reqs)
			}()
			if n := tc.read(len1); n > 0 {
				// The write has sent n bytes and blocks on the rest.
				if _, err := io.ReadFull(end, make([]byte, n)); err != nil {
					t.Fatalf("reading the first %d bytes at the store: %v", n, err)
				}
			} else {
				waitFor(t, "the write to begin", time.Second, func() bool { return end.client.writes.Load() == 2 }) // AUTH, then the call
				time.Sleep(20 * time.Millisecond)                                                                   // it blocks in the pipe
			}
			cancelled := time.Now()
			cancel()
			var rs []Response
			select {
			case rs = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the call still running 2 s after the host cancelled")
			}
			if el := time.Since(cancelled); el >= 100*time.Millisecond {
				t.Errorf("the call ended %v after the host cancelled, want at once (well under D = 300 ms)", el)
			}
			var outcomes []Outcome
			var sentCmds []int
			for _, r := range rs {
				outcomes = append(outcomes, r.Outcome)
				sentCmds = append(sentCmds, r.SentCommands)
				if r.Sent != (r.SentCommands > 0) {
					t.Errorf("Sent %v with SentCommands %d", r.Sent, r.SentCommands)
				}
			}
			if !slices.Equal(outcomes, tc.want) || !slices.Equal(sentCmds, tc.sentCmds) {
				t.Errorf("outcomes %v with SentCommands %v, want %v with %v", outcomes, sentCmds, tc.want, tc.sentCmds)
			}
			want := tc.obs(len1)
			if got := e.obs.bytesOf("acquire"); !slices.Equal(got, want) {
				t.Errorf("observer bytes = %v, want %v", got, want)
			}

			if !tc.closed {
				// Nothing left, so the connection stays: the next call on it gets its reply.
				next := make(chan Response, 1)
				go func() { next <- e.a.Call(t.Context(), acquireReq(sc, 2)) }()
				v, err := resp.NewReader(end).ReadValue()
				if err != nil {
					t.Fatalf("reading the next command at the store: %v", err)
				}
				if args := v.Elems; len(args) != 4 || args[0].Str != "EVALSHA" || args[3].Str != "2" {
					t.Fatalf("the store received %s, want the next call's EVALSHA", v)
				}
				if _, err := end.Write(resp.AppendValue(nil, allowed(2))); err != nil {
					t.Fatalf("answering the next call: %v", err)
				}
				if r := <-next; r.Outcome != OK || r.Reply.Elems[4].Int != 2 {
					t.Errorf("the next call = %v, want ok with its own reply on the same connection", r.Outcome)
				}
				return
			}
			// The connection closed after the bytes counted above: the store end reads EOF.
			if rest, err := io.ReadAll(end); err != nil || len(rest) != 0 {
				t.Errorf("after the cancel the store end read %d more bytes, then %v; want none, then EOF", len(rest), err)
			}
			// Nothing is sent again: once the slot has redialed (to the fake store), and a little
			// later, the store has received no EVALSHA and the observer no more bytes.
			waitFor(t, "the call connection redialed", 2*time.Second, func() bool {
				return e.srv.Accepted() == 1 && e.a.ReadyConns(Call) == 1
			})
			time.Sleep(50 * time.Millisecond)
			if n := e.srv.Count("EVALSHA"); n != 0 {
				t.Errorf("the store received %d EVALSHA after the reconnect, want 0", n)
			}
			if got := e.obs.bytesOf("acquire"); !slices.Equal(got, want) {
				t.Errorf("observer bytes after the reconnect = %v, want %v", got, want)
			}
		})
	}
}

// lateInterruptConn is a store connection on which the interrupt of a write whose ctx ended
// (interruptibleWrite moving the write deadline to now) lands late, as if the goroutine
// moving it had been descheduled: the first write deadline at or before now is held until
// release closes, or stop does when the test ends. requested closes when that deadline
// arrives, applied once it has taken effect. Every other deadline applies at once.
type lateInterruptConn struct {
	net.Conn
	release, stop      <-chan struct{}
	requested, applied chan struct{}
	held               atomic.Bool
}

func (c *lateInterruptConn) SetWriteDeadline(t time.Time) error {
	if t.IsZero() || t.After(time.Now()) || !c.held.CompareAndSwap(false, true) {
		return c.Conn.SetWriteDeadline(t)
	}
	close(c.requested)
	select {
	case <-c.release:
	case <-c.stop:
	}
	defer close(c.applied)
	return c.Conn.SetWriteDeadline(t)
}

// TestLateInterruptSparesNextWrite: the interrupt of a cancelled call's write never reaches
// the next write on the same connection, even when it lands after the cancelled write has
// finished on its own (interruptibleWrite; DESIGN §A5.4). On the one call connection, A's
// write blocks, the host cancels A, and A's interrupt is held; the store reads the rest of
// A, so A's Write finishes with every byte sent; B, with a live ctx, is called at once; the
// interrupt lands; the store reads B and answers A, then B. A ended after sending its
// command, so it is outcome_unknown and its reply is drained; B gets its own reply (SPEC
// API-1). Had A released the write lock before its interrupt took effect, B's write would
// be blocked in the pipe when the interrupt landed, and cut at 0 bytes: unavailable.
func TestLateInterruptSparesNextWrite(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	p := newPipeStore(t, e.srv.Addr())
	release, stop := make(chan struct{}), t.Context().Done()
	e.start(withConns(1, 1), func(o *Options) {
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := p.dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &lateInterruptConn{Conn: c, release: release, stop: stop,
				requested: make(chan struct{}), applied: make(chan struct{})}, nil
		}
	})
	e.warm(1, 1)
	// The call connection, its wrapper, and the store end of its pipe.
	set := e.a.sets[Call]
	set.mu.Lock()
	if len(set.ready) != 1 {
		set.mu.Unlock()
		t.Fatalf("%d ready call connections, want 1", len(set.ready))
	}
	pc := set.ready[0]
	set.mu.Unlock()
	lc, ok := pc.conn.(*lateInterruptConn)
	if !ok {
		t.Fatal("the call connection is not a lateInterruptConn")
	}
	p.mu.Lock()
	end, ok := p.ends[lc.Conn]
	p.mu.Unlock()
	if !ok {
		t.Fatal("the call connection is not a pipe")
	}
	if err := end.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting the store end's read deadline: %v", err)
	}
	lenA := cmdLen("EVALSHA", sc.SHA, "0", "1") // B's command, with g 2, has the same length

	// A's write sends 5 bytes and blocks on the rest. The host cancels A; the interrupt is held.
	hostA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	doneA := make(chan Response, 1)
	go func() { doneA <- e.a.Call(hostA, acquireReq(sc, 1)) }()
	if _, err := io.ReadFull(end, make([]byte, 5)); err != nil {
		t.Fatalf("reading the first 5 bytes of A at the store: %v", err)
	}
	cancelA()
	select {
	case <-lc.requested:
	case <-time.After(2 * time.Second):
		t.Fatal("no interrupt of A's write 2 s after the host cancelled A")
	}
	// The store reads the rest of A, so A's Write finishes on its own, every byte sent.
	if _, err := io.ReadFull(end, make([]byte, lenA-5)); err != nil {
		t.Fatalf("reading the rest of A at the store: %v", err)
	}
	// B goes on the same connection at once, and has 100 ms to begin its write: it can only
	// if A has released the write lock, and then it blocks in the pipe, the store not reading.
	doneB := make(chan Response, 1)
	go func() { doneB <- e.a.Call(t.Context(), acquireReq(sc, 2)) }()
	// A is in the FIFO, and B has reserved the connection.
	waitFor(t, "B to pick the call connection", 2*time.Second, func() bool { return pc.load.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	close(release) // the interrupt lands
	select {
	case <-lc.applied:
	case <-time.After(2 * time.Second):
		t.Fatal("the held interrupt had not taken effect 2 s after its release")
	}
	// The store reads B, then answers A, then B.
	if err := end.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("setting the store end's read deadline: %v", err)
	}
	v, err := resp.NewReader(end).ReadValue()
	switch {
	case err != nil:
		t.Errorf("the store received no command after A: %v", err)
	case len(v.Elems) != 4 || v.Elems[0].Str != "EVALSHA" || v.Elems[3].Str != "2":
		t.Errorf("the store received %s after A, want B's EVALSHA", v)
	default:
		if _, err := end.Write(resp.AppendValue(resp.AppendValue(nil, allowed(1)), allowed(2))); err != nil {
			t.Errorf("answering A, then B: %v", err)
		}
	}
	recv := func(name string, done <-chan Response) Response {
		t.Helper()
		select {
		case r := <-done:
			return r
		case <-time.After(2 * time.Second):
			t.Fatalf("call %s still running 2 s later", name)
			return Response{}
		}
	}
	a, b := recv("A", doneA), recv("B", doneB)
	if a.Outcome != OutcomeUnknown || !a.Sent || a.SentCommands != 1 {
		t.Errorf("A = %v, Sent %v, SentCommands %d; want outcome_unknown, true, 1", a.Outcome, a.Sent, a.SentCommands)
	}
	if b.Outcome != OK || b.Reply.Elems[4].Int != 2 || b.SentCommands != 1 {
		t.Errorf("B = %v, Sent %v, SentCommands %d; want ok with its own reply (g 2), true, 1",
			b.Outcome, b.Sent, b.SentCommands)
	}
	if got, want := e.obs.bytesOf("acquire"), []int{lenA, lenA}; !slices.Equal(got, want) {
		t.Errorf("observer bytes = %v, want %v: A and B each sent whole", got, want)
	}
}

// TestEndedCallTakesNoWaitingReply: a call whose ctx has ended by the time it takes its
// result ends with errDeadline, so outcome_unknown, even with its reply already waiting:
// that reply counts as one that arrived after the call returned (SPEC API-1; plan choice in
// await). The call is abandoned. A call whose ctx is live takes its reply.
func TestEndedCallTakesNoWaitingReply(t *testing.T) {
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	c := &call{res: make(chan result, 1)}
	c.res <- result{v: allowed(1)}
	if v, err := await(ended, c); !errors.Is(err, errDeadline) || v.Kind != 0 {
		t.Errorf("await with an ended ctx = %s, %v; want no reply, %v", v, err, errDeadline)
	}
	if !c.abandoned.Load() {
		t.Error("the call that ended is not abandoned")
	}
	live := &call{res: make(chan result, 1)}
	live.res <- result{v: allowed(2)}
	if v, err := await(t.Context(), live); err != nil || v.Kind != resp.Array || v.Elems[4].Int != 2 {
		t.Errorf("await with a live ctx = %s, %v; want its reply (g 2)", v, err)
	}
}

func TestBatchByteRanges(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	reqs := []Request{acquireReq(sc, 1), acquireReq(sc, 2), acquireReq(sc, 3)}
	len1 := cmdLen("EVALSHA", sc.SHA, "0", "1")
	var once atomic.Bool
	var taps tapList
	e.start(withConns(1, 1), withTap(&taps, func(tc *tapConn, p []byte) (bool, int, error) {
		if !bytes.Contains(p, []byte("EVALSHA")) || !once.CompareAndSwap(false, true) {
			return false, 0, nil
		}
		n, _ := tc.Conn.Write(p[:len1+3])
		return true, n, injectedError{}
	}))
	e.warm(1, 1)
	// The store reads but never answers, so the fully written first command cannot get its
	// reply before the failed write closes the connection: its outcome stays unknown.
	e.srv.Stall()
	rs := e.a.CallBatch(t.Context(), reqs)
	var sent []bool
	var sentCmds []int
	var outcomes []Outcome
	for _, r := range rs {
		sent = append(sent, r.Sent)
		sentCmds = append(sentCmds, r.SentCommands)
		outcomes = append(outcomes, r.Outcome)
	}
	if want := []bool{true, true, false}; !slices.Equal(sent, want) {
		t.Errorf("sent = %v, want %v", sent, want)
	}
	if want := []int{1, 1, 0}; !slices.Equal(sentCmds, want) {
		t.Errorf("SentCommands = %v, want %v", sentCmds, want)
	}
	if got, want := e.obs.bytesOf("acquire"), []int{len1, 3, 0}; !slices.Equal(got, want) {
		t.Errorf("observer bytes = %v, want %v", got, want)
	}
	if want := []Outcome{OutcomeUnknown, OutcomeUnknown, Unavailable}; !slices.Equal(outcomes, want) {
		t.Errorf("outcomes = %v, want %v", outcomes, want)
	}
}

func TestEOFFailsAllInFlight(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	sc := e.script("acquire", blockUntil(t.Context(), release))
	e.start(withConns(1, 1))
	e.warm(1, 1)
	accepted := e.srv.Accepted()

	var wg sync.WaitGroup
	results := make([]Response, 5)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.a.Call(t.Context(), acquireReq(sc, i+1))
		}()
	}
	waitFor(t, "5 commands written", 2*time.Second, func() bool { return len(e.obs.bytesOf("acquire")) == 5 })
	// A Do behind them, which the store answers only after them, is in flight too.
	var pingV resp.Value
	var ping Response
	pinged := goDone(func() { pingV, ping = e.a.Do(t.Context(), Call, "PING") })
	waitFor(t, "the PING written", 2*time.Second, func() bool { return len(e.obs.bytesOf("PING")) == 1 })
	dropped := time.Now()
	e.srv.DropConns()
	wg.Wait()
	if !waitDone(t, "Do(PING)", 2*time.Second, pinged) {
		t.FailNow()
	}
	el := time.Since(dropped)
	close(release)
	for i, r := range results {
		if r.Outcome != OutcomeUnknown || !r.Sent || r.Raw != "" {
			t.Errorf("call %d = %v, Sent %v, Raw %q; want outcome_unknown, true, no reply", i+1, r.Outcome, r.Sent, r.Raw)
		}
	}
	if ping.Outcome != OutcomeUnknown || !ping.Sent || pingV.Kind != 0 {
		t.Errorf("Do(PING) = %s, %v, Sent %v; want no reply, outcome_unknown, true", pingV, ping.Outcome, ping.Sent)
	}
	if el >= 200*time.Millisecond {
		t.Errorf("the calls and the Do ended %v after the drop, want at once (before their own deadline)", el)
	}
	waitFor(t, "a reconnect", 2*time.Second, func() bool {
		return e.srv.Accepted() > accepted && e.a.ReadyConns(Call) == 1
	})
	if r := e.a.Call(t.Context(), acquireReq(sc, 6)); r.Outcome != OK {
		t.Errorf("a call after the reconnect = %v, want ok", r.Outcome)
	}
}

func TestPipelineDepth(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	e.srv.HoldReplies(64)
	e.start(withConns(1, 1), withDeadline(5*time.Second))
	e.warm(1, 1)
	var wg sync.WaitGroup
	results := make([]Response, 64)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.a.Call(t.Context(), acquireReq(sc, i+1))
		}()
	}
	wg.Wait()
	ok := 0
	for i, r := range results {
		if r.Outcome == OK && r.Reply.Elems[4].Int == int64(i+1) {
			ok++
		} else {
			t.Errorf("call %d = %v (%s), want ok with its own reply", i+1, r.Outcome, r.StoreError)
		}
	}
	if ok != 64 {
		t.Errorf("%d of 64 calls succeeded: the 64 were not all written before the first reply", ok)
	}
	if n := e.srv.Count("EVALSHA"); n != 64 {
		t.Errorf("the server received %d EVALSHA, want 64", n)
	}
	if n := e.a.ReadyConns(Call); n != 1 {
		t.Errorf("ready call connections = %d, want 1", n)
	}
}

func TestWaitingWriterRepicks(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	release := make(chan struct{})
	var first atomic.Bool
	var taps tapList
	e.start(withConns(1, 1), withTap(&taps, func(c *tapConn, p []byte) (bool, int, error) {
		if !bytes.Contains(p, []byte("EVALSHA")) || !first.CompareAndSwap(false, true) {
			return false, 0, nil
		}
		n, _ := c.Conn.Write(p[:5]) // call 1 sends 5 bytes, then its write hangs
		select {
		case <-release:
		case <-t.Context().Done():
		}
		return true, n, injectedError{}
	}))
	e.warm(1, 1)
	var wg sync.WaitGroup
	var r1, r2 Response
	wg.Add(1)
	go func() {
		defer wg.Done()
		r1 = e.a.Call(t.Context(), acquireReq(sc, 1))
	}()
	waitFor(t, "call 1's write to start", time.Second, first.Load)
	wg.Add(1)
	go func() {
		defer wg.Done()
		r2 = e.a.Call(t.Context(), acquireReq(sc, 2))
	}()
	waitFor(t, "call 2 to wait for the write lock", time.Second, func() bool {
		set := e.a.sets[Call]
		set.mu.Lock()
		defer set.mu.Unlock()
		return len(set.ready) == 1 && set.ready[0].load.Load() == 2
	})
	close(release) // call 1's write fails after 5 bytes, which closes the connection
	// A call stuck for good fails the test here, not by the test binary's timeout.
	if !within(t, "calls 1 and 2", 2*time.Second, wg.Wait) {
		t.FailNow()
	}
	if r1.Outcome != OutcomeUnknown || r1.SentCommands != 1 {
		t.Errorf("call 1 = %v, SentCommands %d; want outcome_unknown, 1", r1.Outcome, r1.SentCommands)
	}
	if r2.Outcome != OK || r2.Reply.Elems[4].Int != 2 || r2.SentCommands != 1 {
		t.Errorf("call 2 = %v, SentCommands %d; want ok with its own reply on the redialed connection, 1", r2.Outcome, r2.SentCommands)
	}
}

// TestClosedBeforeWriteRepicks (DESIGN §A5.4: commands not yet written move to another ready
// connection within their own deadline; plan S3 step 5, S9 step 1): a call whose picked
// connection closes after the pick, before the call's write starts, writes nothing there and
// moves to the other ready connection, where it is sent once and succeeds. This is the check
// in write that finds the connection already closed while the call holds the write lock, not
// lockWrite's errConnLost. The order is fixed, with no new seam: of 2 call connections, the
// test holds X's write lock (X is the first ready one, so a call picks it at equal load); once
// the call has picked X, the test closes X while holding the set's lock, so the close marks X
// closed and then waits in onClose, before it ends X's done; releasing X's write lock then
// hands it to the call, the only case ready in lockWrite, and the call's write finds X closed.
func TestClosedBeforeWriteRepicks(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	e.start(withConns(2, 1))
	e.warm(2, 1)
	set := e.a.sets[Call]
	set.mu.Lock()
	x := set.ready[0]
	set.mu.Unlock()
	x.wlock <- struct{}{} // the test holds X's write lock
	var r Response
	done := goDone(func() { r = e.a.Call(t.Context(), acquireReq(sc, 7)) })
	waitFor(t, "the call to pick X", time.Second, func() bool { return x.load.Load() == 1 })
	set.mu.Lock()
	unlockSet := sync.OnceFunc(set.mu.Unlock)
	t.Cleanup(unlockSet) // runs before the adapter's Close, also after a failure here
	closing := goDone(x.close)
	waitFor(t, "X marked closed", time.Second, func() bool {
		x.mu.Lock()
		defer x.mu.Unlock()
		return x.closed
	})
	select {
	case <-x.done:
		t.Fatal("X's done closed while its close waits for the set's lock")
	default:
	}
	<-x.wlock // the call takes X's write lock, finds X closed and leaves it
	waitFor(t, "the call to leave X", time.Second, func() bool { return x.load.Load() == 0 })
	unlockSet()
	if !waitDone(t, "the call", 2*time.Second, done) || !waitDone(t, "X's close", time.Second, closing) {
		t.FailNow()
	}
	if r.Outcome != OK || r.Reply.Elems[4].Int != 7 || !r.Sent || r.SentCommands != 1 {
		t.Errorf("the call = %v, Sent %v, SentCommands %d; want ok with its own reply, sent once, on the other ready connection",
			r.Outcome, r.Sent, r.SentCommands)
	}
	if n := e.srv.Count("EVALSHA"); n != 1 {
		t.Errorf("the store received %d EVALSHA, want 1", n)
	}
	if got, want := e.obs.bytesOf("acquire"), []int{cmdLen("EVALSHA", sc.SHA, "0", "7")}; !slices.Equal(got, want) {
		t.Errorf("acquire writes of %v bytes observed, want %v: one whole write, none on X", got, want)
	}
}

// TestClosedDuringEmptyWriteRepicks (DESIGN §A5.4: commands not yet written move to another
// ready connection within their own deadline; plan S9 step 1): a call whose picked connection
// closes after the call joined its FIFO and before any byte of its write left moves to the
// other ready connection, where it is sent once and succeeds: not unavailable, and not the
// errConnLost that the close put in its first record. Of 2 call connections, the tap holds the
// first EVALSHA write, on X, with the call in X's FIFO; the test closes X, then lets the write
// go on, which fails on the closed socket with nothing written.
func TestClosedDuringEmptyWriteRepicks(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	held := make(chan *tapConn, 1)
	release := make(chan struct{})
	var first atomic.Bool
	var taps tapList
	e.start(withConns(2, 1), withTap(&taps, func(c *tapConn, p []byte) (bool, int, error) {
		if !bytes.Contains(p, []byte("EVALSHA")) || !first.CompareAndSwap(false, true) {
			return false, 0, nil
		}
		held <- c
		select {
		case <-release:
		case <-t.Context().Done():
		}
		n, err := c.Conn.Write(p) // X is closed by now: nothing leaves
		return true, n, err
	}))
	e.warm(2, 1)
	var r Response
	done := goDone(func() { r = e.a.Call(t.Context(), acquireReq(sc, 7)) })
	var x *pipeConn
	select {
	case tc := <-held:
		set := e.a.sets[Call]
		set.mu.Lock()
		for _, pc := range set.ready {
			if pc.conn == tc {
				x = pc
			}
		}
		set.mu.Unlock()
	case <-time.After(time.Second):
		t.Fatal("no EVALSHA write within 1 s")
	}
	if x == nil {
		t.Fatal("the held write is not on a ready call connection")
	}
	x.mu.Lock()
	queued := len(x.fifo)
	x.mu.Unlock()
	if queued != 1 {
		t.Fatalf("X's FIFO holds %d commands while the write is held, want 1: the call joined it", queued)
	}
	mustWithin(t, "X's close", time.Second, x.close) // takes the call out of X's FIFO
	close(release)
	if !waitDone(t, "the call", 2*time.Second, done) {
		t.FailNow()
	}
	if r.Outcome != OK || r.Reply.Elems[4].Int != 7 || !r.Sent || r.SentCommands != 1 {
		t.Errorf("the call = %v, Sent %v, SentCommands %d; want ok with its own reply, sent once, on the other ready connection",
			r.Outcome, r.Sent, r.SentCommands)
	}
	if n := e.srv.Count("EVALSHA"); n != 1 {
		t.Errorf("the store received %d EVALSHA, want 1", n)
	}
	if got, want := e.obs.bytesOf("acquire"), []int{0, cmdLen("EVALSHA", sc.SHA, "0", "7")}; !slices.Equal(got, want) {
		t.Errorf("acquire writes of %v bytes observed, want %v: nothing on X, then one whole write on the other connection", got, want)
	}
}

// TestFailedDeadlineClearStillCloses: a connection on which setting the read deadline failed
// is broken (DESIGN §A5.4: a connection is closed on an I/O error), so it still closes by the
// read deadline it kept. Here a reply that matches no command sent pops the one call in the
// FIFO while its write is still under way, and the reader's clearing SetReadDeadline fails;
// the write then fails with nothing sent, so it has no call of its own left to take back out
// of the FIFO and leaves the read deadline as it is: the connection closes at that write's
// time + D instead of staying open with no deadline. It drives one pipeConn over a net.Pipe
// whose client end fails one SetReadDeadline and intercepts the write.
func TestFailedDeadlineClearStillCloses(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { _ = s.Close() })
	buf := resp.AppendCommand(nil, "PING")
	ping := &call{label: "PING", start: 0, end: len(buf), res: make(chan result, 1)}
	tc := &tapConn{Conn: c, intercept: func(tc *tapConn, _ []byte) (bool, int, error) {
		tc.failReadDeadline.Store(true) // the reader's clear, after the reply below, fails
		if _, err := s.Write([]byte("+PONG\r\n")); err != nil {
			t.Errorf("the store end's reply: %v", err)
		}
		waitFor(t, "the reader to take the reply", time.Second, func() bool { return len(ping.res) == 1 })
		return true, 0, injectedError{} // nothing of the PING left
	}}
	pc := newPipeConn(tc, testD, nil, func(*pipeConn) {})
	go pc.readLoop()
	t.Cleanup(func() {
		pc.close()
		<-pc.readerDone
	})

	began := time.Now() // before the write sets the read deadline to its own time + D
	n, closed, err := pc.write(t.Context(), []*call{ping}, buf)
	if n != 0 || closed || err == nil || !tc.readDeadlineFailed.Load() {
		t.Fatalf("write = %d, %v, %v, with the reader's SetReadDeadline failed %v; want 0 bytes, open, an error, true",
			n, closed, err, tc.readDeadlineFailed.Load())
	}
	select {
	case <-pc.done:
		if el := time.Since(began); el < testD {
			t.Errorf("the connection closed %v after the write, before its read deadline (D = %v)", el, testD)
		}
	case <-time.After(time.Until(began.Add(testD + time.Second))):
		t.Fatalf("the connection is still open 1 s after its read deadline, the write's time + D: the deadline was cleared")
	}
}
