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
// process group, running. The orchestrator always catches SIGQUIT, as it does SIGTERM (plan H3
// step 4): Go's default, a goroutine dump and exit 2, would end the orchestrator in the middle
// of an in-process store fault (faultctl_rltest.go), leaving a paused store stopped (SIGSTOP)
// or a shut-down one not restarted; caught, it lets the fault's recovery (SIGCONT, the restart)
// and the teardown run, as faultctl's own stop signals do. An ignore inherited at startup never
// keeps SIGQUIT out: Go's runtime installs its own SIGQUIT handler whatever the parent left
// (os/signal keeps an inherited ignore only for SIGHUP and SIGINT), so ignored never reports
// SIGQUIT. A SIGINT or SIGHUP that ignored reports ignored at startup (nohup ignores SIGHUP; a
// shell ignores SIGINT for a background job) is left out, so it stays ignored: catching it
// would undo the ignore.
func stopSignals(ignored func(os.Signal) bool) []os.Signal {
	sigs := []os.Signal{syscall.SIGTERM, syscall.SIGQUIT}
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
	storeProfile    string         // --store-profile; "" means durable (storeProfileFiles)
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
	client, killClient := newHTTPClient(), newKillClient()
	defer client.CloseIdleConnections()
	defer killClient.CloseIdleConnections()
	return runSequence(ctx, logger, s, in, rd, envs, client, killClient, now)
}

// The orchestrator's HTTP client's limits (plan choice): /health and /_test/faults answer at
// once on loopback. A faultctl api kill's client (newKillClient) waits up to killHTTPTimeout
// for one exchange: the pre-kill call answers only once the instance's admitted requests have
// finished, within P + 1 s, and its run log has drained (plan H4 steps 2 and 7), as faultctl's
// own client does.
const (
	httpTimeout       = 10 * time.Second
	httpDialTimeout   = 5 * time.Second
	httpHeaderTimeout = 10 * time.Second
	httpMaxHeader     = 64 << 10
	killHTTPTimeout   = time.Minute
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

// newKillClient builds the HTTP client of a faultctl api kill run as a [[faults]] entry
// (IMPLEMENTATION_PLAN H4 step 7; AGENTS.md §1 "HTTP"): the pre-kill call, within P + 2 s, and
// the /health reads of the restarted instance, each with its own deadline, to loopback. As
// newHTTPClient, but its timeout, response headers included, is killHTTPTimeout, so it never
// cuts the pre-kill call short at the harness's P (5 s); the runner refuses a kill whose P + 2 s
// is longer (faultctl_rltest.go).
func newKillClient() *http.Client {
	tr := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: httpDialTimeout}).DialContext,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ResponseHeaderTimeout:  killHTTPTimeout,
		MaxResponseHeaderBytes: httpMaxHeader,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       killHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// parseFlags parses the command line (plan H3 step 8): --scenario is required;
// --instance-config may repeat, once per instance; --store-profile names one of the store
// profiles (docs/operator-guide.md "Store profiles"), durable by default.
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
	fs.StringVar(&f.storeProfile, "store-profile", profileDurable,
		"the store profile: durable, everysec, barrier or buffered (SPEC STA-16, STA-16a)")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	switch {
	case fs.NArg() > 0:
		return flags{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	case f.scenario == "":
		return flags{}, errors.New("--scenario is required")
	}
	if f.storeProfile == "" {
		return flags{}, errors.New("--store-profile is empty")
	}
	if _, _, err := storeProfileFiles(f.storeProfile); err != nil {
		return flags{}, err
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
