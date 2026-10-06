package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
)

func TestReconnectBackoff(t *testing.T) {
	ms := time.Millisecond
	want := [][2]time.Duration{
		{25 * ms, 50 * ms}, {50 * ms, 100 * ms}, {100 * ms, 200 * ms}, {200 * ms, 400 * ms},
		{400 * ms, 800 * ms}, {500 * ms, 1000 * ms}, {500 * ms, 1000 * ms},
	}
	for n, w := range want {
		if got := reconnectBackoff(n, 0); got != w[0] {
			t.Errorf("reconnectBackoff(%d, 0) = %v, want %v", n, got, w[0])
		}
		if got := reconnectBackoff(n, 1); got != w[1] {
			t.Errorf("reconnectBackoff(%d, 1) = %v, want %v", n, got, w[1])
		}
		for _, r := range []float64{0.001, 0.25, 0.5, 0.75, 0.999} {
			if got := reconnectBackoff(n, r); got < w[0] || got > w[1] {
				t.Errorf("reconnectBackoff(%d, %v) = %v, outside [%v, %v]", n, r, got, w[0], w[1])
			}
		}
	}
	for _, n := range []int{7, 32, 1 << 20} {
		if got := reconnectBackoff(n, 0.5); got != 750*ms {
			t.Errorf("reconnectBackoff(%d, 0.5) = %v, want 750ms", n, got)
		}
	}
}

func TestFewestInFlight(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	sc := e.script("acquire", blockUntil(t.Context(), release)) // no reply until release
	var mu sync.Mutex
	writes := map[*tapConn]int{} // EVALSHA writes per connection, seen from the client side
	var taps tapList
	e.start(withTap(&taps, func(c *tapConn, p []byte) (bool, int, error) {
		if bytes.Contains(p, []byte("EVALSHA")) {
			mu.Lock()
			writes[c]++
			mu.Unlock()
		}
		return false, 0, nil
	}))
	e.warm(8, 2)

	var wg sync.WaitGroup
	results := make([]Response, 16)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.a.Call(t.Context(), acquireReq(sc, i+1))
		}()
	}
	// With every reply held back, a connection's writes are its commands in flight.
	inFlight := func() (counts []int, total int) {
		mu.Lock()
		defer mu.Unlock()
		for _, n := range writes {
			counts = append(counts, n)
			total += n
		}
		return counts, total
	}
	waitFor(t, "16 commands in flight", 2*time.Second, func() bool { _, total := inFlight(); return total == 16 })
	counts, _ := inFlight()
	close(release)
	wg.Wait()
	if len(counts) != 8 {
		t.Errorf("commands went to %d connections %v, want all 8 call connections", len(counts), counts)
	} else if hi, lo := maxOf(counts), minOf(counts); hi-lo > 1 {
		t.Errorf("in flight per connection = %v: max − min = %d, want ≤ 1", counts, hi-lo)
	}
	for i, r := range results {
		if r.Outcome != OK || r.Reply.Elems[4].Int != int64(i+1) {
			t.Errorf("call %d = %v, want ok with its own reply", i+1, r.Outcome)
		}
	}
}

// TestFewestInFlightCountsReplies: a connection's count of commands in flight drops when a
// reply comes back (DESIGN §A5.4: a command is written on the ready connection with the
// fewest in flight). With call 1 unanswered on one of 2 call connections, 8 sequential calls
// all go to the other one and are answered at once.
func TestFewestInFlightCountsReplies(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	sc := e.script("acquire", func(keys, argv []string) resp.Value {
		if argv[0] == "1" { // call 1 is not answered until release
			select {
			case <-release:
			case <-t.Context().Done():
			}
		}
		return echoAcquire(keys, argv)
	})
	var mu sync.Mutex
	writes := map[*tapConn]int{} // EVALSHA writes per connection
	var taps tapList
	e.start(withConns(2, 1), withTap(&taps, func(c *tapConn, p []byte) (bool, int, error) {
		if bytes.Contains(p, []byte("EVALSHA")) {
			mu.Lock()
			writes[c]++
			mu.Unlock()
		}
		return false, 0, nil
	}))
	e.warm(2, 1)
	first := make(chan Response, 1)
	go func() { first <- e.a.Call(t.Context(), acquireReq(sc, 1)) }()
	waitFor(t, "call 1 written", time.Second, func() bool { return len(e.obs.bytesOf("acquire")) == 1 })
	for g := 2; g <= 9; g++ {
		began := time.Now()
		r := e.a.Call(t.Context(), acquireReq(sc, g))
		if el := time.Since(began); r.Outcome != OK || r.Reply.Elems[4].Int != int64(g) || el >= 150*time.Millisecond {
			t.Errorf("call %d = %v after %v, want ok with its own reply at once, on the connection without call 1",
				g, r.Outcome, el)
		}
	}
	close(release)
	<-first
	mu.Lock()
	defer mu.Unlock()
	var counts []int
	for _, n := range writes {
		counts = append(counts, n)
	}
	slices.Sort(counts)
	if !slices.Equal(counts, []int{1, 8}) {
		t.Errorf("EVALSHA writes per call connection = %v, want [1 8]: call 1 alone on one, the 8 others on the other", counts)
	}
}

// TestFewestInFlightTieGoesToFirstReady: among ready connections with equally few commands in
// flight, a call takes the one that became ready first (plan choice: pick keeps the ready
// order among ties, so a call never passes over an idle connection for a later one). With 3
// call connections and every reply held back, calls 1, 2 and 3 go to the first, second and
// third ready connection, and call 4, with one in flight on each, to the first again.
func TestFewestInFlightTieGoesToFirstReady(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	sc := e.script("acquire", blockUntil(t.Context(), release)) // no reply until release
	var mu sync.Mutex
	var order []*tapConn // the connection of each EVALSHA write, in order
	var taps tapList
	e.start(withConns(3, 1), withDeadline(2*time.Second), withTap(&taps, func(c *tapConn, p []byte) (bool, int, error) {
		if bytes.Contains(p, []byte("EVALSHA")) {
			mu.Lock()
			order = append(order, c)
			mu.Unlock()
		}
		return false, 0, nil
	}))
	e.warm(3, 1)
	set := e.a.sets[Call]
	set.mu.Lock()
	ready := make([]net.Conn, len(set.ready)) // in the order they became ready
	for i, pc := range set.ready {
		ready[i] = pc.conn
	}
	set.mu.Unlock()
	written := func() []int { // each write's connection, as its place in ready
		mu.Lock()
		defer mu.Unlock()
		out := make([]int, len(order))
		for i, c := range order {
			out[i] = slices.Index(ready, net.Conn(c))
		}
		return out
	}

	var wg sync.WaitGroup
	results := make([]Response, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.a.Call(t.Context(), acquireReq(sc, i+1))
		}()
		waitFor(t, fmt.Sprintf("call %d written", i+1), time.Second, func() bool { return len(written()) == i+1 })
	}
	got := written()
	close(release)
	wg.Wait()
	if want := []int{0, 1, 2, 0}; !slices.Equal(got, want) {
		t.Errorf("calls 1–4 went to ready connections %v, want %v: the first of the least loaded each time", got, want)
	}
	for i, r := range results {
		if r.Outcome != OK || r.Reply.Elems[4].Int != int64(i+1) {
			t.Errorf("call %d = %v, want ok with its own reply", i+1, r.Outcome)
		}
	}
}

func maxOf(xs []int) int {
	m := xs[0]
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}

func minOf(xs []int) int {
	m := xs[0]
	for _, x := range xs {
		m = min(m, x)
	}
	return m
}

func TestUnavailableWhenRefused(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil { // nothing listens there now: connecting is refused
		t.Fatalf("closing the listener: %v", err)
	}
	obs := &observer{}
	a := New(t.Context(), Options{
		Addr: addr, User: "rl_lib", Password: newPassword(t), StoreConns: 8, RefundConns: 2,
		DependencyDeadline: testD, Limits: testLimits(), SendObserver: obs.record,
	})
	t.Cleanup(a.Close)
	r := a.Call(t.Context(), acquireReq(scripts.New("acquire", "-- refused"), 1))
	if r.Outcome != Unavailable || r.Sent || r.SentCommands != 0 {
		t.Errorf("Call = %v, Sent %v, SentCommands %d; want unavailable, false, 0", r.Outcome, r.Sent, r.SentCommands)
	}
	total := 0
	for _, rec := range obs.all() {
		total += rec.n
	}
	if total != 0 {
		t.Errorf("%d bytes observed, want 0", total)
	}
}

func TestConnectHangs(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		want    Outcome
		lo, hi  time.Duration
	}{
		{"5 s ctx", 5 * time.Second, Unavailable, 300 * time.Millisecond, 450 * time.Millisecond},
		{"100 ms ctx", 100 * time.Millisecond, Cancelled, 100 * time.Millisecond, 300 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.HangAuth()
			sc := e.script("acquire", echoAcquire)
			e.start()
			start := time.Now() // before the ctx's deadline is set, so it is start + timeout
			ctx, cancel := context.WithDeadline(t.Context(), start.Add(tc.timeout))
			defer cancel()
			r := e.a.Call(ctx, acquireReq(sc, 1))
			el := time.Since(start)
			if r.Outcome != tc.want || r.Sent {
				t.Errorf("Call = %v, Sent %v; want %v, false", r.Outcome, r.Sent, tc.want)
			}
			if el < tc.lo || el > tc.hi {
				t.Errorf("Call ended after %v, want %v–%v", el, tc.lo, tc.hi)
			}
			if n := e.srv.Count("EVALSHA"); n != 0 {
				t.Errorf("the server received %d EVALSHA, want 0", n)
			}
		})
	}
}

// gateConn holds its first SetDeadline call, the handshake's, until gate closes (or the
// test ends), after announcing it on entered: by then the dial has registered the
// connection with its set.
type gateConn struct {
	net.Conn
	testCtx context.Context
	entered chan<- struct{}
	gate    <-chan struct{}
	held    atomic.Bool
}

func (g *gateConn) SetDeadline(t time.Time) error {
	if g.held.CompareAndSwap(false, true) {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.gate:
		case <-g.testCtx.Done():
		}
	}
	return g.Conn.SetDeadline(t)
}

// TestCloseDuringHandshake: Close stays bounded when it lands after a dial registered its
// connection but before the handshake armed its deadline, against a store that accepts TCP
// and never answers AUTH. D = 5 s here (plan choice), so a Close that waits out a re-armed
// handshake deadline cannot pass for a prompt one.
func TestCloseDuringHandshake(t *testing.T) {
	e := newEnv(t)
	e.srv.HangAuth()
	entered := make(chan struct{}, 11)
	gate := make(chan struct{})
	e.start(withDeadline(5*time.Second), func(o *Options) {
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &gateConn{Conn: c, testCtx: t.Context(), entered: entered, gate: gate}, nil
		}
	})
	for range 11 { // each slot's connection is registered; its handshake deadline is not armed
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("waited 5 s for 11 handshakes to start")
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.a.Close()
	}()
	waitFor(t, "every connection set shut down", time.Second, func() bool {
		for _, s := range e.a.sets {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				return false
			}
		}
		return true
	})
	close(gate) // the handshakes go on, arming their deadlines only now
	select {
	case <-done:
	case <-time.After(time.Second):
		// Returning ends t.Context(), the adapter's lifetime, so a Close that never returns
		// fails here by assertion instead of hanging the test.
		t.Fatal("Close still waiting 1 s after the handshakes went on: it waits out their deadline (D = 5 s)")
	}
}

func TestFailFastDuringBackoff(t *testing.T) {
	var dials atomic.Int64
	a := New(t.Context(), Options{
		Addr: "127.0.0.1:1", User: "rl_lib", Password: newPassword(t), StoreConns: 8, RefundConns: 2,
		DependencyDeadline: testD, Limits: testLimits(),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("connection refused (test)")
		},
		Rand: func() float64 { return 0.99 }, // the first backoff is 49.75 ms
	})
	t.Cleanup(a.Close)
	deadline := time.Now().Add(2 * time.Second)
	for dials.Load() < 11 { // every slot's first dial has failed
		if time.Now().After(deadline) {
			t.Fatalf("%d dials after 2 s, want 11", dials.Load())
		}
		time.Sleep(100 * time.Microsecond)
	}
	before := dials.Load()
	sc := scripts.New("acquire", "-- fail fast")
	start := time.Now()
	for i := range 100 {
		r := a.Call(t.Context(), acquireReq(sc, i+1))
		if r.Outcome != Unavailable || r.Sent {
			t.Fatalf("call %d = %v, Sent %v; want unavailable, false", i+1, r.Outcome, r.Sent)
		}
	}
	el := time.Since(start)
	after := dials.Load()
	if el >= 20*time.Millisecond {
		t.Errorf("100 calls took %v, want < 20 ms in total", el)
	}
	if before != 11 || after != 11 {
		t.Errorf("dials = %d before and %d after the calls, want 11: the count grew within the backoff", before, after)
	}
	waitFor(t, "a redial once the backoff elapsed", 2*time.Second, func() bool { return dials.Load() > 11 })
}

// TestNoFailFastWhileReady: a kind fails fast only while none of its connections is ready,
// never while a healthy one exists (DESIGN §A5.4; plan S3 Watch out). Of 2 call
// connections, one is dropped on the client side and its slot's redials fail, so the kind's
// last dial failed within its backoff; every call goes to the other, ready, connection.
// Rand 0.99 makes the backoff steps 49.75, 99.5, 199 and 398 ms, so the calls run inside a
// backoff of 199 ms or more.
func TestNoFailFastWhileReady(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	var refuse atomic.Bool
	e.start(withConns(2, 1), func(o *Options) {
		o.Rand = func() float64 { return 0.99 }
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if refuse.Load() {
				return nil, errors.New("connection refused (test)")
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	})
	e.warm(2, 1)
	refuse.Store(true) // from now on every dial fails; only the dropped connection's slot dials
	set := e.a.sets[Call]
	set.mu.Lock()
	dropped := set.ready[0]
	set.mu.Unlock()
	if err := dropped.conn.Close(); err != nil {
		t.Fatalf("closing a call connection: %v", err)
	}
	failFast := func() (failing bool, left time.Duration, ready int) {
		set.mu.Lock()
		defer set.mu.Unlock()
		return set.failed, time.Until(set.failUntil), len(set.ready)
	}
	waitFor(t, "the dropped slot's redials to fail, with 100 ms or more of backoff left", 2*time.Second, func() bool {
		failing, left, ready := failFast()
		return failing && left > 100*time.Millisecond && ready == 1
	})
	const n = 50
	ok := 0
	for i := range n {
		if r := e.a.Call(t.Context(), acquireReq(sc, i+1)); r.Outcome == OK && r.Reply.Elems[4].Int == int64(i+1) {
			ok++
		}
	}
	if ok != n {
		t.Errorf("%d of %d calls ok while a call connection was ready: a call failed fast", ok, n)
	}
	if failing, _, ready := failFast(); !failing || ready != 1 {
		t.Errorf("after the calls: last dial failed %v, ready connections %d; want true, 1", failing, ready)
	}
}

// TestDialTimesOut: a dial whose connect or AUTH never completes fails at D, since each
// dial with its AUTH has a deadline ≤ D (DESIGN §A5.4; plan S3 steps 3 and 5). The failed
// dial puts the kind in fail-fast, so a call returns unavailable at once, and the slots
// redial after their backoff. Rand 0.99 makes the first backoff 49.75 ms.
func TestDialTimesOut(t *testing.T) {
	cases := []struct {
		name string
		hang bool // the store accepts TCP and never answers AUTH; else the connect never completes
	}{
		{"AUTH never answered", true},
		{"connect never completes", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sc := e.script("acquire", echoAcquire)
			if tc.hang {
				e.srv.HangAuth()
			}
			var dials atomic.Int64
			start := time.Now() // before any dial starts, so no dial's deadline comes before start + D
			e.start(withConns(1, 1), func(o *Options) {
				o.Rand = func() float64 { return 0.99 }
				o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
					dials.Add(1)
					if tc.hang {
						return (&net.Dialer{}).DialContext(ctx, network, addr)
					}
					<-ctx.Done() // a connect that never completes ends only with its ctx
					return nil, ctx.Err()
				}
			})
			set := e.a.sets[Call]
			waitFor(t, "the call connection's dial to fail", testD+300*time.Millisecond, func() bool {
				set.mu.Lock()
				defer set.mu.Unlock()
				return set.failed
			})
			if el := time.Since(start); el < testD || el > testD+150*time.Millisecond {
				t.Errorf("the dial failed %v after it started, want 300–450 ms (its deadline, D)", el)
			}
			began := time.Now()
			r := e.a.Call(t.Context(), acquireReq(sc, 1))
			if el := time.Since(began); r.Outcome != Unavailable || r.Sent || el >= 20*time.Millisecond {
				t.Errorf("a call after the failed dial = %v, Sent %v after %v; want unavailable, false at once (fail fast)",
					r.Outcome, r.Sent, el)
			}
			waitFor(t, "each of the 3 slots to dial again after its backoff", time.Second, func() bool {
				return dials.Load() >= 6
			})
		})
	}
}

// TestCallWaitsForRedial: once the backoff after a failed dial has run out, a call joins the
// redial in progress instead of failing fast (DESIGN §A5.4: fail fast only while the last
// dial failed within the current backoff; plan S3 step 5). Every slot's first dial fails;
// the redials, 49.75 ms later (Rand 0.99), take 200 ms each, below the call's D.
func TestCallWaitsForRedial(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	var dials atomic.Int64
	e.start(withConns(1, 1), func(o *Options) {
		o.Rand = func() float64 { return 0.99 }
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dials.Add(1) <= 3 { // the 3 slots' first dials
				return nil, errors.New("connection refused (test)")
			}
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	})
	// A slot redials only once its backoff has run out, so with every redial started, no
	// backoff is running.
	waitFor(t, "every slot's redial to start", time.Second, func() bool { return dials.Load() >= 6 })
	began := time.Now()
	r := e.a.Call(t.Context(), acquireReq(sc, 1))
	if el := time.Since(began); r.Outcome != OK || r.Reply.Elems[4].Int != 1 {
		t.Errorf("a call during the redial = %v after %v, want ok once the redial completes", r.Outcome, el)
	}
}

// TestBackoffResetsAfterSuccess: a dial that succeeds resets the count of failed dials, so
// after a long outage a short second one is retried at the first backoff step, not near the
// 1 s cap (DESIGN §A5.4: min(1 s, 50 ms × 2^n) after n failed dials in a row). Rand 0.99
// makes the steps 49.75, 99.5, 199, 398 and 796 ms.
func TestBackoffResetsAfterSuccess(t *testing.T) {
	e := newEnv(t)
	sc := e.script("acquire", echoAcquire)
	var failing atomic.Int64 // dials still to fail
	failing.Store(12)        // the first outage: each of the 3 slots fails 4 dials in a row
	e.start(withConns(1, 1), func(o *Options) {
		o.Rand = func() float64 { return 0.99 }
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if failing.Add(-1) >= 0 {
				return nil, errors.New("connection refused (test)")
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	})
	e.warm(1, 1) // each slot's fifth dial, ≈ 746 ms after the first, succeeds
	// The second outage: the connections drop and each slot's next dial fails once. With the
	// count reset, the slot redials 49.75 ms after the drop, fails, and dials again 49.75 ms
	// later; without it the second wait would be the fifth step, 796 ms.
	failing.Store(3)
	dropped := time.Now()
	e.srv.DropConns()
	waitFor(t, "the call connection to drop", time.Second, func() bool { return e.a.ReadyConns(Call) == 0 })
	waitFor(t, "the call connection back within the first backoff steps", 400*time.Millisecond, func() bool {
		return e.a.ReadyConns(Call) == 1
	})
	if r := e.a.Call(t.Context(), acquireReq(sc, 1)); r.Outcome != OK {
		t.Errorf("a call %v after the drop = %v, want ok", time.Since(dropped), r.Outcome)
	}
}

func TestReadyGauge(t *testing.T) {
	e := newEnv(t)
	e.start()
	gauge := func() [3]float64 {
		var out [3]float64
		for _, f := range e.reg.Snapshot().Families {
			if f.Name != "rl_store_connections" {
				continue
			}
			for _, s := range f.Series {
				switch s.LabelValue {
				case "call":
					out[0] = s.Value
				case "refund":
					out[1] = s.Value
				case "maintenance":
					out[2] = s.Value
				}
			}
		}
		return out
	}
	waitFor(t, "8/2/1 ready connections and 11 accepted", 5*time.Second, func() bool {
		return gauge() == [3]float64{8, 2, 1} && e.srv.Accepted() == 11
	})
	e.srv.Stop()
	waitFor(t, "0/0/0 after Stop", time.Second, func() bool { return gauge() == [3]float64{} })
	e.srv.Restart()
	waitFor(t, "8/2/1 after Restart", 2*time.Second, func() bool { return gauge() == [3]float64{8, 2, 1} })
}
