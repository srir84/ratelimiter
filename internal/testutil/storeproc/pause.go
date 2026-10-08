package storeproc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// stateWait bounds Pause's and Resume's wait for ps to show the new process state.
	stateWait = 5 * time.Second
	// statePoll is the pause between two ps runs.
	statePoll = time.Millisecond
	// psTimeout bounds one ps run.
	psTimeout = 5 * time.Second
)

// PaddingA7 is the number of LoadPadding hashes that make a store the size of DESIGN §A7's
// sizing: as many keys as the §C2 reload dataset (100,000 bucket hashes, half of them with a
// marker, their 50,000 marker sets and the index), about 40 MB. Its reload kept a restarted
// store answering LOADING for about half a second here (.data/conformance.json reload_ms_a7).
const PaddingA7 = 150_000

// Padding writes (plan choice).
const (
	paddingConns = 16    // rl_fixture connections LoadPadding writes over, as §C2's dataset load
	paddingBatch = 1_000 // HSET commands per pipeline
)

// PaddingPrefix is the key prefix of LoadPadding's hashes: the filler prefix the driver's store
// audit accepts (DESIGN §C3 grammar, IMPLEMENTATION_PLAN S5 step 10).
const PaddingPrefix = "rl:pad:"

// errNotPaused refuses Resume on a store Pause did not stop.
var errNotPaused = errors.New("the store is not paused")

// Pause stops the store process with SIGSTOP (IMPLEMENTATION_PLAN S9 step 4; HARNESS FLT-2, a
// blackhole outage): it keeps its sockets, and the kernel still completes TCP connects to its
// port into the listen backlog, but the store reads and answers nothing until Resume. It
// returns once ps shows the process stopped, so every command written after it waits for
// Resume. Stop, Kill and Restart continue a paused store themselves, so a paused store never
// outlives its test.
func (s *Store) Pause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.runningLocked()
	if err != nil {
		return fmt.Errorf("pausing the store: %w", err)
	}
	if s.paused {
		return errors.New("pausing the store: it is already paused")
	}
	if err := p.signal(syscall.SIGSTOP); err != nil {
		return fmt.Errorf("pausing the store: %w", err)
	}
	s.paused = true
	if err := s.waitState(p, true); err != nil {
		return fmt.Errorf("pausing the store: %w", err)
	}
	return nil
}

// Resume continues a store that Pause stopped (SIGCONT) and returns once ps no longer shows it
// stopped. It refuses a store that is not paused.
func (s *Store) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.paused {
		return fmt.Errorf("resuming the store: %w", errNotPaused)
	}
	p, err := s.runningLocked()
	if err != nil {
		return fmt.Errorf("resuming the store: %w", err)
	}
	if err := p.signal(syscall.SIGCONT); err != nil {
		return fmt.Errorf("resuming the store: %w", err)
	}
	s.paused = false
	if err := s.waitState(p, false); err != nil {
		return fmt.Errorf("resuming the store: %w", err)
	}
	return nil
}

// runningLocked returns the running process, or an error when there is none; s.mu is held.
func (s *Store) runningLocked() (*proc, error) {
	p := s.proc
	if p == nil {
		return nil, errors.New("the store is not running")
	}
	select {
	case <-p.done:
		return nil, fmt.Errorf("store process %d has exited", p.pid)
	default:
		return p, nil
	}
}

// waitState polls ps until it shows p stopped (stopped true) or not stopped, for at most
// stateWait.
func (s *Store) waitState(p *proc, stopped bool) error {
	ctx, cancel := context.WithTimeout(s.ctx(), stateWait)
	defer cancel()
	for {
		state, err := processState(ctx, p.pid)
		if err != nil {
			return err
		}
		if strings.HasPrefix(state, "T") == stopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("store process %d: ps state %q after %v, want stopped %v", p.pid, state, stateWait, stopped)
		case <-p.done:
			return fmt.Errorf("store process %d exited", p.pid)
		case <-time.After(statePoll):
		}
	}
}

// processState returns ps's state letters of process pid: "T…" while it is stopped.
func processState(ctx context.Context, pid int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// LoadPadding writes n filler hashes rl:pad:0 … rl:pad:<n−1> as rl_fixture (IMPLEMENTATION_PLAN
// S9 step 4), each shaped like a §A3.2 bucket hash with one marker field, pipelined over 16
// connections. PaddingA7 of them give the store the §A7 size, so a Stop and a Restart(false)
// leave it loading its data for a while, answering commands with LOADING (the S1 technique,
// DESIGN §C2, §A3.0 row 16). The keys lie under PaddingPrefix, outside the §A3.2 layout.
func (s *Store) LoadPadding(n int) error {
	if n < 0 {
		return fmt.Errorf("loading padding: %d hashes", n)
	}
	conns := min(paddingConns, max(1, (n+paddingBatch-1)/paddingBatch))
	errs := make([]error, conns)
	var wg sync.WaitGroup
	for w := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[w] = s.padRange(w, conns, n)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("loading padding: %w", err)
	}
	return nil
}

// padRange writes, on its own rl_fixture connection, the hashes i < n with i % step == first,
// paddingBatch per pipeline; every reply must be the integer 7, the fields the hash got.
func (s *Store) padRange(first, step, n int) (err error) {
	c, err := s.Conn("rl_fixture")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }() // every pipeline was read to its end
	batch := make([][]string, 0, paddingBatch)
	flush := func() error {
		reps, err := c.Pipeline(batch)
		if err != nil {
			return err
		}
		for i, r := range reps {
			if r.Kind != ':' || r.Int != 7 {
				return fmt.Errorf("HSET %s: reply %s, want :7", batch[i][1], r)
			}
		}
		batch = batch[:0]
		return nil
	}
	for i := first; i < n; i += step {
		id := strconv.Itoa(i)
		batch = append(batch, []string{
			"HSET", PaddingPrefix + id, "u", "1000000000", "t", "1760000000000000", "g", "1", "s", id,
			"x", "1760000060000000", "mc", "1", "m:1:" + id, "1760000060000000:1760000000000000",
		})
		if len(batch) == paddingBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if len(batch) > 0 {
		return flush()
	}
	return nil
}
