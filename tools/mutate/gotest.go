package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

var (
	// testNameRe is a Go test name as curated entries give it; a trailing _* selects every
	// test with that prefix.
	testNameRe = regexp.MustCompile(`^(?:Test|Fuzz|Example)[A-Za-z0-9_]*(?:_\*)?$`)
	// pyTestRe is a Python test: a file, or a test in it.
	pyTestRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.py(?:::[A-Za-z0-9_]+)?$`)
)

// buildGrace bounds what go test may spend before its test binary starts (the build); its
// -timeout bounds the binary.
const buildGrace = 5 * time.Minute

func isPyTest(name string) bool { return strings.Contains(name, ".py") }

// matchesName reports whether the test (a top-level name) is the one name selects.
func matchesName(test, name string) bool {
	if prefix, ok := strings.CutSuffix(name, "*"); ok {
		return strings.HasPrefix(test, prefix)
	}
	return test == name
}

// testFunc is one Test/Fuzz/Example function of the module.
type testFunc struct {
	name   string
	dir    string // its package's directory, root-relative with slashes ("." for the root)
	file   string // its file, root-relative
	tags   string // the build tag its file needs: "", "rltest" or "scripttest"
	tagErr string // why no build mutants use includes its file ("" when one does)
}

// testIndex lists the module's test functions; it is read once, on first use.
type testIndex struct {
	root  string
	funcs []testFunc
	read  bool
}

// tagSets are the builds mutants may use, in order of preference: never conformance (L0) and
// never a harness package (L3), DESIGN §C9.
func tagSets() [][]string { return [][]string{nil, {"rltest"}, {"scripttest"}} }

func (x *testIndex) load(ctx context.Context) error {
	if x.read {
		return nil
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(x.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(x.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", rel, err)
		}
		tags, tagErr := fileTags(filepath.Dir(p), d.Name())
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !testNameRe.MatchString(fd.Name.Name) {
				continue
			}
			x.funcs = append(x.funcs, testFunc{name: fd.Name.Name, dir: path.Dir(rel), file: rel, tags: tags, tagErr: tagErr})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("listing the test functions: %w", err)
	}
	x.read = true
	return nil
}

// skipDir names the directories the index never reads: testdata, vendor, every one whose name
// starts with "." or "_" (go ./... leaves those out), and bin/ and runs/ at the root.
func skipDir(rel string) bool {
	name := path.Base(rel)
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
		rel == "bin" || rel == "runs"
}

// fileTags returns the first tag set of tagSets whose build includes the file.
func fileTags(dir, name string) (string, string) {
	for _, set := range tagSets() {
		bc := build.Default
		bc.BuildTags = set
		if ok, err := bc.MatchFile(dir, name); err == nil && ok {
			return strings.Join(set, ","), ""
		}
	}
	return "", "its file builds only with tags mutants never use (conformance tests are L0; DESIGN §C9)"
}

// goRun is one go test command of a mutant or a baseline.
type goRun struct {
	pkgs    []string // package patterns
	tags    string
	run     string   // -run pattern ("" = every test)
	names   []string // the named tests (as a curated entry gives them) the run selects; its baseline answers for these
	overlay string   // the -overlay file ("" = none)
	timeout time.Duration
}

func (r goRun) String() string {
	var parts []string
	if r.tags != "" {
		parts = append(parts, "-tags "+r.tags)
	}
	if r.run != "" {
		parts = append(parts, "-run '"+r.run+"'")
	}
	return strings.Join(append(parts, r.pkgs...), " ")
}

// runsFor resolves the named Go tests to go test commands: one per package and build tag, each
// selecting its tests by exact name and listing the names it answers for (a Name_* prefix in
// every run holding one of its tests). A name that matches no test function is an error, and so
// is a test of a harness package (L3) or one whose file only a conformance build includes.
func (x *testIndex) runsFor(ctx context.Context, names []string) ([]goRun, error) {
	if err := x.load(ctx); err != nil {
		return nil, err
	}
	type group struct{ dir, tags string }
	var order []group
	byGroup := map[group][]string{}
	namesOf := map[group][]string{}
	for _, n := range names {
		if !testNameRe.MatchString(n) {
			return nil, fmt.Errorf("%q is no Go test name (Test…, Fuzz…, Example…, or Name_*)", n)
		}
		var matches []testFunc
		dirs := map[string]bool{}
		for _, f := range x.funcs {
			if matchesName(f.name, n) {
				matches = append(matches, f)
				dirs[f.dir] = true
			}
		}
		switch {
		case len(matches) == 0:
			return nil, fmt.Errorf("%s matches no test function in the module", n)
		case !strings.HasSuffix(n, "*") && len(dirs) > 1:
			return nil, fmt.Errorf("%s is defined in more than one package (%s)", n, strings.Join(slices.Sorted(maps.Keys(dirs)), ", "))
		}
		for _, f := range matches {
			if f.dir == "harness" || strings.HasPrefix(f.dir, "harness/") {
				return nil, fmt.Errorf("%s is in %s: an L3 test, which mutants never run (DESIGN §C9)", f.name, f.dir)
			}
			if f.tagErr != "" {
				return nil, fmt.Errorf("%s (%s): %s", f.name, f.file, f.tagErr)
			}
			g := group{f.dir, f.tags}
			if _, ok := byGroup[g]; !ok {
				order = append(order, g)
			}
			if !slices.Contains(byGroup[g], f.name) {
				byGroup[g] = append(byGroup[g], f.name)
			}
			if !slices.Contains(namesOf[g], n) {
				namesOf[g] = append(namesOf[g], n)
			}
		}
	}
	runs := make([]goRun, 0, len(order))
	for _, g := range order {
		quoted := make([]string, 0, len(byGroup[g]))
		for _, n := range byGroup[g] {
			quoted = append(quoted, regexp.QuoteMeta(n))
		}
		runs = append(runs, goRun{pkgs: []string{pkgPattern(g.dir)}, tags: g.tags, run: "^(?:" + strings.Join(quoted, "|") + ")$", names: namesOf[g]})
	}
	return runs, nil
}

// hasTagged reports whether the package in dir has test functions that need the tag.
func (x *testIndex) hasTagged(ctx context.Context, dir, tag string) (bool, error) {
	if err := x.load(ctx); err != nil {
		return false, err
	}
	return slices.ContainsFunc(x.funcs, func(f testFunc) bool { return f.dir == dir && f.tags == tag }), nil
}

func pkgPattern(dir string) string {
	if dir == "." {
		return "."
	}
	return "./" + dir
}

// goResult is what one go test -json run showed.
type goResult struct {
	exit      int
	timedOut  bool
	panicked  bool
	buildFail bool
	ran       bool            // some test started
	pass      map[string]bool // top-level tests that passed
	fail      map[string]bool // top-level tests that failed (a failing subtest fails its parent)
	elapsed   time.Duration
	log       string // the saved output
}

// testEvent is one line of go test -json (test2json, with the build events of Go 1.24+).
type testEvent struct {
	Action      string
	Test        string
	Output      string
	FailedBuild string
}

// parseGoTest reads a go test -json stream; a line that is no JSON event (the go command's own
// errors) is read as output text.
func parseGoTest(data []byte) goResult {
	r := goResult{pass: map[string]bool{}, fail: map[string]bool{}}
	for _, line := range bytes.Split(data, []byte("\n")) {
		text := string(line)
		var ev testEvent
		if len(line) > 0 && line[0] == '{' && json.Unmarshal(line, &ev) == nil && ev.Action != "" {
			text = ev.Output
			switch ev.Action {
			case "build-fail":
				r.buildFail = true
			case "run":
				r.ran = r.ran || ev.Test != ""
			case "pass":
				if ev.Test != "" && !strings.Contains(ev.Test, "/") {
					r.pass[ev.Test] = true
				}
			case "fail":
				if ev.FailedBuild != "" {
					r.buildFail = true
				}
				if ev.Test != "" {
					top, _, _ := strings.Cut(ev.Test, "/")
					r.fail[top] = true
				}
			}
		}
		switch {
		case strings.HasPrefix(text, "panic: "):
			r.panicked = true
			r.timedOut = r.timedOut || strings.Contains(text, "test timed out")
		case strings.HasPrefix(text, "fatal error: "):
			r.panicked = true
		}
		if strings.Contains(text, "[build failed]") || strings.Contains(text, "[setup failed]") {
			r.buildFail = true
		}
	}
	return r
}

// judgeGo classifies a mutant's run (DESIGN §C9): killed only when a named test (any test when
// names is nil) failed with an assertion, the run showing no panic, build error or timeout.
func judgeGo(r goResult, names []string) result {
	switch {
	case r.buildFail:
		return result{verdict: invalid, why: "does not build"}
	case r.timedOut:
		return result{verdict: notKilled, why: "timeout"}
	case r.panicked:
		return result{verdict: notKilled, why: "panic"}
	case r.exit == 0:
		return result{verdict: survived}
	}
	var by []string
	for t := range r.fail {
		if names == nil || slices.ContainsFunc(names, func(n string) bool { return matchesName(t, n) }) {
			by = append(by, t)
		}
	}
	sort.Strings(by)
	if len(by) == 0 {
		return result{verdict: notKilled, why: fmt.Sprintf("go test exited with %d, but no named test failed", r.exit)}
	}
	return result{verdict: killed, by: by}
}

// baselineProblem says why an unmutated run cannot judge mutants: every named test must report
// a pass (a name that matches nothing is an error, never a pass) and none may fail.
func baselineProblem(r goResult, names []string) string {
	switch {
	case r.buildFail:
		return "does not build"
	case r.timedOut:
		return "timed out"
	case r.panicked:
		return "panicked"
	}
	for _, n := range names {
		failed := slices.ContainsFunc(slices.Collect(maps.Keys(r.fail)), func(t string) bool { return matchesName(t, n) })
		passed := slices.ContainsFunc(slices.Collect(maps.Keys(r.pass)), func(t string) bool { return matchesName(t, n) })
		switch {
		case failed:
			return n + " fails on the unmutated code"
		case !passed:
			return n + " reports no pass"
		}
	}
	if r.exit != 0 {
		return fmt.Sprintf("go test exited with %d", r.exit)
	}
	return ""
}

// repliesEnv names the file the L2 tests' scriptrun.Run appends each decoded reply to; when it
// is unset, that is the module's .out/replies-l2.jsonl. It copies scriptrun.RepliesEnv, since
// this code imports only the standard library; TestMutate_RepliesEnv keeps the two equal.
const repliesEnv = "RL_REPLIES_FILE"

// goEnv is the environment of a go test child: cfg.environ, then PWD naming the root as the
// overlay does (os/exec sets it only when Env is nil: the go command names files by os.Getwd,
// which returns $PWD whenever it names the working directory, so a caller's PWD through a
// symlink would name them by the link and miss every overlay key), then RL_REPLIES_FILE naming
// replies, a file in the run's temp directory, so the replies its L2 tests give never reach the
// module's .out/replies-l2.jsonl, which bin/trace replies reads (plan S4 Watch out). os/exec
// keeps the last value of a key, so both override the caller's.
func goEnv(cfg config, replies string) []string {
	return append(slices.Clone(cfg.environ), "PWD="+cfg.root, repliesEnv+"="+replies)
}

// goTest runs one go test -json command in the module root, its output saved to logPath, with a
// temp directory of its own (outside the tree, removed afterwards) for the replies its tests
// record. Only a done ctx (an interrupt) or a failure to run go is an error; a timeout is a
// result.
func goTest(ctx context.Context, cfg config, r goRun, logPath string) (res goResult, err error) {
	if err := ctx.Err(); err != nil {
		return goResult{}, err
	}
	args := []string{"test", "-json", "-count=1", "-failfast", "-timeout", r.timeout.String()}
	if r.tags != "" {
		args = append(args, "-tags", r.tags)
	}
	if r.overlay != "" {
		args = append(args, "-overlay", r.overlay)
	}
	if r.run != "" {
		args = append(args, "-run", r.run)
	}
	args = append(args, r.pkgs...)
	tmp, err := os.MkdirTemp(cfg.tmpDir, "mutate-go-")
	if err != nil {
		return goResult{}, fmt.Errorf("creating the run's temp directory: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(tmp); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the run's temp directory: %w", rerr))
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, r.timeout+buildGrace)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cfg.goBin, args...)
	cmd.Dir, cmd.Env = cfg.root, goEnv(cfg, filepath.Join(tmp, "replies-l2.jsonl"))
	start := time.Now()
	exit, err := runLogged(cmd, logPath)
	elapsed := time.Since(start)
	if err != nil {
		return goResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return goResult{}, fmt.Errorf("interrupted: %w", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return goResult{}, err
	}
	res = parseGoTest(data)
	res.exit, res.elapsed, res.log = exit, elapsed, logPath
	if runCtx.Err() != nil {
		res.timedOut = true
		res.exit = max(res.exit, 1)
	}
	return res, nil
}

// runLogged runs cmd in its own process group with stdout and stderr in logPath (a file, so a
// leftover grandchild holding them open cannot stall Wait). When cmd's context ends, the group
// gets SIGINT, then SIGKILL after a grace period; after cmd exits, whatever is left of the group
// (a test binary's store, say) is killed. It returns cmd's exit code (1 for a signal).
func runLogged(cmd *exec.Cmd, logPath string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, fmt.Errorf("creating %s: %w", filepath.Dir(logPath), err)
	}
	f, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", logPath, err)
	}
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
	cmd.WaitDelay = 15 * time.Second
	runErr := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // nothing of the group may outlive the run
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("saving %s: %w", logPath, err)
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return 0, nil
	case errors.As(runErr, &exitErr):
		if code := exitErr.ExitCode(); code > 0 {
			return code, nil
		}
		return 1, nil // ended by a signal
	case cmd.ProcessState != nil:
		return max(cmd.ProcessState.ExitCode(), 1), nil // ended after its context (WaitDelay)
	}
	return 0, fmt.Errorf("running %s: %w", filepath.Base(cmd.Path), runErr)
}
