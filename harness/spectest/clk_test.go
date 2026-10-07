//go:build rltest

package spectest

import (
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// busyEval busy-waits 300 ms of CPU time inside the store and calls no command: rl_fixture
// scripts cannot call SET, so the clock step follows it as a plain SET in the same pipeline.
const busyEval = "local s = os.clock() while os.clock() - s < 0.3 do end return 1"

// TestT_CLK_1 (SPEC T-CLK-1; CLK-1, CLK-2, API-3, API-1g): with the clock at T1, one rl_fixture
// write pipelines a 300 ms busy EVAL and SET rl:ctl:time T2; an acquire sent 50 ms later is
// written before the EVAL can reply, waits behind it and the buffered SET, and decides at T2:
// the reading taken inside the atomic step. One acquire of another key loads the acquire
// script first: on the driver's fresh store the first EVALSHA gets NOSCRIPT, and the
// adapter's resend would be written only after the EVAL and the SET replied. So the timed
// acquire saw no NOSCRIPT and wrote one EVALSHA, the one written before the EVAL replied.
// Health reports the clock source: controlled, and store for an added store-clock instance.
func TestT_CLK_1(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 1})
	warm := d.Acquire(0, "clk-warm")
	if warm.Kind != limiter.Allowed {
		t.Fatalf("warm-up acquire: %v, want allowed", warm.Kind)
	}
	warm.Permit.Done()
	t1 := d.Clock().NowNs()
	t2 := t1 + 7_000_000_000
	type piped struct {
		replies []storeproc.Reply
		err     error
		at      time.Time
	}
	ch := make(chan piped, 1)
	fx := d.Fixture()
	sent := time.Now() // the pipeline is written after this instant
	go func() {
		rs, err := fx.Pipeline([][]string{
			{"EVAL", busyEval, "0"},
			{"SET", keys.CtlTimeKey, strconv.FormatInt(t2/1000, 10)},
		})
		ch <- piped{rs, err, time.Now()}
	}()
	time.Sleep(50 * time.Millisecond)
	n := len(d.Sent(0))
	op := d.Acquire(0, "clk")
	defer doneAll([]driver.Op{op})
	var p piped
	select {
	case p = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the pipelined EVAL and SET did not return within 10s")
	}
	if p.err != nil || len(p.replies) != 2 || p.replies[0].Kind != ':' || p.replies[1].Str != "OK" {
		t.Fatalf("pipeline: %v %v, want the EVAL's 1 and OK", p.replies, p.err)
	}
	if op.Kind != limiter.Allowed || op.DecisionNs != t2 {
		t.Errorf("acquire: %v at %d, want allowed at T2 = %d (T1 = %d)", op.Kind, op.DecisionNs, t2, t1)
	}
	if op.NoScript || op.SentCommands != 1 {
		t.Errorf("acquire: NoScript %v, %d acquire commands written; want no NOSCRIPT and one EVALSHA", op.NoScript, op.SentCommands)
	}
	var written []time.Time
	for _, s := range d.Sent(0)[n:] {
		if s.Cmd == "acquire" && s.Bytes > 0 {
			written = append(written, s.At)
		}
	}
	// The EVAL replies at least 300 ms after the pipeline was written.
	if len(written) != 1 || !written[0].Before(sent.Add(300*time.Millisecond)) || !written[0].Before(p.at) {
		after := make([]time.Duration, len(written))
		for j, w := range written {
			after[j] = w.Sub(sent)
		}
		t.Errorf("acquire written at %v after the pipeline (EVAL reply by %v), want once, before the EVAL could reply (300ms)",
			after, p.at.Sub(sent))
	}
	if h := d.Limiter(0).Health(); h.ClockSource != "controlled" {
		t.Errorf("health clock_source %q, want controlled", h.ClockSource)
	}
	sp := d.Policy()
	sp.ClockSource = limiter.Store
	i, err := d.AddInstance(driver.InstanceSpec{Policy: &sp})
	if err != nil {
		t.Fatal(err)
	}
	if h := d.Limiter(i).Health(); h.ClockSource != "store" {
		t.Errorf("store-clock instance: health clock_source %q, want store", h.ClockSource)
	}
}

// TestT_CLK_2 (SPEC T-CLK-2; CLK-4, CLK-5): R1 and R2 on K at a frozen t0 expire at E = t0 +
// 6 s. R1's release is applied at t0 with its reply dropped, and its retry is held at a
// barrier. At E + 1 s a pass removes K with R1's marker (step 30, markers_removed 1). The clock
// goes back to E − 1 s, 2 s below the high-water mark E + 1 s. R1's retry is then
// receipt_expired (its budget time is the high-water mark, past E: the expired receipt stays
// expired with its marker gone), and so is R2's release (same E). Later acquires of K decide
// at the high-water mark: a new bucket with exactly 20 allows at decision_ns = E + 1 s, as the
// reference model gives (no extra allowance). After the next epoch read
// rl_clock_lag_microseconds ≥ 1,900,000 (hwm − clock = 2 s).
func TestT_CLK_2(t *testing.T) {
	d := fastDriver(t, 2, nil)
	d.LoadScripts() // the faults meet R1's refund calls, not a NOSCRIPT round
	const key = "clk2"
	r1, r2 := allowed(t, d, 0, key), allowed(t, d, 0, key)
	e := expiryOf(r1) * 1000
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.BeforeSend, Name: "r1"})
	d.Release(r1)
	reached(t, d, 0, "r1")
	if as := d.RefundAttempts(r1); len(as) != 1 || !as[0].Reached || as[0].Store.Outcome != store.OK || as[0].Store.Reply.Code != 3 {
		t.Fatalf("R1's first refund: %+v, want applied at the store with its reply lost", as)
	}
	if in := d.Inspect(key); in.MC != 1 {
		t.Fatalf("K before the pass: %+v, want R1's marker", in)
	}

	hwm := e + second
	setClock(t, d, hwm)
	if rep := passOK(t, d, 1); rep.Removed != 1 {
		t.Errorf("pass at E + 1 s: %+v, want Removed 1", rep)
	}
	if steps := d.StepsFor(key); len(steps) != 1 || steps[0].Code != 30 || steps[0].Markers != 1 {
		t.Fatalf("K's steps %+v, want one 30 removing R1's marker", steps)
	}

	setClock(t, d, e-second)
	d.StoreWrapper(0).Release("r1")
	if ev := event(t, d, r1); ev.Result != limiter.ReceiptExpired {
		t.Errorf("R1's retry after the regression: %+v, want receipt_expired", ev)
	}
	releaseExpired(t, d, r2)
	ops := burst(t, d, key, 20)
	for _, op := range ops {
		if op.DecisionNs != hwm {
			t.Errorf("acquire after the regression decided at %d, want the high-water mark %d", op.DecisionNs, hwm)
			break
		}
	}
	doneAll(ops)
	waitMetric(t, d, 0, "rl_clock_lag_microseconds", "", epochReads(d), "after the regression", func(v float64) bool { return v >= 1_900_000 })
	d.CheckRefModel(t)
}

// TestT_CLK_3 (SPEC T-CLK-3; CLK-7, STA-9, CFG-6; FLT-11): with an open permit on the key, the
// high-water mark is deleted (DeleteHwm), or separately corrupted to "x", and the clock steps
// back 2 s. An acquire then gets the epoch-wide state_integrity_error hwm_missing (or
// hwm_malformed), never degraded; health turns not_ready (state_lost); status shows hwm empty,
// and the record still holds no hwm (or "x"): never re-initialized. The permit's release gets
// code 22 and credits nothing: the key's inspect is unchanged.
func TestT_CLK_3(t *testing.T) {
	cases := []struct {
		name, reason string
		damage       func(tm *fixtures.Tamper) error
		stored       func(fields map[string]string) bool // the record's hwm after the refusals
	}{
		{"deleted", "hwm_missing", func(tm *fixtures.Tamper) error { return tm.DeleteHwm() },
			func(f map[string]string) bool { _, ok := f["hwm"]; return !ok }},
		{"corrupted", "hwm_malformed", func(tm *fixtures.Tamper) error { return tm.CorruptField(keys.EpochKey, "hwm", "x") },
			func(f map[string]string) bool { return f["hwm"] == "x" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := fastDriver(t, 1, nil)
			const key = "clk3"
			open := allowed(t, d, 0, key)
			in0 := d.Inspect(key)
			if err := c.damage(d.Tamper()); err != nil {
				t.Fatal(err)
			}
			if err := d.Clock().Back(2 * second); err != nil {
				t.Fatal(err)
			}
			refusedIntegrity(t, "acquire after the clock went back", d.Acquire(0, key), "epoch", c.reason)
			if err := waitHealth(d.Limiter(0), "not_ready", "state_lost", time.Now().Add(epochReads(d))); err != nil {
				t.Error(err)
			}
			d.Release(open)
			storeReply(t, "refund", firstAttempt(t, d, open), 22, c.reason)
			if st := d.Status(); st.Code != 40 || st.HWM != -1 {
				t.Errorf("status: %+v, want hwm shown empty", st)
			}
			if f := d.Dump()[keys.EpochKey].Fields; !c.stored(f) {
				t.Errorf("record after the refusals: hwm %q (present %v), want it not re-initialized", f["hwm"], f["hwm"] != "")
			}
			if in := d.Inspect(key); !reflect.DeepEqual(in, in0) {
				t.Errorf("key after the refund: %+v, want unchanged %+v (no credit)", in, in0)
			}
			d.Kill(0) // the refund retries until its retention timer
		})
	}
}

// TestT_CLK_4 (SPEC T-CLK-4; CLK-8, API-7a, API-1f, CMP-4): rl:ctl:time set to "abc", to "-5"
// (raw SET as rl_fixture) or deleted, each on its own driver. An acquire gets clock_error, a
// release's refund calls get code 21 (the per-attempt capture), a cleanup pass stops with status
// clock_error, and rladmin start-epoch --replace --expect-epoch <current> exits 4 (without the
// flag the readable record refuses first: exit 2). Nothing changes: status (hwm included), the
// key's inspect and the epoch record stay as they were. Once the clock is readable again the
// refund applies.
func TestT_CLK_4(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  []string
	}{
		{"unreadable", []string{"SET", keys.CtlTimeKey, "abc"}},
		{"negative", []string{"SET", keys.CtlTimeKey, "-5"}},
		{"missing", []string{"DEL", keys.CtlTimeKey}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := fastDriver(t, 1, nil)
			const key = "clk4"
			t0 := d.Clock().NowNs()
			d.Done(allowed(t, d, 0, key))
			open := allowed(t, d, 0, key)
			st0, in0, rec0 := d.Status(), d.Inspect(key), d.Dump()[keys.EpochKey]
			if r, err := d.Fixture().Do(c.cmd...); err != nil || r.Kind == '-' {
				t.Fatalf("%v: %v %s", c.cmd, err, r)
			}
			if op := d.Acquire(0, key); op.Kind != limiter.ClockError || op.Permit != nil {
				t.Errorf("acquire: %v %+v, want clock_error", op.Kind, op.Diag)
			}
			d.Release(open)
			storeReply(t, "refund", firstAttempt(t, d, open), 21, "clock")
			if rep := d.RunCleanupPass(0); rep.Status != "clock_error" {
				t.Errorf("pass: %+v, want status clock_error", rep)
			}
			adminCode(t, "start-epoch --replace", d.RLAdmin("start-epoch", "--replace", "--expect-epoch", st0.EpochID), 4)
			adminCode(t, "start-epoch without the flag", d.RLAdmin("start-epoch"), 2)
			if st, in, rec := d.Status(), d.Inspect(key), d.Dump()[keys.EpochKey]; st != st0 || !reflect.DeepEqual(in, in0) || !reflect.DeepEqual(rec, rec0) {
				t.Errorf("after the clock errors: status %+v, inspect %+v, record %v; want unchanged %+v, %+v, %v", st, in, rec, st0, in0, rec0)
			}
			setClock(t, d, t0)
			if ev := event(t, d, open); ev.Result != limiter.Applied {
				t.Errorf("refund once the clock is readable: %+v, want applied", ev)
			}
		})
	}
}

// clk5Policy is T-CLK-5's deployment (plan S8 Tests (b) choice): the store clock, receipt
// lifetime 4 s, refund deadline 2 s, outage tolerance 0.5 s and timed passes off; the instance
// has a max release delay of 0.5 s, a margin of 0.5 s, D 200 ms, refund backoff 50 → 100 ms and
// 128 refund workers for 128 pending refunds. RCP-8: 0.5 + 2 + 0.5 + 0.5 = 3.5 s ≤ 4 s; §A5.6:
// 0.1 + 1 + (1 + 3) × 0.2 = 1.9 s ≤ 2 s.
func clk5Policy() (limiter.Policy, limiter.Instance) {
	p := driver.FastPolicy()
	p.ClockSource = limiter.Store
	p.ReceiptLifetimeNs, p.RefundDeadlineNs, p.OutageToleranceNs = 4_000_000_000, 2_000_000_000, 500_000_000
	in := driver.FastInstance(p)
	in.MaxReleaseDelayNs, in.MarginNs = 500_000_000, 500_000_000
	in.DependencyDeadlineNs = 200_000_000
	in.RefundBackoffInitialNs, in.RefundBackoffMaxNs = 50_000_000, 100_000_000
	in.MaxHeldReceipts, in.MaxPendingRefunds, in.RefundWorkers = 128, 128, 128
	return p, in
}

// clk5Uptime is the store uptime T-CLK-5 waits for before the restart: 10 s, or RL_CLK5_UPTIME
// (a Go duration) when it is longer.
func clk5Uptime(t *testing.T) time.Duration {
	t.Helper()
	up := 10 * time.Second
	if v := os.Getenv("RL_CLK5_UPTIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("RL_CLK5_UPTIME %q: %v", v, err)
		}
		up = max(up, d)
	}
	return up
}

// storeTimeUs waits up to waitMax until the store clock reads at least us (read_epoch's
// clock_us) and fails the test otherwise.
func storeTimeUs(t *testing.T, d *driver.Driver, us int64) {
	t.Helper()
	deadline := time.Now().Add(waitMax)
	for d.ReadEpoch().ClockUs < us {
		if !time.Now().Before(deadline) {
			t.Fatalf("the store clock did not reach %d µs within %v", us, waitMax)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestT_CLK_5 (SPEC T-CLK-5; CLK-3), durable profile (checked in the store: durableProfile),
// store clock (clk5Policy). A warm-up allow on another key loads the acquire script and makes
// the first fsynced write. With at least 10 s of store uptime (RL_CLK5_UPTIME lengthens it), the
// key is emptied (20 allows) and two of its permits are released and confirmed, leaving two
// markers that outlive the restart; then the store restarts with its data kept. Budget time continues from the same origin: the clock
// lag (hwm − clock) is at most 100,000 µs (read_epoch right after the restart, and the
// instance's gauge after an epoch read); after 2 s idle the refilled key gives 20 allows, the
// first deciding after every pre-restart decision; a pass after the markers' expiry removes both
// (no marker or marker index entry left, the bucket kept: its new receipts are unexpired), and a
// pass after the bucket's eligibility removes it. The durable profile fsyncs every write, so
// calls that end on the transport alone are sent again (acquireSettled, passSettled).
func TestT_CLK_5(t *testing.T) {
	p, in := clk5Policy()
	d := driver.New(t, driver.Options{Instances: 1, Policy: p, Instance: in, Profile: storeproc.Durable})
	up := time.Now().Add(clk5Uptime(t)) // the store started before New returned
	durableProfile(t, d)
	const key = "clk5"
	d.Done(allowedSettled(t, d, 0, "clk5-warm"))
	time.Sleep(time.Until(up.Add(-time.Second)))
	var ops []driver.Op
	for range 20 {
		ops = append(ops, allowedSettled(t, d, 0, key))
	}
	for _, op := range ops[2:] {
		d.Done(op)
	}
	var markerExpiry int64
	for _, op := range ops[:2] {
		releaseConfirmed(t, d, op)
		markerExpiry = max(markerExpiry, expiryOf(op))
	}
	if in := d.Inspect(key); in.MC != 2 {
		t.Fatalf("before the restart: %+v, want the two markers", in)
	}
	before := ops[19].DecisionNs
	time.Sleep(time.Until(up))
	d.Store().Stop()
	d.RestartStore()
	if e := d.ReadEpoch(); e.HWM-e.ClockUs > 100_000 {
		t.Errorf("after the restart: hwm %d − clock %d = %d µs, want at most 100,000", e.HWM, e.ClockUs, e.HWM-e.ClockUs)
	}
	time.Sleep(2 * time.Second) // idle: the key refills to capacity (10 tokens/s, from 2 tokens)
	waitMetric(t, d, 0, "rl_clock_lag_microseconds", "", epochReads(d), "after the restart", func(v float64) bool { return v <= 100_000 })
	var after []driver.Op
	for k := range 20 {
		op := acquireSettled(t, d, 0, key)
		if op.Kind != limiter.Allowed {
			t.Fatalf("allow %d after 2 s idle: %v %+v, want allowed", k+1, op.Kind, op.Diag)
		}
		after = append(after, op)
	}
	doneAll(after)
	if after[0].DecisionNs <= before {
		t.Errorf("first decision after the restart at %d ns, want after the last one before it, %d", after[0].DecisionNs, before)
	}
	storeTimeUs(t, d, markerExpiry)
	passSettled(t, d, 0)
	if in := d.Inspect(key); in.Present != 1 || in.MC != 0 || len(in.Expiries) != 0 {
		t.Errorf("after the pass past the markers' expiry: %+v, want the bucket with no marker left", in)
	}
	storeTimeUs(t, d, expiryOf(after[19])+1_000_000) // x passed, refilled long before
	passSettled(t, d, 0)
	if in := d.Inspect(key); in.Present != 0 {
		t.Errorf("after the pass past the bucket's eligibility: %+v, want it removed", in)
	}
}
