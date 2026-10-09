//go:build !rltest

package main

import (
	"bytes"
	"testing"
)

// TestStub_NeedsRltest (IMPLEMENTATION_PLAN H4 step 3): the untagged faultctl prints
// "faultctl needs -tags rltest" and exits 2.
func TestStub_NeedsRltest(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(&stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if got, want := stderr.String(), "faultctl needs -tags rltest\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
}
