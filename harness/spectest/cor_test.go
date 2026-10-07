//go:build rltest

package spectest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/storeproc"
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

// cor1Mutants are the 8 rule mutants of refmodel/mutants.py (plan S6 step 12).
func cor1Mutants() []string {
	return []string{
		"refill_uncapped", "allow_strict", "credit_uncapped", "hwm_on_limited",
		"expiry_strict", "retry_floor", "new_bucket_short", "credit_before_refill",
	}
}

// TestT_COR_1 (SPEC T-COR-1; BUD-2 … BUD-5, BUD-8, BUD-11, BUD-11a, BUD-12, BUD-13; DESIGN
// §C4, §C6): 3 seeds × 2 policies (the default budget; capacity 40 at 3 tokens per 7 s), each
// on a fresh deployment of 3 instances: the 1,500-op workload of runCOR1 (acquires, dones,
// releases each waited for, µs clock steps with jumps to full and steps back below the
// high-water mark, exact token boundaries, refunds at exactly expiry_us and 1 µs before, and
// on full buckets). Its op log replays exactly in the reference model (identical decisions,
// units, retry_after_ns and credits) and passes check_bud11 (CheckRefModel). Each log is kept
// as .out/tcor1-<policy>-<seed>.jsonl, and on each the replay with each of the 8 model mutants
// exits 1, except retry_floor on the default logs, where every unit value is a multiple of
// 10^4 so floor and ceiling agree (DESIGN §C6).
func TestT_COR_1(t *testing.T) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, ".out")
	if err := os.MkdirAll(out, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, pc := range cor1Policies() {
		for _, seed := range []uint64{1, 2, 3} {
			t.Run(fmt.Sprintf("%s/%d", pc.name, seed), func(t *testing.T) {
				d, ops := runCOR1(t, seed, pc.p, 0)
				counts := map[string]int{}
				for _, op := range ops {
					counts[op.kind+" "+op.result]++
				}
				t.Logf("ops: %v", counts)
				d.CheckRefModel(t)
				path := filepath.Join(out, fmt.Sprintf("tcor1-%s-%d.jsonl", pc.name, seed))
				if err := d.SaveOpLog(path); err != nil {
					t.Fatal(err)
				}
				for _, m := range cor1Mutants() {
					if m == "retry_floor" && pc.name == "default" {
						continue
					}
					code, msg, err := driver.RunRefModel(t.Context(), "refmodel.replay", "--mutant", m, path)
					if err != nil {
						t.Fatalf("mutant %s: %v", m, err)
					}
					if code != 1 {
						t.Errorf("replay with mutant %s exits %d on %s, want 1 (a mismatch):\n%s", m, code, path, msg)
					}
				}
			})
		}
	}
}

// countAllowed acquires key at the current instant, alternating instances, finishing each
// allow with Done, until a result is not allowed (at most limit acquires), and returns the
// number allowed.
func countAllowed(t *testing.T, d *driver.Driver, key string, limit int) int {
	t.Helper()
	for n := range limit {
		op := d.Acquire(n%d.Instances(), key)
		if op.Kind != limiter.Allowed {
			if op.Kind != limiter.Limited {
				t.Fatalf("acquire %d of %q: %v (%+v), want allowed or limited", n+1, key, op.Kind, op.Diag)
			}
			return n
		}
		d.Done(op)
	}
	return limit
}

// TestT_COR_3 (SPEC T-COR-3; BUD-3, BUD-8, REF-2; EX-2, EX-3, EX-4) on the fast profile, one
// key per example, the clock frozen between the steps: EX-2 empties a bucket (20 allows), then
// +100 ms gives exactly 1 allowed and then limited; EX-3 empties one, then +2 s gives exactly
// 20 allowed; EX-4 empties one at an instant and releases one of its permits, whose refund is
// applied with credit 1 token (captured), then exactly 1 more is allowed. The op log replays
// in the reference model and passes check_bud11.
func TestT_COR_3(t *testing.T) {
	d := fastDriver(t, 2, nil)
	p := d.Policy()
	empty := func(key string) []driver.Op {
		t.Helper()
		var ops []driver.Op
		for n := range 20 {
			ops = append(ops, allowed(t, d, n%2, key))
		}
		if op := d.Acquire(0, key); op.Kind != limiter.Limited {
			t.Fatalf("acquire 21 of %q: %v, want limited (empty)", key, op.Kind)
		}
		return ops
	}
	doneAll := func(ops []driver.Op) {
		for _, op := range ops {
			d.Done(op)
		}
	}

	doneAll(empty("ex2"))
	step(t, d, 100_000_000)
	if n := countAllowed(t, d, "ex2", 25); n != 1 {
		t.Errorf("EX-2: %d allowed after +100 ms on an empty bucket, want exactly 1", n)
	}

	doneAll(empty("ex3"))
	step(t, d, 2_000_000_000)
	if n := countAllowed(t, d, "ex3", 25); n != 20 {
		t.Errorf("EX-3: %d allowed after +2 s on an empty bucket, want exactly 20", n)
	}

	ops := empty("ex4")
	d.Release(ops[19])
	ev := event(t, d, ops[19])
	doneAll(ops[:19])
	if ev.Result != limiter.Applied {
		t.Fatalf("EX-4: refund %v, want applied", ev.Result)
	}
	if as := d.RefundAttempts(ops[19]); len(as) == 0 || as[len(as)-1].Credited() != token(p) {
		t.Errorf("EX-4: refund attempts %+v, want the last credited %d units (1 token)", as, token(p))
	}
	if n := countAllowed(t, d, "ex4", 25); n != 1 {
		t.Errorf("EX-4: %d allowed after the refund on an empty bucket, want exactly 1", n)
	}
	d.CheckRefModel(t)
}

// TestT_COR_4 (SPEC T-COR-4; STA-1, REF-3; EX-1, EX-5): with a frozen clock, 120 concurrent
// acquires of one key across 3 instances → exactly 20 allowed; each of the 20 permits is then
// released from 5 goroutines at once while each instance drops its first refund reply per
// permit (drop_reply, counted per instance; the refund backoff is 1 s, so every permit's first
// attempt comes before any retry) → 20 events, each already_applied with its permit's
// deduction_id, Inspect shows 20 markers and u = 20 tokens (one credit each, no double
// credit); 40 more concurrent acquires → exactly 20 allowed; the 40 allowed deduction_ids are
// distinct. check_bud11 at teardown judges the concurrent history.
func TestT_COR_4(t *testing.T) {
	d := fastDriver(t, 3, func(_ *limiter.Policy, in *limiter.Instance) {
		// §A5.6: 1 s + 1 s + (⌈128 / 128⌉ + 3) × 0.2 s = 2.8 s ≤ 3 s.
		in.RefundBackoffInitialNs, in.RefundBackoffMaxNs = 1_000_000_000, 1_000_000_000
	})
	p := d.Policy()
	const key = "cor4"
	burst := func(n int) []driver.Op {
		ops := make([]driver.Op, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for k := range ops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ops[k] = d.Acquire(k%3, key)
			}()
		}
		close(start)
		wg.Wait()
		var won []driver.Op
		for _, op := range ops {
			if op.Kind == limiter.Allowed {
				won = append(won, op)
			} else if op.Kind != limiter.Limited {
				t.Errorf("concurrent acquire: %v (%+v), want allowed or limited", op.Kind, op.Diag)
			}
		}
		return won
	}
	first := burst(120)
	if len(first) != 20 {
		t.Fatalf("120 concurrent acquires: %d allowed, want 20", len(first))
	}
	perInstance := make([]int, 3)
	for _, op := range first {
		perInstance[op.Instance]++
	}
	for i, n := range perInstance {
		if n == 0 {
			continue
		}
		if err := d.StoreWrapper(i).Arm(fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: n}); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, op := range first {
		for range 5 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				d.Release(op)
			}()
		}
	}
	close(start)
	wg.Wait()
	for _, op := range first {
		ev := event(t, d, op)
		if ev.Result != limiter.AlreadyApplied || ev.DeductionID != op.Permit.DeductionID() {
			t.Errorf("permit %s: event %+v, want already_applied with its deduction_id", op.Permit.DeductionID(), ev)
		}
	}
	for i := range 3 {
		if evs := d.RefundEvents(i); len(evs) != perInstance[i] {
			t.Errorf("instance %d: %d events, want %d (one per permit)", i, len(evs), perInstance[i])
		}
	}
	if in := d.Inspect(key); in.MC != 20 || in.U != 20*token(p) {
		t.Errorf("Inspect %+v, want mc 20 and u %d (20 tokens: one credit per permit)", in, 20*token(p))
	}
	second := burst(40)
	if len(second) != 20 {
		t.Errorf("40 more concurrent acquires: %d allowed, want 20", len(second))
	}
	ids := map[string]bool{}
	for _, op := range slices.Concat(first, second) {
		ids[op.Permit.DeductionID()] = true
	}
	if len(ids) != len(first)+len(second) || ids[""] {
		t.Errorf("%d distinct deduction IDs over %d allows (empty among them: %v), want all distinct", len(ids), len(first)+len(second), ids[""])
	}
	for _, op := range second {
		d.Done(op)
	}
}

// TestT_COR_7 (SPEC T-COR-7; BUD-6, BUD-7, API-2b) on the fast profile: a new key gives 20
// allowed at one frozen instant (it starts full), and the 21st is limited within 50 ms of wall
// time with RetryAfterNs = 100,000,000 (no queuing). A key never acquired has no bucket, marker
// index, ix member or q entry after other traffic and 3 epoch reads on each instance. On a
// second deployment, a valid receipt whose key's k, mi and ix member were deleted
// (driver.Fixture) gets the refund reply {23, t, 'refund_bucket_missing'} (captured), and
// Inspect shows no bucket (present 0): a refund never creates one.
func TestT_COR_7(t *testing.T) {
	d := fastDriver(t, 2, nil)
	for n := range 20 {
		d.Done(allowed(t, d, n%2, "cor7"))
	}
	start := time.Now()
	op := d.Acquire(1, "cor7")
	took := time.Since(start)
	if op.Kind != limiter.Limited || op.RetryAfterNs != 100_000_000 {
		t.Errorf("acquire 21: %v, retry_after_ns %d; want limited, 100,000,000", op.Kind, op.RetryAfterNs)
	}
	if took >= 50*time.Millisecond {
		t.Errorf("acquire 21 returned after %v, want within 50 ms (no queuing)", took)
	}

	const idle = "cor7-idle"
	reads := func() []int {
		out := make([]int, d.Instances())
		for i := range out {
			out[i] = driver.Count(d.Sent(i), "read_epoch")
		}
		return out
	}
	before := reads()
	for n := range 10 {
		op := allowed(t, d, n%2, fmt.Sprintf("cor7-busy-%d", n%3))
		if n%2 == 0 {
			d.Release(op)
			event(t, d, op)
		} else {
			d.Done(op)
		}
	}
	deadline := time.Now().Add(3*interval(d) + waitMax)
	if !waitUntil(deadline, func() bool {
		now := reads()
		for i := range now {
			if now[i] < before[i]+3 {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("read_epoch sends %v after %v, want 3 more on each instance", reads(), before)
	}
	enc, err := keys.Encode(idle)
	if err != nil {
		t.Fatal(err)
	}
	id := d.ReadEpoch().ID
	snap := d.Dump()
	for _, k := range []string{keys.BucketKey(id, enc), keys.MarkerIndexKey(id, enc)} {
		if _, ok := snap[k]; ok {
			t.Errorf("the idle key has %s", k)
		}
	}
	if _, ok := snap[keys.IndexKey(id)].Fields[enc]; ok {
		t.Error("the idle key has an ix member")
	}
	if _, ok := snap[keys.QuarantineKey(id)].Fields[enc]; ok {
		t.Error("the idle key has a q entry")
	}

	f := fastDriver(t, 1, nil)
	const gone = "cor7-gone"
	held := allowed(t, f, 0, gone)
	fid := f.ReadEpoch().ID
	genc, err := keys.Encode(gone)
	if err != nil {
		t.Fatal(err)
	}
	fix := f.Fixture()
	for _, cmd := range [][]string{
		{"DEL", keys.BucketKey(fid, genc)}, {"DEL", keys.MarkerIndexKey(fid, genc)}, {"ZREM", keys.IndexKey(fid), genc},
	} {
		if r, err := fix.Do(cmd...); err != nil || r.Kind == '-' {
			t.Fatalf("Fixture %v: %v %s", cmd, err, r)
		}
	}
	f.Release(held)
	if !waitUntil(time.Now().Add(waitMax), func() bool { return len(f.RefundAttempts(held)) >= 2 }) {
		t.Fatalf("refund attempts %+v, want at least 2 within %v", f.RefundAttempts(held), waitMax)
	}
	a := f.RefundAttempts(held)[0]
	if r := a.Response; r.Outcome != store.OK || r.Reply.Code != 23 || r.Reply.Reason != "refund_bucket_missing" || r.Reply.TUs <= 0 {
		t.Errorf("first refund reply: outcome %v, {%d, %d, %q}; want {23, t, 'refund_bucket_missing'}", r.Outcome, r.Reply.Code, r.Reply.TUs, r.Reply.Reason)
	}
	if in := f.Inspect(gone); in.Present != 0 {
		t.Errorf("Inspect after the refunds: %+v, want present 0 (no bucket created)", in)
	}
	f.Kill(0) // the refund retries until its retention; nothing more to see
}
