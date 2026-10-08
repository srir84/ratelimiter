// Command hitgen is the harness's load generator (DESIGN §B1, §B4; HARNESS §5, §7;
// IMPLEMENTATION_PLAN H2): it builds a scenario's schedule from its seed, writes it to
// schedule.jsonl before anything is sent, sends it open loop to the hitapi instances, retries
// only after 429, and records every attempt in gen.jsonl, ending with a summary line.
//
//	hitgen --scenario FILE --targets HOST:PORT[,HOST:PORT…] --out DIR
//	       [--thresholds FILE] [--baseline] [--http-timeout 30s] [--max-conns 512]
//
// --targets names one loopback address per instance, in order (HAR-20). With the scenario's
// stop_early, each step is judged while the next one runs (stopearly.go); --thresholds adds
// the frozen per-step thresholds to that, except with --baseline (a baseline run, §B9). On
// SIGTERM hitgen stops launching, waits at most the HTTP timeout for the attempts in flight,
// writes the summary and exits 0.
//
// Exit codes (plan choice): 0 after the run, or after SIGTERM; 1 a runtime failure (the out
// directory, schedule.jsonl, gen.jsonl); 2 a bad command line, a refused scenario or a refused
// thresholds file.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/runlog"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2

	// The client's defaults (§B4 "HTTP client"; plan H2 step 4): a 30 s timeout, and 512
	// connections per instance, hitapi's in-flight cap.
	defaultTimeout  = 30 * time.Second
	defaultMaxConns = 512
	// idleConnTimeout retires an idle keep-alive connection before hitapi closes it: it is
	// coupled to hitapi's idleTimeout (60 s, cmd/hitapi/main.go) and must stay below it. A /hit
	// request is never resent (plan H2 step 4), so one written to a connection the server is
	// just closing would be a false transport_error (TestClient_IdleTimeout).
	idleConnTimeout = 30 * time.Second

	// logDrainTimeout bounds the wait for gen.jsonl's queued lines and its summary (plan
	// choice: the lines go to a local file).
	logDrainTimeout = 30 * time.Second

	scheduleFile = "schedule.jsonl"
	genFile      = "gen.jsonl"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// flags is a parsed command line.
type flags struct {
	scenario, out, thresholds string
	targets                   []string
	baseline                  bool
	timeout                   time.Duration
	maxConns                  int
}

// run runs hitgen: the schedule, then the sending until it is done or ctx ends (SIGTERM). It
// returns the exit code.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	fl, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		logger.Error("bad command line", "error", err)
		return exitUsage
	}
	sc, err := config.LoadScenario(fl.scenario)
	if err != nil {
		logger.Error("refusing the scenario", "error", err)
		return exitUsage
	}
	if len(fl.targets) != sc.Instances {
		logger.Error("bad command line", "error",
			fmt.Errorf("--targets names %d instances, the scenario has %d", len(fl.targets), sc.Instances))
		return exitUsage
	}
	var thresholds []stepThreshold
	if fl.thresholds != "" {
		thresholds, err = loadThresholds(fl.thresholds, len(sc.Steps))
		if err != nil {
			logger.Error("refusing the thresholds file", "error", err)
			return exitUsage
		}
	}

	if err := os.MkdirAll(fl.out, 0o750); err != nil {
		logger.Error("creating the out directory", "error", err)
		return exitFailure
	}
	entries, skipped := BuildSchedule(sc)
	if err := writeSchedule(filepath.Join(fl.out, scheduleFile), entries); err != nil {
		logger.Error("writing the schedule", "error", err)
		return exitFailure
	}
	gen, err := openGenLog(filepath.Join(fl.out, genFile))
	if err != nil {
		logger.Error("opening the run log", "error", err)
		return exitFailure
	}

	client, transport := newClient(fl.timeout, fl.maxConns)
	defer transport.CloseIdleConnections()
	cfg := sendConfig{client: client, timeout: fl.timeout, seed: sc.Seed, retries: sc.Retries, log: gen, logger: logger,
		runNs: sc.RunNs()}
	for _, t := range fl.targets {
		cfg.urls = append(cfg.urls, "http://"+t+"/hit")
	}
	cfg.eval = stopEarlyEvaluator(logger, sc, entries, thresholds, fl.baseline)
	// judges_thresholds: stop_early compares the steps to the thresholds file, not only to the
	// allowed lag; read from the evaluator itself.
	logger.Info("sending", "scenario", sc.Name, "seed", sc.Seed, "entries", len(entries), "targets", strings.Join(fl.targets, ","),
		"judges_thresholds", cfg.eval != nil && cfg.eval.thresholds != nil)
	sum := newSender(cfg, entries).run(ctx)
	sum.SkippedNoEligible = skipped
	return finish(ctx, logger, gen, sum, logDrainTimeout)
}

// stopEarlyEvaluator returns the evaluator of a stop_early scenario, nil without stop_early. It
// judges the thresholds (nil: none) except in a baseline run, which is judged on the lag only
// (§B4 stop_early).
func stopEarlyEvaluator(logger *slog.Logger, sc config.Scenario, entries []Entry, thresholds []stepThreshold, baseline bool) *evaluator {
	if !sc.StopEarly {
		return nil
	}
	if baseline {
		thresholds = nil
	}
	return newEvaluator(logger, entries, stepBounds(sc), thresholds)
}

// openGenLog opens a fresh gen.jsonl at path: a file left by an earlier run in the directory
// is removed first, as schedule.jsonl is replaced, so the file holds this run's lines only.
func openGenLog(path string) (*runlog.Writer, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing an earlier run log: %w", err)
	}
	return runlog.Open(path)
}

// finish waits, at most drainTimeout in all, until every attempt line has reached gen.jsonl,
// then writes the summary with the log's drops (plan H2 step 6) and closes the log. It returns
// the exit code. When a line is still not on the file by then, or its write failed, it logs how
// many and returns 1 without the summary, which can no longer follow every attempt line, and
// without closing the log: Close would wait on a stuck write for good (AGENTS.md §2: Close is
// bounded by its drain timeout). The process's exit releases the file.
func finish(ctx context.Context, logger *slog.Logger, gen *runlog.Writer, sum summary, drainTimeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	if n, err := gen.Drain(ctx); err != nil {
		logger.Error("attempt lines not written: no summary", "pending_lines", n, "error", err)
		return exitFailure
	}
	sum.LogDrops = gen.Drops()
	if sum.LogDrops > 0 {
		logger.Warn("gen.jsonl dropped lines: the run is invalid", "log_drops", sum.LogDrops)
	}
	gen.Enqueue(sum)
	if n, err := gen.Drain(ctx); err != nil {
		logger.Error("the summary line not written", "pending_lines", n, "error", err)
		return exitFailure
	}
	if err := gen.Close(); err != nil {
		logger.Error("closing the run log", "error", err)
		return exitFailure
	}
	return exitOK
}

// newClient builds hitgen's one HTTP client (§B4 "HTTP client"; plan H2 step 4): one
// Transport with keep-alive and maxConns connections per instance, open and idle alike; the
// timeout bounds each whole exchange, dial and body included. No proxy and no compression, and
// a redirect is answered, not followed, so every request on the wire is one hitgen recorded.
func newClient(timeout time.Duration, maxConns int) (*http.Client, *http.Transport) {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		MaxConnsPerHost:     maxConns,
		MaxIdleConnsPerHost: maxConns,
		IdleConnTimeout:     idleConnTimeout,
		DisableCompression:  true,
	}
	client := &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, tr
}

// parseFlags parses the command line (plan H2 step 8). --scenario, --targets and --out are
// required; every target is a loopback IP address with a port (HAR-20); the timeout and the
// connection count are positive.
func parseFlags(args []string, stderr io.Writer) (flags, error) {
	fs := flag.NewFlagSet("hitgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f flags
	var targets string
	fs.StringVar(&f.scenario, "scenario", "", "the scenario file (DESIGN §B4)")
	fs.StringVar(&targets, "targets", "", "the instances' addresses, HOST:PORT for instance 1, 2, … in order, comma-separated")
	fs.StringVar(&f.out, "out", "", "the directory for schedule.jsonl and gen.jsonl, created if missing")
	fs.StringVar(&f.thresholds, "thresholds", "", "the run's frozen thresholds file, met with stop_early unless --baseline")
	fs.BoolVar(&f.baseline, "baseline", false, "a baseline run: stop_early judges the scheduling lag only")
	fs.DurationVar(&f.timeout, "http-timeout", defaultTimeout, "the timeout of one HTTP exchange")
	fs.IntVar(&f.maxConns, "max-conns", defaultMaxConns, "connections per instance")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	switch {
	case fs.NArg() > 0:
		return flags{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	case f.scenario == "" || targets == "" || f.out == "":
		return flags{}, errors.New("--scenario, --targets and --out are required")
	case f.timeout <= 0:
		return flags{}, fmt.Errorf("--http-timeout %v: want more than 0", f.timeout)
	case f.maxConns < 1:
		return flags{}, fmt.Errorf("--max-conns %d: want 1 or more", f.maxConns)
	}
	for _, t := range strings.Split(targets, ",") {
		if err := checkTarget(t); err != nil {
			return flags{}, err
		}
		f.targets = append(f.targets, t)
	}
	return f, nil
}

// checkTarget accepts a loopback IP address and a port, 1 to 65535, such as 127.0.0.1:8081.
func checkTarget(t string) error {
	host, port, err := net.SplitHostPort(t)
	if err != nil {
		return fmt.Errorf("--targets %q: %w", t, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--targets %q: want a loopback IP address such as 127.0.0.1", t)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("--targets %q: want a port from 1 to 65535", t)
	}
	return nil
}
