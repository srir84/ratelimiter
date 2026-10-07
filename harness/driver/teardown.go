//go:build rltest

package driver

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storestate"
	"ratelimiter/limiter"
)

// The teardown checks of every L3 test (DESIGN §C1, §C4; IMPLEMENTATION_PLAN S5 step 16, S6
// step 13): once every instance is closed or killed and while the store still runs, the
// commands each instance sent, its refund attempts against its refund sends, the store audit
// on a quiet store with every signing-key canary (and the canary search of the store's AOF
// files), and BUD-11 on the op log. S8 adds Tamper writes and restored keys.

const (
	quietTries = 5                      // snapshots taken at most
	quietGap   = 100 * time.Millisecond // between two snapshots
)

// teardownChecks closes every instance the driver built (Close does nothing for a closed or
// killed one), each within its drain timeout plus closeSlack (closeBounded: a Close that does
// not return by then is reported and left behind, and the checks go on, so the store is still
// stopped after them), then runs the allowed-commands check, the refund-sends check, the store
// audit and the BUD-11 check, reporting each violation with tb.Errorf.
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
			d.closeBounded(tb, in)
		}
	}
	d.checkCommands(tb, all)
	d.checkRefundSends(tb, all)
	d.auditStore(tb)
	d.checkBud11(tb)
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

// refundTally is what the refund-sends check needs of one instance.
type refundTally struct {
	attempts    int  // Σ RefundEvent.Attempts over the events the driver's listener received
	allFinal    bool // every release through the driver got a final event
	unresolved  bool // one of them is unresolved
	releases    int  // releases of permits with a receipt through the driver
	eventsCount int
}

// tally adds up instance in's refund events and releases.
func (d *Driver) tally(in *instance) refundTally {
	in.mu.Lock()
	events := slices.Clone(in.events)
	in.mu.Unlock()
	t := refundTally{allFinal: true, eventsCount: len(events)}
	for _, ev := range events {
		t.attempts += ev.Attempts
		if ev.Result == limiter.Unresolved {
			t.unresolved = true
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, rec := range d.permits {
		if rec.inst != in || !rec.released() {
			continue
		}
		t.releases++
		if rec.events == 0 {
			t.allFinal = false
		}
	}
	return t
}

// checkRefundSends is the refund-sends check (DESIGN §C4 command budgets; plan S6 step 13):
// per instance, the attempts of the final events the driver's listener received (API-1d)
// add up to at most the refund sends its send observer heard with bytes written, and to
// exactly that many when every release through the driver got a final event, none of them
// unresolved, no event was dropped (listener_drops 0) and the store refused no refund with
// LOADING or BUSY (written, but not an attempt).
func (d *Driver) checkRefundSends(tb testing.TB, all []*instance) {
	tb.Helper()
	for _, in := range all {
		if in == nil || in.l == nil {
			continue
		}
		t := d.tally(in)
		sends := Count(in.sends(), "refund")
		drops := in.l.Health().ListenerDrops
		notReady := in.sw.NotReadyRefunds()
		if t.attempts > sends {
			tb.Errorf("refund sends: instance %d: the %d final events received count %d attempts, more than the %d refund sends with bytes written",
				in.index, t.eventsCount, t.attempts, sends)
			continue
		}
		if t.allFinal && !t.unresolved && drops == 0 && notReady == 0 && t.attempts != sends {
			tb.Errorf("refund sends: instance %d: %d attempts over %d final events (one for each of its %d releases, none unresolved, no listener drop, no LOADING or BUSY refund), but %d refund sends with bytes written",
				in.index, t.attempts, t.eventsCount, t.releases, sends)
		}
	}
}

// auditStore takes snapshots as rl_lib until two in a row are identical (at most 5, 100 ms
// apart; otherwise "store not quiet"), then reports every storestate.Audit message: the
// policy is the epoch policy whose fingerprint is rl:epoch.fp, Damaged every key a Fixture
// write named, the filler prefix rl:pad:, and Canaries every signing-key canary the driver
// gave an instance. It also searches every file in the store's appendonlydir/ for the
// canaries (searchAOF).
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
		Canaries:       d.canaryListLocked(),
	}
	d.mu.Unlock()
	for _, v := range storestate.Audit(snap, cfg) {
		tb.Errorf("store audit: %s", v)
	}
	d.searchAOF(tb, cfg.Canaries)
}

// canaryListLocked returns the canaries by index; d.mu is held.
func (d *Driver) canaryListLocked() [][]byte {
	idx := slices.Sorted(maps.Keys(d.canaries))
	out := make([][]byte, 0, len(idx))
	for _, i := range idx {
		out = append(out, d.canaries[i])
	}
	return out
}

// searchAOF reports every canary found, in any form of the audit's rule, in a file of the
// store's appendonlydir/ (DESIGN §C4 canaries; plan S6 step 13). The messages name the file,
// the canary's place in the list and the form, never the canary.
func (d *Driver) searchAOF(tb testing.TB, canaries [][]byte) {
	tb.Helper()
	if len(canaries) == 0 {
		return
	}
	dir := filepath.Join(d.st.Dir, "appendonlydir")
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Errorf("canaries: reading the AOF directory: %v", err)
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			tb.Errorf("canaries: reading AOF file %s: %v", e.Name(), err)
			continue
		}
		for _, hit := range storestate.FindCanaries(data, canaries) {
			tb.Errorf("canaries: AOF file %s holds %s", e.Name(), hit)
		}
	}
}

// checkBud11 is the teardown BUD-11 check (DESIGN §C4; plan S6 step 13): when the test logged
// ops (acquire, release, done, cleanup or cleanup_scan records), it writes the op log and runs
// python3 -m refmodel.check_bud11 on it; a non-zero exit,
// or no python3, fails the test (as in CheckRefModel). A test that wrote through Fixture
// (a declared key, or FLUSHALL/FLUSHDB) is exempt: it asserts its refusals directly.
func (d *Driver) checkBud11(tb testing.TB) {
	tb.Helper()
	d.mu.Lock()
	exempt := len(d.damaged) > 0 || d.wiped
	logged := d.logged > 0
	lines := slices.Clone(d.oplog)
	d.mu.Unlock()
	if exempt || !logged {
		return
	}
	path := filepath.Join(tb.TempDir(), "oplog-bud11.jsonl")
	if err := os.WriteFile(path, joinLines(lines), 0o600); err != nil {
		tb.Errorf("BUD-11: writing the op log: %v", err)
		return
	}
	code, out, err := RunRefModel(tb.Context(), "refmodel.check_bud11", path)
	switch {
	case err != nil:
		tb.Errorf("BUD-11: %v (a failure, never a skip)", err)
	case code != 0:
		tb.Errorf("BUD-11: refmodel.check_bud11 rejects the op log (exit status %d):\n%s\nop log, %d records:\n%s", code, out, len(lines), excerpt(lines))
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
