//go:build rltest

package spectest

import (
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/internal/store"
	"ratelimiter/limiter"
)

// interval is the driver's epoch re-read interval (DESIGN §A5.10: 1 s at the harness values).
func interval(d *driver.Driver) time.Duration {
	return time.Duration(d.Instance().EpochRereadIntervalNs)
}

// waitHealth waits until l reports status and reason, by deadline.
func waitHealth(l *limiter.Limiter, status, reason string, deadline time.Time) error {
	for {
		h := l.Health()
		if h.Status == status && h.Reason == reason {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("health %s/%s, want %s/%s", h.Status, h.Reason, status, reason)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// retryAfter is SPEC API-6's exact wait, computed with math/big: ⌈(rp − u) / rt⌉ ns.
func retryAfter(u, rp, rt int64) int64 {
	n := new(big.Int).Sub(big.NewInt(rp), big.NewInt(u))
	q, m := new(big.Int).QuoRem(n, big.NewInt(rt), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// replyU is a limited or allowed acquire's u from its reply (§A4.9: element 3).
func replyU(t *testing.T, op driver.Op) int64 {
	t.Helper()
	if !op.Replied || len(op.Reply.Elems) < 4 {
		t.Fatalf("%s acquire of %q has no reply u", op.Kind, op.Key)
	}
	return op.Reply.Elems[3].Int
}

// doneAll finishes every permit of ops.
func doneAll(ops []driver.Op) {
	for _, op := range ops {
		if op.Permit != nil {
			op.Permit.Done()
		}
	}
}

// step advances d's controlled clock by ns, failing the test on a refusal.
func step(t *testing.T, d *driver.Driver, ns int64) {
	t.Helper()
	if err := d.Clock().Advance(ns); err != nil {
		t.Fatal(err)
	}
}

// waitMax bounds every wait for a refund event or a state the test drives to (plan choice).
const waitMax = 10 * time.Second

// fastDriver returns a driver of n instances on driver.FastPolicy and driver.FastInstance
// (plan S6 step 10), with edit, when not nil, applied to both first.
func fastDriver(t *testing.T, n int, edit func(*limiter.Policy, *limiter.Instance)) *driver.Driver {
	t.Helper()
	p := driver.FastPolicy()
	in := driver.FastInstance(p)
	if edit != nil {
		edit(&p, &in)
	}
	return driver.New(t, driver.Options{Instances: n, Policy: p, Instance: in})
}

// allowed acquires key on instance i and fails the test unless it is allowed.
func allowed(t *testing.T, d *driver.Driver, i int, key string) driver.Op {
	t.Helper()
	op := d.Acquire(i, key)
	if op.Kind != limiter.Allowed || op.Permit == nil {
		t.Fatalf("acquire of %q on instance %d: %v (%+v), want allowed", key, i, op.Kind, op.Diag)
	}
	return op
}

// event waits up to waitMax for the final refund event of op's permit.
func event(t *testing.T, d *driver.Driver, op driver.Op) limiter.RefundEvent {
	t.Helper()
	ev, err := d.WaitRefund(op, waitMax)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// refundSends is the number of refund commands instance i wrote bytes of.
func refundSends(d *driver.Driver, i int) int { return driver.Count(d.Sent(i), "refund") }

// metric returns instance i's value of the series of family name with label value label.
func metric(t *testing.T, d *driver.Driver, i int, name, label string) float64 {
	t.Helper()
	v, ok := driver.MetricValue(d.Limiter(i).Metrics(), name, label)
	if !ok {
		t.Fatalf("instance %d has no series %s{%q}", i, name, label)
	}
	return v
}

// slotsInUse returns instance i's rl_slots_in_use for receipts and refunds.
func slotsInUse(t *testing.T, d *driver.Driver, i int) (receipts, refunds float64) {
	t.Helper()
	return metric(t, d, i, "rl_slots_in_use", "receipt"), metric(t, d, i, "rl_slots_in_use", "refund")
}

// waitUntil polls cond every 2 ms until it holds or deadline passes, and reports whether it
// held.
func waitUntil(deadline time.Time, cond func() bool) bool {
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// token is one token of p in units: rate_period_ns (BUD-13).
func token(p limiter.Policy) int64 { return p.RatePeriodNs }

// modes are the failure modes of a "both modes" test: one subtest per mode, each with its own
// driver and epoch, since the mode is fingerprinted (plan S8 Tests (b)).
func modes() []limiter.FailureMode { return []limiter.FailureMode{limiter.Closed, limiter.Open} }

// modeDriver returns fastDriver(t, n, edit) with failure_mode mode.
func modeDriver(t *testing.T, n int, mode limiter.FailureMode, edit func(*limiter.Policy, *limiter.Instance)) *driver.Driver {
	t.Helper()
	return fastDriver(t, n, func(p *limiter.Policy, in *limiter.Instance) {
		p.FailureMode = mode
		if edit != nil {
			edit(p, in)
		}
	})
}

// bothModes runs f in one subtest per failure mode, each on its own driver of n instances
// (modeDriver).
func bothModes(t *testing.T, n int, edit func(*limiter.Policy, *limiter.Instance), f func(t *testing.T, d *driver.Driver)) {
	t.Helper()
	for _, m := range modes() {
		t.Run(m.String(), func(t *testing.T) { f(t, modeDriver(t, n, m, edit)) })
	}
}

// joinTolerance is the plan's join tolerance (S8 Tests (b), as for T-CFG-2): one re-read
// interval + 100 ms.
func joinTolerance(d *driver.Driver) time.Duration { return interval(d) + 100*time.Millisecond }

// refusedIntegrity fails the test unless op is a state_integrity_error of scope ("key" or
// "epoch") and reason, with no permit: never degraded, in either mode (STA-9, API-9).
func refusedIntegrity(t *testing.T, what string, op driver.Op, scope, reason string) {
	t.Helper()
	if op.Kind != limiter.StateIntegrityError || op.Permit != nil || op.Diag.Scope != scope || op.Diag.Reason != reason {
		t.Errorf("%s: %v %+v (permit %v), want state_integrity_error, scope %s, reason %s, no permit", what, op.Kind, op.Diag, op.Permit != nil, scope, reason)
	}
}

// firstAttempt waits up to waitMax for the first refund call of op's permit to return from the
// store adapter, and returns it.
func firstAttempt(t *testing.T, d *driver.Driver, op driver.Op) driver.RefundAttempt {
	t.Helper()
	var as []driver.RefundAttempt
	if !waitUntil(time.Now().Add(waitMax), func() bool { as = d.RefundAttempts(op); return len(as) > 0 }) {
		t.Fatalf("no refund call of %q returned within %v", op.Key, waitMax)
	}
	return as[0]
}

// storeReply fails the test unless refund attempt a reached the store and its adapter returned
// code with reason.
func storeReply(t *testing.T, what string, a driver.RefundAttempt, code int64, reason string) {
	t.Helper()
	if !a.Reached || a.Store.Outcome != store.OK || a.Store.Reply.Code != code || a.Store.Reply.Reason != reason {
		t.Errorf("%s: refund attempt reached %v, store outcome %v, reply %d %q; want reply %d %q", what, a.Reached, a.Store.Outcome, a.Store.Reply.Code, a.Store.Reply.Reason, code, reason)
	}
}

// sameBucket reports whether two inspects show the same bucket: presence, u, t, g, s, x, the
// marker count and the marker expiries (not the index score or the quarantine flag).
func sameBucket(a, b driver.Inspect) bool {
	return a.Code == b.Code && a.Present == b.Present && a.U == b.U && a.T == b.T && a.G == b.G && a.S == b.S &&
		a.X == b.X && a.MC == b.MC && slices.Equal(a.Expiries, b.Expiries)
}

// waitAllHealth waits until every instance of d reports status and reason, by deadline.
func waitAllHealth(t *testing.T, d *driver.Driver, status, reason string, deadline time.Time) {
	t.Helper()
	for i := range d.Instances() {
		if err := waitHealth(d.Limiter(i), status, reason, deadline); err != nil {
			t.Errorf("instance %d: %v", i, err)
		}
	}
}

// waitAllJoined waits until every instance of d is ready in epoch id, by deadline.
func waitAllJoined(t *testing.T, d *driver.Driver, id string, deadline time.Time) {
	t.Helper()
	for i := range d.Instances() {
		if err := d.WaitJoined(i, id, time.Until(deadline)); err != nil {
			t.Error(err)
		}
	}
}

// durableProfile fails the test unless d's store runs the durable profile (plan S8 Tests (b):
// T-INT-11 and T-CLK-5; deploy/valkey-durable.conf): CONFIG GET appendfsync, run as rl_ops,
// answers always (the fast profile's is no).
func durableProfile(t *testing.T, d *driver.Driver) {
	t.Helper()
	c, err := d.Store().Conn("rl_ops")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	r, err := c.Do("CONFIG", "GET", "appendfsync")
	if err != nil || len(r.Arr) != 2 || r.Arr[0].Str != "appendfsync" || r.Arr[1].Str != "always" {
		t.Fatalf("CONFIG GET appendfsync as rl_ops: %v, %v; want appendfsync always (the durable profile)", r, err)
	}
}

// acquireSettled acquires key on instance i, again while the result is a transport failure
// (unavailable or outcome_unknown with an empty Diag: a connection a store restart closed and
// the adapter has not redialed yet, or a call past its deadline D, §A5.4), up to waitMax, and
// returns the first other result. Tests use it where the store restarts or fsyncs every write
// (the durable profile) and the transport is not what they check; an outcome_unknown may have
// charged, which leaves fewer allows, never more (check_bud11 counts it as no charge).
func acquireSettled(t *testing.T, d *driver.Driver, i int, key string) driver.Op {
	t.Helper()
	deadline := time.Now().Add(waitMax)
	for {
		op := d.Acquire(i, key)
		transport := (op.Kind == limiter.Unavailable || op.Kind == limiter.OutcomeUnknown) && op.Diag == (limiter.Diag{})
		if !transport || !time.Now().Before(deadline) {
			return op
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// allowedSettled is acquireSettled that fails the test unless the result is allowed.
func allowedSettled(t *testing.T, d *driver.Driver, i int, key string) driver.Op {
	t.Helper()
	op := acquireSettled(t, d, i, key)
	if op.Kind != limiter.Allowed || op.Permit == nil {
		t.Fatalf("acquire of %q on instance %d: %v (%+v), want allowed", key, i, op.Kind, op.Diag)
	}
	return op
}

// eventLoop is the store's own record of its event loop, from INFO debug, in µs since it
// started: eventloop_duration_max, its longest cycle, and eventloop_duration_aof_sum, its time
// writing the AOF (Valkey writes it before it replies, on the fast profile too).
type eventLoop struct{ maxUs, aofUs int64 }

// readEventLoop reads d's store's eventLoop from INFO debug as rl_ops (Driver.Info), failing
// the test on a missing or malformed value.
func readEventLoop(t *testing.T, d *driver.Driver) eventLoop {
	t.Helper()
	info := d.Info("debug")
	field := func(name string) int64 {
		n, err := strconv.ParseInt(info[name], 10, 64)
		if err != nil || n < 0 {
			t.Fatalf("INFO debug as rl_ops: %s is %q, want a count of µs", name, info[name])
		}
		return n
	}
	return eventLoop{maxUs: field("eventloop_duration_max"), aofUs: field("eventloop_duration_aof_sum")}
}

// allowedThroughAOFStall is allowed, but acquires the same key again after a transport failure
// (unavailable or outcome_unknown with an empty Diag: the reply missed D, §A5.4) when the store's
// own record shows an AOF stall of at least D around the acquire: in INFO debug, read as rl_ops
// just before the acquire and just after it, eventloop_duration_max (one cycle) is at least D
// and eventloop_duration_aof_sum grew by at least D in between. A host F_FULLFSYNC stall blocks
// the store's AOF write(2) that way, beyond D (plan F6 R03). Each retry logs the values; without
// that record, or waitMax after the first try, the test fails as allowed does. Only for setup
// loops on the fast profile whose results the test does not check, and never where the op log
// is replayed (CheckRefModel): the first try may have run, and the reference model takes an
// outcome_unknown to change nothing. A retry of the same key keeps its one bucket and its one ix
// member (acquire.lua); a first try that ran leaves fewer allows, never more (check_bud11
// counts it as no charge).
func allowedThroughAOFStall(t *testing.T, d *driver.Driver, i int, key string) driver.Op {
	t.Helper()
	dUs := d.Instance().DependencyDeadlineNs / 1000
	deadline := time.Now().Add(waitMax)
	for {
		before := readEventLoop(t, d)
		op := d.Acquire(i, key)
		if op.Kind == limiter.Allowed && op.Permit != nil {
			return op
		}
		if (op.Kind != limiter.Unavailable && op.Kind != limiter.OutcomeUnknown) || op.Diag != (limiter.Diag{}) {
			t.Fatalf("acquire of %q on instance %d: %v (%+v), want allowed", key, i, op.Kind, op.Diag)
		}
		after := readEventLoop(t, d)
		values := fmt.Sprintf("INFO debug: eventloop_duration_max %d µs, eventloop_duration_aof_sum %d → %d µs (+%d); D %d µs",
			after.maxUs, before.aofUs, after.aofUs, after.aofUs-before.aofUs, dUs)
		switch {
		case after.maxUs < dUs || after.aofUs-before.aofUs < dUs:
			t.Fatalf("acquire of %q on instance %d: %v (%+v), want allowed; the store shows no AOF stall of at least D around it (%s)",
				key, i, op.Kind, op.Diag, values)
		case !time.Now().Before(deadline):
			t.Fatalf("acquire of %q on instance %d: %v (%+v), want allowed; still failing %v after its first try, in an AOF stall (%s)",
				key, i, op.Kind, op.Diag, waitMax, values)
		}
		t.Logf("acquire of %q on instance %d: %v with an empty Diag in an AOF stall (%s); acquiring it again", key, i, op.Kind, values)
	}
}

// releaseConfirmed releases op's permit and fails the test unless its final event confirms the
// refund: applied, or already_applied when an attempt's reply was lost after the store applied
// it (API-14).
func releaseConfirmed(t *testing.T, d *driver.Driver, op driver.Op) limiter.RefundEvent {
	t.Helper()
	d.Release(op)
	ev := event(t, d, op)
	if ev.Result != limiter.Applied && ev.Result != limiter.AlreadyApplied {
		t.Fatalf("release of %q (%s): %+v, want applied or already_applied", op.Key, op.Permit.DeductionID(), ev)
	}
	return ev
}

// passSettled runs cleanup passes on instance i until one completes, again while a pass stops on
// the transport alone (transportStop), up to waitMax, and fails the test otherwise.
func passSettled(t *testing.T, d *driver.Driver, i int) limiter.CleanupReport {
	t.Helper()
	deadline := time.Now().Add(waitMax)
	for {
		rep := d.RunCleanupPass(i)
		if rep.Status == "complete" {
			return rep
		}
		if !transportStop(rep) || !time.Now().Before(deadline) {
			t.Fatalf("cleanup pass on instance %d: %+v, want complete", i, rep)
			return rep
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// s9DepNs is the dependency deadline D of the S9 SPEC tests unless a test states another (plan
// S9 Tests (b) choice): 500 ms.
const s9DepNs = 500_000_000

// hourNs is an hour in ns: an epoch re-read interval or cleanup interval that never comes
// during a test, so an instance writes only the test's own commands (plan choice, as in
// TestDesign_PipelinedTimeout).
const hourNs = 3_600_000_000_000

// s9Driver returns a driver of n instances on the harness policy and instance settings
// (driver.DefaultPolicy, driver.DefaultInstance) with failure_mode mode and D = 500 ms, then edit
// (when not nil) applied to both, and specs for the first instances. The §A5.6 inequality holds
// at D = 500 ms: 1 + 1 + (⌈1152 / 128⌉ + 3) × 0.5 = 8 s ≤ 30 s.
func s9Driver(t *testing.T, n int, mode limiter.FailureMode, edit func(*limiter.Policy, *limiter.Instance), specs ...driver.InstanceSpec) *driver.Driver {
	t.Helper()
	p := driver.DefaultPolicy()
	p.FailureMode = mode
	in := driver.DefaultInstance(p)
	in.DependencyDeadlineNs = s9DepNs
	if edit != nil {
		edit(&p, &in)
	}
	return driver.New(t, driver.Options{Instances: n, Policy: p, Instance: in, PerInstance: specs})
}

// s9BothModes runs f in one subtest per failure mode, each on its own s9Driver.
func s9BothModes(t *testing.T, n int, edit func(*limiter.Policy, *limiter.Instance), specs []driver.InstanceSpec,
	f func(t *testing.T, d *driver.Driver, mode limiter.FailureMode)) {
	t.Helper()
	for _, m := range modes() {
		t.Run(m.String(), func(t *testing.T) { f(t, s9Driver(t, n, m, edit, specs...), m) })
	}
}

// quiet sets the epoch re-read interval and the cleanup interval to an hour: the epoch is read
// once at startup and no timed pass runs, so the instances write only the test's acquires.
func quiet(p *limiter.Policy, in *limiter.Instance) {
	p.CleanupIntervalNs, in.EpochRereadIntervalNs = hourNs, hourNs
}

// notSentRefusal reports whether op is the failure-mode result of an acquire whose command was
// not sent (API-11, API-20a, API-20b): unavailable in closed mode; in open mode degraded with a
// permit that carries no deduction_id (API-5). Either way nothing was sent (SentCommands 0).
func notSentRefusal(op driver.Op, mode limiter.FailureMode) bool {
	if op.SentCommands != 0 {
		return false
	}
	if mode == limiter.Open {
		return op.Kind == limiter.Degraded && op.Permit != nil && op.Permit.DeductionID() == ""
	}
	return op.Kind == limiter.Unavailable && op.Permit == nil
}

// finishDegraded finishes a degraded permit (a no-op, API-5), so no permit is left open.
func finishDegraded(op driver.Op) {
	if op.Kind == limiter.Degraded && op.Permit != nil {
		op.Permit.Done()
	}
}

// firstWord is the first space-delimited word of an error reply's text.
func firstWord(s string) string {
	w, _, _ := strings.Cut(s, " ")
	return w
}
