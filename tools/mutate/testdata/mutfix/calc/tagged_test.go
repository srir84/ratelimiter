//go:build rltest

package calc

import "testing"

// TestTagged builds only with -tags rltest, which bin/mutate reads from this file.
func TestTagged(t *testing.T) {
	if got := Add(2, 2); got != 4 {
		t.Errorf("Add(2, 2) = %d, want 4", got)
	}
}
