//go:build rltest

package driver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/keys"
	"ratelimiter/limiter"
)

// scanJSON and stepJSON are the op log's cleanup_scan and cleanup records as the driver writes
// them.
func scanJSON(tUs int64) string { return fmt.Sprintf(`{"type":"cleanup_scan","t_us":%d}`, tUs) }

func stepJSON(enc string, tUs, code int64) string {
	return fmt.Sprintf(`{"type":"cleanup","key_b64url":%q,"t_us":%d,"code":%d}`, enc, tUs, code)
}

// allowedOp acquires key on instance i and fails the test unless it is allowed.
func allowedOp(t *testing.T, d *Driver, i int, key string) Op {
	t.Helper()
	op := d.Acquire(i, key)
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire of %q: %v (%+v), want allowed", key, op.Kind, op.Diag)
	}
	return op
}

// TestDriverCleanupRecords (plan S7 step 11; orchestrator decision, S7 task 2): keys a, b and
// c, one allow each at the frozen start t0 = 1,760,000,000,000,000 µs (done), are eligible at
// E = t0 + 6 s (full, receipts expired, no marker), and score E in ix, so a pass handles them
// in member order YQ, Yg, Yw. With the scripts loaded (LoadScripts), a pass at E, with b's step
// reply dropped and c's step refused by faults keyed to them, stops outcome_unknown with
// Removed 1. The op log then ends with
// cleanup_scan {E} and cleanup {YQ, E, 30} and {Yg, E, 30}: b's step ran, its reply lost behind
// the capture; c, which never ran, has no record; CleanupSteps lists a's and b's steps with
// their markers_removed 0. A second pass removes c and adds cleanup_scan {E}, cleanup {Yw, E,
// 30} and the empty last page's cleanup_scan {E}. The log replays; b's code made 31, or the
// second pass's cleanup_scan made 1 µs later, fails replay at that line.
func TestDriverCleanupRecords(t *testing.T) {
	d := fastDriver(t, InstanceSpec{})
	t0 := d.Clock().NowNs()
	for _, k := range []string{"a", "b", "c"} {
		d.Done(allowedOp(t, d, 0, k))
	}
	e := t0 + d.Policy().ReceiptLifetimeNs
	if err := d.Clock().Set(e); err != nil {
		t.Fatal(err)
	}
	eUs := e / 1000
	d.LoadScripts() // so the faults meet the steps, not a NOSCRIPT round
	for _, f := range []fixtures.Fault{
		{Mode: fixtures.DropReply, Match: "cleanup_entry", Key: "b", Count: 1},
		{Mode: fixtures.Refuse, Match: "cleanup_entry", Key: "c", Count: 1},
	} {
		if err := d.StoreWrapper(0).Arm(f); err != nil {
			t.Fatal(err)
		}
	}
	mark := len(d.OpLog())
	rep := d.RunCleanupPass(0)
	if rep.Status != "outcome_unknown" || rep.Removed != 1 || rep.Moved+rep.Quarantined+rep.Skipped != 0 {
		t.Errorf("pass with b's reply dropped and c refused: %+v, want outcome_unknown with Removed 1 alone", rep)
	}
	want := []string{scanJSON(eUs), stepJSON("YQ", eUs, 30), stepJSON("Yg", eUs, 30)}
	if got := d.OpLog()[mark:]; !slices.Equal(got, want) {
		t.Errorf("records of the first pass:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantSteps := []CleanupStep{
		{Instance: 0, Member: "YQ", Key: "a", Code: 30, TUs: eUs},
		{Instance: 0, Member: "Yg", Key: "b", Code: 30, TUs: eUs},
	}
	if got := d.CleanupSteps(); !slices.Equal(got, wantSteps) {
		t.Errorf("CleanupSteps after the first pass: %+v, want %+v", got, wantSteps)
	}

	mark = len(d.OpLog())
	if rep := d.RunCleanupPass(0); rep.Status != "complete" || rep.Removed != 1 || rep.Moved+rep.Quarantined+rep.Skipped != 0 {
		t.Errorf("second pass: %+v, want complete with Removed 1 alone", rep)
	}
	want = []string{scanJSON(eUs), stepJSON("Yw", eUs, 30), scanJSON(eUs)}
	if got := d.OpLog()[mark:]; !slices.Equal(got, want) {
		t.Errorf("records of the second pass:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := d.StepsFor("c"); len(got) != 1 || got[0].Code != 30 {
		t.Errorf("StepsFor(c): %+v, want one step with code 30", got)
	}
	d.CheckRefModel(t)

	d.mu.Lock()
	orig := slices.Clone(d.oplog)
	d.mu.Unlock()
	stepB := lastLine(t, d, `"key_b64url":"Yg","t_us"`)
	scan := mark // the second pass's first page read
	for _, c := range []struct {
		name string
		line int
		edit func(map[string]any)
		want string
	}{
		{"b's code 31", stepB, func(m map[string]any) { m["code"] = json.Number("31") },
			"line " + strconv.Itoa(stepB+1) + ": code: expected 30, actual 31"},
		{"cleanup_scan 1 µs later", scan, func(m map[string]any) { m["t_us"] = json.Number(strconv.FormatInt(eUs+1, 10)) },
			"line " + strconv.Itoa(scan+1) + ": t_us: expected " + strconv.FormatInt(eUs, 10)},
	} {
		editRecord(t, d, c.line, c.edit)
		f := &recTB{TB: t}
		d.CheckRefModel(f)
		_, fatals := f.reports()
		if len(fatals) != 1 || !strings.Contains(fatals[0], c.want) || !strings.Contains(fatals[0], "exit status 1") {
			t.Errorf("%s: bridge failures %q, want one naming %q with exit status 1", c.name, fatals, c.want)
		}
		d.mu.Lock()
		d.oplog = slices.Clone(orig)
		d.mu.Unlock()
	}
}

// TestDriverStoreKeysSetHold (plan S7 step 11): StoreKeys lists, sorted and each once, the
// keys a SCAN MATCH pattern finds as rl_lib, across SCAN pages: after one allow on each of
// 1,500 new keys, rl:* holds rl:ctl:time, rl:e:<id>:ix, the 1,500 k: keys and rl:epoch;
// rl:e:<id>:k:* the 1,500 alone; a pattern nothing matches, none. SetHold sets the integrity
// hold through hold.lua as rl_lib: Status shows its reason, the instance is not ready
// (integrity_hold) after an epoch read and refuses acquires with state_integrity_error, and a
// second SetHold leaves the first reason in place.
func TestDriverStoreKeysSetHold(t *testing.T) {
	d := New(t, Options{Instances: 1})
	const n = 1500
	id := d.ReadEpoch().ID
	want := []string{keys.CtlTimeKey, keys.IndexKey(id), keys.EpochKey}
	for k := range n {
		key := fmt.Sprintf("sk-%04d", k)
		d.Done(allowedOp(t, d, 0, key))
		enc, err := keys.Encode(key)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, keys.BucketKey(id, enc))
	}
	slices.Sort(want)
	if got := d.StoreKeys("rl:*"); !slices.Equal(got, want) {
		t.Errorf("StoreKeys(rl:*): %d keys, want %d (rl:ctl:time, rl:epoch, ix and %d k: keys)", len(got), len(want), n)
	}
	if got := d.StoreKeys("rl:e:" + id + ":k:*"); len(got) != n || !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != n {
		t.Errorf("StoreKeys(k:*): %d keys, want %d, sorted, each once", len(got), n)
	}
	if got := d.StoreKeys("rl:e:" + id + ":mi:*"); len(got) != 0 {
		t.Errorf("StoreKeys(mi:*): %q, want none", got)
	}

	d.SetHold("store_rollback")
	if st := d.Status(); st.Code != 40 || st.Hold != "store_rollback" {
		t.Errorf("Status after SetHold: %+v, want hold store_rollback", st)
	}
	deadline := time.Now().Add(2*time.Duration(d.inst.EpochRereadIntervalNs) + time.Second)
	for h := d.Limiter(0).Health(); h.Reason != "integrity_hold"; h = d.Limiter(0).Health() {
		if time.Now().After(deadline) {
			t.Fatalf("after SetHold: health %s/%s, want not_ready/integrity_hold", h.Status, h.Reason)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Through the limiter itself; TestDriverHoldAcquireLogged acquires through the driver.
	if r := d.Limiter(0).Acquire(t.Context(), "sk-0000"); r.Kind != limiter.StateIntegrityError || r.Permit != nil {
		t.Errorf("acquire under the hold: %v, want state_integrity_error without a permit", r.Kind)
	}
	d.SetHold("other")
	if st := d.Status(); st.Hold != "store_rollback" {
		t.Errorf("Status after a second SetHold: hold %q, want store_rollback kept", st.Hold)
	}
}

// TestDriverHoldAcquireLogged (orchestrator decision, S7 task 2: state_integrity_error is a
// state-neutral acquire result of the op-log grammar, legal anywhere, with deduction_id and
// epoch_id empty): after an allow on key h (done), SetHold sets the integrity hold. An acquire
// of h through the driver right after it (refused by the store's hold check, or by the
// instance once an epoch read shows the hold) and one once health shows integrity_hold both
// return state_integrity_error without a permit, and each is logged as an acquire record of h
// (aA) with that result and deduction_id and epoch_id empty. The test is not exempt from BUD-11
// (SetHold is no Fixture write), and the teardown's BUD-11 check, run here on the same op log
// and again at teardown, accepts it.
func TestDriverHoldAcquireLogged(t *testing.T) {
	d := fastDriver(t, InstanceSpec{})
	d.Done(allowedOp(t, d, 0, "h"))
	d.SetHold("store_rollback")
	refused := func(when string) {
		t.Helper()
		op := d.Acquire(0, "h")
		if op.Kind != limiter.StateIntegrityError || op.Permit != nil {
			t.Fatalf("acquire %s: %v (%+v), want state_integrity_error without a permit", when, op.Kind, op.Diag)
		}
		lines := d.OpLog()
		var rec acquireRecord
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Type != "acquire" || rec.KeyB64URL != "aA" || rec.Result != "state_integrity_error" || rec.DeductionID != "" || rec.EpochID != "" {
			t.Errorf("record of the acquire %s: %s, want an acquire of aA with result state_integrity_error and deduction_id = epoch_id = ''", when, lines[len(lines)-1])
		}
	}
	refused("right after SetHold")
	deadline := time.Now().Add(2*time.Duration(d.inst.EpochRereadIntervalNs) + time.Second)
	for h := d.Limiter(0).Health(); h.Reason != "integrity_hold"; h = d.Limiter(0).Health() {
		if time.Now().After(deadline) {
			t.Fatalf("after SetHold: health %s/%s, want not_ready/integrity_hold", h.Status, h.Reason)
		}
		time.Sleep(5 * time.Millisecond)
	}
	refused("under integrity_hold")
	d.mu.Lock()
	exempt, logged := len(d.damaged) > 0 || d.wiped, d.logged
	d.mu.Unlock()
	if exempt || logged != 4 {
		t.Fatalf("BUD-11 exemption %v with %d ops logged, want no exemption and 4 (allow, done, 2 refused acquires)", exempt, logged)
	}
	rec := &recTB{TB: t}
	d.checkBud11(rec)
	if errs, fatals := rec.reports(); len(errs)+len(fatals) != 0 {
		t.Errorf("BUD-11 check of the op log with state_integrity_error acquires: errors %q, fatals %q; want none", errs, fatals)
	}
}

// TestDriverRestartStore (plan S7 step 11; AGENTS.md §2): RestartStore restarts the store
// process (a new PID) on the same port with its data kept (the epoch, the bucket of key r and
// the controlled time), and the driver's clock, admin, ops and fixture connections work after
// it; the instance serves again (the restarted store has no scripts loaded: its next acquire
// reloads through NOSCRIPT).
func TestDriverRestartStore(t *testing.T) {
	d := New(t, Options{Instances: 1})
	d.Done(allowedOp(t, d, 0, "r"))
	before := d.Inspect("r")
	epoch := d.ReadEpoch()
	_ = d.Info("server") // the ops connection is open from here on
	fx := d.Fixture()
	pid, port := d.Store().PID, d.Store().Port
	d.RestartStore()
	if d.Store().PID == pid || d.Store().Port != port {
		t.Errorf("after RestartStore: pid %d (was %d), port %d (was %d); want a new process on the same port", d.Store().PID, pid, d.Store().Port, port)
	}
	if e := d.ReadEpoch(); e.ID != epoch.ID || e.HWM != epoch.HWM || e.ClockUs != epoch.ClockUs {
		t.Errorf("epoch after the restart: %+v, want %+v (data kept)", e, epoch)
	}
	if got := d.Inspect("r"); !reflect.DeepEqual(got, before) || got.Present != 1 {
		t.Errorf("bucket r after the restart: %+v, want %+v", got, before)
	}
	if v := d.Info("server")["tcp_port"]; v != strconv.Itoa(port) {
		t.Errorf("INFO server after the restart: tcp_port %q, want %d", v, port)
	}
	if r, err := fx.Do("GET", keys.CtlTimeKey); err != nil || r.Str != strconv.FormatInt(d.Clock().NowNs()/1000, 10) {
		t.Errorf("Fixture GET rl:ctl:time after the restart: %v %v, want the controlled time", r, err)
	}
	if err := d.Clock().Advance(1_000_000); err != nil {
		t.Errorf("clock after the restart: %v", err)
	}
	if err := d.WaitReady(0, readyWait); err != nil {
		t.Fatal(err)
	}
	var op Op
	deadline := time.Now().Add(readyWait)
	for op = d.Acquire(0, "r"); op.Kind != limiter.Allowed && time.Now().Before(deadline); op = d.Acquire(0, "r") {
		time.Sleep(20 * time.Millisecond) // the call connections may still be reconnecting
	}
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire after the restart: %v (%+v), want allowed", op.Kind, op.Diag)
	}
	d.Done(op)
}
