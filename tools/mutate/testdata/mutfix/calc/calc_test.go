package calc

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Errorf("Add(2, 3) = %d, want 5", got)
	}
}

func TestAdd_Small(t *testing.T) {
	if got := Add(0, 1); got != 1 {
		t.Errorf("Add(0, 1) = %d, want 1", got)
	}
}

func TestAdd_Large(t *testing.T) {
	if got := Add(1000, 1000); got != 2000 {
		t.Errorf("Add(1000, 1000) = %d, want 2000", got)
	}
}

func TestAt(t *testing.T) {
	if got := At([]int{1, 2, 3}, 1); got != 2 {
		t.Errorf("At(…, 1) = %d, want 2", got)
	}
}

func TestPause(t *testing.T) {
	if got := Pause(); got != 1 {
		t.Errorf("Pause() = %d, want 1", got)
	}
}

// TestBroken fails on the unmutated code, so no mutant can be judged by it.
func TestBroken(t *testing.T) {
	if got := Add(1, 1); got != 3 {
		t.Errorf("Add(1, 1) = %d, want 3", got)
	}
}
