package core

import (
	"errors"
	"testing"
)

// TestCheck never tries 0, so flipping n < 0 to n <= 0 survives.
func TestCheck(t *testing.T) {
	if err := Check(-1); !errors.Is(err, ErrNegative) {
		t.Errorf("Check(-1) = %v, want ErrNegative", err)
	}
	if err := Check(1); err != nil {
		t.Errorf("Check(1) = %v, want nil", err)
	}
}

// TestPick: a valid index first, fatal, so the negated condition stops the test there; then the
// first index past the end, which the >= flip turns into an index panic.
func TestPick(t *testing.T) {
	xs := []int{1, 2, 3}
	if v, err := Pick(xs, 1); err != nil || v != 2 {
		t.Fatalf("Pick(xs, 1) = %d, %v, want 2, nil", v, err)
	}
	if _, err := Pick(xs, 3); !errors.Is(err, ErrRange) {
		t.Errorf("Pick(xs, 3) = %v, want ErrRange", err)
	}
}

// TestSmall reaches the edge of n <= 10, so its flip is killed.
func TestSmall(t *testing.T) {
	if !Small(10) || Small(11) {
		t.Errorf("Small(10), Small(11) = %v, %v, want true, false", Small(10), Small(11))
	}
}

// TestBig never tries 100, so flipping n > 100 to n >= 100 survives.
func TestBig(t *testing.T) {
	if !Big(101) || Big(50) {
		t.Errorf("Big(101), Big(50) = %v, %v, want true, false", Big(101), Big(50))
	}
}

// TestSame kills the == flip.
func TestSame(t *testing.T) {
	if !Same(2, 2) || Same(2, 3) {
		t.Errorf("Same(2, 2), Same(2, 3) = %v, %v, want true, false", Same(2, 2), Same(2, 3))
	}
}
