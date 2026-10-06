package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/testutil/scriptrun"
)

// fixtureModule copies testdata/mutfix, the fixture module with its own go.mod, into a fresh
// temp directory and returns a config rooted there: its own output directory, mutants file
// and temp parent, and a short timeout so the hanging mutant costs seconds.
func fixtureModule(t *testing.T) config {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir()) // a real path, as moduleRoot gives main
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "mutfix")
	src, err := filepath.Abs(filepath.Join("testdata", "mutfix"))
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(root, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig(root, os.Environ())
	cfg.outDir = filepath.Join(tmp, "out", "mutate")
	cfg.mutantsFile = filepath.Join(tmp, "out", "mutants.txt")
	cfg.tmpDir = filepath.Join(tmp, "scratch")
	cfg.maxTimeout = 5 * time.Second
	if err := os.MkdirAll(cfg.tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// treeHashes maps every file under root to its SHA-256.
func treeHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[p] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// checkTreeUnchanged reports every file under root that changed, went missing or appeared
// since treeHashes gave before.
func checkTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := treeHashes(t, root)
	for p, h := range before {
		if after[p] != h {
			t.Errorf("%s changed or went missing during the mutant runs", p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s appeared in the tree during the mutant runs", p)
		}
	}
}

// TestMutate_PythonTempSymlink: pytest names a test by its resolved path, so under a temp
// parent reached through a symlink (macOS's $TMPDIR: /var is /private/var) the Python baseline
// still finds its named test passing, the mutant is killed by it, and no copy is left behind.
func TestMutate_PythonTempSymlink(t *testing.T) {
	cfg := fixtureModule(t)
	link := filepath.Join(filepath.Dir(cfg.tmpDir), "scratch-link")
	if err := os.Symlink(cfg.tmpDir, link); err != nil {
		t.Fatal(err)
	}
	cfg.tmpDir = link
	idx := &testIndex{root: cfg.root}
	if err := idx.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := curatedEntry{ID: "L1", File: "pkg/calc.py", Tests: []string{"pkg/calc_check.py::test_add"}, From: "return a + b", To: "return a - b"}
	res, err := runCurated(t.Context(), cfg, idx, e)
	if err != nil || res.verdict != killed || !slices.Contains(res.by, "pkg/calc_check.py::test_add") {
		t.Errorf("Python mutant under a symlinked temp parent: %s (by %v, why %q), %v; want killed by pkg/calc_check.py::test_add", res.verdict, res.by, res.why, err)
	}
	if left, _ := os.ReadDir(link); len(left) > 0 {
		t.Errorf("temp directories left behind in %s: %v", link, left)
	}
}

// TestMutate_Kill (plan S4 step 16; DESIGN §C9), on the fixture module: only an assertion
// failure in a named test kills a mutant (Go, embedded Lua, Python's rewritten assert and
// pytest.fail; a prefix name; build tags read from the test's file; named tests spread over two
// go test runs, each run's baseline answering for its own). A panic, a timeout, a pytest
// TypeError, a TestMain failure after the named test passed and an unmutated pass are no kill;
// a mutant that does not build, parse or compile is invalid. A failing unmutated baseline (in
// any run), a name that matches nothing or that no run passes, an L3 or a conformance test, and
// a From that matches no line are errors, and no mutant runs.
func TestMutate_Kill(t *testing.T) {
	cfg := fixtureModule(t)
	idx := &testIndex{root: cfg.root}
	if err := idx.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	add := curatedEntry{File: "calc/calc.go", From: "return a + b", To: "return a - b"}
	pyAdd := curatedEntry{File: "pkg/calc.py", From: "return a + b", To: "return a - b"}
	hello := curatedEntry{File: "lua/hello.lua", Tests: []string{"TestHello"}, From: "local n = 1"}
	with := func(e curatedEntry, id string, tests ...string) curatedEntry {
		e.ID = id
		if len(tests) > 0 {
			e.Tests = tests
		}
		return e
	}
	edit := func(e curatedEntry, from, to string) curatedEntry {
		e.From, e.To = from, to
		return e
	}
	cases := []struct {
		name  string
		entry curatedEntry
		want  verdict
		why   string // a part of the result's reason
		by    string // a test that killed it
	}{
		{"an assertion kills", with(add, "K1", "TestAdd"), killed, "", "TestAdd"},
		{"a prefix name", with(add, "K2", "TestAdd_*"), killed, "", "TestAdd_"},
		{"tags from the test's file", with(add, "K3", "TestTagged"), killed, "", "TestTagged"},
		{"a panic", with(curatedEntry{File: "calc/calc.go", From: "return xs[i]", To: "return xs[i+5]"}, "K4", "TestAt"), notKilled, "panic", ""},
		{"a build error", with(edit(add, "return a + b", "return a +"), "K5", "TestAdd"), invalid, "does not build", ""},
		{"a timeout", with(curatedEntry{File: "calc/calc.go", From: "d := time.Duration(0)", To: "d := time.Hour"}, "K6", "TestPause"), notKilled, "timeout", ""},
		{"an unmutated pass", with(curatedEntry{File: "calc/calc.go", From: "return xs[i]", To: "return xs[1]"}, "K7", "TestAdd"), survived, "", ""},
		{"an embedded Lua assertion", with(edit(hello, "local n = 1", "local n = 2"), "K8"), killed, "", "TestHello"},
		{"a script that does not parse", with(edit(hello, "local n = 1", "local n = = 1"), "K9"), invalid, "does not parse: hello.lua:2:11: (E011)", ""},
		{"a prelude that does not parse", with(curatedEntry{File: "lua/prelude.lua", From: "local OK = 40", To: "local OK = ("}, "K10", "TestHello"), invalid, "does not parse: prelude.lua:2:1: (E011)", ""},
		{"a rewritten assert", with(pyAdd, "K11", "pkg/calc_check.py::test_add"), killed, "", "pkg/calc_check.py::test_add"},
		{"pytest.fail", with(pyAdd, "K12", "pkg/calc_check.py::test_add_fail"), killed, "", "pkg/calc_check.py::test_add_fail"},
		{"a Python file of tests", with(pyAdd, "K13", "pkg/calc_check.py"), killed, "", "pkg/calc_check.py::test_add"},
		{"a TypeError", with(curatedEntry{File: "pkg/calc.py", From: "return str(n)", To: "return str(n) + n"}, "K14", "pkg/calc_check.py::test_label"), notKilled, "TypeError", ""},
		{"Python that does not compile", with(edit(pyAdd, "return a + b", "return a +"), "K15", "pkg/calc_check.py::test_add"), invalid, "does not compile", ""},
		// Named tests in two go test runs: each run's baseline answers for its own tests only.
		{"tests in two builds of a package", with(add, "K16", "TestAdd", "TestTagged"), killed, "", "TestAdd"},
		{"tests in two packages, the second run killing", with(add, "K17", "TestCheck", "TestAdd"), killed, "", "TestAdd"},
		// The mutant fails only TestMain's check after the tests (a leak check), never a named test.
		{"a TestMain failure after the named test passes", with(curatedEntry{File: "teardown/teardown.go", From: "return true", To: "return false"}, "K18", "TestTeardownOne"),
			notKilled, "go test exited with 1, but no named test failed", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res, err := runCurated(t.Context(), cfg, idx, c.entry)
			if err != nil {
				t.Fatalf("%s: %v", c.entry.ID, err)
			}
			if res.verdict != c.want || !strings.Contains(res.why, c.why) ||
				(c.by != "" && !slices.ContainsFunc(res.by, func(b string) bool { return strings.HasPrefix(b, c.by) })) {
				t.Errorf("%s: %s (why %q, by %v), want %s (why containing %q, by %q); log %s", c.entry.ID, res.verdict, res.why, res.by, c.want, c.why, c.by, res.log)
			}
		})
	}

	errCases := []struct {
		name  string
		entry curatedEntry
		err   string
	}{
		{"a failing baseline", with(add, "E1", "TestBroken"), "TestBroken fails on the unmutated code"},
		{"a failing Python baseline", with(pyAdd, "E2", "pkg/broken_check.py::test_broken"), "fails on the unmutated code"},
		{"a name that matches nothing", with(add, "E3", "TestNoSuch"), "TestNoSuch matches no test function"},
		{"a Python name that matches nothing", with(pyAdd, "E4", "pkg/calc_check.py::test_nothing"), "test_nothing reports no pass"},
		{"a conformance test", with(add, "E5", "TestConf"), "conformance"},
		{"an L3 test", with(add, "E6", "TestL3"), "an L3 test"},
		{"a From that matches no line", with(edit(add, "return a * b", "return a / b"), "E7", "TestAdd"), "matches no line"},
		{"a Python test outside the package", with(pyAdd, "E8", "calc/x.py::test_x"), "outside pkg/"},
		{"a run that fails after its tests pass", with(curatedEntry{File: "leaky/leaky.go", From: "return 1", To: "return 2"}, "E9", "TestOne"), "go test exited with 1"},
		{"a failing baseline in the second run", with(add, "E10", "TestTagged", "TestBroken"), "TestBroken fails on the unmutated code"},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res, err := runCurated(t.Context(), cfg, idx, c.entry)
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %s, %v; want an error containing %q", c.entry.ID, res.verdict, err, c.err)
			}
			// No mutant ran: only a baseline may have left output.
			if logs, _ := filepath.Glob(filepath.Join(cfg.outDir, c.entry.ID+".mutant*")); len(logs) > 0 {
				t.Errorf("%s: a mutant ran after the error: %v", c.entry.ID, logs)
			}
		})
	}

	// Every named test must pass in some baseline run, even when no run answers for it.
	t.Run("a named test that no run passes", func(t *testing.T) {
		t.Parallel()
		content, err := os.ReadFile(filepath.Join(cfg.root, "calc", "calc.go"))
		if err != nil {
			t.Fatal(err)
		}
		mutated, err := applyLineEdit(content, "return a + b", "return a - b")
		if err != nil {
			t.Fatal(err)
		}
		m := mutant{id: "E11", file: "calc/calc.go", content: mutated}
		runs := []goRun{{pkgs: []string{"./calc"}, run: "^(?:TestAdd)$", names: []string{"TestAdd"}}}
		res, err := runGoMutant(t.Context(), cfg, m, runs, []string{"TestAdd", "TestTagged"})
		if err == nil || !strings.Contains(err.Error(), "TestTagged reports no pass") {
			t.Errorf("E11: %s, %v; want an error naming TestTagged, which no run passed", res.verdict, err)
		}
		if logs, _ := filepath.Glob(filepath.Join(cfg.outDir, "E11.mutant*")); len(logs) > 0 {
			t.Errorf("E11: a mutant ran after the error: %v", logs)
		}
	})
}

// TestMutate_Tally: a --curated run over fixture entries reports each kill, counts an entry
// whose baseline fails as an error and no mutant, skips an entry whose file does not exist, and
// exits 1; a survivor alone and a failing baseline alone each exit 1, while kills and skipped
// entries alone exit 0.
func TestMutate_Tally(t *testing.T) {
	cfg := fixtureModule(t)
	entries := []curatedEntry{
		{ID: "T1", File: "calc/calc.go", Tests: []string{"TestAdd"}, From: "return a + b", To: "return a - b"},
		{ID: "T2", File: "calc/calc.go", Tests: []string{"TestAdd"}, From: "return xs[i]", To: "return xs[1]"},
		{ID: "T3", File: "calc/calc.go", Tests: []string{"TestBroken"}, From: "return a + b", To: "return a - b"},
		{ID: "T4", File: "calc/later.go", Tests: []string{"TestAdd"}, From: "x", To: "y"},
	}
	var out, errOut strings.Builder
	code := curatedCmd(t.Context(), cfg, entries, nil, &printer{w: &out}, &printer{w: &errOut})
	want := "2 curated mutant(s) run: 1 killed, 1 not killed; 1 error(s), counting no mutant; 1 skipped"
	if code != 1 || !strings.Contains(out.String(), "T1 (calc/calc.go): killed by TestAdd") || !strings.Contains(out.String(), want) ||
		!strings.Contains(errOut.String(), "T2 (calc/calc.go): survived") || !strings.Contains(errOut.String(), "T3 (calc/calc.go): baseline") {
		t.Errorf("curated run: exit %d, want 1 with %q\nstdout:\n%s\nstderr:\n%s", code, want, out.String(), errOut.String())
	}
	out.Reset()
	if code := curatedCmd(t.Context(), cfg, entries, []string{"T1"}, &printer{w: &out}, &printer{w: &errOut}); code != 0 ||
		!strings.Contains(out.String(), "1 curated mutant(s) run: 1 killed, 0 not killed; 0 error(s)") {
		t.Errorf("curated run of T1 alone: exit %d, want 0\n%s", code, out.String())
	}
	// Each cause of exit 1 on its own: a survivor alone, a failing baseline alone; a kill and an
	// entry whose file does not exist yet exit 0.
	for _, c := range []struct {
		ids     []string
		code    int
		summary string
	}{
		{[]string{"T2"}, 1, "1 curated mutant(s) run: 0 killed, 1 not killed; 0 error(s), counting no mutant; 0 skipped"},
		{[]string{"T3"}, 1, "0 curated mutant(s) run: 0 killed, 0 not killed; 1 error(s), counting no mutant; 0 skipped"},
		{[]string{"T1", "T4"}, 0, "1 curated mutant(s) run: 1 killed, 0 not killed; 0 error(s), counting no mutant; 1 skipped"},
	} {
		out.Reset()
		if code := curatedCmd(t.Context(), cfg, entries, c.ids, &printer{w: &out}, &printer{w: &errOut}); code != c.code || !strings.Contains(out.String(), c.summary) {
			t.Errorf("curated run of %v: exit %d, want %d with %q\n%s", c.ids, code, c.code, c.summary, out.String())
		}
	}
}

// TestMutate_Overlay (plan S4 step 16): after mutant runs of a Go file, an embedded Lua file and
// a Python file, every file of the fixture tree is byte-identical and no file was added, and the
// overlay's replacement files lay outside the tree and are gone. Run with a PWD that reaches the
// tree through a symlink, the Go and Lua mutants still reach the tests (killed).
func TestMutate_Overlay(t *testing.T) {
	cfg := fixtureModule(t)
	before := treeHashes(t, cfg.root)
	idx := &testIndex{root: cfg.root}
	for _, e := range []curatedEntry{
		{ID: "O1", File: "calc/calc.go", Tests: []string{"TestAdd"}, From: "return a + b", To: "return a - b"},
		{ID: "O2", File: "lua/hello.lua", Tests: []string{"TestHello"}, From: "local n = 1", To: "local n = 2"},
		{ID: "O3", File: "pkg/calc.py", Tests: []string{"pkg/calc_check.py::test_add"}, From: "return a + b", To: "return a - b"},
	} {
		res, err := runCurated(t.Context(), cfg, idx, e)
		if err != nil {
			t.Fatalf("%s: %v", e.ID, err)
		}
		if res.verdict != killed {
			t.Errorf("%s: %s%s, want killed (the mutant reached the tests)", e.ID, res.verdict, res.whyText())
		}
		if strings.HasSuffix(e.File, ".py") {
			continue // patched in a temp copy of the package, no overlay
		}
		want := filepath.Join(cfg.root, filepath.FromSlash(e.File))
		if len(res.overlay) != 1 || res.overlay[want] == "" {
			t.Fatalf("%s: overlay %v, want one replacement for %s", e.ID, res.overlay, want)
		}
		cp := res.overlay[want]
		rel, err := filepath.Rel(cfg.root, cp)
		if err != nil {
			t.Fatal(err)
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%s: the replacement %s lies inside the tree %s", e.ID, cp, cfg.root)
		}
		if _, err := os.Stat(cp); !os.IsNotExist(err) {
			t.Errorf("%s: the replacement %s is still there after the run (stat: %v)", e.ID, cp, err)
		}
	}
	// Run from a path with a symlink in it (a symlinked checkout; macOS's /tmp and /var), the
	// caller's PWD names the tree by the link, while the overlay names its files by the real
	// path: the go command must name them the same way, or the mutant never reaches the tests.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(cfg.root, link); err != nil {
		t.Fatal(err)
	}
	if root, err := moduleRoot(link); err != nil || root != cfg.root {
		t.Fatalf("moduleRoot(%s) = %q, %v; want the real path %s", link, root, err, cfg.root)
	}
	linked := cfg
	linked.environ = append(slices.Clone(cfg.environ), "PWD="+link)
	for _, e := range []curatedEntry{
		{ID: "O4", File: "calc/calc.go", Tests: []string{"TestAdd"}, From: "return a + b", To: "return a - b"},
		{ID: "O5", File: "lua/hello.lua", Tests: []string{"TestHello"}, From: "local n = 1", To: "local n = 2"},
	} {
		res, err := runCurated(t.Context(), linked, idx, e)
		if err != nil {
			t.Fatalf("%s from the symlinked path: %v", e.ID, err)
		}
		if res.verdict != killed {
			t.Errorf("%s from the symlinked path %s: %s%s, want killed (the mutant reached the tests); log %s", e.ID, link, res.verdict, res.whyText(), res.log)
		}
	}
	checkTreeUnchanged(t, cfg.root, before)
	if left, _ := os.ReadDir(cfg.tmpDir); len(left) > 0 {
		t.Errorf("temp directories left behind in %s: %v", cfg.tmpDir, left)
	}
}

// TestMutate_RepliesFile (plan S4 Watch out): every go test bin/mutate starts, baseline or
// mutant, curated or sweep, gets RL_REPLIES_FILE naming a file of its own in a temp directory
// outside the tree, so the replies a mutant's L2 tests record never reach the module's
// .out/replies-l2.jsonl, where bin/trace replies would count them. The fixture's
// TestRecordReply records where scriptrun.Run does: in the file the variable
// scriptrun.RepliesEnv names (the test passes that name in $MUTFIX_REPLIES_ENV, so the fixture
// reads the variable scriptrun reads, not a copy of its name), or the module's
// .out/replies-l2.jsonl when the variable is unset; it lists the file it used in $MUTFIX_SEEN.
// After a curated mutant and a sweep, each of the five runs used a file of its own outside the
// tree, gone with its temp directory, and the tree, its .out/replies-l2.jsonl included, is
// byte-identical.
func TestMutate_RepliesFile(t *testing.T) {
	cfg := fixtureModule(t)
	writeFiles(t, cfg.root, map[string]string{".out/replies-l2.jsonl": `{"script":"acquire","code":1,"reason":""}` + "\n"})
	seen := filepath.Join(t.TempDir(), "seen.txt")
	cfg.environ = append(slices.Clone(cfg.environ), "MUTFIX_SEEN="+seen, "MUTFIX_REPLIES_ENV="+scriptrun.RepliesEnv)
	cfg.goScope = []string{"replies"}
	before := treeHashes(t, cfg.root)

	e := curatedEntry{ID: "R1", File: "replies/replies.go", Tests: []string{"TestRecordReply"}, From: "return 1", To: "return 2"}
	res, err := runCurated(t.Context(), cfg, &testIndex{root: cfg.root}, e)
	if err != nil {
		t.Fatalf("%s: %v", e.ID, err)
	}
	if res.verdict != killed {
		t.Errorf("%s: %s%s, want killed (the mutant reached the test); log %s", e.ID, res.verdict, res.whyText(), res.log)
	}
	var out, errOut strings.Builder
	if code := sweepCmd(t.Context(), cfg, []string{e.File}, time.Hour, &printer{w: &out}, &printer{w: &errOut}); code != 0 ||
		!strings.Contains(out.String(), "2 mutant(s) of 1 file(s), budget 1h0m0s: 2 killed") {
		t.Errorf("sweep of %s: exit %d, want 0 with both mutants killed\nstdout:\n%s\nstderr:\n%s", e.File, code, out.String(), errOut.String())
	}

	data, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	// R1's baseline and mutant, then the sweep's baseline and its two mutants.
	paths := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(paths) != 5 {
		t.Errorf("the fixture test recorded %d replies files, want 5, one per go test run:\n%s", len(paths), data)
	}
	used := map[string]bool{}
	for _, p := range paths {
		rel, err := filepath.Rel(cfg.root, p)
		if !filepath.IsAbs(p) || err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			t.Errorf("a run recorded its replies in %s, want a file outside the tree %s", p, cfg.root)
		}
		if used[p] {
			t.Errorf("two runs recorded their replies in %s, want a file of its own for each run", p)
		}
		used[p] = true
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the replies file %s is still there after its run (stat: %v)", p, err)
		}
	}
	checkTreeUnchanged(t, cfg.root, before)
	if left, _ := os.ReadDir(cfg.tmpDir); len(left) > 0 {
		t.Errorf("temp directories left behind in %s: %v", cfg.tmpDir, left)
	}
}

// TestMutate_RepliesEnv (plan S4 step 16 and Watch out): the variable bin/mutate sets for every
// go test it starts (repliesEnv) is the one scriptrun.Run reads (scriptrun.RepliesEnv).
// bin/mutate's own code imports only the standard library, so it keeps a copy of the name; if
// the copy differs, scriptrun never sees the temp file, and the replies of every mutant's L2
// tests go back into the module's .out/replies-l2.jsonl, where bin/trace replies counts them.
func TestMutate_RepliesEnv(t *testing.T) {
	if repliesEnv != scriptrun.RepliesEnv {
		t.Errorf("bin/mutate sets %s for its go test runs, but scriptrun.Run reads %s: a mutant's L2 replies would reach the module's %s",
			repliesEnv, scriptrun.RepliesEnv, scriptrun.RepliesFile)
	}
}

// TestMutate_Sweep (plan S4 step 16; DESIGN §C9): every operator on a Go file of an operator
// directory (whose package also has an rltest test, so its mutants run a second time with that
// tag) and on an embedded Lua file; the mutants that are not killed (a comparison the tests
// never reach the edge of, a flip that panics, a 'GT' no test reads, a write_hwm() no test
// reads) and only they go to the mutants file, and the run passes. The pinned count and lines
// change when any operator is dropped. Files with no operators are noted, with no budget left
// every mutant is listed untried, and a failing unmutated baseline stops the sweep before any
// mutant runs.
func TestMutate_Sweep(t *testing.T) {
	cfg := fixtureModule(t)
	cfg.luaPackages = []string{"./lua/..."}
	files := []string{"internal/core/core.go", "lua/hello.lua", "calc/calc.go", "calc/calc_test.go", "pkg/calc.py"}
	var out, errOut strings.Builder
	code := sweepCmd(t.Context(), cfg, files, time.Hour, &printer{w: &out}, &printer{w: &errOut})
	if code != 0 {
		t.Fatalf("sweep: exit %d, want 0 (survivors never fail it)\n%s\n%s", code, out.String(), errOut.String())
	}
	data, err := os.ReadFile(cfg.mutantsFile)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	want := []string{
		"survived   internal/core/core.go:11  flip < to <=",
		"not killed internal/core/core.go:22  flip >= to > (panic)",
		"survived   internal/core/core.go:35  flip > to >=",
		"survived   lua/hello.lua:3  'GT' → 'LT'",
		"survived   lua/hello.lua:13  delete write_hwm()",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("mutants file:\n%s\nwant the survivors:\n%s", data, strings.Join(want, "\n"))
	}
	// core.go, 10: flip each of <, >=, <=, >, == and != (killed: <= and ==; != only by the rltest
	// run), negate both ifs and return nil for both errors (all killed). hello.lua, 11: delete
	// each write call (HSET, pcall(redis.call, 'HSET', …), redis.pcall('HDEL', …) and ZADD killed;
	// the DEL line opens an if, so that mutant does not parse), delete write_hwm(), delete 'XX',
	// reply→finish, finish→reply, 'GT'→'LT', swap OK and REFUSED.
	for _, w := range []string{"21 mutant(s) of 2 file(s)", "15 killed, 4 survived, 1 not killed, 1 invalid, 0 untried"} {
		if !strings.Contains(string(data), w) || !strings.Contains(out.String(), w) {
			t.Errorf("summary lacks %q:\n%s\n%s", w, data, out.String())
		}
	}
	for _, w := range []string{"calc/calc.go: outside", "calc/calc_test.go: a test file", "pkg/calc.py: no §C9 operators"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("output lacks the note %q:\n%s", w, out.String())
		}
	}

	out.Reset()
	if code := sweepCmd(t.Context(), cfg, files[:2], time.Nanosecond, &printer{w: &out}, &printer{w: &errOut}); code != 0 {
		t.Fatalf("sweep with no budget: exit %d, want 0\n%s", code, errOut.String())
	}
	data, err = os.ReadFile(cfg.mutantsFile)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\nuntried "); n != 21 || !strings.Contains(string(data), "0 killed, 0 survived, 0 not killed, 0 invalid, 21 untried") {
		t.Errorf("sweep with no budget: %d untried lines, want all 21:\n%s", n, data)
	}

	// internal/store's unmutated test fails, so no mutant of it can be judged: the sweep stops at
	// the baseline, runs no mutant and writes no mutants file.
	broken := fixtureModule(t)
	out.Reset()
	errOut.Reset()
	if code := sweepCmd(t.Context(), broken, []string{"internal/store/store.go"}, time.Hour, &printer{w: &out}, &printer{w: &errOut}); code != 1 ||
		!strings.Contains(errOut.String(), "baseline go test ./internal/store: go test exited with 1") {
		t.Errorf("sweep with a failing baseline: exit %d, want 1 naming the baseline\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(broken.mutantsFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sweep with a failing baseline wrote %s (stat: %v)", broken.mutantsFile, err)
	}
	if logs, _ := filepath.Glob(filepath.Join(broken.outDir, "s*.mutant*.log")); len(logs) > 0 || strings.Contains(out.String(), "mutant(s) of") {
		t.Errorf("sweep with a failing baseline ran mutants: %v\n%s", logs, out.String())
	}
}

// TestMutate_Args: the command line; a mistake is a usage error.
func TestMutate_Args(t *testing.T) {
	for _, c := range []struct {
		args []string
		err  string // "" when it parses
	}{
		{[]string{"--curated"}, ""},
		{[]string{"--curated", "PB1", "PB2"}, ""},
		{[]string{"--sweep", "a.lua", "b.go", "--budget", "5m"}, ""},
		{[]string{"--budget=1m", "--sweep", "a.lua"}, ""},
		{[]string{"--curated", "--timeout", "1m"}, ""},
		{nil, "give --curated or --sweep"},
		{[]string{"--sweep"}, "needs the files"},
		{[]string{"--curated", "--sweep", "a"}, "once"},
		{[]string{"--curated", "--budget", "1m"}, "--budget is for --sweep"},
		{[]string{"--sweep", "a", "--budget", "soon"}, "not a positive duration"},
		{[]string{"--sweep", "a", "--budget"}, "needs a duration"},
		{[]string{"--curated", "--verbose"}, "unknown flag"},
		{[]string{"PB1", "--curated"}, "unexpected argument"},
	} {
		_, err := parseArgs(c.args)
		if (err == nil) != (c.err == "") || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("parseArgs(%q) = %v, want %q", c.args, err, c.err)
		}
	}
	var out, errOut strings.Builder
	noConfig := func() (config, error) { t.Fatal("a usage error built the config"); return config{}, nil }
	if code := run(t.Context(), []string{"--nope"}, &out, &errOut, noConfig); code != 2 || !strings.Contains(errOut.String(), "usage:") {
		t.Errorf("run(--nope): exit %d, want 2 with the usage\n%s", code, errOut.String())
	}
	if code := run(t.Context(), []string{"--help"}, &out, &errOut, noConfig); code != 0 || !strings.Contains(out.String(), "--sweep") {
		t.Errorf("run(--help): exit %d, want 0 with the usage\n%s", code, out.String())
	}
	if code := run(t.Context(), []string{"--curated", "PB99"}, &out, &errOut, func() (config, error) { return config{root: t.TempDir()}, nil }); code != 2 {
		t.Errorf("run(--curated PB99): exit %d, want 2 (no such entry)", code)
	}
}
