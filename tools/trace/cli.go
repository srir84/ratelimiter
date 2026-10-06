package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

const usageText = `usage: trace <command> [flags]

commands:
  check                       boundary, ids, acl, lua, suppress, tags, testglobals and oracle
  boundary                    product packages never reach the harness (DESIGN §A1)
  ids [--list] [--complete]   SPEC §10 and HARNESS §8 test IDs and their test functions (§C7)
  acl                         ACL template and the scripts' inner commands (§A2)
  lua                         script headers, banned calls and luacheck (§A4.8, §C3)
  suppress                    no inline lint suppressions (AGENTS.md §0)
  tags                        rltest build lines (AGENTS.md §2)
  testglobals                 no writes to package-level variables in test code (§C7)
  oracle                      expected values never come from internal/core (§C7)
  procs                       kill and report recorded child processes still alive (§C7)
  replies [--upto <id>]       every reply literal of internal/scripts/*.lua is in the reply table and
                              every reason of the table in DESIGN; with --upto from S4 on, every row
                              is observed in .out/replies-l2.jsonl (§C3 reply-code closure)
  targets --upto <id>         make targets of the slices up to <id>
  slices --upto <id> [--plan IMPLEMENTATION_PLAN.md] [--results .out]
                              every ID and plan-named test up to <id> exists and passed
  gotest -o <file> -- <args>  go test -json -shuffle=on <args>, stream saved to <file>

Paths given to --plan and --results are relative to the repository root, the nearest
directory with a go.mod.
`

// env is what every command runs against; main fills it and tests build their own.
type env struct {
	root    string // repository root: the nearest directory with a go.mod ("" when none)
	rootErr error  // why root is empty
	dir     string // working directory: gotest runs go test here
	stdout  io.Writer
	stderr  io.Writer
}

// report is a check's outcome: lines for stdout and the rule violations it found.
type report struct {
	out      []string // printed to stdout as they are (lists and summaries)
	problems []string // printed to stderr as "<command>: <problem>"
}

// errUsage marks a command-line mistake (exit 2) rather than a failed check (exit 1).
var errUsage = errors.New("usage")

type checkFunc func(ctx context.Context, e *env, args []string) (report, error)

// checkNames lists what `trace check` runs, in order. It never checks completeness (§C7).
func checkNames() []string {
	return []string{"boundary", "ids", "acl", "lua", "suppress", "tags", "testglobals", "oracle"}
}

func checkByName(name string) (checkFunc, bool) {
	switch name {
	case "boundary":
		return checkBoundary, true
	case "ids":
		return checkIDs, true
	case "acl":
		return checkACL, true
	case "lua":
		return checkLua, true
	case "suppress":
		return checkSuppress, true
	case "tags":
		return checkTags, true
	case "testglobals":
		return checkTestGlobals, true
	case "oracle":
		return checkOracle, true
	case "procs":
		return checkProcs, true
	case "replies":
		return checkReplies, true
	case "slices":
		return checkSlices, true
	}
	return nil, false
}

// run finds the repository root from wd and runs one command; it returns the exit code.
func run(ctx context.Context, wd string, args []string, stdout, stderr io.Writer) int {
	e := &env{dir: wd, stdout: stdout, stderr: stderr}
	e.root, e.rootErr = findRoot(ctx, wd)
	return dispatch(ctx, e, args)
}

func dispatch(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usageText)
		return 2
	}
	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(e.stdout, usageText)
		return 0
	case "gotest":
		return runGotest(ctx, e, rest)
	case "targets":
		return runTargets(e, rest)
	case "check":
		if len(rest) > 0 {
			fmt.Fprintf(e.stderr, "check: takes no arguments\n")
			return 2
		}
		code := 0
		for _, n := range checkNames() {
			if err := ctx.Err(); err != nil {
				fmt.Fprintf(e.stderr, "check: stopped before %s: %v\n", n, err)
				return 1
			}
			fn, _ := checkByName(n)
			if c := runOne(ctx, e, n, fn, nil); c > code {
				code = c
			}
		}
		return code
	}
	fn, ok := checkByName(name)
	if !ok {
		fmt.Fprintf(e.stderr, "trace: unknown command %q\n\n%s", name, usageText)
		return 2
	}
	return runOne(ctx, e, name, fn, rest)
}

// runOne runs one check and prints its report: 0 = pass, 1 = problems or an error, 2 = usage.
func runOne(ctx context.Context, e *env, name string, fn checkFunc, args []string) int {
	if e.root == "" {
		fmt.Fprintf(e.stderr, "%s: error: repository root: %v\n", name, e.rootErr)
		return 1
	}
	r, err := fn(ctx, e, args)
	if errors.Is(err, errUsage) {
		return 2
	}
	for _, line := range r.out {
		fmt.Fprintln(e.stdout, line)
	}
	for _, p := range r.problems {
		fmt.Fprintf(e.stderr, "%s: %s\n", name, p)
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "%s: error: %v\n", name, err)
		return 1
	}
	if len(r.problems) > 0 {
		fmt.Fprintf(e.stderr, "%s: FAIL, %d problem(s)\n", name, len(r.problems))
		return 1
	}
	if len(r.out) == 0 {
		fmt.Fprintf(e.stdout, "%s: ok\n", name)
	}
	return 0
}

// parseFlags parses a command's flags; a mistake prints the flag help and returns errUsage.
func parseFlags(fs *flag.FlagSet, e *env, args []string) error {
	fs.SetOutput(e.stderr)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "%s: unexpected arguments %q\n", fs.Name(), fs.Args())
		return errUsage
	}
	return nil
}
