package main

import (
	"go/build/constraint"
	"strings"
	"testing"
)

func TestTags(t *testing.T) {
	r, err := checkTags(t.Context(), testEnv(testdataDir(t, "tags/fail")), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"cmd/faultctl/main_stub.go: a *_stub.go file needs a //go:build line that excludes rltest",
		"cmd/orchestrator/clock_rltest.go: needs a //go:build line that requires rltest",
		"harness/driver/driver.go: needs a //go:build line that requires rltest",
		"harness/spectest/either_test.go: needs a //go:build line that requires rltest",
		// Any *_rltest*.go name (AGENTS.md §2), not only the two suffixes.
		"internal/core/hooks_rltest_impl.go: needs a //go:build line that requires rltest",
		// A build line go/build ignores, inside /* */. (gofmt moves a //go:build line
		// comment from anywhere into the header, so the cases after the package clause
		// are in the table below rather than in gofmt-clean fixture files.)
		"harness/e2e/e2e.go: needs a //go:build line that requires rltest",
		// A *_rltest_test.go file, and a plain file in each other rltest-only tree.
		"internal/core/acquire_rltest_test.go: needs a //go:build line that requires rltest",
		"limitertest/limitertest.go: needs a //go:build line that requires rltest",
		"harness/fixtures/fixtures.go: needs a //go:build line that requires rltest",
		"harness/pgtest/pgtest.go: needs a //go:build line that requires rltest",
	} {
		if countContaining(r.problems, want) != 1 {
			t.Errorf("want one problem %q", want)
		}
	}
	if len(r.problems) != 10 {
		t.Fatalf("problems:\n%s", strings.Join(r.problems, "\n"))
	}
	r, err = checkTags(t.Context(), testEnv(testdataDir(t, "tags/pass")), nil)
	if err != nil || len(r.problems) > 0 {
		t.Fatalf("correct files: %v\n%s", err, strings.Join(r.problems, "\n"))
	}
	// The header scan, case by case as go list (go1.27.1) builds each file: "" means the
	// file has no build line and is always built.
	for _, c := range []struct{ name, src, want string }{
		{"leading line comments", "// doc\n\n//go:build ignore\n\npackage x\n", "//go:build ignore"},
		{"indented", "  //go:build ignore\n\npackage x\n", "//go:build ignore"},
		{"after a closed block comment", "/* block */\n//go:build ignore\n\npackage x\n", "//go:build ignore"},
		{"inside a block comment", "/*\n//go:build ignore\n*/\n\npackage x\n", ""},
		{"after */ on the same line", "/* a\nb */ //go:build ignore\n\npackage x\n", ""},
		{"after the package clause and a tab", "package\tx\n\n//go:build ignore\n", ""},
		{"after the package clause", "package x\n\n//go:build ignore\n", ""},
		{"go:buildx is not a build line", "//go:buildx ignore\n\npackage x\n", ""},
	} {
		got, err := goBuildLine(c.src)
		if err != nil || got != c.want {
			t.Errorf("%s: goBuildLine = %q, %v, want %q", c.name, got, err, c.want)
		}
	}
	if _, err := goBuildLine("//go:build a\n//go:build b\n\npackage x\n"); err == nil {
		t.Error("two //go:build lines: want an error, as go/build gives")
	}
	for _, c := range []struct {
		line              string
		requires, exclude bool
	}{
		{"//go:build rltest", true, false},
		{"//go:build rltest && linux", true, false},
		{"//go:build rltest || scripttest", false, false},
		{"//go:build !rltest", false, true},
		{"//go:build !rltest && !race", false, true},
		{"//go:build linux", false, false},
	} {
		x, err := constraint.Parse(c.line)
		if err != nil {
			t.Fatal(err)
		}
		if got := !satisfiable(x, false); got != c.requires {
			t.Errorf("%s requires rltest = %v, want %v", c.line, got, c.requires)
		}
		if got := !satisfiable(x, true); got != c.exclude {
			t.Errorf("%s excludes rltest = %v, want %v", c.line, got, c.exclude)
		}
	}
}
