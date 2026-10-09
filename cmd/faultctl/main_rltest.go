//go:build rltest

// Command faultctl arms the harness's process and store faults from the command line (DESIGN
// §B6; IMPLEMENTATION_PLAN H4 step 3), on the run whose directory --run names:
//
//	faultctl --run runs/<id> store down|pause <duration>
//	faultctl --run runs/<id> api kill <N> [--restart]
//	faultctl --run runs/<id> tamper <op> [--key K] [--member g:s] [--field f] [--value v]
//	         [--target ix|q|epoch] [--epoch ID]
//	faultctl --run runs/<id> clock set|advance|back <ns>
//
// It is a thin layer over harness/fixtures, built only into bin/test (§B1): the command grammar
// (ParseFaultCommand: the tamper operations, one per §B6 item, and the flags each takes are
// listed at fixtures.TamperCommand), StoreDown and StorePause (FLT-1 a, FLT-2), KillAPI
// (FLT-6), the S8 tamper tool (FLT-7, FLT-11) and the S5 controlled clock driver. The
// orchestrator's fault runner runs the same functions in-process (plan H4 step 7).
//
// The run directory gives the store's address (config.toml's [store] addr) and the run's
// processes (pids.json). store down rebuilds the store's exact command line from what the
// running store reports (runlog.StoreArgs) and writes the restarted store's PID into
// pids.json; api kill reads the instance's command line from its process and writes a
// restarted instance's PID into pids.json, so the sampler finds either. faultctl is for stacks
// without an orchestrator: every command changes the store or a process, which an orchestrated
// run's evidence would not show (its context.json records the scenario's [[faults]] alone),
// and a process faultctl restarts is detached from it: an orchestrator does not reach it with
// its final SIGTERM, and would fail the run. So faultctl refuses every command while the run's
// orchestrator is live (plan choice: .data/orchestrator.pid in the repository root, the run
// directory's grandparent, names a live process; the orchestrator holds it for its whole run):
// a scenario's [[faults]] arm them instead, run inside the orchestrator and recorded in
// context.json (plan H4 step 7). A standalone stack's run directory (config.toml and pids.json,
// no orchestrator) is accepted, with or without a context.json.
// Passwords come only from the environment (DESIGN §A2): RL_OPS_PASSWORD for the store faults,
// RL_FIXTURE_PASSWORD for tamper and clock, and RL_LIB_PASSWORD, handed to the restarted
// instance, for api kill --restart. rl_fixture cannot read the epoch record, so the operations
// that change the epoch's namespace take its ID from --epoch, or else from the epoch_id an
// instance of the run serves on GET /health (the instance found through pids.json and its
// --listen).
//
// It prints one JSON line on stdout per command: cmd, start_ns and end_ns, the command's own
// start and end (wall-clock ns, DESIGN §B5 "Faults"), and for a store fault whose outage was
// measured, outage_start_ns (plan choice: the wall-clock ns just before SHUTDOWN or SIGSTOP)
// and outage_ns (from there to the first PING answered, on the monotonic clock); error when
// the command failed. Errors and the usage go to stderr as JSON log lines. SIGTERM and SIGQUIT
// end a command early, always (Go's runtime replaces a SIGQUIT ignore faultctl inherits with
// its own handler), and so do SIGINT and SIGHUP (a closed terminal) unless faultctl started
// with them ignored (stopSignals); a fault's own recovery still runs: the store gets SIGCONT
// after a pause, and comes back after a down.
//
// Exit codes (plan choice): 0 ok; 1 the command failed; 2 bad arguments or environment, nothing
// done.
package main

import (
	"context"
	"encoding/json"
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
	"ratelimiter/harness/fixtures"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2

	// The variables that carry rl_ops's and rl_fixture's passwords (DESIGN §A2).
	envOps     = "RL_OPS_PASSWORD"
	envFixture = "RL_FIXTURE_PASSWORD"

	// The HTTP client's limits (plan choice): api kill makes two kinds of request, the pre-kill
	// call (within P + 2 s, 7 s at harness values) and the /health reads of a restarted instance
	// (1 s each); each has its own deadline, and the client's timeout bounds any one exchange.
	httpTimeout     = time.Minute
	httpDialTimeout = 5 * time.Second
	httpMaxHeader   = 64 << 10
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals(signal.Ignored)...)
	code := run(ctx, os.Args[1:], os.LookupEnv, os.Environ(), newClient(), time.Now, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// stopSignals are the signals that end a command early, as SIGTERM does: its fault's recovery
// runs (package comment), where the signal's default action would end faultctl before it. The
// rule is cmd/orchestrator's. SIGTERM and SIGQUIT always: Go's runtime installs its own SIGQUIT
// handler at startup even when faultctl inherits SIGQUIT ignored (os/signal keeps an inherited
// ignore only for SIGHUP and SIGINT, so ignored never reports SIGQUIT), and uncaught, a SIGQUIT
// gets Go's default, a goroutine dump and exit 2, which would leave a paused store stopped.
// SIGINT and SIGHUP (a closed terminal, a dropped SSH session) only when ignored reports them
// not ignored at startup: nohup ignores SIGHUP and a shell ignores SIGINT for a background job;
// catching either would undo that ignore, and a nohup'd store pause would end early on a
// hangup.
func stopSignals(ignored func(os.Signal) bool) []os.Signal {
	sigs := []os.Signal{syscall.SIGTERM, syscall.SIGQUIT}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGHUP} {
		if !ignored(sig) {
			sigs = append(sigs, sig)
		}
	}
	return sigs
}

// newClient builds faultctl's HTTP client for api kill (AGENTS.md §1): explicit timeouts; no
// proxy and no redirect; keep-alive off, so each request has a fresh connection and the
// transport never resends one on a reused connection that failed (AGENTS.md §6).
func newClient() *http.Client {
	tr := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: httpDialTimeout}).DialContext,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ResponseHeaderTimeout:  httpTimeout,
		MaxResponseHeaderBytes: httpMaxHeader,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// secrets are the store passwords the command uses, read from the environment; the others
// stay "". Formatting or logging a secrets value shows none of them.
type secrets struct {
	ops, fixture, lib string
}

func (secrets) String() string       { return "secrets{redacted}" }
func (secrets) GoString() string     { return "secrets{redacted}" }
func (secrets) LogValue() slog.Value { return slog.StringValue("redacted") }

// invocation is a checked command line: the run directory, the command and its passwords.
type invocation struct {
	run string
	cmd fixtures.FaultCommand
	pw  secrets
}

// run runs one faultctl command and returns the exit code. lookupEnv and environ are the
// process's environment (os.LookupEnv and os.Environ in main): environ, less its RL_
// variables, is the environment of a process faultctl restarts. client makes api kill's
// requests; now reads the wall clock for the command's start and end (time.Now in main); the
// command's line goes to stdout, the log to stderr.
func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), environ []string,
	client *http.Client, now func() time.Time, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	inv, err := parseInvocation(args, lookupEnv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		logger.Error("bad command line", "error", err)
		return exitUsage
	}
	if err := resolveEpoch(ctx, &inv, client); err != nil {
		logger.Error("bad command line", "error", err)
		return exitUsage
	}
	rep, err := execute(ctx, inv, environ, client, now)
	if err != nil {
		rep.Error = err.Error()
	}
	line, merr := json.Marshal(rep)
	if merr != nil {
		logger.Error("encoding the command's line", "cmd", inv.cmd.Line, "error", merr)
		return exitFailure
	}
	if _, werr := stdout.Write(append(line, '\n')); werr != nil {
		logger.Error("writing the command's line", "cmd", inv.cmd.Line, "error", werr)
		return exitFailure
	}
	if err != nil {
		logger.Error("the command failed", "cmd", inv.cmd.Line, "error", err)
		return exitFailure
	}
	return exitOK
}

// usageText is the usage -h prints.
const usageText = `usage:
  faultctl --run runs/<id> store down|pause <duration>
  faultctl --run runs/<id> api kill <N> [--restart]
  faultctl --run runs/<id> tamper <op> [--key K] [--member g:s] [--field f] [--value v]
           [--target ix|q|epoch] [--epoch ID]
  faultctl --run runs/<id> clock set|advance|back <ns>
tamper operations: del-bucket-fields, del-k, del-mi, del-mi-member, del-ix, del-marker,
  corrupt-field, add-bad-ix, retype-shared, del-hwm, corrupt-record, wipe
  (without --epoch, the epoch_id an instance of the run serves on GET /health)
faultctl is for stacks without an orchestrator: every command changes the store or a process,
  which a run's records would not show, and a process it restarts would outlive the
  orchestrator and fail the run, so every command is refused while the run's orchestrator is
  live (.data/orchestrator.pid names a live process); arm the command as one of the
  scenario's [[faults]] instead, which context.json records
`

// parseInvocation parses and checks the command line: --run, a directory, then the command
// (fixtures.ParseFaultCommand), no live orchestrator, for any command (refuseUnderOrchestrator),
// and the password each command needs (RL_OPS_PASSWORD for the store faults,
// RL_FIXTURE_PASSWORD for tamper and clock, RL_LIB_PASSWORD for api kill --restart).
func parseInvocation(args []string, lookupEnv func(string) (string, bool), stderr io.Writer) (invocation, error) {
	fs := flag.NewFlagSet("faultctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(fs.Output(), usageText) }
	var inv invocation
	fs.StringVar(&inv.run, "run", "", "the run directory, runs/<id>: config.toml and pids.json are read from it")
	if err := fs.Parse(args); err != nil {
		return invocation{}, err
	}
	cmd, err := fixtures.ParseFaultCommand(fs.Args())
	if err != nil {
		return invocation{}, err
	}
	inv.cmd = cmd
	if inv.run == "" {
		return invocation{}, errors.New("--run is required: the run directory, runs/<id>")
	}
	if fi, err := os.Stat(inv.run); err != nil || !fi.IsDir() {
		return invocation{}, fmt.Errorf("--run %s: not a directory", inv.run)
	}
	// Every command changes the store or a process (store down and pause, api kill with or
	// without --restart, tamper, clock): none runs beside the run's live orchestrator.
	if err := refuseUnderOrchestrator(inv.run, cmd.Line); err != nil {
		return invocation{}, err
	}
	need := func(name string) (string, error) {
		v, _ := lookupEnv(name)
		if v == "" {
			return "", fmt.Errorf("%s is not set: %s needs it", name, cmd.Op)
		}
		return v, nil
	}
	switch cmd.Op {
	case fixtures.OpStoreDown, fixtures.OpStorePause:
		inv.pw.ops, err = need(envOps)
	case fixtures.OpTamper:
		inv.pw.fixture, err = need(envFixture)
	case fixtures.OpClockSet, fixtures.OpClockAdvance, fixtures.OpClockBack:
		inv.pw.fixture, err = need(envFixture)
	case fixtures.OpAPIKill:
		if cmd.Restart {
			inv.pw.lib, err = need(config.LibPasswordEnv)
		}
	}
	if err != nil {
		return invocation{}, err
	}
	return inv, nil
}

// resolveEpoch fills in the epoch ID of a tamper operation that changes the epoch's namespace
// when --epoch did not give it: the epoch_id an instance of the run serves (runEpoch). It
// leaves every other command as it is. Its error means nothing was done: exit 2.
func resolveEpoch(ctx context.Context, inv *invocation, client *http.Client) error {
	tc := &inv.cmd.Tamper
	if inv.cmd.Op != fixtures.OpTamper || !tc.NeedsEpoch() || tc.EpochID != "" {
		return nil
	}
	id, err := runEpoch(ctx, inv.run, client)
	if err != nil {
		return fmt.Errorf("tamper %s needs the epoch's ID (rl_fixture cannot read the epoch record): no --epoch ID, and the run's instances give none: %w", tc.Op, err)
	}
	tc.EpochID = id
	return nil
}
