//go:build rltest

// Package e2e holds the end-to-end tests (DESIGN §C1 L4, §C5; IMPLEMENTATION_PLAN H1 steps
// 12 and 14): each test starts a stack with StartStack (a fast-profile store, an epoch started
// by rladmin, the harness PostgreSQL and bin/test or bin/bench hitapi processes) and drives it
// over HTTP. Every child the helpers start from bin/ is recorded in <repo>/.out/procs/<pid>
// (content: the command) from right after its start until its Wait returns, for bin/trace
// procs; the store is recorded by storeproc, and PostgreSQL is shared and never recorded. The
// tests share the hits table, so the package runs with -p 1 and no test runs in parallel.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/dbclient"
	"ratelimiter/harness/fixtures"
	"ratelimiter/harness/pgtest"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// Frozen is the controlled clock's start in these tests (ns, a whole number of µs, after
// 10^15 µs): StackOpts{Clock: Frozen} runs a stack on a frozen controlled clock.
const Frozen = int64(1_791_374_400_000_000_000)

// The stack's waits (plan H1 step 12 and plan choices).
const (
	readyWait     = 10 * time.Second // hitapi up and /health status = ready
	killSlack     = 5 * time.Second  // the kill grace past P + drain_timeout (DESIGN §B10)
	killWait      = 10 * time.Second // a SIGKILLed child to be reaped
	rladminWait   = 45 * time.Second // rladmin's own 30 s command deadline plus 15 s
	clientTimeout = 30 * time.Second // one HTTP exchange; prekill may take P + 6 s
	pollEvery     = 20 * time.Millisecond
	portTries     = 3 // hitapi starts on another free port when its port was taken meanwhile
	// helperWait bounds each query of the stack's own database client (Stack.DB), its wait for
	// a pool connection included, so a starved pool fails the test instead of hanging it.
	helperWait = 10 * time.Second
	// helperPool is the smallest pool Stack.DB gets: a held key (holdKey) or two, and the
	// helper queries beside them.
	helperPool = 4
)

// StackOpts selects how StartStack runs a stack.
type StackOpts struct {
	// Instances is the number of hitapi processes, numbered from 1; 0 means 1.
	Instances int
	// Build is "test" (bin/test, the default) or "bench" (bin/bench): only the bench-binary
	// checks use "bench" (DESIGN §C5).
	Build string
	// Clock, when not 0, is the controlled clock's frozen time in ns (a multiple of 1,000):
	// the config gets clock_source = "controlled" and the store's rl:ctl:time is set with
	// fixtures.Clock.Set before the epoch starts. 0 runs on the store's own clock.
	Clock int64
	// Edit, when not nil, changes the stack's config (harness.toml with the stack's store
	// address and clock) before it is written.
	Edit func(*config.File)
	// PerInstanceConfig gives instance N its own config file: these TOML overrides of
	// [policy], [instance] and [api] merged onto the stack's config (config.Merge). StartStack
	// waits for such an instance to be ready, like every other.
	PerInstanceConfig map[int]string
	// NoEpoch, when true, starts no epoch on the store (ERR-12a): each hitapi stays not ready,
	// and StartStack waits for its /health to answer status not_ready with reason no_epoch
	// instead of ready. The test may start the epoch later (Stack.startEpoch).
	NoEpoch bool
}

// Stack is one running stack. Its cleanup stops every hitapi (SIGTERM, then SIGKILL after
// P + drain_timeout + 5 s), then the store.
type Stack struct {
	Store *storeproc.Store
	Clock *fixtures.Clock // nil on the store's clock
	// DB is the stack's own client of the harness database, opened on harness.toml's db_dsn as
	// loaded (before Edit and PerInstanceConfig), with a pool of at least helperPool
	// connections; hits was truncated before hitapi started.
	DB         *dbclient.Client
	Config     config.File // the stack's config as written (no password)
	ConfigPath string
	Dir        string
	APIs       []*API // APIs[i] is instance i+1
	HTTP       *http.Client

	t       testing.TB
	root    string
	build   string
	noEpoch bool // StackOpts.NoEpoch: waitReady takes not_ready (no_epoch) for ready
}

// API is one hitapi process.
type API struct {
	N       int
	Addr    string // 127.0.0.1:port
	URL     string // http://Addr
	LogPath string // its run log, api-N.jsonl
	*child

	stack *Stack
	grace time.Duration // P + drain_timeout + 5 s from its config
}

// child is a bin/ process a helper started: recorded in .out/procs/<pid> from right after
// Start until Wait returns.
type child struct {
	Cmdline string // the command, as recorded
	PID     int
	Out     string // the file holding its stdout and stderr

	cmd     *exec.Cmd
	record  string
	done    chan struct{}
	waitErr error // set before done closes
	recErr  error // removing the record, set before done closes
}

// StartStack starts a stack for t and registers its cleanup (IMPLEMENTATION_PLAN H1 step 12):
// a fast-profile store (storeproc); with o.Clock the controlled time; rladmin start-epoch
// --policy <config> from bin/<build> (RL_STORE_ADDR and RL_ADMIN_PASSWORD in its environment),
// unless o.NoEpoch; PostgreSQL through pgtest.Ensure and TRUNCATE hits; then each hitapi from
// bin/<build> on a free loopback port, waiting up to 10 s for /health status = ready (with
// o.NoEpoch, not_ready with reason no_epoch). Any failure ends the test; a missing binary or
// PostgreSQL is a failure, never a skip.
func StartStack(t *testing.T, o StackOpts) *Stack {
	t.Helper()
	if o.Instances == 0 {
		o.Instances = 1
	}
	if o.Build == "" {
		o.Build = "test"
	}
	if o.Build != "test" && o.Build != "bench" {
		t.Fatalf("StartStack: build %q, want test or bench", o.Build)
	}
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatalf("StartStack: %v", err)
	}
	s := &Stack{t: t, root: root, build: o.Build, Dir: t.TempDir(), noEpoch: o.NoEpoch}
	for _, name := range []string{"hitapi", "rladmin"} {
		if err := checkBinary(s.bin(name)); err != nil {
			t.Fatalf("StartStack: %v", err)
		}
	}

	s.Store = storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
	var baseDSN string
	s.Config, baseDSN = s.stackConfig(o)
	s.ConfigPath = filepath.Join(s.Dir, "config.toml")
	s.writeConfig(s.ConfigPath, s.Config)
	if o.Clock != 0 {
		conn, err := s.Store.Conn("rl_fixture")
		if err != nil {
			t.Fatalf("StartStack: connecting as rl_fixture: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		s.Clock = fixtures.NewClock(conn, nil)
		if err := s.Clock.Set(o.Clock); err != nil {
			t.Fatalf("StartStack: setting the controlled time: %v", err)
		}
	}
	if !o.NoEpoch {
		s.startEpoch()
	}

	pgtest.Ensure(t)
	dsn, err := helperDSN(baseDSN)
	if err != nil {
		t.Fatalf("StartStack: %v", err)
	}
	s.DB, err = dbclient.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("StartStack: opening the database client: %v", err)
	}
	t.Cleanup(s.DB.Close)
	ctx, cancel := context.WithTimeout(t.Context(), helperWait)
	err = s.DB.Truncate(ctx)
	cancel()
	if err != nil {
		t.Fatalf("StartStack: %v", err)
	}

	tr := &http.Transport{DisableKeepAlives: true} // a fresh connection per request: no hidden resend
	s.HTTP = &http.Client{Timeout: clientTimeout, Transport: tr}
	t.Cleanup(tr.CloseIdleConnections)
	t.Cleanup(s.stopAPIs) // registered after the store's and the database's: runs before them
	for n := 1; n <= o.Instances; n++ {
		path, cfg := s.ConfigPath, s.Config
		if own, ok := o.PerInstanceConfig[n]; ok {
			path, cfg = s.instanceConfig(n, own)
		}
		s.APIs = append(s.APIs, s.startAPI(n, path, cfg))
	}
	return s
}

// helperDSN is the DSN of the stack's own database client: dsn, harness.toml's db_dsn (a
// postgres:// URL), with pool_max_conns raised to helperPool when it is lower or absent. The
// error never quotes dsn, which may hold a password.
func helperDSN(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("harness.toml's db_dsn is not a postgres:// URL")
	}
	q := u.Query()
	if n, err := strconv.Atoi(q.Get("pool_max_conns")); err == nil && n >= helperPool {
		return dsn, nil
	}
	q.Set("pool_max_conns", strconv.Itoa(helperPool))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// bin is <repo>/bin/<build>/<name>.
func (s *Stack) bin(name string) string { return filepath.Join(s.root, "bin", s.build, name) }

// checkBinary fails on a missing binary or a directory.
func checkBinary(path string) error {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("binary %s does not exist: run make build", path)
	case err != nil:
		return fmt.Errorf("binary %s: %w", path, err)
	case fi.IsDir():
		return fmt.Errorf("binary %s is a directory", path)
	}
	return nil
}

// stackConfig is harness.toml with the stack's store address and clock, then o.Edit; baseDSN
// is harness.toml's db_dsn as loaded, before o.Edit, for the stack's own database client.
func (s *Stack) stackConfig(o StackOpts) (f config.File, baseDSN string) {
	s.t.Helper()
	f, err := config.Load(filepath.Join(s.root, "harness.toml"))
	if err != nil {
		s.t.Fatalf("StartStack: %v", err)
	}
	baseDSN = f.API.DBDSN
	f.Store.Addr = s.Store.Addr
	f.Store.Password = "" // it reaches hitapi only through RL_LIB_PASSWORD
	if o.Clock != 0 {
		f.Policy.ClockSource = limiter.Controlled
	}
	if o.Edit != nil {
		o.Edit(&f)
	}
	return f, baseDSN
}

// writeConfig writes f to path.
func (s *Stack) writeConfig(path string, f config.File) {
	s.t.Helper()
	data, err := config.Encode(f)
	if err != nil {
		s.t.Fatalf("StartStack: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		s.t.Fatalf("StartStack: writing %s: %v", path, err)
	}
}

// instanceConfig writes instance n's own config: overrides merged onto the stack's.
func (s *Stack) instanceConfig(n int, overrides string) (string, config.File) {
	s.t.Helper()
	base, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		s.t.Fatalf("StartStack: %v", err)
	}
	merged, err := config.Merge(base, []byte(overrides))
	if err != nil {
		s.t.Fatalf("StartStack: instance %d's config: %v", n, err)
	}
	path := filepath.Join(s.Dir, fmt.Sprintf("config-%d.toml", n))
	if err := os.WriteFile(path, merged, 0o600); err != nil {
		s.t.Fatalf("StartStack: writing %s: %v", path, err)
	}
	f, err := config.Load(path)
	if err != nil {
		s.t.Fatalf("StartStack: %v", err)
	}
	return path, f
}

// startEpoch runs bin/<build>/rladmin start-epoch --policy <the stack's config>.
func (s *Stack) startEpoch() {
	s.t.Helper()
	code, out := s.RLAdmin(s.t, "start-epoch", "--policy", s.ConfigPath)
	if code != 0 {
		s.t.Fatalf("StartStack: rladmin start-epoch exited %d: %s", code, out)
	}
}

// RLAdmin runs bin/<build>/rladmin with args against the stack's store as rl_admin, with
// RL_STORE_ADDR and RL_ADMIN_PASSWORD as its whole environment, killed after 45 s. It returns
// the exit code and the output (stdout, then stderr), and fails the test when rladmin cannot
// be run or does not exit in time.
func (s *Stack) RLAdmin(t testing.TB, args ...string) (code int, out string) {
	t.Helper()
	env := []string{"RL_STORE_ADDR=" + s.Store.Addr, "RL_ADMIN_PASSWORD=" + s.Store.PW.Admin}
	c, err := s.startChild(t, s.bin("rladmin"), args, env, "rladmin")
	if err != nil {
		t.Fatalf("rladmin %s: %v", strings.Join(args, " "), err)
	}
	if !c.exitedWithin(rladminWait) {
		c.kill(t)
		t.Fatalf("rladmin %s did not exit within %v", strings.Join(args, " "), rladminWait)
	}
	c.checkRecord(t)
	data, err := os.ReadFile(c.Out)
	if err != nil {
		t.Fatalf("reading rladmin's output: %v", err)
	}
	code, err = exitCode(c.waitErr)
	if err != nil {
		t.Fatalf("rladmin %s: %v: %s", strings.Join(args, " "), err, data)
	}
	return code, string(data)
}

// startAPI starts instance n of hitapi with the config at path and waits up to 10 s for its
// /health to answer status = ready. A port another process took between the free-port probe
// and hitapi's bind is tried again on a new port.
func (s *Stack) startAPI(n int, path string, cfg config.File) *API {
	s.t.Helper()
	for try := 1; ; try++ {
		port, err := freePort(s.t.Context())
		if err != nil {
			s.t.Fatalf("StartStack: %v", err)
		}
		a := &API{
			N:       n,
			Addr:    "127.0.0.1:" + strconv.Itoa(port),
			LogPath: filepath.Join(s.Dir, fmt.Sprintf("api-%d.jsonl", n)),
			stack:   s,
			grace:   time.Duration(cfg.Instance.MaxReleaseDelayNs+cfg.Instance.DrainTimeoutNs) + killSlack,
		}
		a.URL = "http://" + a.Addr
		args := []string{"--config", path, "--listen", a.Addr, "--log", a.LogPath,
			"--instance", strconv.Itoa(n), "--run-id", "e2e-" + strconv.Itoa(n)}
		env := []string{config.LibPasswordEnv + "=" + s.Store.PW.Lib}
		a.child, err = s.startChild(s.t, s.bin("hitapi"), args, env, fmt.Sprintf("hitapi-%d", n))
		if err != nil {
			s.t.Fatalf("StartStack: hitapi %d: %v", n, err)
		}
		err = a.waitReady(s.t)
		if err == nil {
			return a
		}
		out, _ := os.ReadFile(a.Out)
		portTaken := bytes.Contains(out, []byte("address already in use"))
		a.stopChild(s.t) // reaps it either way
		if !portTaken || try == portTries {
			s.t.Fatalf("StartStack: hitapi %d: %v; its output:\n%s", n, err, out)
		}
		// The next try starts a fresh run log: the failed one wrote its start and stop records.
		if err := os.Remove(a.LogPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.t.Fatalf("StartStack: %v", err)
		}
	}
}

// waitReady polls /health until it answers status = ready (on a stack started with NoEpoch:
// status not_ready with reason no_epoch), the process exits, or 10 s have passed since the
// call. Each probe runs under that deadline, so a /health that hangs ends the wait on time and
// a ready that comes later is never taken.
func (a *API) waitReady(t testing.TB) error {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(readyWait))
	defer cancel()
	last := "no answer"
	for {
		select {
		case <-a.done:
			return fmt.Errorf("exited before it was ready: %w", a.waitErr)
		default:
		}
		if r, err := a.try(ctx, http.MethodGet, "/health", nil, ""); err == nil {
			if r.JSON["status"] == "ready" && !a.stack.noEpoch ||
				r.JSON["status"] == "not_ready" && r.JSON["reason"] == "no_epoch" && a.stack.noEpoch {
				return nil
			}
			last = string(r.Body)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("/health not ready after %v; last answer: %s", readyWait, last)
		}
		time.Sleep(pollEvery)
	}
}

// freePort returns a loopback port that was free a moment ago.
func freePort(ctx context.Context) (int, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding a free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, fmt.Errorf("finding a free port: %w", err)
	}
	return port, nil
}

// startChild starts bin with args and exactly env, its output going to <s.Dir>/<name>.out,
// recorded in .out/procs/<pid> (startProcess).
func (s *Stack) startChild(t testing.TB, bin string, args, env []string, name string) (*child, error) {
	return startProcess(t, s.root, s.Dir, bin, args, env, name)
}

// startProcess starts bin with args and exactly env, its output going to <dir>/<name>.out,
// records it in <root>/.out/procs/<pid> right after Start, and reaps it in a goroutine that
// deletes the record once Wait returns. The child outlives the test's context until a helper
// stops it (AGENTS.md §2).
func startProcess(t testing.TB, root, dir, bin string, args, env []string, name string) (*child, error) {
	return startProcessIn(t, root, dir, "", bin, args, env, name)
}

// startProcessIn is startProcess with the child's working directory workDir ("": the test's
// own).
func startProcessIn(t testing.TB, root, dir, workDir, bin string, args, env []string, name string) (*child, error) {
	outPath := filepath.Join(dir, name+".out")
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("output file: %w", err)
	}
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), bin, args...)
	cmd.Dir = workDir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Start()
	_ = out.Close() // the child holds its own descriptor
	if err != nil {
		return nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	c := &child{
		Cmdline: strings.Join(append([]string{bin}, args...), " "),
		PID:     cmd.Process.Pid,
		Out:     outPath,
		cmd:     cmd,
		record:  filepath.Join(root, ".out", "procs", strconv.Itoa(cmd.Process.Pid)),
		done:    make(chan struct{}),
	}
	if err := writeRecord(c.record, c.Cmdline); err != nil {
		_ = cmd.Process.Kill() // unrecorded, it must not outlive this call
		_ = cmd.Wait()
		return nil, errors.Join(err, removeRecord(c.record))
	}
	go func() {
		c.waitErr = cmd.Wait()
		c.recErr = removeRecord(c.record)
		close(c.done)
	}()
	return c, nil
}

func writeRecord(path, cmdline string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("process record: %w", err)
	}
	if err := os.WriteFile(path, []byte(cmdline+"\n"), 0o644); err != nil {
		return fmt.Errorf("process record: %w", err)
	}
	return nil
}

func removeRecord(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the process record: %w", err)
	}
	return nil
}

// RecordPath is the child's process record, <repo>/.out/procs/<pid>.
func (c *child) RecordPath() string { return c.record }

// Exited is closed once the child has exited and its record is gone.
func (c *child) Exited() <-chan struct{} { return c.done }

// exitedWithin reports whether the child exits (and is reaped) within d.
func (c *child) exitedWithin(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c.done:
		return true
	case <-timer.C:
		return false
	}
}

// signal sends sig to a child that may have exited already.
func (c *child) signal(t testing.TB, sig syscall.Signal) {
	t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("sending %v to %d (%s): %v", sig, c.PID, c.Cmdline, err)
	}
}

// kill SIGKILLs the child and waits until it is reaped.
func (c *child) kill(t testing.TB) {
	t.Helper()
	c.signal(t, syscall.SIGKILL)
	if !c.exitedWithin(killWait) {
		t.Errorf("process %d (%s) still running %v after SIGKILL", c.PID, c.Cmdline, killWait)
	}
}

// checkRecord fails the test when the reaper could not delete the record.
func (c *child) checkRecord(t testing.TB) {
	t.Helper()
	if c.recErr != nil {
		t.Errorf("process %d: %v", c.PID, c.recErr)
	}
}

// exitCode is a reaped child's exit code from its Wait error; an error for a child that did
// not exit by itself (a signal, or a Wait failure).
func exitCode(waitErr error) (int, error) {
	var exit *exec.ExitError
	switch {
	case waitErr == nil:
		return 0, nil
	case errors.As(waitErr, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode(), nil
	default:
		return -1, fmt.Errorf("did not exit by itself: %w", waitErr)
	}
}

// Terminate stops the hitapi gracefully, as the orchestrator does (DESIGN §B5): SIGTERM, then
// SIGKILL once its grace (P + drain_timeout + 5 s) has passed. It returns the exit code and
// whether the process exited within its grace; a killed process fails the test.
func (a *API) Terminate(t testing.TB) (code int, inGrace bool) {
	t.Helper()
	select {
	case <-a.done:
	default:
		a.signal(t, syscall.SIGTERM)
		if !a.exitedWithin(a.grace) {
			a.kill(t)
			t.Errorf("hitapi %d ignored SIGTERM for its grace of %v and was killed", a.N, a.grace)
			return -1, false
		}
	}
	a.checkRecord(t)
	code, err := exitCode(a.waitErr)
	if err != nil {
		t.Errorf("hitapi %d: %v", a.N, err)
	}
	return code, true
}

// stopChild ends a child that failed to start properly: SIGKILL, reaped.
func (a *API) stopChild(t testing.TB) {
	t.Helper()
	select {
	case <-a.done:
	default:
		a.kill(t)
	}
	a.checkRecord(t)
}

// stopAPIs is the stack's cleanup: every hitapi still running gets SIGTERM, and must exit 0
// within its grace (HAR-15).
func (s *Stack) stopAPIs() {
	for _, a := range s.APIs {
		select {
		case <-a.done:
			a.checkRecord(s.t)
			continue
		default:
		}
		if code, ok := a.Terminate(s.t); ok && code != 0 {
			out, _ := os.ReadFile(a.Out)
			s.t.Errorf("hitapi %d exited %d after SIGTERM, want 0; its output:\n%s", a.N, code, out)
		}
	}
}

// Response is one HTTP exchange, its body read and closed. JSON holds the body when it is a
// JSON object (numbers as json.Number). Before and After are the client's wall time around
// the exchange.
type Response struct {
	Status        int
	Header        http.Header
	Body          []byte
	JSON          map[string]any
	Before, After time.Time
}

// Do sends one request to the API and fails the test when no response comes back.
func (a *API) Do(t testing.TB, method, path string, h http.Header, body string) Response {
	t.Helper()
	r, err := a.try(t.Context(), method, path, h, body)
	if err != nil {
		t.Fatalf("%s %s on hitapi %d: %v", method, path, a.N, err)
	}
	return r
}

// try sends one request on ctx.
func (a *API) try(ctx context.Context, method, path string, h http.Header, body string) (Response, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL+path, rd)
	if err != nil {
		return Response{}, fmt.Errorf("building the request: %w", err)
	}
	for k, vs := range h {
		req.Header[k] = append([]string(nil), vs...)
	}
	r := Response{Before: time.Now()}
	resp, err := a.stack.HTTP.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("sending the request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // read to the end below; nothing left to lose
	r.Body, err = io.ReadAll(resp.Body)
	r.After = time.Now()
	if err != nil {
		return Response{}, fmt.Errorf("reading the response: %w", err)
	}
	r.Status, r.Header = resp.StatusCode, resp.Header
	if obj, err := decodeObject(r.Body); err == nil {
		r.JSON = obj
	}
	return r, nil
}

// Answer is a response, or why none came back, from a request sent on another goroutine,
// which must not end the test itself (t.Fatal belongs to the test's goroutine).
type Answer struct {
	Response
	Err error
}

// Go sends one request on a new goroutine, on t's context; the answer arrives on the
// returned channel.
func (a *API) Go(t testing.TB, method, path string, h http.Header, body string) <-chan Answer {
	ch := make(chan Answer, 1)
	ctx := t.Context()
	go func() {
		r, err := a.try(ctx, method, path, h, body)
		ch <- Answer{r, err}
	}()
	return ch
}

// Wait receives an answer within d, failing the test when none comes or it is an error.
func Wait(t testing.TB, ch <-chan Answer, d time.Duration, what string) Response {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case a := <-ch:
		if a.Err != nil {
			t.Fatalf("%s: %v", what, a.Err)
		}
		return a.Response
	case <-timer.C:
		t.Fatalf("%s: no answer within %v", what, d)
		return Response{}
	}
}

// IDs is the /hit headers with one value each.
func IDs(user, request string) http.Header {
	return http.Header{"X-User-Id": {user}, "X-Request-Id": {request}}
}

// Hit sends GET /hit with one X-User-ID and one X-Request-ID.
func (a *API) Hit(t testing.TB, user, request string) Response {
	t.Helper()
	return a.Do(t, http.MethodGet, "/hit", IDs(user, request), "")
}

// Control sends POST path (a /_test route) with body and wants 200.
func (a *API) Control(t testing.TB, path, body string) Response {
	t.Helper()
	r := a.Do(t, http.MethodPost, path, nil, body)
	if r.Status != http.StatusOK {
		t.Fatalf("POST %s %s: %d %s, want 200", path, body, r.Status, r.Body)
	}
	return r
}

// Health returns /health's JSON.
func (a *API) Health(t testing.TB) map[string]any {
	t.Helper()
	r := a.Do(t, http.MethodGet, "/health", nil, "")
	if r.Status != http.StatusOK || r.JSON == nil {
		t.Fatalf("/health: %d %s", r.Status, r.Body)
	}
	return r.JSON
}

// AcquireTotal returns the sum of rl_acquire_results_total over every result, from /metrics.
func (a *API) AcquireTotal(t testing.TB) int64 {
	t.Helper()
	return a.metricSum(t, "rl_acquire_results_total")
}

// metricSum sums every series of a metric family in /metrics.
func (a *API) metricSum(t testing.TB, family string) int64 {
	t.Helper()
	r := a.Do(t, http.MethodGet, "/metrics", nil, "")
	if r.Status != http.StatusOK {
		t.Fatalf("/metrics: %d %s", r.Status, r.Body)
	}
	var sum int64
	seen := false
	for _, line := range strings.Split(string(r.Body), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if base, _, _ := strings.Cut(name, "{"); base != family {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || v != float64(int64(v)) {
			t.Fatalf("/metrics line %q: not a whole number", line)
		}
		sum += int64(v)
		seen = true
	}
	if !seen {
		t.Fatalf("/metrics has no %s series:\n%s", family, r.Body)
	}
	return sum
}

// Lines returns the run log's complete lines as they are on the file now: a line hitapi is
// still writing (no newline yet) is left for a later read.
func (a *API) Lines(t testing.TB) [][]byte {
	t.Helper()
	data, err := os.ReadFile(a.LogPath)
	if err != nil {
		t.Fatalf("reading hitapi %d's run log: %v", a.N, err)
	}
	data = data[:bytes.LastIndexByte(data, '\n')+1]
	var out [][]byte
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// Records returns the run log's records of kind, decoded (numbers as json.Number).
func (a *API) Records(t testing.TB, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range a.Lines(t) {
		m, err := decodeObject(line)
		if err != nil {
			t.Fatalf("hitapi %d's run log line %s: %v", a.N, line, err)
		}
		if m["kind"] == kind {
			out = append(out, m)
		}
	}
	return out
}

// WaitRecords polls the run log until want(records of kind) holds, or 5 s pass: the run log
// is written by hitapi's writer goroutine, a moment after the response.
func (a *API) WaitRecords(t testing.TB, kind string, want func([]map[string]any) bool) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := a.Records(t, kind)
		if want(recs) {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("hitapi %d's run log: the %s records never matched after 5 s; they are %v", a.N, kind, recs)
		}
		time.Sleep(pollEvery)
	}
}

// decodeObject decodes data as exactly one JSON object, numbers kept as json.Number.
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if m == nil {
		return nil, errors.New("not a JSON object: null")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more after the JSON object")
	}
	return m, nil
}

// Int reads a JSON number field as an int64, failing the test when it is not one.
func Int(t testing.TB, m map[string]any, key string) int64 {
	t.Helper()
	n, ok := m[key].(json.Number)
	if !ok {
		t.Fatalf("%s = %v (%T) in %v, want a number", key, m[key], m[key], m)
	}
	v, err := n.Int64()
	if err != nil {
		t.Fatalf("%s = %v: %v", key, n, err)
	}
	return v
}

// Balance returns the key's stored balance u in units from rladmin inspect --json (ADM-4).
func (s *Stack) Balance(t testing.TB, key string) int64 {
	t.Helper()
	code, out := s.RLAdmin(t, "inspect", "--key", key, "--json")
	if code != 0 {
		t.Fatalf("rladmin inspect --key %s exited %d: %s", key, code, out)
	}
	m, err := decodeObject([]byte(out))
	if err != nil {
		t.Fatalf("rladmin inspect --key %s --json printed %q: %v", key, out, err)
	}
	return Int(t, m, "u")
}

// waitLockWaiters polls until at least n database sessions wait for a lock (requests blocked
// behind a held key), or 5 s pass: every query runs within those 5 s.
func (s *Stack) waitLockWaiters(t testing.TB, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	for {
		got, err := s.DB.LockWaiters(ctx)
		if err != nil {
			t.Fatalf("LockWaiters (want %d lock waiters within 5 s): %v", n, err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d database sessions wait for a lock after 5 s, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdKey holds (user, request) in an open, uncommitted insert, rolled back at cleanup at the
// latest (before the stack stops). Taking the hold may take up to helperWait; the rollback
// has its own deadline (dbclient.HoldKey).
func (s *Stack) holdKey(t testing.TB, user, request string) func() error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), helperWait)
	defer cancel()
	rollback, err := s.DB.HoldKey(ctx, user, request)
	if err != nil {
		t.Fatalf("HoldKey(%s, %s): %v", user, request, err)
	}
	t.Cleanup(func() {
		if err := rollback(); err != nil {
			t.Errorf("rolling back the held key (%s, %s): %v", user, request, err)
		}
	})
	return rollback
}

// rows returns the hits table's row count, within helperWait.
func (s *Stack) rows(t testing.TB) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), helperWait)
	defer cancel()
	n, err := s.DB.CountRows(ctx)
	if err != nil {
		t.Fatalf("CountRows: %v", err)
	}
	return n
}

// keyRows returns how many rows hold (user, request), 0 or 1, within helperWait.
func (s *Stack) keyRows(t testing.TB, user, request string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), helperWait)
	defer cancel()
	n, err := s.DB.CountKey(ctx, user, request)
	if err != nil {
		t.Fatalf("CountKey: %v", err)
	}
	return n
}

// rowArrival returns the arrival_ns of the row holding (user, request), within helperWait;
// found is false when no row holds the key.
func (s *Stack) rowArrival(t testing.TB, user, request string) (arrivalNs int64, found bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), helperWait)
	defer cancel()
	arrivalNs, found, err := s.DB.RowArrival(ctx, user, request)
	if err != nil {
		t.Fatalf("RowArrival(%s, %s): %v", user, request, err)
	}
	return arrivalNs, found
}
