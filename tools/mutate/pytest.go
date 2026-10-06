package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// pyPlugin is the pytest plugin bin/mutate loads with -p (plan S4 step 16): JUnit messages do
// not carry the exception type (a rewritten assert reports only `assert 1 == 2`), so it records
// each test's phase outcomes, "passed" or call.excinfo.type.__name__, into a JSON file. %s is
// the file's path as a Python string literal.
const pyPlugin = `"""bin/mutate's pytest plugin: each test's phase outcomes, by exception type name."""

import json

_OUT = %s
_RESULTS = {}


def pytest_runtest_makereport(item, call):
    phases = _RESULTS.setdefault(item.nodeid, {})
    phases[call.when] = "passed" if call.excinfo is None else call.excinfo.type.__name__


def pytest_collectreport(report):
    if report.failed:
        _RESULTS.setdefault("collect " + report.nodeid, {})["collect"] = "failed"


def pytest_sessionfinish(session, exitstatus):
    with open(_OUT, "w", encoding="utf-8") as f:
        json.dump({"exitstatus": int(exitstatus), "tests": _RESULTS}, f)
`

// pyPluginModule is the plugin's module name.
const pyPluginModule = "mutate_report"

// pyResult is what one pytest run showed.
type pyResult struct {
	exitStatus int
	timedOut   bool
	invalid    string                       // why the patched file does not compile ("" when it does)
	tests      map[string]map[string]string // node id → phase → "passed" or an exception type name
	elapsed    time.Duration
	log        string
}

// pyMatches reports whether a pytest node id is one that name selects: a file selects its tests,
// path.py::name the test (parametrized cases included).
func pyMatches(node, name string) bool {
	if strings.Contains(name, "::") {
		return node == name || strings.HasPrefix(node, name+"[") || strings.HasPrefix(node, name+"::")
	}
	return strings.HasPrefix(node, name+"::")
}

// runPyMutant runs a mutant of a Python file against named tests: they first pass in an
// unpatched temp copy of the package, then run in a patched one (plan S4 step 16).
func runPyMutant(ctx context.Context, cfg config, m mutant, names []string) (result, error) {
	pkg, _, _ := strings.Cut(m.file, "/")
	for _, n := range names {
		file, _, _ := strings.Cut(n, "::")
		if !pyTestRe.MatchString(n) {
			return result{}, fmt.Errorf("%q is no Python test (path.py or path.py::name)", n)
		}
		if top, _, _ := strings.Cut(file, "/"); top != pkg || !strings.Contains(file, "/") {
			return result{}, fmt.Errorf("%s is outside %s/, the package the mutant patches", n, pkg)
		}
		ok, err := fileExists(ctx, filepath.Join(cfg.root, filepath.FromSlash(file)))
		if err != nil {
			return result{}, err
		}
		if !ok {
			return result{}, fmt.Errorf("%s: no such file", file)
		}
	}
	base, err := pytest(ctx, cfg, pkg, nil, names, cfg.maxTimeout, logPath(cfg, m.id, "baseline", 0))
	if err != nil {
		return result{}, err
	}
	if p := pyBaselineProblem(base, names); p != "" {
		return result{}, fmt.Errorf("baseline pytest %s: %s (see %s)", strings.Join(names, " "), p, base.log)
	}
	res, err := pytest(ctx, cfg, pkg, &m, names, mutantTimeout(cfg, base.elapsed), logPath(cfg, m.id, "mutant", 0))
	if err != nil {
		return result{}, err
	}
	if res.invalid != "" {
		return result{verdict: invalid, why: "does not compile: " + res.invalid, log: res.log}, nil
	}
	j := judgePy(res, names)
	j.log = res.log
	return j, nil
}

// judgePy classifies a Python mutant's run: killed only when a named test's call phase raised
// AssertionError (rewritten asserts included) or pytest's Failed (pytest.fail); any other
// exception, an error in setup or collection, or a timeout is no kill.
func judgePy(r pyResult, names []string) result {
	if r.timedOut {
		return result{verdict: notKilled, why: "timeout"}
	}
	var by, other []string
	for node, phases := range r.tests {
		if c, ok := phases["collect"]; ok {
			other = append(other, node+": "+c)
			continue
		}
		matched := false
		for _, n := range names {
			matched = matched || pyMatches(node, n)
		}
		if !matched {
			continue
		}
		if s := phases["setup"]; s != "passed" {
			other = append(other, node+": setup "+s)
			continue
		}
		switch c := phases["call"]; c {
		case "", "passed":
		case "AssertionError", "Failed":
			by = append(by, node)
		default:
			other = append(other, node+": "+c)
		}
	}
	sort.Strings(by)
	sort.Strings(other)
	switch {
	case len(other) > 0:
		return result{verdict: notKilled, why: strings.Join(other, "; ")}
	case len(by) > 0:
		return result{verdict: killed, by: by}
	case r.exitStatus == 0:
		return result{verdict: survived}
	}
	return result{verdict: notKilled, why: fmt.Sprintf("pytest exited with %d, but no named test failed", r.exitStatus)}
}

// pyBaselineProblem says why an unpatched run cannot judge mutants: every name must select a
// test that passes, and none may fail.
func pyBaselineProblem(r pyResult, names []string) string {
	if r.timedOut {
		return "timed out"
	}
	for node, phases := range r.tests {
		if _, ok := phases["collect"]; ok {
			return "collecting " + strings.TrimPrefix(node, "collect ") + " failed"
		}
	}
	for _, n := range names {
		passed := false
		for node, phases := range r.tests {
			if !pyMatches(node, n) {
				continue
			}
			for _, when := range []string{"setup", "call", "teardown"} {
				if s, ok := phases[when]; ok && s != "passed" {
					return fmt.Sprintf("%s fails on the unmutated code (%s %s)", node, when, s)
				}
			}
			passed = passed || phases["call"] == "passed"
		}
		if !passed {
			return n + " reports no pass"
		}
	}
	if r.exitStatus != 0 {
		return fmt.Sprintf("pytest exited with %d", r.exitStatus)
	}
	return ""
}

// pytest runs pytest on names in a temp copy of the package pkg (patched with m when it is not
// nil), from the copy's root, with the plugin; a patched file that does not compile is
// reported in invalid without running pytest. The copy is removed afterwards.
func pytest(ctx context.Context, cfg config, pkg string, m *mutant, names []string, timeout time.Duration, logFile string) (res pyResult, err error) {
	tmp, err := os.MkdirTemp(cfg.tmpDir, "mutate-py-")
	if err != nil {
		return pyResult{}, fmt.Errorf("creating the Python copy: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(tmp); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the Python copy: %w", rerr))
		}
	}()
	// pytest names a test by its file's path relative to --rootdir after resolving symlinks, so
	// under a temp parent behind a symlink (macOS's $TMPDIR: /var is /private/var) every node id
	// would lose its file ("::test_x") and match no name: work under the resolved path.
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return pyResult{}, fmt.Errorf("resolving the Python copy: %w", err)
	}
	tree, pluginDir := filepath.Join(real, "tree"), filepath.Join(real, "plugin")
	if err := copyPyTree(ctx, filepath.Join(cfg.root, pkg), filepath.Join(tree, pkg)); err != nil {
		return pyResult{}, err
	}
	for _, f := range []string{"conftest.py", "pytest.ini"} { // the root's pytest setup, if any
		data, err := os.ReadFile(filepath.Join(cfg.root, f))
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return pyResult{}, err
		}
		if err := os.WriteFile(filepath.Join(tree, f), data, 0o644); err != nil {
			return pyResult{}, err
		}
	}
	env := pyEnv(cfg.environ, pluginDir)
	if m != nil {
		target := filepath.Join(tree, filepath.FromSlash(m.file))
		if err := os.WriteFile(target, m.content, 0o644); err != nil {
			return pyResult{}, fmt.Errorf("patching the copy: %w", err)
		}
		why, err := pyCompile(ctx, cfg, env, target)
		if err != nil || why != "" {
			return pyResult{invalid: why}, err
		}
	}
	results := filepath.Join(pluginDir, "results.json")
	literal, err := json.Marshal(results) // a JSON string is a valid Python string literal
	if err != nil {
		return pyResult{}, err
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return pyResult{}, err
	}
	if err := os.WriteFile(filepath.Join(pluginDir, pyPluginModule+".py"), fmt.Appendf(nil, pyPlugin, literal), 0o644); err != nil {
		return pyResult{}, fmt.Errorf("writing the pytest plugin: %w", err)
	}
	args := []string{"-m", "pytest", "-p", pyPluginModule, "-p", "no:cacheprovider", "-x", "-q", "--rootdir", tree}
	args = append(args, names...)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cfg.python, args...)
	cmd.Dir, cmd.Env = tree, env
	start := time.Now()
	exit, err := runLogged(cmd, logFile)
	if err != nil {
		return pyResult{}, err
	}
	res.elapsed, res.log, res.exitStatus = time.Since(start), logFile, exit
	if err := ctx.Err(); err != nil {
		return pyResult{}, fmt.Errorf("interrupted: %w", err)
	}
	if runCtx.Err() != nil {
		res.timedOut = true
		return res, nil
	}
	data, err := os.ReadFile(results)
	if err != nil {
		return pyResult{}, fmt.Errorf("pytest wrote no results (see %s): %w", logFile, err)
	}
	var out struct {
		ExitStatus int                          `json:"exitstatus"`
		Tests      map[string]map[string]string `json:"tests"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return pyResult{}, fmt.Errorf("reading the pytest results: %w", err)
	}
	res.exitStatus, res.tests = out.ExitStatus, out.Tests
	return res, nil
}

// pyCompile reports why a Python file does not compile ("" when it does).
func pyCompile(ctx context.Context, cfg config, env []string, file string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.python, "-m", "py_compile", file)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "", nil
	case errors.As(err, &exitErr) && ctx.Err() == nil:
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return strings.TrimSpace(lines[len(lines)-1]), nil
	}
	return "", fmt.Errorf("python -m py_compile: %w", err)
}

// pyEnv is environ with the plugin's directory first on PYTHONPATH and no bytecode written.
func pyEnv(environ []string, pluginDir string) []string {
	env := make([]string, 0, len(environ)+2)
	pythonPath := pluginDir
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "PYTHONPATH":
			if v != "" {
				pythonPath += string(os.PathListSeparator) + v
			}
		case "PYTHONDONTWRITEBYTECODE":
		default:
			env = append(env, kv)
		}
	}
	return append(env, "PYTHONPATH="+pythonPath, "PYTHONDONTWRITEBYTECODE=1")
}

// copyPyTree copies a Python package directory, leaving out the tools' caches. A done ctx
// stops the copy.
func copyPyTree(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			switch path.Base(filepath.ToSlash(p)) {
			case "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache":
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
