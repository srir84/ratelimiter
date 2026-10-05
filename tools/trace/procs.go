package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procsDir holds one record per child process a test helper started (§C7).
const procsDir = ".out/procs"

// checkProcs is `trace procs` (§C7): storeproc and the harness/e2e helpers record each
// child they start as .out/procs/<pid> (content: the command) and delete the record
// once the child has exited. Every recorded PID still alive is killed (SIGKILL) and
// listed, which fails the check; records of dead PIDs are deleted. A PID whose process
// started after its record was written was reused: the record goes, nothing is killed.
// Dotfiles (Finder's .DS_Store) are ignored; any other name is not a record and fails.
// PIDs only, never process names: Valkey rewrites its title, and the owner's own stores
// must never match.
func checkProcs(ctx context.Context, e *env, args []string) (report, error) {
	if err := parseFlags(flag.NewFlagSet("procs", flag.ContinueOnError), e, args); err != nil {
		return report{}, err
	}
	if err := ctx.Err(); err != nil {
		return report{}, fmt.Errorf("reading %s: %w", procsDir, err)
	}
	dir := filepath.Join(e.root, filepath.FromSlash(procsDir))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return report{out: []string{"procs: no " + procsDir + " records"}}, nil
	}
	if err != nil {
		return report{}, fmt.Errorf("reading %s: %w", procsDir, err)
	}
	var r report
	cleared := 0
	for _, ent := range entries {
		if err := ctx.Err(); err != nil {
			return r, fmt.Errorf("stopped before %s/%s: %w", procsDir, ent.Name(), err)
		}
		if strings.HasPrefix(ent.Name(), ".") {
			continue // a dotfile, such as Finder's .DS_Store, is no record
		}
		rec := filepath.Join(dir, ent.Name())
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 || ent.IsDir() {
			r.problems = append(r.problems, fmt.Sprintf("%s/%s: not a PID record", procsDir, ent.Name()))
			continue
		}
		info, err := ent.Info()
		if err != nil {
			return r, fmt.Errorf("reading %s/%s: %w", procsDir, ent.Name(), err)
		}
		command, err := os.ReadFile(rec)
		if err != nil {
			return r, fmt.Errorf("reading %s/%s: %w", procsDir, ent.Name(), err)
		}
		if alive(pid) {
			started, err := processStart(ctx, pid)
			switch {
			case err != nil && alive(pid):
				return r, err
			case err == nil && started.After(info.ModTime().Truncate(time.Second)):
				// Reused PID: another process started after the record was written.
			case err == nil:
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					return r, fmt.Errorf("killing leftover process %d: %w", pid, err)
				}
				r.problems = append(r.problems, fmt.Sprintf("killed leftover child process %d (%s), recorded in %s/%d: its starter never stopped it",
					pid, strings.TrimSpace(string(command)), procsDir, pid))
			}
		}
		if err := os.Remove(rec); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return r, fmt.Errorf("removing %s/%d: %w", procsDir, pid, err)
		}
		cleared++
	}
	if len(r.problems) == 0 {
		r.out = []string{fmt.Sprintf("procs: no recorded child process alive; %d stale record(s) removed", cleared)}
	}
	return r, nil
}

// alive reports whether a process with this PID exists (a zombie counts).
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processStart reads a process's start time, to the second, with ps -o lstart=.
func processStart(ctx context.Context, pid int) (time.Time, error) {
	bin, err := exec.LookPath("ps")
	if err != nil {
		return time.Time{}, fmt.Errorf("ps is not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = []string{"LC_ALL=C", "TZ=UTC"} // fixed names and zone, so the time parses as UTC
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("ps -o lstart= -p %d: %w", pid, err)
	}
	s := strings.Join(strings.Fields(string(out)), " ")
	t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("start time of process %d: %w", pid, err)
	}
	return t, nil
}
