//go:build rltest

package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/internal/testutil/golden"
)

// goldenAPI is the hand-written api-N.jsonl golden file (DESIGN §C5).
func goldenAPI(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "runlog", "testdata", "golden", "api.jsonl")
}

// keys returns m's keys, sorted.
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestDesign_HitapiLifecycle: the stack's SIGKILL grace for the hitapi is P + drain_timeout +
// 5 s = 45 s at the harness values (plan step 12); bin/test/hitapi's start record has every
// DESIGN §B8 field (the in-flight cap, queue length 0, P, the dependency deadline, the drain
// timeout) with the harness values, and an effective nofile limit of at least 1,024 (§B1);
// while it runs, its process record .out/procs/<pid> holds its command; after SIGTERM it
// writes a stop record with listener_drops 0 and log_drops 0, exits 0, and its record is gone
// (HAR-15, plan step 14). Its start, ready and stop lines match the golden api.jsonl on kind
// and event.
func TestDesign_HitapiLifecycle(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	if api.grace != 45*time.Second {
		t.Errorf("hitapi's SIGKILL grace is %v, want P + drain_timeout + 5 s = 45 s at the harness values", api.grace)
	}

	rec, err := os.ReadFile(api.RecordPath())
	if err != nil {
		t.Fatalf("hitapi's process record: %v", err)
	}
	if got := strings.TrimSpace(string(rec)); got != api.Cmdline || !strings.Contains(got, filepath.Join("bin", "test", "hitapi")) {
		t.Errorf("process record %s holds %q, want the hitapi command %q", api.RecordPath(), got, api.Cmdline)
	}
	if r := api.Hit(t, "life", "r1"); r.Status != http.StatusOK {
		t.Errorf("a /hit while it runs: %d %s, want 200", r.Status, r.Body)
	}

	starts := api.WaitRecords(t, "lifecycle", func(rs []map[string]any) bool { return len(rs) >= 2 })
	start := starts[0]
	want := []string{"dependency_deadline_ns", "drain_timeout_ns", "event", "kind", "max_in_flight", "max_release_delay_ns",
		"nofile_limit", "queue_len"}
	if got := keys(start); !slices.Equal(got, want) {
		t.Errorf("start record keys %v, want %v", got, want)
	}
	if start["event"] != "start" || Int(t, start, "max_in_flight") != 512 || Int(t, start, "queue_len") != 0 ||
		Int(t, start, "max_release_delay_ns") != 5_000_000_000 || Int(t, start, "dependency_deadline_ns") != 2_000_000_000 ||
		Int(t, start, "drain_timeout_ns") != 35_000_000_000 {
		t.Errorf("start record %v, want max_in_flight 512, queue_len 0, P 5 s, dependency deadline 2 s, drain 35 s", start)
	}
	if n := Int(t, start, "nofile_limit"); n < 1024 {
		t.Errorf("start record nofile_limit %d, want at least 1,024", n)
	}
	if starts[1]["event"] != "ready" {
		t.Errorf("second lifecycle record %v, want ready", starts[1])
	}

	code, ok := api.Terminate(t)
	if !ok || code != 0 {
		out, _ := os.ReadFile(api.Out)
		t.Errorf("hitapi after SIGTERM: exit %d (within its grace: %v), want 0; its output:\n%s", code, ok, out)
	}
	if _, err := os.Stat(api.RecordPath()); !os.IsNotExist(err) {
		t.Errorf("process record %s after hitapi exited: %v, want it gone", api.RecordPath(), err)
	}
	life := api.Records(t, "lifecycle")
	if len(life) != 3 {
		t.Fatalf("lifecycle records %v, want start, ready and stop", life)
	}
	stop := life[2]
	if got := keys(stop); !slices.Equal(got, []string{"event", "kind", "listener_drops", "log_drops"}) {
		t.Errorf("stop record keys %v, want event, kind, listener_drops, log_drops", got)
	}
	if stop["event"] != "stop" || Int(t, stop, "listener_drops") != 0 || Int(t, stop, "log_drops") != 0 {
		t.Errorf("stop record %v, want listener_drops 0 and log_drops 0", stop)
	}
	lines := api.Lines(t)
	if last, err := decodeObject(lines[len(lines)-1]); err != nil || last["event"] != "stop" {
		t.Errorf("the run log's last line is %s, want the stop record", lines[len(lines)-1])
	}
	var lifeLines [][]byte
	for _, line := range lines {
		if m, err := decodeObject(line); err == nil && m["kind"] == "lifecycle" {
			lifeLines = append(lifeLines, line)
		}
	}
	golden.Match(t, lifeLines, goldenAPI(t), "kind", "event")
}

// TestDesign_HitapiPrekill: with one /hit held in flight by an open uncommitted insert of its
// key, POST /_test/prekill stops accepting /hit (a new one gets 503 service_overloaded, ERR-7)
// and waits for the held request; once the insert rolls back, the held request answers 200 and
// prekill replies listener_drops 0, log_drops 0 and log_queue_len 0, and /health shows
// log_queue_len 0 (plan H1 step 11).
func TestDesign_HitapiPrekill(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	rollback := s.holdKey(t, "pk", "held")

	held := api.Go(t, http.MethodGet, "/hit", IDs("pk", "held"), "")
	s.waitLockWaiters(t, 1)

	prekill := api.Go(t, http.MethodPost, "/_test/prekill", nil, "")
	// A /hit sent before prekill stopped accepting is admitted and answers 200 at once (its key
	// is free); once prekill has stopped accepting, a new /hit gets 503 service_overloaded.
	var refused Response
	for i, deadline := 0, time.Now().Add(5*time.Second); ; i++ {
		r := api.Hit(t, "pk", "probe-"+string(rune('a'+i%26))+strings.Repeat("x", i/26))
		if r.Status == http.StatusServiceUnavailable {
			refused = r
			break
		}
		if r.Status != http.StatusOK || time.Now().After(deadline) {
			t.Fatalf("a /hit after POST /_test/prekill: %d %s, want 200 until prekill stops accepting, then 503", r.Status, r.Body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if refused.JSON["error"] != "service_overloaded" {
		t.Errorf("the refused /hit: %s, want error service_overloaded", refused.Body)
	}
	select {
	case a := <-prekill:
		t.Fatalf("prekill answered %d %s (%v) while a /hit was in flight", a.Status, a.Body, a.Err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := rollback(); err != nil {
		t.Fatalf("rolling back the held key: %v", err)
	}
	if r := Wait(t, held, 10*time.Second, "the held /hit"); r.Status != http.StatusOK || r.JSON["existed"] != false {
		t.Errorf("the held /hit after the rollback: %d %s, want 200 with existed false", r.Status, r.Body)
	}
	r := Wait(t, prekill, 10*time.Second, "prekill")
	if r.Status != http.StatusOK || r.JSON == nil {
		t.Fatalf("prekill: %d %s, want 200 with a JSON reply", r.Status, r.Body)
	}
	if got := keys(r.JSON); !slices.Equal(got, []string{"listener_drops", "log_drops", "log_queue_len"}) {
		t.Errorf("prekill reply keys %v, want listener_drops, log_drops, log_queue_len", got)
	}
	if Int(t, r.JSON, "listener_drops") != 0 || Int(t, r.JSON, "log_drops") != 0 || Int(t, r.JSON, "log_queue_len") != 0 {
		t.Errorf("prekill reply %s, want listener_drops 0, log_drops 0, log_queue_len 0", r.Body)
	}
	if h := api.Health(t); Int(t, h, "log_queue_len") != 0 {
		t.Errorf("/health log_queue_len %v after prekill, want 0", h["log_queue_len"])
	}
	// The held request's record was on the file when prekill answered.
	found := false
	for _, rec := range api.Records(t, "request") {
		if rec["user_id"] == "pk" && rec["request_id"] == "held" && Int(t, rec, "http_status") == 200 {
			found = true
		}
	}
	if !found {
		t.Error("the held request's request record is not on the run log after prekill answered")
	}
}

// TestDesign_HitapiBenchHasNoTestRoutes: the benchmark binary bin/bench/hitapi has no /_test
// routes (DESIGN §B6: compiled only with -tags rltest), so POST /_test/faults and POST
// /_test/prekill get 404, and its /health reports fixtures: false (§B5 "Construction").
func TestDesign_HitapiBenchHasNoTestRoutes(t *testing.T) {
	s := StartStack(t, StackOpts{Build: "bench"})
	api := s.APIs[0]
	if !strings.Contains(api.Cmdline, filepath.Join("bin", "bench", "hitapi")) {
		t.Fatalf("the stack runs %q, want bin/bench/hitapi", api.Cmdline)
	}
	for path, body := range map[string]string{
		"/_test/faults":  `{"target":"store","mode":"refuse","match":"acquire","count":1}`,
		"/_test/prekill": "",
	} {
		r := api.Do(t, http.MethodPost, path, nil, body)
		if r.Status != http.StatusNotFound || r.JSON["error"] != "not_found" {
			t.Errorf("POST %s on bin/bench/hitapi: %d %s, want 404 not_found", path, r.Status, r.Body)
		}
	}
	h := api.Health(t)
	if h["fixtures"] != false || h["status"] != "ready" {
		t.Errorf("bin/bench/hitapi /health: fixtures %v, status %v; want false and ready", h["fixtures"], h["status"])
	}
	if _, ok := h["listener_queue_len"]; ok {
		t.Errorf("bin/bench/hitapi /health has listener_queue_len %v, a test-build field", h["listener_queue_len"])
	}
	// The refusal fault was not armed: a /hit is allowed.
	if r := api.Hit(t, "bench", "r1"); r.Status != http.StatusOK {
		t.Errorf("a /hit on bin/bench/hitapi: %d %s, want 200", r.Status, r.Body)
	}
}

// TestDesign_StackInstances: StartStack with two instances, instance 2 with its own config
// ([api] max_in_flight = 3 merged onto the stack's, plan step 12): two hitapi processes on
// distinct ports, each ready when StartStack returns; their start records show max_in_flight
// 512 for instance 1 (harness.toml) and 3 for instance 2; both answer /hit with 200.
func TestDesign_StackInstances(t *testing.T) {
	s := StartStack(t, StackOpts{Instances: 2, PerInstanceConfig: map[int]string{2: "[api]\nmax_in_flight = 3\n"}})
	if len(s.APIs) != 2 {
		t.Fatalf("StartStack started %d hitapi processes, want 2", len(s.APIs))
	}
	a1, a2 := s.APIs[0], s.APIs[1]
	if a1.N != 1 || a2.N != 2 || a1.Addr == a2.Addr || a1.PID == a2.PID {
		t.Errorf("instances %d at %s (pid %d) and %d at %s (pid %d), want instances 1 and 2 as two processes on distinct ports",
			a1.N, a1.Addr, a1.PID, a2.N, a2.Addr, a2.PID)
	}
	for _, a := range s.APIs {
		if h := a.Health(t); h["status"] != "ready" {
			t.Errorf("hitapi %d's /health status %v right after StartStack, want ready", a.N, h["status"])
		}
	}
	for i, want := range []int64{512, 3} {
		a := s.APIs[i]
		start := a.WaitRecords(t, "lifecycle", func(rs []map[string]any) bool { return len(rs) >= 1 })[0]
		if start["event"] != "start" || Int(t, start, "max_in_flight") != want {
			t.Errorf("hitapi %d's first lifecycle record %v, want start with max_in_flight %d", a.N, start, want)
		}
		user := fmt.Sprintf("si-%d", a.N)
		wantCreated(t, fmt.Sprintf("a /hit on hitapi %d", a.N), a.Hit(t, user, "r1"), user, "r1")
	}
}

// TestDesign_StackHelperDSN: the stack's own database client runs on harness.toml's db_dsn
// with pool_max_conns of at least 4 (helperPool): a smaller or missing pool is raised to 4,
// while a larger one and the other keys stay; a DSN that is not a postgres:// URL is refused
// with an error that does not quote it (it may hold a password).
func TestDesign_StackHelperDSN(t *testing.T) {
	const b2 = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64"
	for _, c := range []struct{ in, want string }{
		{b2, b2},
		{"postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=4",
			"postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=4"},
		{"postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=1",
			"postgres://rl@127.0.0.1:5433/hits?pool_max_conns=4&statement_timeout=2500"},
		{"postgresql://rl@127.0.0.1:5433/hits", "postgresql://rl@127.0.0.1:5433/hits?pool_max_conns=4"},
	} {
		if got, err := helperDSN(c.in); err != nil || got != c.want {
			t.Errorf("helperDSN(%s) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"host=127.0.0.1 port=5433 user=rl password=pw-123 dbname=hits",
		"mysql://rl:pw-123@127.0.0.1/hits",
		"postgres://rl:pw-123@[::1/hits",
	} {
		if _, err := helperDSN(bad); err == nil || strings.Contains(err.Error(), "pw-123") {
			t.Errorf("helperDSN(a DSN that is no postgres:// URL) = %v, want an error that does not quote it", err)
		}
	}
}

// TestDesign_StackEditedConfig: a stack whose Edit changes P, drain_timeout and db_dsn (plan
// step 12). The hitapi's SIGKILL grace is that config's P + drain_timeout + 5 s = 3 s + 20 s
// + 5 s = 28 s, and its start record shows the edited P and drain_timeout. The stack's own
// database client keeps harness.toml's db_dsn: its session has harness.toml's
// statement_timeout (2500ms), not the edited 2 s; and with hitapi's pool cut to one
// connection (pool_max_conns=1), the helper still holds a key and queries beside it, so the
// /hit blocked on the held key is seen waiting for the lock within 5 s, and answers 200 after
// the rollback.
func TestDesign_StackEditedConfig(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) {
		f.Instance.MaxReleaseDelayNs = 3_000_000_000
		f.Instance.DrainTimeoutNs = 20_000_000_000
		dsn := strings.Replace(f.API.DBDSN, "statement_timeout=2500&pool_max_conns=64", "statement_timeout=2000&pool_max_conns=1", 1)
		if dsn == f.API.DBDSN {
			t.Fatal("harness.toml's db_dsn does not end in statement_timeout=2500&pool_max_conns=64, to edit")
		}
		f.API.DBDSN = dsn
	}})
	api := s.APIs[0]
	if api.grace != 28*time.Second {
		t.Errorf("hitapi's SIGKILL grace is %v, want P + drain_timeout + 5 s = 28 s for the edited config", api.grace)
	}
	start := api.WaitRecords(t, "lifecycle", func(rs []map[string]any) bool { return len(rs) >= 1 })[0]
	if Int(t, start, "max_release_delay_ns") != 3_000_000_000 || Int(t, start, "drain_timeout_ns") != 20_000_000_000 {
		t.Errorf("start record %v, want the edited max_release_delay_ns 3 s and drain_timeout_ns 20 s", start)
	}
	ctx, cancel := context.WithTimeout(t.Context(), helperWait)
	got, err := s.DB.Settings(ctx, "statement_timeout")
	cancel()
	if err != nil || got["statement_timeout"] != "2500ms" {
		t.Errorf("the stack's own database client has statement_timeout %q (%v), want harness.toml's 2500ms, not the edited 2s",
			got["statement_timeout"], err)
	}

	rollback := s.holdKey(t, "ec", "held")
	held := api.Go(t, http.MethodGet, "/hit", IDs("ec", "held"), "")
	s.waitLockWaiters(t, 1)
	if n := s.rows(t); n != 0 {
		t.Errorf("%d rows while the key is held, want 0", n)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rolling back the held key: %v", err)
	}
	wantCreated(t, "the held /hit", Wait(t, held, 10*time.Second, "the held /hit"), "ec", "held")
	if n := s.keyRows(t, "ec", "held"); n != 1 {
		t.Errorf("rows for (ec, held) = %d, want 1", n)
	}
}

// TestDesign_StackReadyDeadline: StartStack's wait for a hitapi to be ready ends 10 s after it
// began (plan H1 step 12: "waits ≤ 10 s for status = ready"), even while a /health probe is
// still waiting for its answer, and a ready that comes after that is not taken: here /health
// answers ready only 10.5 s after the wait began, so waitReady fails at 10 s.
func TestDesign_StackReadyDeadline(t *testing.T) {
	began := time.Now() // before waitReady sets its deadline
	late := began.Add(10*time.Second + 500*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(time.Until(late))
		defer timer.Stop()
		select {
		case <-timer.C:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ready"}`) // a probe that gave up reads nothing
		case <-r.Context().Done(): // the probe gave up
		}
	}))
	defer srv.Close()
	a := &API{URL: srv.URL, child: &child{done: make(chan struct{})}, stack: &Stack{HTTP: &http.Client{Timeout: clientTimeout}}}
	err := a.waitReady(t)
	took := time.Since(began)
	if err == nil {
		t.Fatalf("waitReady took the ready that /health answered %v after the wait began, past its 10 s", took)
	}
	if took < 10*time.Second || took >= 10*time.Second+500*time.Millisecond {
		t.Errorf("waitReady gave up %v after it began (%v), want at 10 s, before the late answer at 10.5 s", took, err)
	}
}

// TestDesign_StackReadyWaitsForStatus: StartStack's wait for a hitapi takes only the /health
// answer that means ready (plan H1 step 12: "waits ≤ 10 s for status = ready"): status ready,
// or on a stack started with NoEpoch, status not_ready with reason no_epoch. /health answers
// 200 whatever the status, so every other answer must keep it polling. A fake /health gives
// its answers in turn, then its last one again, and waitReady must return nil right after the
// first answer it may take: on a stack with an epoch, no_epoch (the NoEpoch stack's answer),
// settings_invalid and fingerprint_mismatch come first, so it returns after probe 4, at ready;
// on a NoEpoch stack, settings_invalid, ready (the other stack's answer) and
// fingerprint_mismatch come first, so it returns after probe 4, at no_epoch. A hitapi that has
// exited fails the wait with its Wait error before any probe, though /health would say ready.
func TestDesign_StackReadyWaitsForStatus(t *testing.T) {
	notReady := func(reason string) string { return `{"status":"not_ready","reason":"` + reason + `"}` }
	const ready = `{"status":"ready","reason":"ready"}`
	exited := errors.New("signal: killed") // the exited child's Wait error
	for _, c := range []struct {
		name    string
		noEpoch bool
		exited  bool
		answers []string
		probes  int64 // waitReady returns after this many probes
		ok      bool  // and returns nil
	}{
		{"epoch: ready after probe 4", false, false,
			[]string{notReady("no_epoch"), notReady("settings_invalid"), notReady("fingerprint_mismatch"), ready}, 4, true},
		{"NoEpoch: no_epoch after probe 4", true, false,
			[]string{notReady("settings_invalid"), ready, notReady("fingerprint_mismatch"), notReady("no_epoch")}, 4, true},
		{"exited: no probe", false, true, []string{ready}, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var probes, stray atomic.Int64 // GET /health requests, and any other request
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/health" {
					stray.Add(1)
					http.NotFound(w, r)
					return
				}
				n := min(probes.Add(1), int64(len(c.answers)))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, c.answers[n-1]) // a probe that gave up reads nothing
			}))
			defer srv.Close()
			ch := &child{done: make(chan struct{})}
			if c.exited {
				ch.waitErr = exited
				close(ch.done)
			}
			tr := &http.Transport{DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			a := &API{URL: srv.URL, child: ch, stack: &Stack{noEpoch: c.noEpoch, HTTP: &http.Client{Timeout: clientTimeout, Transport: tr}}}
			err := a.waitReady(t)
			if got := probes.Load(); got != c.probes || (err == nil) != c.ok {
				t.Errorf("waitReady returned %v after %d probes, want nil=%v after %d", err, got, c.ok, c.probes)
			}
			if c.exited && !errors.Is(err, exited) {
				t.Errorf("waitReady on an exited hitapi: %v, want its Wait error %q", err, exited)
			}
			if n := stray.Load(); n != 0 {
				t.Errorf("waitReady sent %d requests other than GET /health", n)
			}
		})
	}
}

// rawRequest sends requestLine (with a Host header and Connection: close) to a over a new
// loopback connection, so the request reaches hitapi exactly as written, and returns the
// response: its status, headers, body and, when the body is a JSON object, JSON.
func rawRequest(t *testing.T, a *API, requestLine string) Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", a.Addr)
	if err != nil {
		t.Fatalf("%s: %v", requestLine, err)
	}
	defer func() { _ = conn.Close() }() // the response is read in full below
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			t.Fatalf("%s: %v", requestLine, err)
		}
	}
	if _, err := io.WriteString(conn, requestLine+"\r\nHost: "+a.Addr+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("%s: sending: %v", requestLine, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("%s: reading the response: %v", requestLine, err)
	}
	defer func() { _ = resp.Body.Close() }() // read to the end below
	r := Response{Status: resp.StatusCode, Header: resp.Header}
	if r.Body, err = io.ReadAll(resp.Body); err != nil {
		t.Fatalf("%s: reading the body: %v", requestLine, err)
	}
	if obj, err := decodeObject(r.Body); err == nil {
		r.JSON = obj
	}
	return r
}

// TestDesign_HitapiEdgeRequests: the requests net/http or its ServeMux would answer outside
// the ERR table get bin/test/hitapi's JSON 404 not_found with Cache-Control: no-store
// (HARNESS HTTP-7, HTTP-8): "OPTIONS *" (net/http's own 200 without the header), and the
// unclean paths //hit and /x/../hit (the mux's redirect to /hit).
func TestDesign_HitapiEdgeRequests(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	for _, line := range []string{"OPTIONS * HTTP/1.1", "GET //hit HTTP/1.1", "GET /x/../hit HTTP/1.1"} {
		r := rawRequest(t, api, line)
		if r.Status != http.StatusNotFound || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d, Content-Type %q, Cache-Control %q; want 404, application/json, no-store (body %s)",
				line, r.Status, r.Header.Get("Content-Type"), r.Header.Get("Cache-Control"), r.Body)
			continue
		}
		if msg, _ := r.JSON["message"].(string); r.JSON["error"] != "not_found" || msg == "" {
			t.Errorf("%s: body %s, want the JSON error not_found with a message", line, r.Body)
		}
	}
}
