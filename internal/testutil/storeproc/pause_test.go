package storeproc

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPauseResume (IMPLEMENTATION_PLAN S9 step 4): Pause stops the process (ps state T) and
// Resume continues it; Resume of a store that is not paused and a second Pause are refused,
// and both are refused once the process is gone. Stop on a paused store continues it after
// its SIGTERM, which a stopped process holds, so it ends at once instead of after the 10 s
// grace and its SIGKILL. The process is /bin/sleep: Pause and Resume judge only by the process.
func TestPauseResume(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Bin: "/bin/sleep", Args: []string{"/bin/sleep", "30"}, tb: t, root: t.TempDir()}
	if err := s.spawn(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Kill) // a no-op once Stop has run
	stopped := func(what string) bool {
		t.Helper()
		st, err := processState(t.Context(), s.PID)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return strings.HasPrefix(st, "T")
	}
	if err := s.Resume(); !errors.Is(err, errNotPaused) {
		t.Errorf("Resume before any Pause: %v, want %v", err, errNotPaused)
	}
	if stopped("before Pause") {
		t.Fatal("the process shows stopped before Pause")
	}
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	if !stopped("after Pause") {
		t.Error("the process does not show stopped after Pause")
	}
	if err := s.Pause(); err == nil {
		t.Error("a second Pause succeeded, want it refused")
	}
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if stopped("after Resume") {
		t.Error("the process shows stopped after Resume")
	}
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Stop()
	if d := time.Since(start); d > stopGrace/2 {
		t.Errorf("Stop of a paused store took %v, want it to continue the store so SIGTERM ends it at once (grace %v)", d, stopGrace)
	}
	if err := s.Pause(); err == nil {
		t.Error("Pause of a stopped store succeeded, want it refused")
	}
	if err := s.Resume(); err == nil {
		t.Error("Resume of a stopped store succeeded, want it refused")
	}
}
