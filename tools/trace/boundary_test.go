package main

import (
	"strings"
	"testing"
)

func boundaryProblems(t *testing.T, fixtureName string) []string {
	t.Helper()
	r, err := checkBoundary(t.Context(), testEnv(testdataDir(t, fixtureName)), nil)
	if err != nil {
		t.Fatalf("boundary: %v", err)
	}
	return r.problems
}

func TestBoundary_Direct(t *testing.T) {
	problems := boundaryProblems(t, "boundary/direct")
	want := []string{
		"ratelimiter/internal/x -> ratelimiter/cmd/hitgen/plan:",
		"ratelimiter/limiter -> ratelimiter/harness/driver:",
		"ratelimiter/limitertest -> ratelimiter/harness/fixtures:", // limitertest is product code too
	}
	if len(problems) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(problems), len(want), strings.Join(problems, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(problems[i], w) {
			t.Errorf("problem %d = %q, want the chain %q", i, problems[i], w)
		}
	}
	// The harness importing the product, and a harness binary importing the harness, pass.
	if countContaining(problems, "ratelimiter/harness/driver ->") > 0 || countContaining(problems, "cmd/hitapi ->") > 0 {
		t.Errorf("harness code was flagged:\n%s", strings.Join(problems, "\n"))
	}
}

func TestBoundary_Transitive(t *testing.T) {
	problems := boundaryProblems(t, "boundary/transitive")
	chains := []string{
		"ratelimiter/cmd/rladmin -> ratelimiter/internal/helper -> ratelimiter/harness/runlog:",
		"ratelimiter/internal/core -> ratelimiter/internal/helper -> ratelimiter/harness/runlog:",
		"ratelimiter/internal/helper -> ratelimiter/harness/runlog:",
		"ratelimiter/limiter -> ratelimiter/internal/core -> ratelimiter/internal/helper -> ratelimiter/harness/runlog:",
	}
	if len(problems) != len(chains) {
		t.Fatalf("got %d problems, want %d:\n%s", len(problems), len(chains), strings.Join(problems, "\n"))
	}
	for i, c := range chains {
		if !strings.HasPrefix(problems[i], c) {
			t.Errorf("problem %d = %q, want the chain %q", i, problems[i], c)
		}
	}
	if !strings.Contains(problems[3], "internal/helper/helper.go imports ratelimiter/harness/runlog") {
		t.Errorf("the chain does not name the importing file: %q", problems[3])
	}
}

func TestBoundary_TaggedFile(t *testing.T) {
	problems := boundaryProblems(t, "boundary/tagged")
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "ratelimiter/internal/core -> ratelimiter/harness/driver:") ||
		!strings.Contains(problems[0], "internal/core/hooks_rltest.go") {
		t.Fatalf("an rltest-only file importing the harness was not caught:\n%s", strings.Join(problems, "\n"))
	}
}

func TestBoundary_IgnoresTests(t *testing.T) {
	if problems := boundaryProblems(t, "boundary/tests"); len(problems) > 0 {
		t.Fatalf("_test.go imports were flagged:\n%s", strings.Join(problems, "\n"))
	}
	// The command path: exit 0 and a summary.
	code, stdout, stderr := runIn(t, testdataDir(t, "boundary/tests"), "boundary")
	if code != 0 || !strings.HasPrefix(stdout, "boundary: ") {
		t.Fatalf("trace boundary: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

// TestBoundary_SkippedDirs: the walk reads nested runs/ and bin/ directories (only the
// root's are skipped), and a non-test import of an in-module package the walk skips
// fails, whoever makes it, with its chain: a dot or underscore directory, a testdata/
// package, or the root's runs/ or bin/. Go builds every one of these imports.
func TestBoundary_SkippedDirs(t *testing.T) {
	root := fixture(t, "boundary/skipped")
	// git ignores every bin/ directory, so the test writes these two packages.
	writeTestFile(t, root, "internal/x/bin/bin.go", "package bin\n\nimport _ \"ratelimiter/harness/runlog\"\n")
	writeTestFile(t, root, "bin/tool/tool.go", "package tool\n\nimport _ \"ratelimiter/harness/runlog\"\n")
	r, err := checkBoundary(t.Context(), testEnv(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	const unread = ", which sits in a directory the walk skips"
	want := []string{
		"ratelimiter/cmd/rladmin -> ratelimiter/bin/tool: cmd/rladmin/main.go imports ratelimiter/bin/tool" + unread,
		"ratelimiter/cmd/rladmin -> ratelimiter/internal/x -> ratelimiter/internal/x/bin -> ratelimiter/harness/runlog: product code never imports the harness (DESIGN §A1); internal/x/bin/bin.go imports",
		"ratelimiter/harness/driver -> ratelimiter/_shared: harness/driver/driver.go imports ratelimiter/_shared" + unread,
		"ratelimiter/internal/core -> ratelimiter/internal/core/testdata/sneak: internal/core/core.go imports ratelimiter/internal/core/testdata/sneak" + unread,
		"ratelimiter/internal/runs -> ratelimiter/harness/runlog: product code never imports the harness (DESIGN §A1); internal/runs/runs.go imports",
		"ratelimiter/internal/x -> ratelimiter/internal/x/bin -> ratelimiter/harness/runlog: product code",
		"ratelimiter/internal/x/bin -> ratelimiter/harness/runlog: product code",
		"ratelimiter/limiter -> ratelimiter/.helper: limiter/limiter.go imports ratelimiter/.helper" + unread,
		"ratelimiter/limiter -> ratelimiter/runs/r: limiter/limiter.go imports ratelimiter/runs/r" + unread,
	}
	if len(r.problems) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(r.problems), len(want), strings.Join(r.problems, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(r.problems[i], w) {
			t.Errorf("problem %d = %q, want it to start %q", i, r.problems[i], w)
		}
	}
}
