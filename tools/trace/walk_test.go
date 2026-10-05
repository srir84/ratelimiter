package main

import (
	"context"
	"errors"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestWalk_SkippedDirs: every walk skips bin/, runs/, .data/ and .out/ only directly
// under the root, and testdata/ and dot directories at any depth. An underscore
// directory is skipped for .go files only, as go ./... skips it; its .py and .lua files
// are read, as mypy, ruff, pytest and stylua read them. So suppress reads a nested runs/
// or bin/ and the .py and .lua files of an underscore directory, and the lua and ids
// walks read those too, while the walks of .go files leave the directory out.
func TestWalk_SkippedDirs(t *testing.T) {
	root := fixture(t, "walk/skips")
	// git ignores every bin/, .data/ and .out/ directory, so the test writes these.
	nolint := func(pkg, why string) string {
		return "package " + pkg + "\n\nfunc F() {} //nolint:errcheck // " + why + "\n"
	}
	writeTestFile(t, root, "internal/x/bin/bin.go", nolint("bin", "read: a bin/ below the root"))
	writeTestFile(t, root, "bin/b.go", nolint("b", "skipped: the root's bin/"))
	writeTestFile(t, root, ".data/d.go", nolint("d", "skipped: the root's .data/"))
	writeTestFile(t, root, ".out/o.go", nolint("o", "skipped: the root's .out/"))
	got := walkFiles(t, root, func(string) bool { return true })
	want := []string{"harness/runs/runs.go", "internal/_old/o.lua", "internal/runs/data/x.py", "internal/x/bin/bin.go", "ok.go", "report/_impl/a.py"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("walked\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	goGot, problems, err := goFiles(t.Context(), root, nil)
	if want := "harness/runs/runs.go internal/x/bin/bin.go ok.go"; err != nil || len(problems) > 0 || strings.Join(goGot, " ") != want {
		t.Fatalf("goFiles = %v, %v, %v, want %s", goGot, problems, err, want)
	}
	r, err := checkSuppress(t.Context(), testEnv(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	sameLines(t, "suppress", problemLines(t, r.problems), []string{"harness/runs/runs.go:3", "internal/_old/o.lua:1",
		"internal/runs/data/x.py:1", "internal/x/bin/bin.go:3", "ok.go:3", "report/_impl/a.py:1"}, r.problems)
	for _, form := range []string{`internal/_old/o.lua:1: inline suppression "-- luacheck:"`, `report/_impl/a.py:1: inline suppression "# type: ignore"`} {
		if countContaining(r.problems, form) != 1 {
			t.Errorf("want one problem %q", form)
		}
	}
	// trace lua checks internal/_old/o.lua (luacheck honours its inline option), and
	// trace ids reads the test function in report/_impl/a.py.
	copyFile(t, root, ".luacheckrc")
	copyFile(t, root, "SPEC.md")
	copyFile(t, root, "HARNESS.md")
	if code, stdout, stderr := runIn(t, root, "lua"); code != 0 || stdout != "lua: 1 .lua file(s) checked\n" {
		t.Errorf("trace lua does not check internal/_old/o.lua: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if code, _, stderr := runIn(t, root, "ids"); code != 1 ||
		!strings.Contains(stderr, "ids: report/_impl/a.py:4: test_t_cor_99 maps to T-COR-99, which SPEC.md §10 and HARNESS.md §8 do not define") {
		t.Errorf("trace ids does not read report/_impl/a.py: exit %d\n%s", code, stderr)
	}
}

// TestWalk_Symlinks: a symlink to a regular file is read as that file, so a symlinked
// product file that imports the harness fails boundary and a //nolint in it fails
// suppress; a symlink to a directory is a problem for every check (plan choice) unless
// its name is one the walk skips, so one named like an underscore directory is a problem
// only for the checks that read .py or .lua files; a symlink with no target is passed
// over, and a run from a symlink to the root still walks the real tree. The links are
// made here: symlinks in git fixtures are fragile.
func TestWalk_Symlinks(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "go.mod", "module ratelimiter\n\ngo 1.24\n")
	copyFile(t, root, "SPEC.md") // trace ids reads the defined IDs first
	copyFile(t, root, "HARNESS.md")
	writeTestFile(t, root, "harness/runlog/runlog.go", "package runlog\n")
	writeTestFile(t, root, "internal/core/core.go", "package core\n")
	writeTestFile(t, root, "elsewhere/link.txt",
		"package core\n\nimport _ \"ratelimiter/harness/runlog\"\n\nfunc F() {} //nolint:errcheck\n")
	writeTestFile(t, root, "internal/lt/lt.go", "package lt\n")
	writeTestFile(t, root, "outside/x.go", "package x\n")
	for _, l := range []struct{ target, link string }{
		{"../../elsewhere/link.txt", "internal/core/link.go"}, // a file: read as one
		{"../internal/lt", "harness/alias"},                   // a directory: a problem
		{"outside", ".data"},                                  // named like a skipped directory: passed over
		{"../outside", "internal/testdata"},                   //
		{"../outside", "internal/_alias"},                     // skipped by the walks of .go files only
		{"../../nowhere.go", "internal/core/gone.go"},         // no target: passed over
	} {
		if err := os.Symlink(l.target, filepath.Join(root, filepath.FromSlash(l.link))); err != nil {
			t.Fatal(err)
		}
	}
	files, problems, err := goFiles(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "harness/runlog/runlog.go internal/core/core.go internal/core/link.go internal/lt/lt.go outside/x.go"; strings.Join(files, " ") != want {
		t.Errorf("walked %v, want %s", files, want)
	}
	const dirLink = "harness/alias: a symlink to a directory"
	if len(problems) != 1 || !strings.HasPrefix(problems[0], dirLink) {
		t.Errorf("walk problems: %v, want one %q", problems, dirLink)
	}
	const underLink = "internal/_alias: a symlink to a directory"
	if _, problems, err = repoFiles(t.Context(), root, withSuffix(".py")); err != nil ||
		len(problems) != 2 || !strings.HasPrefix(problems[0], dirLink) || !strings.HasPrefix(problems[1], underLink) {
		t.Errorf("walk problems of the .py walk: %v, %v, want %q and %q", problems, err, dirLink, underLink)
	}
	r, err := checkBoundary(t.Context(), testEnv(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.problems) != 2 || countContaining(r.problems, dirLink) != 1 || countContaining(r.problems,
		"ratelimiter/internal/core -> ratelimiter/harness/runlog: product code never imports the harness (DESIGN §A1); internal/core/link.go imports") != 1 {
		t.Errorf("boundary:\n%s", strings.Join(r.problems, "\n"))
	}
	r, err = checkSuppress(t.Context(), testEnv(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.problems) != 3 || countContaining(r.problems, dirLink) != 1 || countContaining(r.problems, underLink) != 1 ||
		countContaining(r.problems, `internal/core/link.go:5:13: inline suppression "//nolint:errcheck"`) != 1 {
		t.Errorf("suppress:\n%s", strings.Join(r.problems, "\n"))
	}
	// From a symlink to the root (a symlinked checkout, whose path os.Getwd returns),
	// trace still walks the real tree and reports the planted import.
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := run(t.Context(), filepath.Join(link, "internal"), []string{"boundary"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "boundary: ratelimiter/internal/core -> ratelimiter/harness/runlog") {
		t.Errorf("boundary from a symlink to the root: exit %d, want 1 with the planted import\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	// Every check that walks the tree reports the directory symlink. The one named like
	// an underscore directory hides only .py and .lua files from trace, so the checks
	// that read those report it, and the checks of .go files pass it over as go ./... does.
	for _, name := range checkNames() {
		if name == "acl" { // reads deploy/ and internal/scripts/ only, never walks
			continue
		}
		_, _, stderr := runIn(t, root, name)
		if !strings.Contains(stderr, name+": "+dirLink) {
			t.Errorf("trace %s does not report the directory symlink:\n%s", name, stderr)
		}
		readsPyOrLua := name == "ids" || name == "lua" || name == "suppress"
		if got := strings.Contains(stderr, name+": "+underLink); got != readsPyOrLua {
			t.Errorf("trace %s reports %s: %v, want %v:\n%s", name, underLink, got, readsPyOrLua, stderr)
		}
	}
}

// TestModulePath: the module line is read as the go command reads it, so a trailing
// comment or a quoted path still gives ratelimiter, and boundary and oracle still catch
// their violations under a commented module line.
func TestModulePath(t *testing.T) {
	for _, c := range []struct{ gomod, want string }{
		{"module ratelimiter\n", "ratelimiter"},
		{"module ratelimiter // the rate limiter (DESIGN §A1)\n\ngo 1.24\n", "ratelimiter"},
		{"module ratelimiter//no space\n", "ratelimiter"},
		{"module \"ratelimiter\"\n", "ratelimiter"},
		{"module \"ratelimiter\" // quoted\n", "ratelimiter"},
		{"module `ratelimiter`\n", "ratelimiter"},
		{"// module other\n  module\tratelimiter\n", "ratelimiter"},
	} {
		root := t.TempDir()
		writeTestFile(t, root, "go.mod", c.gomod)
		if got, err := modulePath(t.Context(), root); err != nil || got != c.want {
			t.Errorf("modulePath(%q) = %q, %v, want %q", c.gomod, got, err, c.want)
		}
	}
	for _, gomod := range []string{"go 1.24\n", "// module ratelimiter\n", "module \"ratelimiter\n"} {
		root := t.TempDir()
		writeTestFile(t, root, "go.mod", gomod)
		if got, err := modulePath(t.Context(), root); err == nil {
			t.Errorf("modulePath(%q) = %q, want an error", gomod, got)
		}
	}
	const commented = "module ratelimiter // the rate limiter (DESIGN §A1)\n\ngo 1.24\n"
	root := fixture(t, "boundary/direct")
	writeTestFile(t, root, "go.mod", commented)
	if r, err := checkBoundary(t.Context(), testEnv(root), nil); err != nil || len(r.problems) != 3 {
		t.Errorf("boundary under a commented module line: %v\n%s", err, strings.Join(r.problems, "\n"))
	}
	root = fixture(t, "oracle")
	writeTestFile(t, root, "go.mod", commented)
	if r, err := checkOracle(t.Context(), testEnv(root), nil); err != nil || len(r.problems) != 4 {
		t.Errorf("oracle under a commented module line: %v\n%s", err, strings.Join(r.problems, "\n"))
	}
}

// TestDiskHelpers_Cancelled: every helper that reads or writes files for a caller takes
// ctx first (AGENTS.md §2) and stops on a done one before it touches the disk. Each case
// succeeds with a live ctx, so it does reach the disk, and returns context.Canceled with
// a cancelled one; writeFile then leaves no file, and procs keeps a dead PID's record.
func TestDiskHelpers_Cancelled(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "go.mod", "module ratelimiter\n\ngo 1.24\n")
	writeTestFile(t, root, "a/a.go", "//go:build rltest\n\npackage a\n")
	writeTestFile(t, root, "SPEC.md", "## 10. Tests\n\n| T-COR-1 | x |\n")
	writeTestFile(t, root, "HARNESS.md", "## 8. Tests\n\n| T-HAR-1 | x |\n")
	writeTestFile(t, root, "IMPLEMENTATION_PLAN.md", "### S0 — x\n\n| Test | Slice | Where it lives |\n|---|---|---|\n| T-COR-1 | S0 | a/ |\n")
	writeTestFile(t, root, "report/tests/test_a.py", "def test_a():\n    pass\n")
	writeTestFile(t, root, "internal/scripts/acquire.lua", "#!lua\nreturn redis.call('GET', KEYS[1])\n")
	writeTestFile(t, root, ".out/a.json", `{"Action":"pass","Package":"ratelimiter/a","Test":"TestA"}`+"\n")
	writeTestFile(t, root, ".out/a.xml", `<testsuite><testcase classname="a" name="test_a"/></testsuite>`+"\n")
	copyFile(t, root, aclTemplate)
	out := filepath.Join(root, ".out")
	for _, c := range []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"findRoot", func(ctx context.Context) error { _, err := findRoot(ctx, filepath.Join(root, "a")); return err }},
		{"modulePath", func(ctx context.Context) error { _, err := modulePath(ctx, root); return err }},
		{"readFile", func(ctx context.Context) error { _, err := readFile(ctx, root, "go.mod"); return err }},
		{"readLines", func(ctx context.Context) error { _, err := readLines(ctx, root, "go.mod"); return err }},
		{"parseGo", func(ctx context.Context) error {
			_, err := parseGo(ctx, token.NewFileSet(), root, "a/a.go", 0)
			return err
		}},
		{"fileExists", func(ctx context.Context) error { _, err := fileExists(ctx, root, "go.mod"); return err }},
		{"repoFiles", func(ctx context.Context) error { _, _, err := repoFiles(ctx, root, withSuffix(".go")); return err }},
		{"goFiles", func(ctx context.Context) error { _, _, err := goFiles(ctx, root, nil); return err }},
		{"definedIDs", func(ctx context.Context) error { _, err := definedIDs(ctx, root); return err }},
		{"parsePlan", func(ctx context.Context) error { _, err := parsePlan(ctx, root, "IMPLEMENTATION_PLAN.md"); return err }},
		{"loadResults", func(ctx context.Context) error { _, err := loadResults(ctx, out); return err }},
		{"addStream", func(ctx context.Context) error {
			return (&testResults{goTests: map[string]*testOutcome{}, goRan: map[string]bool{}}).addStream(ctx, filepath.Join(out, "a.json"))
		}},
		{"addJUnit", func(ctx context.Context) error { return (&testResults{}).addJUnit(ctx, filepath.Join(out, "a.xml")) }},
		{"pyDefNames", func(ctx context.Context) error { _, err := pyDefNames(ctx, root, "report/tests/test_a.py"); return err }},
		{"buildLine", func(ctx context.Context) error { _, err := buildLine(ctx, root, "a/a.go"); return err }},
		{"aclTemplateProblems", func(ctx context.Context) error {
			_, err := aclTemplateProblems(ctx, filepath.Join(root, filepath.FromSlash(aclTemplate)))
			return err
		}},
		{"aclScriptProblems", func(ctx context.Context) error { _, _, err := aclScriptProblems(ctx, root); return err }},
		{"luaTextProblems", func(ctx context.Context) error {
			_, err := luaTextProblems(ctx, root, []string{"internal/scripts/acquire.lua"})
			return err
		}},
		{"writeFile", func(ctx context.Context) error { return writeFile(ctx, root, "w/new.txt", "x\n") }},
	} {
		if err := c.run(t.Context()); err != nil {
			t.Errorf("%s with a live ctx: %v", c.name, err)
		}
		if c.name == "writeFile" {
			if err := os.Remove(filepath.Join(root, "w", "new.txt")); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := c.run(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a cancelled ctx: err = %v, want context.Canceled", c.name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "w", "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("writeFile with a cancelled ctx wrote w/new.txt (stat: %v)", err)
	}

	// procs with a cancelled ctx stops before it acts on a record: a dead PID's record stays.
	dead := exec.CommandContext(t.Context(), "true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	rec := writeRecord(t, root, dead.Process.Pid)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := checkProcs(ctx, testEnv(root), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("procs with a cancelled ctx: err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(rec); err != nil {
		t.Errorf("procs with a cancelled ctx removed the record %s: %v", strconv.Itoa(dead.Process.Pid), err)
	}
}
