package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ratelimiter/internal/keys"
)

// usage is what --help prints: one line per subcommand (plan choice).
const usage = `rladmin start-epoch --policy FILE [--replace --expect-epoch ID] [--store ADDR] [--json]
rladmin status [--store ADDR] [--json]
rladmin inspect --key KEY [--store ADDR] [--json]
rladmin restore --policy FILE --key KEY [--balance-units U | --balance-tokens T] --expect-epoch ID [--store ADDR] [--json]
rladmin clear --policy FILE --key KEY --expect-epoch ID [--store ADDR] [--json]
rladmin store-info [--store ADDR] [--json]
rladmin store-rewrite [--store ADDR] [--json]
`

// Deadlines (plan choices): every command's connection, AUTH and exchanges end within
// commandTimeout; store-rewrite's BGREWRITEAOF and its wait for the rewrite, within
// rewriteTimeout. A signal ends either at once.
const (
	commandTimeout = 30 * time.Second
	rewriteTimeout = 10 * time.Minute
)

func main() {
	ctx, stop := signalContext()
	code := run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// signalContext is main's root context: the first SIGINT or SIGTERM cancels it, which ends
// every store wait (exit 3). Handling that first signal also releases both signals
// (context.AfterFunc(ctx, stop)), so a SIGINT or SIGTERM after the first has been handled takes
// its default action, which ends rladmin by that signal (with no exit code) wherever it waits
// without a context, such as a policy file read held in the kernel (loadPolicy) or a write to
// a pipe nobody reads, unless the kernel holds that call uninterruptibly (a macOS hard mount
// without intr, where even SIGKILL waits). One that arrives together with the first, before
// the release, only cancels again. A SIGINT that was ignored when rladmin started (a script's
// background job) still cancels the first time, since Notify stops it being ignored, but the
// release ignores it again (os/signal), so from then on only SIGTERM ends rladmin.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// invocation is a parsed command line and environment.
type invocation struct {
	cmd        string
	addr, user string
	json       bool
	// withPolicy: the subcommand takes --policy, so run always loads and validates the policy
	// file before connecting (parse has refused a missing or empty --policy).
	withPolicy          bool
	policyPath          string
	key, enc            string
	expect              string
	replace             bool
	units, tokens       int64
	unitsSet, tokensSet bool
}

// run runs one rladmin command line and returns its exit code. lookupEnv reads the
// environment (os.LookupEnv in main); everything run writes to stdout and stderr goes through
// a redactor, so the password never reaches either.
func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	secret, _ := lookupEnv("RL_ADMIN_PASSWORD")
	out, errOut := newRedactor(stdout, secret), newRedactor(stderr, secret)
	fail := func(e *cmdError) int {
		failure(errOut, e, secret)
		return e.code
	}
	inv, err := parse(args, lookupEnv, secret)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(out, usage) // CLI output: a failed write changes nothing
		return exitOK
	}
	if err != nil {
		return fail(&cmdError{code: exitRefused, msg: err.Error(), hint: "usage:\n" + strings.TrimSuffix(usage, "\n")})
	}
	var pol policyInfo
	if inv.withPolicy {
		if pol, err = loadPolicy(inv.policyPath); err != nil {
			return fail(&cmdError{code: exitRefused, msg: "invalid policy: " + err.Error()})
		}
	}
	timeout := commandTimeout
	if inv.cmd == "store-rewrite" {
		timeout = rewriteTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := dial(ctx, inv.addr, inv.user, secret)
	if err != nil {
		return fail(&cmdError{code: exitStore, msg: err.Error()})
	}
	defer c.close()
	text, err := execute(ctx, c, inv, pol)
	if err != nil {
		var ce *cmdError
		if !errors.As(err, &ce) {
			ce = &cmdError{code: exitStore, msg: err.Error()}
		}
		return fail(ce)
	}
	_, _ = out.Write(text) // CLI output: a failed write changes nothing
	return exitOK
}

// execute runs inv's command on c and renders its result.
func execute(ctx context.Context, c *conn, inv invocation, pol policyInfo) ([]byte, error) {
	var fs []field
	var err error
	switch inv.cmd {
	case "start-epoch":
		fs, err = startEpoch(ctx, c, pol, inv.replace, inv.expect)
	case "status":
		fs, err = status(ctx, c)
	case "inspect":
		fs, err = inspect(ctx, c, inv.enc)
	case "restore":
		balance := balanceArg(inv.units, inv.unitsSet, inv.tokens, inv.tokensSet, pol.p.RatePeriodNs)
		fs, err = restoreKey(ctx, c, pol, inv.enc, inv.expect, balance)
	case "clear":
		fs, err = clearKey(ctx, c, pol, inv.enc, inv.expect)
	case "store-info":
		si, err := fetchStoreInfo(ctx, c)
		if err != nil {
			return nil, err
		}
		return renderStoreInfo(si, inv.json)
	case "store-rewrite":
		fs, err = storeRewrite(ctx, c, rewritePoll)
	default:
		return nil, fmt.Errorf("unknown subcommand %q", inv.cmd)
	}
	if err != nil {
		return nil, err
	}
	return renderFields(fs, inv.json)
}

// parse reads the subcommand, its flags (DESIGN §A6) and the environment. It returns
// flag.ErrHelp for --help, and any other error for a usage error.
func parse(args []string, lookupEnv func(string) (string, bool), secret string) (invocation, error) {
	if len(args) == 0 {
		return invocation{}, errors.New("no subcommand")
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		return invocation{}, flag.ErrHelp
	}
	inv := invocation{cmd: args[0]}
	fs := flag.NewFlagSet("rladmin "+inv.cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&inv.addr, "store", "", "")
	fs.BoolVar(&inv.json, "json", false, "")
	var required []string
	var unitsText, tokensText string
	switch inv.cmd {
	case "start-epoch":
		fs.StringVar(&inv.policyPath, "policy", "", "")
		fs.BoolVar(&inv.replace, "replace", false, "")
		fs.StringVar(&inv.expect, "expect-epoch", "", "")
		inv.withPolicy, required = true, []string{"policy"}
	case "status", "store-info", "store-rewrite":
	case "inspect":
		fs.StringVar(&inv.key, "key", "", "")
		required = []string{"key"}
	case "restore":
		fs.StringVar(&inv.policyPath, "policy", "", "")
		fs.StringVar(&inv.key, "key", "", "")
		fs.StringVar(&inv.expect, "expect-epoch", "", "")
		fs.StringVar(&unitsText, "balance-units", "", "")
		fs.StringVar(&tokensText, "balance-tokens", "", "")
		inv.withPolicy, required = true, []string{"policy", "key", "expect-epoch"}
	case "clear":
		fs.StringVar(&inv.policyPath, "policy", "", "")
		fs.StringVar(&inv.key, "key", "", "")
		fs.StringVar(&inv.expect, "expect-epoch", "", "")
		inv.withPolicy, required = true, []string{"policy", "key", "expect-epoch"}
	default:
		return inv, fmt.Errorf("unknown subcommand %q", inv.cmd)
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return inv, err
		}
		return inv, fmt.Errorf("%s: %w", inv.cmd, err)
	}
	if fs.NArg() > 0 {
		return inv, fmt.Errorf("%s: unexpected argument %q", inv.cmd, fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, name := range required {
		if !set[name] {
			return inv, fmt.Errorf("%s: --%s is required", inv.cmd, name)
		}
		// Given but empty (an unset shell variable) is no value: `--policy ""` would otherwise
		// send a zero policy, fingerprint "" included.
		if fs.Lookup(name).Value.String() == "" {
			return inv, fmt.Errorf("%s: --%s is empty", inv.cmd, name)
		}
	}
	inv.unitsSet, inv.tokensSet = set["balance-units"], set["balance-tokens"]
	switch {
	case inv.unitsSet && inv.tokensSet:
		return inv, fmt.Errorf("%s: give --balance-units or --balance-tokens, not both", inv.cmd)
	case inv.replace != set["expect-epoch"] && inv.cmd == "start-epoch":
		return inv, fmt.Errorf("%s: --replace and --expect-epoch go together", inv.cmd)
	case set["expect-epoch"] && !isEpochID(inv.expect):
		return inv, fmt.Errorf("%s: --expect-epoch must be an epoch ID: 32 lowercase hex characters", inv.cmd)
	}
	var err error
	if inv.unitsSet {
		if inv.units, err = parseDecimal(unitsText); err != nil {
			return inv, fmt.Errorf("%s: --balance-units %w", inv.cmd, err)
		}
	}
	if inv.tokensSet {
		if inv.tokens, err = parseDecimal(tokensText); err != nil {
			return inv, fmt.Errorf("%s: --balance-tokens %w", inv.cmd, err)
		}
	}
	if set["key"] {
		if inv.enc, err = keys.Encode(inv.key); err != nil {
			return inv, fmt.Errorf("%s: --key: %w", inv.cmd, err)
		}
	}
	if !set["store"] {
		inv.addr, _ = lookupEnv("RL_STORE_ADDR")
	}
	if inv.addr == "" {
		return inv, errors.New("no store address: give --store or set RL_STORE_ADDR")
	}
	inv.user = "rl_admin"
	if u, ok := lookupEnv("RL_ADMIN_USER"); ok && u != "" {
		inv.user = u
	}
	if secret == "" {
		return inv, errors.New("RL_ADMIN_PASSWORD is not set")
	}
	return inv, nil
}

// isEpochID reports whether s is an epoch ID: 32 lowercase hex characters (§A5.9).
func isEpochID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// parseDecimal reads a balance flag (DESIGN §A6: U sent as it is, T as T × rate_period_ns): a
// decimal integer written as strconv.FormatInt writes it, an optional '-' and digits with no
// leading zero. So no base prefix or '_' is taken, and "010" is refused rather than read as
// octal 8 (or as 10). The error never repeats the value.
func parseDecimal(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != s {
		return 0, errors.New("must be a decimal integer in int64 range, with no base prefix, '_', '+' or leading zero")
	}
	return n, nil
}
