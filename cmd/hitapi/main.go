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
	"syscall"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/dbclient"
	"ratelimiter/harness/httpapi"
	"ratelimiter/harness/runlog"
	"ratelimiter/limiter"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2

	// minNofile is the lowest soft RLIMIT_NOFILE hitapi starts with: 12 store + 64 DB + 512
	// HTTP descriptors, plus spare (§B1).
	minNofile = 1024

	// Server timeouts (plan choices): a /hit answers within P from just before its acquire,
	// plus its last steps, so writes get P + writeSlack. idleTimeout is coupled to hitgen's
	// idleConnTimeout (cmd/hitgen/main.go), which must stay below it: hitgen never resends a
	// /hit request, so one written to a connection hitapi is just closing would fail
	// (cmd/hitgen TestClient_IdleTimeout).
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	idleTimeout       = 60 * time.Second
	writeSlack        = 10 * time.Second

	// logDrainTimeout bounds the wait for the run log's channel to empty before the stop
	// record, from the limiter's Close returning (plan choice: the lines go to a local file).
	// It is SIGTERM's last timed wait. The timed worst case, P (stopRequests) + drain_timeout +
	// the limiter's listener grace (at most 100 ms past the drain) + logDrainTimeout = 5 s +
	// 35 s + 0.1 s + 4 s = 44.1 s at harness values, leaves 0.9 s of the orchestrator's 45 s
	// kill grace (P + drain_timeout + 5 s, DESIGN §B10) for the steps hitapi does not time, in
	// order: the drain's cancel of the refund attempts in flight, the limiter's shutdown, the
	// run log writing its remaining lines and closing, the database pool's close and the exit.
	// They take milliseconds while the disk and PostgreSQL answer; a disk or PostgreSQL stall
	// can hold them past the kill grace. TestShutdownBudget pins the timed sum.
	logDrainTimeout = 4 * time.Second
)

func main() {
	processStart := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := run(ctx, processStart, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// flags is a parsed command line.
type flags struct {
	config, listen, log, runID string
	instance                   int
}

// built is what construct returns: the limiter, the database the handler uses, and what the
// build adds to /health and the routes.
type built struct {
	lim              *limiter.Limiter
	listenerQueueLen func() int       // rltest builds: limitertest.ListenerQueueLen; nil in bench builds
	db               httpapi.DB       // rltest builds: the database wrapper (FLT-5, FLT-9); bench: the client
	fixtures         httpapi.Fixtures // rltest builds: the fixtures the /_test routes drive; bench: empty
}

// run runs hitapi until ctx ends (SIGTERM) or serving fails, and returns the exit code.
func run(ctx context.Context, processStart time.Time, args []string, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	fl, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		logger.Error("bad command line", "error", err)
		return exitUsage
	}
	file, err := loadConfig(fl.config)
	if err != nil {
		logger.Error("refusing to start", "error", err)
		return exitUsage
	}
	nofile, err := nofileLimit()
	if err != nil {
		logger.Error("reading the file descriptor limit", "error", err)
		return exitFailure
	}
	if err := checkNofile(nofile); err != nil {
		logger.Error("refusing to start", "error", err, "nofile_limit", nofile, "minimum", minNofile)
		return exitUsage
	}

	// Shutdown work must outlive the signal that starts it.
	base := context.WithoutCancel(ctx)
	rl, err := runlog.Open(fl.log)
	if err != nil {
		logger.Error("opening the run log", "error", err)
		return exitFailure
	}
	db, err := dbclient.Open(base, file.API.DBDSN)
	if err != nil {
		logger.Error("opening the database client", "error", err)
		closeRunLog(logger, rl)
		return exitFailure
	}
	b, err := construct(file, db, httpapi.RefundListener(rl))
	if err != nil {
		// New never returns nil: this limiter stays not ready (settings_invalid), so every /hit
		// gets ERR-12a and /health says why.
		logger.Error("building the limiter", "error", err)
	}
	api, err := httpapi.New(httpapi.Config{
		API:              file.API,
		Instance:         file.Instance,
		RunID:            fl.runID,
		InstanceNum:      fl.instance,
		ProcessStart:     processStart,
		NofileLimit:      nofile,
		ListenerQueueLen: b.listenerQueueLen,
		Fixtures:         b.fixtures,
	}, httpapi.FromLimiter(b.lim), b.db, rl)
	if err != nil {
		logger.Error("building the API", "error", err)
		b.lim.Close()
		closeRunLog(logger, rl)
		db.Close()
		return exitUsage
	}

	// finish closes the limiter, drains the run log, writes the stop record and closes
	// everything (HAR-15).
	finish := func(code int) int {
		if err := api.Stop(base, logDrainTimeout); err != nil {
			logger.Warn("the run log was not drained before the stop record", "error", err)
		}
		if !closeRunLog(logger, rl) {
			code = exitFailure
		}
		db.Close()
		return code
	}

	api.LogStart()
	ln, err := (&net.ListenConfig{}).Listen(base, "tcp", fl.listen)
	if err != nil {
		logger.Error("listening", "addr", fl.listen, "error", err)
		return finish(exitFailure)
	}
	p := time.Duration(file.Instance.MaxReleaseDelayNs)
	srv := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      p + writeSlack,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		// "OPTIONS *" goes to the handler, which answers it with JSON and no-store (HTTP-7,
		// HTTP-8), not to net/http's own reply.
		DisableGeneralOptionsHandler: true,
	}
	api.LogReady()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.Info("serving", "addr", ln.Addr().String(), "instance", fl.instance, "run_id", fl.runID)

	code := exitOK
	select {
	case <-ctx.Done():
		logger.Info("signal received: shutting down")
	case err := <-served:
		logger.Error("serving stopped", "error", err)
		code = exitFailure
		served = nil
	}
	// HAR-15: stop accepting /hit, refuse work not yet at its first DB statement (ERR-15),
	// and wait at most P for the rest.
	stopRequests(base, logger, api, srv, p)
	if served != nil {
		<-served // Serve returns as soon as Shutdown closes the listener
	}
	return finish(code)
}

// requestStopper is the API's part of the SIGTERM sequence (httpapi.Server).
type requestStopper interface {
	BeginShutdown()
	WaitIdle(ctx context.Context) error
}

// listenerStopper is the HTTP server's part of the SIGTERM sequence (http.Server).
type listenerStopper interface {
	Shutdown(ctx context.Context) error
}

// stopRequests runs the SIGTERM steps before Stop (HAR-15): BeginShutdown first, so a /hit
// not yet at its first DB statement is refused (ERR-15) and a new one gets ERR-7 while the
// server waits; then http.Server.Shutdown (stop accepting, wait for the handlers) and
// WaitIdle, together bounded by P.
func stopRequests(base context.Context, logger *slog.Logger, api requestStopper, srv listenerStopper, p time.Duration) {
	api.BeginShutdown()
	pCtx, cancel := context.WithTimeout(base, p)
	defer cancel()
	if err := srv.Shutdown(pCtx); err != nil {
		logger.Warn("requests still running after P", "error", err)
	}
	if err := api.WaitIdle(pCtx); err != nil {
		logger.Warn("requests still running after P", "error", err)
	}
}

// closeRunLog closes the run log, which writes every line still queued; false on an error.
func closeRunLog(logger *slog.Logger, rl *runlog.Writer) bool {
	if err := rl.Close(); err != nil {
		logger.Error("closing the run log", "error", err)
		return false
	}
	return true
}

// parseFlags parses the command line (plan choice for the flags). --config, --listen and
// --log are required; --listen must be a loopback address.
func parseFlags(args []string, stderr io.Writer) (flags, error) {
	fs := flag.NewFlagSet("hitapi", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f flags
	fs.StringVar(&f.config, "config", "", "the harness config file (DESIGN §B2)")
	fs.StringVar(&f.listen, "listen", "", "the loopback address to serve on, 127.0.0.1:PORT")
	fs.StringVar(&f.log, "log", "", "the run-log file to append to (api-N.jsonl)")
	fs.IntVar(&f.instance, "instance", 1, "this instance's number N, from 1")
	fs.StringVar(&f.runID, "run-id", "", "the run ID that request records carry")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	switch {
	case fs.NArg() > 0:
		return flags{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	case f.config == "" || f.listen == "" || f.log == "":
		return flags{}, errors.New("--config, --listen and --log are required")
	case f.instance < 1:
		return flags{}, fmt.Errorf("--instance %d: want 1 or more", f.instance)
	}
	host, _, err := net.SplitHostPort(f.listen)
	if err != nil {
		return flags{}, fmt.Errorf("--listen %q: %w", f.listen, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return flags{}, fmt.Errorf("--listen %q: want a loopback IP address such as 127.0.0.1", f.listen)
	}
	return f, nil
}

// loadConfig loads the config file and refuses one without the store password, which
// config.Load leaves to the process that connects (RL_LIB_PASSWORD; DESIGN §A2, §B2).
func loadConfig(path string) (config.File, error) {
	f, err := config.Load(path)
	if err != nil {
		return config.File{}, fmt.Errorf("loading the config: %w", err)
	}
	if f.Store.Password == "" {
		return config.File{}, fmt.Errorf("the store password variable %s is not set", config.LibPasswordEnv)
	}
	return f, nil
}

// checkNofile refuses a soft RLIMIT_NOFILE below minNofile (§B1).
func checkNofile(n uint64) error {
	if n < minNofile {
		return fmt.Errorf("soft RLIMIT_NOFILE %d is below the minimum %d", n, minNofile)
	}
	return nil
}

// nofileLimit returns the effective soft RLIMIT_NOFILE (§B1).
func nofileLimit() (uint64, error) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0, fmt.Errorf("reading RLIMIT_NOFILE: %w", err)
	}
	return rl.Cur, nil
}
