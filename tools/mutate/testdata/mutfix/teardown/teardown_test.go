package teardown

import (
	"os"
	"testing"
)

// TestMain fails the run, after every test passed, when Clean reports a leftover.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && !Clean() {
		code = 1
	}
	os.Exit(code)
}

func TestTeardownOne(t *testing.T) {
	if got := One(); got != 1 {
		t.Errorf("One() = %d, want 1", got)
	}
}
