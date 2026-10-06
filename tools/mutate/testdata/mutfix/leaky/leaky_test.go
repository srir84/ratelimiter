package leaky

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	m.Run()
	os.Exit(3) // the run fails after every test passed
}

func TestOne(t *testing.T) {
	if got := One(); got != 1 {
		t.Errorf("One() = %d, want 1", got)
	}
}
