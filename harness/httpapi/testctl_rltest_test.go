//go:build rltest

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/dbclient"
	"ratelimiter/harness/fixtures"
	"ratelimiter/harness/runlog"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
	"ratelimiter/limiter"
)

// fakeAdapter is the store behind the store wrapper in these tests: every call is sent and
// succeeds, and it counts the calls that reached it.
type fakeAdapter struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeAdapter) Call(context.Context, store.Request) store.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
}

func (f *fakeAdapter) CallBatch(ctx context.Context, reqs []store.Request) []store.Response {
	out := make([]store.Response, len(reqs))
	for i, r := range reqs {
		out[i] = f.Call(ctx, r)
	}
	return out
}

func (f *fakeAdapter) Do(context.Context, store.Kind, ...string) (resp.Value, store.Response) {
	return resp.Value{}, store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
}

func (f *fakeAdapter) ReadyConns(store.Kind) int { return 1 }

func (f *fakeAdapter) Close() {}

func (f *fakeAdapter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ctlServer is a test server with both fixtures installed, and the store wrapper wrapped
// around a fake store so a test can see what the armed faults do to a call.
type ctlServer struct {
	*testServer
	fix   Fixtures
	inner *fakeAdapter
	store store.Adapter // the wrapper around inner
}

func newCtlServer(t *testing.T, cfg Config, db *fakeDB) *ctlServer {
	t.Helper()
	fix := Fixtures{Store: fixtures.NewStoreWrapper(), DB: fixtures.NewDBWrap(db)}
	cfg.Fixtures = fix
	ts := newTestServer(t, cfg, &fakeLimiter{acquire: allowedWith(&fakePermit{id: "d1"})}, db)
	inner := &fakeAdapter{}
	return &ctlServer{testServer: ts, fix: fix, inner: inner, store: fix.Store.Wrap(inner)}
}

// control sends one control request from remote (host:port) and returns the response.
func (cs *ctlServer) control(ctx context.Context, remote, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	cs.h.ServeHTTP(rec, req)
	return rec
}

// call runs one call of script through the store wrapper.
func (cs *ctlServer) call(ctx context.Context, script string) store.Response {
	return cs.store.Call(ctx, store.Request{Script: scripts.Script{Name: script}})
}

const local = "127.0.0.1:40000"

// TestTestctl_RejectsBadRequests: the /_test routes answer 403 to a remote address that is not
// loopback, before anything else; 405 with Allow: POST to another method; and 400
// invalid_request to a body they cannot act on: target "signer" (until H4) or another unknown
// target, an unknown mode, a match, count or timing the fault does not take, unknown keys,
// a body that is not one JSON object. A refused request arms nothing and stops nothing. The
// arming bodies DESIGN §B6 gives without a count or a match are accepted: {target:"db", mode}
// arms the next operation alone, and a barrier {name, at} holds every call ("any").
func TestTestctl_RejectsBadRequests(t *testing.T) {
	db := &fakeDB{hit: committed()}
	cs := newCtlServer(t, testConfig(), db)
	ctx := t.Context()

	valid := map[string]string{
		"/_test/faults":          `{"target":"store","mode":"refuse","match":"acquire","count":1}`,
		"/_test/barrier":         `{"name":"b1","at":"before_send","match":"refund"}`,
		"/_test/barrier/release": `{"name":"b1"}`,
		"/_test/prekill":         ``,
	}
	for path, body := range valid {
		for _, remote := range []string{"192.0.2.1:1234", "10.0.0.7:80", "[2001:db8::1]:443", "localhost:80", "garbage"} {
			rec := cs.control(ctx, remote, http.MethodPost, path, body)
			checkResponse(t, rec, http.StatusForbidden, errForbidden)
		}
		rec := cs.control(ctx, local, http.MethodGet, path, body)
		checkResponse(t, rec, http.StatusMethodNotAllowed, errMethodNotAllowed)
		if allow := sentHeader(rec).Get("Allow"); allow != http.MethodPost {
			t.Errorf("GET %s: Allow %q, want POST", path, allow)
		}
	}

	for _, c := range []struct{ path, body string }{
		{"/_test/faults", `{"target":"signer","mode":"fail","count":1}`},
		{"/_test/faults", `{"target":"signer","mode":"refuse","match":"acquire"}`},
		{"/_test/faults", `{"target":"clock","mode":"refuse","match":"acquire"}`},
		{"/_test/faults", `{"target":"","mode":"refuse","match":"acquire"}`},
		{"/_test/faults", `{"target":"store","mode":"explode","match":"acquire"}`},
		{"/_test/faults", `{"target":"store","mode":"fail_before","match":"acquire"}`},
		{"/_test/faults", `{"target":"store","mode":"barrier","match":"acquire"}`},
		{"/_test/faults", `{"target":"store","mode":"refuse","match":"cleanup_entry"}`},
		{"/_test/faults", `{"target":"store","mode":"refuse"}`},
		{"/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":-1}`},
		{"/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","duration_ms":-5}`},
		{"/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","delay_ms":10}`},
		{"/_test/faults", `{"target":"store","mode":"delay","match":"acquire"}`},
		{"/_test/faults", `{"target":"store","mode":"delay","match":"acquire","delay_ms":-1}`},
		{"/_test/faults", `{"target":"db","mode":"refuse","count":1}`},
		{"/_test/faults", `{"target":"db","mode":"fail_later","count":1}`},
		{"/_test/faults", `{"target":"db","mode":"fail_before","count":0}`},
		{"/_test/faults", `{"target":"db","mode":"fail_before","count":1,"match":"acquire"}`},
		{"/_test/faults", `{"target":"db","mode":"fail_before","count":1,"duration_ms":100}`},
		{"/_test/faults", `{"target":"db","mode":"fail_before","count":1,"delay_ms":100}`},
		{"/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","key":"u1"}`},
		{"/_test/faults", `{"target":"store","mode":"refuse","match":"acquire"} {}`},
		{"/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":"1"}`},
		{"/_test/faults", `[]`},
		{"/_test/faults", ``},
		{"/_test/faults", `not json`},
		{"/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":1,"pad":"` + strings.Repeat("x", 70_000) + `"}`},
		{"/_test/barrier", `{"at":"before_send","match":"acquire"}`},
		{"/_test/barrier", `{"name":"b2","at":"during_send","match":"acquire"}`},
		{"/_test/barrier", `{"name":"b2","at":"after_reply","match":"everything"}`},
		{"/_test/barrier", `{"name":"b2","at":"after_reply","match":"any","count":1}`},
		{"/_test/barrier/release", `{}`},
		{"/_test/barrier/release", `{"name":"b1","at":"before_send"}`},
		{"/_test/barrier/release", `{"name":"b1","match":"acquire"}`},
		{"/_test/prekill", `{"now":true}`},
		{"/_test/prekill", `[]`},
		{"/_test/prekill", `nope`},
	} {
		rec := cs.control(ctx, local, http.MethodPost, c.path, c.body)
		body := checkResponse(t, rec, http.StatusBadRequest, errInvalidRequest)
		if msg, _ := body["message"].(string); msg == "" {
			t.Errorf("POST %s %.80s: no message", c.path, c.body)
		}
	}

	// Nothing was armed or stopped: a call passes the store wrapper, the database sees the
	// next operation, and /hit still admits requests.
	if r := cs.call(ctx, "acquire"); r.Outcome != store.OK || cs.inner.count() != 1 {
		t.Errorf("an acquire call after the refused requests = %+v (store calls %d), want it passed through", r, cs.inner.count())
	}
	if n := cs.fix.DB.Armed(); n != 0 {
		t.Errorf("the database wrapper has %d operations armed, want 0", n)
	}
	checkResponse(t, cs.get(t, ids1("u1", "r1")), http.StatusOK, "")
	if n := db.statements(); n != 1 {
		t.Errorf("%d database operations reached the database, want 1", n)
	}

	// The bodies DESIGN §B6 documents without a count or a match are accepted, with their
	// defaults: a db fault without a count applies to the next operation alone (FLT-5, FLT-9).
	for _, c := range []struct {
		mode string
		want dbclient.Class
	}{
		{"fail_before", dbclient.FailedNoRow},
		{"fail_after_commit", dbclient.FailedRowMayRemain},
		{"lose_commit_reply", dbclient.Unknown},
	} {
		body := `{"target":"db","mode":"` + c.mode + `"}`
		checkResponse(t, cs.control(ctx, local, http.MethodPost, "/_test/faults", body), http.StatusOK, "")
		if n := cs.fix.DB.Armed(); n != 1 {
			t.Errorf("POST /_test/faults %s: %d operations armed, want 1 (the next operation)", body, n)
		}
		if got := cs.fix.DB.Hit(ctx, "u1", "r-"+c.mode, 1_000, nil); got.Class != c.want {
			t.Errorf("the operation after %s = %+v, want class %s", body, got, c.want)
		}
		if got := cs.fix.DB.Hit(ctx, "u1", "r-"+c.mode+"-next", 1_000, nil); got.Class != dbclient.Committed {
			t.Errorf("the second operation after %s = %+v, want committed: the fault applied to one operation", body, got)
		}
	}
	// A barrier without a match holds every call: here a read_epoch call, after its reply.
	checkResponse(t, cs.control(ctx, local, http.MethodPost, "/_test/barrier", `{"name":"b2","at":"after_reply"}`), http.StatusOK, "")
	reached := cs.fix.Store.Reached("b2")
	sent := cs.inner.count()
	held := make(chan store.Response, 1)
	go func() { held <- cs.call(ctx, "read_epoch") }()
	recv(t, reached, "a read_epoch call to reach the barrier without a match")
	if n := cs.inner.count(); n != sent+1 {
		t.Errorf("%d store calls once the call reached the after_reply barrier, want %d", n, sent+1)
	}
	select {
	case r := <-held:
		t.Fatalf("the call held at the barrier without a match returned %+v before the release", r)
	case <-time.After(50 * time.Millisecond):
	}
	checkResponse(t, cs.control(ctx, local, http.MethodPost, "/_test/barrier/release", `{"name":"b2"}`), http.StatusOK, "")
	timer := time.NewTimer(waitFor)
	defer timer.Stop()
	select {
	case r := <-held:
		if r.Outcome != store.OK {
			t.Errorf("the released call = %+v, want it to succeed", r)
		}
	case <-timer.C:
		t.Fatal("the held call has not returned after the release")
	}
}

// TestTestctl_ArmsFixtures: valid control requests from loopback (IPv4 or IPv6) arm exactly the
// fault they name: store refuse, drop_reply and delay on the matching calls for count calls,
// database faults for the next count operations, and a barrier that holds the matching calls
// until its release.
func TestTestctl_ArmsFixtures(t *testing.T) {
	db := &fakeDB{hit: committed()}
	cs := newCtlServer(t, testConfig(), db)
	ctx := t.Context()
	post := func(remote, path, body string) {
		t.Helper()
		checkResponse(t, cs.control(ctx, remote, http.MethodPost, path, body), http.StatusOK, "")
	}

	post(local, "/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":2}`)
	if r := cs.call(ctx, "refund"); r.Outcome != store.OK {
		t.Errorf("a refund call under an acquire refusal = %+v, want it passed through", r)
	}
	for i := range 2 {
		if r := cs.call(ctx, "acquire"); r.Outcome != store.Unavailable || r.Sent {
			t.Errorf("acquire call %d under the refusal = %+v, want unavailable, not sent", i+1, r)
		}
	}
	if r := cs.call(ctx, "acquire"); r.Outcome != store.OK {
		t.Errorf("the third acquire call = %+v, want it passed through: count 2", r)
	}
	if n := cs.inner.count(); n != 2 {
		t.Errorf("%d calls reached the store, want 2 (the refund and the third acquire)", n)
	}

	post("[::1]:5000", "/_test/faults", `{"target":"store","mode":"drop_reply","match":"refund","count":1}`)
	if r := cs.call(ctx, "refund"); r.Outcome != store.OutcomeUnknown || !r.Sent {
		t.Errorf("a refund call under drop_reply = %+v, want outcome_unknown after a send", r)
	}
	if r := cs.call(ctx, "refund"); r.Outcome != store.OK {
		t.Errorf("the next refund call = %+v, want it passed through", r)
	}

	post(local, "/_test/faults", `{"target":"store","mode":"delay","match":"any","count":1,"delay_ms":200}`)
	start := time.Now()
	if r := cs.call(ctx, "read_epoch"); r.Outcome != store.OK {
		t.Errorf("a delayed call = %+v, want it to succeed", r)
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Errorf("the delayed call took %v, want at least 200 ms", took)
	}

	post(local, "/_test/faults", `{"target":"store","mode":"refuse","match":"any","duration_ms":100}`)
	if r := cs.call(ctx, "cleanup_entry"); r.Outcome != store.Unavailable {
		t.Errorf("a call under a timed refusal = %+v, want unavailable", r)
	}
	time.Sleep(150 * time.Millisecond)
	if r := cs.call(ctx, "cleanup_entry"); r.Outcome != store.OK {
		t.Errorf("a call after the refusal's 100 ms = %+v, want it passed through", r)
	}

	post(local, "/_test/faults", `{"target":"db","mode":"fail_before","count":2}`)
	post(local, "/_test/faults", `{"target":"db","mode":"lose_commit_reply","match":"any","count":1}`)
	if n := cs.fix.DB.Armed(); n != 3 {
		t.Errorf("the database wrapper has %d operations armed, want 3", n)
	}
	for _, want := range []dbclient.Class{dbclient.FailedNoRow, dbclient.FailedNoRow, dbclient.Unknown, dbclient.Committed} {
		if got := cs.fix.DB.Hit(ctx, "u1", "r1", 1_000, nil); got.Class != want {
			t.Errorf("database operation = %+v, want class %s", got, want)
		}
	}
	post(local, "/_test/faults", `{"target":"db","mode":"fail_after_commit","count":1}`)
	if got := cs.fix.DB.Hit(ctx, "u1", "r2", 1_000, nil); got.Class != dbclient.FailedRowMayRemain {
		t.Errorf("database operation under fail_after_commit = %+v, want failed_row_may_remain", got)
	}

	// A before_send barrier holds the call before it reaches the store.
	post(local, "/_test/barrier", `{"name":"hold","at":"before_send","match":"acquire"}`)
	reached := cs.fix.Store.Reached("hold")
	sent := cs.inner.count()
	held := make(chan store.Response, 1)
	go func() { held <- cs.call(ctx, "acquire") }()
	recv(t, reached, "the acquire call to reach the barrier")
	if n := cs.inner.count(); n != sent {
		t.Errorf("the acquire call held before its send reached the store (%d store calls, want %d)", n, sent)
	}
	passed := make(chan store.Response, 1)
	go func() { passed <- cs.call(ctx, "refund") }()
	within(t, passed, time.Second, "a refund call past an acquire barrier", store.OK)
	if n := cs.inner.count(); n != sent+1 {
		t.Errorf("%d store calls after the refund call, want %d: the refund only", n, sent+1)
	}
	select {
	case r := <-held:
		t.Fatalf("the held call returned %+v before the release", r)
	case <-time.After(50 * time.Millisecond):
	}
	post(local, "/_test/barrier/release", `{"name":"hold"}`)
	within(t, held, waitFor, "the call released from the before_send barrier", store.OK)
	if n := cs.inner.count(); n != sent+2 {
		t.Errorf("%d store calls after the release, want %d: the released call went out", n, sent+2)
	}

	// An after_reply barrier holds the call once the store has answered it.
	post(local, "/_test/barrier", `{"name":"late","at":"after_reply","match":"refund"}`)
	reached = cs.fix.Store.Reached("late")
	sent = cs.inner.count()
	late := make(chan store.Response, 1)
	go func() { late <- cs.call(ctx, "refund") }()
	recv(t, reached, "the refund call to reach the after_reply barrier")
	if n := cs.inner.count(); n != sent+1 {
		t.Errorf("the refund call held after its reply: %d store calls, want %d (it reached the store)", n, sent+1)
	}
	select {
	case r := <-late:
		t.Fatalf("the call held after its reply returned %+v before the release", r)
	case <-time.After(50 * time.Millisecond):
	}
	post(local, "/_test/barrier/release", `{"name":"late"}`)
	within(t, late, waitFor, "the call released from the after_reply barrier", store.OK)
}

// within waits up to d for a store call made on another goroutine and checks its outcome; a
// call that has not returned by then fails the test.
func within(t *testing.T, ch <-chan store.Response, d time.Duration, what string, want store.Outcome) {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.Outcome != want {
			t.Errorf("%s = %+v, want outcome %v", what, r, want)
		}
	case <-timer.C:
		t.Fatalf("%s has not returned within %v", what, d)
	}
}

// TestTestctl_Prekill: POST /_test/prekill stops accepting /hit (each new one gets 503
// service_overloaded with no acquire, ERR-7) without refusing the admitted ones (no ERR-15:
// a request still in its acquire goes on to its database work), waits for them to finish
// (here a /hit held 1.5 s, past the 1 s slack but within P + 1 s = 6 s), drains the run log,
// and answers listener_drops, log_drops and log_queue_len 0. A body of {} is accepted too.
func TestTestctl_Prekill(t *testing.T) {
	inAcquire := make(chan struct{}, 1)
	proceed := make(chan struct{})
	p := &fakePermit{id: "d1"}
	db := &fakeDB{hit: committed()}
	cs := newCtlServer(t, testConfig(), db)
	cs.lim.acquire = func(context.Context, string) Decision {
		select {
		case inAcquire <- struct{}{}:
		default:
		}
		<-proceed
		return Decision{Kind: limiter.Allowed, DecisionNs: testDecisionNs, Permit: p}
	}
	ctx := t.Context()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- cs.get(t, ids1("u1", "r1")) }()
	defer func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	}()
	recv(t, inAcquire, "the first request to reach Acquire")

	reply := make(chan *httptest.ResponseRecorder, 1)
	go func() { reply <- cs.control(ctx, local, http.MethodPost, "/_test/prekill", `{}`) }()
	// Once prekill has stopped accepting /hit, a new one gets 503 service_overloaded.
	stopped := func() bool {
		cs.s.slots.mu.Lock()
		defer cs.s.slots.mu.Unlock()
		return cs.s.slots.stopped
	}
	for deadline := time.Now().Add(waitFor); !stopped(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("prekill never stopped accepting /hit")
		}
	}
	checkResponse(t, cs.get(t, ids1("u2", "r2")), http.StatusServiceUnavailable, errOverloaded)
	if cs.s.shuttingDown() {
		t.Error("prekill began the SIGTERM refusals (ERR-15), want it to stop accepting /hit only")
	}
	if got := cs.lim.calls(); len(got) != 1 {
		t.Errorf("Acquire keys %q, want [u1]: the refused /hit makes no acquire", got)
	}
	// The admitted /hit stays in flight 1.5 s, past the plan's 1 s slack on its own; with P = 5 s
	// prekill still waits.
	select {
	case rec := <-reply:
		t.Fatalf("prekill answered %d %s while a /hit was still in flight, before P + 1 s", rec.Code, rec.Body)
	case <-time.After(1500 * time.Millisecond):
	}

	close(proceed)
	checkResponse(t, recvAnswer(t, first), http.StatusOK, "")
	if d, r := p.finished(); d != 1 || r != 0 {
		t.Errorf("the admitted request's permit: done %d, released %d; want 1, 0 (prekill refuses no admitted work)", d, r)
	}
	rec := recvAnswer(t, reply)
	body := checkResponse(t, rec, http.StatusOK, "")
	if got := keysOf(body); strings.Join(got, ",") != "listener_drops,log_drops,log_queue_len" {
		t.Errorf("prekill reply keys %v, want listener_drops, log_drops, log_queue_len", got)
	}
	if num(t, body, "listener_drops") != 0 || num(t, body, "log_drops") != 0 || num(t, body, "log_queue_len") != 0 {
		t.Errorf("prekill reply %v, want every count 0", body)
	}
	// The request records of both /hit calls were on the file when prekill answered.
	if n := len(cs.records(t, kindRequest)); n != 2 {
		t.Errorf("%d request records, want 2", n)
	}
}

// TestTestctl_PrekillWaitBound: prekill waits for the admitted /hit requests at most P + 1 s
// (plan H1 step 11). With P = 300 ms and a /hit held in Acquire past that, prekill answers
// between 1.3 s and 2.3 s after it was sent, never earlier, while the /hit is still in
// flight.
func TestTestctl_PrekillWaitBound(t *testing.T) {
	const p = 300 * time.Millisecond
	bound := p + time.Second // the plan's P + 1 s = 1.3 s
	cfg := testConfig()
	cfg.Instance.MaxReleaseDelayNs = int64(p)
	cs := newCtlServer(t, cfg, &fakeDB{hit: committed()})
	inAcquire := make(chan struct{}, 1)
	proceed := make(chan struct{})
	cs.lim.acquire = func(context.Context, string) Decision {
		select {
		case inAcquire <- struct{}{}:
		default:
		}
		<-proceed
		return Decision{Kind: limiter.Allowed, DecisionNs: testDecisionNs, Permit: &fakePermit{id: "d1"}}
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- cs.get(t, ids1("u1", "r1")) }()
	var once sync.Once
	release := func() { once.Do(func() { close(proceed) }) }
	defer release()
	recv(t, inAcquire, "the request to reach Acquire")

	sent := time.Now() // before prekill's wait begins, so a correct answer comes ≥ 1.3 s after it
	reply := make(chan *httptest.ResponseRecorder, 1)
	go func() { reply <- cs.control(t.Context(), local, http.MethodPost, "/_test/prekill", "") }()
	timer := time.NewTimer(bound + time.Second)
	defer timer.Stop()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-reply:
	case <-timer.C:
		t.Fatalf("prekill has not answered within %v of being sent, with a /hit held past P + 1 s = %v", bound+time.Second, bound)
	}
	if took := time.Since(sent); took < bound {
		t.Errorf("prekill answered %v after it was sent, want at least P + 1 s = %v", took, bound)
	}
	select {
	case r := <-first:
		t.Errorf("the held /hit answered %d before prekill did: it was to stay in flight", r.Code)
	default:
	}
	checkResponse(t, rec, http.StatusOK, "")
	release()
	checkResponse(t, recvAnswer(t, first), http.StatusOK, "")
}

// TestTestctl_ReleaseDisarmsBarrier: POST /_test/barrier/release disarms the barrier as well as
// opening it, so a store fault or another barrier armed afterwards on the same calls takes
// effect: the released barrier no longer comes first among the faults they match.
func TestTestctl_ReleaseDisarmsBarrier(t *testing.T) {
	cs := newCtlServer(t, testConfig(), &fakeDB{hit: committed()})
	ctx := t.Context()
	post := func(path, body string) {
		t.Helper()
		checkResponse(t, cs.control(ctx, local, http.MethodPost, path, body), http.StatusOK, "")
	}
	// holdAndRelease arms nothing: it sends an acquire call, wants it held at the barrier
	// name, releases the barrier and wants the call through.
	holdAndRelease := func(name string) {
		t.Helper()
		reached := cs.fix.Store.Reached(name)
		held := make(chan store.Response, 1)
		go func() { held <- cs.call(ctx, "acquire") }()
		recv(t, reached, "the acquire call to reach barrier "+name)
		select {
		case r := <-held:
			t.Fatalf("the call held at barrier %s returned %+v before the release", name, r)
		case <-time.After(50 * time.Millisecond):
		}
		post("/_test/barrier/release", `{"name":"`+name+`"}`)
		within(t, held, waitFor, "the call released from barrier "+name, store.OK)
	}

	post("/_test/barrier", `{"name":"b1","at":"before_send","match":"acquire"}`)
	holdAndRelease("b1")
	post("/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":1}`)
	sent := cs.inner.count()
	if r := cs.call(ctx, "acquire"); r.Outcome != store.Unavailable || r.Sent || cs.inner.count() != sent {
		t.Errorf("an acquire call under a refusal armed after barrier b1's release = %+v, want unavailable, not sent", r)
	}
	post("/_test/faults", `{"target":"store","mode":"drop_reply","match":"acquire","count":1}`)
	if r := cs.call(ctx, "acquire"); r.Outcome != store.OutcomeUnknown || !r.Sent {
		t.Errorf("an acquire call under drop_reply armed after barrier b1's release = %+v, want outcome_unknown after a send", r)
	}

	// A second barrier, here without a match, holds the acquire calls b1 held; once it is
	// released too, a refusal on every call takes effect.
	post("/_test/barrier", `{"name":"b2","at":"after_reply"}`)
	holdAndRelease("b2")
	post("/_test/faults", `{"target":"store","mode":"refuse","match":"any","count":1}`)
	if r := cs.call(ctx, "acquire"); r.Outcome != store.Unavailable || r.Sent {
		t.Errorf("an acquire call under a refusal armed after barrier b2's release = %+v, want unavailable, not sent", r)
	}
	if r := cs.call(ctx, "acquire"); r.Outcome != store.OK {
		t.Errorf("the next acquire call = %+v, want it passed through: every fault was used up or disarmed", r)
	}
}

// TestTestctl_TimeCap: duration_ms and delay_ms are at most math.MaxInt64 / 10^6 =
// 9,223,372,036,854 ms (maxControlMs), the largest whose nanoseconds fit a time.Duration: a
// value above it gets 400 before any conversion, so it cannot wrap into a short or negative
// time and arm a fault that ends at once; a value at it is armed as given.
func TestTestctl_TimeCap(t *testing.T) {
	cs := newCtlServer(t, testConfig(), &fakeDB{hit: committed()})
	ctx := t.Context()
	for _, body := range []string{
		`{"target":"store","mode":"refuse","match":"acquire","duration_ms":9223372036855}`,
		`{"target":"store","mode":"delay","match":"acquire","count":1,"delay_ms":9223372036855}`,
		// 18,446,744,073,710 ms is 448,384 ns past 2^64 ns: converted, it wraps to 0.45 ms.
		`{"target":"store","mode":"refuse","match":"acquire","duration_ms":18446744073710}`,
		`{"target":"store","mode":"delay","match":"acquire","count":1,"delay_ms":18446744073710}`,
		`{"target":"store","mode":"delay","match":"acquire","count":1,"delay_ms":9223372036854775807}`,
	} {
		checkResponse(t, cs.control(ctx, local, http.MethodPost, "/_test/faults", body), http.StatusBadRequest, errInvalidRequest)
	}
	if r := cs.call(ctx, "acquire"); r.Outcome != store.OK {
		t.Errorf("an acquire call after the refused requests = %+v, want it passed through: nothing armed", r)
	}

	// At the cap: a delay holds its call until the call's ctx ends; a refusal outlasts the test.
	checkResponse(t, cs.control(ctx, local, http.MethodPost, "/_test/faults",
		`{"target":"store","mode":"delay","match":"refund","count":1,"delay_ms":9223372036854}`), http.StatusOK, "")
	start := time.Now() // before the call's deadline is created: a call held to it takes ≥ 200 ms
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	cs.call(cctx, "refund")
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Errorf("a refund call under a delay of 9,223,372,036,854 ms returned after %v, want it held until its 200 ms ctx ended", took)
	}
	checkResponse(t, cs.control(ctx, local, http.MethodPost, "/_test/faults",
		`{"target":"store","mode":"refuse","match":"acquire","duration_ms":9223372036854}`), http.StatusOK, "")
	for i := range 2 {
		if r := cs.call(ctx, "acquire"); r.Outcome != store.Unavailable {
			t.Errorf("acquire call %d under a refusal of 9,223,372,036,854 ms = %+v, want unavailable", i+1, r)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTestctl_PrekillReport: prekill drains the run log only once the in-flight /hit has
// queued its records, its drain's deadline 5 s from then (plan choice), and reports the
// limiter's listener drops and the run log's drops as they stand then, with log_queue_len
// the drain's own result: 0, since the drain completed, although Len (the channel's length,
// /health's log_queue_len) reports 3 lines queued since.
func TestTestctl_PrekillReport(t *testing.T) {
	log := &orderLog{drops: 4, qlen: 3}
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{id: "d1"}),
		health: limiter.Health{Status: "ready", Reason: "ready", ListenerDrops: 7}}
	inDB := make(chan struct{}, 1)
	unblock := make(chan struct{})
	db := &fakeDB{hit: func(_ context.Context, _, _ string, arrivalNs int64) dbclient.Result {
		inDB <- struct{}{}
		<-unblock
		return dbclient.Result{ArrivalNs: arrivalNs, Class: dbclient.Committed}
	}}
	s, err := New(testConfig(), lim, db, log)
	if err != nil {
		t.Fatal(err)
	}
	cs := &ctlServer{testServer: &testServer{s: s, h: s.Handler(), lim: lim, db: db}}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- cs.get(t, ids1("u1", "r1")) }()
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	recv(t, inDB, "the request's database work")

	reply := make(chan *httptest.ResponseRecorder, 1)
	go func() { reply <- cs.control(t.Context(), local, http.MethodPost, "/_test/prekill", "") }()
	for deadline := time.Now().Add(waitFor); ; time.Sleep(time.Millisecond) {
		s.slots.mu.Lock()
		stopped := s.slots.stopped
		s.slots.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prekill never stopped accepting /hit")
		}
	}
	finished := time.Now()
	close(unblock)
	checkResponse(t, recvAnswer(t, answered), http.StatusOK, "")
	body := checkResponse(t, recvAnswer(t, reply), http.StatusOK, "")
	end := time.Now()
	if num(t, body, "listener_drops") != 7 || num(t, body, "log_drops") != 4 || num(t, body, "log_queue_len") != 0 {
		t.Errorf("prekill reply %v, want listener_drops 7, log_drops 4 as they stand and log_queue_len 0 (the drain completed)", body)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if want := []string{"other", "other", "drain"}; !slices.Equal(log.calls, want) {
		t.Errorf("run log calls %v, want the decision and request records, then the drain", log.calls)
	}
	if log.drainErr != nil {
		t.Errorf("the drain began with its ctx done: %v", log.drainErr)
	}
	if lo, hi := finished.Add(5*time.Second), end.Add(5*time.Second); log.drainDeadline.Before(lo) || log.drainDeadline.After(hi) {
		t.Errorf("the drain's deadline is %v, want 5 s after the in-flight /hit finished (between %v and %v)", log.drainDeadline, lo, hi)
	}
}

// drainLog is a RunLog for prekill's drain: Drain marks the drain begun, runs during (what
// happens while a real drain waits), then returns left and err. Len reports the lines
// enqueued since Drain began, none of which it has written.
type drainLog struct {
	drops  int64
	during func()
	left   int64
	err    error

	mu       sync.Mutex
	draining bool
	queued   int // lines enqueued since Drain began
}

func (l *drainLog) Enqueue(any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.draining {
		l.queued++
	}
}

func (l *drainLog) Drops() int64 { return l.drops }

func (l *drainLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.queued
}

func (l *drainLog) Drain(context.Context) (int64, error) {
	l.mu.Lock()
	l.draining = true
	l.mu.Unlock()
	if l.during != nil {
		l.during()
	}
	return l.left, l.err
}

// TestTestctl_PrekillDrainResult: prekill's log_queue_len is its run-log drain's own result
// (plan H1 step 11 "log_queue_len 0 when drained"; H4's kill writes read_ok:true only for a
// successful call whose log_queue_len is 0). A /hit refused with ERR-7 while the drain waits
// queues its request record after the drain's snapshot: the drain completed, so the reply is
// 200 with log_queue_len 0 while Len reports that 1 line. A drain that timed out with 1 line
// still in its write (off the channel, so Len is 0) answers 503 with log_queue_len 1 and the
// drop counts.
func TestTestctl_PrekillDrainResult(t *testing.T) {
	serve := func(t *testing.T, log *drainLog) (*ctlServer, *fakeLimiter) {
		t.Helper()
		lim := &fakeLimiter{acquire: allowedWith(&fakePermit{id: "d1"}),
			health: limiter.Health{Status: "ready", Reason: "ready", ListenerDrops: 2}}
		db := &fakeDB{hit: committed()}
		s, err := New(testConfig(), lim, db, log)
		if err != nil {
			t.Fatal(err)
		}
		return &ctlServer{testServer: &testServer{s: s, h: s.Handler(), lim: lim, db: db}}, lim
	}
	reply := func(t *testing.T, rec *httptest.ResponseRecorder, status int) map[string]any {
		t.Helper()
		body := checkResponse(t, rec, status, "")
		if got := keysOf(body); strings.Join(got, ",") != "listener_drops,log_drops,log_queue_len" {
			t.Errorf("prekill reply keys %v, want listener_drops, log_drops, log_queue_len", got)
		}
		return body
	}

	t.Run("lines queued during a drain that completed", func(t *testing.T) {
		log := &drainLog{}
		cs, lim := serve(t, log)
		log.during = func() {
			checkResponse(t, cs.get(t, ids1("u2", "r2")), http.StatusServiceUnavailable, errOverloaded)
		}
		body := reply(t, cs.control(t.Context(), local, http.MethodPost, "/_test/prekill", ""), http.StatusOK)
		if num(t, body, "listener_drops") != 2 || num(t, body, "log_drops") != 0 || num(t, body, "log_queue_len") != 0 {
			t.Errorf("prekill reply %v, want listener_drops 2, log_drops 0, log_queue_len 0 (the drain completed)", body)
		}
		if n := log.Len(); n != 1 {
			t.Errorf("Len() = %d, want 1: the refused /hit's request record, queued during the drain", n)
		}
		if got := lim.calls(); len(got) != 0 {
			t.Errorf("Acquire keys %q, want none: the refused /hit makes no acquire", got)
		}
	})

	t.Run("a drain that timed out with a line in its write", func(t *testing.T) {
		log := &drainLog{drops: 3, left: 1, err: fmt.Errorf("draining the run log: %w", context.DeadlineExceeded)}
		cs, _ := serve(t, log)
		body := reply(t, cs.control(t.Context(), local, http.MethodPost, "/_test/prekill", ""), http.StatusServiceUnavailable)
		if num(t, body, "listener_drops") != 2 || num(t, body, "log_drops") != 3 || num(t, body, "log_queue_len") != 1 {
			t.Errorf("prekill reply %v, want listener_drops 2, log_drops 3 and log_queue_len 1, the line still in its write", body)
		}
		if n := log.Len(); n != 0 {
			t.Errorf("Len() = %d, want 0: the line in its write is off the channel", n)
		}
	})
}

// TestTestctl_PrekillFailedWrite: a line of prekill's drain whose write failed is not on the
// file, so the drain did not complete: prekill answers 503 with log_queue_len counting that
// line, never 200 with 0 (plan H1 step 11 "log_queue_len 0 when drained"; H4's kill then writes
// read_ok:false), and log_drops counts it too. The run log here is a real Writer on a FIFO
// whose reader has gone, so each write fails with EPIPE: the /hit's decision and request
// records, both queued before prekill, are lost.
func TestTestctl_PrekillFailedWrite(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "api-1.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// A reader first, so the run log's write-only open does not wait for one; once it is
	// closed the FIFO has no reader.
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	w, err := runlog.Open(fifo)
	if cerr := r.Close(); err == nil && cerr != nil {
		_ = w.Close()
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); !errors.Is(err, syscall.EPIPE) {
			t.Errorf("closing the run log = %v, want its writes' EPIPE", err)
		}
	})
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{id: "d1"}),
		health: limiter.Health{Status: "ready", Reason: "ready"}}
	db := &fakeDB{hit: committed()}
	s, err := New(testConfig(), lim, db, w)
	if err != nil {
		t.Fatal(err)
	}
	cs := &ctlServer{testServer: &testServer{s: s, h: s.Handler(), lim: lim, db: db}}
	checkResponse(t, cs.get(t, ids1("u1", "r1")), http.StatusOK, "")

	body := checkResponse(t, cs.control(t.Context(), local, http.MethodPost, "/_test/prekill", ""), http.StatusServiceUnavailable, "")
	if got := keysOf(body); strings.Join(got, ",") != "listener_drops,log_drops,log_queue_len" {
		t.Errorf("prekill reply keys %v, want listener_drops, log_drops, log_queue_len", got)
	}
	queued, drops := num(t, body, "log_queue_len"), num(t, body, "log_drops")
	if queued < 1 || drops < queued {
		t.Errorf("prekill reply %v, want log_queue_len at least 1 (the lost records) and log_drops at least log_queue_len (each lost record is a drop)", body)
	}
}

// TestTestctl_DBFaultCap: POST /_test/faults arms at most 64 database faults at once (the
// database wrapper's cap; plan choice, AGENTS.md §1 Queues): the 64th is accepted, the 65th
// answers 400 invalid_request and arms nothing, and once an operation has used up the oldest
// fault the route accepts one more, then refuses the next.
func TestTestctl_DBFaultCap(t *testing.T) {
	cs := newCtlServer(t, testConfig(), &fakeDB{hit: committed()})
	arm := func() *httptest.ResponseRecorder {
		return cs.control(t.Context(), local, http.MethodPost, "/_test/faults", `{"target":"db","mode":"fail_before"}`)
	}
	for i := range 64 {
		if rec := arm(); rec.Code != http.StatusOK {
			t.Fatalf("db fault %d of 64: %d %s, want 200", i+1, rec.Code, rec.Body)
		}
	}
	checkResponse(t, arm(), http.StatusBadRequest, errInvalidRequest)
	if n := cs.fix.DB.Armed(); n != 64 {
		t.Errorf("Armed() = %d after the refused request, want 64", n)
	}
	if got := cs.fix.DB.Hit(t.Context(), "u1", "r1", 1_000, nil); got.Class != dbclient.FailedNoRow {
		t.Errorf("an operation through the wrapper = %+v, want the oldest fault's fail_before", got)
	}
	checkResponse(t, arm(), http.StatusOK, "")
	checkResponse(t, arm(), http.StatusBadRequest, errInvalidRequest)
}

// TestTestctl_StoreFaultCap: POST /_test/faults (target store) and POST /_test/barrier arm at
// most 64 store faults at once, under at most 64 barrier names (the store wrapper's caps, the
// database wrapper's 64; plan choice, AGENTS.md §1 Queues): the 64th is accepted, the 65th
// answers 400 invalid_request and arms nothing, and once a call has used up a fault, or a
// barrier was released, the route accepts one more, then refuses the next.
func TestTestctl_StoreFaultCap(t *testing.T) {
	post := func(cs *ctlServer, path, body string) *httptest.ResponseRecorder {
		return cs.control(t.Context(), local, http.MethodPost, path, body)
	}

	t.Run("store faults", func(t *testing.T) {
		cs := newCtlServer(t, testConfig(), &fakeDB{hit: committed()})
		refusal := `{"target":"store","mode":"refuse","match":"acquire","count":1}`
		for i := range 64 {
			if rec := post(cs, "/_test/faults", refusal); rec.Code != http.StatusOK {
				t.Fatalf("store fault %d of 64: %d %s, want 200", i+1, rec.Code, rec.Body)
			}
		}
		checkResponse(t, post(cs, "/_test/faults", `{"target":"store","mode":"drop_reply","match":"refund"}`),
			http.StatusBadRequest, errInvalidRequest)
		if n := cs.fix.Store.Armed(); n != 64 {
			t.Errorf("Armed() = %d after the refused request, want 64", n)
		}
		if r := cs.call(t.Context(), "refund"); r.Outcome != store.OK {
			t.Errorf("a refund call after the refused drop_reply = %+v, want it passed through: nothing armed on refunds", r)
		}
		if r := cs.call(t.Context(), "acquire"); r.Outcome != store.Unavailable || r.Sent {
			t.Errorf("an acquire call = %+v, want the oldest refusal: unavailable, not sent", r)
		}
		checkResponse(t, post(cs, "/_test/faults", refusal), http.StatusOK, "")
		checkResponse(t, post(cs, "/_test/faults", refusal), http.StatusBadRequest, errInvalidRequest)
		checkResponse(t, post(cs, "/_test/barrier", `{"name":"b1","at":"before_send","match":"refund"}`),
			http.StatusBadRequest, errInvalidRequest)
	})

	t.Run("barriers", func(t *testing.T) {
		cs := newCtlServer(t, testConfig(), &fakeDB{hit: committed()})
		barrier := func(name string) *httptest.ResponseRecorder {
			return post(cs, "/_test/barrier", `{"name":"`+name+`","at":"before_send","match":"refund"}`)
		}
		for i := range 64 {
			if rec := barrier(fmt.Sprintf("b%d", i)); rec.Code != http.StatusOK {
				t.Fatalf("barrier %d of 64: %d %s, want 200", i+1, rec.Code, rec.Body)
			}
		}
		checkResponse(t, barrier("b64"), http.StatusBadRequest, errInvalidRequest)
		checkResponse(t, post(cs, "/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":1}`),
			http.StatusBadRequest, errInvalidRequest)
		if n := cs.fix.Store.Armed(); n != 64 {
			t.Errorf("Armed() = %d after the refused requests, want 64", n)
		}
		if r := cs.call(t.Context(), "acquire"); r.Outcome != store.OK {
			t.Errorf("an acquire call after the refused refusal = %+v, want it passed through: nothing armed on acquires", r)
		}
		checkResponse(t, post(cs, "/_test/barrier/release", `{"name":"b0"}`), http.StatusOK, "")
		checkResponse(t, barrier("b64"), http.StatusOK, "")
		checkResponse(t, barrier("b65"), http.StatusBadRequest, errInvalidRequest)
		// Releasing names that were never armed answers 200 and holds no slot: once b1's
		// release frees one, b65 takes it and b66 is refused.
		for i := range 100 {
			checkResponse(t, post(cs, "/_test/barrier/release", fmt.Sprintf(`{"name":"never%d"}`, i)), http.StatusOK, "")
		}
		checkResponse(t, post(cs, "/_test/barrier/release", `{"name":"b1"}`), http.StatusOK, "")
		checkResponse(t, barrier("b65"), http.StatusOK, "")
		checkResponse(t, barrier("b66"), http.StatusBadRequest, errInvalidRequest)
	})
}
