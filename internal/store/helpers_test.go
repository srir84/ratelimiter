package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/metrics"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
)

// testD is the dependency deadline D of the unit tests (IMPLEMENTATION_PLAN S3 Tests (a)).
const testD = 300 * time.Millisecond

const (
	tNow   = 1_760_000_000_000_000 // a budget time in µs since the Unix epoch (October 2025)
	lifeUs = 60_000_000            // testLimits' LifetimeUs: a 60 s receipt lifetime
	max53  = 1<<53 - 1             // the largest reply integer (§A4.9)
	tBnd   = 1<<53 - 1<<43         // TBOUND (§A3.1): 8,998,403,161,718,784
)

// testLimits are the default policy's values (§A5.10), written out: capacity 20 × rate
// period 1 s = 2 × 10^10 cap_units, a 60 s receipt lifetime and 1,000 markers per key.
func testLimits() Limits {
	return Limits{CapUnits: 20_000_000_000, RatePeriodNs: 1_000_000_000, LifetimeUs: lifeUs, MaxMarkersPerKey: 1000}
}

// rep builds an array reply: an int or int64 becomes an integer, a string a bulk string, and
// a resp.Value stays as it is.
func rep(vals ...any) resp.Value {
	elems := make([]resp.Value, len(vals))
	for i, v := range vals {
		switch v := v.(type) {
		case int:
			elems[i] = resp.Value{Kind: resp.Int, Int: int64(v)}
		case int64:
			elems[i] = resp.Value{Kind: resp.Int, Int: v}
		case string:
			elems[i] = resp.Value{Kind: resp.Bulk, Str: v}
		case resp.Value:
			elems[i] = v
		default:
			elems[i] = resp.Value{}
		}
	}
	return resp.Value{Kind: resp.Array, Elems: elems}
}

func vInt(n int64) resp.Value { return resp.Value{Kind: resp.Int, Int: n} }

// allowed is a valid E_ALLOWED acquire reply {1, t, ”, u, g, s, expiry_us} carrying g.
func allowed(g int64) resp.Value {
	return rep(1, tNow, "", 0, g, 1, tNow+lifeUs)
}

// echoAcquire answers an acquire like an allowed one whose g is the call's first ARGV, so
// every caller can tell its own reply.
func echoAcquire(_, argv []string) resp.Value {
	if len(argv) == 0 {
		return resp.Value{Kind: resp.Error, Str: "ERR test: no ARGV"}
	}
	g, err := strconv.ParseInt(argv[0], 10, 64)
	if err != nil {
		return resp.Value{Kind: resp.Error, Str: "ERR test: ARGV[1] is not a number"}
	}
	return allowed(g)
}

// newPassword returns a fresh store password: 32 random bytes in hex.
func newPassword(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("reading random bytes: %v", err)
	}
	return hex.EncodeToString(b)
}

// cmdLen is the length of args as a RESP array of bulk strings, counted by hand.
func cmdLen(args ...string) int {
	n := len("*") + len(strconv.Itoa(len(args))) + len("\r\n")
	for _, a := range args {
		n += len("$") + len(strconv.Itoa(len(a))) + len("\r\n") + len(a) + len("\r\n")
	}
	return n
}

// obsRec is one send-observer call.
type obsRec struct {
	cmd string
	n   int
}

// observer records send-observer calls.
type observer struct {
	mu   sync.Mutex
	recs []obsRec
}

func (o *observer) record(cmd string, n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.recs = append(o.recs, obsRec{cmd, n})
}

func (o *observer) all() []obsRec {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]obsRec(nil), o.recs...)
}

// bytesOf returns the byte counts reported for cmd, in order.
func (o *observer) bytesOf(cmd string) []int {
	var out []int
	for _, r := range o.all() {
		if r.cmd == cmd {
			out = append(out, r.n)
		}
	}
	return out
}

// env is one test's fake store, and the adapter once start runs.
type env struct {
	t   *testing.T
	pw  string
	srv *fakeresp.Server
	a   *StoreAdapter
	obs *observer
	reg *metrics.Registry
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pw := newPassword(t)
	return &env{t: t, pw: pw, srv: fakeresp.Start(t, "rl_lib", pw), obs: &observer{}, reg: metrics.NewRegistry()}
}

// script makes a script known to the fake store with handler h.
func (e *env) script(name string, h fakeresp.Handler) scripts.Script {
	sc := scripts.New(name, "-- "+name+" for "+e.t.Name())
	e.srv.Load(sc.Text)
	e.srv.Handle(sc.SHA, h)
	return sc
}

// start builds the adapter: 8 call and 2 refund connections, D = 300 ms, the test limits,
// the observer and the registry, then opts. The test's cleanup closes it.
func (e *env) start(opts ...func(*Options)) {
	e.t.Helper()
	o := Options{
		Addr: e.srv.Addr(), User: "rl_lib", Password: e.pw,
		StoreConns: 8, RefundConns: 2, DependencyDeadline: testD,
		Limits: testLimits(), SendObserver: e.obs.record, Metrics: e.reg,
	}
	for _, f := range opts {
		f(&o)
	}
	e.a = New(e.t.Context(), o)
	e.t.Cleanup(e.a.Close)
}

// warm waits until every connection is ready: 8 call, 2 refund and 1 maintenance by
// default, or call and refund as given.
func (e *env) warm(call, refund int) {
	e.t.Helper()
	waitFor(e.t, "ready connections", 5*time.Second, func() bool {
		return e.a.ReadyConns(Call) == call && e.a.ReadyConns(Refund) == refund && e.a.ReadyConns(Maintenance) == 1
	})
}

func withConns(call, refund int) func(*Options) {
	return func(o *Options) { o.StoreConns, o.RefundConns = call, refund }
}

func withDeadline(d time.Duration) func(*Options) {
	return func(o *Options) { o.DependencyDeadline = d }
}

// acquireReq is a call-kind request of sc with ARGV g.
func acquireReq(sc scripts.Script, g int) Request {
	return Request{Kind: Call, Script: sc, Args: []string{strconv.Itoa(g)}}
}

// waitFor polls cond every millisecond and fails the test if it does not hold within d.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", d, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// blockUntil returns a handler that holds every EVALSHA until release closes (or the test's
// context ends), then answers like echoAcquire.
func blockUntil(ctx context.Context, release <-chan struct{}) fakeresp.Handler {
	return func(keys, argv []string) resp.Value {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return echoAcquire(keys, argv)
	}
}

// tapConn is a real store connection whose writes a test can intercept: when intercept
// handles a write, its result is the write's.
type tapConn struct {
	net.Conn
	intercept func(tc *tapConn, p []byte) (handled bool, n int, err error)
	closed    atomic.Bool
	// failDeadline, while set, makes the next SetWriteDeadline fail; deadlineFailed records it.
	failDeadline, deadlineFailed atomic.Bool
}

func (tc *tapConn) SetWriteDeadline(t time.Time) error {
	if tc.failDeadline.CompareAndSwap(true, false) {
		tc.deadlineFailed.Store(true)
		return injectedError{}
	}
	return tc.Conn.SetWriteDeadline(t)
}

func (tc *tapConn) Write(p []byte) (int, error) {
	if tc.intercept != nil {
		if handled, n, err := tc.intercept(tc, p); handled {
			return n, err
		}
	}
	return tc.Conn.Write(p)
}

func (tc *tapConn) Close() error {
	tc.closed.Store(true)
	return tc.Conn.Close()
}

// withTap dials for real and wraps every connection in a tapConn with intercept; taps
// collects them.
func withTap(taps *tapList, intercept func(tc *tapConn, p []byte) (bool, int, error)) func(*Options) {
	return func(o *Options) {
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := &tapConn{Conn: c, intercept: intercept}
			taps.add(tc)
			return tc, nil
		}
	}
}

type tapList struct {
	mu   sync.Mutex
	taps []*tapConn
}

func (l *tapList) add(tc *tapConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.taps = append(l.taps, tc)
}

func (l *tapList) all() []*tapConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*tapConn(nil), l.taps...)
}

// errInjected is the error an intercepted write returns.
type injectedError struct{}

func (injectedError) Error() string { return "injected write failure" }

// pipeStore stands in for the store on an adapter's first three dials: with withConns(1, 1),
// the call, refund and maintenance slots. Each gets the client end of a net.Pipe, which
// buffers nothing: a Write blocks until the store end reads its bytes, so a test decides to
// the byte how much of a command leaves, like a store that stopped reading with full
// socket buffers. The store end answers AUTH with +OK and then reads only what the test
// reads. Later dials (redials) reach addr, the fake store, over TCP.
type pipeStore struct {
	addr string
	wg   sync.WaitGroup // the AUTH answerers

	mu    sync.Mutex
	dials int
	ends  map[net.Conn]*pipeEnd // the store end of each client end
}

// pipeEnd is the store end of one pipe.
type pipeEnd struct {
	net.Conn
	client *pipeClient
}

// pipeClient is the adapter's end of a pipe; writes counts the Write calls begun on it.
type pipeClient struct {
	net.Conn
	writes atomic.Int64
}

func (c *pipeClient) Write(b []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(b)
}

// newPipeStore returns a pipeStore whose redials reach addr; t's cleanup closes the store
// ends and waits for the AUTH answerers.
func newPipeStore(t *testing.T, addr string) *pipeStore {
	p := &pipeStore{addr: addr, ends: map[net.Conn]*pipeEnd{}}
	t.Cleanup(func() {
		p.mu.Lock()
		for _, end := range p.ends {
			_ = end.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

// withPipes makes the adapter dial through p.
func withPipes(p *pipeStore) func(*Options) {
	return func(o *Options) { o.Dial = p.dial }
}

func (p *pipeStore) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	p.mu.Lock()
	p.dials++
	first := p.dials <= 3
	p.mu.Unlock()
	if !first {
		return (&net.Dialer{}).DialContext(ctx, network, p.addr)
	}
	c, s := net.Pipe()
	client := &pipeClient{Conn: c}
	p.mu.Lock()
	p.ends[client] = &pipeEnd{Conn: s, client: client}
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		// The handshake writes AUTH in one Write, and a pipe Read returns no more than one
		// Write's bytes, so the reader's buffer holds AUTH alone.
		if _, err := resp.NewReader(s).ReadValue(); err == nil {
			_, _ = s.Write([]byte("+OK\r\n"))
		}
	}()
	return client, nil
}

// callEnd returns the store end of a's one ready call connection.
func (p *pipeStore) callEnd(t *testing.T, a *StoreAdapter) *pipeEnd {
	t.Helper()
	set := a.sets[Call]
	set.mu.Lock()
	if len(set.ready) != 1 {
		set.mu.Unlock()
		t.Fatalf("%d ready call connections, want 1", len(set.ready))
	}
	conn := set.ready[0].conn
	set.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	end, ok := p.ends[conn]
	if !ok {
		t.Fatal("the call connection is not a pipe")
	}
	return end
}
