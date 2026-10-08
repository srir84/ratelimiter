package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/internal/testutil/golden"
)

// goldenGen is the hand-written golden gen.jsonl (DESIGN §C5; plan H2 step 6).
const goldenGen = "../../harness/runlog/testdata/golden/gen.jsonl"

// genRec is one gen.jsonl line as the tests read it: an attempt's fields, or the summary's.
type genRec struct {
	Kind          string  `json:"kind"`
	PlannedNs     int64   `json:"planned_ns"`
	SentNs        *int64  `json:"sent_ns"`
	GotConnWaitNs *int64  `json:"got_conn_wait_ns"`
	Instance      int     `json:"instance"`
	User          string  `json:"user"`
	RequestID     string  `json:"request_id"`
	DuplicateOf   *int    `json:"duplicate_of"`
	Attempt       int     `json:"attempt"`
	HintSource    *string `json:"hint_source"`
	HintMs        *int64  `json:"hint_ms"`
	JitterNs      *int64  `json:"jitter_ns"`
	Status        int     `json:"status"`
	Error         *string `json:"error"`
	Latency       *int64  `json:"latency"`
	ArrivalNs     *int64  `json:"arrival_ns"`
	DecisionNs    *int64  `json:"decision_ns"`

	Planned           int   `json:"planned"`
	Launched          int   `json:"launched"`
	Unsent            int   `json:"unsent"`
	LogDrops          int64 `json:"log_drops"`
	StoppedAfterStep  *int  `json:"stopped_after_step"`
	ExcludedStep      *int  `json:"excluded_step"`
	SkippedNoEligible int   `json:"skipped_no_eligible"`
}

// show prints a nullable value.
func show[T any](v *T) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

// decode reads gen.jsonl lines, each one JSON object.
func decode(t *testing.T, lines [][]byte) []genRec {
	t.Helper()
	out := make([]genRec, 0, len(lines))
	for n, l := range lines {
		var r genRec
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatalf("gen.jsonl line %d is not a record: %v: %s", n+1, err, l)
		}
		out = append(out, r)
	}
	return out
}

// attempts returns the attempt lines of a finished gen.jsonl, checking that the summary is
// its last line and its only one.
func attempts(t *testing.T, lines [][]byte) []genRec {
	t.Helper()
	recs := decode(t, lines)
	if len(recs) == 0 || recs[len(recs)-1].Kind != "summary" {
		t.Fatalf("gen.jsonl does not end with a summary line: %d lines", len(recs))
	}
	for n, r := range recs[:len(recs)-1] {
		if r.Kind == "summary" {
			t.Fatalf("gen.jsonl line %d is a summary before the last line", n+1)
		}
	}
	return recs[:len(recs)-1]
}

// summaryOf returns gen.jsonl's summary line.
func summaryOf(t *testing.T, lines [][]byte) genRec {
	t.Helper()
	attempts(t, lines)
	return decode(t, lines[len(lines)-1:])[0]
}

// readJSONL returns a file's lines.
func readJSONL(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		out = append(out, bytes.Clone(sc.Bytes()))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// testLogger logs into a buffer that the test prints if it fails.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	var buf syncBuffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("hitgen log:\n%s", buf.String())
		}
	})
	return slog.New(slog.NewJSONHandler(&buf, nil))
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sendAll sends entries to urls with a sender (seed 7, max_extra 2, jitter_max_ms 50, a 5 s
// client timeout with 512 connections per instance; edit, when not nil, changes that or the
// sender's caps before it runs), writes gen.jsonl through finish in a fresh directory, and
// returns its lines.
func sendAll(t *testing.T, entries []Entry, urls []string, edit func(*sender)) [][]byte {
	t.Helper()
	return sendAllCtx(t, t.Context(), entries, urls, edit)
}

// sendAllCtx is sendAll with the sender running under ctx.
func sendAllCtx(t *testing.T, ctx context.Context, entries []Entry, urls []string, edit func(*sender)) [][]byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gen.jsonl")
	gen, err := openGenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	client, tr := newClient(5*time.Second, defaultMaxConns)
	t.Cleanup(tr.CloseIdleConnections)
	logger := testLogger(t)
	cfg := sendConfig{urls: urls, client: client, timeout: 5 * time.Second, seed: 7,
		retries: config.Retries{MaxExtra: 2, JitterMaxMs: 50}, log: gen, logger: logger}
	snd := newSender(cfg, entries)
	if edit != nil {
		edit(snd)
	}
	sum := snd.run(ctx)
	if code := finish(t.Context(), logger, gen, sum, logDrainTimeout); code != exitOK {
		t.Fatalf("finish exited %d, want 0", code)
	}
	return readJSONL(t, path)
}

// stubReq is one request a stub received: its IDs, method and target, the connection it came
// on, and when it arrived and was answered, from the stub's start.
type stubReq struct {
	user, request string
	method, path  string
	query         string // the raw query: empty for /hit (HTTP-1)
	conn          net.Conn
	connSeq       int  // its place among the connection's requests, from 1
	seen          int  // how many times its request ID arrived before
	body          bool // it came with a body: a Content-Length or a Transfer-Encoding
	at, answered  time.Duration
}

// answerFunc writes the stub's response to request r, described by q.
type answerFunc func(s *stub, w http.ResponseWriter, r *http.Request, q stubReq)

// stub is a loopback HTTP server for the sender tests: answer writes each response. Every
// request must be GET /hit with no query (HTTP-1); any other fails the test.
type stub struct {
	*httptest.Server
	t      testing.TB
	start  time.Time
	answer answerFunc
	quit   chan struct{} // closed at cleanup: hanging answers return

	mu          sync.Mutex
	reqs        []stubReq
	perConn     map[net.Conn]int
	inFlight    int
	maxInFlight int
}

type connKey struct{}

func newStub(t *testing.T, answer answerFunc) *stub {
	t.Helper()
	s := &stub{t: t, start: time.Now(), answer: answer, quit: make(chan struct{}), perConn: map[net.Conn]int{}}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.Config.ReadHeaderTimeout = 10 * time.Second
	s.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, connKey{}, c)
	}
	s.Start()
	t.Cleanup(func() {
		close(s.quit)
		s.CloseClientConnections()
		s.Close()
	})
	return s
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	conn, _ := r.Context().Value(connKey{}).(net.Conn)
	id := r.Header.Get("X-Request-ID")
	s.mu.Lock()
	q := stubReq{user: r.Header.Get("X-User-ID"), request: id, method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		conn: conn, at: time.Since(s.start), body: r.ContentLength != 0 || len(r.TransferEncoding) > 0}
	for _, p := range s.reqs {
		if p.request == id {
			q.seen++
		}
	}
	s.perConn[conn]++
	q.connSeq = s.perConn[conn]
	n := len(s.reqs)
	s.reqs = append(s.reqs, q)
	s.inFlight++
	s.maxInFlight = max(s.maxInFlight, s.inFlight)
	s.mu.Unlock()
	if q.method != http.MethodGet || q.path != "/hit" || q.query != "" || r.URL.ForceQuery {
		s.t.Errorf("%s arrived as %s %s?%s, want GET /hit with no query (HTTP-1)", id, q.method, q.path, q.query)
	}

	s.answer(s, w, r, q)

	s.mu.Lock()
	s.reqs[n].answered = time.Since(s.start)
	s.inFlight--
	s.mu.Unlock()
}

// requests returns the requests that carried request ID id, in arrival order.
func (s *stub) requests(id string) []stubReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stubReq
	for _, q := range s.reqs {
		if q.request == id {
			out = append(out, q)
		}
	}
	return out
}

// all returns every request the stub received.
func (s *stub) all() []stubReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubReq(nil), s.reqs...)
}

// peak returns the most requests the stub held at once.
func (s *stub) peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFlight
}

// conns returns how many connections brought the stub requests.
func (s *stub) conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.perConn)
}

// addr is the stub's host:port.
func (s *stub) addr() string { return strings.TrimPrefix(s.URL, "http://") }

// writeJSON writes a JSON response as hitapi does (HTTP-7, HTTP-8).
func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
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

// answerOK answers 200 as hitapi does for a new row (HTTP-6): arrival_ns and decision_ns as
// decimal strings.
func answerOK(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user_id": r.Header.Get("X-User-ID"), "request_id": r.Header.Get("X-Request-ID"),
		"arrival_ns": "1791374400013214457", "existed": false, "degraded": false, "decision_ns": "1791374400013906000"})
}

// answerLimited answers 429 as hitapi does (ERR-4, HTTP-6, HTTP-9): Retry-After, and
// retry_after_ms and decision_ns in the body.
func answerLimited(w http.ResponseWriter, r *http.Request, retryAfter string, retryAfterMs int64) {
	w.Header().Set("Retry-After", retryAfter)
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate_limited", "message": "rate limited",
		"user_id": r.Header.Get("X-User-ID"), "request_id": r.Header.Get("X-Request-ID"),
		"retry_after_ms": retryAfterMs, "decision_ns": "1791374400020710000"})
}

// answerReset resets the request's connection without replying: the request was read, then
// the socket is closed with SO_LINGER 0, which sends a RST.
func answerReset(t *testing.T, w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("the stub's response writer cannot hijack")
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijacking the connection: %v", err)
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetLinger(0); err != nil {
			t.Errorf("SO_LINGER 0: %v", err)
		}
	}
	_ = conn.Close()
}

// hang holds the request until its client gives up or the stub closes.
func (s *stub) hang(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-s.quit:
	}
}

// scenarioFile writes a scenario file (header, the seed, then body) and returns its path.
func scenarioFile(t *testing.T, seed uint64, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.toml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%sseed = %d\n%s", header, seed, body)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hitgenRun is one run of hitgen's run in this process.
type hitgenRun struct {
	code     int
	schedule []scheduleLine
	gen      [][]byte
	log      string // what it logged
}

// runHitgen runs hitgen's run under ctx with --scenario path, --targets targets, --out a fresh
// directory and extra flags, and reads what it wrote.
func runHitgen(t *testing.T, ctx context.Context, path, targets string, extra ...string) hitgenRun {
	t.Helper()
	return runHitgenIn(t, ctx, filepath.Join(t.TempDir(), "out"), path, targets, extra...)
}

// runHitgenIn is runHitgen with --out out.
func runHitgenIn(t *testing.T, ctx context.Context, out, path, targets string, extra ...string) hitgenRun {
	t.Helper()
	var stderr syncBuffer
	args := append([]string{"--scenario", path, "--targets", targets, "--out", out}, extra...)
	res := hitgenRun{code: run(ctx, args, &stderr)}
	res.log = stderr.String()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("hitgen %s:\n%s", strings.Join(args, " "), res.log)
		}
	})
	for _, l := range readJSONL(t, filepath.Join(out, "schedule.jsonl")) {
		var sl scheduleLine
		if err := json.Unmarshal(l, &sl); err != nil {
			t.Fatalf("schedule.jsonl: %v: %s", err, l)
		}
		res.schedule = append(res.schedule, sl)
	}
	res.gen = readJSONL(t, filepath.Join(out, "gen.jsonl"))
	return res
}

// written counts the first attempts that were written (sent_ns set).
func written(recs []genRec) int {
	n := 0
	for _, r := range recs {
		if r.Attempt == 1 && r.SentNs != nil {
			n++
		}
	}
	return n
}

// TestSender_OpenLoop (plan H2 step 4, TRF-3): with a server that takes 500 ms per request,
// 100/s for 3 s is still sent on schedule: every planned entry is launched, and every
// sent_ns − planned_ns is at most 50 ms (a closed loop would lag by seconds). schedule.jsonl
// is in --out before the first request arrives (plan H2 step 3), and the connections are kept
// alive: the server sees at most 10 more of them than the most requests it held at once.
func TestSender_OpenLoop(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	var first sync.Once
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) {
		first.Do(func() {
			if fi, err := os.Stat(filepath.Join(out, "schedule.jsonl")); err != nil || fi.Size() == 0 {
				t.Errorf("the first request arrived before schedule.jsonl was written: %v", err)
			}
		})
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
		answerOK(w, r)
	})
	res := runHitgenIn(t, t.Context(), out, scenarioFile(t, 61, `
instances = 1
[[steps]]
rate_per_s = 100
duration_s = 3
[users]
count = 10
`), st.addr())
	if res.code != exitOK {
		t.Fatalf("exit code %d, want 0", res.code)
	}
	// 300 ± 70 entries (4σ, σ = √300).
	if n := len(res.schedule); n < 230 || n > 370 {
		t.Fatalf("%d entries planned, want 300 ± 70", n)
	}
	sum := summaryOf(t, res.gen)
	if sum.Planned != len(res.schedule) || sum.Launched != sum.Planned || sum.Unsent != 0 {
		t.Errorf("summary planned %d, launched %d, unsent %d; want %d planned, all launched, none unsent",
			sum.Planned, sum.Launched, sum.Unsent, len(res.schedule))
	}
	recs := attempts(t, res.gen)
	if len(recs) != len(res.schedule) {
		t.Errorf("%d attempt lines, want one per entry (%d)", len(recs), len(res.schedule))
	}
	for _, r := range recs {
		if r.Status != http.StatusOK || r.SentNs == nil {
			t.Errorf("%s: status %d, sent_ns %s; want 200 and a sent time", r.RequestID, r.Status, show(r.SentNs))
			continue
		}
		if lag := *r.SentNs - r.PlannedNs; lag < 0 || lag > 50_000_000 {
			t.Errorf("%s: sent %d ns after its planned time, want 0 to 50 ms", r.RequestID, lag)
		}
	}
	if conns, peak := st.conns(), st.peak(); conns > peak+10 {
		t.Errorf("%d connections brought requests, with at most %d requests in flight: want at most %d (keep-alive, "+
			"%d idle connections kept per instance)", conns, peak, peak+10, defaultMaxConns)
	}
}

// TestSender_SummaryAndUnsent (plan H2 steps 6 and 8, PRF-1): SIGTERM 1 s into a 3 s schedule
// stops the launching; the attempts in flight then (the server takes 400 ms) still finish, so
// every attempt line is a 200; the summary is gen.jsonl's last line, with every planned entry
// counted, unsent = planned − written > 0 (written: first attempts with a sent_ns), no log
// drops and stopped_after_step null; hitgen exits 0.
func TestSender_SummaryAndUnsent(t *testing.T) {
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) {
		select {
		case <-time.After(400 * time.Millisecond):
		case <-r.Context().Done():
		}
		answerOK(w, r)
	})
	path := scenarioFile(t, 67, `
instances = 1
[[steps]]
rate_per_s = 100
duration_s = 3
[users]
count = 10
`)
	// SIGTERM reaches run's context as it does in main (signal.NotifyContext). The handler stays
	// registered until a signal sent has been handled, so the test process never dies of it.
	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	sent := make(chan struct{})
	start := time.Now()
	term := time.AfterFunc(time.Second, func() {
		defer close(sent)
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("sending SIGTERM: %v", err)
		}
	})
	res := runHitgen(t, ctx, path, st.addr())
	took := time.Since(start)
	handled := ctx.Err() != nil
	if !term.Stop() { // the signal went out: wait until its handler has it
		<-sent
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			t.Error("the SIGTERM sent was not handled within 10 s")
		}
	}
	stop()
	if !handled {
		t.Fatalf("hitgen returned after %v, before the SIGTERM", took)
	}
	if res.code != exitOK {
		t.Errorf("exit code %d after SIGTERM, want 0", res.code)
	}
	if took > 2500*time.Millisecond {
		t.Errorf("hitgen took %v after a SIGTERM at 1 s, want it to stop launching then", took)
	}
	sum := summaryOf(t, res.gen)
	recs := attempts(t, res.gen)
	t.Logf("after %v: planned %d, launched %d, unsent %d", took, sum.Planned, sum.Launched, sum.Unsent)
	if sum.Planned != len(res.schedule) {
		t.Errorf("summary planned %d, want every planned entry (%d)", sum.Planned, len(res.schedule))
	}
	if w := written(recs); sum.Unsent != sum.Planned-w || sum.Unsent <= 0 {
		t.Errorf("summary unsent %d, planned %d, written %d: want unsent = planned − written > 0", sum.Unsent, sum.Planned, w)
	}
	if sum.LogDrops != 0 || sum.StoppedAfterStep != nil || sum.ExcludedStep != nil {
		t.Errorf("summary log_drops %d, stopped_after_step %s, excluded_step %s; want 0, null, null",
			sum.LogDrops, show(sum.StoppedAfterStep), show(sum.ExcludedStep))
	}
	if sum.Launched != len(recs) || sum.Launched == 0 || sum.Launched >= sum.Planned {
		t.Errorf("summary launched %d with %d attempt lines and %d planned, want one line per launch, some but not all launched",
			sum.Launched, len(recs), sum.Planned)
	}
	notOK := 0
	for _, r := range recs {
		if r.Status != http.StatusOK {
			if notOK == 0 {
				t.Errorf("%s: status %d, error %s; want 200: an attempt in flight at SIGTERM finishes", r.RequestID, r.Status, show(r.Error))
			}
			notOK++
		}
	}
	if notOK > 1 {
		t.Errorf("%d of %d attempt lines are not 200", notOK, len(recs))
	}
}

// TestSender_StopEarlyGeneratorLimited (plan H2 step 7, §B9, PRF-8): with 2 connections to a
// server that takes 100 ms, two steps of 200/s for 3 s lag far beyond 5 ms. Step 0 is judged
// once its first attempts have finished, at most the 1 s client timeout after it ends, during
// step 1: generator-limited, so the launching stops then. The summary has
// stopped_after_step 0 and excluded_step 1, counts as planned exactly the entries planned
// before the stop (stop_ns in the log; some of step 1's), and nothing planned after them was
// sent. A planned entry not launched came due in the last moments before the stop, before the
// dispatcher reached it: there is no generator drop here.
func TestSender_StopEarlyGeneratorLimited(t *testing.T) {
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
		}
		answerOK(w, r)
	})
	res := runHitgen(t, t.Context(), scenarioFile(t, 71, `
instances = 1
stop_early = true
[[steps]]
rate_per_s = 200
duration_s = 3
[[steps]]
rate_per_s = 200
duration_s = 3
[users]
count = 10
`), st.addr(), "--max-conns", "2", "--http-timeout", "1s")
	if res.code != exitOK {
		t.Fatalf("exit code %d, want 0", res.code)
	}
	sum := summaryOf(t, res.gen)
	if sum.StoppedAfterStep == nil || *sum.StoppedAfterStep != 0 || sum.ExcludedStep == nil || *sum.ExcludedStep != 1 {
		t.Fatalf("summary stopped_after_step %s, excluded_step %s; want 0 and 1", show(sum.StoppedAfterStep), show(sum.ExcludedStep))
	}
	t.Logf("summary: planned %d of %d, launched %d, unsent %d", sum.Planned, len(res.schedule), sum.Launched, sum.Unsent)
	step0 := 0
	for _, e := range res.schedule {
		if e.PlannedNs < 3_000_000_000 {
			step0++
		}
	}
	if sum.Planned <= step0 || sum.Planned >= len(res.schedule) {
		t.Fatalf("summary planned %d, want more than step 0's %d entries and fewer than all %d: stopped during step 1",
			sum.Planned, step0, len(res.schedule))
	}
	// The last entry counted was planned before step 0's last first attempt could have ended
	// (3 s + the 1 s timeout) plus 1 s of slack.
	if last := res.schedule[sum.Planned-1].PlannedNs; last >= 5_000_000_000 {
		t.Errorf("the last entry counted was planned at %d ns, want the stop soon after step 0 was judged (before 5 s)", last)
	}
	// Step 0 is judged when its last first attempt ends: sent_ns + latency with a response,
	// else at most its planned time + the 1 s timeout (it starts at its launch, within a tick of
	// that). The stop follows at once: within 50 ms of that end.
	var step0End int64
	for _, r := range attempts(t, res.gen) {
		if r.Attempt != 1 || r.PlannedNs >= 3_000_000_000 {
			continue
		}
		end := r.PlannedNs + 1_000_000_000
		if r.Status != 0 && r.SentNs != nil && r.Latency != nil {
			end = *r.SentNs + *r.Latency
		}
		step0End = max(step0End, end)
	}
	if last := res.schedule[sum.Planned-1].PlannedNs; last > step0End+50_000_000 {
		t.Errorf("the last entry counted was planned at %d ns, %d ms after step 0's last first attempt ended (%d ns): "+
			"want the stop within 50 ms of step 0 being judged", last, (last-step0End)/1_000_000, step0End)
	}
	stopNs := loggedStopNs(t, res.log)
	before := 0
	for _, e := range res.schedule {
		if e.PlannedNs < stopNs {
			before++
		}
	}
	if sum.Planned != before {
		t.Errorf("summary planned %d, want the %d entries planned before the stop at %d ns", sum.Planned, before, stopNs)
	}
	recs := attempts(t, res.gen)
	launched := map[string]bool{}
	for _, r := range recs {
		if r.Attempt == 1 {
			launched[r.RequestID] = true
		}
	}
	if len(launched) != sum.Launched {
		t.Errorf("summary launched %d, with %d first attempt lines: want one per launch", sum.Launched, len(launched))
	}
	// Not launched but planned: due within the dispatcher's 50 ms lag bound (TestSender_OpenLoop)
	// before the stop, never a drop.
	for _, e := range res.schedule[:sum.Planned] {
		if !launched[e.RequestID] && e.PlannedNs < stopNs-50_000_000 {
			t.Errorf("%s, planned at %d ns, more than 50 ms before the stop at %d ns, was not launched: want no generator drop",
				e.RequestID, e.PlannedNs, stopNs)
		}
	}
	if w := written(recs); sum.Unsent != sum.Planned-w {
		t.Errorf("summary unsent %d, want planned − written = %d − %d", sum.Unsent, sum.Planned, w)
	}
	for _, r := range recs {
		i, err := strconv.Atoi(strings.TrimPrefix(r.RequestID, "r"))
		if err != nil || i >= sum.Planned {
			t.Errorf("an attempt of %s, planned at %d ns: entry %d is after the stop (planned %d)", r.RequestID, r.PlannedNs, i, sum.Planned)
		}
	}
	for _, q := range st.all() {
		if i, err := strconv.Atoi(strings.TrimPrefix(q.request, "r")); err != nil || i >= sum.Planned {
			t.Errorf("the server got %s, an entry after the stop (planned %d)", q.request, sum.Planned)
		}
	}
}

// TestSender_InflightCap (plan H2 step 4, PRF-1): with maxInFlight lowered to 10 and a server
// that never answers, 10 entries are in flight at most: the server holds 10 requests at once,
// 10 entries are launched, every later entry due is a generator drop with no line, and
// unsent = planned − written.
func TestSender_InflightCap(t *testing.T) {
	old := maxInFlight
	maxInFlight = 10
	t.Cleanup(func() { maxInFlight = old })
	st := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request, _ stubReq) {
		s.hang(r)
		answerOK(w, r)
	})
	res := runHitgen(t, t.Context(), scenarioFile(t, 73, `
instances = 1
[[steps]]
rate_per_s = 100
duration_s = 1
[users]
count = 10
`), st.addr(), "--http-timeout", "3s")
	if res.code != exitOK {
		t.Fatalf("exit code %d, want 0", res.code)
	}
	if n := len(res.schedule); n < 60 {
		t.Fatalf("%d entries planned, want 100 ± 40 (4σ)", n)
	}
	if p := st.peak(); p != 10 {
		t.Errorf("the server held at most %d requests at once, want 10 (the cap)", p)
	}
	sum := summaryOf(t, res.gen)
	recs := attempts(t, res.gen)
	if sum.Planned != len(res.schedule) || sum.Launched != 10 || len(recs) != 10 {
		t.Errorf("summary planned %d, launched %d, with %d attempt lines; want %d planned, 10 launched and 10 lines",
			sum.Planned, sum.Launched, len(recs), len(res.schedule))
	}
	if w := written(recs); sum.Unsent != sum.Planned-w || w != 10 {
		t.Errorf("summary unsent %d with %d planned and %d written, want unsent = planned − written and 10 written",
			sum.Unsent, sum.Planned, w)
	}
	for _, r := range recs {
		if r.Status != 0 || r.Error == nil || *r.Error != "timeout" {
			t.Errorf("%s: status %d, error %s; want 0 and timeout", r.RequestID, r.Status, show(r.Error))
		}
	}

	// The cap counts attempts in flight, not attempts made: with the same cap, a server that
	// answers at once gets every entry of the same schedule, none unsent.
	fast := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) { answerOK(w, r) })
	res = runHitgen(t, t.Context(), scenarioFile(t, 73, `
instances = 1
[[steps]]
rate_per_s = 100
duration_s = 1
[users]
count = 10
`), fast.addr(), "--http-timeout", "3s")
	if res.code != exitOK {
		t.Fatalf("exit code %d with a fast server, want 0", res.code)
	}
	sum = summaryOf(t, res.gen)
	if sum.Planned != len(res.schedule) || sum.Launched != sum.Planned || sum.Unsent != 0 {
		t.Errorf("with a fast server: summary planned %d, launched %d, unsent %d; want %d planned, all launched, none unsent",
			sum.Planned, sum.Launched, sum.Unsent, len(res.schedule))
	}
}

// TestSender_NoHiddenResend (plan H2 step 4, TRF-5, AGENTS.md §6): the server answers the first
// request on a connection, then reads the next request on that same connection and resets it.
// hitgen's request arrives exactly once and is recorded with status 0 and transport_error; its
// requests carry no body on the wire, no Content-Length and no Transfer-Encoding (HTTP-1). A
// plain GET (no body) on the same client arrives twice, a resend net/http makes on its own
// with no error to the caller, which is what the empty body prevents.
func TestSender_NoHiddenResend(t *testing.T) {
	resetSecond := func(_ *stub, w http.ResponseWriter, r *http.Request, q stubReq) {
		if q.connSeq > 1 {
			answerReset(t, w)
			return
		}
		answerOK(w, r)
	}
	st := newStub(t, resetSecond)
	entries := []Entry{
		{I: 0, PlannedNs: 0, User: "u000000", RequestID: "r000000000", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 1, PlannedNs: 300_000_000, User: "u000001", RequestID: "r000000001", Instance: 1, Kind: "initial", DuplicateOf: -1},
	}
	recs := attempts(t, sendAll(t, entries, []string{st.URL + "/hit"}, nil))
	first, second := st.requests("r000000000"), st.requests("r000000001")
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("r000000000 arrived %d times and r000000001 %d times, want each exactly once", len(first), len(second))
	}
	if second[0].conn != first[0].conn {
		t.Fatal("r000000001 came on a new connection, want the first request's connection reused (else nothing is resent anyway)")
	}
	if first[0].body || second[0].body {
		t.Error("a /hit request came with a body (Content-Length or Transfer-Encoding), want none (HTTP-1)")
	}
	byID := map[string]genRec{}
	for _, r := range recs {
		byID[r.RequestID] = r
	}
	if len(recs) != 2 {
		t.Errorf("%d attempt lines, want 2", len(recs))
	}
	if r := byID["r000000001"]; r.Status != 0 || r.Error == nil || *r.Error != "transport_error" || r.SentNs == nil {
		t.Errorf("r000000001: status %d, error %s, sent_ns %s; want 0, transport_error and a sent time", r.Status, show(r.Error), show(r.SentNs))
	}

	// The control: plain GETs on the same kind of client.
	plain := newStub(t, resetSecond)
	client, tr := newClient(5*time.Second, defaultMaxConns)
	t.Cleanup(tr.CloseIdleConnections)
	for _, id := range []string{"r000000000", "r000000001"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, plain.URL+"/hit", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-User-ID", "u000000")
		req.Header.Set("X-Request-ID", id)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("plain GET %s: %v, want no error: net/http resends it", id, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("plain GET %s: status %d, want 200", id, resp.StatusCode)
		}
	}
	if n := len(plain.requests("r000000001")); n != 2 {
		t.Errorf("the plain GET of r000000001 arrived %d times, want 2: the stub must provoke net/http's resend", n)
	}
}

// TestGenLog_Golden (plan H2 step 6, DESIGN §C5): a 200, a 429 (Retry-After: 1, and in the body
// rate_limited, retry_after_ms 200 and decision_ns, HTTP-6) whose retry is answered 200, and a
// third request whose connection is reset give 4 attempt lines and the summary, each matching
// the hand-written golden gen.jsonl on kind, status, error and hint_source.
func TestGenLog_Golden(t *testing.T) {
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, q stubReq) {
		switch q.request {
		case "r000000001":
			if q.seen == 0 {
				answerLimited(w, r, "1", 200)
				return
			}
			answerOK(w, r)
		case "r000000002":
			answerReset(t, w)
		default:
			answerOK(w, r)
		}
	})
	entries := []Entry{
		{I: 0, PlannedNs: 12_000_000, User: "u000003", RequestID: "r000000000", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 1, PlannedNs: 19_000_000, User: "u000000", RequestID: "r000000001", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 2, PlannedNs: 27_000_000, User: "u000001", RequestID: "r000000002", Instance: 1, Kind: "initial", DuplicateOf: -1},
	}
	lines := sendAll(t, entries, []string{st.URL + "/hit"}, nil)
	if len(lines) != 5 {
		t.Fatalf("%d gen.jsonl lines, want 4 attempts and the summary", len(lines))
	}
	golden.Match(t, lines, goldenGen, "kind", "status", "error", "hint_source")
	sum := summaryOf(t, lines)
	if sum.Planned != 3 || sum.Launched != 3 || sum.Unsent != 0 || sum.LogDrops != 0 {
		t.Errorf("summary planned %d, launched %d, unsent %d, log_drops %d; want 3, 3, 0, 0", sum.Planned, sum.Launched, sum.Unsent, sum.LogDrops)
	}
	for _, r := range attempts(t, lines) {
		if r.Kind == "retry" && (r.HintMs == nil || *r.HintMs != 1000 || r.Attempt != 2) {
			t.Errorf("the retry: attempt %d, hint_ms %s; want attempt 2 and 1000 ms from Retry-After", r.Attempt, show(r.HintMs))
		}
	}
}

// maxWaiting returns the most request IDs that were waiting for a retry at once at the stub:
// each waits from an attempt's answer to the arrival of its next attempt.
func maxWaiting(s *stub) int {
	type event struct {
		at    time.Duration
		delta int
	}
	var events []event
	byID := map[string][]stubReq{}
	var ids []string
	for _, q := range s.all() {
		if _, ok := byID[q.request]; !ok {
			ids = append(ids, q.request)
		}
		byID[q.request] = append(byID[q.request], q)
	}
	for _, id := range ids {
		reqs := byID[id]
		for n := 1; n < len(reqs); n++ {
			events = append(events, event{reqs[n-1].answered, 1}, event{reqs[n].at, -1})
		}
	}
	slices.SortFunc(events, func(a, b event) int {
		if a.at != b.at {
			return cmp.Compare(a.at, b.at)
		}
		return cmp.Compare(a.delta, b.delta) // an end before a start at the same instant
	})
	most, now := 0, 0
	for _, e := range events {
		now += e.delta
		most = max(most, now)
	}
	return most
}

// TestSender_RetryWaitHoldsNoSlot (plan H2 steps 4 and 5, TRF-3, TRF-5): a retry waiting out its
// 429 hint holds no in-flight slot. With maxInFlight lowered to 10 and a server that answers
// every request at once with 429 (Retry-After: 1, retry_after_ms 200, so a 1 s hint), 100/s for
// 1 s still launches every planned entry while far more than 10 entries wait for their retries
// at once; every entry makes its 3 attempts (max_extra 2), and none is unsent.
func TestSender_RetryWaitHoldsNoSlot(t *testing.T) {
	old := maxInFlight
	maxInFlight = 10
	t.Cleanup(func() { maxInFlight = old })
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) { answerLimited(w, r, "1", 200) })
	res := runHitgen(t, t.Context(), scenarioFile(t, 79, `
instances = 1
[[steps]]
rate_per_s = 100
duration_s = 1
[users]
count = 10
`), st.addr())
	if res.code != exitOK {
		t.Fatalf("exit code %d, want 0", res.code)
	}
	if n := len(res.schedule); n < 60 {
		t.Fatalf("%d entries planned, want 100 ± 40 (4σ)", n)
	}
	sum := summaryOf(t, res.gen)
	if sum.Planned != len(res.schedule) || sum.Launched != sum.Planned || sum.Unsent != 0 {
		t.Errorf("summary planned %d, launched %d, unsent %d; want %d planned, all launched, none unsent: waiting retries "+
			"must not hold back initial arrivals", sum.Planned, sum.Launched, sum.Unsent, len(res.schedule))
	}
	per := map[string]int{}
	for _, r := range attempts(t, res.gen) {
		per[r.RequestID]++
	}
	short := 0
	for _, e := range res.schedule {
		if per[e.RequestID] != 3 {
			if short == 0 {
				t.Errorf("%s: %d attempt lines, want 3 (max_extra 2, every attempt answered 429)", e.RequestID, per[e.RequestID])
			}
			short++
		}
	}
	if short > 1 {
		t.Errorf("%d of %d entries without 3 attempt lines", short, len(res.schedule))
	}
	w := maxWaiting(st)
	t.Logf("%d entries; at most %d waited for a retry at once", len(res.schedule), w)
	if w <= 10 {
		t.Errorf("at most %d entries waited for a retry at once, want more than the 10 in-flight slots", w)
	}
}

// TestSender_StopEarlyExcludedStep (plan H2 step 7): step 0 is judged only once its first
// attempts have all finished, up to the client timeout after it ends, so the stop can land
// after a short step 1, in step 2; excluded_step then names step 2, the step whose interval
// holds the stop, not the fully run step 1, even when step 2 has rate 0 and so no entry. With
// 1 connection to a server that takes 50 ms, steps of 200/s × 2 s and 200/s × 0.5 s, then a
// step 2 of 4 s, and a 1 s client timeout, step 0's last first attempts end about 3 s in, inside
// step 2 ([2.5 s, 6.5 s)). Every entry of steps 0 and 1 counts as planned; the last one counted
// is in step 2 when it sends at 200/s, in step 1 when it has rate 0 (a step 3 then sends after
// it, so that the stop comes while launching goes on).
func TestSender_StopEarlyExcludedStep(t *testing.T) {
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-r.Context().Done():
		}
		answerOK(w, r)
	})
	for _, c := range []struct {
		name     string
		after    string // the steps after steps 0 and 1
		lastStep int    // the step of the last entry counted as planned
	}{
		{"step 2 at 200/s", `
[[steps]]
rate_per_s = 200
duration_s = 4
`, 2},
		{"step 2 at rate 0", `
[[steps]]
rate_per_s = 0
duration_s = 4
[[steps]]
rate_per_s = 200
duration_s = 1
`, 1},
	} {
		res := runHitgen(t, t.Context(), scenarioFile(t, 83, `
instances = 1
stop_early = true
[[steps]]
rate_per_s = 200
duration_s = 2
[[steps]]
rate_per_s = 200
duration_s = 0.5
`+c.after+`
[users]
count = 10
`), st.addr(), "--max-conns", "1", "--http-timeout", "1s")
		if res.code != exitOK {
			t.Fatalf("%s: exit code %d, want 0", c.name, res.code)
		}
		sum := summaryOf(t, res.gen)
		t.Logf("%s: summary planned %d of %d, launched %d, unsent %d", c.name, sum.Planned, len(res.schedule), sum.Launched, sum.Unsent)
		if sum.StoppedAfterStep == nil || *sum.StoppedAfterStep != 0 || sum.ExcludedStep == nil || *sum.ExcludedStep != 2 {
			t.Errorf("%s: summary stopped_after_step %s, excluded_step %s; want 0 and 2 (the stop lands in step 2)", c.name,
				show(sum.StoppedAfterStep), show(sum.ExcludedStep))
			continue
		}
		if sum.Planned < 1 || sum.Planned >= len(res.schedule) {
			t.Errorf("%s: summary planned %d of %d entries, want some but not all", c.name, sum.Planned, len(res.schedule))
			continue
		}
		if last := res.schedule[sum.Planned-1]; last.Step != c.lastStep {
			t.Errorf("%s: the last entry counted, %s planned at %d ns, is in step %d, want step %d", c.name, last.RequestID,
				last.PlannedNs, last.Step, c.lastStep)
		}
		for _, e := range res.schedule {
			if e.Step <= 1 && e.I >= sum.Planned {
				t.Errorf("%s: %s of step %d is not counted as planned (planned %d), want every entry of steps 0 and 1", c.name,
					e.RequestID, e.Step, sum.Planned)
				break
			}
		}
	}
}

// answerLimitedMs answers 429 with retry_after_ms ms in the body and no Retry-After header: a
// hint shorter than hitapi's whole seconds, for tests that should not wait them out.
func answerLimitedMs(w http.ResponseWriter, r *http.Request, ms int64) {
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate_limited", "message": "rate limited",
		"user_id": r.Header.Get("X-User-ID"), "request_id": r.Header.Get("X-Request-ID"), "retry_after_ms": ms})
}

// retryDrops returns the sender's retry drops once it has run.
func retryDrops(s *sender) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retryDrops
}

// TestSender_RetryDrops (plan H2 step 5; AGENTS.md §1 Queues): a retry that cannot be made is a
// counted drop, and its entry's attempts end with the 429. (a) With the sender's cap on waiting
// retries lowered to 1, two entries limited at once (retry_after_ms 100) get one retry between
// them. (b) With maxInFlight lowered to 1 and no jitter, a retry due 200 ms after its 429, while
// a later entry's attempt holds the one slot for 500 ms, is dropped.
func TestSender_RetryDrops(t *testing.T) {
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, q stubReq) {
		if q.seen == 0 {
			answerLimitedMs(w, r, 100)
			return
		}
		answerOK(w, r)
	})
	entries := []Entry{
		{I: 0, PlannedNs: 10_000_000, User: "u000000", RequestID: "r000000000", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 1, PlannedNs: 10_000_000, User: "u000001", RequestID: "r000000001", Instance: 1, Kind: "initial", DuplicateOf: -1},
	}
	var snd *sender
	recs := attempts(t, sendAll(t, entries, []string{st.URL + "/hit"}, func(s *sender) { s.pendingCap, snd = 1, s }))
	if len(recs) != 3 || len(st.all()) != 3 {
		t.Errorf("waiting cap 1: %d attempt lines and %d requests, want 3 and 3: one entry retried, the other's retry dropped",
			len(recs), len(st.all()))
	}
	if n := retryDrops(snd); n != 1 {
		t.Errorf("waiting cap 1: %d retry drops counted, want 1", n)
	}

	old := maxInFlight
	maxInFlight = 1
	t.Cleanup(func() { maxInFlight = old })
	st = newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, q stubReq) {
		if q.request == "r000000000" {
			answerLimitedMs(w, r, 200)
			return
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
		answerOK(w, r)
	})
	entries[1].PlannedNs = 80_000_000
	recs = attempts(t, sendAll(t, entries, []string{st.URL + "/hit"}, func(s *sender) { s.retries.JitterMaxMs, snd = 0, s }))
	if len(recs) != 2 || len(st.requests("r000000000")) != 1 {
		t.Errorf("one slot: %d attempt lines, r000000000 arrived %d times; want 2 lines and 1 arrival: its retry fell due "+
			"while r000000001 held the slot", len(recs), len(st.requests("r000000000")))
	}
	if n := retryDrops(snd); n != 1 {
		t.Errorf("one slot: %d retry drops counted, want 1", n)
	}
}

// spanTransport wraps a RoundTripper and records, for each X-Request-ID, the span of its last
// exchange: from the call to the close of the response body.
type spanTransport struct {
	inner http.RoundTripper
	mu    sync.Mutex
	spans map[string]time.Duration
}

func (s *spanTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	id := r.Header.Get("X-Request-ID")
	resp, err := s.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &spanBody{ReadCloser: resp.Body, closed: func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.spans[id] = time.Since(start)
	}}
	return resp, nil
}

func (s *spanTransport) span(id string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.spans[id]
	return d, ok
}

// spanBody calls closed when the body is closed.
type spanBody struct {
	io.ReadCloser
	closed func()
}

func (b *spanBody) Close() error {
	err := b.ReadCloser.Close()
	b.closed()
	return err
}

// TestRecord_AttemptTimes (plan H2 step 4; DESIGN §B4 "Sending"; PRF-8): latency runs from
// WroteRequest to the end of the response body, and got_conn_wait_ns from GetConn to GotConn.
// With one connection per instance, two entries due at once and a server that holds each
// request 300 ms, the one served second waits for the connection through the first one's
// service. Each latency is at least the 300 ms and at most its exchange's span (from the
// client's call to the body's close, seen by a wrapping RoundTripper) less its connection wait,
// which comes before the write; 0 ≤ got_conn_wait_ns ≤ sent_ns − planned_ns; and the second
// one's wait is at least the first one's service time at the server, less the 50 ms the
// dispatcher may lag (TestSender_OpenLoop's bound).
func TestRecord_AttemptTimes(t *testing.T) {
	const service = 300 * time.Millisecond
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) {
		select {
		case <-time.After(service):
		case <-r.Context().Done():
		}
		answerOK(w, r)
	})
	entries := []Entry{
		{I: 0, PlannedNs: 100_000_000, User: "u000000", RequestID: "r000000000", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 1, PlannedNs: 100_000_000, User: "u000001", RequestID: "r000000001", Instance: 1, Kind: "initial", DuplicateOf: -1},
	}
	spans := &spanTransport{spans: map[string]time.Duration{}}
	recs := attempts(t, sendAll(t, entries, []string{st.URL + "/hit"}, func(s *sender) {
		client, tr := newClient(5*time.Second, 1)
		t.Cleanup(tr.CloseIdleConnections)
		spans.inner = tr
		client.Transport = spans
		s.client = client
	}))
	reqs := st.all()
	if len(recs) != 2 || len(reqs) != 2 {
		t.Fatalf("%d attempt lines and %d requests, want 2 and 2", len(recs), len(reqs))
	}
	byID := map[string]genRec{}
	for _, r := range recs {
		byID[r.RequestID] = r
		if r.Status != http.StatusOK || r.SentNs == nil || r.Latency == nil || r.GotConnWaitNs == nil {
			t.Fatalf("%s: status %d, sent_ns %s, latency %s, got_conn_wait_ns %s; want 200 and all three", r.RequestID,
				r.Status, show(r.SentNs), show(r.Latency), show(r.GotConnWaitNs))
		}
		lat, wait := *r.Latency, *r.GotConnWaitNs
		span, ok := spans.span(r.RequestID)
		if !ok {
			t.Fatalf("%s: no exchange seen by the wrapping RoundTripper", r.RequestID)
		}
		if lat < service.Nanoseconds() || lat > span.Nanoseconds()-wait {
			t.Errorf("%s: latency %d ns, want from the server's %v to its span %v less its connection wait %d ns", r.RequestID,
				lat, service, span, wait)
		}
		if wait < 0 || wait > *r.SentNs-r.PlannedNs {
			t.Errorf("%s: got_conn_wait_ns %d, want 0 to sent_ns − planned_ns = %d", r.RequestID, wait, *r.SentNs-r.PlannedNs)
		}
	}
	served := reqs[0].answered - reqs[0].at
	if second := byID[reqs[1].request]; *second.GotConnWaitNs < (served - 50*time.Millisecond).Nanoseconds() {
		t.Errorf("%s, served second, waited %d ns for the connection, want at least %s's service time %v less 50 ms",
			second.RequestID, *second.GotConnWaitNs, reqs[0].request, served)
	}
}

// TestRecord_KindsAndInstances (plan H2 step 6; DESIGN §B4 "Record"; TRF-4, TRF-6, TRF-10): each
// attempt line carries its entry's user, request ID, instance and planned_ns, its retries'
// lines included; a duplicate's first attempt has kind duplicate and duplicate_of naming the
// entry it repeats, and its retry kind retry with the same duplicate_of. Three instances;
// entry 3 duplicates entry 0 (both instance 1); that instance answers the first two arrivals
// of r000000000 with 429 (retry_after_ms 100) and everything else 200.
func TestRecord_KindsAndInstances(t *testing.T) {
	answer := func(_ *stub, w http.ResponseWriter, r *http.Request, q stubReq) {
		if q.request == "r000000000" && q.seen < 2 {
			answerLimitedMs(w, r, 100)
			return
		}
		answerOK(w, r)
	}
	stubs := []*stub{newStub(t, answer), newStub(t, answer), newStub(t, answer)}
	entries := []Entry{
		{I: 0, PlannedNs: 50_000_000, User: "u000000", RequestID: "r000000000", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 1, PlannedNs: 60_000_000, User: "u000001", RequestID: "r000000001", Instance: 2, Kind: "initial", DuplicateOf: -1},
		{I: 2, PlannedNs: 70_000_000, User: "u000002", RequestID: "r000000002", Instance: 3, Kind: "initial", DuplicateOf: -1},
		{I: 3, PlannedNs: 80_000_000, User: "u000000", RequestID: "r000000000", Instance: 1, Kind: "duplicate", DuplicateOf: 0},
	}
	recs := attempts(t, sendAll(t, entries, []string{stubs[0].URL + "/hit", stubs[1].URL + "/hit", stubs[2].URL + "/hit"}, nil))
	line := func(kind, dup string, planned int64, user, id string, instance, attempt, status int) string {
		return fmt.Sprintf("%s dup=%s planned=%d %s %s instance=%d attempt=%d status=%d", kind, dup, planned, user, id, instance, attempt, status)
	}
	var got []string
	for _, r := range recs {
		got = append(got, line(r.Kind, show(r.DuplicateOf), r.PlannedNs, r.User, r.RequestID, r.Instance, r.Attempt, r.Status))
	}
	want := []string{
		line("initial", "null", 50_000_000, "u000000", "r000000000", 1, 1, 429),
		line("retry", "null", 50_000_000, "u000000", "r000000000", 1, 2, 200),
		line("initial", "null", 60_000_000, "u000001", "r000000001", 2, 1, 200),
		line("initial", "null", 70_000_000, "u000002", "r000000002", 3, 1, 200),
		line("duplicate", "0", 80_000_000, "u000000", "r000000000", 1, 1, 429),
		line("retry", "0", 80_000_000, "u000000", "r000000000", 1, 2, 200),
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("attempt lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for n, wantIDs := range [][]string{{"r000000000", "r000000000", "r000000000", "r000000000"}, {"r000000001"}, {"r000000002"}} {
		var ids []string
		for _, q := range stubs[n].all() {
			ids = append(ids, q.request)
		}
		if !slices.Equal(ids, wantIDs) {
			t.Errorf("instance %d got %v, want %v", n+1, ids, wantIDs)
		}
	}
}

// TestSender_StopEarlyDrops (plan H2 step 7, PRF-8): generator drops are first attempts that
// never went out; they finish their step for stop_early and count as unbounded lag. With
// maxInFlight lowered to 10, a server that never answers and a 1 s client timeout, step 0
// (50/s for 1 s) launches 10 entries and drops the rest; once those 10 time out, step 0 is
// judged generator-limited during the 5 s step 1, and the run stops: stopped_after_step 0,
// excluded_step 1.
func TestSender_StopEarlyDrops(t *testing.T) {
	old := maxInFlight
	maxInFlight = 10
	t.Cleanup(func() { maxInFlight = old })
	st := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request, _ stubReq) {
		s.hang(r)
		answerOK(w, r)
	})
	res := runHitgen(t, t.Context(), scenarioFile(t, 89, `
instances = 1
stop_early = true
[[steps]]
rate_per_s = 50
duration_s = 1
[[steps]]
rate_per_s = 50
duration_s = 5
[users]
count = 10
`), st.addr(), "--http-timeout", "1s")
	if res.code != exitOK {
		t.Fatalf("exit code %d, want 0", res.code)
	}
	sum := summaryOf(t, res.gen)
	t.Logf("summary: planned %d of %d, launched %d, unsent %d", sum.Planned, len(res.schedule), sum.Launched, sum.Unsent)
	if sum.StoppedAfterStep == nil || *sum.StoppedAfterStep != 0 || sum.ExcludedStep == nil || *sum.ExcludedStep != 1 {
		t.Errorf("summary stopped_after_step %s, excluded_step %s; want 0 and 1: step 0's drops make it generator-limited",
			show(sum.StoppedAfterStep), show(sum.ExcludedStep))
	}
	if sum.Planned >= len(res.schedule) {
		t.Errorf("summary planned %d of %d entries, want the run stopped during step 1", sum.Planned, len(res.schedule))
	}
}

// loggedLine decodes into v the first line of hitgen's log whose msg is msg, and reports
// whether there is one.
func loggedLine(t *testing.T, log, msg string, v any) bool {
	t.Helper()
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		var rec struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("hitgen log line is not JSON: %v: %s", err, l)
		}
		if rec.Msg == msg {
			if err := json.Unmarshal([]byte(l), v); err != nil {
				t.Fatalf("hitgen's %q line: %v: %s", msg, err, l)
			}
			return true
		}
	}
	return false
}

// lateMissMsg is the evaluator's log line for a miss judged once launching had ended.
const lateMissMsg = "a step missed after the schedule was sent; nothing left to stop"

// loggedStopReason returns the reason of the step miss in hitgen's log: from its "stopping
// early" line, or else, with late true, from the line of a miss judged once the whole schedule
// had been sent, which stops nothing; "" without either.
func loggedStopReason(t *testing.T, log string) (reason string, late bool) {
	t.Helper()
	var rec struct {
		Reason string `json:"reason"`
	}
	if loggedLine(t, log, "stopping early", &rec) {
		return rec.Reason, false
	}
	if loggedLine(t, log, lateMissMsg, &rec) {
		return rec.Reason, true
	}
	return "", false
}

// loggedStopNs returns stop_ns, the stop's time from the run's start, from hitgen's "stopping
// early" log line. The test fails without the line or the field.
func loggedStopNs(t *testing.T, log string) int64 {
	t.Helper()
	var rec struct {
		StopNs *int64 `json:"stop_ns"`
	}
	if !loggedLine(t, log, "stopping early", &rec) || rec.StopNs == nil {
		t.Fatalf("hitgen logged no stopping early line with stop_ns")
	}
	return *rec.StopNs
}

// loggedJudgesThresholds returns judges_thresholds from hitgen's "sending" log line: whether its
// stop_early evaluator holds the thresholds. The test fails without the line or the field.
func loggedJudgesThresholds(t *testing.T, log string) bool {
	t.Helper()
	var rec struct {
		JudgesThresholds *bool `json:"judges_thresholds"`
	}
	if !loggedLine(t, log, "sending", &rec) || rec.JudgesThresholds == nil {
		t.Fatalf("hitgen logged no sending line with judges_thresholds")
	}
	return *rec.JudgesThresholds
}

// TestRun_ThresholdsAndBaseline (plan H2 step 7; DESIGN §B4 stop_early, §B9): run() gives
// stop_early the thresholds file, except with --baseline (the lag only). Whatever the timing,
// every run's "sending" line says so: judges_thresholds true with --thresholds, false with
// --thresholds --baseline. As an extra, end to end, against a server that answers every
// request 503 and a thresholds file whose step 0 allows no unexpected error
// (error_rate_threshold 0, latency_threshold_ns 10 s), hitgen stops after step 0 for the
// unexpected-error rate, and with --baseline runs to the end. Step 0 (0.4/s for 5.5 s) has one
// entry, planned inside its observation window (after its first 5 s, §B9), where the error rate
// is judged. Step 0's lag is judged first, and a loaded machine can make it generator-limited;
// its miss can also be judged only once the whole schedule was sent, which stops nothing. A run
// whose step 0 missed for the lag, either way, is run again, up to 10 runs in all; a later miss
// for the error rate shows the thresholds judged, with the stop fields null. If every run missed
// for the lag, no run judged the thresholds and the test fails.
func TestRun_ThresholdsAndBaseline(t *testing.T) {
	const (
		lagReason   = "generator-limited: p99 scheduling lag above 5 ms"
		errorReason = "unexpected-error rate above the threshold"
		tries       = 10
	)
	st := newStub(t, func(_ *stub, w http.ResponseWriter, _ *http.Request, _ stubReq) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "limiter_unavailable", "message": "unavailable"})
	})
	path := scenarioFile(t, 167, `
instances = 1
stop_early = true
[[steps]]
rate_per_s = 0.4
duration_s = 5.5
[[steps]]
rate_per_s = 5
duration_s = 1
[users]
count = 10
`)
	sc, err := config.LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	// The schedule the test needs: 1 entry in step 0, inside its window, and step 1 still
	// sending 500 ms after it, so that step 0's miss stops the launching.
	entries, _ := BuildSchedule(sc)
	var step0 []Entry
	for _, e := range entries {
		if e.Step == 0 {
			step0 = append(step0, e)
		}
	}
	if len(step0) != 1 || step0[0].PlannedNs < 5*sec || entries[len(entries)-1].PlannedNs < step0[0].PlannedNs+500_000_000 {
		t.Fatalf("seed 167 plans %d entries in step 0 and %d in all, want 1 in step 0, at or after 5 s, and step 1 sending "+
			"500 ms after it", len(step0), len(entries))
	}
	thresholds := filepath.Join(t.TempDir(), "thresholds.json")
	if err := os.WriteFile(thresholds, []byte(`{"steps": [{"step": 0, "latency_threshold_ns": 10000000000, "error_rate_threshold": 0}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		extra  []string
		judged bool   // the sending line's judges_thresholds
		want   string // the miss reason once step 0's lag is allowed; "" for no miss
	}{
		{"--thresholds", []string{"--thresholds", thresholds}, true, errorReason},
		{"--thresholds --baseline", []string{"--thresholds", thresholds, "--baseline"}, false, ""},
	} {
		reason, late := lagReason, false
		var sum genRec
		for try := 1; try <= tries && reason == lagReason; try++ {
			res := runHitgen(t, t.Context(), path, st.addr(), c.extra...)
			if res.code != exitOK {
				t.Fatalf("%s: exit code %d, want 0", c.name, res.code)
			}
			if got := loggedJudgesThresholds(t, res.log); got != c.judged {
				t.Fatalf("%s: the sending line's judges_thresholds is %v, want %v", c.name, got, c.judged)
			}
			sum = summaryOf(t, res.gen)
			reason, late = loggedStopReason(t, res.log)
			if reason == lagReason {
				t.Logf("%s: run %d missed for the scheduling lag (a loaded machine; judged late: %v)", c.name, try, late)
			}
		}
		switch {
		case reason == lagReason:
			t.Errorf("%s: all %d runs missed for the scheduling lag: no run judged step 0 past its lag", c.name, tries)
		case reason != c.want:
			t.Errorf("%s: missed for %q (judged late: %v), want %q", c.name, reason, late, c.want)
		case c.want != "" && !late && (show(sum.StoppedAfterStep) != "0" || show(sum.ExcludedStep) != "1"):
			t.Errorf("%s: summary stopped_after_step %s, excluded_step %s; want 0 and 1", c.name, show(sum.StoppedAfterStep), show(sum.ExcludedStep))
		case (c.want == "" || late) && (sum.StoppedAfterStep != nil || sum.ExcludedStep != nil):
			t.Errorf("%s: summary stopped_after_step %s, excluded_step %s; want null and null (judged late: %v)", c.name,
				show(sum.StoppedAfterStep), show(sum.ExcludedStep), late)
		}
	}
}

// TestRun_RetriesPolicy (plan H2 steps 1 and 5, TRF-5): the scenario's [retries] reach the
// sender: with max_extra = 1 and jitter_max_ms = 0, against a server that answers every request
// 429 (retry_after_ms 100), every entry makes exactly 2 attempts, and its retry has hint_ms 100
// and jitter_ns 0.
func TestRun_RetriesPolicy(t *testing.T) {
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) { answerLimitedMs(w, r, 100) })
	res := runHitgen(t, t.Context(), scenarioFile(t, 101, `
instances = 1
[[steps]]
rate_per_s = 20
duration_s = 1
[users]
count = 5
[retries]
max_extra = 1
jitter_max_ms = 0
`), st.addr())
	if res.code != exitOK {
		t.Fatalf("exit code %d, want 0", res.code)
	}
	if len(res.schedule) == 0 {
		t.Fatal("no entry planned")
	}
	recs := attempts(t, res.gen)
	for _, e := range res.schedule {
		lines, arrivals := linesFor(recs, e.RequestID), st.requests(e.RequestID)
		if len(lines) != 2 || len(arrivals) != 2 {
			t.Errorf("%s: %d attempt lines and %d arrivals, want 2 and 2 (max_extra 1)", e.RequestID, len(lines), len(arrivals))
			continue
		}
		if r := lines[1]; r.Attempt != 2 || r.Kind != "retry" || show(r.HintMs) != "100" || show(r.JitterNs) != "0" {
			t.Errorf("%s's retry: attempt %d, kind %s, hint_ms %s, jitter_ns %s; want 2, retry, 100 and 0 (jitter_max_ms 0)",
				e.RequestID, r.Attempt, r.Kind, show(r.HintMs), show(r.JitterNs))
		}
	}
}

// TestRun_SkippedNoEligible (plan H2 steps 2 and 6): the arrivals BuildSchedule leaves out for
// want of an eligible user reach the summary. With the only cohort, all 5 users, stopped over
// [0.3 s, 0.7 s) of a 50/s step of 1 s, the summary's skipped_no_eligible equals BuildSchedule's
// count, which is above 0 (20 expected).
func TestRun_SkippedNoEligible(t *testing.T) {
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) { answerOK(w, r) })
	path := scenarioFile(t, 103, `
instances = 1
[[steps]]
rate_per_s = 50
duration_s = 1
[users]
count = 5
[[cohorts]]
users = [0, 4]
stop_s = 0.3
revisit_s = 0.7
`)
	sc, err := config.LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	_, skipped := BuildSchedule(sc)
	if skipped <= 0 {
		t.Fatalf("BuildSchedule skipped %d arrivals, want some: the test needs them", skipped)
	}
	res := runHitgen(t, t.Context(), path, st.addr())
	if res.code != exitOK {
		t.Fatalf("exit code %d, want 0", res.code)
	}
	if sum := summaryOf(t, res.gen); sum.SkippedNoEligible != skipped {
		t.Errorf("summary skipped_no_eligible %d, want BuildSchedule's %d", sum.SkippedNoEligible, skipped)
	}
}

// TestSummary_StopFields (plan H2 step 7): for a run stopped at stop_ns after step k's miss,
// planned counts exactly the entries planned before stop_ns, and excluded_step is the step
// whose [start, end) holds stop_ns, at least k+1, and the last step for a stop after the run's
// end. Steps [0, 2 s), [2 s, 2.5 s), [2.5 s, 6.5 s) and [6.5 s, 7.5 s); entries planned at 1 s,
// 2 s, 2.2 s, 3 s and 7 s.
func TestSummary_StopFields(t *testing.T) {
	const ms = int64(1_000_000)
	bounds := []int64{0, 2 * sec, 2500 * ms, 6500 * ms, 7500 * ms}
	var entries []Entry
	for i, planned := range []int64{1 * sec, 2 * sec, 2200 * ms, 3 * sec, 7 * sec} {
		entries = append(entries, Entry{I: i, PlannedNs: planned, Instance: 1, Kind: "initial", DuplicateOf: -1})
	}
	for _, c := range []struct {
		name              string
		k                 int
		stopNs            int64
		planned, excluded int
	}{
		{"inside step 0, whose last first attempts finished early", 0, 1500 * ms, 1, 1},
		{"inside step 1", 0, 2100 * ms, 2, 1},
		{"at an entry's planned time, which is not before it", 0, 2 * sec, 1, 1},
		{"1 ns after an entry's planned time", 0, 2*sec + 1, 2, 1},
		{"at step 2's start", 0, 2500 * ms, 3, 2},
		{"inside step 2, a later step", 0, 4 * sec, 4, 2},
		{"inside step 1, after step 1 missed", 1, 2100 * ms, 2, 2},
		{"after the run's end", 0, 8 * sec, 5, 3},
		{"before every entry", 0, 0, 0, 1},
	} {
		planned, excluded := stopFields(entries, bounds, c.k, c.stopNs)
		if planned != c.planned || excluded != c.excluded {
			t.Errorf("%s (k %d, stop at %d ns): planned %d, excluded_step %d; want %d and %d", c.name, c.k, c.stopNs,
				planned, excluded, c.planned, c.excluded)
		}
	}
}

// TestSender_StopEarlyUnreached (plan H2 step 7): a run stopped early counts as planned exactly
// the entries planned before the stop, those already due that the dispatcher has not reached
// yet included, so unsent covers them too. The test drives the dispatcher's wake-ups: one at
// 50 ms launches the entry planned at 10 ms, and none follows, so the entries planned at 100,
// 150 and 200 ms come due unreached. At 400 ms the test reports dropped the one entry of step 0
// ([0, 5 ms)), which the sender never gets: step 0 is generator-limited, and its miss stops the
// run then. The entries planned at 600 and 700 ms come after the stop. So the summary has
// planned 4 (the entries planned before stop_ns, from the log), launched 1, unsent 3,
// stopped_after_step 0 and excluded_step 1 (the stop is inside step 1), and the server got
// the first entry only.
func TestSender_StopEarlyUnreached(t *testing.T) {
	const ms = int64(1_000_000)
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) { answerOK(w, r) })
	entry := func(i, step int, planned int64) Entry {
		return Entry{I: i, PlannedNs: planned, Step: step, User: fmt.Sprintf("u%06d", i), RequestID: fmt.Sprintf("r%09d", i),
			Instance: 1, Kind: "initial", DuplicateOf: -1}
	}
	entries := []Entry{entry(0, 1, 10*ms), entry(1, 1, 100*ms), entry(2, 1, 150*ms), entry(3, 1, 200*ms), entry(4, 1, 600*ms),
		entry(5, 1, 700*ms)}
	phantom := entry(6, 0, 0)
	var logs syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("hitgen log:\n%s", logs.String())
		}
	})
	ev := newEvaluator(logger, append(slices.Clone(entries), phantom), []int64{0, 5 * ms, 10 * sec}, nil)
	path := filepath.Join(t.TempDir(), "gen.jsonl")
	gen, err := openGenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	client, tr := newClient(5*time.Second, defaultMaxConns)
	t.Cleanup(tr.CloseIdleConnections)
	snd := newSender(sendConfig{urls: []string{st.URL + "/hit"}, client: client, timeout: 5 * time.Second, seed: 7,
		retries: config.Retries{MaxExtra: 2, JitterMaxMs: 50}, eval: ev, log: gen, logger: logger}, entries)
	ticks := make(chan time.Time)
	snd.ticks = ticks

	start := time.Now()
	sums := make(chan summary, 1)
	go func() { sums <- snd.run(t.Context()) }()
	at := func(d time.Duration) {
		if left := time.Until(start.Add(d)); left > 0 {
			<-time.After(left)
		}
	}
	at(50 * time.Millisecond)
	select {
	case ticks <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("the dispatcher took no wake-up within 10 s")
	}
	at(400 * time.Millisecond)
	ev.dropped(phantom)
	var sum summary
	select {
	case sum = <-sums:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end within 10 s of the stop")
	}
	if code := finish(t.Context(), logger, gen, sum, logDrainTimeout); code != exitOK {
		t.Fatalf("finish exited %d, want 0", code)
	}

	stopNs := loggedStopNs(t, logs.String())
	if stopNs <= 200*ms || stopNs >= 600*ms {
		t.Fatalf("the stop came at %d ns, want it between the entries planned at 200 and 600 ms (the drop came at 400 ms)", stopNs)
	}
	if sum.Planned != 4 || sum.Launched != 1 || sum.Unsent != 3 {
		t.Errorf("summary planned %d, launched %d, unsent %d; want 4 (planned before the stop at %d ns), 1 and 3", sum.Planned,
			sum.Launched, sum.Unsent, stopNs)
	}
	if show(sum.StoppedAfterStep) != "0" || show(sum.ExcludedStep) != "1" {
		t.Errorf("summary stopped_after_step %s, excluded_step %s; want 0 and 1", show(sum.StoppedAfterStep), show(sum.ExcludedStep))
	}
	if reqs := st.all(); len(reqs) != 1 || reqs[0].request != "r000000000" {
		t.Errorf("the server got %d requests, want r000000000 only", len(reqs))
	}
	if recs := attempts(t, readJSONL(t, path)); len(recs) != 1 || recs[0].SentNs == nil {
		t.Errorf("%d attempt lines, want r000000000's, written", len(recs))
	}
}

// TestRecord_FailureLatency (plan H2 step 6; DESIGN §B4 "Record": latency, monotonic ns): an
// attempt written to the wire has a latency even without a response, from WroteRequest to its
// failure. A server that reads the request, waits 200 ms, then resets the connection without
// replying gives status 0, transport_error, a sent_ns and a latency of at least those 200 ms.
// An attempt to a closed port fails before WroteRequest: status 0, transport_error, and
// sent_ns and latency both null.
func TestRecord_FailureLatency(t *testing.T) {
	const delay = 200 * time.Millisecond
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		answerReset(t, w)
	})
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{I: 0, PlannedNs: 0, User: "u000000", RequestID: "r000000000", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 1, PlannedNs: 0, User: "u000001", RequestID: "r000000001", Instance: 2, Kind: "initial", DuplicateOf: -1},
	}
	recs := attempts(t, sendAll(t, entries, []string{st.URL + "/hit", "http://" + closed + "/hit"}, nil))
	byID := map[string]genRec{}
	for _, r := range recs {
		byID[r.RequestID] = r
	}
	if len(recs) != 2 {
		t.Fatalf("%d attempt lines, want 2", len(recs))
	}
	if r := byID["r000000000"]; r.Status != 0 || show(r.Error) != "transport_error" || r.SentNs == nil || r.Latency == nil ||
		*r.Latency < delay.Nanoseconds() {
		t.Errorf("reset after %v: status %d, error %s, sent_ns %s, latency %s; want 0, transport_error, a sent time and a "+
			"latency of at least %d ns", delay, r.Status, show(r.Error), show(r.SentNs), show(r.Latency), delay.Nanoseconds())
	}
	if r := byID["r000000001"]; r.Status != 0 || show(r.Error) != "transport_error" || r.SentNs != nil || r.Latency != nil {
		t.Errorf("a closed port: status %d, error %s, sent_ns %s, latency %s; want 0, transport_error, null and null",
			r.Status, show(r.Error), show(r.SentNs), show(r.Latency))
	}
}

// TestRun_StepsFixRunLength (plan H2 step 1: the steps fix the run length; rate_per_s = 0 is
// allowed): a run that sends its whole schedule lasts the sum of its steps' durations from its
// start, with a trailing rate-0 step or a single rate-0 step that plans nothing, and ends
// within 2 s after that (the server answers at once). A context that ends (main's SIGTERM)
// ends that wait: a 30 s rate-0 run whose context ends at 300 ms exits 0 within 2 s of that,
// with its summary.
func TestRun_StepsFixRunLength(t *testing.T) {
	const slack = 2 * time.Second
	st := newStub(t, func(_ *stub, w http.ResponseWriter, r *http.Request, _ stubReq) { answerOK(w, r) })
	for _, c := range []struct {
		name    string
		steps   string
		length  time.Duration
		entries bool // the schedule plans entries
	}{
		{"20/s for 1 s, then rate 0 for 2 s", `
[[steps]]
rate_per_s = 20
duration_s = 1
[[steps]]
rate_per_s = 0
duration_s = 2
`, 3 * time.Second, true},
		{"one step of rate 0 for 2 s", `
[[steps]]
rate_per_s = 0
duration_s = 2
`, 2 * time.Second, false},
	} {
		path := scenarioFile(t, 107, "instances = 1\n"+c.steps+"[users]\ncount = 5\n")
		start := time.Now()
		res := runHitgen(t, t.Context(), path, st.addr())
		took := time.Since(start)
		if res.code != exitOK {
			t.Fatalf("%s: exit code %d, want 0", c.name, res.code)
		}
		if (len(res.schedule) > 0) != c.entries {
			t.Fatalf("%s: %d entries planned, want entries: %v", c.name, len(res.schedule), c.entries)
		}
		if took < c.length || took > c.length+slack {
			t.Errorf("%s: the run took %v, want from %v (the steps' length) to %v", c.name, took, c.length, c.length+slack)
		}
		if sum := summaryOf(t, res.gen); sum.Planned != len(res.schedule) || sum.Launched != sum.Planned || sum.Unsent != 0 {
			t.Errorf("%s: summary planned %d, launched %d, unsent %d; want %d, all launched, none unsent", c.name,
				sum.Planned, sum.Launched, sum.Unsent, len(res.schedule))
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	path := scenarioFile(t, 107, "instances = 1\n[[steps]]\nrate_per_s = 0\nduration_s = 30\n[users]\ncount = 5\n")
	start := time.Now()
	res := runHitgen(t, ctx, path, st.addr())
	if took := time.Since(start); res.code != exitOK || took > 300*time.Millisecond+slack {
		t.Errorf("a 30 s rate-0 run whose context ends at 300 ms: exit code %d after %v, want 0 within %v", res.code, took,
			300*time.Millisecond+slack)
	}
	if sum := summaryOf(t, res.gen); sum.Planned != 0 || sum.StoppedAfterStep != nil {
		t.Errorf("that run's summary: planned %d, stopped_after_step %s; want 0 and null", sum.Planned, show(sum.StoppedAfterStep))
	}
}
