package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeStub stands in for each tool variable of the Makefile. Saved under the variable's
// name (GO, TRACE, ...), it appends "<NAME> <args>" to the file $STUB_LOG, answers
// "targets" with two real make targets, and exits 42 when $STUB_FAIL, a call prefix of
// whole words, matches the call: on the $STUB_NTH-th matching call, or on every one when
// STUB_NTH is 0. The rule comes from the environment, which make passes to every recipe
// and sub-make, so the stubs are written once: macOS checks each new executable on its
// first run, which costs more than the run itself.
const makeStub = `#!/bin/bash
set -eu
call="${0##*/}${*:+ $*}"
printf '%s\n' "$call" >>"$STUB_LOG"
case $call in "TRACE targets "*) echo "test-unit build" ;; esac
case $call in "$STUB_FAIL" | "$STUB_FAIL "*) ;; *) exit 0 ;; esac
seen=0
while IFS= read -r line; do
	case $line in "$STUB_FAIL" | "$STUB_FAIL "*) seen=$((seen + 1)) ;; esac
done <"$STUB_LOG"
if [ "$STUB_NTH" = 0 ] || [ "$seen" = "$STUB_NTH" ]; then exit 42; fi
`

// makeTools are the Makefile's tool variables, each replaced by a stub.
func makeTools() []string { return []string{"GO", "GOLANGCI", "STYLUA", "PYTHON", "TRACE"} }

// makeMain is the Go file of a scratch tree's command packages.
const makeMain = "package main\n\nfunc main() {}\n"

// makeTree is a scratch tree holding only a copy of the Makefile, an empty refmodel/ (so
// check and fmt run the Python tools), a cmd/rladmin/main.go (so build runs go) and a
// stale file in .out/.
func makeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	copyFile(t, root, "Makefile")
	if err := os.MkdirAll(filepath.Join(root, "refmodel"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "cmd/rladmin/main.go", makeMain)
	writeTestFile(t, root, ".out/stale.json", "{}\n")
	return root
}

// makeStubs writes the stub of every tool variable into a fresh directory.
func makeStubs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range makeTools() {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(makeStub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// makeRun is the outcome of one make run.
type makeRun struct {
	code  int      // make's exit code
	out   string   // make's stdout and stderr
	calls []string // the stub calls in order, "<NAME> <args>"
}

// runMake runs make with args in root, every tool variable naming its stub in stubs.
// fail and n are the stubs' rule: the prefix of the calls that exit 42 ("": none) and
// which matching call fails (0: every one).
func runMake(t *testing.T, stubs, root, fail string, n int, args ...string) makeRun {
	t.Helper()
	vars := make([]string, 0, len(makeTools()))
	for _, tool := range makeTools() {
		vars = append(vars, tool+"="+filepath.Join(stubs, tool))
	}
	log := filepath.Join(t.TempDir(), "calls.log")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", append(vars, args...)...)
	cmd.Dir = root
	// An outer make (slice-check running make test-unit) exports its flags, its level and
	// its command-line variables, S among them; none of them may reach this make.
	cmd.Env = append(os.Environ(), "MAKEFLAGS=", "MFLAGS=", "MAKELEVEL=", "MAKEFILES=", "S=",
		"STUB_LOG="+log, "STUB_FAIL="+fail, "STUB_NTH="+strconv.Itoa(n))
	out, err := cmd.CombinedOutput()
	r := makeRun{out: string(out)}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("make %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	data, err := os.ReadFile(log)
	switch {
	case err == nil:
		r.calls = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	case !errors.Is(err, os.ErrNotExist):
		t.Fatal(err)
	}
	return r
}

// callMatches reports whether call starts with the whole words of prefix, as the stub
// matches: "GO" matches "GO build ..." but not "GOLANGCI run ...".
func callMatches(call, prefix string) bool {
	return call == prefix || strings.HasPrefix(call, prefix+" ")
}

// upToCall is calls up to and including the n-th call matching prefix (0: the first):
// what a run makes when that call fails and the run stops there.
func upToCall(t *testing.T, calls []string, prefix string, n int) []string {
	t.Helper()
	left := max(n, 1)
	for i, c := range calls {
		if callMatches(c, prefix) {
			if left--; left == 0 {
				return calls[:i+1]
			}
		}
	}
	t.Fatalf("no call #%d matching %q in\n  %s", max(n, 1), prefix, strings.Join(calls, "\n  "))
	return nil
}

// TestMakefile_FailurePropagates runs the Makefile with a stub for every tool variable
// (DESIGN §C9). With every stub exiting 0, every target passes; each stub in turn exiting
// 42 fails every target that ran it, stopping right at that call, and so does each single
// call of a target's all-pass log failing alone (any recipe line); slice-check runs procs,
// check, each target followed by procs, then slices, in that order, and stops at the
// first failing step (slices failing fails it too); build builds only the packages that
// hold Go files. So it catches both traps of IMPLEMENTATION_PLAN S0's Watch out: a
// SHELL without -eu -o pipefail (macOS's make 3.81 ignores .SHELLFLAGS) and a target list
// read in the for word list. make runs in a temp tree holding only a copy of the Makefile,
// never in the live tree: slice-check deletes .out/, which the outer make test-unit is
// writing.
func TestMakefile_FailurePropagates(t *testing.T) {
	const buildTrace = "GO build -o bin/trace ./tools/trace" // the bin/trace rule, whatever TRACE names
	stubs := makeStubs(t)
	runs := [][]string{{"check"}, {"test-unit"}, {"fmt"}, {"build"}, {"slice-check", "S=S0"}}
	name := func(args []string) string { return "make " + strings.Join(args, " ") }
	list := func(calls []string) string { return "\n  " + strings.Join(calls, "\n  ") }
	ranTool := func(calls []string, tool string) bool {
		return slices.ContainsFunc(calls, func(c string) bool { return callMatches(c, tool) })
	}
	staleKept := func(t *testing.T, root string) bool {
		t.Helper()
		_, err := os.Stat(filepath.Join(root, ".out", "stale.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}

	// Every stub exits 0: every target passes. pass keeps the calls each one made.
	pass := map[string][]string{}
	for _, args := range runs {
		root := makeTree(t)
		r := runMake(t, stubs, root, "", 0, args...)
		if r.code != 0 || len(r.calls) == 0 {
			t.Fatalf("%s, every stub passing: exit %d, want 0, after calls:%s\n%s", name(args), r.code, list(r.calls), r.out)
		}
		pass[args[0]] = r.calls
		if args[0] == "slice-check" {
			if fi, err := os.Stat(filepath.Join(root, ".out")); err != nil || !fi.IsDir() || staleKept(t, root) {
				t.Errorf("%s did not replace .out/ with an empty directory (stat: %v)", name(args), err)
			}
		}
	}
	for _, tool := range makeTools() {
		if !ranTool(pass["check"], tool) {
			t.Errorf("make check never ran the %s stub, so its failure goes untested:%s", tool, list(pass["check"]))
		}
	}
	want := slices.Concat(
		[]string{buildTrace, "TRACE procs"},
		pass["check"],
		[]string{"TRACE targets --upto S0"},
		pass["test-unit"], []string{"TRACE procs"},
		pass["build"], []string{"TRACE procs"},
		[]string{"TRACE slices --upto S0"},
	)
	if got := pass["slice-check"]; !slices.Equal(got, want) {
		t.Fatalf("make slice-check S=S0 made the calls%s\nwant procs, check, each target followed by procs, then slices:%s", list(got), list(want))
	}

	// Each stub in turn exits 42: every target that ran it fails, and stops at that call.
	// Every run below has its own tree and call log, so they run in parallel.
	for _, tool := range makeTools() {
		for _, args := range runs {
			ran := pass[args[0]]
			if !ranTool(ran, tool) {
				continue
			}
			t.Run(tool+" fails/"+args[0], func(t *testing.T) {
				t.Parallel()
				r := runMake(t, stubs, makeTree(t), tool, 0, args...)
				if want := upToCall(t, ran, tool, 0); r.code == 0 || !slices.Equal(r.calls, want) {
					t.Errorf("%s with %s exiting 42: exit %d after calls:%s\nwant a failure right after:%s\n%s",
						name(args), tool, r.code, list(r.calls), list(want), r.out)
				}
			})
		}
	}

	// Each call of every target's all-pass log exits 42 in turn, alone (the n-th call that
	// matches its whole text, by STUB_NTH): the target fails right at that call, so a
	// failure on any recipe line fails the target, not only each tool's first call (build's
	// second line, go build -tags rltest, among them).
	for _, args := range runs {
		ran := pass[args[0]]
		for i, call := range ran {
			n := 0
			for _, c := range ran[:i+1] {
				if callMatches(c, call) {
					n++
				}
			}
			t.Run(args[0]+" fails at call "+strconv.Itoa(i+1), func(t *testing.T) {
				t.Parallel()
				r := runMake(t, stubs, makeTree(t), call, n, args...)
				if want := ran[:i+1]; r.code == 0 || !slices.Equal(r.calls, want) {
					t.Errorf("%s with call %d (%s) exiting 42: exit %d after calls:%s\nwant a failure right after:%s\n%s",
						name(args), i+1, call, r.code, list(r.calls), list(want), r.out)
				}
			})
		}
	}

	// slice-check stops at its first failing step, and fails when its last step, slices,
	// fails; .out/ goes only after the first procs.
	for _, c := range []struct {
		step, prefix string
		n            int
	}{
		{"first procs", "TRACE procs", 1},
		{"targets", "TRACE targets", 0},
		{"first target", "TRACE gotest", 0},
		{"procs after the first target", "TRACE procs", 2},
		{"slices", "TRACE slices", 0},
	} {
		t.Run("slice-check stops at "+c.step, func(t *testing.T) {
			t.Parallel()
			root := makeTree(t)
			r := runMake(t, stubs, root, c.prefix, c.n, "slice-check", "S=S0")
			if want := upToCall(t, pass["slice-check"], c.prefix, c.n); r.code == 0 || !slices.Equal(r.calls, want) {
				t.Errorf("a failing %s: exit %d after calls:%s\nwant a failure right after:%s\n%s", c.step, r.code, list(r.calls), list(want), r.out)
			}
			if kept, wantKept := staleKept(t, root), c.step == "first procs"; kept != wantKept {
				t.Errorf("a failing %s: .out/stale.json kept = %v, want %v", c.step, kept, wantKept)
			}
		})
	}
	t.Run("slice-check needs S", func(t *testing.T) {
		t.Parallel()
		r := runMake(t, stubs, makeTree(t), "", 0, "slice-check")
		if r.code == 0 || ranTool(r.calls, "TRACE") || !strings.Contains(r.out, "S is required") {
			t.Errorf("make slice-check without S: exit %d after calls:%s\nwant a failure naming S before any trace step\n%s", r.code, list(r.calls), r.out)
		}
	})

	// build judges by Go files which packages exist: with no Go file in any cmd/<name>/
	// it prints "no binaries yet", runs nothing and exits 0; bin/bench gets hitapi and
	// rladmin only when they hold Go files, and never a harness tool, while bin/test gets
	// every binary.
	const (
		benchRladmin = "GO build -o bin/bench/ ./cmd/rladmin"
		benchBoth    = "GO build -o bin/bench/ ./cmd/hitapi ./cmd/rladmin"
		buildAll     = "GO build -tags rltest -o bin/test/ ./cmd/..."
	)
	for _, c := range []struct {
		name  string
		paths []string // made in place of the tree's cmd/; a path ending in / is a directory
		calls []string // build's calls; none means it printed "no binaries yet"
	}{
		{"no cmd/", nil, nil},
		{"an empty cmd/", []string{"cmd/"}, nil},
		{"cmd/ dirs with no Go files", []string{"cmd/rladmin/", "cmd/hitapi/.DS_Store", "cmd/hitgen/README"}, nil},
		{"hitapi with no Go files", []string{"cmd/hitapi/.DS_Store", "cmd/rladmin/main.go"}, []string{benchRladmin, buildAll}},
		{"only a harness tool", []string{"cmd/hitgen/main.go"}, []string{buildAll}},
		{"hitapi, rladmin and a harness tool", []string{"cmd/hitapi/main.go", "cmd/rladmin/main.go", "cmd/hitgen/main.go"}, []string{benchBoth, buildAll}},
	} {
		t.Run("build with "+c.name, func(t *testing.T) {
			t.Parallel()
			root := makeTree(t)
			if err := os.RemoveAll(filepath.Join(root, "cmd")); err != nil {
				t.Fatal(err)
			}
			for _, p := range c.paths {
				switch dir, isDir := strings.CutSuffix(p, "/"); {
				case isDir:
					if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
						t.Fatal(err)
					}
				case strings.HasSuffix(p, ".go"):
					writeTestFile(t, root, p, makeMain)
				default:
					writeTestFile(t, root, p, "not Go\n")
				}
			}
			r := runMake(t, stubs, root, "", 0, "build")
			if none := strings.Contains(r.out, "no binaries yet"); r.code != 0 || !slices.Equal(r.calls, c.calls) || none != (len(c.calls) == 0) {
				t.Errorf("make build with %s: exit %d after calls:%s\nwant exit 0 after calls:%s\n(none: \"no binaries yet\")\n%s",
					c.name, r.code, list(r.calls), list(c.calls), r.out)
			}
		})
	}
}
