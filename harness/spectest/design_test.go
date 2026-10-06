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
