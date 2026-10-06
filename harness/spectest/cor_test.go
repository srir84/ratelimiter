//go:build rltest

package spectest

import (
	"strings"
	"sync"
	"testing"

	"ratelimiter/harness/driver"
	"ratelimiter/limiter"
)

// TestT_COR_2 (SPEC T-COR-2; EX-1): a frozen clock and 100 concurrent acquires of one key
// across 3 instances → exactly 20 allowed (the capacity) and 80 limited; the 20 permits carry
// 20 distinct, non-empty deduction IDs.
func TestT_COR_2(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 3})
	ops := make([]driver.Op, 100)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for n := range ops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ops[n] = d.Acquire(n%3, "ex1")
		}()
	}
	close(start)
	wg.Wait()
	defer doneAll(ops)
	ids := map[string]bool{}
	kinds := map[limiter.ResultKind]int{}
	for _, op := range ops {
		kinds[op.Kind]++
		if op.Kind == limiter.Allowed {
			ids[op.Permit.DeductionID()] = true
		}
	}
	if kinds[limiter.Allowed] != 20 || kinds[limiter.Limited] != 80 {
		t.Errorf("results %v, want 20 allowed and 80 limited", kinds)
	}
	if len(ids) != 20 || ids[""] {
		t.Errorf("%d distinct deduction IDs (empty among them: %v), want 20 non-empty", len(ids), ids[""])
	}
}

// TestT_COR_5 (SPEC T-COR-5; API-6, BUD-13, EX-2): for the default policy and {capacity 40, 3
// tokens per 7 s}, empty a key at t0, then acquire one at a time at the plan's instants, each an
// offset from t0 (limited acquires change no state, so the offsets run in increasing order),
// then after further clock steps (1 µs, a step back below the high-water mark, long refills).
// Every retry_after_ns equals ⌈(rp − u) / rt⌉ to the ns (math/big, from the reply's u), the
// whole op log matches the reference model (CheckRefModel), and at least one limited instant
// has (rp − u) mod rt ≠ 0. The kind and retry_after_ns at each instant are also pinned by hand
// (a token is rp units, and rt units accrue per ns): under the default policy (rp 1 s, rt 10)
// the key is 10,000 units short at +99,999 µs (retry 1,000 ns) and has its token at +100 ms
// (EX-2); under the second (rp 7 s, rt 3) it is 1,000 units short at +2,333,333 µs (retry
// ⌈1,000 / 3⌉ = 334 ns) and has its first token at +2,333,334 µs (⌈7 s / 3⌉ = 2,333,333,334 ns).
func TestT_COR_5(t *testing.T) {
	other := driver.DefaultPolicy()
	other.Capacity, other.RateTokens, other.RatePeriodNs = 40, 3, 7_000_000_000
	type instant struct {
		us    int64              // the offset from t0, in µs
		kind  limiter.ResultKind // by hand
		retry int64              // a limited acquire's retry_after_ns, by hand
	}
	cases := []struct {
		p        limiter.Policy
		instants []instant
	}{
		{driver.DefaultPolicy(), []instant{
			{0, limiter.Limited, 100_000_000},  // u 0
			{1, limiter.Limited, 99_999_000},   // u 10,000
			{333, limiter.Limited, 99_667_000}, // u 3,330,000
			{99_999, limiter.Limited, 1_000},   // u 999,990,000: 10,000 units short of a token
			{100_000, limiter.Allowed, 0},      // u 10^9: one token (EX-2)
			{2_333_333, limiter.Allowed, 0},    // refilled to capacity since the allow at +100 ms
			{2_333_334, limiter.Allowed, 0},
		}},
		{other, []instant{
			{0, limiter.Limited, 2_333_333_334},       // u 0: ⌈7·10^9 / 3⌉
			{1, limiter.Limited, 2_333_332_334},       // u 3,000
			{333, limiter.Limited, 2_333_000_334},     // u 999,000
			{99_999, limiter.Limited, 2_233_334_334},  // u 299,997,000
			{100_000, limiter.Limited, 2_233_333_334}, // u 300,000,000
			{2_333_333, limiter.Limited, 334},         // u 6,999,999,000: 1,000 units short of a token
			{2_333_334, limiter.Allowed, 0},           // u 7,000,002,000: the first token
		}},
	}
	// The clock steps after the instants, in µs; 0 is a step back of 500 ms, below the
	// high-water mark (the budget time holds).
	later := []int64{1, 0, 7_000_000, 1_000, 2_333_333, 20_000_000}
	nonDivisible := 0
	for _, c := range cases {
		p := c.p
		d := driver.New(t, driver.Options{Instances: 2, Policy: p})
		var ops []driver.Op
		acquire := func() driver.Op {
			op := d.Acquire(len(ops)%2, "cor5")
			ops = append(ops, op)
			return op
		}
		for range p.Capacity {
			if op := acquire(); op.Kind != limiter.Allowed {
				t.Fatalf("capacity %d: emptying got %v", p.Capacity, op.Kind)
			}
		}
		check := func(op driver.Op) {
			t.Helper()
			if op.Kind != limiter.Allowed && op.Kind != limiter.Limited {
				t.Errorf("capacity %d: %v (%+v), want allowed or limited", p.Capacity, op.Kind, op.Diag)
				return
			}
			if op.Kind == limiter.Limited {
				u := replyU(t, op)
				if want := retryAfter(u, p.RatePeriodNs, p.RateTokens); op.RetryAfterNs != want {
					t.Errorf("capacity %d, u %d: retry_after_ns %d, want %d", p.Capacity, u, op.RetryAfterNs, want)
				}
				if (p.RatePeriodNs-u)%p.RateTokens != 0 {
					nonDivisible++
				}
			}
		}
		t0 := d.Clock().NowNs() // the key is empty
		for _, in := range c.instants {
			if err := d.Clock().Set(t0 + in.us*1000); err != nil {
				t.Fatal(err)
			}
			op := acquire()
			check(op)
			if op.Kind != in.kind || (in.kind == limiter.Limited && op.RetryAfterNs != in.retry) {
				t.Errorf("capacity %d at t0 + %d µs: %v, retry_after_ns %d; want %v, retry_after_ns %d",
					p.Capacity, in.us, op.Kind, op.RetryAfterNs, in.kind, in.retry)
			}
		}
		for _, us := range later {
			if us == 0 {
				if err := d.Clock().Back(500_000_000); err != nil {
					t.Fatal(err)
				}
			} else {
				step(t, d, us*1000)
			}
			check(acquire())
		}
		doneAll(ops)
		d.CheckRefModel(t)
	}
	if nonDivisible == 0 {
		t.Error("no limited instant had (rp − u) mod rt ≠ 0")
	}
}

// TestT_COR_8 (SPEC T-COR-8; BUD-9): 20 acquires of one key over 2 instances, each tagged in
// the op log with the same host request ID (the library takes none) → 20 allowed with 20
// distinct deduction IDs, and the 21st limited: each allow spent a token. The op log matches
// the reference model.
func TestT_COR_8(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 2})
	const requestID = "req-0042"
	ids := map[string]bool{}
	var ops []driver.Op
	for n := range 20 {
		op := d.AcquireTagged(n%2, "order-7", requestID)
		ops = append(ops, op)
		if op.Kind != limiter.Allowed {
			t.Fatalf("acquire %d with request ID %s: %v, want allowed", n+1, requestID, op.Kind)
		}
		ids[op.Permit.DeductionID()] = true
	}
	defer doneAll(ops)
	if op := d.AcquireTagged(0, "order-7", requestID); op.Kind != limiter.Limited {
		t.Errorf("acquire 21: %v, want limited", op.Kind)
	}
	if len(ids) != 20 || ids[""] {
		t.Errorf("%d distinct deduction IDs, want 20 non-empty", len(ids))
	}
	tagged := 0
	for _, l := range d.OpLog() {
		if strings.Contains(l, `"type":"acquire"`) && strings.Contains(l, "request_id="+requestID) {
			tagged++
		}
	}
	if tagged != 21 {
		t.Errorf("%d acquire records carry request ID %s, want 21", tagged, requestID)
	}
	d.CheckRefModel(t)
}
