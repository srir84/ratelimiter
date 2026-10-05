package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startSleep starts a sleep child that the test reaps at cleanup at the latest.
func startSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// endSignal reaps cmd and returns the signal that ended it; the test fails if it exited.
// Unlike kill(pid, 0), which succeeds on a killed child not yet reaped, the wait status
// tells which signal ended the process.
func endSignal(t *testing.T, cmd *exec.Cmd) syscall.Signal {
	t.Helper()
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("sleep did not end by a signal: %v", err)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		t.Fatalf("sleep did not end by a signal: %v", exitErr)
	}
	return ws.Signal()
}

func writeRecord(t *testing.T, root string, pid int) string {
	t.Helper()
	rec := filepath.Join(root, ".out", "procs", strconv.Itoa(pid))
	writeTestFile(t, root, ".out/procs/"+strconv.Itoa(pid), "sleep 60\n")
	return rec
}

func TestProcs(t *testing.T) {
	t.Run("no records", func(t *testing.T) {
		if code, stdout, stderr := runIn(t, t.TempDir(), "procs"); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
		}
	})
	t.Run("dotfiles are ignored, other names fail", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, ".out/procs/.DS_Store", "Finder metadata")
		if code, stdout, stderr := runIn(t, root, "procs"); code != 0 {
			t.Fatalf("a .DS_Store: exit %d\n%s%s", code, stdout, stderr)
		}
		writeTestFile(t, root, ".out/procs/notapid", "sleep 60\n")
		code, _, stderr := runIn(t, root, "procs")
		if code != 1 || !strings.Contains(stderr, "procs: .out/procs/notapid: not a PID record") || strings.Contains(stderr, ".DS_Store") {
			t.Fatalf("a non-numeric record: exit %d, want 1 naming it\n%s", code, stderr)
		}
	})
	t.Run("live child is killed", func(t *testing.T) {
		root := t.TempDir()
		cmd := startSleep(t)
		rec := writeRecord(t, root, cmd.Process.Pid)
		code, _, stderr := runIn(t, root, "procs")
		if code != 1 || !strings.Contains(stderr, "killed leftover child process "+strconv.Itoa(cmd.Process.Pid)+" (sleep 60)") {
			t.Fatalf("exit %d, want 1 listing the PID\n%s", code, stderr)
		}
		if sig := endSignal(t, cmd); sig != syscall.SIGKILL {
			t.Fatalf("sleep ended by %v, want SIGKILL", sig)
		}
		if _, err := os.Stat(rec); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the record is still there: %v", err)
		}
	})
	t.Run("dead PID record is removed", func(t *testing.T) {
		root := t.TempDir()
		cmd := exec.CommandContext(t.Context(), "true")
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		rec := writeRecord(t, root, cmd.Process.Pid)
		if code, _, stderr := runIn(t, root, "procs"); code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		if _, err := os.Stat(rec); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the record is still there: %v", err)
		}
	})
	t.Run("reused PID is left alone", func(t *testing.T) {
		root := t.TempDir()
		cmd := startSleep(t)
		rec := writeRecord(t, root, cmd.Process.Pid)
		old := time.Now().Add(-time.Hour) // the record predates the process: the PID was reused
		if err := os.Chtimes(rec, old, old); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := runIn(t, root, "procs"); code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		if _, err := os.Stat(rec); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the record is still there: %v", err)
		}
		// Still running: our own SIGTERM ends it. Had procs killed it, the wait status
		// would show SIGKILL, even while the child was not yet reaped.
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("signalling sleep: %v", err)
		}
		if sig := endSignal(t, cmd); sig != syscall.SIGTERM {
			t.Fatalf("sleep ended by %v, want the test's SIGTERM: procs killed a reused PID's process", sig)
		}
	})
}
