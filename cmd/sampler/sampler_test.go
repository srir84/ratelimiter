package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"ratelimiter/harness/runlog"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
	"ratelimiter/internal/testutil/golden"
)

// goldenSamples is the hand-written golden samples.jsonl (DESIGN §C5; plan H3 step 7).
const goldenSamples = "../../harness/runlog/testdata/golden/samples.jsonl"

// The store passwords the tests give the sampler: no line, error or log may show them.
const (
	opsPass   = "ops-pw-7c1f0b6e94d2a5"
	adminPass = "admin-pw-3a9d2e41c8f5b7"
)

// stubHealth is a hitapi /health body, written by hand from DESIGN §A5.11, §B8 and plan H1
// step 9: the library's Health, with the per-instance settings at harness.toml's values, and
// the API's own counters, as a benchmark build serves it.
const stubHealth = `{"status":"ready","reason":"ready","epoch_id":"5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f",` +
	`"clock_source":"store","instance":{"MaxReleaseDelayNs":5000000000,"MarginNs":1000000000,` +
	`"ClockRateBoundPPM":1000,"DependencyDeadlineNs":2000000000,"EpochRereadIntervalNs":1000000000,` +
	`"DrainTimeoutNs":35000000000,"MaxHeldReceipts":1152,"MaxPendingRefunds":1152,"ListenerQueueSize":10000,` +
	`"RefundWorkers":128,"RefundBackoffInitialNs":50000000,"RefundBackoffMaxNs":1000000000,"StoreConns":8,` +
	`"RefundConns":2,"CleanupBatch":1000,"CleanupPipelines":4,"ReclaimBatch":1000},"listener_drops":0,` +
	`"store_error":"","fixtures":false,"log_drops":0,"in_flight":3,"nofile_limit":8192,"log_queue_len":0}` + "\n"

// stubMetrics is part of a hitapi /metrics, written by hand in the Prometheus text form
// harness/httpapi writes (DESIGN §A5.11): TYPE lines, gauges, labelled counters (PRF-4's
// cleanup counts among them), and one histogram series with two of its buckets, +Inf, _sum
// and _count.
const stubMetrics = `# TYPE rl_slots_in_use gauge
rl_slots_in_use{kind="receipt"} 3
rl_slots_in_use{kind="refund"} 0
# TYPE rl_pending_refunds gauge
rl_pending_refunds 0
# TYPE rl_cleanup_lag_seconds gauge
rl_cleanup_lag_seconds 0.25
# TYPE rl_cleanup_passes_total counter
rl_cleanup_passes_total{status="complete"} 4
# TYPE rl_cleanup_entries_total counter
rl_cleanup_entries_total{action="removed"} 2
# TYPE rl_clock_lag_microseconds gauge
rl_clock_lag_microseconds 0
# TYPE rl_listener_drops_total counter
rl_listener_drops_total 0
# TYPE rl_acquire_results_total counter
rl_acquire_results_total{result="allowed"} 41
rl_acquire_results_total{result="limited"} 7
# TYPE rl_store_call_seconds histogram
rl_store_call_seconds_bucket{script="acquire",le="0.001"} 0
rl_store_call_seconds_bucket{script="acquire",le="0.002"} 30
rl_store_call_seconds_bucket{script="acquire",le="+Inf"} 48
rl_store_call_seconds_sum{script="acquire"} 0.0912
rl_store_call_seconds_count{script="acquire"} 48
# TYPE rl_tracked_buckets gauge
rl_tracked_buckets 12
# TYPE rl_live_markers gauge
rl_live_markers 1
# TYPE rl_quarantined_keys gauge
rl_quarantined_keys 0
`

// wantMetrics is stubMetrics read by hand: each series' value by its series text.
func wantMetrics() map[string]*float64 {
	return map[string]*float64{
		`rl_slots_in_use{kind="receipt"}`:                           num(3),
		`rl_slots_in_use{kind="refund"}`:                            num(0),
		`rl_pending_refunds`:                                        num(0),
		`rl_cleanup_lag_seconds`:                                    num(0.25),
		`rl_cleanup_passes_total{status="complete"}`:                num(4),
		`rl_cleanup_entries_total{action="removed"}`:                num(2),
		`rl_clock_lag_microseconds`:                                 num(0),
		`rl_listener_drops_total`:                                   num(0),
		`rl_acquire_results_total{result="allowed"}`:                num(41),
		`rl_acquire_results_total{result="limited"}`:                num(7),
		`rl_store_call_seconds_bucket{script="acquire",le="0.001"}`: num(0),
		`rl_store_call_seconds_bucket{script="acquire",le="0.002"}`: num(30),
		`rl_store_call_seconds_bucket{script="acquire",le="+Inf"}`:  num(48),
		`rl_store_call_seconds_sum{script="acquire"}`:               num(0.0912),
		`rl_store_call_seconds_count{script="acquire"}`:             num(48),
		`rl_tracked_buckets`:                                        num(12),
		`rl_live_markers`:                                           num(1),
		`rl_quarantined_keys`:                                       num(0),
	}
}

// The store's INFO sections, written by hand in the store's reply form (CRLF lines under a
// "# Section" header), and what the sampler must make of them.
const (
	persistenceInfo = "# Persistence\r\nloading:0\r\naof_enabled:1\r\naof_rewrite_in_progress:1\r\n" +
		"aof_rewrite_scheduled:0\r\naof_last_bgrewrite_status:ok\r\naof_last_write_status:ok\r\n"
	clientsInfo = "# Clients\r\nconnected_clients:38\r\nmaxclients:4096\r\nblocked_clients:0\r\n"
)

func wantPersistence() map[string]string {
	return map[string]string{"loading": "0", "aof_enabled": "1", "aof_rewrite_in_progress": "1",
		"aof_rewrite_scheduled": "0", "aof_last_bgrewrite_status": "ok", "aof_last_write_status": "ok"}
}

func wantClients() map[string]string {
	return map[string]string{"connected_clients": "38", "maxclients": "4096", "blocked_clients": "0"}
}

// statusJSON is `rladmin status --json` output, written by hand from cmd/rladmin's status
// fields (DESIGN §A6, SPEC ADM-4).
const statusJSON = `{"epoch_id":"5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f","fingerprint":"c41d8e7a9b2f06e35d1a4c7b8e9f0a12",` +
	`"hold":"","hwm_us":1791374400123456,"tracked_buckets":12,"live_markers":1,"quarantined_keys":0}`

func num(v float64) *float64 { return &v }

// setup is a test sampler's surroundings.
type setup struct {
	runDir, store, rladmin, pgData string
	apis                           []string
	timeout                        time.Duration
}

// newTestSampler returns a sampler of su writing samples.jsonl in su.runDir. Its environment
// base holds RL_ variables that rladmin must not get.
func newTestSampler(t *testing.T, su setup) *sampler {
	t.Helper()
	out, err := runlog.Open(filepath.Join(su.runDir, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := samplerConfig{runDir: su.runDir, apis: su.apis, rladmin: su.rladmin, pgData: su.pgData,
		interval: time.Minute, timeout: su.timeout, waitDelay: 100 * time.Millisecond}
	base := []string{"PATH=/usr/bin:/bin", envOpsPassword + "=" + opsPass, "RL_LIB_PASSWORD=lib-pw", "HOME=/nonexistent"}
	s := newSampler(cfg, newStoreAccess(su.store, opsPass, adminPass, base), newClient(su.timeout), out)
	t.Cleanup(func() { _ = out.Close() }) // a no-op once lines has closed it
	return s
}

// lines closes s's run log and returns samples.jsonl's lines; none may be dropped, and none
// may show a password.
func lines(t *testing.T, s *sampler) [][]byte {
	t.Helper()
	if err := s.out.Close(); err != nil {
		t.Fatal(err)
	}
	if d := s.out.Drops(); d != 0 {
		t.Errorf("samples.jsonl dropped %d lines", d)
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.runDir, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pw := range []string{opsPass, adminPass} {
		if bytes.Contains(data, []byte(pw)) {
			t.Errorf("samples.jsonl shows a password")
		}
	}
	if len(data) == 0 {
		return nil
	}
	return bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
}

// decode decodes one samples.jsonl line, refusing keys a sample does not have.
func decode(t *testing.T, line []byte) sample {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var l sample
	if err := dec.Decode(&l); err != nil {
		t.Fatalf("line %s: %v", line, err)
	}
	return l
}

// isNull reports whether a decoded JSON value was null (or absent).
func isNull(m json.RawMessage) bool { return len(m) == 0 || string(m) == "null" }

// sameJSON reports whether got and want hold equal JSON values.
func sameJSON(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

// errIs reports whether e is an error text holding every one of parts.
func errIs(e *string, parts ...string) bool {
	if e == nil {
		return false
	}
	for _, p := range parts {
		if !strings.Contains(*e, p) {
			return false
		}
	}
	return true
}

func show(e *string) string {
	if e == nil {
		return "<nil>"
	}
	return *e
}

// val shows a number a pointer holds, or null.
func val[T float64 | uint64](v *T) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

// stubAPI serves stubHealth at /health, or 503 while down is set, and stubMetrics at /metrics.
func stubAPI(t *testing.T, down *atomic.Bool) string {
	t.Helper()
	return serveAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if down.Load() {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, stubHealth)
		case "/metrics":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = io.WriteString(w, stubMetrics)
		default:
			http.NotFound(w, r)
		}
	})
}

// serveAPI starts an HTTP server with handler on a loopback port and returns its address.
func serveAPI(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// writePIDs writes dir's pids.json.
func writePIDs(t *testing.T, dir string, procs ...runlog.Proc) {
	t.Helper()
	if err := runlog.WritePIDs(filepath.Join(dir, runlog.PIDsFile), runlog.PIDs{Procs: procs}); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// listenLoopback listens on a free loopback port; cleanup closes the listener.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// droppingStore returns the address of a listener that closes each connection it accepts at
// once: a store the sampler cannot read.
func droppingStore(t *testing.T) string {
	t.Helper()
	ln := listenLoopback(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return ln.Addr().String()
}

// silentStore returns the address of a listener that accepts connections and never answers on
// them: a stuck store. Cleanup closes them.
func silentStore(t *testing.T) string {
	t.Helper()
	ln := listenLoopback(t)
	conns := make(chan net.Conn, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case conns <- c:
			default:
				_ = c.Close()
			}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		close(conns)
		for c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// startChild starts a process of its own group that cleanup kills, group and all; it returns
// the PID once the process has printed its first line. The command's shell must print one.
func startChild(t *testing.T, script string) int {
	t.Helper()
	pid, _ := startChildLine(t, script)
	return pid
}

// startChildLine is startChild that also returns the first line the process printed, without
// its newline.
func startChildLine(t *testing.T, script string) (int, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("starting %q: %q, %v", script, line, err)
	}
	return cmd.Process.Pid, strings.TrimSuffix(line, "\n")
}

// psRSS is pid's resident set size in bytes as ps shows it (in KiB), a source independent of
// gopsutil.
func psRSS(t *testing.T, pid int) uint64 {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/bin/ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps -o rss= -p %d: %v", pid, err)
	}
	kib, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || kib == 0 {
		t.Fatalf("ps -o rss= -p %d printed %q (%v), want a size in KiB", pid, out, err)
	}
	return kib * 1024
}

// tickBounded runs one tick of s and fails the test unless it ends within bound. A tick still
// running then has a read waiting on one of fifos: each is opened to write and closed again,
// which ends a reader's wait (it reads the end of the file), until the tick ends, so its
// goroutines do not outlive the test.
func tickBounded(t *testing.T, s *sampler, bound time.Duration, fifos ...string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.tick(t.Context())
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	giveUp := time.Now().Add(10 * time.Second)
	for {
		for _, path := range fifos {
			if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
		}
		select {
		case <-done:
			t.Fatalf("the tick still ran %v after it started, want it ended by its deadline: a read waited on a FIFO", bound)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(giveUp) {
			t.Fatalf("the tick still ran %v after it started, and 10 s after its FIFOs were released", bound)
		}
	}
}

// TestSampler_Tick (plan H3 step 7): with a stubbed /metrics and /health and the test's own
// PID in pids.json, a tick writes one line with the process's cpu and rss and the instance's
// metrics and health, and one error for each read the test leaves unavailable (rladmin status,
// INFO as rl_ops, the postmaster); a second tick with /health down records its error. The two
// lines match the hand-written golden samples.jsonl by position. t_ns is the wall-clock time
// in ns while the tick ran, and rss is in bytes: within a factor of 2 of what ps shows for the
// process (KiB × 1024) before and after the ticks.
func TestSampler_Tick(t *testing.T) {
	var down atomic.Bool
	api := stubAPI(t, &down)
	runDir := t.TempDir()
	writePIDs(t, runDir, runlog.Proc{Role: runlog.RoleHitAPI, Instance: 1, PID: os.Getpid()})
	s := newTestSampler(t, setup{runDir: runDir, apis: []string{api}, store: droppingStore(t),
		rladmin: filepath.Join(t.TempDir(), "rladmin"), pgData: t.TempDir(), timeout: 10 * time.Second})

	rssBefore := psRSS(t, os.Getpid())
	s.prime(t.Context())
	var wall [2][2]int64 // each tick's wall-clock ns just before it and just after it
	for i := range wall {
		down.Store(i == 1) // /health down on the second tick
		wall[i][0] = time.Now().UnixNano()
		s.tick(t.Context())
		wall[i][1] = time.Now().UnixNano()
	}
	rssAfter := psRSS(t, os.Getpid())
	rssLow, rssHigh := min(rssBefore, rssAfter)/2, max(rssBefore, rssAfter)*2
	got := lines(t, s)
	if len(got) != 2 {
		t.Fatalf("samples.jsonl has %d lines, want 2", len(got))
	}
	golden.Match(t, got, goldenSamples)

	ticks := []sample{decode(t, got[0]), decode(t, got[1])}
	for i, l := range ticks {
		if l.ProcsError != nil || len(l.Procs) != 1 {
			t.Fatalf("tick %d: procs %+v, procs_error %s; want the one pids.json process", i+1, l.Procs, show(l.ProcsError))
		}
		p := l.Procs[0]
		if p.Role != runlog.RoleHitAPI || p.Instance != 1 || p.PID != os.Getpid() || p.Error != nil {
			t.Errorf("tick %d: process %+v (error %s), want hitapi 1 with pid %d and no error", i+1, p, show(p.Error), os.Getpid())
		}
		if p.CPU == nil || *p.CPU < 0 {
			t.Errorf("tick %d: cpu %s, want a percentage: the process was read on the tick before", i+1, val(p.CPU))
		}
		if p.RSS == nil || *p.RSS == 0 || *p.RSS < rssLow || *p.RSS > rssHigh {
			t.Errorf("tick %d: rss %s, want the process's resident bytes: %d to %d, within a factor of 2 of ps's %d and %d",
				i+1, val(p.RSS), rssLow, rssHigh, rssBefore, rssAfter)
		}
		if l.TNs < wall[i][0] || l.TNs > wall[i][1] {
			t.Errorf("tick %d: t_ns %d, want wall-clock ns from %d to %d, while the tick ran", i+1, l.TNs, wall[i][0], wall[i][1])
		}
		if l.Postgres != nil || !errIs(l.PostgresError, "postmaster.pid") {
			t.Errorf("tick %d: postgres %+v, error %s; want the missing postmaster.pid's error", i+1, l.Postgres, show(l.PostgresError))
		}
		if !isNull(l.Status) || !errIs(l.StatusError, "rladmin status", "no such file") {
			t.Errorf("tick %d: status %s, error %s; want the missing rladmin's error", i+1, l.Status, show(l.StatusError))
		}
		if l.Store != nil || !errIs(l.StoreError, "INFO", "as rl_ops") {
			t.Errorf("tick %d: store %+v, error %s; want the unreadable store's error, as rl_ops", i+1, l.Store, show(l.StoreError))
		}
		if len(l.APIs) != 1 {
			t.Fatalf("tick %d: %d apis, want 1", i+1, len(l.APIs))
		}
		a := l.APIs[0]
		if a.Instance != 1 || a.Addr != api {
			t.Errorf("tick %d: api instance %d at %s, want 1 at %s", i+1, a.Instance, a.Addr, api)
		}
		if a.MetricsError != nil || !reflect.DeepEqual(a.Metrics, wantMetrics()) {
			t.Errorf("tick %d: metrics %v, error %s; want stubMetrics' series", i+1, a.Metrics, show(a.MetricsError))
		}
	}
	first, second := ticks[0].APIs[0], ticks[1].APIs[0]
	if first.HealthError != nil || !sameJSON(t, first.Health, stubHealth) {
		t.Errorf("tick 1: health %s, error %s; want stubHealth as served", first.Health, show(first.HealthError))
	}
	if !isNull(second.Health) || !errIs(second.HealthError, "/health", "503") {
		t.Errorf("tick 2: health %s, error %s; want /health's 503", second.Health, show(second.HealthError))
	}
	if ticks[0].TNs <= 0 || ticks[1].TNs <= ticks[0].TNs {
		t.Errorf("t_ns %d then %d, want wall-clock ns that increase", ticks[0].TNs, ticks[1].TNs)
	}
}

// fakeRladmin is a stand-in for `rladmin status --json`: it prints statusJSON when it gets
// exactly those arguments, rl_admin's password and the store address, and no other RL_
// variable; otherwise it fails.
func fakeRladmin(storeAddr string) string {
	return fmt.Sprintf(`#!/bin/sh
if [ "$#" -ne 2 ] || [ "$1" != status ] || [ "$2" != --json ]; then
	echo "rladmin: bad arguments: $*" >&2
	exit 2
fi
if [ "$RL_ADMIN_PASSWORD" != '%s' ] || [ "$RL_STORE_ADDR" != '%s' ] || [ -n "${RL_OPS_PASSWORD+set}" ] || [ -n "${RL_LIB_PASSWORD+set}" ]; then
	echo "rladmin: wrong environment" >&2
	exit 2
fi
printf '%%s\n' '%s'
`, adminPass, storeAddr, statusJSON)
}

// TestSampler_Reads: with every source up, one tick records each read with no error: the
// postmaster (PID from postmaster.pid) with its child, CPU % and RSS summed over both (the
// child spins: the sum's CPU % is 5 or more over the 300 ms since the first reading, which the
// waiting postmaster alone does not reach, even on a loaded machine; its RSS at least the
// postmaster's plus half its child's, as ps shows them); rladmin status --json as rladmin
// printed it, run with RL_ADMIN_PASSWORD and RL_STORE_ADDR and no other RL_ variable; and INFO
// persistence and INFO clients read as rl_ops. A pids.json process without an instance has no
// instance key.
func TestSampler_Reads(t *testing.T) {
	store := fakeresp.Start(t, "rl_ops", opsPass)
	store.ReplyValue("INFO", resp.Value{Kind: resp.Bulk, Str: persistenceInfo}, 1)
	store.ReplyValue("INFO", resp.Value{Kind: resp.Bulk, Str: clientsInfo}, 1)
	postmaster, line := startChildLine(t, "while :; do :; done & echo $!; wait")
	backend, err := strconv.Atoi(line)
	if err != nil {
		t.Fatalf("the postmaster's child: %q is no PID", line)
	}
	pgData := t.TempDir()
	writeFile(t, filepath.Join(pgData, "postmaster.pid"), fmt.Sprintf("%d\n%s\n1791374400\n5433\n", postmaster, pgData), 0o600)
	rladmin := filepath.Join(t.TempDir(), "rladmin")
	writeFile(t, rladmin, fakeRladmin(store.Addr()), 0o700)
	runDir := t.TempDir()
	writePIDs(t, runDir, runlog.Proc{Role: runlog.RoleValkey, PID: os.Getpid()})
	s := newTestSampler(t, setup{runDir: runDir, apis: []string{stubAPI(t, &atomic.Bool{})}, store: store.Addr(),
		rladmin: rladmin, pgData: pgData, timeout: 10 * time.Second})

	s.prime(t.Context())
	time.Sleep(300 * time.Millisecond) // the spinning child's CPU time to sum
	s.tick(t.Context())
	pmRSS, backendRSS := psRSS(t, postmaster), psRSS(t, backend)
	got := lines(t, s)
	if len(got) != 1 {
		t.Fatalf("samples.jsonl has %d lines, want 1", len(got))
	}
	l := decode(t, got[0])
	if pg := l.Postgres; l.PostgresError != nil || pg == nil || pg.PID != postmaster || pg.Processes != 2 || pg.CPU == nil ||
		*pg.CPU < 0 || pg.RSS == 0 {
		t.Errorf("postgres %+v, error %s; want pid %d with its one child (2 processes), a CPU %% and an RSS",
			pg, show(l.PostgresError), postmaster)
	}
	if pg := l.Postgres; pg != nil {
		if pg.RSS < pmRSS+backendRSS/2 {
			t.Errorf("postgres rss %d, want the postmaster's %d bytes plus its child's %d (ps), at least half of the child's",
				pg.RSS, pmRSS, backendRSS)
		}
		if pg.CPU == nil || *pg.CPU < 5 {
			t.Errorf("postgres cpu %s, want 5 or more: the postmaster's child spins", val(pg.CPU))
		}
	}
	if l.StatusError != nil || !sameJSON(t, l.Status, statusJSON) {
		t.Errorf("status %s, error %s; want rladmin's output %s", l.Status, show(l.StatusError), statusJSON)
	}
	if l.StoreError != nil || l.Store == nil || !reflect.DeepEqual(l.Store.Persistence, wantPersistence()) ||
		!reflect.DeepEqual(l.Store.Clients, wantClients()) {
		t.Errorf("store %+v, error %s; want the two INFO sections", l.Store, show(l.StoreError))
	}
	var sent [][]string
	for _, c := range store.Commands() {
		sent = append(sent, c.Args)
	}
	if want := [][]string{{"AUTH", "rl_ops", opsPass}, {"INFO", "persistence"}, {"INFO", "clients"}}; !reflect.DeepEqual(sent, want) {
		t.Errorf("the store got %v, want AUTH as rl_ops with its password, INFO persistence, INFO clients", store.Commands())
	}
	if l.ProcsError != nil || len(l.Procs) != 1 || l.Procs[0].Role != runlog.RoleValkey || l.Procs[0].Error != nil {
		t.Errorf("procs %+v, error %s; want the valkey-server entry", l.Procs, show(l.ProcsError))
	}
	if bytes.Contains(got[0], []byte(`"instance":0`)) {
		t.Errorf("a process without an instance shows one: %s", got[0])
	}
}

// TestSampler_ReadErrors: each read that fails records its own error, the value null, and the
// line is still written: pids.json that does not validate, a postmaster.pid whose first line is
// no PID, rladmin exiting 3 (its stderr quoted without the password it showed), an INFO
// refused by the store, a /health that is no JSON object, and a /metrics line that is no
// "series value". A second tick has a FIFO at pids.json and at postmaster.pid, with no writer
// (neither read waits for one: the line is written within the tick's deadline), and an rladmin
// that exits 0 having printed text that is no JSON object.
func TestSampler_ReadErrors(t *testing.T) {
	store := fakeresp.Start(t, "rl_ops", opsPass)
	store.ReplyError("INFO", "NOPERM User rl_ops has no permissions to run the 'info' command", 1)
	pgData := t.TempDir()
	writeFile(t, filepath.Join(pgData, "postmaster.pid"), "not-a-pid\n/data\n", 0o600)
	rladmin := filepath.Join(t.TempDir(), "rladmin")
	writeFile(t, rladmin, "#!/bin/sh\necho \"rladmin: store error: AUTH as rl_admin with $RL_ADMIN_PASSWORD refused\" >&2\nexit 3\n", 0o700)
	runDir := t.TempDir()
	writeFile(t, filepath.Join(runDir, runlog.PIDsFile), `{"procs":[{"role":"postgres","pid":7}]}`, 0o600)
	api := serveAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = io.WriteString(w, `["ready"]`)
			return
		}
		_, _ = io.WriteString(w, "# TYPE rl_tracked_buckets gauge\nrl_tracked_buckets twelve\n")
	})
	s := newTestSampler(t, setup{runDir: runDir, apis: []string{api}, store: store.Addr(), rladmin: rladmin,
		pgData: pgData, timeout: 10 * time.Second})

	s.tick(t.Context())
	got := lines(t, s)
	if len(got) != 1 {
		t.Fatalf("samples.jsonl has %d lines, want 1", len(got))
	}
	l := decode(t, got[0])
	if l.Procs != nil || !errIs(l.ProcsError, runlog.PIDsFile) {
		t.Errorf("procs %+v, error %s; want null and the pids.json error", l.Procs, show(l.ProcsError))
	}
	if l.Postgres != nil || !errIs(l.PostgresError, "first line", "not-a-pid") {
		t.Errorf("postgres %+v, error %s; want null and the postmaster.pid error", l.Postgres, show(l.PostgresError))
	}
	if !isNull(l.Status) || !errIs(l.StatusError, "rladmin status", "exit status 3", "store error", "[redacted]") {
		t.Errorf("status %s, error %s; want null and rladmin's exit with its stderr", l.Status, show(l.StatusError))
	}
	if l.Store != nil || !errIs(l.StoreError, "INFO persistence", "NOPERM") {
		t.Errorf("store %+v, error %s; want null and the refusal", l.Store, show(l.StoreError))
	}
	a := l.APIs[0]
	if !isNull(a.Health) || !errIs(a.HealthError, "/health", "not a JSON object") {
		t.Errorf("health %s, error %s; want null and the body's error", a.Health, show(a.HealthError))
	}
	if a.Metrics != nil || !errIs(a.MetricsError, "/metrics", "line 2", "not a number") {
		t.Errorf("metrics %v, error %s; want null and the bad line's error", a.Metrics, show(a.MetricsError))
	}

	fifoRun, fifoPG := t.TempDir(), t.TempDir()
	fifos := []string{filepath.Join(fifoRun, runlog.PIDsFile), filepath.Join(fifoPG, "postmaster.pid")}
	for _, path := range fifos {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	textRladmin := filepath.Join(t.TempDir(), "rladmin")
	writeFile(t, textRladmin, "#!/bin/sh\necho 'epoch 5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f (text output)'\n", 0o700)
	const timeout = 2 * time.Second
	second := newTestSampler(t, setup{runDir: fifoRun, store: droppingStore(t), rladmin: textRladmin, pgData: fifoPG, timeout: timeout})
	tickBounded(t, second, timeout+second.cfg.waitDelay+2*time.Second, fifos...)
	got = lines(t, second)
	if len(got) != 1 {
		t.Fatalf("second tick: samples.jsonl has %d lines, want 1", len(got))
	}
	l = decode(t, got[0])
	if l.Procs != nil || !errIs(l.ProcsError, runlog.PIDsFile, "not a regular file") {
		t.Errorf("a FIFO pids.json: procs %+v, error %s; want null and an error: not a regular file", l.Procs, show(l.ProcsError))
	}
	if l.Postgres != nil || !errIs(l.PostgresError, "postmaster.pid", "not a regular file") {
		t.Errorf("a FIFO postmaster.pid: postgres %+v, error %s; want null and an error: not a regular file", l.Postgres,
			show(l.PostgresError))
	}
	if !isNull(l.Status) || !errIs(l.StatusError, "rladmin status --json", "not a JSON object") {
		t.Errorf("rladmin's text output: status %s, error %s; want null and an error: not a JSON object", l.Status, show(l.StatusError))
	}
}

// TestSampler_Deadline: a tick never runs into the next one. With /health, rladmin and the
// store all stuck, each of those reads ends at the tick's deadline with its error, /metrics
// still answers, and the line is written by the deadline plus rladmin's wait delay, with room
// for a loaded machine.
func TestSampler_Deadline(t *testing.T) {
	api := serveAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			_, _ = io.WriteString(w, stubMetrics)
			return
		}
		select { // stuck until the sampler gives up
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
	rladmin := filepath.Join(t.TempDir(), "rladmin")
	writeFile(t, rladmin, "#!/bin/sh\nexec sleep 30\n", 0o700)
	runDir := t.TempDir()
	writePIDs(t, runDir, runlog.Proc{Role: runlog.RoleHitAPI, Instance: 1, PID: os.Getpid()})
	const timeout = 400 * time.Millisecond
	s := newTestSampler(t, setup{runDir: runDir, apis: []string{api}, store: silentStore(t), rladmin: rladmin,
		pgData: t.TempDir(), timeout: timeout})

	start := time.Now()
	s.tick(t.Context())
	if took := time.Since(start); took < timeout || took > timeout+s.cfg.waitDelay+2*time.Second {
		t.Errorf("the tick took %v, want the %v deadline (plus rladmin's %v wait delay and slack)", took, timeout, s.cfg.waitDelay)
	}
	got := lines(t, s)
	if len(got) != 1 {
		t.Fatalf("samples.jsonl has %d lines, want 1", len(got))
	}
	l := decode(t, got[0])
	a := l.APIs[0]
	if !isNull(a.Health) || a.HealthError == nil {
		t.Errorf("health %s, error %s; want null and the deadline's error", a.Health, show(a.HealthError))
	}
	if a.MetricsError != nil || !reflect.DeepEqual(a.Metrics, wantMetrics()) {
		t.Errorf("metrics %v, error %s; want stubMetrics' series", a.Metrics, show(a.MetricsError))
	}
	if !isNull(l.Status) || !errIs(l.StatusError, "rladmin status", "deadline exceeded") {
		t.Errorf("status %s, error %s; want null and the deadline's error", l.Status, show(l.StatusError))
	}
	if l.Store != nil || l.StoreError == nil {
		t.Errorf("store %+v, error %s; want null and the deadline's error", l.Store, show(l.StoreError))
	}
	if l.ProcsError != nil || len(l.Procs) != 1 || l.Procs[0].Error != nil {
		t.Errorf("procs %+v, error %s; want the process read", l.Procs, show(l.ProcsError))
	}
}

// TestSampler_CPUFollowsPID: pids.json is re-read every tick, and CPU % covers the interval
// since the same process's reading on the tick before. A PID new to the list has no CPU % on
// its first tick and has one on the next; a PID the previous tick did not read starts over.
func TestSampler_CPUFollowsPID(t *testing.T) {
	runDir := t.TempDir()
	self := runlog.Proc{Role: runlog.RoleHitAPI, Instance: 1, PID: os.Getpid()}
	writePIDs(t, runDir, self)
	s := newTestSampler(t, setup{runDir: runDir, store: droppingStore(t), rladmin: filepath.Join(t.TempDir(), "rladmin"),
		pgData: t.TempDir(), timeout: 10 * time.Second})
	child := startChild(t, "echo started; exec sleep 60")

	s.prime(t.Context())
	s.tick(t.Context()) // 1: self, read at prime
	writePIDs(t, runDir, runlog.Proc{Role: runlog.RoleHitAPI, Instance: 1, PID: child})
	s.tick(t.Context()) // 2: the new process, first read
	s.tick(t.Context()) // 3: the new process, read on tick 2
	writePIDs(t, runDir, self)
	s.tick(t.Context()) // 4: self again, not read on tick 3
	got := lines(t, s)
	if len(got) != 4 {
		t.Fatalf("samples.jsonl has %d lines, want 4", len(got))
	}
	for i, c := range []struct {
		pid    int
		hasCPU bool
	}{{os.Getpid(), true}, {child, false}, {child, true}, {os.Getpid(), false}} {
		l := decode(t, got[i])
		if len(l.Procs) != 1 {
			t.Fatalf("tick %d: procs %+v, error %s", i+1, l.Procs, show(l.ProcsError))
		}
		p := l.Procs[0]
		if p.PID != c.pid || (p.CPU != nil) != c.hasCPU || p.RSS == nil || p.Error != nil {
			t.Errorf("tick %d: pid %d cpu %s rss %s error %s; want pid %d, a CPU %% %v, an RSS", i+1, p.PID, val(p.CPU), val(p.RSS),
				show(p.Error), c.pid, c.hasCPU)
		}
	}
}

// TestSampler_ExitedProcess: a pids.json process that has exited records an error, never
// zeros: a zombie (exited and not reaped yet), whose task info gopsutil reads as all zeros on
// macOS without an error, and a PID that names no process at all.
func TestSampler_ExitedProcess(t *testing.T) {
	zombie := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	stdout, err := zombie.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zombie.Wait() })
	if _, err := io.ReadAll(stdout); err != nil { // EOF once the shell has exited
		t.Fatal(err)
	}
	waitZombie(t, zombie.Process.Pid)
	gone := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	if err := gone.Run(); err != nil { // exited and reaped
		t.Fatal(err)
	}
	runDir := t.TempDir()
	writePIDs(t, runDir, runlog.Proc{Role: runlog.RoleValkey, PID: zombie.Process.Pid},
		runlog.Proc{Role: runlog.RoleHitgen, PID: gone.Process.Pid})
	s := newTestSampler(t, setup{runDir: runDir, store: droppingStore(t), rladmin: filepath.Join(t.TempDir(), "rladmin"),
		pgData: t.TempDir(), timeout: 10 * time.Second})

	s.tick(t.Context())
	got := lines(t, s)
	if len(got) != 1 {
		t.Fatalf("samples.jsonl has %d lines, want 1", len(got))
	}
	l := decode(t, got[0])
	if l.ProcsError != nil || len(l.Procs) != 2 {
		t.Fatalf("procs %+v, error %s; want the two pids.json processes", l.Procs, show(l.ProcsError))
	}
	for i, want := range []string{"no task info", "does not exist"} {
		if p := l.Procs[i]; p.CPU != nil || p.RSS != nil || !errIs(p.Error, want) {
			t.Errorf("process %s (pid %d): cpu %s rss %s error %s; want no stats and an error with %q", p.Role, p.PID, val(p.CPU),
				val(p.RSS), show(p.Error), want)
		}
	}
}

// waitZombie waits, at most 5 s, until ps shows pid as a zombie.
func waitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, err := process.NewProcessWithContext(t.Context(), int32(pid))
		if err == nil {
			if st, err := p.StatusWithContext(t.Context()); err == nil && slices.Contains(st, process.Zombie) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d: not a zombie after 5 s", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSampler_Instances: each --apis address is its own instance, in order: entry i of apis is
// instance i+1 at the i-th address, with that instance's own /health and /metrics. Instance 1
// serves stubHealth and stubMetrics; instance 2 has /health down and other metrics.
func TestSampler_Instances(t *testing.T) {
	one := stubAPI(t, &atomic.Bool{})
	two := serveAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			_, _ = io.WriteString(w, "# TYPE rl_tracked_buckets gauge\nrl_tracked_buckets 7\n")
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	runDir := t.TempDir()
	writePIDs(t, runDir)
	s := newTestSampler(t, setup{runDir: runDir, apis: []string{one, two}, store: droppingStore(t),
		rladmin: filepath.Join(t.TempDir(), "rladmin"), pgData: t.TempDir(), timeout: 10 * time.Second})

	s.tick(t.Context())
	got := lines(t, s)
	if len(got) != 1 {
		t.Fatalf("samples.jsonl has %d lines, want 1", len(got))
	}
	l := decode(t, got[0])
	if len(l.APIs) != 2 {
		t.Fatalf("apis %+v, want the 2 instances", l.APIs)
	}
	a1, a2 := l.APIs[0], l.APIs[1]
	if a1.Instance != 1 || a1.Addr != one || a1.HealthError != nil || isNull(a1.Health) || !sameJSON(t, a1.Health, stubHealth) ||
		a1.MetricsError != nil || !reflect.DeepEqual(a1.Metrics, wantMetrics()) {
		t.Errorf("apis[0] %+v (health error %s, metrics error %s); want instance 1 at %s with stubHealth and stubMetrics' series",
			a1, show(a1.HealthError), show(a1.MetricsError), one)
	}
	if a2.Instance != 2 || a2.Addr != two || !isNull(a2.Health) || !errIs(a2.HealthError, "http://"+two+"/health", "503") ||
		a2.MetricsError != nil || !reflect.DeepEqual(a2.Metrics, map[string]*float64{"rl_tracked_buckets": num(7)}) {
		t.Errorf("apis[1] %+v (health error %s, metrics error %s); want instance 2 at %s with its /health's 503 and its one series",
			a2, show(a2.HealthError), show(a2.MetricsError), two)
	}
}

// TestSampler_CPUPerTick: a process's CPU % is its user plus system CPU time over the interval
// since its reading on the tick before: neither an average since the process started nor one
// since it was first read. The bounds are relative to what the spinning shell got on tick 1,
// so a loaded machine, which gives every busy process less, moves them all together: tick 1
// must show the shell spinning (5 % or more, a precondition); stopped (SIGSTOP), it reads at
// most a quarter of its tick 1 over the next second (an average since it started, or since its
// first reading, gives about half). dd copying one byte at a time, which spends most of its
// time in the kernel, reads at least half the shell's tick 1 (its user time alone gives about
// a sixth). Each interval lasts 1 s: under heavy load, two busy processes' shares of 300 ms
// differ by a factor of 3, while their shares of a second stay within about 20 %.
func TestSampler_CPUPerTick(t *testing.T) {
	spin := startChild(t, "echo started; while :; do :; done")
	dd := startChild(t, "echo started; exec dd if=/dev/zero of=/dev/null bs=1")
	runDir := t.TempDir()
	writePIDs(t, runDir, runlog.Proc{Role: runlog.RoleHitAPI, Instance: 1, PID: spin}, runlog.Proc{Role: runlog.RoleHitgen, PID: dd})
	s := newTestSampler(t, setup{runDir: runDir, store: droppingStore(t), rladmin: filepath.Join(t.TempDir(), "rladmin"),
		pgData: t.TempDir(), timeout: 10 * time.Second})

	s.prime(t.Context())
	time.Sleep(time.Second)
	s.tick(t.Context()) // 1: both busy since the first reading
	if err := syscall.Kill(spin, syscall.SIGSTOP); err != nil {
		t.Fatalf("stopping the spinning shell: %v", err)
	}
	time.Sleep(time.Second)
	s.tick(t.Context()) // 2: the spinning shell stopped since tick 1
	got := lines(t, s)
	if len(got) != 2 {
		t.Fatalf("samples.jsonl has %d lines, want 2", len(got))
	}
	ticks := []sample{decode(t, got[0]), decode(t, got[1])}
	for i, l := range ticks {
		if len(l.Procs) != 2 || l.Procs[0].PID != spin || l.Procs[1].PID != dd {
			t.Fatalf("tick %d: procs %+v, error %s; want the spinning shell (pid %d) and dd (pid %d)", i+1, l.Procs,
				show(l.ProcsError), spin, dd)
		}
	}
	spun := ticks[0].Procs[0].CPU
	if spun == nil || *spun < 5 {
		t.Fatalf("tick 1: the spinning shell's cpu %s, want 5 or more (precondition): it spun since the first reading", val(spun))
	}
	if c := ticks[0].Procs[1].CPU; c == nil || *c < *spun/2 {
		t.Errorf("tick 1: dd's cpu %s, want at least half the spinning shell's %v: its user plus system time", val(c), *spun)
	}
	if c := ticks[1].Procs[0].CPU; c == nil || *c > *spun/4 {
		t.Errorf("tick 2: the spinning shell's cpu %s, want at most a quarter of its tick 1 %v: it was stopped since tick 1",
			val(c), *spun)
	}
}

// TestCPUPercent: CPU seconds over elapsed seconds × 100, only between two readings of one
// process (same start time), later and with no less CPU time.
func TestCPUPercent(t *testing.T) {
	t0 := time.Now()
	r := func(created int64, cpu float64, after time.Duration) cpuReading {
		return cpuReading{created: created, cpu: cpu, at: t0.Add(after)}
	}
	for _, c := range []struct {
		name     string
		old      cpuReading
		ok       bool
		cur      cpuReading
		want     float64
		wantNone bool
	}{
		{"half a CPU", r(5, 10, 0), true, r(5, 10.5, time.Second), 50, false},
		{"three CPUs", r(5, 10, 0), true, r(5, 11.5, 500*time.Millisecond), 300, false},
		{"idle", r(5, 10, 0), true, r(5, 10, time.Second), 0, false},
		{"no earlier reading", cpuReading{}, false, r(5, 10, time.Second), 0, true},
		{"the PID reused", r(5, 10, 0), true, r(6, 0.1, time.Second), 0, true},
		{"no time elapsed", r(5, 10, time.Second), true, r(5, 10.5, time.Second), 0, true},
		{"CPU time went back", r(5, 10, 0), true, r(5, 9, time.Second), 0, true},
	} {
		got := cpuPercent(c.old, c.ok, c.cur)
		switch {
		case c.wantNone && got != nil:
			t.Errorf("%s: %v, want none", c.name, *got)
		case !c.wantNone && (got == nil || *got != c.want):
			t.Errorf("%s: %s, want %v", c.name, val(got), c.want)
		}
	}
}

// TestParseMetrics: hitapi's text form reads into each series' value by its series text, NaN
// and ±Inf as null; a line that is not "series value" or a series given twice fails, naming
// the line.
func TestParseMetrics(t *testing.T) {
	got, err := parseMetrics([]byte("# TYPE a gauge\na 1.5\n\nb_total{result=\"a b\",le=\"+Inf\"} 2\nc NaN\nd +Inf\ne -Inf\n" +
		"f:g_1 -3e-3\n"))
	want := map[string]*float64{"a": num(1.5), `b_total{result="a b",le="+Inf"}`: num(2), "c": nil, "d": nil, "e": nil,
		"f:g_1": num(-0.003)}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("parseMetrics: %v, %v; want %v", got, err, want)
	}
	for _, c := range []struct{ text, wantErr string }{
		{"a\n", "line 1"},
		{"a 1\nb two\n", "line 2"},
		{"a 1 1791374400000\n", "line 1"}, // a timestamp: hitapi writes none
		{"1a 1\n", "line 1"},
		{"a-b 1\n", "line 1"},
		{"a{x=\"1\" 1\n", "line 1"},
		{"{x=\"1\"} 1\n", "line 1"},
		{"a 1\na 2\n", "given twice"},
		{"a 1\r\n", "line 1"},
	} {
		if got, err := parseMetrics([]byte(c.text)); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("parseMetrics(%q) = %v, %v; want an error naming %q", c.text, got, err, c.wantErr)
		}
	}
}

// TestParseInfo: an INFO section reads into its values by key; a line that is not key:value,
// or a key given twice, fails.
func TestParseInfo(t *testing.T) {
	got, err := parseInfo(persistenceInfo)
	if err != nil || !reflect.DeepEqual(got, wantPersistence()) {
		t.Errorf("parseInfo: %v, %v; want %v", got, err, wantPersistence())
	}
	if got, err := parseInfo("# Clients\r\naddr:127.0.0.1:6390\r\n"); err != nil || got["addr"] != "127.0.0.1:6390" {
		t.Errorf("parseInfo of a value with colons: %v, %v", got, err)
	}
	for _, text := range []string{"# Persistence\r\nloading\r\n", "# Persistence\r\n:0\r\n", "a:1\r\na:2\r\n"} {
		if got, err := parseInfo(text); err == nil {
			t.Errorf("parseInfo(%q) = %v, want an error", text, got)
		}
	}
}

// testEnv is a lookupEnv with the sampler's three variables set.
func testEnv(storeAddr string) func(string) (string, bool) {
	vals := map[string]string{envStoreAddr: storeAddr, envOpsPassword: opsPass, envAdminPassword: adminPass}
	return func(name string) (string, bool) {
		v, ok := vals[name]
		return v, ok
	}
}

// waitLines waits, at most 10 s, until path holds n lines or more.
func waitLines(t *testing.T, path string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && bytes.Count(data, []byte{'\n'}) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: fewer than %d lines after 10 s (%v)", path, n, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRun_UntilSIGTERM: the sampler started from its command line and environment writes a
// samples.jsonl line every interval until its context ends (SIGTERM in main), then exits 0;
// its log shows no password.
func TestRun_UntilSIGTERM(t *testing.T) {
	runDir := t.TempDir()
	writePIDs(t, runDir, runlog.Proc{Role: runlog.RoleSampler, PID: os.Getpid()})
	args := []string{"--run-dir", runDir, "--apis", stubAPI(t, &atomic.Bool{}), "--rladmin",
		filepath.Join(t.TempDir(), "rladmin"), "--interval", "200ms", "--pg-data", t.TempDir()}
	env := testEnv(droppingStore(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, args, env, []string{"PATH=/usr/bin:/bin"}, &stderr) }()

	waitLines(t, filepath.Join(runDir, "samples.jsonl"), 2)
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("exit code %d, want %d; log: %s", code, exitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler did not stop within 10 s of its context ending")
	}
	data, err := os.ReadFile(filepath.Join(runDir, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		if l := decode(t, line); len(l.Procs) != 1 || l.Procs[0].Role != runlog.RoleSampler || len(l.APIs) != 1 {
			t.Errorf("line %d: %s", i+1, line)
		}
	}
	for _, pw := range []string{opsPass, adminPass} {
		if strings.Contains(stderr.String(), pw) || bytes.Contains(data, []byte(pw)) {
			t.Error("the log or samples.jsonl shows a password")
		}
	}
	if !strings.Contains(stderr.String(), `"msg":"stopped"`) {
		t.Errorf("log %s: no stopped record", stderr.String())
	}
}

// mainEnv, set to 1 in the environment of a copy of this test binary, makes the copy run the
// sampler's main instead of the tests (TestSampler_SIGTERM).
const mainEnv = "SAMPLER_TEST_RUN_MAIN"

// TestMain runs the tests, or, in a copy of this test binary started with mainEnv, the sampler
// itself: main takes the command line and exits.
func TestMain(m *testing.M) {
	if os.Getenv(mainEnv) == "1" {
		main()
	}
	os.Exit(m.Run())
}

// TestSampler_SIGTERM (plan H3 step 7: exits on SIGTERM): the sampler as a process of its own
// (this test binary run as the sampler) writes a samples.jsonl line every interval; on
// SIGTERM it ends, its log's last record "stopped", and exits 0 within 10 s.
func TestSampler_SIGTERM(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runDir := t.TempDir()
	writePIDs(t, runDir, runlog.Proc{Role: runlog.RoleHitgen, PID: os.Getpid()})
	cmd := exec.CommandContext(t.Context(), bin, "--run-dir", runDir, "--apis", stubAPI(t, &atomic.Bool{}), "--rladmin",
		filepath.Join(t.TempDir(), "rladmin"), "--interval", "200ms", "--pg-data", t.TempDir())
	cmd.Env = []string{mainEnv + "=1", "PATH=/usr/bin:/bin", envStoreAddr + "=" + droppingStore(t),
		envOpsPassword + "=" + opsPass, envAdminPassword + "=" + adminPass}
	var stderr bytes.Buffer // read only once the process has been waited for
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // fails once the process has exited: nothing to stop
		<-exited
	})

	waitLines(t, filepath.Join(runDir, "samples.jsonl"), 2)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler did not exit within 10 s of SIGTERM")
	}
	if waitErr != nil {
		t.Errorf("after SIGTERM: %v, want exit 0; log: %s", waitErr, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"msg":"stopped"`) {
		t.Errorf("log %s: no stopped record", stderr.String())
	}
	if strings.Contains(stderr.String(), opsPass) || strings.Contains(stderr.String(), adminPass) {
		t.Error("the log shows a password")
	}
	data, err := os.ReadFile(filepath.Join(runDir, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		decode(t, line) // every line whole, the last one too
	}
}

// TestRun_Refusals: a bad command line or environment exits 2 before samples.jsonl exists,
// --help exits 0, and a run directory that cannot hold samples.jsonl exits 1.
func TestRun_Refusals(t *testing.T) {
	runDir := t.TempDir()
	good := []string{"--run-dir", runDir, "--apis", "127.0.0.1:8081,127.0.0.1:8082", "--rladmin", "bin/bench/rladmin"}
	without := func(name string) func(string) (string, bool) {
		env := testEnv("127.0.0.1:6390")
		return func(n string) (string, bool) {
			if n == name {
				return "", false
			}
			return env(n)
		}
	}
	for _, c := range []struct {
		name string
		args []string
		env  func(string) (string, bool)
		want int
	}{
		{"--help", []string{"--help"}, testEnv("127.0.0.1:6390"), exitOK},
		{"no --run-dir", good[2:], testEnv("127.0.0.1:6390"), exitUsage},
		{"no --apis", []string{"--run-dir", runDir, "--rladmin", "x"}, testEnv("127.0.0.1:6390"), exitUsage},
		{"no --rladmin", good[:4], testEnv("127.0.0.1:6390"), exitUsage},
		{"a non-loopback API", append(good[:2:2], "--apis", "10.0.0.1:8081", "--rladmin", "x"), testEnv("127.0.0.1:6390"), exitUsage},
		{"an API without a port", append(good[:2:2], "--apis", "127.0.0.1", "--rladmin", "x"), testEnv("127.0.0.1:6390"), exitUsage},
		{"port 0", append(good[:2:2], "--apis", "127.0.0.1:0", "--rladmin", "x"), testEnv("127.0.0.1:6390"), exitUsage},
		{"an API twice", append(good[:2:2], "--apis", "127.0.0.1:8081,127.0.0.1:8081", "--rladmin", "x"), testEnv("127.0.0.1:6390"), exitUsage},
		{"four APIs", append(good[:2:2], "--apis", "127.0.0.1:1,127.0.0.1:2,127.0.0.1:3,127.0.0.1:4", "--rladmin", "x"),
			testEnv("127.0.0.1:6390"), exitUsage},
		{"interval 99ms", append(good[:6:6], "--interval", "99ms"), testEnv("127.0.0.1:6390"), exitUsage},
		{"empty --pg-data", append(good[:6:6], "--pg-data", ""), testEnv("127.0.0.1:6390"), exitUsage},
		{"a positional argument", append(good[:6:6], "extra"), testEnv("127.0.0.1:6390"), exitUsage},
		{"no RL_STORE_ADDR", good, without(envStoreAddr), exitUsage},
		{"no RL_OPS_PASSWORD", good, without(envOpsPassword), exitUsage},
		{"no RL_ADMIN_PASSWORD", good, without(envAdminPassword), exitUsage},
		{"RL_STORE_ADDR without a port", good, testEnv("127.0.0.1"), exitUsage},
		{"no run directory", append([]string{"--run-dir", filepath.Join(runDir, "missing")}, good[2:]...), testEnv("127.0.0.1:6390"), exitFailure},
	} {
		var stderr bytes.Buffer
		if got := run(t.Context(), c.args, c.env, nil, &stderr); got != c.want {
			t.Errorf("%s: exit code %d, want %d; log: %s", c.name, got, c.want, stderr.String())
		}
		if strings.Contains(stderr.String(), opsPass) || strings.Contains(stderr.String(), adminPass) {
			t.Errorf("%s: the log shows a password", c.name)
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, "samples.jsonl")); err == nil {
		t.Error("a refused start created samples.jsonl")
	}
}

// TestParseFlags: a valid command line keeps the instances in order and the defaults.
func TestParseFlags(t *testing.T) {
	f, err := parseFlags([]string{"--run-dir", "runs/x", "--apis", "127.0.0.1:8081,[::1]:8082", "--rladmin", "bin/test/rladmin"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := flags{runDir: "runs/x", rladmin: "bin/test/rladmin", pgData: ".data/pg", apis: []string{"127.0.0.1:8081", "[::1]:8082"},
		interval: time.Second}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("parseFlags: %+v, want %+v", f, want)
	}
	if got := readTimeout(time.Second); got != 800*time.Millisecond {
		t.Errorf("readTimeout(1s) = %v, want 800ms", got)
	}
}
