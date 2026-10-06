//go:build conformance

package calc

import "testing"

// TestConf builds only with -tags conformance (L0), which mutants never use.
func TestConf(t *testing.T) {
	if got := Add(1, 1); got != 2 {
		t.Errorf("Add(1, 1) = %d, want 2", got)
	}
}
