//go:build rltest

package spectest

import (
	"math/rand/v2"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/limiter"
)

// The T-COR-1 workload (IMPLEMENTATION_PLAN S6 Tests (b); DESIGN §C4): a seeded generator
// that drives one deployment of 3 instances and 4 keys through exactly cor1Ops sequential ops
// (an acquire, a done, a release with its wait for the final event, or a controlled-clock
// step). TestDesign_MetamorphicTimeShift and TestDesign_MetamorphicScale run it too (plan
// choice: one shared helper taking a seed, a policy and a time offset).
//
// Besides random ops, every run builds in, on purpose, at least twice each, what each of the
// 8 model mutants of refmodel/mutants.py gets wrong (plan S6 Watch out):
//   - an acquire exactly on a token boundary, u = rate_period_ns after refill (allow_strict):
//     after a limited reply, a step of exactly (rp − u) / rt ns when that is a whole number
//     of µs, else one more allow to move u's residue (at most 3 tries);
//   - refunds at exactly expiry_us (receipt_expired) and 1 µs before it (applied)
//     (expiry_strict);
//   - a refund on a full bucket: an acquire right after a jump to full, released a token's
//     refill time later, inside the receipt lifetime, credits 0 (credit_uncapped,
//     credit_before_refill);
//   - a limited acquire above the high-water mark, then a step back below it and an acquire
//     (hwm_on_limited); every other step back is 1 µs to 2 s below the high-water mark;
//   - jumps of at least the time to full, then acquires of existing keys (refill_uncapped);
//   - every key's first acquire (new_bucket_short), and limited acquires whose wait is not a
//     whole number of ns per token (retry_floor; only the c40 policy has them, DESIGN §C6).
//
// Every choice depends on the replies only through ratios of units to rate_period_ns and
// rate_tokens, so a run with both × 7 makes the same choices (MetamorphicScale), and on times
// only through differences, so a run shifted in time does too (MetamorphicTimeShift).

// cor1Ops is the number of ops of one run.
const cor1Ops = 1500

// cor1Op is one op of the workload and what it returned, for the metamorphic comparisons.
type cor1Op struct {
	kind     string // "acquire", "done", "release" or "clock"
	result   string // acquire: the result kind; release: the final refund result
	units    int64  // acquire: the reply's u; release: an applied refund's credited units
	retry    int64  // acquire: retry_after_ns
	decision int64  // acquire: decision_ns; release: CreditDecisionNs (0 when unknown)
	expiryUs int64  // acquire: expiry_us of an allow
	clockNs  int64  // clock: the value written
}

// cor1Keys are the workload's 4 keys: any UTF-8 is a key (API-1).
func cor1Keys() []string { return []string{"alpha", "βeta-2", "ключ", "k3 ✓"} }

// cor1Policies are T-COR-1's two policies on the fast profile: the default budget (10 tokens
// per s, capacity 20) and capacity 40 at 3 tokens per 7 s (plan names default and c40).
func cor1Policies() []struct {
	name string
	p    limiter.Policy
} {
	c40 := driver.FastPolicy()
	c40.Capacity, c40.RateTokens, c40.RatePeriodNs = 40, 3, 7_000_000_000
	return []struct {
		name string
		p    limiter.Policy
	}{{"default", driver.FastPolicy()}, {"c40", c40}}
}

// cor1 is one run of the workload.
type cor1 struct {
	t     *testing.T
	d     *driver.Driver
	p     limiter.Policy
	rng   *rand.Rand
	keys  []string
	ops   []cor1Op
	held  []driver.Op
	maxNs int64 // the highest controlled time written: the high-water mark is at most this
}

// runCOR1 runs the workload of seed under p on a fresh driver whose controlled time starts at
// driver.DefaultStartNs + offsetNs, and returns the driver (its op log, for the bridge) and
// the ops.
func runCOR1(t *testing.T, seed uint64, p limiter.Policy, offsetNs int64) (*driver.Driver, []cor1Op) {
	t.Helper()
	d := driver.New(t, driver.Options{
		Instances: 3, Policy: p, Instance: driver.FastInstance(p), StartNs: driver.DefaultStartNs + offsetNs,
	})
	g := &cor1{t: t, d: d, p: p, rng: rand.New(rand.NewPCG(seed, 0x7c01)), keys: cor1Keys(), maxNs: d.Clock().NowNs()}
	scenes := []func(){g.sceneBoundary, g.sceneExpiry, g.sceneFull, g.sceneBack, g.sceneJump}
	var required []func()
	for range 2 {
		required = append(required, scenes...)
	}
	g.rng.Shuffle(len(required), func(i, j int) { required[i], required[j] = required[j], required[i] })
	for _, s := range required {
		s()
		g.randomOps(g.rng.IntN(20))
	}
	const sceneMax = 400 // more than any scene's ops (a drain is at most 3 × capacity + 6 acquires, each finished)
	for len(g.ops)+len(g.held)+sceneMax < cor1Ops {
		if g.rng.IntN(3) == 0 {
			scenes[g.rng.IntN(len(scenes))]()
		} else {
			g.randomOps(1 + g.rng.IntN(40))
		}
	}
	for len(g.ops)+len(g.held) < cor1Ops {
		g.randomStep()
	}
	for len(g.held) > 0 {
		g.done(g.held[0])
	}
	if len(g.ops) != cor1Ops {
		t.Fatalf("seed %d: %d ops, want %d", seed, len(g.ops), cor1Ops)
	}
	return d, g.ops
}

// Policy-derived times, in ns, rounded up to whole µs: one token's refill and the refill from
// empty to full.
func (g *cor1) tokenNs() int64 { return ceilUs(ceilDiv(g.p.RatePeriodNs, g.p.RateTokens)) }
func (g *cor1) fullNs() int64 {
	return ceilUs(ceilDiv(g.p.Capacity*g.p.RatePeriodNs, g.p.RateTokens))
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }
func ceilUs(ns int64) int64    { return ceilDiv(ns, 1000) * 1000 }

func (g *cor1) key() string { return g.keys[g.rng.IntN(len(g.keys))] }
func (g *cor1) inst() int   { return g.rng.IntN(3) }

// usBetween returns a whole number of µs in [lo, hi] µs, in ns.
func (g *cor1) usBetween(lo, hi int64) int64 { return (lo + g.rng.Int64N(hi-lo+1)) * 1000 }

// set writes the controlled time ns.
func (g *cor1) set(ns int64) {
	g.t.Helper()
	if err := g.d.Clock().Set(ns); err != nil {
		g.t.Fatal(err)
	}
	g.maxNs = max(g.maxNs, ns)
	g.ops = append(g.ops, cor1Op{kind: "clock", clockNs: ns})
}

func (g *cor1) forward(ns int64) { g.set(g.d.Clock().NowNs() + ns) }

// back steps the clock back by ns, staying at or above 0.
func (g *cor1) back(ns int64) { g.set(max(g.d.Clock().NowNs()-ns, 0)) }

// aboveHWM moves the clock 1 µs to 1 ms past the highest time written, so the next decision
// is at the clock.
func (g *cor1) aboveHWM() { g.set(g.maxNs + g.usBetween(1, 1000)) }

// acquire acquires key on instance i and keeps an allowed permit; with more than 6 held, the
// oldest is finished first (done, or released and waited for).
func (g *cor1) acquire(i int, key string) driver.Op {
	g.t.Helper()
	op := g.d.Acquire(i, key)
	rec := cor1Op{kind: "acquire", result: op.Kind.String(), retry: op.RetryAfterNs, decision: op.DecisionNs}
	if op.Replied && len(op.Reply.Elems) > 3 {
		rec.units = op.Reply.Elems[3].Int
	}
	if op.Kind == limiter.Allowed {
		rec.expiryUs = expiryOf(op)
		g.held = append(g.held, op)
	}
	g.ops = append(g.ops, rec)
	if len(g.held) > 6 {
		g.finish(g.held[0])
	}
	return op
}

// expiryOf is an allowed acquire's expiry_us (§A4.9: element 6 of E_ALLOWED).
func expiryOf(op driver.Op) int64 {
	if !op.Replied || len(op.Reply.Elems) < 7 {
		return 0
	}
	return op.Reply.Elems[6].Int
}

// drop removes op from the held permits.
func (g *cor1) drop(op driver.Op) {
	for k, h := range g.held {
		if h.Permit == op.Permit {
			g.held = append(g.held[:k], g.held[k+1:]...)
			return
		}
	}
}

func (g *cor1) done(op driver.Op) {
	g.drop(op)
	g.d.Done(op)
	g.ops = append(g.ops, cor1Op{kind: "done"})
}

// release releases op and waits for its final event; the op records the result, the credited
// units of an applied refund (from the captured attempt) and the credit's decision time.
func (g *cor1) release(op driver.Op) limiter.RefundEvent {
	g.t.Helper()
	g.drop(op)
	g.d.Release(op)
	ev, err := g.d.WaitRefund(op, 10*time.Second)
	if err != nil {
		g.t.Fatal(err)
	}
	rec := cor1Op{kind: "release", result: ev.Result.String(), decision: ev.CreditDecisionNs}
	if as := g.d.RefundAttempts(op); len(as) > 0 && ev.Result == limiter.Applied {
		rec.units = as[len(as)-1].Credited()
	}
	g.ops = append(g.ops, rec)
	return ev
}

// finish ends op one way or the other: done (2 in 3) or release.
func (g *cor1) finish(op driver.Op) {
	if g.rng.IntN(3) == 0 {
		g.release(op)
		return
	}
	g.done(op)
}

// drain acquires key on instance i until a limited reply and returns it; each allow is
// finished (or kept) at random. It gives up after 3 × capacity + 6 acquires.
func (g *cor1) drain(i int, key string) (driver.Op, bool) {
	for range 3*g.p.Capacity + 6 {
		op := g.acquire(i, key)
		if op.Kind == limiter.Limited {
			return op, true
		}
		if op.Kind != limiter.Allowed {
			g.t.Fatalf("draining %q: %v (%+v), want allowed or limited", key, op.Kind, op.Diag)
		}
		if g.rng.IntN(4) != 0 {
			g.finish(op)
		}
	}
	return driver.Op{}, false
}

// sceneBoundary: after a limited reply with u, an acquire exactly when u reaches
// rate_period_ns, when that is a whole number of µs away; otherwise one more allow at the
// next whole µs moves u's residue by −rate_period_ns, and the boundary comes within 3 tries
// (units are whole thousands, so the residue mod 1000·rate_tokens takes at most 3 values).
func (g *cor1) sceneBoundary() {
	i, key := g.inst(), g.key()
	g.aboveHWM()
	lim, ok := g.drain(i, key)
	for try := 0; ok && try < 4; try++ {
		need := g.p.RatePeriodNs - replyU(g.t, lim)
		if need%(g.p.RateTokens*1000) == 0 {
			g.set(lim.DecisionNs + need/g.p.RateTokens)
			g.acquire(i, key)
			return
		}
		g.set(lim.DecisionNs + ceilUs(ceilDiv(need, g.p.RateTokens)))
		if op := g.acquire(i, key); op.Kind != limiter.Allowed {
			return
		}
		lim = g.acquire(i, key)
		ok = lim.Kind == limiter.Limited
	}
}

// acquireAllowed acquires key on instance i, after a jump to full when it is limited.
func (g *cor1) acquireAllowed(i int, key string) (driver.Op, bool) {
	op := g.acquire(i, key)
	if op.Kind == limiter.Allowed {
		return op, true
	}
	g.forward(g.fullNs())
	op = g.acquire(i, key)
	return op, op.Kind == limiter.Allowed
}

// sceneExpiry: an allow a above the high-water mark and, 1 µs later, an allow b of any key
// that has a token (decided at the clock, so b expires 1 µs after a); both leave the held
// permits, so nothing else finishes them. A few ops inside the lifetime follow (the clock stays
// below a's expiry − 1 µs), then one refund 1 µs before its receipt's expiry (applied) and the
// other at exactly its expiry (receipt_expired), in either order: b at a's expiry is 1 µs
// before its own. Without b, a alone is released at one of the two instants.
func (g *cor1) sceneExpiry() {
	g.aboveHWM()
	a, ok := g.acquireAllowed(g.inst(), g.key())
	if !ok {
		return
	}
	g.drop(a)
	g.forward(1000)
	var b driver.Op
	for _, k := range g.rng.Perm(len(g.keys)) {
		if op := g.acquire(g.inst(), g.keys[k]); op.Kind == limiter.Allowed {
			b = op
			g.drop(b)
			break
		}
	}
	lifetime := g.p.ReceiptLifetimeNs
	for n := g.rng.IntN(5); n > 0; n-- { // at most 4 × lifetime / 20 forward
		g.forward(g.usBetween(1, lifetime/20_000))
		if op := g.acquire(g.inst(), g.key()); op.Kind == limiter.Allowed && g.rng.IntN(2) == 0 {
			g.done(op)
		}
	}
	ea := expiryOf(a) * 1000
	exact := g.rng.IntN(2) == 0
	if b.Permit == nil {
		if exact {
			g.set(ea)
		} else {
			g.set(ea - 1000)
		}
		g.release(a)
		return
	}
	eb := expiryOf(b) * 1000
	if exact {
		g.set(ea)
		g.release(a) // at exactly its expiry
		g.set(eb - 1000)
		g.release(b) // 1 µs before its expiry, at a's expiry
		return
	}
	g.set(ea - 1000)
	g.release(a) // 1 µs before its expiry
	g.set(eb)
	g.release(b) // at exactly its expiry
}

// sceneFull: a jump to full, an acquire (u = cap − rp), then, a token's refill later and well
// inside the lifetime, its release: the bucket is full again, so the credit is 0.
func (g *cor1) sceneFull() {
	g.forward(g.fullNs() + g.usBetween(0, 1000))
	op := g.acquire(g.inst(), g.key())
	if op.Kind != limiter.Allowed {
		return
	}
	g.forward(g.tokenNs() + g.usBetween(0, min(1_000_000, g.p.ReceiptLifetimeNs/4_000)))
	g.release(op)
}

// sceneBack: a limited acquire 1 µs to 1 ms above the high-water mark (the bucket stays
// below a token), then a step back of 1 µs to 2 s and an acquire, and a step back below the
// high-water mark right after an allow.
func (g *cor1) sceneBack() {
	i, key := g.inst(), g.key()
	g.aboveHWM()
	lim, ok := g.drain(i, key)
	if !ok {
		return
	}
	// The bucket gains rt units per ns; stay under half the way to the next token.
	room := (g.p.RatePeriodNs - replyU(g.t, lim)) / g.p.RateTokens / 2 / 1000 // µs
	if room >= 1 {
		g.forward(g.usBetween(1, min(room, 1000)))
		if op := g.acquire(i, key); op.Kind == limiter.Limited {
			g.back(g.backStep())
			g.acquire(g.inst(), g.key())
		}
	}
	g.forward(g.fullNs())
	if op := g.acquire(g.inst(), g.key()); op.Kind == limiter.Allowed {
		g.back(g.backStep()) // the allow wrote the high-water mark at the clock
		g.acquire(g.inst(), g.key())
	}
}

// backStep is a step back of 1 µs, 2 s, or a whole number of µs between.
func (g *cor1) backStep() int64 {
	switch g.rng.IntN(4) {
	case 0:
		return 1000
	case 1:
		return 2_000_000_000
	}
	return g.usBetween(1, 2_000_000)
}

// sceneJump: a jump of one to two times the time to full, then acquires of 2 or 3 keys.
func (g *cor1) sceneJump() {
	g.forward(g.fullNs() + g.usBetween(0, g.fullNs()/1000))
	for n := 2 + g.rng.IntN(2); n > 0; n-- {
		g.acquire(g.inst(), g.key())
	}
}

// randomOps runs n random ops.
func (g *cor1) randomOps(n int) {
	for range n {
		switch r := g.rng.IntN(10); {
		case r < 5:
			g.acquire(g.inst(), g.key())
		case r < 7 && len(g.held) > 0:
			g.finish(g.held[g.rng.IntN(len(g.held))])
		default:
			g.randomStep()
		}
	}
}

// randomStep is one clock step: forward 1 µs to 300 ms (5 in 6) or back (1 in 6).
func (g *cor1) randomStep() {
	if g.rng.IntN(6) == 0 {
		g.back(g.backStep())
		return
	}
	g.forward(g.usBetween(1, 300_000))
}
