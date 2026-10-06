package store

import "testing"

// TestPositiveBroken fails on the unmutated code, so no mutant of this package can be judged.
func TestPositiveBroken(t *testing.T) {
	if got := Positive(1); got {
		t.Errorf("Positive(1) = %v, want false (this fixture's test is wrong on purpose)", got)
	}
}
