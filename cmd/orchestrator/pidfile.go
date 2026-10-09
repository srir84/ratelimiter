package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pidFileName is the running orchestrator's pid file, relative to the repository root
// (IMPLEMENTATION_PLAN H3 step 4): taken once preflight passes, removed at every exit.
const pidFileName = ".data/orchestrator.pid"

// maxPIDFileOpens bounds takePIDFile's attempts at a file that another run removes or
// replaces between the open and the lock (plan choice).
const maxPIDFileOpens = 5

// errRunning is takePIDFile's refusal: another run holds the pid file, or the file names a
// live process (preflight's orchestrator_running).
var errRunning = errors.New("another run is in progress")

// livePIDFile reads the pid file at path and reports the PID it names and whether that
// process is alive (plan choice for stale-run detection): alive when the file holds one
// decimal PID from 1 to math.MaxInt32 (kill(2)'s pid_t is 32 bits, so a larger number names
// no process), surrounding white space allowed, and kill(pid, 0) succeeds or fails with EPERM
// (a live process of another user). A missing file, any other content or a dead PID is a
// stale run's leftover, which the next run replaces. Only a read failure is an error.
func livePIDFile(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading the pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 1 || pid > math.MaxInt32 {
		return 0, false, nil
	}
	return pid, alive(pid), nil
}

// alive reports whether a process pid exists: kill(pid, 0) checks without signalling it.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pidLock is the pid file a run holds (IMPLEMENTATION_PLAN H3 step 4): open, under an
// exclusive flock(2) from before the run's PID is written until release, so two runs that
// start at once never both hold it. The kernel drops the lock when the file is closed or the
// process ends, crash included, so a crashed run leaves only a stale file for the next run.
// The file is opened close-on-exec, so no child holds the lock.
type pidLock struct {
	f    *os.File
	path string
	pid  int
}

// takePIDFile takes the pid file at path for pid (mode 0600, its directory created when
// missing) and holds it until release. It opens the file, creating it when missing, and locks
// it without waiting: a lock another run holds, even on a file still empty because that run
// has not written its PID yet, is errRunning. Holding the lock, it applies the stale rules
// (livePIDFile): a file naming a live process other than pid is errRunning, left as it is; a
// stale one is overwritten with pid. A file removed or replaced between the open and the lock
// (a run released it meanwhile) is opened again, at most maxPIDFileOpens times in all.
func takePIDFile(path string, pid int) (*pidLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("taking the pid file: %w", err)
	}
	for range maxPIDFileOpens {
		f, err := lockPIDFile(path)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue // the file locked was no longer the one at path
		}
		l := &pidLock{f: f, path: path, pid: pid}
		if err := l.write(); err != nil {
			if !errors.Is(err, errRunning) {
				_ = os.Remove(path) // a partial file names no run; the write error is reported
			}
			_ = f.Close() // drops the lock
			return nil, err
		}
		return l, nil
	}
	return nil, fmt.Errorf("taking the pid file: %s was removed or replaced %d times while it was being locked", path, maxPIDFileOpens)
}

// lockPIDFile opens the pid file at path, creating it when missing, and locks it without
// waiting. It returns nil, with the file closed again, when the file it locked is no longer
// the one at path.
func lockPIDFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("taking the pid file: %w", err)
	}
	fd := f.Fd()
	if fd > math.MaxInt32 {
		_ = f.Close() // the descriptor cannot be passed to flock(2); that is the error
		return nil, fmt.Errorf("taking the pid file: descriptor %d out of range", fd)
	}
	if err := syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close() // unlocked: nothing was written
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: another orchestrator holds %s", errRunning, path)
		}
		return nil, fmt.Errorf("taking the pid file: locking it: %w", err)
	}
	held, err := f.Stat()
	if err != nil {
		_ = f.Close() // the stat error is reported
		return nil, fmt.Errorf("taking the pid file: %w", err)
	}
	now, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		_ = f.Close() // removed by the run that held it: open again
		return nil, nil
	case err != nil:
		_ = f.Close() // the stat error is reported
		return nil, fmt.Errorf("taking the pid file: %w", err)
	case !os.SameFile(held, now):
		_ = f.Close() // replaced since the open: open again
		return nil, nil
	}
	return f, nil
}

// write applies the stale rules to the locked file and makes it name l.pid: a file naming a
// live process other than l.pid is errRunning, left as it is.
func (l *pidLock) write() error {
	other, live, err := livePIDFile(l.path)
	if err != nil {
		return fmt.Errorf("taking the pid file: %w", err)
	}
	if live && other != l.pid {
		return fmt.Errorf("%w: %s names process %d, which is running (delete the file only if that process is not an orchestrator)",
			errRunning, l.path, other)
	}
	if err := l.f.Chmod(0o600); err != nil {
		return fmt.Errorf("writing the pid file: %w", err)
	}
	if err := l.f.Truncate(0); err != nil {
		return fmt.Errorf("writing the pid file: %w", err)
	}
	if _, err := l.f.WriteAt([]byte(strconv.Itoa(l.pid)+"\n"), 0); err != nil {
		return fmt.Errorf("writing the pid file: %w", err)
	}
	return nil
}

// release removes the pid file if it still names this run's PID (removePIDFile), then closes
// it, which drops the lock. It runs at every exit.
func (l *pidLock) release() error {
	err := removePIDFile(l.path, l.pid)
	return errors.Join(err, l.f.Close())
}

// removePIDFile removes the pid file at path if it still names pid; a file another process
// has taken over since is left alone, and a missing one is not an error.
func removePIDFile(path string, pid int) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the pid file: %w", err)
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(pid) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the pid file: %w", err)
	}
	return nil
}
