package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
	"github.com/srir84/ratelimiter/harness/dbclient"
	"github.com/srir84/ratelimiter/harness/runlog"
	"github.com/srir84/ratelimiter/limiter"
)

// testP is P (max_release_delay) in these tests: the harness value (DESIGN §B10).
const testP = 5 * time.Second

// A decision time and a row time for the fakes: whole µs, as the store gives them.
const (
	testDecisionNs = int64(1791374400124002000)
	testRowNs      = int64(1791374400000000123)
)

// fakePermit records how it was finished.
type fakePermit struct {
	id string

	mu             sync.Mutex
	done, released int
}

func (p *fakePermit) Done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
}

func (p *fakePermit) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.released++
}

func (p *fakePermit) DeductionID() string { return p.id }

// finished returns how many times Done and Release were called.
func (p *fakePermit) finished() (done, released int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done, p.released
}

// fakeLimiter answers Acquire with acquire and counts the calls. onHealth, when set, runs at
// the start of every Health call, and onClose inside Close.
type fakeLimiter struct {
	acquire  func(ctx context.Context, key string) Decision
	health   limiter.Health
	metrics  limiter.MetricsSnapshot
	onHealth func()
	onClose  func()

	mu     sync.Mutex
	keys   []string // the key of each Acquire call
	closed int
}

func (f *fakeLimiter) Acquire(ctx context.Context, key string) Decision {
	f.mu.Lock()
	f.keys = append(f.keys, key)
	f.mu.Unlock()
	return f.acquire(ctx, key)
}

func (f *fakeLimiter) Health() limiter.Health {
	if f.onHealth != nil {
		f.onHealth()
	}
	return f.health
}

func (f *fakeLimiter) Metrics() limiter.MetricsSnapshot { return f.metrics }

func (f *fakeLimiter) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	if f.onClose != nil {
		f.onClose()
	}
}

func (f *fakeLimiter) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys)
}

// allowedWith returns an Acquire that allows every call with p.
func allowedWith(p *fakePermit) func(context.Context, string) Decision {
	return func(context.Context, string) Decision {
		return Decision{Kind: limiter.Allowed, DecisionNs: testDecisionNs, Permit: p}
	}
}

// fakeDB records each Hit call and keeps dbclient.Client.Hit's order: it takes a connection
// (connect, when set, stands for the wait in pool.Acquire), then checks the caller's stop
// channel (closed: Cancelled, nothing sent), then runs the statements (hit).
type fakeDB struct {
	hit     func(ctx context.Context, userID, requestID string, arrivalNs int64) dbclient.Result
	connect func()

	mu    sync.Mutex
	calls []dbCall
	sent  int // calls that reached the statements
}

type dbCall struct {
	ctx                context.Context
	userID, requestID  string
	arrivalNs          int64
	deadline           time.Time
	deadlineSet, alive bool            // the ctx had a deadline; it was not done when Hit began
	stop               <-chan struct{} // the caller's cancel signal before the first statement
}

func (f *fakeDB) Hit(ctx context.Context, userID, requestID string, arrivalNs int64, stop <-chan struct{}) dbclient.Result {
	dl, ok := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, dbCall{ctx, userID, requestID, arrivalNs, dl, ok, ctx.Err() == nil, stop})
	f.mu.Unlock()
	if f.connect != nil {
		f.connect()
	}
	if isClosed(stop) {
		return dbclient.Result{Class: dbclient.Cancelled, Err: errors.New("cancelled before the first statement")}
	}
	f.mu.Lock()
	f.sent++
	f.mu.Unlock()
	return f.hit(ctx, userID, requestID, arrivalNs)
}

// isClosed reports whether stop is closed; a nil stop never is.
func isClosed(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// statements returns how many calls reached the statements.
func (f *fakeDB) statements() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent
}

func (f *fakeDB) recorded() []dbCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// committed returns a Hit that commits a new row holding the request's own arrival time.
func committed() func(context.Context, string, string, int64) dbclient.Result {
	return func(_ context.Context, _, _ string, arrivalNs int64) dbclient.Result {
		return dbclient.Result{ArrivalNs: arrivalNs, Class: dbclient.Committed}
	}
}

// testConfig returns the harness values (DESIGN §B2, §B10) with the limiter on.
func testConfig() Config {
	return Config{
		API: config.API{MaxInFlight: 512, LimiterEnabled: true, DBDSN: "postgres://unused"},
		Instance: limiter.Instance{
			MaxReleaseDelayNs:    int64(testP),
			DependencyDeadlineNs: 2_000_000_000,
			DrainTimeoutNs:       35_000_000_000,
		},
		RunID:        "20261007-120000-test-1",
		InstanceNum:  1,
		ProcessStart: time.Now(),
		NofileLimit:  8192,
	}
}

// testServer is one Server with its fakes and a real run log in the test's directory.
type testServer struct {
	s       *Server
	h       http.Handler
	lim     *fakeLimiter
	db      *fakeDB
	log     *runlog.Writer
	logPath string
}

func newTestServer(t *testing.T, cfg Config, lim *fakeLimiter, db *fakeDB) *testServer {
	t.Helper()
	if lim.health.Status == "" {
		lim.health = limiter.Health{Status: "ready", Reason: "ready", EpochID: "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f", ClockSource: "store"}
	}
	path := filepath.Join(t.TempDir(), "api-1.jsonl")
	w, err := runlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("closing the run log: %v", err)
		}
	})
	s, err := New(cfg, lim, db, w)
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{s: s, h: s.Handler(), lim: lim, db: db, log: w, logPath: path}
}

// hdr builds /hit headers; a nil slice leaves that header out.
func hdr(user, request []string) http.Header {
	h := http.Header{}
	for _, v := range user {
		h.Add(headerUserID, v)
	}
	for _, v := range request {
		h.Add(headerRequestID, v)
	}
	return h
}

// ids1 is one valid value for each /hit header.
func ids1(user, request string) http.Header { return hdr([]string{user}, []string{request}) }

// do serves one request with ctx and returns the response.
func (ts *testServer) do(ctx context.Context, method, target string, h http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, target, nil)
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)
	return rec
}

// get sends GET /hit.
func (ts *testServer) get(t *testing.T, h http.Header) *httptest.ResponseRecorder {
	t.Helper()
	return ts.do(t.Context(), http.MethodGet, "/hit", h)
}

// lines waits until the run log has written every line enqueued so far and returns them.
func (ts *testServer) lines(t *testing.T) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := ts.log.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ts.logPath)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// records returns the run log's records of kind, decoded.
func (ts *testServer) records(t *testing.T, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range ts.lines(t) {
		m := decodeJSON(t, line)
		if m["kind"] == kind {
			out = append(out, m)
		}
	}
	return out
}

// decodeJSON decodes one JSON object, keeping numbers as json.Number.
func decodeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not a JSON object: %v: %s", err, data)
	}
	return m
}

// keysOf returns m's keys, sorted.
func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// num reads a JSON number field as int64.
func num(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	n, ok := m[key].(json.Number)
	if !ok {
		t.Fatalf("%s = %#v, want a JSON number", key, m[key])
	}
	v, err := n.Int64()
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return v
}

// sentHeader returns the headers a client received: the snapshot taken at WriteHeader, not
// the recorder's live map, which also holds headers set after the response was written.
func sentHeader(rec *httptest.ResponseRecorder) http.Header {
	res := rec.Result()
	_ = res.Body.Close() // the recorder's body is in memory: closing it cannot fail
	return res.Header
}

// checkResponse checks the status, the JSON content type, Cache-Control: no-store (HTTP-8)
// and the error code ("" for none), and returns the decoded body.
func checkResponse(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status %d, want %d (body %s)", rec.Code, status, rec.Body.Bytes())
	}
	sent := sentHeader(rec)
	if got := sent.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q, want application/json", got)
	}
	if got := sent.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	body := decodeJSON(t, rec.Body.Bytes())
	got, has := body["error"]
	switch {
	case code == "" && has:
		t.Errorf("error %v, want none", got)
	case code != "" && got != code:
		t.Errorf("error %v, want %s", got, code)
	}
	if code != "" {
		if msg, _ := body["message"].(string); msg == "" {
			t.Errorf("message %#v, want a short text", body["message"])
		}
	}
	return body
}

// TestHTTPAPI_HeaderValidation: X-User-ID and X-Request-ID must each be exactly one value of
// 1-64 characters from [A-Za-z0-9_-] (HTTP-2, HTTP-3); anything else is 400 invalid_request
// (ERR-5) with no acquire and no database work, and the error body carries the ID that was
// valid (HTTP-7).
func TestHTTPAPI_HeaderValidation(t *testing.T) {
	id64 := strings.Repeat("a", 64)
	id65 := strings.Repeat("b", 65)
	ok := []string{"r1"}
	cases := []struct {
		name          string
		user, request []string // nil: header absent
		// valid: 200; else 400, and the body echoes each ID marked echo (the valid ones)
		valid                 bool
		echoUser, echoRequest bool
	}{
		{"valid", []string{"u1"}, ok, true, false, false},
		{"length 1", []string{"u"}, []string{"r"}, true, false, false},
		{"length 64", []string{id64}, []string{id64}, true, false, false},
		{"every character class", []string{"AZaz09_-"}, []string{"-_9aZ"}, true, false, false},
		{"user missing", nil, ok, false, false, true},
		{"request missing", []string{"u1"}, nil, false, true, false},
		{"both missing", nil, nil, false, false, false},
		{"user duplicate", []string{"u1", "u2"}, ok, false, false, true},
		{"user duplicate same value", []string{"u1", "u1"}, ok, false, false, true},
		{"request duplicate", []string{"u1"}, []string{"r1", "r2"}, false, true, false},
		{"user length 0", []string{""}, ok, false, false, true},
		{"request length 0", []string{"u1"}, []string{""}, false, true, false},
		{"user length 65", []string{id65}, ok, false, false, true},
		{"request length 65", []string{"u1"}, []string{id65}, false, true, false},
		{"space", []string{"a b"}, ok, false, false, true},
		{"dot", []string{"a.b"}, ok, false, false, true},
		{"comma list", []string{"a,b"}, ok, false, false, true},
		{"slash", []string{"u1"}, []string{"r/1"}, false, true, false},
		{"colon", []string{"u:1"}, ok, false, false, true},
		{"plus", []string{"u+1"}, ok, false, false, true},
		{"non-ASCII", []string{"\u00fc"}, ok, false, false, true},
		{"tab", []string{"u\t1"}, ok, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &fakePermit{id: "d1"}
			ts := newTestServer(t, testConfig(), &fakeLimiter{acquire: allowedWith(p)}, &fakeDB{hit: committed()})
			rec := ts.get(t, hdr(c.user, c.request))
			if c.valid {
				checkResponse(t, rec, http.StatusOK, "")
				if got := ts.lim.calls(); len(got) != 1 || got[0] != c.user[0] {
					t.Errorf("Acquire keys %q, want [%q]", got, c.user[0])
				}
				if n := len(ts.db.recorded()); n != 1 {
					t.Errorf("%d database calls, want 1", n)
				}
				return
			}
			body := checkResponse(t, rec, http.StatusBadRequest, errInvalidRequest)
			if n := len(ts.lim.calls()); n != 0 {
				t.Errorf("%d Acquire calls, want 0", n)
			}
			if n := len(ts.db.recorded()); n != 0 {
				t.Errorf("%d database calls, want 0", n)
			}
			for _, f := range []struct {
				key  string
				vals []string
				echo bool
			}{{"user_id", c.user, c.echoUser}, {"request_id", c.request, c.echoRequest}} {
				got, has := body[f.key]
				switch {
				case f.echo && got != f.vals[0]:
					t.Errorf("%s %#v, want %q", f.key, got, f.vals[0])
				case !f.echo && has:
					t.Errorf("%s %#v, want no %s for an invalid header", f.key, got, f.key)
				}
			}
			reqs := ts.records(t, kindRequest)
			if len(reqs) != 1 || num(t, reqs[0], "http_status") != http.StatusBadRequest || reqs[0]["error"] != errInvalidRequest ||
				reqs[0]["result"] != "" {
				t.Errorf("request records %v, want one with 400 invalid_request and no result", reqs)
			}
		})
	}
}

// TestHTTPAPI_ResultMapping: every ResultKind maps to its HARNESS ERR row (HAR-18, HOST-4);
// only limited gives 429. Allowed and degraded go on to the database work; the other kinds
// do none.
func TestHTTPAPI_ResultMapping(t *testing.T) {
	cases := []struct {
		kind   limiter.ResultKind
		result string // the request record's result: the SPEC name
		status int
		code   string
	}{
		{limiter.Allowed, "allowed", 200, ""},                                                        // ERR-1
		{limiter.Degraded, "degraded", 200, ""},                                                      // ERR-3
		{limiter.Limited, "limited", 429, "rate_limited"},                                            // ERR-4
		{limiter.ValidationError, "validation_error", 400, "invalid_request"},                        // ERR-12d
		{limiter.ClockError, "clock_error", 503, "limiter_clock_error"},                              // ERR-12b
		{limiter.Busy, "busy", 503, "service_overloaded"},                                            // ERR-7
		{limiter.Cancelled, "cancelled", 503, "limiter_deadline_exceeded"},                           // ERR-9a
		{limiter.ConfigError, "config_error", 503, "limiter_config_error"},                           // ERR-12
		{limiter.NotReady, "not_ready", 503, "limiter_not_ready"},                                    // ERR-12a
		{limiter.StateIntegrityError, "state_integrity_error", 503, "limiter_state_integrity_error"}, // ERR-11
		{limiter.CapacityError, "capacity_error", 503, "limiter_capacity_exceeded"},                  // ERR-10
		{limiter.Unavailable, "unavailable", 503, "limiter_unavailable"},                             // ERR-8
		{limiter.OutcomeUnknown, "outcome_unknown", 503, "limiter_outcome_unknown"},                  // ERR-9
		{limiter.ReceiptError, "receipt_error", 503, "limiter_receipt_error"},                        // ERR-12c
	}
	seen := map[limiter.ResultKind]bool{}
	for _, c := range cases {
		seen[c.kind] = true
	}
	if len(cases) != 14 || len(seen) != 14 {
		t.Fatalf("%d cases over %d kinds, want all 14 ResultKind values once", len(cases), len(seen))
	}
	for _, c := range cases {
		t.Run(c.result, func(t *testing.T) {
			p := &fakePermit{}
			if c.kind == limiter.Allowed {
				p.id = "d1"
			}
			lim := &fakeLimiter{acquire: func(context.Context, string) Decision {
				d := Decision{Kind: c.kind}
				if c.kind == limiter.Allowed {
					d.DecisionNs, d.Permit = testDecisionNs, p
				}
				if c.kind == limiter.Degraded {
					d.Permit = p
				}
				if c.kind == limiter.Limited {
					d.DecisionNs, d.RetryAfterNs = testDecisionNs, 1_500_000_000
				}
				return d
			}}
			ts := newTestServer(t, testConfig(), lim, &fakeDB{hit: committed()})
			body := checkResponse(t, ts.get(t, ids1("u1", "r1")), c.status, c.code)
			if (c.status == http.StatusTooManyRequests) != (c.kind == limiter.Limited) {
				t.Errorf("%s gave %d: only limited gives 429", c.result, c.status)
			}
			goesOn := c.kind == limiter.Allowed || c.kind == limiter.Degraded
			wantDB, wantDone := 0, 0
			if goesOn {
				wantDB, wantDone = 1, 1
			}
			if n := len(ts.db.recorded()); n != wantDB {
				t.Errorf("%d database calls, want %d", n, wantDB)
			}
			if done, released := p.finished(); done != wantDone || released != 0 {
				t.Errorf("permit done %d, released %d; want %d, 0", done, released, wantDone)
			}
			// The bodies have exactly the HTTP-6 / HTTP-7 fields.
			want, special := map[limiter.ResultKind][]string{
				limiter.Allowed:  {"arrival_ns", "decision_ns", "degraded", "existed", "request_id", "user_id"},
				limiter.Degraded: {"arrival_ns", "degraded", "existed", "request_id", "user_id"},
				limiter.Limited:  {"decision_ns", "error", "message", "request_id", "retry_after_ms", "user_id"},
			}[c.kind]
			if !special {
				want = []string{"error", "message", "request_id", "user_id"}
			}
			if got := keysOf(body); !slices.Equal(got, want) {
				t.Errorf("body keys %v, want %v", got, want)
			}
			if c.kind == limiter.Degraded && body["degraded"] != true {
				t.Errorf("degraded %v, want true", body["degraded"])
			}
			if c.kind == limiter.Allowed && (body["degraded"] != false || body["decision_ns"] != "1791374400124002000") {
				t.Errorf("degraded %v, decision_ns %#v; want false, \"1791374400124002000\"", body["degraded"], body["decision_ns"])
			}
			reqs := ts.records(t, kindRequest)
			if len(reqs) != 1 || reqs[0]["result"] != c.result || num(t, reqs[0], "http_status") != int64(c.status) ||
				reqs[0]["error"] != c.code {
				t.Errorf("request records %v, want one with result %s, %d, error %q", reqs, c.result, c.status, c.code)
			}
			wantDecisions := 0
			if goesOn {
				wantDecisions = 1
			}
			if n := len(ts.records(t, kindDecision)); n != wantDecisions {
				t.Errorf("%d decision records, want %d", n, wantDecisions)
			}
		})
	}
}

// TestHTTPAPI_RetryHints: a 429 carries Retry-After in whole seconds rounded up and
// retry_after_ms rounded up, from retry_after_ns, plus the decision's decision_ns (HTTP-6,
// HTTP-9). The values are the plan's.
func TestHTTPAPI_RetryHints(t *testing.T) {
	for _, c := range []struct {
		ns, seconds, ms int64
	}{
		{1, 1, 1},
		{999_999, 1, 1},
		{1_000_000, 1, 1},
		{1_000_000_000, 1, 1000}, // exactly 1 s: whole seconds stay as they are
		{1_000_000_001, 2, 1001},
	} {
		t.Run(strconv.FormatInt(c.ns, 10), func(t *testing.T) {
			if s, ms := retryHints(c.ns); s != c.seconds || ms != c.ms {
				t.Errorf("retryHints(%d) = (%d s, %d ms), want (%d, %d)", c.ns, s, ms, c.seconds, c.ms)
			}
			lim := &fakeLimiter{acquire: func(context.Context, string) Decision {
				return Decision{Kind: limiter.Limited, DecisionNs: testDecisionNs, RetryAfterNs: c.ns}
			}}
			ts := newTestServer(t, testConfig(), lim, &fakeDB{hit: committed()})
			rec := ts.get(t, ids1("u1", "r1"))
			body := checkResponse(t, rec, http.StatusTooManyRequests, errRateLimited)
			if got := sentHeader(rec).Get("Retry-After"); got != strconv.FormatInt(c.seconds, 10) {
				t.Errorf("Retry-After %q, want %d", got, c.seconds)
			}
			if got := num(t, body, "retry_after_ms"); got != c.ms {
				t.Errorf("retry_after_ms %d, want %d", got, c.ms)
			}
			if body["decision_ns"] != "1791374400124002000" || body["user_id"] != "u1" || body["request_id"] != "r1" {
				t.Errorf("body %v: want decision_ns \"1791374400124002000\" and both IDs", body)
			}
			if n := len(ts.db.recorded()); n != 0 {
				t.Errorf("%d database calls after a 429, want 0", n)
			}
		})
	}
}

// TestHTTPAPI_RefuseSelection: the refusal after acquire (ERR-15, §B3) selects a request when
// the FNV-1a 64-bit hash of user_id ‖ 0x00 ‖ request_id, mod 10,000, is below
// refuse_after_acquire_bp; a selected request is released and gets 503
// service_aborted_after_acquire with no database work.
func TestHTTPAPI_RefuseSelection(t *testing.T) {
	// The plan's vectors.
	if got := refuseBucket("u1", "r1"); got != 64 {
		t.Errorf(`bucket("u1", "r1") = %d, want 64`, got)
	}
	if got := refuseBucket("alice", "req-1"); got != 9537 {
		t.Errorf(`bucket("alice", "req-1") = %d, want 9537`, got)
	}
	// A bucket-0 request, computed by hand: FNV-1a 64 of "u1" 0x00 "r1901" is
	// 0xd4a5a6d151d722d0, which is 0 mod 10,000.
	if got := refuseBucket("u1", "r1901"); got != 0 {
		t.Errorf(`bucket("u1", "r1901") = %d, want 0`, got)
	}

	// One request at each side of its threshold (bucket < bp).
	for _, c := range []struct {
		user, request string
		bp            int
		refused       bool
	}{
		{"u1", "r1", 64, false},
		{"u1", "r1", 65, true},
		{"alice", "req-1", 9537, false},
		{"alice", "req-1", 9538, true},
		{"u1", "r1901", 0, false},
		{"u1", "r1901", 1, true},
	} {
		t.Run(c.user+"/"+strconv.Itoa(c.bp), func(t *testing.T) {
			cfg := testConfig()
			cfg.API.RefuseAfterAcquireBP = c.bp
			p := &fakePermit{id: "d1"}
			ts := newTestServer(t, cfg, &fakeLimiter{acquire: allowedWith(p)}, &fakeDB{hit: committed()})
			rec := ts.get(t, ids1(c.user, c.request))
			done, released := p.finished()
			if c.refused {
				checkResponse(t, rec, http.StatusServiceUnavailable, errAborted)
				if done != 0 || released != 1 || len(ts.db.recorded()) != 0 {
					t.Errorf("done %d, released %d, %d database calls; want 0, 1, 0", done, released, len(ts.db.recorded()))
				}
				// The admission stays in the run log for the refund to credit.
				if len(ts.records(t, kindDecision)) != 1 || len(ts.records(t, kindRelease)) != 1 {
					t.Errorf("want one decision and one release record")
				}
				return
			}
			checkResponse(t, rec, http.StatusOK, "")
			if done != 1 || released != 0 || len(ts.db.recorded()) != 1 {
				t.Errorf("done %d, released %d, %d database calls; want 1, 0, 1", done, released, len(ts.db.recorded()))
			}
		})
	}

	// bp 0 refuses none; bp 10,000 refuses all.
	for _, c := range []struct {
		bp      int
		refused int
	}{{0, 0}, {config.MaxRefuseBP, 200}} {
		t.Run("bp "+strconv.Itoa(c.bp), func(t *testing.T) {
			cfg := testConfig()
			cfg.API.RefuseAfterAcquireBP = c.bp
			p := &fakePermit{id: "d1"}
			ts := newTestServer(t, cfg, &fakeLimiter{acquire: allowedWith(p)}, &fakeDB{hit: committed()})
			refused := 0
			for i := range 200 {
				rec := ts.get(t, ids1("user"+strconv.Itoa(i%7), "req-"+strconv.Itoa(i)))
				if rec.Code == http.StatusServiceUnavailable && decodeJSON(t, rec.Body.Bytes())["error"] == errAborted {
					refused++
				}
			}
			if refused != c.refused {
				t.Errorf("%d of 200 refused, want %d", refused, c.refused)
			}
			if _, released := p.finished(); released != c.refused {
				t.Errorf("%d releases, want %d", released, c.refused)
			}
			if n := len(ts.db.recorded()); n != 200-c.refused {
				t.Errorf("%d database calls, want %d", n, 200-c.refused)
			}
		})
	}

	// A baseline run refuses the same requests, with no acquire and nothing to release.
	t.Run("baseline", func(t *testing.T) {
		cfg := testConfig()
		cfg.API.LimiterEnabled = false
		cfg.API.RefuseAfterAcquireBP = 65
		ts := newTestServer(t, cfg, &fakeLimiter{acquire: allowedWith(&fakePermit{})}, &fakeDB{hit: committed()})
		checkResponse(t, ts.get(t, ids1("u1", "r1")), http.StatusServiceUnavailable, errAborted)
		if len(ts.lim.calls()) != 0 || len(ts.db.recorded()) != 0 || len(ts.records(t, kindRelease)) != 0 {
			t.Errorf("want no acquire, no database call and no release record")
		}
	})
}

// TestHTTPAPI_DBOutcomeToPermit: each database outcome class finishes the permit and answers
// per the DESIGN §B3 table: done() 200 for committed (an existing row keeps its original
// arrival_ns, HTTP-10); release() 503 database_unavailable for a confirmed failure with no
// row; release() 500 internal_error when the row may remain; done() 503
// application_outcome_unknown for an unknown outcome; release() 503
// service_aborted_after_acquire when SIGTERM cancelled the work before its first statement.
func TestHTTPAPI_DBOutcomeToPermit(t *testing.T) {
	for _, c := range []struct {
		name           string
		res            dbclient.Result
		done, released int
		status         int
		code           string
	}{
		{"committed new row", dbclient.Result{ArrivalNs: testRowNs, Class: dbclient.Committed}, 1, 0, 200, ""},
		{"committed existing row", dbclient.Result{ArrivalNs: testRowNs, Existed: true, Class: dbclient.Committed}, 1, 0, 200, ""},
		{"failed no row", dbclient.Result{Class: dbclient.FailedNoRow, Err: errors.New("refused")}, 0, 1, 503, "database_unavailable"},
		{"failed row may remain", dbclient.Result{Class: dbclient.FailedRowMayRemain, Err: errors.New("after commit")}, 0, 1, 500, "internal_error"},
		{"unknown", dbclient.Result{Class: dbclient.Unknown, Err: errors.New("reply lost")}, 1, 0, 503, "application_outcome_unknown"},
		// SIGTERM cancelled the work before its first statement: the refusal after acquire (ERR-15).
		{"cancelled before the first statement", dbclient.Result{Class: dbclient.Cancelled, Err: errors.New("cancelled")}, 0, 1, 503, "service_aborted_after_acquire"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &fakePermit{id: "d1"}
			db := &fakeDB{hit: func(context.Context, string, string, int64) dbclient.Result { return c.res }}
			ts := newTestServer(t, testConfig(), &fakeLimiter{acquire: allowedWith(p)}, db)
			body := checkResponse(t, ts.get(t, ids1("u1", "r1")), c.status, c.code)
			if done, released := p.finished(); done != c.done || released != c.released {
				t.Errorf("done %d, released %d; want %d, %d", done, released, c.done, c.released)
			}
			if c.code == "" {
				if body["arrival_ns"] != strconv.FormatInt(testRowNs, 10) || body["existed"] != c.res.Existed {
					t.Errorf("arrival_ns %#v, existed %v; want the row's %d, %v", body["arrival_ns"], body["existed"], testRowNs, c.res.Existed)
				}
			}
			if n := len(ts.records(t, kindRelease)); n != c.released {
				t.Errorf("%d release records, want %d", n, c.released)
			}
			reqs := ts.records(t, kindRequest)
			if len(reqs) != 1 || reqs[0]["existed"] != c.res.Existed || reqs[0]["deduction_id"] != "d1" {
				t.Errorf("request records %v, want one with existed %v and deduction_id d1", reqs, c.res.Existed)
			}
		})
	}
}

// TestHTTPAPI_PDeadline: P starts just before acquire, so the acquire's ctx deadline is the
// call's start + P (± 1 ms), and the database work gets the same ctx (HTTP-13, HAR-11). The
// ctx is not cancelled with the request: a client that goes away cannot change the outcome.
// arrival_ns is taken first (HTTP-5): with the in-flight slots' lock held for 200 ms while
// the request starts, its arrival comes before the slot is free, and P counts only from then.
func TestHTTPAPI_PDeadline(t *testing.T) {
	const hold = 200 * time.Millisecond
	var acquireCtx context.Context
	var acquireAt time.Time
	p := &fakePermit{id: "d1"}
	lim := &fakeLimiter{acquire: func(ctx context.Context, _ string) Decision {
		acquireCtx, acquireAt = ctx, time.Now()
		// An arrival time taken after Acquire would be later than acquireAt by at least this.
		time.Sleep(5 * time.Millisecond)
		return Decision{Kind: limiter.Allowed, DecisionNs: testDecisionNs, Permit: p}
	}}
	reqCtx, cancelReq := context.WithCancel(t.Context())
	defer cancelReq()
	var liveAfterDisconnect bool
	db := &fakeDB{hit: func(ctx context.Context, _, _ string, arrivalNs int64) dbclient.Result {
		cancelReq() // the client goes away during the database work
		liveAfterDisconnect = ctx.Err() == nil
		return dbclient.Result{ArrivalNs: arrivalNs, Class: dbclient.Committed}
	}}
	ts := newTestServer(t, testConfig(), lim, db)
	answered := make(chan *httptest.ResponseRecorder, 1)
	ts.s.slots.mu.Lock()
	go func() { answered <- ts.do(reqCtx, http.MethodGet, "/hit", ids1("u1", "r1")) }()
	time.Sleep(hold) // the request takes its arrival time, then waits for the slots' lock
	slotFree := time.Now()
	ts.s.slots.mu.Unlock()
	timer := time.NewTimer(waitFor)
	defer timer.Stop()
	select {
	case rec := <-answered:
		checkResponse(t, rec, http.StatusOK, "")
	case <-timer.C:
		t.Fatalf("the request has not answered %v after the slot was free", waitFor)
	}

	if acquireCtx == nil {
		t.Fatal("Acquire was not called")
	}
	deadline, ok := acquireCtx.Deadline()
	if !ok {
		t.Fatal("the acquire ctx has no deadline")
	}
	if d := deadline.Sub(acquireAt.Add(testP)); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("acquire deadline = call start + P %+v, want ± 1 ms", d)
	}
	// P counts from after the slot, not from arrival (200 ms earlier).
	if d := deadline.Sub(slotFree.Add(testP)); d < 0 || d > time.Millisecond {
		t.Errorf("acquire deadline = slot free + P %+v, want within [0, 1 ms]", d)
	}
	calls := ts.db.recorded()
	if len(calls) != 1 {
		t.Fatalf("%d database calls, want 1", len(calls))
	}
	if calls[0].arrivalNs > acquireAt.UnixNano() || calls[0].arrivalNs >= slotFree.UnixNano() {
		t.Errorf("arrival_ns %d, want before the slot was free (%d) and before Acquire (%d)",
			calls[0].arrivalNs, slotFree.UnixNano(), acquireAt.UnixNano())
	}
	if calls[0].ctx != acquireCtx {
		t.Error("the database work got another ctx than the acquire")
	}
	if !calls[0].deadlineSet || !calls[0].deadline.Equal(deadline) || !calls[0].alive {
		t.Errorf("database ctx deadline %v (set %v, live %v), want the acquire's %v", calls[0].deadline, calls[0].deadlineSet, calls[0].alive, deadline)
	}
	if !liveAfterDisconnect {
		t.Error("the P ctx ended when the request's ctx was cancelled")
	}
}

// TestHTTPAPI_BaselineSkipsLimiter: with limiter_enabled = false (HAR-17, TRF-8) the handler
// makes no Acquire call and still validates, writes the row (within P) and answers 200, with
// no decision_ns and degraded false.
func TestHTTPAPI_BaselineSkipsLimiter(t *testing.T) {
	cfg := testConfig()
	cfg.API.LimiterEnabled = false
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{id: "d1"})}
	ts := newTestServer(t, cfg, lim, &fakeDB{hit: committed()})
	before := time.Now()
	body := checkResponse(t, ts.get(t, ids1("u1", "r1")), http.StatusOK, "")
	if n := len(lim.calls()); n != 0 {
		t.Errorf("%d Acquire calls, want 0", n)
	}
	calls := ts.db.recorded()
	if len(calls) != 1 || calls[0].userID != "u1" || calls[0].requestID != "r1" || calls[0].arrivalNs < before.UnixNano() {
		t.Fatalf("database calls %+v, want one row (u1, r1) with this call's arrival", calls)
	}
	if d := time.Until(calls[0].deadline); !calls[0].deadlineSet || d > testP || d < testP-time.Second {
		t.Errorf("database ctx deadline in %v, want about P", d)
	}
	if _, has := body["decision_ns"]; has || body["degraded"] != false || body["existed"] != false {
		t.Errorf("body %v: want no decision_ns, degraded false, existed false", body)
	}
	if body["arrival_ns"] != strconv.FormatInt(calls[0].arrivalNs, 10) {
		t.Errorf("arrival_ns %#v, want the row's %d", body["arrival_ns"], calls[0].arrivalNs)
	}
	if n := len(ts.records(t, kindDecision)); n != 0 {
		t.Errorf("%d decision records, want 0", n)
	}
	reqs := ts.records(t, kindRequest)
	if len(reqs) != 1 || reqs[0]["result"] != "" || num(t, reqs[0], "decision_ns") != 0 || num(t, reqs[0], "limiter_ns") != 0 {
		t.Errorf("request records %v, want one with no result, decision_ns 0, limiter_ns 0", reqs)
	}
}
