package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"ratelimiter/harness/runlog"
)

// postmasterPIDFile is the file in PostgreSQL's data directory whose first line is the
// postmaster's PID (DESIGN §B1: the harness cluster in .data/pg).
const postmasterPIDFile = "postmaster.pid"

// maxPIDFileSize bounds the part of postmaster.pid that is read, in bytes; its first line
// is a few digits.
const maxPIDFileSize = 4 << 10

// cpuReading is one process's CPU time when it was read, kept until the next tick so its CPU %
// covers the interval between the two readings.
type cpuReading struct {
	created int64     // the kernel's start time of the process (ms): a reused PID is another process
	cpu     float64   // user plus system CPU seconds
	at      time.Time // when it was read (monotonic)
}

// readProcesses samples the processes into line (DESIGN §B7): pids.json's, re-read on every
// tick since the orchestrator rewrites it when hitgen starts (and when a process restarts), and
// the PostgreSQL postmaster with its children. It alone reads and replaces s.cpu: the
// readings it takes become the previous ones for the next tick, and a PID it did not read is
// forgotten, so a process seen for the first time, or a PID reused since, has no CPU % yet.
func (s *sampler) readProcesses(ctx context.Context, line *sample) {
	prev, next := s.cpu, make(map[int32]cpuReading, len(s.cpu))
	pids, err := runlog.ReadPIDs(filepath.Join(s.cfg.runDir, runlog.PIDsFile))
	if err != nil {
		line.ProcsError = s.errText(err)
	} else {
		line.Procs = make([]procSample, 0, len(pids.Procs))
		for _, p := range pids.Procs {
			ps := procSample{Role: p.Role, Instance: p.Instance, PID: p.PID}
			cpu, rss, err := readPID(ctx, p.PID, prev, next)
			if err != nil {
				ps.Error = s.errText(err)
			} else {
				ps.CPU, ps.RSS = round2(cpu), &rss
			}
			line.Procs = append(line.Procs, ps)
		}
	}
	pg, err := readPostgres(ctx, filepath.Join(s.cfg.pgData, postmasterPIDFile), prev, next)
	line.Postgres, line.PostgresError = pg, s.errText(err)
	s.cpu = next
}

// readPID reads the process pid of pids.json: its CPU % since prev's reading of it (nil
// without one) and its RSS; its reading goes into next.
func readPID(ctx context.Context, pid int, prev, next map[int32]cpuReading) (*float64, uint64, error) {
	if pid < 1 || pid > math.MaxInt32 {
		return nil, 0, fmt.Errorf("pid %d: out of range", pid)
	}
	p, err := process.NewProcessWithContext(ctx, int32(pid))
	if err != nil {
		return nil, 0, fmt.Errorf("pid %d: %w", pid, err)
	}
	return readProcess(ctx, p, prev, next)
}

// readProcess reads p with gopsutil: its CPU % since prev's reading of the same process (nil
// without one) and its RSS. Its reading goes into next.
func readProcess(ctx context.Context, p *process.Process, prev, next map[int32]cpuReading) (*float64, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, fmt.Errorf("pid %d: %w", p.Pid, err)
	}
	created, err := p.CreateTimeWithContext(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("pid %d: start time: %w", p.Pid, err)
	}
	times, err := p.TimesWithContext(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("pid %d: CPU times: %w", p.Pid, err)
	}
	mem, err := p.MemoryInfoWithContext(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("pid %d: memory: %w", p.Pid, err)
	}
	if mem.RSS == 0 {
		// On macOS gopsutil does not check its task-info call, and a process that has exited
		// (a zombie) or is not readable comes back as all zeros. A live process is never 0.
		return nil, 0, fmt.Errorf("pid %d: no task info: the process has exited or is not readable", p.Pid)
	}
	cur := cpuReading{created: created, cpu: times.User + times.System, at: time.Now()}
	next[p.Pid] = cur
	old, ok := prev[p.Pid]
	return cpuPercent(old, ok, cur), mem.RSS, nil
}

// cpuPercent is one process's CPU % between two of its readings: CPU seconds over elapsed
// seconds, × 100. It is nil when there is no earlier reading (ok false), when the PID now names
// a process started at another time, or when the readings are not later and not monotonic.
func cpuPercent(old cpuReading, ok bool, cur cpuReading) *float64 {
	if !ok || old.created != cur.created {
		return nil
	}
	elapsed := cur.at.Sub(old.at).Seconds()
	if elapsed <= 0 || cur.cpu < old.cpu {
		return nil
	}
	pct := (cur.cpu - old.cpu) / elapsed * 100
	return &pct
}

// readPostgres samples the postmaster named by the postmaster.pid at path and its children:
// the backends and PostgreSQL's own workers. A child that cannot be read, one that exited
// after the listing, is left out of the sums.
func readPostgres(ctx context.Context, path string, prev, next map[int32]cpuReading) (*pgSample, error) {
	pid, err := postmasterPID(path)
	if err != nil {
		return nil, err
	}
	p, err := process.NewProcessWithContext(ctx, pid)
	if err != nil {
		return nil, fmt.Errorf("the postmaster (pid %d): %w", pid, err)
	}
	cpu, rss, err := readProcess(ctx, p, prev, next)
	if err != nil {
		return nil, fmt.Errorf("the postmaster: %w", err)
	}
	children, err := p.ChildrenWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("the postmaster's children: %w", err)
	}
	pg := &pgSample{PID: int(pid), Processes: 1, RSS: rss}
	for _, c := range children {
		ccpu, crss, err := readProcess(ctx, c, prev, next)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, fmt.Errorf("the postmaster's children: %w", cerr) // the sums would fall short
			}
			continue // a child that exited after the listing
		}
		pg.Processes++
		pg.RSS += crss
		if cpu != nil && ccpu != nil {
			*cpu += *ccpu
		}
	}
	pg.CPU = round2(cpu)
	return pg, nil
}

// postmasterPID reads the postmaster's PID from the first line of postmaster.pid, a regular
// file (readRegular).
func postmasterPID(path string) (int32, error) {
	data, err := readRegular(path, maxPIDFileSize)
	if err != nil {
		return 0, fmt.Errorf("reading the postmaster PID: %w", err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	n, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		var ne *strconv.NumError
		if errors.As(err, &ne) {
			err = ne.Err // without the whole line, which the message quotes shortened
		}
		return 0, fmt.Errorf("reading the postmaster PID: %s: first line %.40q: %w", path, first, err)
	}
	if n < 1 || n > math.MaxInt32 {
		return 0, fmt.Errorf("reading the postmaster PID: %s: %d is not a PID", path, n)
	}
	return int32(n), nil
}

// readRegular reads at most limit bytes of the file at path. Only a regular file is read, so
// the read cannot wait without bound (AGENTS.md §2; cmd/rladmin reads its policy file so): a
// FIFO, a device or any other file is refused unopened, and the open (O_NONBLOCK: a FIFO
// swapped in meanwhile does not wait for a writer) is checked again before reading.
func readRegular(path string, limit int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %v)", path, fi.Mode())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // opened read-only: closing cannot change what was read
	if fi, err = f.Stat(); err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %v)", path, fi.Mode())
	}
	return io.ReadAll(io.LimitReader(f, limit))
}
