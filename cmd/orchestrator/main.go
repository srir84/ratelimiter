// Command orchestrator runs one harness run end to end (DESIGN §B1, §B5; HARNESS §4, HAR-24;
// IMPLEMENTATION_PLAN H3): it checks the run before anything starts (preflight.go), takes the
// pid file (pidfile.go), creates the run directory with the merged config and the rendered ACL
// file (rundir.go), then runs the §B5 sequence (run.go).
//
//	orchestrator --scenario FILE [--baseline] [--instance-config N=FILE]… [--bin-dir DIR]
//
// --baseline runs with the limiter off ([api] limiter_enabled = false, HAR-17).
// --instance-config gives instance N its own config file in place of the run's config.toml
// (test runs only, T-HAR-1). --bin-dir is the directory hitapi and rladmin run from (default
// bin/<run_kind>); hitgen and the sampler always run from bin/test (§B1). It runs in the
// repository root, where it finds harness.toml, bin/, deploy/, runs/ and .data/.
//
// Exit codes (plan choice): 0 after a finished run; 1 a failure once the run has begun, or a
// signal (SIGINT, SIGTERM or SIGHUP); 2 a bad command line or a refused run. A refused run logs one "refusing the run"
// error line per reason found, each with a fixed reason code (preflight.go, run.go) and a
// detail. Preflight's refusals start nothing; the checks that need the running store or
// instances (maxclients, the benchmark guard's /health check) refuse once the run is torn down.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ratelimiter/harness/runlog"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitRefused = 2

	// The run's fixed ports (DESIGN §B1): the store, and hitapi instance n at apiBasePort+n-1.
	storePort   = 6390
	apiBasePort = 8081
)

func main() {
	// With SIGPIPE caught, a log line written to a stderr pipe whose reader has gone (Ctrl-C on
	// `make run … 2>&1 | tee log` ends tee too) fails with EPIPE, which the logger drops,
	// instead of killing the orchestrator before it stops its children. Children are
	// unaffected: exec resets a caught signal to its default action.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals(signal.Ignored)...)
	s := site{root: ".", storePort: storePort, apiBase: apiBasePort}
	code := orchestrate(ctx, os.Args[1:], s, os.Environ(), os.Getpid(), time.Now, os.Stderr)
	stop()
	os.Exit(code)
}

// stopSignals are the signals that stop a run (IMPLEMENTATION_PLAN H3 step 4): each ends the
// run's ctx, so the run stops every child's group in order, shuts the store down, removes the
// pid file and exits 1. SIGHUP (a closed terminal, a dropped SSH session) is among them: its
// default action would kill the orchestrator alone and leave its children, each in its own
// process group, running. A SIGINT or SIGHUP that ignored reports ignored at startup (nohup
// ignores SIGHUP; a shell ignores SIGINT for a background job) is left out, so it stays
// ignored: catching it would undo the ignore. SIGTERM is always caught. SIGQUIT keeps Go's
// default (a goroutine dump, for debugging).
func stopSignals(ignored func(os.Signal) bool) []os.Signal {
	sigs := []os.Signal{syscall.SIGTERM}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGHUP} {
		if !ignored(sig) {
			sigs = append(sigs, sig)
		}
	}
	return sigs
}

// flags is a parsed command line (plan H3 step 8).
type flags struct {
	scenario        string
	baseline        bool
	instanceConfigs map[int]string // --instance-config: instance number → its config file
	binDir          string         // --bin-dir; "" means bin/<run_kind>
}

// orchestrate runs the orchestrator and returns its exit code: the command line, preflight,
// the pid file, the run directory, then the run sequence. environ is the orchestrator's own
// environment, the base of every child's (passwords.envs); pid is its process ID, which the
// pid file names; now reads the wall clock, for the run ID.
func orchestrate(ctx context.Context, args []string, s site, environ []string, pid int, now func() time.Time, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	fl, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		logger.Error("bad command line", "error", err)
		return exitRefused
	}
	in, refusals := preflight(ctx, s, fl)
	if err := ctx.Err(); err != nil {
		logger.Error("stopped during preflight", "error", err)
		return exitFailure
	}
	if len(refusals) > 0 {
		for _, r := range refusals {
			logger.Error("refusing the run", "reason", r.reason, "detail", r.detail)
		}
		return exitRefused
	}
	lock, err := takePIDFile(s.path(pidFileName), pid)
	switch {
	case errors.Is(err, errRunning): // another run took the pid file since preflight
		logger.Error("refusing the run", "reason", reasonRunning, "detail", err.Error())
		return exitRefused
	case err != nil:
		logger.Error("taking the pid file", "error", err)
		return exitFailure
	}
	defer func() {
		if err := lock.release(); err != nil {
			logger.Error("removing the pid file", "error", err)
		}
	}()
	rd, err := createRunDir(s, in, now())
	if err != nil {
		logger.Error("creating the run directory", "error", err)
		return exitFailure
	}
	envs, err := rd.pw.envs(environ, in.settings.Store.Addr)
	if err != nil {
		logger.Error("building the children's environments", "error", err)
		return exitFailure
	}
	logger.Info("run directory ready", "run_id", rd.id, "dir", rd.path, "run_kind", in.scenario.RunKind,
		"baseline", in.baseline, "thresholds_sha256", rd.thresholdsSHA256)
	client := newHTTPClient()
	defer client.CloseIdleConnections()
	return runSequence(ctx, logger, s, in, rd, envs, client, now)
}

// The orchestrator's HTTP client's limits (plan choice): /health and /_test/faults answer at
// once on loopback.
const (
	httpTimeout       = 10 * time.Second
	httpDialTimeout   = 5 * time.Second
	httpHeaderTimeout = 10 * time.Second
	httpMaxHeader     = 64 << 10
)

// newHTTPClient builds the orchestrator's one HTTP client (AGENTS.md §1 "HTTP"): the instances'
// /health and the /_test/faults posts, all to loopback addresses. No proxy, no compression, no
// keep-alive (so no request is ever resent on a reused connection), redirects answered and not
// followed, and every exchange bounded.
func newHTTPClient() *http.Client {
	tr := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: httpDialTimeout}).DialContext,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ResponseHeaderTimeout:  httpHeaderTimeout,
		MaxResponseHeaderBytes: httpMaxHeader,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// parseFlags parses the command line (plan H3 step 8): --scenario is required;
// --instance-config may repeat, once per instance.
func parseFlags(args []string, stderr io.Writer) (flags, error) {
	fs := flag.NewFlagSet("orchestrator", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := flags{instanceConfigs: map[int]string{}}
	fs.StringVar(&f.scenario, "scenario", "", "the scenario file (DESIGN §B4)")
	fs.BoolVar(&f.baseline, "baseline", false, "run with the limiter off ([api] limiter_enabled = false, HAR-17)")
	fs.Func("instance-config", "N=FILE: instance N gets FILE in place of the run's config.toml (test runs only, T-HAR-1); once per instance",
		func(v string) error {
			n, path, err := parseInstanceConfig(v)
			if err != nil {
				return err
			}
			if _, dup := f.instanceConfigs[n]; dup {
				return fmt.Errorf("instance %d named twice", n)
			}
			f.instanceConfigs[n] = path
			return nil
		})
	fs.StringVar(&f.binDir, "bin-dir", "", "the directory hitapi and rladmin run from (default bin/<run_kind>)")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	switch {
	case fs.NArg() > 0:
		return flags{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	case f.scenario == "":
		return flags{}, errors.New("--scenario is required")
	}
	return f, nil
}

// parseInstanceConfig parses an --instance-config value, N=FILE: N an instance number from 1 to
// runlog.MaxInstances, FILE not empty. Preflight checks N against the scenario's instances and
// loads FILE.
func parseInstanceConfig(v string) (int, string, error) {
	ns, path, ok := strings.Cut(v, "=")
	if !ok || path == "" {
		return 0, "", fmt.Errorf("%q: want N=FILE", v)
	}
	n, err := strconv.Atoi(ns)
	if err != nil || n < 1 || n > runlog.MaxInstances {
		return 0, "", fmt.Errorf("%q: want an instance number from 1 to %d before the =", v, runlog.MaxInstances)
	}
	return n, path, nil
}
