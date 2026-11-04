//go:build rltest

package driver

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
	"github.com/srir84/ratelimiter/limiter"
)

// The driver's cleanup support (IMPLEMENTATION_PLAN S7 step 11; DESIGN §A4.5, §A5.8, §C4): the
// capture of every cleanup_entry step result and due_entries page an instance gets from its
// store adapter, which writes the op log's cleanup and cleanup_scan records (oplog.go); store
// reads as rl_lib (StoreKeys) and the integrity hold through hold.lua as rl_lib (SetHold); and
// a store restart that keeps the data (RestartStore). A barrier on one key's cleanup_entry step
// is a fixtures.Fault with Match "cleanup_entry" and that Key: the store wrapper applies faults
// per request inside CallBatch, and sends a request held before its send alone after Release.

// CleanupStep is one cleanup_entry step result an instance's store adapter returned, before
// any fault of its store wrapper acted on it: a decoded {code, t, reason, markers_removed}
// reply (§A4.9) with code 30 (removed), 31 (moved), 32 (quarantined) or 33 (skipped).
type CleanupStep struct {
	Instance int
	Member   string // the step's ix member: the key's enc
	Key      string // the budget key Member encodes; "" when Member fails is_enc
	Code     int64
	TUs      int64  // the step's budget time
	Reason   string // "" or the reply's reason (gone, not_due, quarantined, a §A3.3 reason)
	Markers  int64  // markers_removed
}

// CleanupSteps returns the cleanup steps captured so far, in completion order.
func (d *Driver) CleanupSteps() []CleanupStep {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.steps)
}

// StepsFor returns the captured cleanup steps of key, in completion order.
func (d *Driver) StepsFor(key string) []CleanupStep {
	var out []CleanupStep
	for _, s := range d.CleanupSteps() {
		if s.Key == key {
			out = append(out, s)
		}
	}
	return out
}

// CallBatch hands the adapter's responses to the driver before any fault acts on them
// (logSteps): the store did the steps they confirm whatever the limiter gets, so a dropped
// reply still has its record.
func (a inner) CallBatch(ctx context.Context, reqs []store.Request) []store.Response {
	rs := a.Adapter.CallBatch(ctx, reqs)
	if a.d != nil {
		a.d.logSteps(a.inst, reqs, rs)
	}
	return rs
}

// logSteps records every cleanup_entry step result among rs, the responses to reqs, in order:
// CleanupSteps, and the op log's cleanup record {key_b64url, t_us, code} (orchestrator
// decision, S7 task 2). A member that fails is_enc has no key to log; its step can only be a
// skip, which writes nothing (§A4.5), and it arises from damaged state alone, whose tests are
// exempt from the op-log checks.
func (d *Driver) logSteps(in *instance, reqs []store.Request, rs []store.Response) {
	var steps []CleanupStep
	var recs []any
	for i, req := range reqs {
		if i >= len(rs) || req.Script.Name != scripts.CleanupEntry.Name || len(req.Args) < 3 {
			continue
		}
		r := rs[i]
		if r.Outcome != store.OK || r.Reply.Code < 30 || r.Reply.Code > 33 {
			continue
		}
		member := req.Args[2]
		st := CleanupStep{
			Instance: in.index, Member: member, Code: r.Reply.Code, TUs: r.Reply.TUs,
			Reason: r.Reply.Reason, Markers: elemInt(r.Reply, 3),
		}
		if keys.IsEnc(member) {
			if b, err := base64.RawURLEncoding.DecodeString(member); err == nil {
				st.Key = string(b)
				recs = append(recs, cleanupRecord{Type: "cleanup", KeyB64URL: member, TUs: r.Reply.TUs, Code: r.Reply.Code})
			}
		}
		steps = append(steps, st)
	}
	if len(steps) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.steps = append(d.steps, steps...)
	d.logged += len(recs)
	d.appendLocked(recs...)
}

// logScan records a due_entries page the adapter returned, before any fault acts on it: the
// op log's cleanup_scan record {t_us}, t_us the page's T0, which the page stored as the
// high-water mark (CLK-4; orchestrator decision, S7 task 2). A refusal stores nothing and has
// no record.
func (d *Driver) logScan(r store.Response) {
	if r.Outcome != store.OK || r.Reply.Code != 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logged++
	d.appendLocked(cleanupScanRecord{Type: "cleanup_scan", TUs: r.Reply.TUs})
}

// passWait bounds every cleanup pass the driver runs (plan choice). A pass that has not ended by
// then, such as one that keeps reading the same page because a step re-scored its entry at
// the pass's t instead of above it (DESIGN §A4.5), stops at its next store call, whose
// deadline the context sets, as unavailable or outcome_unknown: the test then fails by
// assertion, not at go test's timeout.
const passWait = 30 * time.Second

// RunCleanupPass runs one cleanup pass on instance i now (API-1f), on the test's context
// bounded by passWait.
func (d *Driver) RunCleanupPass(i int) limiter.CleanupReport {
	in := d.current(i)
	if in == nil {
		return limiter.CleanupReport{}
	}
	ctx, cancel := context.WithTimeout(d.t.Context(), passWait)
	defer cancel()
	return in.l.RunCleanupPass(ctx)
}

// StoreKeys returns the distinct names of the store's keys that match pattern (a SCAN MATCH
// glob), sorted: SCAN 0 MATCH pattern COUNT 1000 as rl_lib until the cursor is 0 again (plan
// S7 step 11). A key that SCAN returns twice is listed once.
func (d *Driver) StoreKeys(pattern string) []string {
	d.t.Helper()
	c, err := d.st.Conn("rl_lib")
	if err != nil {
		d.t.Fatalf("driver: StoreKeys: %v", err)
		return nil
	}
	defer func() { _ = c.Close() }()
	seen := map[string]bool{}
	cursor := "0"
	for {
		r, err := c.Do("SCAN", cursor, "MATCH", pattern, "COUNT", "1000")
		if err != nil {
			d.t.Fatalf("driver: StoreKeys: SCAN %s: %v", cursor, err)
			return nil
		}
		next, names, ok := scanPage(r)
		if !ok {
			d.t.Fatalf("driver: StoreKeys: SCAN %s replied %s", cursor, r)
			return nil
		}
		for _, n := range names {
			seen[n] = true
		}
		if next == "0" {
			break
		}
		cursor = next
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// scanPage reads a SCAN reply, [cursor, [name, …]]: the cursor in decimal and every name a
// bulk string.
func scanPage(r storeproc.Reply) (string, []string, bool) {
	if r.Kind != '*' || r.Nil || len(r.Arr) != 2 || r.Arr[0].Kind != '$' || r.Arr[1].Kind != '*' {
		return "", nil, false
	}
	if _, err := strconv.ParseUint(r.Arr[0].Str, 10, 64); err != nil {
		return "", nil, false
	}
	names := make([]string, 0, len(r.Arr[1].Arr))
	for _, e := range r.Arr[1].Arr {
		if e.Kind != '$' || e.Nil {
			return "", nil, false
		}
		names = append(names, e.Str)
	}
	return r.Arr[0].Str, names, true
}

// SetHold sets the integrity hold of the current epoch with reason (STA-9), as rl_lib through
// hold.lua (KEYS rl:epoch; ARGV the epoch id read_epoch shows and reason; plan S7 step 11), the
// script an instance runs when it sees the store go back in time (§A5.7). It fails the test
// unless the reply is code 40 with value 0: the hold set, or one already set and left
// unchanged.
func (d *Driver) SetHold(reason string) {
	d.t.Helper()
	e := d.ReadEpoch()
	if e.ID == "" {
		d.t.Fatalf("driver: SetHold: no readable epoch record (read_epoch %d %q)", e.State, e.Reason)
		return
	}
	c, err := d.st.Conn("rl_lib")
	if err != nil {
		d.t.Fatalf("driver: SetHold: %v", err)
		return
	}
	defer func() { _ = c.Close() }()
	rep, ok := d.scriptOn(c, "rl_lib", scripts.Hold, []string{keys.EpochKey}, []string{e.ID, reason})
	if !ok {
		return
	}
	if rep.Code != 40 || elemInt(rep, 3) != 0 {
		d.t.Fatalf("driver: hold as rl_lib: code %d %q, want 40", rep.Code, rep.Reason)
	}
}

// LoadScripts loads the seven scripts rl_lib runs (§A2) into the store's script cache, as
// rl_lib (SCRIPT LOAD on a connection of the driver's own, which no send observer hears), and
// fails the test unless the store confirms each SHA. A test calls it before it arms a fault on
// an instance's next calls of a script that may not be loaded yet: the store wrapper takes a
// fault when a call enters, and a cleanup_entry step that gets NOSCRIPT is resent past it
// (§A5.4), so the fault would meet the NOSCRIPT round instead of the step.
func (d *Driver) LoadScripts() {
	d.t.Helper()
	c, err := d.st.Conn("rl_lib")
	if err != nil {
		d.t.Fatalf("driver: LoadScripts: %v", err)
		return
	}
	defer func() { _ = c.Close() }()
	for _, s := range []scripts.Script{
		scripts.Acquire, scripts.Refund, scripts.DueEntries, scripts.CleanupEntry, scripts.Reclaim,
		scripts.ReadEpoch, scripts.Hold,
	} {
		r, err := c.Do("SCRIPT", "LOAD", s.Text)
		if err != nil || r.Kind != '$' || r.Str != s.SHA {
			d.t.Fatalf("driver: LoadScripts: SCRIPT LOAD of %s: %v %s, want its SHA", s.Name, err, r)
			return
		}
	}
}

// swapConn is a driver connection that RestartStore replaces: the clock writes through one.
type swapConn struct {
	mu sync.Mutex
	c  *storeproc.Client
}

// Do sends one command on the current connection.
func (s *swapConn) Do(args ...string) (storeproc.Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return storeproc.Reply{}, errors.New("driver: connection closed")
	}
	return s.c.Do(args...)
}

// swap closes the current connection and puts c in its place.
func (s *swapConn) swap(c *storeproc.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		_ = s.c.Close()
	}
	s.c = c
}

// Close closes the current connection.
func (s *swapConn) Close() error {
	s.swap(nil)
	return nil
}

// RestartStore restarts the store process with its data kept (storeproc Restart: SIGTERM, a
// grace period, then SIGKILL; the same port, directory, AOF and ACL file; AGENTS.md §2), waits
// until it has loaded its data, then reconnects the driver's own connections (the clock's and
// the admin, ops and fixture ones). The instances reconnect by themselves (§A5.4); the restarted
// store has an empty script cache.
func (d *Driver) RestartStore() {
	d.t.Helper()
	if err := d.st.Restart(true); err != nil {
		d.t.Fatalf("driver: %v", err)
		return
	}
	d.reconnect()
}

// storeReadyWait bounds ReconnectStore's wait for a restarted store to load its data (plan
// choice: storeproc's own wait after Restart(true)).
const storeReadyWait = 30 * time.Second

// ReconnectStore is the second half of RestartStore, for a test that restarted the store itself
// without waiting for its data (Store().Restart(false), to watch it answer LOADING, plan S9
// Tests (b)): it waits up to storeReadyWait until the store has loaded its data, then
// reconnects the driver's own connections.
func (d *Driver) ReconnectStore() {
	d.t.Helper()
	if err := d.st.WaitReady(storeReadyWait); err != nil {
		d.t.Fatalf("driver: %v", err)
		return
	}
	d.reconnect()
}

// reconnect replaces the driver's own connections (the clock's and the admin, ops and fixture
// ones) with new ones to the running store.
func (d *Driver) reconnect() {
	d.t.Helper()
	var conns [3]*storeproc.Client // the clock's, admin, fixture
	for i, user := range []string{"rl_fixture", "rl_admin", "rl_fixture"} {
		c, err := d.st.Conn(user)
		if err != nil {
			for _, o := range conns[:i] {
				_ = o.Close()
			}
			d.t.Fatalf("driver: reconnecting as %s after the restart: %v", user, err)
			return
		}
		conns[i] = c
	}
	d.clockConn.swap(conns[0])
	d.swapAdmin(conns[1])
	d.swapOps()
	d.swapFixture(conns[2])
}

// swapAdmin closes the admin connection and puts c in its place.
func (d *Driver) swapAdmin(c *storeproc.Client) {
	d.adminMu.Lock()
	defer d.adminMu.Unlock()
	_ = d.admin.Close()
	d.admin = c
}

// swapOps closes the ops connection, if open: Info opens a new one.
func (d *Driver) swapOps() {
	d.opsMu.Lock()
	defer d.opsMu.Unlock()
	if d.ops != nil {
		_ = d.ops.Close()
		d.ops = nil
	}
}

// swapFixture puts c in place of the Fixture connection, if one is open, else closes c.
func (d *Driver) swapFixture(c *storeproc.Client) {
	d.fixMu.Lock()
	defer d.fixMu.Unlock()
	f := d.fixture
	if f == nil {
		_ = c.Close()
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.c.Close()
	f.c = c
}
