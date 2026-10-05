package core

import "testing"

// Read-only package-level tables of expected results pass: no variable here holds a
// Permit's atomic itself, since a Result points at its Permit.
var (
	limitedCases = []struct {
		name string
		want Result
	}{{"limited", Result{Kind: 3, RetryAfterNs: 5}}}
	byName   = map[string]Result{"limited": {Kind: 3, Diag: Diag{Reason: "limited"}}}
	allowed  = Result{Kind: 1}
	degraded = &Result{Kind: 2}
	noPermit *Result
	permits  = []*Permit{nil}
)

func TestResults(t *testing.T) {
	for _, c := range limitedCases {
		if got := byName[c.name]; got.Kind != c.want.Kind || got.Permit != nil {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	if allowed.Kind != 1 || degraded.Kind != 2 || noPermit != nil || len(permits) != 1 {
		t.Error("tables")
	}
}
