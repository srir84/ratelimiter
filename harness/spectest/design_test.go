//go:build rltest

package spectest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
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
// Closing all of A's instances leaves B's results matching its model, and the goroutine
// count returns to its value just before A's instances were built (A starts with none and
// adds 3, plan choice), so B's goroutines are untouched.
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
	for k := range n {
		d.Done(allowed(t, d, 0, fmt.Sprintf("restart-%03d", k)))
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
