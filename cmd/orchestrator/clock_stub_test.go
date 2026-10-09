//go:build !rltest

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestClockStub_Refuses (plan H3 steps 1 and 3): an untagged orchestrator refuses a
// controlled-clock scenario and a faultctl cmd with exit 2 and the reason, starting nothing;
// its clock call fails. It checks this build's own reasons (clock_stub.go): the rules every
// build applies have their cases in TestPreflight_Refusals.
func TestClockStub_Refuses(t *testing.T) {
	const steps = "[[steps]]\nrate_per_s = 30\nduration_s = 10\n\n[users]\ncount = 50\n"
	for _, c := range []struct {
		name, tables string
		want         []string
	}{
		{"controlled clock", "[policy]\nclock_source = \"controlled\"\n" + steps, []string{reasonClockUntagged}},
		{"faultctl cmd", steps + "[[faults]]\nat_s = 1\ncmd = \"faultctl store pause 4.5s\"\n", []string{reasonFaultctlUntagged}},
		{"store clock, no faults", steps, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := stubSite(t)
			path := filepath.Join(t.TempDir(), "scenario.toml")
			text := "name = \"stub\"\nseed = 1\nrun_kind = \"test\"\n" + c.tables
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			if c.want == nil {
				if _, refs := preflight(t.Context(), s, flags{scenario: path}); len(refs) > 0 {
					t.Fatalf("refusals %+v, want none", refs)
				}
				return
			}
			var stderr bytes.Buffer
			code := orchestrate(t.Context(), []string{"--scenario", path}, s, nil, os.Getpid(), time.Now, &stderr)
			if code != exitRefused {
				t.Fatalf("exit code %d, want %d\nstderr:\n%s", code, exitRefused, stderr.String())
			}
			var got []string
			for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
				var rec struct{ Msg, Reason, Detail string }
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("stderr line %q is not JSON: %v", line, err)
				}
				own := rec.Reason == reasonClockUntagged || rec.Reason == reasonFaultctlUntagged
				if rec.Msg == "refusing the run" && rec.Detail != "" && own {
					got = append(got, rec.Reason)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("refusal reasons %q, want %q\nstderr:\n%s", got, c.want, stderr.String())
			}
			if _, err := os.Stat(s.path(runsDir)); !os.IsNotExist(err) {
				t.Errorf("a refused run created %s (stat: %v)", runsDir, err)
			}
			if _, err := os.Stat(s.path(pidFileName)); !os.IsNotExist(err) {
				t.Errorf("a refused run wrote %s (stat: %v)", pidFileName, err)
			}
		})
	}
	if _, err := setControlledClock(t.Context(), "127.0.0.1:1", "pw", time.Now()); !errors.Is(err, errNotTestBuild) {
		t.Errorf("setControlledClock in an untagged build: error %v, want %v", err, errNotTestBuild)
	}
}

// stubSite is a repository tree in a temporary directory that a test run passes preflight in:
// the repository's harness.toml, executable stand-ins for bin/test's binaries, and four
// consecutive free loopback ports (freePortBlock).
func stubSite(t *testing.T) site {
	t.Helper()
	root := t.TempDir()
	base, err := os.ReadFile(filepath.Join("..", "..", "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, harnessConfigFile), base, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin", "test"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hitapi", "rladmin", "hitgen", "sampler"} {
		if err := os.WriteFile(filepath.Join(root, "bin", "test", name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p := freePortBlock(t, 4)
	return site{root: root, storePort: p, apiBase: p + 1}
}

// The loopback ports freePortBlock picks from: below the kernels' ephemeral ranges (macOS
// 49152–65535, Linux 32768–60999), so a listener another process opens on port 0 never lands in
// a block once it is picked.
const (
	portRangeLow  = 20_000
	portRangeHigh = 32_000
)

// freePortBlock returns the first of n consecutive loopback ports that preflight's own check
// finds free now (portHeld: no connection to the port succeeds, so nothing listens on it or on
// every address, and it binds). It walks the n-port blocks of [portRangeLow, portRangeHigh)
// once, from a block derived from the test's name and this process's PID, so the tests and
// concurrent test processes start apart. preflight_test.go holds the rltest build's copy.
func freePortBlock(t *testing.T, n int) int {
	t.Helper()
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name() + " " + strconv.Itoa(os.Getpid()))) // a hash write never fails
	blocks := (portRangeHigh - portRangeLow) / n
	start := int(h.Sum32()) % blocks
	for i := range blocks {
		base := portRangeLow + (start+i)%blocks*n
		free := true
		for p := base; p < base+n && free; p++ {
			held, _ := portHeld(t.Context(), p)
			free = !held
		}
		if free {
			return base
		}
	}
	t.Fatalf("no %d consecutive free loopback ports in [%d, %d)", n, portRangeLow, portRangeHigh)
	return 0
}
