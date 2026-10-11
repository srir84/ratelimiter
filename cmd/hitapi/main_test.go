package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"ratelimiter/harness/config"
)

// harnessTOML is the repo's §B2 config file.
const harnessTOML = "../../harness.toml"

// TestParseFlags: --config, --listen and --log are required, --instance is 1 or more, and
// --listen must be a loopback IP address with a port; anything else is refused.
func TestParseFlags(t *testing.T) {
	base := []string{"--config", "c.toml", "--listen", "127.0.0.1:8080", "--log", "api-1.jsonl"}
	with := func(extra ...string) []string { return append(slices.Clone(base), extra...) }
	for _, c := range []struct {
		name string
		args []string
		want *flags // nil: refused
	}{
		{"required only", base, &flags{config: "c.toml", listen: "127.0.0.1:8080", log: "api-1.jsonl", instance: 1}},
		{"all flags", with("--instance", "3", "--run-id", "run-7"),
			&flags{config: "c.toml", listen: "127.0.0.1:8080", log: "api-1.jsonl", instance: 3, runID: "run-7"}},
		{"instance 1", with("--instance", "1"), &flags{config: "c.toml", listen: "127.0.0.1:8080", log: "api-1.jsonl", instance: 1}},
		{"instance 0", with("--instance", "0"), nil},
		{"other loopback address", []string{"--config", "c", "--listen", "127.0.0.2:1", "--log", "l"},
			&flags{config: "c", listen: "127.0.0.2:1", log: "l", instance: 1}},
		{"IPv6 loopback", []string{"--config", "c", "--listen", "[::1]:8080", "--log", "l"},
			&flags{config: "c", listen: "[::1]:8080", log: "l", instance: 1}},
		{"no --config", []string{"--listen", "127.0.0.1:8080", "--log", "l"}, nil},
		{"no --listen", []string{"--config", "c", "--log", "l"}, nil},
		{"no --log", []string{"--config", "c", "--listen", "127.0.0.1:8080"}, nil},
		{"extra argument", with("more"), nil},
		{"unknown flag", with("--bogus"), nil},
		{"all interfaces", []string{"--config", "c", "--listen", "0.0.0.0:8080", "--log", "l"}, nil},
		{"LAN address", []string{"--config", "c", "--listen", "192.168.1.5:8080", "--log", "l"}, nil},
		{"host name", []string{"--config", "c", "--listen", "localhost:8080", "--log", "l"}, nil},
		{"no port", []string{"--config", "c", "--listen", "127.0.0.1", "--log", "l"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFlags(c.args, io.Discard)
			switch {
			case c.want == nil && err == nil:
				t.Errorf("parseFlags(%q) = %+v, want an error", c.args, got)
			case c.want != nil && err != nil:
				t.Errorf("parseFlags(%q): %v", c.args, err)
			case c.want != nil && got != *c.want:
				t.Errorf("parseFlags(%q) = %+v, want %+v", c.args, got, *c.want)
			}
		})
	}
	if _, err := parseFlags([]string{"--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("parseFlags(--help) error %v, want flag.ErrHelp", err)
	}
}

// TestLoadConfig_RequiresLibPassword: hitapi refuses to start when RL_LIB_PASSWORD is unset
// or empty; with it, the store password is that value.
func TestLoadConfig_RequiresLibPassword(t *testing.T) {
	t.Setenv(config.LibPasswordEnv, "")
	if _, err := loadConfig(harnessTOML); err == nil {
		t.Errorf("loadConfig with %s empty succeeded, want an error", config.LibPasswordEnv)
	}
	t.Setenv(config.LibPasswordEnv, "pw-for-test")
	f, err := loadConfig(harnessTOML)
	if err != nil {
		t.Fatalf("loadConfig with %s set: %v", config.LibPasswordEnv, err)
	}
	if f.Store.Password != "pw-for-test" {
		t.Error("the store password is not RL_LIB_PASSWORD's value")
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Error("loadConfig of a missing file succeeded")
	}
}

// TestCheckNofile: a soft RLIMIT_NOFILE below 1,024 is refused, 1,024 and more start (§B1).
func TestCheckNofile(t *testing.T) {
	for _, c := range []struct {
		n  uint64
		ok bool
	}{{0, false}, {512, false}, {1023, false}, {1024, true}, {1025, true}, {245760, true}} {
		if err := checkNofile(c.n); (err == nil) != c.ok {
			t.Errorf("checkNofile(%d) = %v, want ok %v", c.n, err, c.ok)
		}
	}
}

// TestRun_RefusesToStart: a bad command line or an unset RL_LIB_PASSWORD exits 2 before the
// run log is created; --help exits 0.
func TestRun_RefusesToStart(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "api-1.jsonl")
	args := []string{"--config", harnessTOML, "--listen", "127.0.0.1:1", "--log", logPath}
	t.Setenv(config.LibPasswordEnv, "")
	if code := run(t.Context(), time.Now(), args, io.Discard); code != exitUsage {
		t.Errorf("run without %s = %d, want %d", config.LibPasswordEnv, code, exitUsage)
	}
	if code := run(t.Context(), time.Now(), []string{"--listen", "127.0.0.1:1"}, io.Discard); code != exitUsage {
		t.Errorf("run with a bad command line = %d, want %d", code, exitUsage)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run log exists after a refused start (%v), want none", err)
	}
	if code := run(t.Context(), time.Now(), []string{"--help"}, io.Discard); code != exitOK {
		t.Errorf("run --help = %d, want %d", code, exitOK)
	}
}

// stopSteps records the SIGTERM steps in order, with each step's ctx deadline (zero for
// BeginShutdown, which takes none).
type stopSteps struct {
	steps     []string
	deadlines []time.Time
	idleErr   error
}

func (s *stopSteps) add(step string, dl time.Time) {
	s.steps = append(s.steps, step)
	s.deadlines = append(s.deadlines, dl)
}

func (s *stopSteps) BeginShutdown() { s.add("BeginShutdown", time.Time{}) }

func (s *stopSteps) WaitIdle(ctx context.Context) error {
	dl, _ := ctx.Deadline()
	s.add("WaitIdle", dl)
	return s.idleErr
}

// stopServer is the HTTP server's part, recording into the same steps.
type stopServer struct{ s *stopSteps }

func (v stopServer) Shutdown(ctx context.Context) error {
	dl, _ := ctx.Deadline()
	v.s.add("Shutdown", dl)
	return nil
}

// TestStopRequests: on SIGTERM (HAR-15) BeginShutdown comes first, so work not yet at its
// first DB statement is refused while the server waits; then http.Server.Shutdown and
// WaitIdle, both bounded by one P deadline from the start of the sequence.
func TestStopRequests(t *testing.T) {
	const p = 5 * time.Second
	steps := &stopSteps{idleErr: errors.New("still busy")}
	logger := slog.New(slog.DiscardHandler)
	before := time.Now()
	stopRequests(t.Context(), logger, steps, stopServer{steps}, p)
	after := time.Now()
	if want := []string{"BeginShutdown", "Shutdown", "WaitIdle"}; !slices.Equal(steps.steps, want) {
		t.Fatalf("steps %v, want %v", steps.steps, want)
	}
	dl := steps.deadlines[1]
	if dl.Before(before.Add(p)) || dl.After(after.Add(p)) {
		t.Errorf("Shutdown's deadline is %v after the sequence began, want P = %v", dl.Sub(before), p)
	}
	if !steps.deadlines[2].Equal(dl) {
		t.Errorf("WaitIdle's deadline %v, want Shutdown's %v: P bounds both together", steps.deadlines[2], dl)
	}
}

// TestShutdownBudget (SPEC HOST-6; HARNESS HAR-15; DESIGN §B10): hitapi's timed SIGTERM waits
// fit the orchestrator's kill grace, by DESIGN's harness numbers. P 5 s (stopRequests), the
// drain_timeout 35 s (Close's drain), the limiter's 100 ms listener grace past the drain
// (internal/core TestListenerGraceBudget caps it) and logDrainTimeout (the run log's drain)
// must add up to less than the 45 s kill grace, leaving time for the steps hitapi does not
// time: the drain's cancel of the attempts in flight, the limiter's shutdown, the run log
// writing its remaining lines and closing, the database pool's close and the exit.
func TestShutdownBudget(t *testing.T) {
	const (
		p             = 5 * time.Second
		drainTimeout  = 35 * time.Second
		listenerGrace = 100 * time.Millisecond
		killGrace     = 45 * time.Second
	)
	worst := p + drainTimeout + listenerGrace + logDrainTimeout
	if slack := killGrace - worst; slack <= 0 {
		t.Errorf("P %v + drain_timeout %v + listener grace %v + run-log drain %v = %v, want less than the %v kill grace (slack %v)",
			p, drainTimeout, listenerGrace, logDrainTimeout, worst, killGrace, slack)
	}
}
