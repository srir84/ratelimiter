//go:build rltest

package spectest

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/driver"
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
