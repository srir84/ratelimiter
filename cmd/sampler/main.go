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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/srir84/ratelimiter/harness/runlog"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2

	samplesFile = "samples.jsonl"

	defaultInterval = time.Second
	// minInterval is the shortest interval accepted (plan choice): reads get 4/5 of it.
	minInterval   = 100 * time.Millisecond
	defaultPGData = ".data/pg"
	// drainTimeout bounds the wait for samples.jsonl's queued lines at exit (plan choice: the
	// lines go to a local file).
	drainTimeout = 5 * time.Second

	// The environment (DESIGN §A2, §A6).
	envPrefix        = "RL_"
	envStoreAddr     = "RL_STORE_ADDR"
	envOpsPassword   = "RL_OPS_PASSWORD"
	envAdminPassword = "RL_ADMIN_PASSWORD"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := run(ctx, os.Args[1:], os.LookupEnv, os.Environ(), os.Stderr)
	stop()
	os.Exit(code)
}

// flags is a parsed command line.
type flags struct {
	runDir, rladmin, pgData string
	apis                    []string
	interval                time.Duration
}

// run runs the sampler until ctx ends (SIGTERM) and returns the exit code. lookupEnv and
// environ are the process's environment (os.LookupEnv and os.Environ in main).
func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), environ []string, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	fl, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		logger.Error("bad command line", "error", err)
		return exitUsage
	}
	access, err := storeAccessFrom(lookupEnv, environ)
	if err != nil {
		logger.Error("bad environment", "error", err)
		return exitUsage
	}
	out, err := runlog.Open(filepath.Join(fl.runDir, samplesFile))
	if err != nil {
		logger.Error("opening samples.jsonl", "error", err)
		return exitFailure
	}
	cfg := samplerConfig{runDir: fl.runDir, apis: fl.apis, rladmin: fl.rladmin, pgData: fl.pgData,
		interval: fl.interval, timeout: readTimeout(fl.interval), waitDelay: fl.interval / 10}
	s := newSampler(cfg, access, newClient(cfg.timeout), out)
	logger.Info("sampling", "run_dir", fl.runDir, "apis", strings.Join(fl.apis, ","), "interval", fl.interval.String(),
		"read_timeout", cfg.timeout.String())
	s.run(ctx)
	return finish(ctx, logger, out)
}

// readTimeout is the deadline of every read in a tick: 4/5 of the interval (plan choice), which
// leaves the rest of it for writing the line, and for rladmin's output after a kill (1/10).
func readTimeout(interval time.Duration) time.Duration { return interval - interval/5 }

// finish waits, at most drainTimeout, until every queued line is on samples.jsonl, then closes
// it, and returns the exit code. A line not written by then is reported, and the file is left
// to the process's exit: Close would wait on a stuck write for good.
func finish(ctx context.Context, logger *slog.Logger, out *runlog.Writer) int {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	if n, err := out.Drain(ctx); err != nil {
		logger.Error("sample lines not written", "pending_lines", n, "error", err)
		return exitFailure
	}
	if err := out.Close(); err != nil {
		logger.Error("closing samples.jsonl", "error", err)
		return exitFailure
	}
	if drops := out.Drops(); drops > 0 {
		logger.Warn("samples.jsonl dropped lines", "log_drops", drops)
	}
	logger.Info("stopped")
	return exitOK
}

// newClient builds the sampler's HTTP client for /health and /metrics: every exchange,
// dial and body included, bounded by timeout; no proxy and no redirect. Keep-alive is off, so
// each read has a fresh connection and the transport never resends one on a reused connection
// that failed (AGENTS.md §6).
func newClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		DisableCompression:    true,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// storeAccessFrom reads the store address and the two passwords from the environment
// (DESIGN §A2): each must be set, and the address be HOST:PORT. environ is the base of
// rladmin's environment.
func storeAccessFrom(lookupEnv func(string) (string, bool), environ []string) (storeAccess, error) {
	vals := map[string]string{}
	var missing []string
	for _, name := range []string{envStoreAddr, envOpsPassword, envAdminPassword} {
		v, _ := lookupEnv(name)
		if v == "" {
			missing = append(missing, name)
		}
		vals[name] = v
	}
	if len(missing) > 0 {
		return storeAccess{}, fmt.Errorf("%s not set", strings.Join(missing, ", "))
	}
	addr := vals[envStoreAddr]
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return storeAccess{}, fmt.Errorf("%s: %w", envStoreAddr, err)
	}
	return newStoreAccess(addr, vals[envOpsPassword], vals[envAdminPassword], environ), nil
}

// parseFlags parses the command line (plan H3 step 7; --rladmin and --pg-data are plan
// choices). --run-dir, --apis and --rladmin are required; --apis names 1 to 3 distinct
// loopback addresses (HAR-20), instance 1 first; the interval is at least minInterval.
func parseFlags(args []string, stderr io.Writer) (flags, error) {
	fs := flag.NewFlagSet("sampler", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f flags
	var apis string
	fs.StringVar(&f.runDir, "run-dir", "", "the run directory: pids.json is read from it, samples.jsonl written to it")
	fs.StringVar(&apis, "apis", "", "the hitapi instances' addresses, HOST:PORT for instance 1, 2, … in order, comma-separated")
	fs.DurationVar(&f.interval, "interval", defaultInterval, "the time between samples")
	fs.StringVar(&f.rladmin, "rladmin", "", "the rladmin binary that runs status --json (bin/<run_kind>/rladmin)")
	fs.StringVar(&f.pgData, "pg-data", defaultPGData, "the PostgreSQL data directory, which holds postmaster.pid")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	switch {
	case fs.NArg() > 0:
		return flags{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	case f.runDir == "" || apis == "" || f.rladmin == "":
		return flags{}, errors.New("--run-dir, --apis and --rladmin are required")
	case f.pgData == "":
		return flags{}, errors.New("--pg-data: want a directory")
	case f.interval < minInterval:
		return flags{}, fmt.Errorf("--interval %v: want %v or more", f.interval, minInterval)
	}
	for _, a := range strings.Split(apis, ",") {
		if err := checkAddr(a); err != nil {
			return flags{}, err
		}
		if slices.Contains(f.apis, a) {
			return flags{}, fmt.Errorf("--apis names %s twice", a)
		}
		f.apis = append(f.apis, a)
	}
	if len(f.apis) > runlog.MaxInstances {
		return flags{}, fmt.Errorf("--apis names %d instances: want 1 to %d", len(f.apis), runlog.MaxInstances)
	}
	return f, nil
}

// checkAddr accepts a loopback IP address and a port, 1 to 65535, such as 127.0.0.1:8081.
func checkAddr(a string) error {
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		return fmt.Errorf("--apis %q: %w", a, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--apis %q: want a loopback IP address such as 127.0.0.1", a)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("--apis %q: want a port from 1 to 65535", a)
	}
	return nil
}
