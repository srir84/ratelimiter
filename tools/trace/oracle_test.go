package main

import (
	"strings"
	"testing"
)

func TestOracle(t *testing.T) {
	root := testdataDir(t, "oracle")
	r, err := checkOracle(t.Context(), testEnv(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	// In harness/spectest, core.RetryAfterNs(…), c.DecisionNs(…) under a renamed import,
	// and a dot import of internal/core fail, and so does core.DecisionNs(…) under
	// internal/scripts. res.RetryAfterNs, core.Result{RetryAfterNs: 5}, a local named core,
	// and the same call outside the two directories pass.
	sameLines(t, "oracle", problemLines(t, r.problems), wantLines(t, root), r.problems)
	for _, want := range []string{
		"harness/spectest/direct_test.go:10:14: core.RetryAfterNs from ratelimiter/internal/core",
		"harness/spectest/renamed_test.go:10:6: c.DecisionNs from ratelimiter/internal/core",
		"harness/spectest/dot_test.go:6:2: dot import of ratelimiter/internal/core",
		"internal/scripts/scripts_test.go:10:6: core.DecisionNs from ratelimiter/internal/core",
	} {
		if countContaining(r.problems, want) != 1 {
			t.Errorf("want one problem %q:\n%s", want, strings.Join(r.problems, "\n"))
		}
	}
}
