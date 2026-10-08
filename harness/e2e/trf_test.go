//go:build rltest

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
)

// hitgenWait bounds a bin/test/hitgen run of a few seconds of schedule and its retries.
const hitgenWait = 60 * time.Second

// trfArrival is one request the T-TRF-1 stub received: its IDs, method and target, and when
// it arrived (monotonic, from the stub's start).
type trfArrival struct {
	user, request string
	method, path  string
	query         string // the raw query: empty for /hit (HTTP-1)
	at            time.Duration
}

// trfStub is T-TRF-1's loopback server: it answers each request by its X-User-ID.
type trfStub struct {
	srv   *httptest.Server
	start time.Time
	quit  chan struct{} // closed at cleanup: a hanging request returns

	mu       sync.Mutex
	arrivals []trfArrival
}

func newTRFStub(t *testing.T) *trfStub {
	t.Helper()
	s := &trfStub{start: time.Now(), quit: make(chan struct{})}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(t, w, r) }))
	s.srv.Config.ReadHeaderTimeout = 10 * time.Second
	s.srv.Start()
	t.Cleanup(func() {
		close(s.quit)
		s.srv.CloseClientConnections()
		s.srv.Close()
	})
	return s
}

// serve answers by user (plan H2, T-TRF-1): u000000 always 429 (Retry-After: 1,
// retry_after_ms 200), u000001 429 the first time a request ID arrives and 200 after that,
// u000002 503 and u000003 400 (each with the same hints as the 429s), u000004 never (it holds
// the request until the client gives up), u000005 a connection reset. Every request must be
// GET /hit with no query (HTTP-1).
func (s *trfStub) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	user, request := r.Header.Get("X-User-ID"), r.Header.Get("X-Request-ID")
	s.mu.Lock()
	seen := 0
	for _, a := range s.arrivals {
		if a.request == request {
			seen++
		}
	}
	s.arrivals = append(s.arrivals, trfArrival{user: user, request: request, method: r.Method, path: r.URL.Path,
		query: r.URL.RawQuery, at: time.Since(s.start)})
	s.mu.Unlock()
	if r.Method != http.MethodGet || r.URL.Path != "/hit" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		t.Errorf("%s arrived as %s %s?%s, want GET /hit with no query (HTTP-1)", request, r.Method, r.URL.Path, r.URL.RawQuery)
	}

	switch {
	case user == "u000000", user == "u000001" && seen == 0:
		w.Header().Set("Retry-After", "1")
		trfJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate_limited", "message": "rate limited",
			"user_id": user, "request_id": request, "retry_after_ms": 200, "decision_ns": "1791374400020710000"})
	case user == "u000001":
		trfJSON(w, http.StatusOK, map[string]any{"user_id": user, "request_id": request, "arrival_ns": "1791374400013214457",
			"existed": false, "degraded": false, "decision_ns": "1791374400013906000"})
	case user == "u000002": // with a 429's hints: only a 429 is retried, whatever the hints
		w.Header().Set("Retry-After", "1")
		trfJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "limiter_unavailable", "message": "unavailable",
			"user_id": user, "request_id": request, "retry_after_ms": 200})
	case user == "u000003": // also with a 429's hints
		w.Header().Set("Retry-After", "1")
		trfJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": "invalid request",
			"retry_after_ms": 200})
	case user == "u000004":
		select {
		case <-r.Context().Done():
		case <-s.quit:
		}
	default: // u000005, and anything unexpected
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the stub's response writer cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijacking %s's connection: %v", request, err)
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			if err := tcp.SetLinger(0); err != nil {
				t.Errorf("SO_LINGER 0: %v", err)
			}
		}
		_ = conn.Close() // with SO_LINGER 0: a reset
	}
}

func trfJSON(w http.ResponseWriter, status int, body map[string]any) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// byRequest returns the stub's arrivals for each request ID, in arrival order.
func (s *trfStub) byRequest() map[string][]trfArrival {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]trfArrival{}
	for _, a := range s.arrivals {
		out[a.request] = append(out[a.request], a)
	}
	return out
}

// genLine is the part of a gen.jsonl line T-TRF-1 reads.
type genLine struct {
	Kind       string  `json:"kind"`
	User       string  `json:"user"`
	RequestID  string  `json:"request_id"`
	Attempt    int     `json:"attempt"`
	HintSource *string `json:"hint_source"`
	HintMs     *int64  `json:"hint_ms"`
	JitterNs   *int64  `json:"jitter_ns"`
	Status     int     `json:"status"`
	Error      *string `json:"error"`
}

// readJSONLines decodes every line of the JSON-lines file at path into a new T.
func readJSONLines[T any](t *testing.T, path string) []T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []T
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("%s line %d: %v", path, n, err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// nullable prints a value that may be null.
func nullable[T any](v *T) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

// TestT_TRF_1 (HARNESS §8, TRF-5; IMPLEMENTATION_PLAN H2): bin/test/hitgen sends
// testdata/trf1.toml (one instance; six users, each with its own 2/s stream for 2 s) to a
// loopback stub that answers by user (trfStub.serve), with --http-timeout 1s. Every request is
// GET /hit with no query. Only the 429 users are retried, although the 503 and the 400 carry
// the same hints, every retry carrying its first attempt's X-User-ID and X-Request-ID; the
// server sees each retry at least the logged hint_ms after the attempt before it; u000000's
// operations get exactly 3 attempts (u000001's 2), u000002 to u000005's exactly 1; gen.jsonl's
// retry lines have kind retry, attempt 2 or 3, a hint_source, hint_ms and a jitter_ns in
// [0, 50 ms].
func TestT_TRF_1(t *testing.T) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin", "test", "hitgen")
	if err := checkBinary(bin); err != nil {
		t.Fatal(err)
	}
	scenario, err := filepath.Abs(filepath.Join("testdata", "trf1.toml"))
	if err != nil {
		t.Fatal(err)
	}
	stub := newTRFStub(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	args := []string{"--scenario", scenario, "--targets", stub.srv.Listener.Addr().String(), "--out", out, "--http-timeout", "1s"}
	c, err := startProcess(t, root, dir, bin, args, []string{}, "hitgen")
	if err != nil {
		t.Fatal(err)
	}
	if !c.exitedWithin(hitgenWait) {
		c.kill(t)
		t.Fatalf("hitgen did not exit within %v", hitgenWait)
	}
	c.checkRecord(t)
	output, err := os.ReadFile(c.Out)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := exitCode(c.waitErr); err != nil || code != 0 {
		t.Fatalf("hitgen exit code %d (%v), want 0; output:\n%s", code, err, output)
	}

	type scheduled struct {
		User      string `json:"user"`
		RequestID string `json:"request_id"`
	}
	schedule := readJSONLines[scheduled](t, filepath.Join(out, "schedule.jsonl"))
	userOf := map[string]string{}
	opsPerUser := map[string]int{}
	for _, e := range schedule {
		userOf[e.RequestID] = e.User
		opsPerUser[e.User]++
	}
	for u := range 6 {
		if user := fmt.Sprintf("u%06d", u); opsPerUser[user] == 0 {
			t.Fatalf("the schedule gives %s no operation; every user needs one (%v)", user, opsPerUser)
		}
	}

	lines := readJSONLines[genLine](t, filepath.Join(out, "gen.jsonl"))
	if len(lines) == 0 || lines[len(lines)-1].Kind != "summary" {
		t.Fatalf("gen.jsonl (%d lines) does not end with the summary", len(lines))
	}
	lines = lines[:len(lines)-1]
	linesOf := map[string][]genLine{}
	for _, l := range lines {
		linesOf[l.RequestID] = append(linesOf[l.RequestID], l)
		if l.Attempt == 1 {
			continue
		}
		if l.User != "u000000" && l.User != "u000001" {
			t.Errorf("%s (%s) was retried after status %v, want retries only for the 429 users", l.RequestID, l.User, l.Status)
		}
		if l.Kind != "retry" || (l.Attempt != 2 && l.Attempt != 3) || l.HintSource == nil || l.HintMs == nil ||
			l.JitterNs == nil || *l.JitterNs < 0 || *l.JitterNs > 50_000_000 {
			t.Errorf("%s's retry line: kind %s, attempt %d, hint_source %s, hint_ms %s, jitter_ns %s; want retry, 2 or 3, "+
				"a hint source and hint_ms, and a jitter of 0 to 50 ms", l.RequestID, l.Kind, l.Attempt, nullable(l.HintSource),
				nullable(l.HintMs), nullable(l.JitterNs))
		}
	}

	wantAttempts := map[string]int{"u000000": 3, "u000001": 2, "u000002": 1, "u000003": 1, "u000004": 1, "u000005": 1}
	wantLast := map[string]struct {
		status int
		err    string // "" for null
	}{
		"u000000": {http.StatusTooManyRequests, "rate_limited"},
		"u000001": {http.StatusOK, ""},
		"u000002": {http.StatusServiceUnavailable, "limiter_unavailable"},
		"u000003": {http.StatusBadRequest, "invalid_request"},
		"u000004": {0, "timeout"},
		"u000005": {0, "transport_error"},
	}
	arrivals := stub.byRequest()
	for _, e := range schedule {
		got, logged := arrivals[e.RequestID], linesOf[e.RequestID]
		want := wantAttempts[e.User]
		if len(got) != want || len(logged) != want {
			t.Errorf("%s (%s): %d attempts at the server and %d in gen.jsonl, want %d", e.RequestID, e.User, len(got), len(logged), want)
			continue
		}
		for _, a := range got {
			if a.user != e.User {
				t.Errorf("an attempt of %s arrived as %s, want its first attempt's user %s", e.RequestID, a.user, e.User)
			}
			if a.method != http.MethodGet || a.path != "/hit" || a.query != "" {
				t.Errorf("an attempt of %s arrived as %s %s?%s, want GET /hit with no query", e.RequestID, a.method, a.path, a.query)
			}
		}
		for n, l := range logged {
			if l.Attempt != n+1 || l.User != e.User {
				t.Errorf("%s's gen.jsonl line %d: attempt %d as %s, want attempt %d as %s", e.RequestID, n+1, l.Attempt, l.User, n+1, e.User)
			}
			if n == 0 || l.HintMs == nil {
				continue
			}
			if gap := got[n].at - got[n-1].at; gap < time.Duration(*l.HintMs)*time.Millisecond {
				t.Errorf("%s's attempt %d reached the server %v after attempt %d, want at least the logged hint_ms %d",
					e.RequestID, n+1, gap, n, *l.HintMs)
			}
		}
		last, w := logged[len(logged)-1], wantLast[e.User]
		wantErr := w.err
		if wantErr == "" {
			wantErr = "null"
		}
		if last.Status != w.status || nullable(last.Error) != wantErr {
			t.Errorf("%s (%s): last attempt status %d, error %s; want %d and %s", e.RequestID, e.User, last.Status,
				nullable(last.Error), w.status, wantErr)
		}
	}
	for id, got := range arrivals {
		if _, ok := userOf[id]; !ok {
			t.Errorf("the server got %d requests with request ID %q, which the schedule does not have", len(got), id)
		}
	}
}

// TestDesign_HitgenSIGTERM (IMPLEMENTATION_PLAN H2 step 8): SIGTERM to a running
// bin/test/hitgen, started as T-TRF-1 starts it, stops the launching: the attempts in flight
// still finish (the stub answers 200 after 400 ms, so every attempt line is a 200), the summary
// is gen.jsonl's last line with launched below planned, and hitgen exits 0 within 5 s of the
// signal, long before its 10 s schedule ends.
func TestDesign_HitgenSIGTERM(t *testing.T) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin", "test", "hitgen")
	if err := checkBinary(bin); err != nil {
		t.Fatal(err)
	}
	quit := make(chan struct{})
	started := make(chan struct{})
	var mu sync.Mutex
	arrivals := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals++
		if arrivals == 5 {
			close(started)
		}
		mu.Unlock()
		select {
		case <-time.After(400 * time.Millisecond):
		case <-quit:
		}
		trfJSON(w, http.StatusOK, map[string]any{"user_id": r.Header.Get("X-User-ID"), "request_id": r.Header.Get("X-Request-ID"),
			"arrival_ns": "1791374400013214457", "existed": false, "degraded": false, "decision_ns": "1791374400013906000"})
	}))
	srv.Config.ReadHeaderTimeout = 10 * time.Second
	srv.Start()
	t.Cleanup(func() {
		close(quit)
		srv.CloseClientConnections()
		srv.Close()
	})
	dir := t.TempDir()
	scenario := filepath.Join(dir, "sigterm.toml")
	if err := os.WriteFile(scenario, []byte(`name = "sigterm"
seed = 1
run_kind = "test"
instances = 1

[[steps]]
rate_per_s = 50
duration_s = 10

[users]
count = 5
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	args := []string{"--scenario", scenario, "--targets", srv.Listener.Addr().String(), "--out", out, "--http-timeout", "2s"}
	c, err := startProcess(t, root, dir, bin, args, []string{}, "hitgen")
	if err != nil {
		t.Fatal(err)
	}
	wait := time.NewTimer(10 * time.Second)
	defer wait.Stop()
	select {
	case <-started:
	case <-c.Exited():
		t.Fatal("hitgen exited before its fifth request")
	case <-wait.C:
		c.kill(t)
		t.Fatal("the stub got fewer than 5 requests in 10 s")
	}
	signalled := time.Now()
	c.signal(t, syscall.SIGTERM)
	if !c.exitedWithin(hitgenWait) {
		c.kill(t)
		t.Fatalf("hitgen still running %v after SIGTERM", hitgenWait)
	}
	took := time.Since(signalled)
	c.checkRecord(t)
	output, err := os.ReadFile(c.Out)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := exitCode(c.waitErr); err != nil || code != 0 {
		t.Fatalf("hitgen after SIGTERM: exit code %d (%v), want 0; output:\n%s", code, err, output)
	}
	if took > 5*time.Second {
		t.Errorf("hitgen exited %v after SIGTERM, want within 5 s: it stops launching at once", took)
	}
	type genRecord struct {
		Kind     string `json:"kind"`
		Status   int    `json:"status"`
		Planned  int    `json:"planned"`
		Launched int    `json:"launched"`
	}
	lines := readJSONLines[genRecord](t, filepath.Join(out, "gen.jsonl"))
	if len(lines) < 2 || lines[len(lines)-1].Kind != "summary" {
		t.Fatalf("gen.jsonl (%d lines) does not end with the summary after attempt lines; output:\n%s", len(lines), output)
	}
	sum := lines[len(lines)-1]
	if sum.Launched == 0 || sum.Launched >= sum.Planned {
		t.Errorf("summary launched %d of %d planned, want some but not all: launching stopped at SIGTERM", sum.Launched, sum.Planned)
	}
	for n, l := range lines[:len(lines)-1] {
		if l.Kind == "summary" || l.Status != http.StatusOK {
			t.Errorf("gen.jsonl line %d: kind %s, status %d; want an attempt answered 200: attempts in flight finish", n+1, l.Kind, l.Status)
		}
	}
}
