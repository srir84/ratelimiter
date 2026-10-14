//go:build rltest

package spectest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// evalshaCalls is the evalsha call count of INFO commandstats, read as rl_ops.
func evalshaCalls(t *testing.T, d *driver.Driver) int64 {
	t.Helper()
	line := d.Info("commandstats")["cmdstat_evalsha"] // calls=N,usec=…
	for _, f := range strings.Split(line, ",") {
		if v, ok := strings.CutPrefix(f, "calls="); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	return 0
}

// sentSince returns, per instance, the send records after the marks.
func sentSince(d *driver.Driver, marks []int) [][]driver.Send {
	out := make([][]driver.Send, len(marks))
	for i, m := range marks {
		out[i] = d.Sent(i)[m:]
	}
	return out
}

// marks returns the current record count of every instance.
func marks(d *driver.Driver) []int {
	out := make([]int, d.Instances())
	for i := range out {
		out[i] = len(d.Sent(i))
	}
	return out
}

// TestDesign_CommandBudget (DESIGN §C4 command budgets; plan S5): epoch re-read and cleanup
// every hour, so nothing timed sends. On the fresh store, calls one at a time each send at
// most 2 acquire and 1 SCRIPT LOAD, and a second acquire only when the store wrapper saw
// NoScript on that response (the very first acquire does). With the scripts warm, N acquires
// (allowed and limited) send exactly N acquire, no SCRIPT LOAD, and raise INFO commandstats'
// evalsha count by N; local refusals (validation, cancelled, busy) send nothing; a Done()
// window and an idle window (1 s each, plan choice) send no acquire or refund.
func TestDesign_CommandBudget(t *testing.T) {
	const hour = 3_600_000_000_000
	p := driver.DefaultPolicy()
	p.CleanupIntervalNs = hour
	in := driver.DefaultInstance(p)
	in.EpochRereadIntervalNs = hour
	in.MaxHeldReceipts, in.MaxPendingRefunds = 4, 4 // so busy is reachable (plan choice)
	d := driver.New(t, driver.Options{Instances: 3, Policy: p, Instance: in})

	// The fresh store: the first acquire of each instance, one at a time.
	var warm []driver.Op
	for i := range 3 {
		m := len(d.Sent(i))
		ns := d.StoreWrapper(i).NoScripts()
		op := d.Acquire(i, "warm")
		warm = append(warm, op)
		sent := d.Sent(i)[m:]
		acq, loads := driver.Count(sent, "acquire"), driver.Count(sent, "SCRIPT LOAD")
		sawNoScript := d.StoreWrapper(i).NoScripts() - ns
		if op.Kind != limiter.Allowed || acq < 1 || acq > 2 || loads > 1 || len(sent) != acq+loads {
			t.Errorf("instance %d, first acquire: %v, sent %v; want allowed, 1 or 2 acquire, at most 1 SCRIPT LOAD, nothing else", i, op.Kind, sent)
		}
		if (acq == 2) != (sawNoScript == 1 && op.NoScript) || loads != sawNoScript {
			t.Errorf("instance %d: %d acquire and %d SCRIPT LOAD with %d NoScript responses (op %v); a second acquire and a load only after NoScript",
				i, acq, loads, sawNoScript, op.NoScript)
		}
		if i == 0 && acq != 2 {
			t.Errorf("the first acquire on the fresh store sent %d acquire, want 2 (NOSCRIPT, load, resend)", acq)
		}
	}
	doneAll(warm)

	// Scripts warm: N acquires, allowed and limited, are exactly N sends.
	const n = 30
	m := marks(d)
	calls := evalshaCalls(t, d)
	kinds := map[limiter.ResultKind]int{}
	perInstance := make([]int, 3)
	for k := range n {
		op := d.Acquire(k%3, "budget")
		kinds[op.Kind]++
		perInstance[k%3]++
		doneAll([]driver.Op{op})
	}
	if kinds[limiter.Allowed] != 20 || kinds[limiter.Limited] != 10 {
		t.Errorf("results %v, want 20 allowed and 10 limited", kinds)
	}
	for i, sent := range sentSince(d, m) {
		if acq, loads := driver.Count(sent, "acquire"), driver.Count(sent, "SCRIPT LOAD"); acq != perInstance[i] || loads != 0 || len(sent) != acq {
			t.Errorf("instance %d: %d acquires sent %v, want exactly %d acquire and nothing else", i, perInstance[i], sent, perInstance[i])
		}
	}
	if delta := evalshaCalls(t, d) - calls; delta != n {
		t.Errorf("INFO commandstats evalsha delta %d, want %d", delta, n)
	}

	// Local refusals send nothing.
	var held []driver.Op
	for range 4 {
		op := d.Acquire(2, "held")
		if op.Kind != limiter.Allowed {
			t.Fatalf("filling the slots: %v", op.Kind)
		}
		held = append(held, op)
	}
	m = marks(d)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, c := range []struct {
		name string
		op   driver.Op
		want limiter.ResultKind
	}{
		{"validation", d.Acquire(0, ""), limiter.ValidationError},
		{"cancelled", d.AcquireContext(cancelled, 1, "k"), limiter.Cancelled},
		{"busy", d.Acquire(2, "k"), limiter.Busy},
	} {
		if c.op.Kind != c.want {
			t.Errorf("%s: %v, want %v", c.name, c.op.Kind, c.want)
		}
	}
	for i, sent := range sentSince(d, m) {
		if len(sent) != 0 {
			t.Errorf("instance %d: local refusals sent %v, want nothing", i, sent)
		}
	}

	// A Done() window and an idle window: no acquire or refund sends.
	for _, w := range []struct {
		name string
		do   func()
	}{
		{"Done()", func() { doneAll(held) }},
		{"idle", func() {}},
	} {
		m = marks(d)
		w.do()
		time.Sleep(time.Second)
		for i, sent := range sentSince(d, m) {
			if a, r := driver.Count(sent, "acquire"), driver.Count(sent, "refund"); a+r != 0 {
				t.Errorf("%s window, instance %d: sent %v, want no acquire or refund", w.name, i, sent)
			}
		}
	}
}

// goroutines samples runtime.NumGoroutine until two samples 20 ms apart agree (≤ 2 s).
func goroutines(t *testing.T) int {
	t.Helper()
	prev := runtime.NumGoroutine()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == prev {
			return n
		}
		prev = n
	}
	t.Fatalf("the goroutine count did not settle within 2s (last %d)", prev)
	return 0
}

// TestDesign_SeparateDeployments (DESIGN §C4, §A5.2; F32): two drivers in one process, each
// with its own store and policy (A: the default; B: capacity 40 at 3 tokens per 7 s), use the
// same key names. Interleaved sequential traffic matches each driver's own reference model.
// ReplaceEpoch on A (S8), whose instances join the new epoch within the join tolerance, leaves
// B's epoch (id, fp) unchanged, B's instances ready in it, and B's further results matching its
// model. While A's store is stopped (plan S9), B's acquires are only allowed or limited and
// still match B's model, and B's instances stay ready with an empty store_error; A's store then
// restarts with its data kept, and A's instances are ready with all their connections again
// (call 8, refund 2, maintenance 1), so A's teardown audit sees a live store. Closing all of
// A's instances leaves B's results matching its model, and the goroutine count returns to its
// value just before A's instances were built (A starts with none and adds 3, plan choice), so
// B's goroutines are untouched.
func TestDesign_SeparateDeployments(t *testing.T) {
	pb := driver.DefaultPolicy()
	pb.Capacity, pb.RateTokens, pb.RatePeriodNs = 40, 3, 7_000_000_000
	b := driver.New(t, driver.Options{Instances: 3, Policy: pb})
	a := driver.New(t, driver.Options{Instances: 0})
	before := goroutines(t)
	for range 3 {
		i, err := a.AddInstance(driver.InstanceSpec{})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.WaitReady(i, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"user:1", "user:2", "shared"}
	traffic := func(drivers []*driver.Driver, rounds int) {
		t.Helper()
		for k := range rounds {
			if k%9 == 8 {
				for _, d := range drivers {
					step(t, d, 250_000_000)
				}
			}
			for _, d := range drivers {
				op := d.Acquire(k%3, names[k%len(names)])
				if op.Kind != limiter.Allowed && op.Kind != limiter.Limited {
					t.Errorf("%v (%+v), want allowed or limited", op.Kind, op.Diag)
				}
				doneAll([]driver.Op{op})
			}
		}
	}
	traffic([]*driver.Driver{a, b}, 90)
	a.CheckRefModel(t)
	b.CheckRefModel(t)
	eb := b.ReadEpoch()
	start := time.Now()
	idA := a.ReplaceEpoch()
	waitAllJoined(t, a, idA, start.Add(joinTolerance(a)))
	if e := b.ReadEpoch(); e.ID != eb.ID || e.FP != eb.FP {
		t.Errorf("B's epoch after A's replace: %s %s, want %s %s", e.ID, e.FP, eb.ID, eb.FP)
	}
	for i := range 3 {
		if h := b.Limiter(i).Health(); h.Status != "ready" || h.EpochID != eb.ID {
			t.Errorf("B's instance %d after A's replace: %s/%s in %q, want ready in %s", i, h.Status, h.Reason, h.EpochID, eb.ID)
		}
	}
	traffic([]*driver.Driver{b}, 30)
	b.CheckRefModel(t)

	// A's store goes down (plan S9): B serves on, and its health shows no store error.
	down := true
	// Registered after both drivers' teardowns, so it runs first: a test that failed while A's
	// store was down starts it again for A's teardown audit.
	t.Cleanup(func() {
		if down {
			a.RestartStore()
		}
	})
	a.Store().Stop()
	traffic([]*driver.Driver{b}, 30)
	b.CheckRefModel(t)
	for i := range 3 {
		if h := b.Limiter(i).Health(); h.Status != "ready" || h.StoreError != "" {
			t.Errorf("B's instance %d while A's store is down: %s/%s, store_error %q; want ready with no store error", i, h.Status, h.Reason, h.StoreError)
		}
	}
	a.RestartStore() // Restart(true): data kept
	down = false
	restarted := time.Now()
	for i := range 3 {
		if err := a.WaitReady(i, waitMax); err != nil {
			t.Fatal(err)
		}
		for _, k := range []struct {
			kind string
			n    float64
		}{{"call", 8}, {"refund", 2}, {"maintenance", 1}} {
			waitMetric(t, a, i, "rl_store_connections", k.kind, time.Until(restarted.Add(waitMax)), "A's instance once its store is back",
				func(v float64) bool { return v == k.n })
		}
	}

	for i := range 3 {
		a.Close(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() != before {
		if time.Now().After(deadline) {
			t.Fatalf("after A closed: %d goroutines, want %d as before A's instances", runtime.NumGoroutine(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := goroutines(t); n != before {
		t.Errorf("after A closed the goroutine count settled at %d, want %d", n, before)
	}
	traffic([]*driver.Driver{b}, 60)
	b.CheckRefModel(t)
}

// TestDesign_RefundInequality (DESIGN §A5.6, §C4; plan S6 Tests (a)): at the harness instance
// values (N 1,152, 128 workers, D 2 s, backoff max 1 s), the startup check needs
// 1 + 1 + (⌈1,152 / 128⌉ + 3) × 2 = 26 s ≤ refund_deadline. With refund_deadline 25 s, New
// returns an error and the instance is permanently not ready (settings_invalid): Acquire is
// not_ready and nothing is ever written to the store. A deployment whose policy has
// refund_deadline 26 s joins and serves.
func TestDesign_RefundInequality(t *testing.T) {
	short := driver.DefaultPolicy()
	short.RefundDeadlineNs = 25_000_000_000
	d := driver.New(t, driver.Options{Policy: short})
	in := d.Instance()
	if in.MaxPendingRefunds != 1152 || in.RefundWorkers != 128 || in.DependencyDeadlineNs != 2_000_000_000 || in.RefundBackoffMaxNs != 1_000_000_000 {
		t.Fatalf("instance settings %+v, want the harness values (N 1,152, 128 workers, D 2 s, backoff max 1 s)", in)
	}
	if _, err := d.AddInstance(driver.InstanceSpec{}); err == nil {
		t.Error("New with refund_deadline 25 s: no error, want the §A5.6 inequality's")
	}
	if h := d.Limiter(0).Health(); h.Status != "not_ready" || h.Reason != "settings_invalid" {
		t.Errorf("health %s (%s), want not_ready (settings_invalid)", h.Status, h.Reason)
	}
	if op := d.Acquire(0, "k"); op.Kind != limiter.NotReady {
		t.Errorf("acquire: %v, want not_ready", op.Kind)
	}
	time.Sleep(interval(d)) // an epoch read would have come by now
	if sent := d.Sent(0); len(sent) != 0 {
		t.Errorf("the instance wrote %v, want nothing at all", sent)
	}

	enough := driver.DefaultPolicy()
	enough.RefundDeadlineNs = 26_000_000_000
	e := driver.New(t, driver.Options{Instances: 1, Policy: enough})
	op := e.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire with refund_deadline 26 s: %v (%+v), want allowed", op.Kind, op.Diag)
	}
	e.Done(op)
}

// mustEqualRuns compares two runs of the T-COR-1 workload op by op: kinds and results equal,
// units scaled by k, retry_after_ns equal, and every time (decision, credit, expiry, clock)
// shifted by deltaNs (expiry by deltaNs / 1000), except that a time of 0 (no credit time)
// stays 0.
func mustEqualRuns(t *testing.T, base, other []cor1Op, k, deltaNs int64) {
	t.Helper()
	if len(base) != len(other) {
		t.Fatalf("%d ops against %d", len(base), len(other))
	}
	shift := func(v, by int64) int64 {
		if v == 0 {
			return 0
		}
		return v + by
	}
	bad := 0
	for n, b := range base {
		want := cor1Op{
			kind: b.kind, result: b.result, units: b.units * k, retry: b.retry,
			decision: shift(b.decision, deltaNs), expiryUs: shift(b.expiryUs, deltaNs/1000), clockNs: shift(b.clockNs, deltaNs),
		}
		if other[n] != want {
			bad++
			if bad <= 10 {
				t.Errorf("op %d: %+v, want %+v (from %+v)", n, other[n], want, b)
			}
		}
	}
	if bad > 10 {
		t.Errorf("… %d ops differ in all", bad)
	}
}

// TestDesign_MetamorphicTimeShift (DESIGN §C4 metamorphic tests; plan S6 Tests (a)): for each
// T-COR-1 policy, the workload of one seed runs twice, each on a fresh store and epoch; the
// second has every controlled time and the epoch origin shifted by Δ = 4 × 10^15 µs (all
// values stay below TBOUND = 2^53 − 2^43 µs). Kinds, units, credits and retry_after_ns are
// equal; decision, credit and expiry times differ by exactly Δ; zero placeholders stay 0;
// epoch and deduction IDs (crypto/rand) are not compared.
func TestDesign_MetamorphicTimeShift(t *testing.T) {
	const deltaUs int64 = 4_000_000_000_000_000
	const tbound int64 = 1<<53 - 1<<43 // µs
	for _, pc := range cor1Policies() {
		t.Run(pc.name, func(t *testing.T) {
			_, base := runCOR1(t, 11, pc.p, 0)
			_, shifted := runCOR1(t, 11, pc.p, deltaUs*1000)
			var top int64 // µs
			for _, op := range shifted {
				top = max(top, op.clockNs/1000, op.decision/1000, op.expiryUs)
			}
			if top >= tbound {
				t.Fatalf("the shifted run reached %d µs, want every value below TBOUND %d µs", top, tbound)
			}
			mustEqualRuns(t, base, shifted, 1, deltaUs*1000)
		})
	}
}

// TestDesign_MetamorphicScale (DESIGN §C4 metamorphic tests; plan S6 Tests (a)): both T-COR-1
// policies, then each with rate_tokens and rate_period_ns × 7 (a fresh store and epoch: the
// fingerprint differs), within the §A3.1 bounds (7 · rate_tokens ≤ 2^31 − 1, 7 · cap_units <
// 2^52): the same workload seed gives the same kinds and retry_after_ns, and units and credits
// × 7; times are unchanged.
func TestDesign_MetamorphicScale(t *testing.T) {
	for _, pc := range cor1Policies() {
		t.Run(pc.name, func(t *testing.T) {
			scaled := pc.p
			scaled.RateTokens, scaled.RatePeriodNs = 7*pc.p.RateTokens, 7*pc.p.RatePeriodNs
			if scaled.RateTokens > 1<<31-1 || scaled.Capacity*scaled.RatePeriodNs >= 1<<52 {
				t.Fatalf("scaled policy %+v outside the §A3.1 bounds", scaled)
			}
			_, base := runCOR1(t, 12, pc.p, 0)
			_, big := runCOR1(t, 12, scaled, 0)
			mustEqualRuns(t, base, big, 7, 0)
		})
	}
}

// TestDesign_CleanupAfterStoreRestart (DESIGN §C4, §A5.4; plan S7 Tests (a)): 500 keys, one
// allow each at a frozen t0 (done), are eligible at t0 + 6 s. The store restarts with its data
// kept (storeproc Restart), so its script cache is empty. Once the instance reports ready, up
// to 3 RunCleanupPass calls run, each once the maintenance connection is ready, while a pass
// stops on the transport alone (unavailable or outcome_unknown with an empty Diag: no error
// reply, no reply that failed decoding, no reason; a reconnect race, plan choice): the last is
// complete, the Removed counts add up to 500 and the namespace is empty. No pass stops on
// NOSCRIPT, since the pipeline's reload resends each step that got it (§A5.4), and the
// completing pass itself sends at least one SCRIPT LOAD and more than 500 but at most 1,000
// cleanup_entry commands: every step went out, the steps that got NOSCRIPT once more after the
// reload, none more than twice. Over all the passes after the restart, the cleanup_entry
// sends are at most 1,000 (each step at most twice).
func TestDesign_CleanupAfterStoreRestart(t *testing.T) {
	d := fastDriver(t, 1, nil)
	const n = 500
	t0 := d.Clock().NowNs()
	for k := range n { // setup: a reply an AOF stall the store records delays past D is retried
		d.Done(allowedThroughAOFStall(t, d, 0, fmt.Sprintf("restart-%03d", k)))
	}
	setClock(t, d, t0+d.Policy().ReceiptLifetimeNs)
	mark := len(d.Sent(0))
	d.RestartStore()
	if err := d.WaitReady(0, waitMax); err != nil {
		t.Fatal(err)
	}
	var removed int64
	var reps []limiter.CleanupReport
	var last []driver.Send // the sends during the last pass
	for range 3 {
		waitMetric(t, d, 0, "rl_store_connections", "maintenance", waitMax, "after the restart", func(v float64) bool { return v == 1 })
		from := len(d.Sent(0))
		rep := d.RunCleanupPass(0)
		last = d.Sent(0)[from:]
		reps = append(reps, rep)
		removed += rep.Removed
		if strings.HasPrefix(rep.Diag.StoreError, "NOSCRIPT") {
			t.Fatalf("pass %d after the restart stopped on NOSCRIPT: %+v; want each step that got NOSCRIPT resent once after the pipeline's reload (§A5.4)", len(reps), rep)
		}
		if !transportStop(rep) {
			break
		}
	}
	if final := reps[len(reps)-1]; final.Status != "complete" || removed != n {
		t.Errorf("passes after the restart: %+v; want the last complete and Removed adding up to %d (got %d)", reps, n, removed)
	}
	loads, steps := driver.Count(last, "SCRIPT LOAD"), driver.Count(last, "cleanup_entry")
	all := driver.Count(d.Sent(0)[mark:], "cleanup_entry")
	t.Logf("passes after the restart: %+v; the last one sent %d SCRIPT LOAD and %d cleanup_entry, %d cleanup_entry in all", reps, loads, steps, all)
	if loads < 1 || steps <= n || steps > 2*n {
		t.Errorf("the completing pass sent %d SCRIPT LOAD and %d cleanup_entry; want at least 1, and more than %d but at most %d (every step, those that got NOSCRIPT once more after the reload)",
			loads, steps, n, 2*n)
	}
	if all > 2*n {
		t.Errorf("after the restart: %d cleanup_entry sends, want at most %d (each step at most twice)", all, 2*n)
	}
	if left := nsKeys(t, d); len(left) != 0 {
		t.Errorf("%d keys left under rl:e:<id>:, want none", len(left))
	}
}

// transportStop reports whether rep is a pass that the transport alone stopped: unavailable or
// outcome_unknown with an empty Diag, as a call ends when its connection drops or is not back
// yet (§A5.4). An error reply (NOSCRIPT, LOADING, …), a reply that failed decoding or a
// reason is no transport stop.
func transportStop(rep limiter.CleanupReport) bool {
	return (rep.Status == "unavailable" || rep.Status == "outcome_unknown") && rep.Diag == (limiter.Diag{})
}

// TestDesign_CleanupClockRegression (DESIGN §C4, §A4.5; plan S7 Tests (a)): 300 keys due at the
// cutoff (one allow each at a frozen t0, done; the clock at t0 + 6 s, which is the pass's
// cutoff T0) on an instance whose CleanupBatch is 100. With the scripts loaded, an after_reply
// barrier on the pass's first cleanup_entry pipeline holds it once page 1's 100 steps have
// run (100 steps captured, each 30 at T0). The clock then moves 3 s below the cutoff and the
// barrier is released: the pass completes within 10 s of its start with Removed 300. After the clock
// record of the regression, pages 2 and 3 and the empty page 4 are read at the high-water
// mark (3 cleanup_scan records at T0) and every step runs at it. The op log matches the
// reference model.
func TestDesign_CleanupClockRegression(t *testing.T) {
	d := fastDriver(t, 1, func(_ *limiter.Policy, in *limiter.Instance) { in.CleanupBatch = 100 })
	const n = 300
	t0 := d.Clock().NowNs()
	for k := range n {
		d.Done(allowed(t, d, 0, fmt.Sprintf("regress-%03d", k)))
	}
	cutoff := t0 + d.Policy().ReceiptLifetimeNs
	setClock(t, d, cutoff)
	d.LoadScripts()
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Barrier, Match: "cleanup_entry", Count: 1, At: fixtures.AfterReply, Name: "page1"})
	started := time.Now()
	ch := passAsync(t, d, 0)
	reached(t, d, 0, "page1")
	if steps := d.CleanupSteps(); len(steps) != 100 || steps[0].Code != 30 || steps[99].Code != 30 || steps[0].TUs != cutoff/1000 {
		t.Fatalf("held after page 1: %d steps captured (first %+v), want 100 removals at T0 %d", len(steps), steps[:min(1, len(steps))], cutoff/1000)
	}
	back := cutoff - 3*second
	setClock(t, d, back)
	d.StoreWrapper(0).Release("page1")
	var rep limiter.CleanupReport
	select {
	case rep = <-ch:
	case <-time.After(time.Until(started.Add(10 * time.Second))):
		t.Fatal("the pass did not complete within 10 s of its start")
	}
	if rep.Status != "complete" || rep.Removed != n || rep.Moved+rep.Skipped+rep.Quarantined != 0 {
		t.Errorf("pass: %+v (%v after its start), want complete with Removed %d alone", rep, time.Since(started), n)
	}
	for _, s := range d.CleanupSteps() {
		if s.Code != 30 || s.TUs != cutoff/1000 {
			t.Errorf("step %+v, want 30 at the high-water mark %d", s, cutoff/1000)
			break
		}
	}
	var scans []int64
	after := false
	for _, line := range d.OpLog() {
		var rec struct {
			Type    string `json:"type"`
			ValueNs int64  `json:"value_ns"`
			TUs     int64  `json:"t_us"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		switch {
		case rec.Type == "clock" && rec.ValueNs == back:
			after = true
		case rec.Type == "cleanup_scan" && after:
			scans = append(scans, rec.TUs)
		}
	}
	if want := []int64{cutoff / 1000, cutoff / 1000, cutoff / 1000}; !slices.Equal(scans, want) {
		t.Errorf("page reads after the regression at t_us %v, want %v (pages 2, 3 and the empty 4 at the high-water mark)", scans, want)
	}
	d.CheckRefModel(t)
}

// connectedClients returns connected_clients from INFO clients, read as rl_ops.
func connectedClients(t *testing.T, d *driver.Driver) int {
	t.Helper()
	n, err := strconv.Atoi(d.Info("clients")["connected_clients"])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDesign_ConnectionBudget (DESIGN §A5.4, §A7; plan choice): at the default instance
// settings (store_conns 8, refund_conns 2), each ready instance reports rl_store_connections
// call 8, refund 2 and maintenance 1, and each of 3 instances raises INFO clients'
// connected_clients (read as rl_ops) by exactly 11, §A7's per-instance budget; after a pause
// the count stays at 33 above the base.
func TestDesign_ConnectionBudget(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 0})
	if in := d.Instance(); in.StoreConns != 8 || in.RefundConns != 2 {
		t.Fatalf("instance settings: store_conns %d, refund_conns %d; want the defaults 8 and 2", in.StoreConns, in.RefundConns)
	}
	base := connectedClients(t, d) // the rl_ops connection is open from here on
	for k := 1; k <= 3; k++ {
		i, err := d.AddInstance(driver.InstanceSpec{})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.WaitReady(i, waitMax); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			kind string
			want float64
		}{{"call", 8}, {"refund", 2}, {"maintenance", 1}} {
			waitMetric(t, d, i, "rl_store_connections", c.kind, 5*time.Second, fmt.Sprintf("instance %d", i), func(v float64) bool { return v == c.want })
		}
		var got int
		if !waitUntil(time.Now().Add(5*time.Second), func() bool { got = connectedClients(t, d); return got == base+11*k }) {
			t.Fatalf("after %d instances: connected_clients %d, want %d (11 per instance)", k, got, base+11*k)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if got := connectedClients(t, d); got != base+33 {
		t.Errorf("3 instances, settled: connected_clients %d, want %d", got, base+33)
	}
}

// TestDesign_MetamorphicCleanup (DESIGN §C4 metamorphic tests; plan S7 Tests (a)): one seeded
// sequential history, T-COR-1's op mix with forward clock steps only (some past the receipt
// lifetime: sceneLong), runs twice, each on a fresh store and epoch with the same policy and
// start time: the fast profile with a 60 s receipt lifetime, timed passes off (3,600 s), far
// below every limit, each run within 10 s of wall time so no retention timer fires. The second
// run also calls RunCleanupPass on a random instance after random ops (a second seeded
// generator; every pass complete). The op results are identical (kinds, units, credits,
// retry_after_ns, decision, credit and expiry times; g, s and deduction IDs are not compared),
// and the second run's passes report Removed > 0. Both op logs match the reference model.
// Inspect output is not compared.
func TestDesign_MetamorphicCleanup(t *testing.T) {
	p := driver.FastPolicy()
	p.ReceiptLifetimeNs = 60_000_000_000
	const seed = 13
	start := time.Now()
	d1, base := runWorkload(t, workload{seed: seed, policy: p, forward: true})
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the first run took %v, want at most 10 s (no retention timer may fire)", took)
	}
	passes := rand.New(rand.NewPCG(seed, 0xc1ea))
	var removed, n int64
	start = time.Now()
	d2, other := runWorkload(t, workload{seed: seed, policy: p, forward: true, afterOp: func(d *driver.Driver) {
		if passes.IntN(10) != 0 {
			return
		}
		i := passes.IntN(d.Instances())
		rep := d.RunCleanupPass(i)
		if rep.Status != "complete" {
			t.Errorf("pass %d on instance %d: %+v, want complete", n+1, i, rep)
		}
		n++
		removed += rep.Removed
	}})
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the second run took %v, want at most 10 s (no retention timer may fire)", took)
	}
	t.Logf("the second run ran %d passes, removing %d buckets", n, removed)
	if n == 0 || removed == 0 {
		t.Errorf("the second run ran %d passes removing %d buckets, want passes that remove", n, removed)
	}
	mustEqualRuns(t, base, other, 1, 0)
	d1.CheckRefModel(t)
	d2.CheckRefModel(t)
}

// rollbackTraffic makes n new keys named prefix-<k>, one allow each on alternating instances
// (allowedSettled: the instances redial after a store restart), finished with done(), with the
// controlled clock stepping 250 ms after every 4th: hwm and gen grow.
func rollbackTraffic(t *testing.T, d *driver.Driver, prefix string, n int) {
	t.Helper()
	for k := range n {
		d.Done(allowedSettled(t, d, k%d.Instances(), fmt.Sprintf("%s-%02d", prefix, k)))
		if k%4 == 3 {
			step(t, d, 250_000_000)
		}
	}
}

// restoreAndCheckHold replaces the stopped store's data with the copy in dir, restarts it, and
// checks DESIGN §C4's rollback rule (§A5.7): among acquires sent on every instance from the
// moment the restored store is up, none is allowed, and within one re-read interval + 100 ms
// status shows the hold store_rollback and every instance reports not_ready (integrity_hold).
func restoreAndCheckHold(t *testing.T, d *driver.Driver, dir string) {
	t.Helper()
	d.Store().Stop()
	if err := d.Store().ReplaceDataDir(dir); err != nil {
		t.Fatal(err)
	}
	d.RestartStore()
	up := time.Now()
	deadline := up.Add(joinTolerance(d))
	held := func() bool {
		if d.Status().Hold != "store_rollback" {
			return false
		}
		for i := range d.Instances() {
			if h := d.Limiter(i).Health(); h.Status != "not_ready" || h.Reason != "integrity_hold" {
				return false
			}
		}
		return true
	}
	ok := false
	for n := 0; !ok; n++ {
		for i := range d.Instances() {
			if op := d.Acquire(i, fmt.Sprintf("rollback-after-%d", n%5)); op.Kind == limiter.Allowed {
				t.Errorf("acquire on instance %d after the restored store came up: allowed", i)
				d.Done(op)
			}
		}
		ok = held()
		if !ok && !time.Now().Before(deadline) {
			break
		}
	}
	if !ok {
		hs := make([]string, d.Instances())
		for i := range hs {
			h := d.Limiter(i).Health()
			hs[i] = h.Status + "/" + h.Reason
		}
		t.Fatalf("%v after the restored store came up: status %+v, health %v; want hold store_rollback and every instance integrity_hold within %v",
			time.Since(up), d.Status(), hs, joinTolerance(d))
	}
	t.Logf("hold store_rollback on every instance %v after the restored store came up", time.Since(up))
	for i := range d.Instances() {
		if op := d.Acquire(i, "rollback-after-0"); op.Kind != limiter.StateIntegrityError {
			t.Errorf("instance %d under the hold: %v, want state_integrity_error", i, op.Kind)
		}
	}
}

// TestDesign_RollbackHold (DESIGN §C4, §A5.7; plan S8 Tests (a)), closed mode, two instances:
// (same epoch) after traffic, the stopped store's data directory is copied; the store restarts
// and more traffic advances the clock 3 s and creates keys, so hwm and gen grow (and every
// instance reads them); then the copy replaces the data and the store restarts. (earlier
// epoch) epoch A's data is copied, ReplaceEpoch starts B, every instance joins it and B serves;
// then A's copy is restored. Both: no acquire sent after the restored store is up is allowed, and
// within one re-read interval + 100 ms status shows the hold store_rollback and every instance
// reports integrity_hold.
func TestDesign_RollbackHold(t *testing.T) {
	t.Run("same_epoch", func(t *testing.T) {
		d := fastDriver(t, 2, nil)
		rollbackTraffic(t, d, "before", 8)
		dir := filepath.Join(t.TempDir(), "copy")
		d.Store().Stop()
		if err := d.Store().CopyDataDir(dir); err != nil {
			t.Fatal(err)
		}
		d.RestartStore()
		for i := range d.Instances() {
			if err := d.WaitReady(i, waitMax); err != nil {
				t.Fatal(err)
			}
		}
		e0 := d.ReadEpoch()
		rollbackTraffic(t, d, "after", 48)
		if e := d.ReadEpoch(); e.HWM < e0.HWM+3_000_000 || e.Gen <= e0.Gen {
			t.Fatalf("after the copy: hwm %d, gen %d; want hwm 3 s past %d and gen past %d", e.HWM, e.Gen, e0.HWM, e0.Gen)
		}
		time.Sleep(joinTolerance(d)) // every instance's epoch read takes the grown hwm and gen as floors
		restoreAndCheckHold(t, d, dir)
	})
	t.Run("earlier_epoch", func(t *testing.T) {
		d := fastDriver(t, 2, nil)
		rollbackTraffic(t, d, "epoch-a", 8)
		dir := filepath.Join(t.TempDir(), "copy-a")
		d.Store().Stop()
		if err := d.Store().CopyDataDir(dir); err != nil {
			t.Fatal(err)
		}
		d.RestartStore()
		start := time.Now()
		idB := d.ReplaceEpoch()
		waitAllJoined(t, d, idB, start.Add(joinTolerance(d)))
		rollbackTraffic(t, d, "epoch-b", 8)
		restoreAndCheckHold(t, d, dir)
	})
}

// TestDesign_ReclaimOldEpoch (DESIGN §A4.7; plan S8 Tests (a)): epoch A gets 5,000 keys; the
// driver replaces the epoch (ReplaceEpoch: admin_start_epoch with replace) and both instances
// join B within the join tolerance; B gets 100 keys. After at most 3 completed passes per
// instance, rl:e:<A>:* is empty, all 100 of B's buckets are there, and the instances'
// rl_reclaim_keys_total add up to A's key count.
func TestDesign_ReclaimOldEpoch(t *testing.T) {
	d := fastDriver(t, 2, nil)
	idA := d.ReadEpoch().ID
	for k := range 5000 { // setup: a reply an AOF stall the store records delays past D is retried
		d.Done(allowedThroughAOFStall(t, d, k%2, fmt.Sprintf("reclaim-a-%04d", k)))
	}
	aKeys := len(d.StoreKeys("rl:e:" + idA + ":*"))
	start := time.Now()
	idB := d.ReplaceEpoch()
	waitAllJoined(t, d, idB, start.Add(joinTolerance(d)))
	var want []string
	for k := range 100 { // setup, as for epoch A
		key := fmt.Sprintf("reclaim-b-%03d", k)
		d.Done(allowedThroughAOFStall(t, d, k%2, key))
		want = append(want, keys.BucketKey(idB, b64(key)))
	}
	slices.Sort(want)
	for round := 0; round < 3 && len(d.StoreKeys("rl:e:"+idA+":*")) > 0; round++ {
		for i := range d.Instances() {
			passOK(t, d, i)
		}
	}
	if left := d.StoreKeys("rl:e:" + idA + ":*"); len(left) != 0 {
		t.Errorf("after 3 passes per instance %d of epoch A's %d keys are left, want none", len(left), aKeys)
	}
	if got := d.StoreKeys("rl:e:" + idB + ":k:*"); !slices.Equal(got, want) {
		t.Errorf("epoch B's buckets: %d keys, want its %d", len(got), len(want))
	}
	reclaimed := 0.0
	for i := range d.Instances() {
		reclaimed += metric(t, d, i, "rl_reclaim_keys_total", "")
	}
	if reclaimed != float64(aKeys) {
		t.Errorf("rl_reclaim_keys_total over the instances: %v, want epoch A's %d keys", reclaimed, aKeys)
	}
}

// TestDesign_StoreReconnect (DESIGN §A5.4, §A7; plan S9 Tests (a)): one instance at the
// default instance settings (store_conns 8, refund_conns 2, D 2 s) on a real store; S7's
// TestDesign_ConnectionBudget covers the counts. Within 1 s of Stop, the ready call
// connections, rl_store_connections{kind="call"}, drop to 0. Once every slot's first redial
// has been refused (100 ms after the drop: a slot redials within 50 ms of losing its
// connection, §A5.4), 100 acquires in a row fail fast during the reconnect backoff: each is
// unavailable with nothing sent, and the 100 take less than one D together, so not one waited
// for its deadline. Within 2 s of the restart (data kept), rl_store_connections shows call 8,
// refund 2 and maintenance 1 again, and an acquire is allowed.
func TestDesign_StoreReconnect(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 1})
	in := d.Instance()
	dep := time.Duration(in.DependencyDeadlineNs)
	if in.StoreConns != 8 || in.RefundConns != 2 || dep != 2*time.Second {
		t.Fatalf("instance settings: store_conns %d, refund_conns %d, D %v; want the defaults 8, 2 and 2 s", in.StoreConns, in.RefundConns, dep)
	}
	kinds := []struct {
		kind string
		n    float64
	}{{"call", 8}, {"refund", 2}, {"maintenance", 1}}
	for _, k := range kinds {
		waitMetric(t, d, 0, "rl_store_connections", k.kind, waitMax, "warm-up", func(v float64) bool { return v == k.n })
	}
	d.Done(allowed(t, d, 0, "reconnect"))

	down := true
	// Registered after the driver's teardown, so it runs first: a test that failed while the
	// store was down starts it again for the teardown.
	t.Cleanup(func() {
		if down {
			d.RestartStore()
		}
	})
	stopped := time.Now()
	d.Store().Stop()
	waitMetric(t, d, 0, "rl_store_connections", "call", time.Until(stopped.Add(time.Second)), "within 1 s of Stop",
		func(v float64) bool { return v == 0 })
	time.Sleep(100 * time.Millisecond) // every slot's first redial has been refused
	from := len(d.Sent(0))
	began := time.Now()
	for k := range 100 {
		if op := d.Acquire(0, "reconnect"); op.Kind != limiter.Unavailable || op.SentCommands != 0 {
			t.Fatalf("acquire %d during the reconnect backoff: %v (SentCommands %d, %+v), want unavailable with nothing sent",
				k+1, op.Kind, op.SentCommands, op.Diag)
		}
	}
	if el := time.Since(began); el >= dep {
		t.Errorf("100 acquires during the reconnect backoff took %v, want less than D = %v together (fail fast)", el, dep)
	}
	if n := driver.Count(d.Sent(0)[from:], "acquire"); n != 0 {
		t.Errorf("%d acquire sends while the store was down, want 0", n)
	}

	restarted := time.Now()
	d.RestartStore()
	down = false
	for _, k := range kinds {
		waitMetric(t, d, 0, "rl_store_connections", k.kind, time.Until(restarted.Add(2*time.Second)), "within 2 s of the restart",
			func(v float64) bool { return v == k.n })
	}
	d.Done(allowed(t, d, 0, "reconnect")) // the restarted store's empty script cache: one NOSCRIPT reload
}

// TestDesign_AuthRefusedText (plan S9 step 1: keep the last error reply text for Diag.StoreError
// and Health.store_error; DESIGN §A5.4, §A5.11): one instance at the default settings on a real
// store, every connection ready. The store restarts (data kept) with a fresh rl_lib password in
// its ACL file, so each redial's AUTH gets the store's own refusal, WRONGPASS. Acquires every
// 50 ms are unavailable with nothing sent, until one carries that text as Diag.StoreError (the
// first may come while a refused connect during the restart is still the kind's last dial);
// then Health.store_error is the text too, the instance still ready. The store then restarts
// with the instance's password again, and every connection comes back within waitMax.
func TestDesign_AuthRefusedText(t *testing.T) {
	const wrongPass = "WRONGPASS invalid username-password pair or user is disabled."
	d := driver.New(t, driver.Options{Instances: 1})
	in := d.Instance()
	kinds := []struct {
		kind string
		n    float64
	}{{"call", float64(in.StoreConns)}, {"refund", float64(in.RefundConns)}, {"maintenance", 1}}
	for _, k := range kinds {
		waitMetric(t, d, 0, "rl_store_connections", k.kind, waitMax, "warm-up", func(v float64) bool { return v == k.n })
	}
	st := d.Store()
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	tmpl, aclFile := filepath.Join(root, "deploy", "acl.conf.tmpl"), filepath.Join(st.Dir, "acl.conf")
	refusing := true
	// Registered after the driver's teardown, so it runs first: a test that failed while the store
	// refused the instance, or while its ACL file was being rewritten, gives the store its old ACL
	// file back for the teardown.
	t.Cleanup(func() {
		if !refusing {
			return
		}
		if err := storeproc.RenderACL(tmpl, aclFile, st.PW); err != nil {
			t.Error(err)
			return
		}
		d.RestartStore()
	})
	rotated := st.PW
	rotated.Lib = storeproc.NewPasswords().Lib // not the instance's
	if err := storeproc.RenderACL(tmpl, aclFile, rotated); err != nil {
		t.Fatal(err)
	}
	d.RestartStore()

	var tries []driver.Op
	deadline := time.Now().Add(waitMax)
	for {
		op := d.Acquire(0, "auth-refused")
		tries = append(tries, op)
		if op.Kind != limiter.Unavailable || op.SentCommands != 0 {
			t.Fatalf("acquire %d while the store refuses the instance's AUTH: %v (SentCommands %d), want unavailable with nothing sent",
				len(tries), op.Kind, op.SentCommands)
		}
		if op.Diag.StoreError == wrongPass {
			break
		}
		if op.Diag.StoreError != "" {
			t.Fatalf("acquire %d: Diag.StoreError %q, want %q or empty", len(tries), op.Diag.StoreError, wrongPass)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no acquire with Diag.StoreError %q within %v (%d unavailable)", wrongPass, waitMax, len(tries))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h := d.Limiter(0).Health(); h.StoreError != wrongPass || h.Status != "ready" {
		t.Errorf("Health: store_error %q, status %q; want %q, ready", h.StoreError, h.Status, wrongPass)
	}

	if err := storeproc.RenderACL(tmpl, aclFile, st.PW); err != nil {
		t.Fatal(err)
	}
	d.RestartStore()
	refusing = false
	for _, k := range kinds {
		waitMetric(t, d, 0, "rl_store_connections", k.kind, waitMax, "with the instance's password back", func(v float64) bool { return v == k.n })
	}
}

// acquireAsync runs d.Acquire(i, key) on its own goroutine; the channel gets its op.
func acquireAsync(d *driver.Driver, i int, key string) <-chan driver.Op {
	ch := make(chan driver.Op, 1)
	go func() { ch <- d.Acquire(i, key) }()
	return ch
}

// opBy waits for the op on ch until deadline.
func opBy(t *testing.T, ch <-chan driver.Op, deadline time.Time, what string) driver.Op {
	t.Helper()
	select {
	case op := <-ch:
		return op
	case <-time.After(time.Until(deadline)):
		t.Fatalf("%s: no result by its deadline", what)
		return driver.Op{}
	}
}

// waitSends waits until instance i's send records from index from on hold n sends of cmd that
// wrote bytes, by deadline.
func waitSends(t *testing.T, d *driver.Driver, i, from int, cmd string, n int, deadline time.Time, what string) {
	t.Helper()
	got := 0
	if !waitUntil(deadline, func() bool { got = driver.Count(d.Sent(i)[from:], cmd); return got >= n }) {
		t.Fatalf("%s: %d %s sends, want %d", what, got, cmd, n)
	}
}

// TestDesign_PipelinedTimeout (DESIGN §C4, §A5.4; plan S9 Tests (a)): one instance with one call
// connection (StoreConns 1) and D = 300 ms (plan choice), its epoch read once at startup and
// timed cleanup off (both every hour, plan choice), so nothing but these acquires is written on
// any connection; a first allowed acquire loads the script.
//   - Part 1: while the store is paused (SIGSTOP) for 100 ms, three acquires are written to the
//     one connection, the first with a 10 ms host deadline. It ends outcome_unknown, and the
//     other two are allowed once the store resumes, after the first's late reply was drained;
//     no AUTH is written (the connection stays up), and inspect shows the first's late allow
//     charged: u = (capacity − 1) × rate_period_ns (API-1).
//   - Part 2: while the store is paused, three acquires are written; the oldest waits D, so the
//     connection closes (rl_store_connections{kind="call"} 0) and exactly those three, the
//     acquire sends written before the close (Sent), are outcome_unknown. Calls after the close
//     send nothing and are unavailable until the store resumes, once 600 ms have passed since
//     the pause and no call is waiting (plan choice: such a call waits up to D for the redial,
//     whose AUTH cannot complete while the store is paused, so resuming under a waiting call
//     could hand it the new connection). After the resume an acquire every 100 ms is allowed
//     within 2 s, each earlier one unavailable with nothing sent (the redial backoff), over a
//     new connection: an AUTH written after the close and before the allowed acquire.
func TestDesign_PipelinedTimeout(t *testing.T) {
	p := driver.DefaultPolicy()
	p.CleanupIntervalNs = 3_600_000_000_000
	in := driver.DefaultInstance(p)
	in.StoreConns, in.DependencyDeadlineNs, in.EpochRereadIntervalNs = 1, 300_000_000, 3_600_000_000_000
	dep := 300 * time.Millisecond
	d := driver.New(t, driver.Options{Instances: 1, Policy: p, Instance: in})
	st := d.Store()
	// Registered after the driver's teardown, so it runs first: a test that failed while the
	// store was paused resumes it for the teardown (Resume of a store not paused does nothing).
	t.Cleanup(func() { _ = st.Resume() })
	waitMetric(t, d, 0, "rl_store_connections", "call", waitMax, "warm-up", func(v float64) bool { return v == 1 })
	d.Done(allowed(t, d, 0, "pipelined-warm"))

	// Part 1: a host deadline after the send ends that call only.
	from := len(d.Sent(0))
	if err := st.Pause(); err != nil {
		t.Fatal(err)
	}
	paused := time.Now()
	short := make(chan driver.Op, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		short <- d.AcquireContext(ctx, 0, "p1-short")
	}()
	waitSends(t, d, 0, from, "acquire", 1, paused.Add(dep), "part 1, the 10 ms acquire")
	others := []<-chan driver.Op{acquireAsync(d, 0, "p1-b"), acquireAsync(d, 0, "p1-c")}
	waitSends(t, d, 0, from, "acquire", 3, paused.Add(100*time.Millisecond), "part 1, three acquires written while paused")
	time.Sleep(time.Until(paused.Add(100 * time.Millisecond)))
	if err := st.Resume(); err != nil {
		t.Fatal(err)
	}
	if op := opBy(t, short, paused.Add(waitMax), "part 1, the 10 ms acquire"); op.Kind != limiter.OutcomeUnknown || op.SentCommands != 1 {
		t.Errorf("part 1, the 10 ms acquire: %v (SentCommands %d), want outcome_unknown, sent", op.Kind, op.SentCommands)
	}
	for k, ch := range others {
		op := opBy(t, ch, paused.Add(waitMax), "part 1, another acquire")
		if op.Kind != limiter.Allowed {
			t.Errorf("part 1, acquire %d on the same connection: %v (%+v), want allowed", k+2, op.Kind, op.Diag)
		}
		d.Done(op)
	}
	if n := driver.Count(d.Sent(0)[from:], "AUTH"); n != 0 {
		t.Errorf("part 1: %d AUTH sends, want none (the connection stays up)", n)
	}
	if v := metric(t, d, 0, "rl_store_connections", "call"); v != 1 {
		t.Errorf("part 1: rl_store_connections{call} = %v, want 1", v)
	}
	if got, want := d.Inspect("p1-short"), (p.Capacity-1)*p.RatePeriodNs; got.Present != 1 || got.U != want {
		t.Errorf("part 1, inspect of the 10 ms acquire's key: present %d, u %d; want 1 and %d (its late allow charged)", got.Present, got.U, want)
	}

	// Part 2: a command that waits D closes the connection.
	from = len(d.Sent(0))
	if err := st.Pause(); err != nil {
		t.Fatal(err)
	}
	paused = time.Now()
	var inflight []<-chan driver.Op
	for k := range 3 {
		inflight = append(inflight, acquireAsync(d, 0, fmt.Sprintf("p2-%d", k)))
	}
	waitSends(t, d, 0, from, "acquire", 3, paused.Add(dep), "part 2, three acquires written while paused")
	for k, ch := range inflight {
		if op := opBy(t, ch, paused.Add(waitMax), "part 2, an acquire in flight"); op.Kind != limiter.OutcomeUnknown || op.SentCommands != 1 {
			t.Errorf("part 2, acquire %d in flight: %v (SentCommands %d), want outcome_unknown, sent", k+1, op.Kind, op.SentCommands)
		}
	}
	waitMetric(t, d, 0, "rl_store_connections", "call", time.Second, "part 2, once the oldest command waited D",
		func(v float64) bool { return v == 0 })
	var after []driver.Op
	for len(after) == 0 || time.Since(paused) < 600*time.Millisecond {
		after = append(after, d.Acquire(0, "p2-after"))
	}
	written := driver.Count(d.Sent(0)[from:], "acquire")
	if err := st.Resume(); err != nil {
		t.Fatal(err)
	}
	resumed := time.Now()
	t.Logf("part 2: paused %v; %d calls after the close", resumed.Sub(paused), len(after))
	if written != 3 {
		t.Errorf("part 2: %d acquire sends while paused, want exactly the 3 written before the close", written)
	}
	for k, op := range after {
		if op.Kind != limiter.Unavailable || op.SentCommands != 0 {
			t.Errorf("part 2, call %d after the close: %v (SentCommands %d), want unavailable with nothing sent", k+1, op.Kind, op.SentCommands)
		}
	}
	// An acquire every 100 ms from the resume (or at once after one that took longer); the
	// allowed one must return within 2 s of the resume, measured, not counted in tries.
	var tries []driver.Op
	var allowedAt time.Duration
	for {
		op := d.Acquire(0, "p2-resumed")
		el := time.Since(resumed)
		tries = append(tries, op)
		if op.Kind == limiter.Allowed {
			d.Done(op)
			allowedAt = el
			break
		}
		if op.Kind != limiter.Unavailable || op.SentCommands != 0 {
			t.Fatalf("part 2, acquire %d after the resume: %v (SentCommands %d), want unavailable with nothing sent until one is allowed",
				len(tries), op.Kind, op.SentCommands)
		}
		if el >= 2*time.Second {
			t.Fatalf("part 2: no allowed acquire within 2 s of the resume (%d unavailable, the last returned %v after it)", len(tries), el)
		}
		time.Sleep(time.Until(resumed.Add(time.Duration(len(tries)) * 100 * time.Millisecond)))
	}
	if allowedAt > 2*time.Second {
		t.Errorf("part 2: the allowed acquire (try %d) returned %v after the resume, want within 2 s", len(tries), allowedAt)
	}
	t.Logf("part 2: allowed at try %d, %v after the resume", len(tries), allowedAt)
	sends := d.Sent(0)[from:]
	last := -1 // the allowed acquire's send
	for k, s := range sends {
		if s.Cmd == "acquire" && s.Bytes > 0 {
			last = k
		}
	}
	if auth := driver.Count(sends[:max(last, 0)], "AUTH"); last < 0 || auth == 0 {
		t.Errorf("part 2: %d AUTH sends before the allowed acquire's, want at least 1 (a new connection)", auth)
	}
	if v := metric(t, d, 0, "rl_store_connections", "call"); v != 1 {
		t.Errorf("part 2: rl_store_connections{call} = %v, want 1", v)
	}
}
