//go:build rltest

package main

import (
	"context"
	"testing"

	"github.com/srir84/ratelimiter/harness/config"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// TestClock_SetControlled (plan H3 step 3; DESIGN §B6 "Controlled time"): the rltest build
// sets the controlled clock in-process as rl_fixture, writing now truncated to µs, and
// refuses nothing in preflight; a wrong password or an ended context writes nothing.
func TestClock_SetControlled(t *testing.T) {
	st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
	read := func() storeproc.Reply {
		t.Helper()
		c, err := storeproc.Dial(st.Addr, "rl_fixture", st.PW.Fixture)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		r, err := c.Do("GET", keys.CtlTimeKey)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if _, err := setControlledClock(t.Context(), st.Addr, "not-the-password", testNow()); err == nil {
		t.Error("setControlledClock with a wrong password: no error")
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := setControlledClock(ended, st.Addr, st.PW.Fixture, testNow()); err == nil {
		t.Error("setControlledClock with an ended context: no error")
	}
	if r := read(); r.Kind != '$' || !r.Nil {
		t.Fatalf("rl:ctl:time after the refused calls = %s, want nil", r)
	}

	// testNow is 2026-10-08T14:22:33.123456789Z: Unix second 1791469353.
	ns, err := setControlledClock(t.Context(), st.Addr, st.PW.Fixture, testNow())
	if err != nil {
		t.Fatal(err)
	}
	if ns != 1_791_469_353_123_456_000 {
		t.Errorf("setControlledClock wrote %d ns, want 1791469353123456000 (now truncated to µs)", ns)
	}
	if r := read(); r.Kind != '$' || r.Str != "1791469353123456" {
		t.Errorf("rl:ctl:time = %s, want \"1791469353123456\" (µs)", r)
	}
	faultctl := config.Scenario{Faults: []config.Fault{{AtS: 1, Cmd: "faultctl store down 4s", Targets: []int{1}}}}
	if refs := buildRefusals(faultctl, true); len(refs) != 0 {
		t.Errorf("the rltest build's own refusals %+v, want none", refs)
	}
}
