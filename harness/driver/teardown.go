//go:build rltest

package driver

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storestate"
	"ratelimiter/limiter"
)

// The teardown checks of every L3 test (DESIGN §C1, §C4; IMPLEMENTATION_PLAN S5 step 16):
// once every instance is closed or killed and while the store still runs, the commands each
// instance sent, then the store audit on a quiet store. S6 adds canaries and the BUD-11
// check, S8 Tamper writes and restored keys.

const (
	quietTries = 5                      // snapshots taken at most
	quietGap   = 100 * time.Millisecond // between two snapshots
)

// teardownChecks closes every instance the driver built (Close does nothing for a closed or
// killed one), then runs the allowed-commands check and the store audit, reporting each
// violation with tb.Errorf.
func (d *Driver) teardownChecks(tb testing.TB) {
	tb.Helper()
	if d.st == nil {
		return
	}
	d.mu.Lock()
	all := slices.Clone(d.all)
	d.mu.Unlock()
	for _, in := range all {
		if in != nil && in.l != nil {
			in.l.Close()
		}
	}
	d.checkCommands(tb, all)
	d.auditStore(tb)
}

// allowedCommand reports whether an instance may send cmd (DESIGN §C4): an EVALSHA of one of
// the seven scripts rl_lib runs (§A2), recorded by script name, SCRIPT LOAD, SCAN (S7's
// reclaimer), PING or AUTH.
func allowedCommand(cmd string) bool {
	switch cmd {
	case "acquire", "refund", "due_entries", "cleanup_entry", "reclaim", "read_epoch", "hold",
		"SCRIPT LOAD", "SCAN", "PING", "AUTH":
		return true
	}
	return false
}

// checkCommands is the allowed-commands check: each instance's send-observer records with
// bytes written name only allowed commands, and it sent no more SCRIPT LOAD than the
// responses with NoScript its store wrapper saw. The ACL cannot enforce this: rl_lib must be
// allowed the scripts' inner commands, so Go code could send HGETALL directly.
func (d *Driver) checkCommands(tb testing.TB, all []*instance) {
	tb.Helper()
	for _, in := range all {
		if in == nil {
			continue
		}
		loads := 0
		for _, s := range in.sends() {
			if s.Bytes <= 0 {
				continue
			}
			if !allowedCommand(s.Cmd) {
				tb.Errorf("allowed commands: instance %d sent %q (%d bytes); only rl_lib scripts, SCRIPT LOAD, SCAN, PING and AUTH may be sent", in.index, s.Cmd, s.Bytes)
			}
			if s.Cmd == "SCRIPT LOAD" {
				loads++
			}
		}
		if ns := in.sw.NoScripts(); loads > ns {
			tb.Errorf("allowed commands: instance %d sent %d SCRIPT LOAD, more than the %d responses with NoScript its store wrapper saw", in.index, loads, ns)
		}
	}
}

// auditStore takes snapshots as rl_lib until two in a row are identical (at most 5, 100 ms
// apart; otherwise "store not quiet"), then reports every storestate.Audit message: the
// policy is the epoch policy whose fingerprint is rl:epoch.fp, Damaged every key a Fixture
// write named, and the filler prefix rl:pad:.
func (d *Driver) auditStore(tb testing.TB) {
	tb.Helper()
	c, err := d.st.Conn("rl_lib")
	if err != nil {
		tb.Errorf("store audit: connecting as rl_lib: %v", err)
		return
	}
	defer func() { _ = c.Close() }()
	var snap storestate.State
	quiet := false
	for try := range quietTries {
		if try > 0 {
			time.Sleep(quietGap)
		}
		s, err := storestate.Snapshot(c)
		if err != nil {
			tb.Errorf("store audit: %v", err)
			return
		}
		if try > 0 && reflect.DeepEqual(s, snap) {
			quiet = true
			break
		}
		snap = s
	}
	if !quiet {
		tb.Errorf("store audit: store not quiet: no two snapshots in a row were identical in %d tries, %v apart", quietTries, quietGap)
		return
	}
	d.mu.Lock()
	cfg := storestate.AuditConfig{
		Policy:         d.policyFor(snap[keys.EpochKey].Fields["fp"]),
		Damaged:        slices.Clone(d.damaged),
		FillerPrefixes: []string{FillerPrefix},
	}
	d.mu.Unlock()
	for _, v := range storestate.Audit(snap, cfg) {
		tb.Errorf("store audit: %s", v)
	}
}

// policyFor returns the epoch policy whose fingerprint is fp, else the driver's policy (so
// Audit reports a mismatch); d.mu is held.
func (d *Driver) policyFor(fp string) limiter.Policy {
	for _, p := range d.policies {
		if fingerprint.Of(p) == fp {
			return p
		}
	}
	return d.policy
}
