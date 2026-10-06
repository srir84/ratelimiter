//go:build rltest

package core

import "testing"

// TestDiffer builds only with -tags rltest: the sweep's second run of this package, the only
// one that kills the != flip.
func TestDiffer(t *testing.T) {
	if !Differ(1, 2) || Differ(3, 3) {
		t.Errorf("Differ(1, 2), Differ(3, 3) = %v, %v, want true, false", Differ(1, 2), Differ(3, 3))
	}
}
