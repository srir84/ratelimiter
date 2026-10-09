//go:build rltest

package fixtures

import (
	"math"
	"os"
	"testing"
)

// TestCheckPID: checkPID refuses every PID a fault must never signal — 0 and -1 (kill(2) would
// reach the caller's process group, or every process the user owns), 1 (init or launchd), this
// process, and a value a pid_t cannot hold (MaxInt32+1 would wrap) — and accepts 2 and
// MaxInt32 (plan choice, IMPLEMENTATION_PLAN H4 steps 1–2).
func TestCheckPID(t *testing.T) {
	cases := []struct {
		name string
		pid  int
		ok   bool
	}{
		{"zero", 0, false},
		{"one", 1, false},
		{"minus_one", -1, false},
		{"this_process", os.Getpid(), false},
		{"above_max_int32", math.MaxInt32 + 1, false},
		{"two", 2, true},
		{"max_int32", math.MaxInt32, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkPID(c.pid); (err == nil) != c.ok {
				t.Errorf("checkPID(%d) = %v, want accepted %v", c.pid, err, c.ok)
			}
		})
	}
}
