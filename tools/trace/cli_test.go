package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCheck: trace check runs boundary, ids, acl, lua, suppress, tags, testglobals, oracle
// and api, in that order, and goes on after a failing one. It fails on an orphan test-ID
// function but never checks completeness: IDs with no function pass (§C7).
func TestCheck(t *testing.T) {
	checks := []string{"boundary", "ids", "acl", "lua", "suppress", "tags", "testglobals", "oracle", "api"}
	// Each check prints one summary line, ids even when it fails.
	summaries := func(t *testing.T, stdout string) {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if len(lines) != len(checks) {
			t.Fatalf("%d summary lines, want one per check (%d):\n%s", len(lines), len(checks), stdout)
		}
		for i, c := range checks {
			if !strings.HasPrefix(lines[i], c+": ") {
				t.Errorf("line %d = %q, want the summary of %s", i+1, lines[i], c)
			}
		}
	}

	// T-COR-2, T-KEY-1 and T-HTTP-1 have no function, and no script exists: exit 0.
	root := fixture(t, "ids/docs", "check/root")
	copyFile(t, root, aclTemplate)
	code, stdout, stderr := runIn(t, root, "check")
	if code != 0 || stderr != "" {
		t.Fatalf("IDs with no function: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	summaries(t, stdout)
	if !strings.Contains(stdout, "\nids: 4 IDs defined, 1 test-ID functions\n") {
		t.Errorf("ids did not see the fixture's IDs and its function:\n%s", stdout)
	}

	// An orphan test-ID function: exit 1 with the ids problem, and every check still ran.
	root = fixture(t, "ids/docs", "check/root", "ids/orphan")
	copyFile(t, root, aclTemplate)
	code, stdout, stderr = runIn(t, root, "check")
	want := "ids: a/a_test.go:5: TestT_COR_99 maps to T-COR-99, which SPEC.md §10 and HARNESS.md §8 do not define\n" +
		"ids: FAIL, 1 problem(s)\n"
	if code != 1 || stderr != want {
		t.Fatalf("an orphan: exit %d, want 1\nstderr: %s\nwant:   %s", code, stderr, want)
	}
	summaries(t, stdout)
}

// TestCancelledContext: a done ctx stops every check that walks or reads the tree with
// its error, and trace check runs nothing more once ctx is done (Ctrl-C, SIGTERM).
func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := slicesFixture(t, "slices/passing")
	for _, name := range append(checkNames(), "slices") {
		var args []string
		if name == "slices" {
			args = []string{"--upto", "S0"}
		}
		fn, _ := checkByName(name)
		if _, err := fn(ctx, testEnv(root), args); !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a cancelled context: err = %v, want context.Canceled", name, err)
		}
	}
	var stdout, stderr strings.Builder
	code := dispatch(ctx, &env{root: root, dir: root, stdout: &stdout, stderr: &stderr}, []string{"check"})
	if code != 1 || stdout.String() != "" || stderr.String() != "check: stopped before boundary: context canceled\n" {
		t.Fatalf("trace check with a cancelled context: exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
}
