package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSlices_RealPlanTable(t *testing.T) {
	root := repoRoot(t)
	plan, err := parsePlan(t.Context(), root, "IMPLEMENTATION_PLAN.md")
	if err != nil {
		t.Fatal(err)
	}
	defs, err := definedIDs(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.rows) != 83 {
		t.Fatalf("the Test → slice table has %d rows, want 83", len(plan.rows))
	}
	if problems := tableProblems(plan, defs); len(problems) > 0 {
		t.Fatalf("table:\n%s", strings.Join(problems, "\n"))
	}
	// The same, directly rather than through tableProblems: the rows name the 83 defined
	// IDs, each exactly once, and each row's slice is one of the plan's.
	defined := map[string]bool{}
	for _, d := range defs {
		defined[d.id] = true
	}
	if len(defined) != 83 {
		t.Fatalf("%d distinct defined IDs, want 83", len(defined))
	}
	rows := map[string]int{}
	for _, row := range plan.rows {
		rows[row.id]++
		if !planSliceRe.MatchString(row.slice) {
			t.Errorf("IMPLEMENTATION_PLAN.md:%d: %s has slice %q, want S0…S9, H1…H6 or D1", row.line, row.id, row.slice)
		}
		if !defined[row.id] {
			t.Errorf("IMPLEMENTATION_PLAN.md:%d: %s is not a defined ID", row.line, row.id)
		}
	}
	for id := range defined {
		if rows[id] != 1 {
			t.Errorf("%s appears %d times in the Test → slice table, want once", id, rows[id])
		}
	}
	// Every slice has a section with its dependencies; S2 and S3 do not depend on S1.
	for _, s := range slicePlan() {
		if _, err := plan.path(s.id); err != nil {
			t.Errorf("%s: %v", s.id, err)
		}
	}
	for _, id := range []string{"S2", "S3"} {
		if p, err := plan.path(id); err != nil || p["S1"] || !p["S0"] {
			t.Errorf("dependency path of %s = %v, %v: want S0 and not S1", id, p, err)
		}
	}
}

// planSliceRe matches the plan's slice IDs, in order S0…S9, H1…H6, D1.
var planSliceRe = regexp.MustCompile(`^(S[0-9]|H[1-6]|D1)$`)

// TestSlices_TableRules: each rule of the Test → slice table fails on its own row: an ID
// listed twice, a defined ID left out, a row with an unknown slice and a row naming an
// undefined ID. Every row is off S0's dependency path, so nothing else is reported.
func TestSlices_TableRules(t *testing.T) {
	code, stdout, stderr := runIn(t, fixture(t, "slices/table"), "slices", "--upto", "S0")
	problems := strings.Split(strings.TrimSpace(stderr), "\n")
	want := []string{
		"slices: IMPLEMENTATION_PLAN.md: T-COR-1 appears 2 times in the Test → slice table, want exactly once",
		"slices: IMPLEMENTATION_PLAN.md: T-COR-2 appears 0 times in the Test → slice table, want exactly once",
		`slices: IMPLEMENTATION_PLAN.md:18: T-COR-3 has unknown slice "S99"`,
		"slices: IMPLEMENTATION_PLAN.md:21: T-COR-99 is not an ID that SPEC.md §10 or HARNESS.md §8 defines",
	}
	for _, w := range want {
		if countContaining(problems, w) != 1 {
			t.Errorf("want one problem %q", w)
		}
	}
	if code != 1 || len(problems) != len(want)+1 || !strings.HasPrefix(stdout, "slices: 0 of 0 IDs up to S0 passed") {
		t.Fatalf("exit %d, want 1 with the %d problems and the FAIL line\nstdout: %s\nstderr:\n%s", code, len(want), stdout, stderr)
	}
}

// slicesFixture copies a slices fixture and moves its out/ to the default .out/ (git
// ignores every .out/ directory, so the fixtures keep theirs as out/).
func slicesFixture(t *testing.T, name string) string {
	t.Helper()
	root := fixture(t, name)
	if err := os.Rename(filepath.Join(root, "out"), filepath.Join(root, ".out")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSlices_Results(t *testing.T) {
	code, stdout, stderr := runIn(t, slicesFixture(t, "slices/results"), "slices", "--upto", "S0")
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	problems := strings.Split(strings.TrimSpace(stderr), "\n")
	for _, want := range []string{
		"T-COR-2 (S0): TestT_COR_2 failed",
		"T-COR-3 (S0): TestT_COR_3 was skipped",
		"T-COR-4 (S0): wrongly located: TestT_COR_4 is in internal/misplaced/misplaced_test.go",
		"T-COR-5 (S0): missing: no test function TestT_COR_5 or test_t_cor_5",
		"T-COR-6 (S0): TestT_COR_6 has no result",
		"T-RPT-2 (S0): test_t_rpt_2 failed",
		// A fail in any run or any results file is a fail, whatever passed after it.
		"T-COR-7 (S0): TestT_COR_7 failed",  // fail, then pass, in one stream (-count=2)
		"T-COR-8 (S0): TestT_COR_8 failed",  // fail in test-integration.json, pass in test-unit.json
		"T-RPT-3 (S0): test_t_rpt_3 failed", // fail in test-py.xml, pass in test-rerun.xml
		// A skip in any run, subtest or parametrized case is a skip, whatever passed beside it.
		"T-COR-9 (S0): TestT_COR_9 was skipped",   // pass in test-unit.json, skip in test-integration.json
		"T-RPT-4 (S0): test_t_rpt_4 was skipped",  // case [a] passed, case [b] skipped
		"T-COR-10 (S0): TestT_COR_10 was skipped", // passed, but its table case b was skipped
	} {
		if countContaining(problems, "slices: "+want) != 1 {
			t.Errorf("want one problem %q", want)
		}
	}
	// T-COR-1 passes: the skipped TestT_COR_10/b is not one of its subtests.
	if len(problems) != 13 { // twelve problems and the FAIL line
		t.Errorf("stderr:\n%s", stderr)
	}
	if !strings.HasPrefix(stdout, "slices: 2 of 14 IDs up to S0 passed, 5 failed, 4 skipped\n") {
		t.Errorf("summary: %s", stdout)
	}

	// A fully passing results directory.
	code, stdout, stderr = runIn(t, slicesFixture(t, "slices/passing"), "slices", "--upto", "S0")
	want := "slices: 2 of 2 IDs up to S0 passed, 0 failed, 0 skipped\n" +
		"slices: 1 named tests up to S0: 1 passed, 0 existence only\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("exit %d\nstdout: %q\nwant:   %q\nstderr: %s", code, stdout, want, stderr)
	}
}

func TestSlices_NamedTests(t *testing.T) {
	// The fixture's Makefile gives make -n a -skip pattern, so run in a copy.
	root := fixture(t, "slices/named")
	code, stdout, stderr := runIn(t, root, "slices", "--upto", "S2", "--results", "out")
	problems := strings.Split(strings.TrimSpace(stderr), "\n")
	reported := []string{
		"TestMissing: missing: no func TestMissing",                      // a missing test
		"TestRenamed: missing: no func TestRenamed",                      // renamed: no prefix match
		"TestNoMatch_*: missing: no func TestNoMatch_…",                  // a star with no match
		"TestNoPass: ratelimiter/pkg/ran ran",                            // its package ran, no pass (a -skip in a recipe comment or an echo exempts nothing)
		"TestGap_B: ratelimiter/pkg/ran ran",                             // its parent passed, but subtest B never ran
		"report/tests/test_a.py::test_py_nopass: the JUnit results have", // its file has results
		"TestFlaky: TestFlaky failed in ratelimiter/pkg/ran",             // a fail in one run, a pass in another
		"TestFlakyStream: TestFlakyStream failed in ratelimiter/pkg/ran", // a fail in one stream, a pass in another
		// A skip in any run, subtest or case is a problem, whatever passed beside it (never
		// t.Skip or pytest.skip). TestGroup_B passes: the skipped TestGroup/A/y is not under it.
		"TestSkipThenPass: TestSkipThenPass was skipped in ratelimiter/pkg/ran",
		"report/tests/test_a.py::test_py_skip: report/tests/test_a.py::test_py_skip was skipped",
		"TestCaseSkip: TestCaseSkip was skipped in ratelimiter/pkg/ran", // its table case b was skipped
		"TestGroup_A: TestGroup/A was skipped in ratelimiter/pkg/ran",   // its case TestGroup/A/y was skipped
		// A -skip exempts only the packages its own command runs: the one that skips
		// TestUnrelatedSkip runs ./pkg/unrelated, not ./pkg/ran.
		"TestUnrelatedSkip: ratelimiter/pkg/ran ran",
	}
	for _, want := range reported {
		if countContaining(problems, want) != 1 {
			t.Errorf("want one problem containing %q", want)
		}
	}
	// The bare test_py_underscore is defined only in report/_impl/test_c.py: the walk for
	// Python defs reads "_" directories, so it is found (and passed), never missing.
	if strings.Contains(stderr, "test_py_underscore") {
		t.Errorf("test_py_underscore, defined in report/_impl/test_c.py, was reported:\n%s", stderr)
	}
	// A -skip exempts in a command after then or do, and in a gotest run whose program has
	// another name (a TRACE override), as in any other command.
	for _, exempt := range []string{"TestThenSkip", "TestDoSkip", "TestWrapperSkip"} {
		if strings.Contains(stderr, exempt) {
			t.Errorf("%s, excluded by a target command's -skip, was reported:\n%s", exempt, stderr)
		}
	}
	if code != 1 || len(problems) != len(reported)+1 {
		t.Fatalf("exit %d, want 1 with %d problems\nstderr:\n%s", code, len(reported), stderr)
	}
	// Passed: TestPass, TestStar_One, TestStar_Two, TestBrace_A and _B, TestGap_A and
	// TestGroup_B (as subtests), TestOther_kept (subtest), test_py_pass and
	// test_py_underscore (in report/_impl/test_c.py). Existence only: TestOtherPkg
	// (no target ran it), TestSkipped and TestOther_sub (excluded by the alternatives of
	// the ./... command's -skip 'TestSkipped|TestOther/sub', as go test splits it),
	// TestRanOnly (excluded by the -skip of the command that runs ./pkg/ran),
	// TestThenSkip, TestDoSkip and TestWrapperSkip (excluded by commands after then and
	// do, and by /tmp/trace-wrapper gotest), ExampleNoOutput (never run) and
	// test_py_nojunit (no JUnit for its file). Ignored: TestT_COR_99, the names outside
	// Tests, prose, and out/marker-latency.json.
	want := "slices: 0 of 0 IDs up to S2 passed, 0 failed, 0 skipped\n" +
		"slices: 32 named tests up to S2: 10 passed, 9 existence only, 13 with problems\n"
	if stdout != want {
		t.Fatalf("stdout: %q\nwant:   %q", stdout, want)
	}
	for _, ignored := range []string{"TestT_COR_99", "TestOutside", "TestAfterVerify", "TestOnlyInS1", "marker-latency"} {
		if strings.Contains(stderr, ignored) {
			t.Errorf("%s was not ignored:\n%s", ignored, stderr)
		}
	}
	// S1 is on its own dependency path.
	_, _, stderr = runIn(t, root, "slices", "--upto", "S1", "--results", "out")
	if !strings.Contains(stderr, "S1 (plan line") || !strings.Contains(stderr, "TestOnlyInS1: missing") {
		t.Fatalf("--upto S1 does not check S1's tests:\n%s", stderr)
	}
}

func TestSkipMatches(t *testing.T) {
	// Each case as go test -skip (go1.27.1) decides it, run by hand on a module with
	// these tests and subtests.
	for _, c := range []struct {
		pattern, test string
		want          bool
	}{
		{"TestSkipped|TestOther/sub", "TestSkipped", true},
		{"TestSkipped|TestOther/sub", "TestOther", false}, // a partial match: the parent runs
		{"TestSkipped|TestOther/sub", "TestOther/sub", true},
		{"TestSkipped|TestOther/sub", "TestOther/kept", false},
		{"TestOther", "TestOther/sub", true}, // a skipped parent takes its subtests along
		{"TestA/x|TestA", "TestA", false},    // the first alternative that matches decides
		{"TestA/x|TestA", "TestA/b", true},   // … and here it is the second
		{"Test(A|B)", "TestB", true},         // no split inside ()
		{"Test(A|B)/x", "TestA/x", true},     //
		{"Test(A|B)/x", "TestA/b", false},    //
		{"Test[|]A|TestB", "TestB", true},    // nor inside []
		{`TestX/a\|b`, "TestX/a|b", true},    // nor after a backslash
		{"TestX/a b", "TestX/a_b", true},     // white space is rewritten as _
		{"TestA", "TestAB", true},            // unanchored
		{"^TestA$", "TestAB", false},         //
		{"", "TestA", false},                 // no pattern skips nothing
		{"Test(", "TestA", false},            // nor does one that does not compile
	} {
		if got := skipMatches(c.pattern, c.test); got != c.want {
			t.Errorf("skipMatches(%q, %q) = %v, want %v", c.pattern, c.test, got, c.want)
		}
	}
}

// TestGoTestCommands: the go test commands of a recipe as make -n prints it, each with
// its package patterns and -skip, read as the go command reads them (each case checked
// by hand against go1.27.1 on a module with packages ., ./a and ./b), and found as the
// shell finds commands: never in a comment or as another command's arguments.
func TestGoTestCommands(t *testing.T) {
	recipe := "go build -o bin/trace ./tools/trace\n" + // not a go test run
		"bin/trace gotest -o .out/test-unit.json -- -race -count=1 -skip 'TestA|TestB/x' ./...\n" +
		"RL_SCRIPT_AUDIT=1 bin/trace gotest -o=.out/s.json -- -tags scripttest -p 1 -timeout 45m ./internal/scripts/... && echo done\n" +
		"targets=\"x\"; /usr/local/go/bin/go test -skip=TestA -skip \"TestC\" ./a ./b\n" + // the last -skip wins
		"go test ./a -v ./b -skip TestD\n" + // ./b, after the package list, goes to the test binary
		"go test -count 1 \\\n  -run X\n" + // a continued line; no pattern: the root package
		"bin/trace gotest -o f ./pkg -skip TestE -- -test.v\n" + // gotest's own flags end at the first other argument
		// The gotest form is known by its shape, whatever $(TRACE) names the program (the
		// Makefile tests pass TRACE=<stubs>/TRACE), and needs a "--": without one, the
		// words are not go test's.
		"/tmp/trace-wrapper gotest -o .out/x.json -- -skip TestC ./c\n" +
		"/tmp/stubs/TRACE gotest -o .out/x.json -- -skip TestStubs ./d\n" +
		"bin/trace gotest -o f ./pkg -skip TestNoDashes\n" +
		"go test -skip TestF ./a -args -test.skip TestG -test.v\n" + // the binary's -test.skip wins
		"go test -skip TestH ./a -- -test.skip TestI\n" + // after --, the binary reads no flags
		// Not commands: a recipe comment, and go test or trace gotest as an argument.
		"# quick runs leave slow tests out: go test -skip TestJ ./...\n" +
		"echo go test -skip TestK ./...; echo bin/trace gotest -o f -- -skip TestL ./...\n" +
		// Nor is the gotest form when its program never runs its arguments: echo, printf, :,
		// true and false, by any path and after any prefix.
		"echo gotest -o f -- -skip TestX ./...\n" +
		": gotest -o f -- -skip TestY ./...\n" +
		"printf gotest -o f -- -skip TestZ ./...; true gotest -o f -- -skip TestTrue ./...\n" +
		"if :; then env /bin/false gotest -o f -- -skip TestFalse ./...; fi\n" +
		"bin/trace procs\n" +
		// A comment ends the command, and a # inside a word is part of it.
		"go test -skip TestM ./a # then -skip TestN\n" +
		"go test -skip Test#O ./b\n" +
		// The command word comes after assignments and env, time and nice prefixes.
		"CGO_ENABLED=0 env -u GOFLAGS GOTOOLCHAIN=local nice -n 5 time -p /usr/local/go/bin/go test -skip TestP ./a\n" +
		// A command also starts after the reserved words if, then, elif, else, while,
		// until, do, { and !, and after each separator: ;, &&, ||, |, &, (, ) and a newline.
		"if [ -n \"$X\" ]; then go test -skip TestThen ./a; fi\n" +
		"for p in a b; do go test -skip TestDo ./$p; done\n" + // $p is not expanded
		"if go test -skip TestIf ./a; then :; elif ! go test -skip TestElif ./b; then :; else { go test -skip TestElse ./a; }; fi\n" +
		"while go test -skip TestWhile ./b; do :; done; until go test -skip TestUntil ./a; do :; done\n" +
		"true && go test -skip TestAnd ./b || go test -skip TestOr ./a | go test -skip TestPipe ./b & (go test -skip TestParen ./a)\n" +
		"if true; then CGO_ENABLED=0 bin/trace gotest -o f -- -skip TestThenGotest ./b; fi\n" +
		// Not commands: the words after for and in, and a reserved word as an argument.
		"for t in go test -skip TestFor; do :; done; echo then go test -skip TestEcho ./...\n"
	got := goTestCommands(recipe)
	want := []goTestCmd{
		{pkgs: []string{"./..."}, skip: "TestA|TestB/x"},
		{pkgs: []string{"./internal/scripts/..."}},
		{pkgs: []string{"./a", "./b"}, skip: "TestC"},
		{pkgs: []string{"./a"}},
		{pkgs: []string{"."}},
		{pkgs: []string{"./pkg"}, skip: "TestE"},
		{pkgs: []string{"./c"}, skip: "TestC"},
		{pkgs: []string{"./d"}, skip: "TestStubs"},
		{pkgs: []string{"./a"}, skip: "TestG"},
		{pkgs: []string{"./a"}, skip: "TestH"},
		{pkgs: []string{"./a"}, skip: "TestM"},
		{pkgs: []string{"./b"}, skip: "Test#O"},
		{pkgs: []string{"./a"}, skip: "TestP"},
		{pkgs: []string{"./a"}, skip: "TestThen"},
		{pkgs: []string{"./$p"}, skip: "TestDo"},
		{pkgs: []string{"./a"}, skip: "TestIf"},
		{pkgs: []string{"./b"}, skip: "TestElif"},
		{pkgs: []string{"./a"}, skip: "TestElse"},
		{pkgs: []string{"./b"}, skip: "TestWhile"},
		{pkgs: []string{"./a"}, skip: "TestUntil"},
		{pkgs: []string{"./b"}, skip: "TestAnd"},
		{pkgs: []string{"./a"}, skip: "TestOr"},
		{pkgs: []string{"./b"}, skip: "TestPipe"},
		{pkgs: []string{"./a"}, skip: "TestParen"},
		{pkgs: []string{"./b"}, skip: "TestThenGotest"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("goTestCommands:\n got %v\nwant %v", got, want)
	}
}

// TestGoPatternMatch: which in-module packages a recipe's package pattern names, as go
// help packages says.
func TestGoPatternMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, pkg string
		want         bool
	}{
		{"./...", "ratelimiter", true},
		{"./...", "ratelimiter/pkg/ran", true},
		{"./pkg/...", "ratelimiter/pkg", true},
		{"./pkg/...", "ratelimiter/pkg/ran", true},
		{"./pkg/...", "ratelimiter/pkgs", false},
		{"./pkg/unrelated", "ratelimiter/pkg/ran", false},
		{"./pkg/ran", "ratelimiter/pkg/ran", true},
		{"./pkg/ran/", "ratelimiter/pkg/ran", true},
		{"./pkg/ran", "ratelimiter/pkg/ran/sub", false},
		{".", "ratelimiter", true},
		{".", "ratelimiter/pkg", false},
		{"ratelimiter/pkg/...", "ratelimiter/pkg/ran", true},
		{"ratelimiter/pkg/ran", "ratelimiter/pkg/ran", true},
		{"./internal/.../conformance", "ratelimiter/internal/testutil/conformance", true},
		{"all", "ratelimiter/pkg/ran", true},
		{"std", "ratelimiter/pkg/ran", false},
		{"../other/...", "ratelimiter/pkg/ran", false},
		{"./pkg/../pkg/ran", "ratelimiter/pkg/ran", true},
	} {
		if got := goPatternMatch("ratelimiter", c.pattern, c.pkg); got != c.want {
			t.Errorf("goPatternMatch(%q, %q) = %v, want %v", c.pattern, c.pkg, got, c.want)
		}
	}
}

func TestTargets(t *testing.T) {
	for _, c := range []struct {
		upto, want string
	}{
		{"S0", "test-unit"},
		{"S1", "test-unit"},
		{"S3", "test-unit"},
		{"S4", "test-unit test-store-quick test-scripts"},
		{"S5", "test-unit test-store-quick test-scripts test-integration test-py"},
		{"D1", "test-unit test-store-quick test-scripts test-integration test-py test-e2e test-report"},
	} {
		code, stdout, stderr := runIn(t, t.TempDir(), "targets", "--upto", c.upto)
		if code != 0 || stdout != c.want+"\n" {
			t.Errorf("targets --upto %s: exit %d, %q, want %q\n%s", c.upto, code, stdout, c.want, stderr)
		}
	}
	counts := map[string]int{"S0": 1, "S1": 1, "S5": 5, "D1": 7}
	for id, n := range counts {
		ts, err := targetsUpTo(id)
		if err != nil || len(ts) != n {
			t.Errorf("targetsUpTo(%s) = %v, %v, want %d targets", id, ts, err, n)
		}
	}
	if code, _, _ := runIn(t, t.TempDir(), "targets", "--upto", "S10"); code != 2 {
		t.Errorf("an unknown slice gave exit %d, want 2", code)
	}
}
