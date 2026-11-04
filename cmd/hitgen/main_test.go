package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
	"github.com/srir84/ratelimiter/harness/runlog"
)

// TestParseFlags (plan H2 step 8): --scenario, --targets and --out are required; every target
// is a loopback IP address with a port from 1 to 65535 (HAR-20); --http-timeout and
// --max-conns are positive and default to 30 s and 512; anything else is refused.
func TestParseFlags(t *testing.T) {
	base := []string{"--scenario", "s.toml", "--targets", "127.0.0.1:8081", "--out", "out"}
	with := func(extra ...string) []string { return append(slices.Clone(base), extra...) }
	def := flags{scenario: "s.toml", out: "out", targets: []string{"127.0.0.1:8081"}, timeout: 30 * time.Second, maxConns: 512}
	for _, c := range []struct {
		name string
		args []string
		want *flags // nil: refused
	}{
		{"required only", base, &def},
		{"every flag", with("--thresholds", "th.json", "--baseline", "--http-timeout", "1s", "--max-conns", "2"),
			&flags{scenario: "s.toml", out: "out", thresholds: "th.json", targets: []string{"127.0.0.1:8081"}, baseline: true,
				timeout: time.Second, maxConns: 2}},
		{"three targets in order", []string{"--scenario", "s", "--out", "o", "--targets", "127.0.0.1:8081,127.0.0.1:8082,[::1]:8083"},
			&flags{scenario: "s", out: "o", targets: []string{"127.0.0.1:8081", "127.0.0.1:8082", "[::1]:8083"},
				timeout: 30 * time.Second, maxConns: 512}},
		{"no --scenario", []string{"--targets", "127.0.0.1:1", "--out", "o"}, nil},
		{"no --targets", []string{"--scenario", "s", "--out", "o"}, nil},
		{"no --out", []string{"--scenario", "s", "--targets", "127.0.0.1:1"}, nil},
		{"extra argument", with("more"), nil},
		{"unknown flag", with("--bogus"), nil},
		{"timeout 0", with("--http-timeout", "0s"), nil},
		{"negative timeout", with("--http-timeout", "-1s"), nil},
		{"max-conns 0", with("--max-conns", "0"), nil},
		{"an empty target", []string{"--scenario", "s", "--out", "o", "--targets", "127.0.0.1:1,"}, nil},
		{"a LAN target", []string{"--scenario", "s", "--out", "o", "--targets", "192.168.1.5:8081"}, nil},
		{"a host name", []string{"--scenario", "s", "--out", "o", "--targets", "localhost:8081"}, nil},
		{"no port", []string{"--scenario", "s", "--out", "o", "--targets", "127.0.0.1"}, nil},
		{"port 0", []string{"--scenario", "s", "--out", "o", "--targets", "127.0.0.1:0"}, nil},
		{"port 65536", []string{"--scenario", "s", "--out", "o", "--targets", "127.0.0.1:65536"}, nil},
		{"a named port", []string{"--scenario", "s", "--out", "o", "--targets", "127.0.0.1:http"}, nil},
	} {
		got, err := parseFlags(c.args, io.Discard)
		switch {
		case c.want == nil && err == nil:
			t.Errorf("%s: parseFlags(%q) = %+v, want an error", c.name, c.args, got)
		case c.want != nil && err != nil:
			t.Errorf("%s: parseFlags(%q): %v", c.name, c.args, err)
		case c.want != nil && !reflect.DeepEqual(got, *c.want):
			t.Errorf("%s: parseFlags(%q) = %+v, want %+v", c.name, c.args, got, *c.want)
		}
	}
}

// TestRun_ExitCodes (plan H2 step 8): a bad command line, a refused scenario, a --targets list
// that does not name one address per instance and a refused thresholds file exit 2 before
// anything is written; an out directory that cannot be created exits 1; --help exits 0.
func TestRun_ExitCodes(t *testing.T) {
	good := scenarioFile(t, 3, `
instances = 1
[[steps]]
rate_per_s = 0
duration_s = 1
[users]
count = 1
`)
	refused := scenarioFile(t, 3, `
instances = 2
[[steps]]
rate_per_s = 1
duration_s = 1
[users]
count = 1
`)
	badThresholds := filepath.Join(t.TempDir(), "thresholds.json")
	if err := os.WriteFile(badThresholds, []byte(`{"steps": [{"step": 5, "generator_limited": true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args []string
		want int
	}{
		{"--help", []string{"--help"}, exitOK},
		{"a bad command line", []string{"--scenario", good}, exitUsage},
		{"a refused scenario (instances 2)", []string{"--scenario", refused, "--targets", "127.0.0.1:1"}, exitUsage},
		{"a missing scenario", []string{"--scenario", filepath.Join(t.TempDir(), "none.toml"), "--targets", "127.0.0.1:1"}, exitUsage},
		{"two targets for one instance", []string{"--scenario", good, "--targets", "127.0.0.1:1,127.0.0.1:2"}, exitUsage},
		{"a refused thresholds file", []string{"--scenario", good, "--targets", "127.0.0.1:1", "--thresholds", badThresholds}, exitUsage},
		{"an out directory under a file", []string{"--scenario", good, "--targets", "127.0.0.1:1", "--out", filepath.Join(blocker, "out")}, exitFailure},
	} {
		out := filepath.Join(t.TempDir(), "out")
		args := c.args
		if !slices.Contains(args, "--out") && c.want == exitUsage {
			args = append(slices.Clone(args), "--out", out)
		}
		if got := run(t.Context(), args, io.Discard); got != c.want {
			t.Errorf("%s: exit code %d, want %d", c.name, got, c.want)
		}
		if _, err := os.Stat(out); c.want == exitUsage && err == nil {
			t.Errorf("%s: the out directory was created, want nothing written on a refusal", c.name)
		}
	}
}

// TestRecord_ResponseFields checks what gen.jsonl takes from a response body (HTTP-6, HTTP-7,
// HTTP-9; plan H2 step 6): error when it is a stable code, retry_after_ms when a whole number
// ≥ 0, arrival_ns and decision_ns when decimal strings of a positive int64; anything else, and
// any field of a body that is not a JSON object, is null.
func TestRecord_ResponseFields(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	str := func(v string) *string { return &v }
	for _, c := range []struct {
		name string
		body string
		want bodyFields
	}{
		{"a 200", `{"user_id":"u1","request_id":"r1","arrival_ns":"1791374400013214457","existed":false,"degraded":false,"decision_ns":"1791374400013906000"}`,
			bodyFields{arrivalNs: i64(1791374400013214457), decisionNs: i64(1791374400013906000)}},
		{"a 429", `{"error":"rate_limited","message":"m","retry_after_ms":200,"decision_ns":"7"}`,
			bodyFields{errCode: str("rate_limited"), retryAfterMs: i64(200), decisionNs: i64(7)}},
		{"a degraded 200", `{"arrival_ns":"5","degraded":true}`, bodyFields{arrivalNs: i64(5)}},
		{"not JSON", `rate limited`, bodyFields{}},
		{"a JSON array", `["error"]`, bodyFields{}},
		{"empty", ``, bodyFields{}},
		{"an error that is not a code", `{"error":"Rate Limited!"}`, bodyFields{}},
		{"an error of 64 characters", `{"error":"` + strings.Repeat("a", 64) + `"}`, bodyFields{errCode: str(strings.Repeat("a", 64))}},
		{"an error of 65 characters", `{"error":"` + strings.Repeat("a", 65) + `"}`, bodyFields{}},
		{"an empty error", `{"error":""}`, bodyFields{}},
		{"an error that is a number", `{"error":5,"retry_after_ms":3}`, bodyFields{retryAfterMs: i64(3)}},
		{"retry_after_ms negative", `{"retry_after_ms":-1}`, bodyFields{}},
		{"retry_after_ms a fraction", `{"retry_after_ms":1.5}`, bodyFields{}},
		{"retry_after_ms with an exponent", `{"retry_after_ms":2e2}`, bodyFields{}},
		{"retry_after_ms a string", `{"retry_after_ms":"200"}`, bodyFields{}},
		{"arrival_ns a number", `{"arrival_ns":5}`, bodyFields{}},
		{"arrival_ns 0", `{"arrival_ns":"0"}`, bodyFields{}},
		{"arrival_ns signed", `{"arrival_ns":"+5","decision_ns":"-5"}`, bodyFields{}},
		{"arrival_ns past int64", `{"arrival_ns":"9223372036854775808"}`, bodyFields{}},
		{"decision_ns not decimal", `{"decision_ns":"0x10"}`, bodyFields{}},
	} {
		got := parseBody([]byte(c.body))
		if show(got.errCode) != show(c.want.errCode) || show(got.retryAfterMs) != show(c.want.retryAfterMs) ||
			show(got.arrivalNs) != show(c.want.arrivalNs) || show(got.decisionNs) != show(c.want.decisionNs) {
			t.Errorf("%s: error %s, retry_after_ms %s, arrival_ns %s, decision_ns %s; want %s, %s, %s, %s", c.name,
				show(got.errCode), show(got.retryAfterMs), show(got.arrivalNs), show(got.decisionNs),
				show(c.want.errCode), show(c.want.retryAfterMs), show(c.want.arrivalNs), show(c.want.decisionNs))
		}
	}
}

// constDuration returns the time.Duration constant name declared in the Go file at path, whose
// value must be written as an integer literal times one of time's units.
func constDuration(t *testing.T, path, name string) time.Duration {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	units := map[string]time.Duration{"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond,
		"Millisecond": time.Millisecond, "Second": time.Second, "Minute": time.Minute, "Hour": time.Hour}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				mul, ok := vs.Values[i].(*ast.BinaryExpr)
				if !ok || mul.Op != token.MUL {
					t.Fatalf("%s: %s is not written as <integer> * time.<unit>", path, name)
				}
				lit, okLit := mul.X.(*ast.BasicLit)
				sel, okSel := mul.Y.(*ast.SelectorExpr)
				if !okLit || lit.Kind != token.INT || !okSel {
					t.Fatalf("%s: %s is not written as <integer> * time.<unit>", path, name)
				}
				pkg, okPkg := sel.X.(*ast.Ident)
				unit, okUnit := units[sel.Sel.Name]
				v, err := strconv.ParseInt(lit.Value, 0, 64)
				if !okPkg || pkg.Name != "time" || !okUnit || err != nil {
					t.Fatalf("%s: %s is not written as <integer> * time.<unit>", path, name)
				}
				return time.Duration(v) * unit
			}
		}
	}
	t.Fatalf("%s: no constant %s", path, name)
	return 0
}

// TestClient_IdleTimeout (plan H2 step 4; AGENTS.md §6): hitgen retires an idle keep-alive
// connection before hitapi closes it, since a /hit request is never resent: the client's
// IdleConnTimeout is idleConnTimeout, above 0 and below hitapi's idleTimeout, read from
// cmd/hitapi/main.go.
func TestClient_IdleTimeout(t *testing.T) {
	_, tr := newClient(time.Second, 1)
	server := constDuration(t, filepath.Join("..", "hitapi", "main.go"), "idleTimeout")
	if tr.IdleConnTimeout != idleConnTimeout || idleConnTimeout <= 0 || idleConnTimeout >= server {
		t.Errorf("the client's IdleConnTimeout is %v (idleConnTimeout %v), want idleConnTimeout, above 0 and below hitapi's "+
			"idleTimeout %v", tr.IdleConnTimeout, idleConnTimeout, server)
	}
}

// TestRun_StopEarlyEvaluator (plan H2 step 7; DESIGN §B4 stop_early): a stop_early scenario gets
// an evaluator over its step bounds that judges the thresholds given, except in a baseline run,
// which is judged on the lag only; a scenario without stop_early gets none.
func TestRun_StopEarlyEvaluator(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	sc := config.Scenario{StopEarly: true, Steps: []config.Step{{RatePerS: 1, DurationS: 2}, {RatePerS: 1, DurationS: 3}}}
	th := []stepThreshold{{set: true, latencyNs: 7, errorRate: 0.5}, {}}
	ev := stopEarlyEvaluator(logger, sc, nil, th, false)
	if ev == nil || !reflect.DeepEqual(ev.thresholds, th) || !reflect.DeepEqual(ev.bounds, []int64{0, 2_000_000_000, 5_000_000_000}) {
		t.Errorf("stop_early with thresholds: evaluator %+v, want thresholds %+v and bounds [0 2e9 5e9]", ev, th)
	}
	if ev := stopEarlyEvaluator(logger, sc, nil, th, true); ev == nil || ev.thresholds != nil {
		t.Errorf("stop_early with thresholds and --baseline: evaluator %+v, want one without thresholds", ev)
	}
	sc.StopEarly = false
	if ev := stopEarlyEvaluator(logger, sc, nil, th, false); ev != nil {
		t.Errorf("no stop_early: evaluator %+v, want none", ev)
	}
}

// TestFinish_StuckLog (plan H2 steps 6 and 8; AGENTS.md §2: Close is bounded by its drain
// timeout): when a gen.jsonl write is stuck, as on a FIFO whose reader stays open but reads
// nothing, finish gives up once its drain timeout (200 ms here) has passed and does not wait on
// the stuck write: it returns 1 within that timeout plus 2 s, logs how many lines were still
// pending, and writes no summary (the reader, drained at the end, finds none).
func TestFinish_StuckLog(t *testing.T) {
	const drainTimeout = 200 * time.Millisecond
	path := filepath.Join(t.TempDir(), "gen.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// Each end's open waits for the other's: the reader opens in its own goroutine.
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.Open(path)
		if err != nil {
			t.Errorf("opening the FIFO to read: %v", err)
		}
		opened <- f
	}()
	gen, err := runlog.Open(path)
	if err != nil {
		if w, werr := os.OpenFile(path, os.O_WRONLY, 0); werr == nil { // lets the reader's open return
			_ = w.Close()
		}
		if f := <-opened; f != nil {
			_ = f.Close()
		}
		t.Fatal(err)
	}
	reader := <-opened
	if reader == nil {
		t.FailNow()
	}
	// Far more than a FIFO holds (64 KiB at most on macOS): the writer's write(2) blocks.
	for range 256 {
		gen.Enqueue(map[string]string{"pad": strings.Repeat("x", 4096)})
	}
	var stderr syncBuffer
	logger := slog.New(slog.NewJSONHandler(&stderr, nil))
	code := -1
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		code = finish(t.Context(), logger, gen, summary{Kind: kindSummary}, drainTimeout)
	}()
	// release reads the FIFO to its end: the stuck write completes, Close writes the rest of the
	// lines and closes the writer, and the reader sees the end of the file. finish then returns,
	// if it has not already.
	var once sync.Once
	var rest []byte
	release := func() {
		once.Do(func() {
			read := make(chan []byte, 1)
			go func() {
				data, err := io.ReadAll(reader)
				if err != nil {
					t.Errorf("reading the FIFO: %v", err)
				}
				read <- data
			}()
			_ = gen.Close()
			rest = <-read
			_ = reader.Close()
			<-done
		})
	}
	t.Cleanup(release)

	bound := time.NewTimer(drainTimeout + 2*time.Second)
	defer bound.Stop()
	returned := true
	select {
	case <-done:
	case <-bound.C:
		returned = false
		t.Errorf("finish still waiting %v after it started, with a %v drain timeout: want it to give up on the stuck write",
			time.Since(start), drainTimeout)
	}
	took := time.Since(start)
	release()
	if returned && (code != exitFailure || took > drainTimeout+2*time.Second) {
		t.Errorf("finish returned %d after %v, want 1 within %v", code, took, drainTimeout+2*time.Second)
	}
	var rec struct {
		PendingLines *int64 `json:"pending_lines"`
	}
	if !loggedLine(t, stderr.String(), "attempt lines not written: no summary", &rec) || rec.PendingLines == nil ||
		*rec.PendingLines <= 0 {
		t.Errorf("finish logged no line count still pending (log: %s)", stderr.String())
	}
	if bytes.Contains(rest, []byte(`"kind":"summary"`)) {
		t.Error("the summary reached gen.jsonl after its attempt lines were stuck, want none")
	}
}
