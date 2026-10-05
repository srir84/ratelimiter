package gotestflaky

import (
	"runtime"
	"testing"
)

// With -cpu 1,4 every test runs twice, the GOMAXPROCS 1 run first.

// TestCPU fails in the first run and passes in the second.
func TestCPU(t *testing.T) {
	if runtime.GOMAXPROCS(0) == 1 {
		t.Fatal("planted failure under GOMAXPROCS 1")
	}
}

// TestSkipOnce is skipped in the first run and passes in the second.
func TestSkipOnce(t *testing.T) {
	if runtime.GOMAXPROCS(0) == 1 {
		t.Skip("planted skip under GOMAXPROCS 1")
	}
}

// TestSteady passes in both runs.
func TestSteady(t *testing.T) {}
