package storeproc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// rerun runs this test binary again for the test name alone, with env added to the
// child's environment, and returns its combined output. A test that must panic, or must
// change a process-wide limit, does so in the child.
func rerun(t *testing.T, name, env string) (string, error) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), bin, "-test.run=^"+name+"$", "-test.v", "-test.timeout=1m")
	cmd.Env = append(os.Environ(), env)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSpawnRecordWriteFails: when the PID record cannot be written, spawn ends the child
// and leaves no record, even one the failed write created (plan S1 step 4: the record goes
// on every path). The write fails in a child copy of this test binary, under a zero
// RLIMIT_FSIZE that only that process sees: creating a file still works, writing fails.
func TestSpawnRecordWriteFails(t *testing.T) {
	if os.Getenv("STOREPROC_TEST_FSIZE_CHILD") == "" {
		out, err := rerun(t, "TestSpawnRecordWriteFails", "STOREPROC_TEST_FSIZE_CHILD=1")
		if err != nil || !strings.Contains(out, "--- PASS: TestSpawnRecordWriteFails") {
			t.Fatalf("child test (%v):\n%s", err, out)
		}
		return
	}
	root := t.TempDir()
	s := &Store{Dir: t.TempDir(), Bin: "/bin/sleep", Args: []string{"/bin/sleep", "30"}, tb: t, root: root}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
	zero := lim
	zero.Cur = 0
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &zero); err != nil {
		t.Fatal(err)
	}
	err := s.spawn()
	if rerr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); rerr != nil {
		t.Fatal(rerr)
	}
	if err == nil {
		s.Kill()
		t.Fatal("spawn succeeded although the PID record could not be written")
	}
	if !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("spawn error %v, want the record write's EFBIG", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".out", "procs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("record %s left behind by the failed write", e.Name())
	}
}

// panicky's String method panics. Package fmt documents that it catches a panic from a
// String method, so TestPanicking sees a panic unwind and end without calling recover,
// which only the listener dispatcher may do (AGENTS.md §2).
type panicky struct{ during *bool }

func (p panicky) String() string {
	defer func() { *p.during = panicking() }()
	panic("boom")
}

func TestPanicking(t *testing.T) {
	if panicking() {
		t.Error("panicking() = true in normal code")
	}
	var atReturn bool
	func() {
		defer func() { atReturn = panicking() }()
	}()
	if atReturn {
		t.Error("panicking() = true in a deferred call at a normal return")
	}
	var during bool
	if got := fmt.Sprint(panicky{&during}); got != "%!v(PANIC=String method: boom)" {
		t.Fatalf("fmt.Sprint = %q, want fmt's report of the recovered panic", got)
	}
	if !during {
		t.Error("panicking() = false in a deferred call while a panic unwinds")
	}
	if panicking() {
		t.Error("panicking() = true after the panic was recovered")
	}
}

// TestStopLogsTailOnPanic: the testing package runs a panicking test's cleanups before it
// marks the test failed, and Stop must still log the store's tail then. The panicking
// test runs in a child copy of this test binary; Stop needs only the store directory.
func TestStopLogsTailOnPanic(t *testing.T) {
	if dir := os.Getenv("STOREPROC_TEST_PANIC_DIR"); dir != "" {
		t.Cleanup((&Store{Dir: dir, tb: t}).Stop)
		panic("storeproc test: the test body panics")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "store.log")
	const last = "the store's last log line"
	if err := os.WriteFile(logPath, []byte("an earlier log line\n"+last+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "store.out"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := rerun(t, "TestStopLogsTailOnPanic", "STOREPROC_TEST_PANIC_DIR="+dir)
	if err == nil || !strings.Contains(out, "panic: storeproc test: the test body panics") {
		t.Fatalf("the child test did not panic (%v):\n%s", err, out)
	}
	if !strings.Contains(out, "storeproc: last 2 lines of "+logPath) || !strings.Contains(out, last) {
		t.Fatalf("the panicking test's output has no tail of %s:\n%s", logPath, out)
	}
}
