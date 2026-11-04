package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/dbclient"
	"github.com/srir84/ratelimiter/internal/metrics"
	"github.com/srir84/ratelimiter/limiter"
)

// waitFor is the bound on every wait for another goroutine in these tests.
const waitFor = 10 * time.Second

// recv waits for ch within waitFor and fails the test otherwise.
func recv(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	timer := time.NewTimer(waitFor)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestHTTPAPI_Routes: unknown paths are a JSON 404 not_found, a wrong method a JSON 405
// method_not_allowed with Allow: GET (ERR-6), and every response, /health and /metrics
// included, carries Cache-Control: no-store (HTTP-8). The requests the mux would answer
// itself outside the ERR table, the request URI "*" (a bare 400) and an unclean path such as
// //hit or /x/../hit (a redirect), get the JSON 404 too (HTTP-7). /health and /metrics call
// neither Acquire nor the database (HAR-16).
func TestHTTPAPI_Routes(t *testing.T) {
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{id: "d1"})}
	ts := newTestServer(t, testConfig(), lim, &fakeDB{hit: committed()})
	ctx := t.Context()

	for _, c := range []struct {
		method, target string
		status         int
		code           string
	}{
		{http.MethodGet, "/nope", http.StatusNotFound, errNotFound},
		{http.MethodGet, "/hit/x", http.StatusNotFound, errNotFound},
		{http.MethodPost, "/", http.StatusNotFound, errNotFound},
		{http.MethodPost, "/hit", http.StatusMethodNotAllowed, errMethodNotAllowed},
		{http.MethodHead, "/hit", http.StatusMethodNotAllowed, errMethodNotAllowed},
		{http.MethodPut, "/health", http.StatusMethodNotAllowed, errMethodNotAllowed},
		{http.MethodDelete, "/metrics", http.StatusMethodNotAllowed, errMethodNotAllowed},
	} {
		rec := ts.do(ctx, c.method, c.target, ids1("u1", "r1"))
		body := checkResponse(t, rec, c.status, c.code)
		if allow := sentHeader(rec).Get("Allow"); c.status == http.StatusMethodNotAllowed && allow != http.MethodGet {
			t.Errorf("%s %s: Allow %q, want GET", c.method, c.target, allow)
		}
		if body["user_id"] != "u1" || body["request_id"] != "r1" {
			t.Errorf("%s %s: body %v, want both valid IDs", c.method, c.target, body)
		}
	}
	// Neither the mux's bare 400 for "*" nor its redirect for an unclean path: the JSON 404.
	for _, c := range []struct{ method, target string }{
		{http.MethodOptions, "*"},
		{http.MethodGet, "*"},
		{http.MethodGet, "//hit"},
		{http.MethodGet, "/x/../hit"},
		{http.MethodGet, "/a/../hit"},
		{http.MethodGet, "/./hit"},
		{http.MethodGet, "/hit/"},
		{http.MethodPost, "//hit"},
		{http.MethodGet, "/health//"},
	} {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			body := checkResponse(t, ts.do(t.Context(), c.method, c.target, ids1("u1", "r1")), http.StatusNotFound, errNotFound)
			if body["user_id"] != "u1" || body["request_id"] != "r1" {
				t.Errorf("body %v, want both valid IDs", body)
			}
		})
	}

	health := checkResponse(t, ts.do(ctx, http.MethodGet, "/health", nil), http.StatusOK, "")
	if health["status"] != "ready" || health["epoch_id"] != "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f" {
		t.Errorf("/health %v: want the library's Health fields", health)
	}
	metrics := ts.do(ctx, http.MethodGet, "/metrics", nil)
	if sent := sentHeader(metrics); metrics.Code != http.StatusOK || sent.Get("Cache-Control") != "no-store" ||
		!strings.HasPrefix(sent.Get("Content-Type"), "text/plain") {
		t.Errorf("/metrics: %d, Cache-Control %q, Content-Type %q", metrics.Code, sent.Get("Cache-Control"), sent.Get("Content-Type"))
	}
	if n := len(lim.calls()); n != 0 {
		t.Errorf("%d Acquire calls, want 0", n)
	}
	if n := len(ts.db.recorded()); n != 0 {
		t.Errorf("%d database calls, want 0", n)
	}
	// Only the two /hit calls are request records.
	reqs := ts.records(t, kindRequest)
	if len(reqs) != 2 || num(t, reqs[0], "http_status") != 405 || reqs[0]["error"] != errMethodNotAllowed {
		t.Errorf("request records %v, want the two 405 /hit calls", reqs)
	}
}

// TestHTTPAPI_Health: /health is the library's Health JSON plus log_drops, in_flight,
// nofile_limit and log_queue_len (the run log's drop count and queue length, here 4 and 3
// from a fake run log), always 200; listener_queue_len only when the build gives it (test
// builds).
func TestHTTPAPI_Health(t *testing.T) {
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{}), health: limiter.Health{
		Status: "not_ready", Reason: "no_epoch", ClockSource: "controlled", ListenerDrops: 2, Fixtures: true,
	}}
	ts := newTestServer(t, testConfig(), lim, &fakeDB{hit: committed()})
	body := checkResponse(t, ts.do(t.Context(), http.MethodGet, "/health", nil), http.StatusOK, "")
	want := []string{"clock_source", "epoch_id", "fixtures", "in_flight", "instance", "listener_drops", "log_drops",
		"log_queue_len", "nofile_limit", "reason", "status", "store_error"}
	if got := keysOf(body); !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v", got, want)
	}
	if body["status"] != "not_ready" || body["reason"] != "no_epoch" || body["fixtures"] != true ||
		num(t, body, "listener_drops") != 2 || num(t, body, "nofile_limit") != 8192 || num(t, body, "in_flight") != 0 ||
		num(t, body, "log_drops") != 0 {
		t.Errorf("body %v", body)
	}

	cfg := testConfig()
	cfg.ListenerQueueLen = func() int { return 7 }
	ts = newTestServer(t, cfg, &fakeLimiter{acquire: allowedWith(&fakePermit{})}, &fakeDB{hit: committed()})
	body = checkResponse(t, ts.do(t.Context(), http.MethodGet, "/health", nil), http.StatusOK, "")
	if num(t, body, "listener_queue_len") != 7 {
		t.Errorf("listener_queue_len %v, want 7", body["listener_queue_len"])
	}

	// log_drops and log_queue_len are the run log's own counts.
	lim = &fakeLimiter{acquire: allowedWith(&fakePermit{}), health: limiter.Health{Status: "ready", Reason: "ready"}}
	s, err := New(testConfig(), lim, &fakeDB{hit: committed()}, &orderLog{drops: 4, qlen: 3})
	if err != nil {
		t.Fatal(err)
	}
	ts = &testServer{s: s, h: s.Handler(), lim: lim}
	body = checkResponse(t, ts.do(t.Context(), http.MethodGet, "/health", nil), http.StatusOK, "")
	if num(t, body, "log_drops") != 4 || num(t, body, "log_queue_len") != 3 {
		t.Errorf("/health log_drops %v, log_queue_len %v; want the run log's 4 and 3", body["log_drops"], body["log_queue_len"])
	}
}

// TestHTTPAPI_Metrics: /metrics renders the MetricsSnapshot as Prometheus text: a TYPE line
// per family, a line per series, and a histogram's cumulative buckets, +Inf bucket, _sum and
// _count.
func TestHTTPAPI_Metrics(t *testing.T) {
	snap := limiter.MetricsSnapshot{Families: []metrics.Family{
		{Name: "rl_pending_refunds", Type: "gauge", Series: []metrics.Series{{Value: 3}}},
		{Name: "rl_acquire_results_total", Type: "counter", LabelName: "result", Series: []metrics.Series{
			{LabelValue: "allowed", Value: 1234567}, {LabelValue: "limited", Value: 0},
		}},
		{Name: "rl_store_call_seconds", Type: "histogram", LabelName: "script", Series: []metrics.Series{{
			LabelValue: "acquire",
			Buckets:    []metrics.Bucket{{UpperBound: 0.001, Count: 2}, {UpperBound: 2, Count: 5}},
			Sum:        0.25,
			Count:      6,
		}}},
	}}
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{}), metrics: snap}
	ts := newTestServer(t, testConfig(), lim, &fakeDB{hit: committed()})
	rec := ts.do(t.Context(), http.MethodGet, "/metrics", nil)
	want := `# TYPE rl_pending_refunds gauge
rl_pending_refunds 3
# TYPE rl_acquire_results_total counter
rl_acquire_results_total{result="allowed"} 1234567
rl_acquire_results_total{result="limited"} 0
# TYPE rl_store_call_seconds histogram
rl_store_call_seconds_bucket{script="acquire",le="0.001"} 2
rl_store_call_seconds_bucket{script="acquire",le="2"} 5
rl_store_call_seconds_bucket{script="acquire",le="+Inf"} 6
rl_store_call_seconds_sum{script="acquire"} 0.25
rl_store_call_seconds_count{script="acquire"} 6
`
	if got := rec.Body.String(); got != want {
		t.Errorf("/metrics body:\n%s\nwant:\n%s", got, want)
	}
	if got := sentHeader(rec).Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
}

// TestHTTPAPI_InFlightCap: with every in-flight slot taken, a new /hit gets 503
// service_overloaded at once, with no acquire (HAR-14, ERR-7); the held requests then finish.
func TestHTTPAPI_InFlightCap(t *testing.T) {
	cfg := testConfig()
	cfg.API.MaxInFlight = 2
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{id: "d1"})}
	entered := make(chan struct{}, 3)
	unblock := make(chan struct{})
	db := &fakeDB{hit: func(_ context.Context, _, _ string, arrivalNs int64) dbclient.Result {
		entered <- struct{}{}
		<-unblock
		return dbclient.Result{ArrivalNs: arrivalNs, Class: dbclient.Committed}
	}}
	ts := newTestServer(t, cfg, lim, db)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = ts.do(t.Context(), http.MethodGet, "/hit", ids1("u1", "held-"+string(rune('a'+i)))).Code
		}()
	}
	defer func() { // never leave a request blocked
		select {
		case <-unblock:
		default:
			close(unblock)
		}
		wg.Wait()
	}()
	recv(t, entered, "the first held request")
	recv(t, entered, "the second held request")

	body := checkResponse(t, ts.do(t.Context(), http.MethodGet, "/health", nil), http.StatusOK, "")
	if got := num(t, body, "in_flight"); got != 2 {
		t.Errorf("in_flight %d, want 2", got)
	}
	// The third request runs on its own goroutine: one that waits for a slot, or takes one and
	// blocks in the database, must fail the test, not hang it.
	type answer struct {
		rec  *httptest.ResponseRecorder
		took time.Duration
	}
	third := make(chan answer, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		rec := ts.get(t, ids1("u1", "third"))
		third <- answer{rec, time.Since(start)}
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case a := <-third:
		checkResponse(t, a.rec, http.StatusServiceUnavailable, errOverloaded)
		if a.took > 100*time.Millisecond {
			t.Errorf("the third /hit answered after %v, want at once (within 100 ms): no queue", a.took)
		}
	case <-timer.C:
		t.Errorf("the third /hit has not answered after 2 s: it waited for or took a slot")
	}
	if n := len(lim.calls()); n != 2 {
		t.Errorf("%d Acquire calls, want 2: the refused request makes none", n)
	}
	close(unblock)
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Errorf("held requests got %v, want 200 and 200", codes)
	}
	ctx, cancel := context.WithTimeout(t.Context(), waitFor)
	defer cancel()
	if err := ts.s.WaitIdle(ctx); err != nil {
		t.Error(err)
	}
}

// TestHTTPAPI_Shutdown: after BeginShutdown (SIGTERM, HAR-15) a new /hit gets 503
// service_overloaded with no acquire, and a request still in its acquire is refused after it:
// release() and 503 service_aborted_after_acquire (ERR-15), with no database work. Stop
// closes the limiter and writes the stop record.
func TestHTTPAPI_Shutdown(t *testing.T) {
	p := &fakePermit{id: "d1"}
	inAcquire := make(chan struct{}, 1)
	proceed := make(chan struct{})
	lim := &fakeLimiter{acquire: func(context.Context, string) Decision {
		select { // never blocks: a /hit that wrongly reaches Acquire after BeginShutdown fails by assertion
		case inAcquire <- struct{}{}:
		default:
		}
		<-proceed
		return Decision{Kind: limiter.Allowed, DecisionNs: testDecisionNs, Permit: p}
	}}
	ts := newTestServer(t, testConfig(), lim, &fakeDB{hit: committed()})
	var wg sync.WaitGroup
	done := make(chan struct{})
	var code int
	var body []byte
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		rec := ts.do(t.Context(), http.MethodGet, "/hit", ids1("u1", "r1"))
		code, body = rec.Code, rec.Body.Bytes()
	}()
	defer func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
		wg.Wait()
	}()
	recv(t, inAcquire, "the request to reach Acquire")

	ts.s.BeginShutdown()
	// A new /hit, on its own goroutine: one that took a slot would block in Acquire.
	second := make(chan *httptest.ResponseRecorder, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		second <- ts.get(t, ids1("u2", "r2"))
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case rec := <-second:
		checkResponse(t, rec, http.StatusServiceUnavailable, errOverloaded)
	case <-timer.C:
		t.Errorf("the /hit sent after BeginShutdown has not answered after 2 s: it took a slot")
	}
	if got := lim.calls(); len(got) != 1 {
		t.Errorf("Acquire keys %q, want [u1]: a /hit after BeginShutdown makes no acquire", got)
	}
	close(proceed)
	recv(t, done, "the request in its acquire")
	if code != http.StatusServiceUnavailable || decodeJSON(t, body)["error"] != errAborted {
		t.Errorf("in-flight request: %d %s, want 503 %s", code, body, errAborted)
	}
	if d, r := p.finished(); d != 0 || r != 1 {
		t.Errorf("permit done %d, released %d; want 0, 1", d, r)
	}
	if got := lim.calls(); len(got) != 1 || len(ts.db.recorded()) != 0 {
		t.Errorf("Acquire keys %q and %d database calls, want [u1] and 0", got, len(ts.db.recorded()))
	}
	ctx, cancel := context.WithTimeout(t.Context(), waitFor)
	defer cancel()
	if err := ts.s.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ts.s.Stop(ctx, waitFor); err != nil {
		t.Fatal(err)
	}
	stops := ts.records(t, kindLifecycle)
	if len(stops) != 1 || stops[0]["event"] != eventStop {
		t.Errorf("lifecycle records %v, want one stop", stops)
	}
}

// TestHTTPAPI_ShutdownDuringDBWork: BeginShutdown (SIGTERM, HAR-15) lets database work whose
// first statement was sent finish: done() and 200. A request still waiting for its database
// connection is refused at the database step's last check before its first statement:
// release() and 503 service_aborted_after_acquire, with no statement sent (ERR-15, §B3).
func TestHTTPAPI_ShutdownDuringDBWork(t *testing.T) {
	t.Run("statement sent", func(t *testing.T) {
		p := &fakePermit{id: "d1"}
		inDB := make(chan struct{}, 1)
		unblock := make(chan struct{})
		db := &fakeDB{hit: func(_ context.Context, _, _ string, arrivalNs int64) dbclient.Result {
			inDB <- struct{}{}
			<-unblock
			return dbclient.Result{ArrivalNs: arrivalNs, Class: dbclient.Committed}
		}}
		ts := newTestServer(t, testConfig(), &fakeLimiter{acquire: allowedWith(p)}, db)
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- ts.get(t, ids1("u1", "r1")) }()
		defer func() {
			select {
			case <-unblock:
			default:
				close(unblock)
			}
		}()
		recv(t, inDB, "the request's statement")
		ts.s.BeginShutdown()
		close(unblock)
		checkResponse(t, recvAnswer(t, answered), http.StatusOK, "")
		if d, r := p.finished(); d != 1 || r != 0 {
			t.Errorf("permit done %d, released %d; want 1, 0", d, r)
		}
		if n := len(ts.records(t, kindRelease)); n != 0 {
			t.Errorf("%d release records, want 0", n)
		}
	})
	t.Run("waiting for a connection", func(t *testing.T) {
		p := &fakePermit{id: "d1"}
		connecting := make(chan struct{}, 1)
		connected := make(chan struct{})
		db := &fakeDB{hit: committed(), connect: func() {
			connecting <- struct{}{}
			<-connected
		}}
		ts := newTestServer(t, testConfig(), &fakeLimiter{acquire: allowedWith(p)}, db)
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- ts.get(t, ids1("u1", "r1")) }()
		defer func() {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}()
		recv(t, connecting, "the request to wait for a connection")
		stop := ts.db.recorded()[0].stop
		if stop == nil || isClosed(stop) {
			t.Fatal("the database step's stop channel is nil or closed before BeginShutdown, want it open")
		}
		ts.s.BeginShutdown()
		if !isClosed(stop) {
			t.Error("the database step's stop channel is still open after BeginShutdown, want it closed")
		}
		ts.s.BeginShutdown() // a second call changes nothing
		close(connected)
		checkResponse(t, recvAnswer(t, answered), http.StatusServiceUnavailable, errAborted)
		if d, r := p.finished(); d != 0 || r != 1 {
			t.Errorf("permit done %d, released %d; want 0, 1", d, r)
		}
		if n := ts.db.statements(); n != 0 {
			t.Errorf("%d calls reached the statements, want 0", n)
		}
		if n := len(ts.records(t, kindRelease)); n != 1 {
			t.Errorf("%d release records, want 1", n)
		}
	})
}

// recvAnswer waits within waitFor for a response from a /hit sent on another goroutine.
func recvAnswer(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	timer := time.NewTimer(waitFor)
	defer timer.Stop()
	select {
	case rec := <-ch:
		return rec
	case <-timer.C:
		t.Fatal("timed out waiting for the /hit response")
		return nil
	}
}

// TestHTTPAPI_WaitIdle: WaitIdle waits while a /hit holds its in-flight slot, until ctx ends,
// and returns once the request has finished and its request record is on the run log (plan
// H1 step 11 builds prekill on it).
func TestHTTPAPI_WaitIdle(t *testing.T) {
	inDB := make(chan struct{}, 1)
	unblock := make(chan struct{})
	db := &fakeDB{hit: func(_ context.Context, _, _ string, arrivalNs int64) dbclient.Result {
		inDB <- struct{}{}
		<-unblock
		return dbclient.Result{ArrivalNs: arrivalNs, Class: dbclient.Committed}
	}}
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{id: "d1"})}
	ts := newTestServer(t, testConfig(), lim, db)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ts.get(t, ids1("u1", "r1"))
	}()
	inHealth := make(chan struct{}, 1)
	healthGo := make(chan struct{})
	defer func() {
		for _, ch := range []chan struct{}{unblock, healthGo} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		<-done
	}()
	recv(t, inDB, "the request's database work")

	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	err := ts.s.WaitIdle(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitIdle while a /hit holds its slot = %v, want the ctx's deadline", err)
	}

	// Hold the request after its response, where it reads Health for the request record's
	// epoch_id, before it enqueues that record.
	lim.onHealth = func() {
		inHealth <- struct{}{}
		<-healthGo
	}
	waited := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		waited <- ts.s.WaitIdle(ctx)
	}()
	waiting := func() bool {
		ts.s.slots.mu.Lock()
		defer ts.s.slots.mu.Unlock()
		return ts.s.slots.idle != nil
	}
	for deadline := time.Now().Add(waitFor); !waiting(); {
		select {
		case err := <-waited:
			t.Fatalf("WaitIdle returned %v while the request held its slot", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("WaitIdle has not started waiting")
		}
		time.Sleep(time.Millisecond)
	}
	close(unblock)
	recv(t, inHealth, "the request to read Health for its record")
	select {
	case err := <-waited:
		t.Fatalf("WaitIdle returned %v before the request record was enqueued", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(healthGo)
	timer := time.NewTimer(waitFor)
	defer timer.Stop()
	select {
	case err := <-waited:
		if err != nil {
			t.Errorf("WaitIdle after the request finished = %v, want nil", err)
		}
	case <-timer.C:
		t.Fatal("WaitIdle has not returned")
	}
	if n := len(ts.records(t, kindRequest)); n != 1 {
		t.Errorf("%d request records once WaitIdle returned, want 1", n)
	}
}

// orderLog is a RunLog that records the order of the calls that matter to Stop: each record
// enqueued (by kind; the stop record kept), and Drain with its ctx's state when it began.
// Drain reports every line queued before it as written.
type orderLog struct {
	drops int64
	qlen  int // what Len reports

	mu            sync.Mutex
	calls         []string
	stop          *stopRecord
	drainErr      error     // the ctx's Err when Drain began
	drainDeadline time.Time // the ctx's deadline
}

func (l *orderLog) Enqueue(rec any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch r := rec.(type) {
	case refundRecord:
		l.calls = append(l.calls, kindRefund)
	case stopRecord:
		l.calls = append(l.calls, eventStop)
		l.stop = &r
	default:
		l.calls = append(l.calls, "other")
	}
}

func (l *orderLog) Drops() int64 { return l.drops }

func (l *orderLog) Len() int { return l.qlen }

func (l *orderLog) Drain(ctx context.Context) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "drain")
	l.drainErr = ctx.Err()
	l.drainDeadline, _ = ctx.Deadline()
	return 0, nil // drained
}

// TestHTTPAPI_StopOrder: Stop closes the limiter first, so the refund events its drain
// finishes reach the run log; then waits for the run log, the wait's timer starting only once
// Close returned; then writes the stop record with the listener drops read after Close and
// the run log's drops (HAR-13, HAR-15, §B8).
func TestHTTPAPI_StopOrder(t *testing.T) {
	const closeTakes, drain = 100 * time.Millisecond, 50 * time.Millisecond
	log := &orderLog{drops: 4}
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{})}
	var closeReturned time.Time
	lim.onClose = func() {
		time.Sleep(closeTakes) // the default drain waits for pending refunds
		RefundListener(log)(limiter.RefundEvent{DeductionID: "d1", Result: limiter.Applied, Attempts: 1})
		lim.health.ListenerDrops = 7
		closeReturned = time.Now()
	}
	s, err := New(testConfig(), lim, &fakeDB{hit: committed()}, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(t.Context(), drain); err != nil {
		t.Fatal(err)
	}
	if want := []string{kindRefund, "drain", eventStop}; !slices.Equal(log.calls, want) {
		t.Errorf("run log calls %v, want %v", log.calls, want)
	}
	if log.drainErr != nil {
		t.Errorf("the wait for the run log began with its ctx done (%v): its timer started before Close returned", log.drainErr)
	}
	if d := log.drainDeadline.Sub(closeReturned); d < drain {
		t.Errorf("the wait's deadline is %v after Close returned, want at least %v", d, drain)
	}
	if log.stop == nil || log.stop.ListenerDrops != 7 || log.stop.LogDrops != 4 {
		t.Errorf("stop record %+v, want listener_drops 7 (after Close) and log_drops 4", log.stop)
	}
	if lim.closed != 1 {
		t.Errorf("limiter closed %d times, want 1", lim.closed)
	}
}

// TestFromLimiter_NilPermit: the adapter leaves Decision.Permit a nil interface for a result
// without a permit (plan H1 Watch out): here a not-ready limiter, built with invalid settings,
// which contacts no store.
func TestFromLimiter_NilPermit(t *testing.T) {
	p := limiter.DefaultPolicy()
	l, err := limiter.New(p, limiter.DefaultInstance(p), limiter.StoreOptions{})
	if err == nil {
		t.Fatal("New accepted settings with the required fields 0")
	}
	defer l.Close()
	d := FromLimiter(l).Acquire(t.Context(), "u1")
	if d.Kind != limiter.NotReady {
		t.Errorf("kind %v, want not_ready", d.Kind)
	}
	if d.Permit != nil {
		t.Errorf("Permit = %#v, want a nil interface", d.Permit)
	}
}
