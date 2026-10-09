//go:build rltest

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
)

// TestPIDFile_WriteAndRemove (plan H3 steps 1 and 4): the pid file names the orchestrator
// (mode 0600, its directory created when missing); a stale file is replaced and a live one is
// not (errRunning); removal takes only a file that still names this process; a missing file,
// garbage, PID 0, a number above math.MaxInt32 (kill(2)'s pid_t is 32 bits: 2^32+1 would check
// PID 1, 2^32-1 every process, 2^32 the caller's group) and a dead PID are stale; PID 1 is
// live. Each taken file's lock is dropped before the next case.
func TestPIDFile_WriteAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".data", "orchestrator.pid")
	const me = 4242
	lock, err := takePIDFile(path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLock(t, lock)
	if data, err := os.ReadFile(path); err != nil || string(data) != "4242\n" {
		t.Fatalf("pid file %q (%v), want \"4242\\n\"", data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("pid file stat %v, %v; want mode 600", info, err)
	}
	if err := removePIDFile(path, me+1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("removePIDFile for another PID removed the file (stat: %v)", err)
	}
	if err := removePIDFile(path, me); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("removePIDFile left the file (stat: %v)", err)
	}
	if err := removePIDFile(path, me); err != nil {
		t.Errorf("removePIDFile of a missing file: %v", err)
	}

	for _, content := range []string{"garbage\n", "0\n", "-5\n", strconv.Itoa(deadPID(t)) + "\n", "",
		"4294967297\n", "4294967295\n", "4294967296\n"} {
		writeFile(t, path, content, 0o600)
		if pid, live, err := livePIDFile(path); live || err != nil {
			t.Errorf("pid file %q: livePIDFile = %d, %v, %v; want stale", content, pid, live, err)
		}
		lock, err := takePIDFile(path, me)
		if err != nil {
			t.Errorf("pid file %q: takePIDFile: %v, want the stale file replaced", content, err)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "4242\n" {
			t.Errorf("pid file %q replaced by %q (%v), want \"4242\\n\"", content, data, err)
		}
		if lock != nil {
			closeLock(t, lock)
		}
	}

	live := strconv.Itoa(os.Getpid()) + "\n"
	writeFile(t, path, live, 0o600)
	if pid, ok, err := livePIDFile(path); !ok || err != nil || pid != os.Getpid() {
		t.Errorf("livePIDFile of this process's PID = %d, %v, %v; want %d, true, nil", pid, ok, err, os.Getpid())
	}
	if lock, err := takePIDFile(path, me); err == nil {
		closeLock(t, lock)
		t.Error("takePIDFile over a live process's pid file: no error")
	} else if !errors.Is(err, errRunning) {
		t.Errorf("takePIDFile over a live process's pid file: %v, want errRunning", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != live {
		t.Errorf("a live process's pid file became %q (%v), want %q", data, err, live)
	}
	// PID 1 (launchd, or init) always runs. For any user but root, kill(1, 0) fails with EPERM,
	// which still means a live process.
	writeFile(t, path, "1\n", 0o600)
	if pid, ok, err := livePIDFile(path); !ok || err != nil || pid != 1 {
		t.Errorf("livePIDFile of PID 1 = %d, %v, %v; want 1, true, nil", pid, ok, err)
	}
	if _, live, err := livePIDFile(filepath.Join(t.TempDir(), "absent.pid")); live || err != nil {
		t.Errorf("livePIDFile of a missing file = %v, %v; want false, nil", live, err)
	}
}

// closeLock drops a pid file lock without removing the file.
func closeLock(t *testing.T, l *pidLock) {
	t.Helper()
	if err := l.f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Error(err)
	}
}

// flockHeld opens the file at path and takes its exclusive flock, as a run does from before
// it writes its PID until it ends, and holds it until the test ends.
func flockHeld(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

// TestPIDFile_HeldByStartingRun (IMPLEMENTATION_PLAN H3 steps 1 and 4): a run holds the pid
// file's lock from before it writes its PID, so another run that finds the file still empty,
// which the stale rules alone judge stale, gets errRunning (orchestrator_running) and leaves
// it as it is: the same file, still empty, still locked.
func TestPIDFile_HeldByStartingRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".data", "orchestrator.pid")
	writeFile(t, path, "", 0o600)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	flockHeld(t, path)
	if _, live, err := livePIDFile(path); live || err != nil {
		t.Fatalf("an empty pid file: livePIDFile = %v, %v; want stale by content", live, err)
	}
	lock, err := takePIDFile(path, 4242)
	if err == nil {
		closeLock(t, lock)
		t.Fatal("takePIDFile took the pid file another run holds, still empty: two runs own it")
	}
	if !errors.Is(err, errRunning) {
		t.Errorf("takePIDFile over a held pid file: %v, want errRunning", err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || after.Size() != 0 {
		t.Errorf("the held pid file after the refused take: %v (stat %v), want the same empty file", after, err)
	}
}

// TestPIDFile_ConcurrentTakes (IMPLEMENTATION_PLAN H3 step 4): two runs that take the pid file
// at once, both naming a live process (this one), never both get it: in each of 200 rounds,
// on a fresh path, exactly one takes it and the other gets errRunning; the file names the
// winner, and its release removes it.
func TestPIDFile_ConcurrentTakes(t *testing.T) {
	for round := range 200 {
		path := filepath.Join(t.TempDir(), "orchestrator.pid")
		locks := make([]*pidLock, 2)
		errs := make([]error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				locks[i], errs[i] = takePIDFile(path, os.Getpid())
			}()
		}
		close(start)
		wg.Wait()
		won := 0
		for i := range 2 {
			switch {
			case errs[i] == nil:
				won++
			case !errors.Is(errs[i], errRunning):
				t.Errorf("round %d: take %d: %v, want the pid file or errRunning", round, i+1, errs[i])
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d takes got the pid file (errors %v), want exactly 1", round, won, errs)
		}
		for _, l := range locks {
			if l == nil {
				continue
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != strconv.Itoa(os.Getpid())+"\n" {
				t.Errorf("round %d: the pid file holds %q (%v), want this process's PID", round, data, err)
			}
			if err := l.release(); err != nil {
				t.Errorf("round %d: release: %v", round, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("round %d: the pid file after its release: stat %v, want it removed", round, err)
			}
		}
	}
}
