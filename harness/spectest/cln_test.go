//go:build rltest

package spectest

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/driver"
	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/limiter"
)

// The cleanup tests (IMPLEMENTATION_PLAN S7 Tests (b)): the driver on driver.FastPolicy (a 6 s
// receipt lifetime, timed passes off at a 3,600 s cleanup interval) with deterministic passes
// (RunCleanupPass) unless a test says otherwise. Each sequential one bridges its op log, with
// the cleanup and cleanup_scan records, to the reference model.

// second is one budget-second in ns.
const second int64 = 1_000_000_000

// epochReads bounds a wait for an epoch read to show a state (plan choice): two re-read
// intervals and a second.
func epochReads(d *driver.Driver) time.Duration { return 2*interval(d) + time.Second }

// waitMetric polls instance i's series name{label} until want holds for it, up to within, and
// fails the test with the last value otherwise.
func waitMetric(t *testing.T, d *driver.Driver, i int, name, label string, within time.Duration, what string, want func(float64) bool) {
	t.Helper()
	var v float64
	if !waitUntil(time.Now().Add(within), func() bool { v = metric(t, d, i, name, label); return want(v) }) {
		t.Fatalf("%s: %s{%q} = %v after %v", what, name, label, v, within)
	}
}

// passOK runs a cleanup pass on instance i and fails the test unless it completes.
func passOK(t *testing.T, d *driver.Driver, i int) limiter.CleanupReport {
	t.Helper()
	rep := d.RunCleanupPass(i)
	if rep.Status != "complete" {
		t.Fatalf("cleanup pass on instance %d: %+v, want complete", i, rep)
	}
	return rep
}

// setClock writes the controlled time ns.
func setClock(t *testing.T, d *driver.Driver, ns int64) {
	t.Helper()
	if err := d.Clock().Set(ns); err != nil {
		t.Fatal(err)
	}
}

// nsKeys returns the store's keys in the namespace of the current epoch (rl:e:<id>:*).
func nsKeys(t *testing.T, d *driver.Driver) []string {
	t.Helper()
	id := d.ReadEpoch().ID
	if id == "" {
		t.Fatal("no readable epoch record")
	}
	return d.StoreKeys("rl:e:" + id + ":*")
}

// releaseApplied releases op's permit and fails the test unless its final event is applied.
func releaseApplied(t *testing.T, d *driver.Driver, op driver.Op) limiter.RefundEvent {
	t.Helper()
	d.Release(op)
	ev := event(t, d, op)
	if ev.Result != limiter.Applied {
		t.Fatalf("release of %q (%s): %+v, want applied", op.Key, op.Permit.DeductionID(), ev)
	}
	return ev
}

// releaseExpired releases op's permit and fails the test unless its final event is
// receipt_expired.
func releaseExpired(t *testing.T, d *driver.Driver, op driver.Op) limiter.RefundEvent {
	t.Helper()
	d.Release(op)
	ev := event(t, d, op)
	if ev.Result != limiter.ReceiptExpired {
		t.Fatalf("release of %q (%s): %+v, want receipt_expired", op.Key, op.Permit.DeductionID(), ev)
	}
	return ev
}

// cln1Keys sets up T-CLN-1's 200 keys on d at its frozen start t0: each gets 3 acquires through
// allow, which fails the test unless one is allowed (allowed, or allowedThroughAOFStall where
// the op log is not replayed), 2 of them finished with done() and 1 released and applied (its
// marker expires with the receipts, at t0 + 6 s).
func cln1Keys(t *testing.T, d *driver.Driver, allow func(*testing.T, *driver.Driver, int, string) driver.Op) []string {
	t.Helper()
	ks := make([]string, 200)
	for k := range ks {
		ks[k] = fmt.Sprintf("cln1-%03d", k)
		ops := []driver.Op{allow(t, d, k%d.Instances(), ks[k]), allow(t, d, k%d.Instances(), ks[k]), allow(t, d, k%d.Instances(), ks[k])}
		d.Done(ops[0])
		d.Done(ops[1])
		releaseApplied(t, d, ops[2])
	}
	return ks
}

// TestT_CLN_1 (SPEC T-CLN-1; STA-3, STA-4, STA-5): 200 keys, each with 3 acquires and 1 applied
// release at a frozen t0, then idle. Every receipt and every marker expires at E = t0 + 6 s,
// and the buckets are full 0.2 s after t0. Before cleanup the epoch reads show 200 tracked
// buckets and 200 live markers. A pass started at E − 1 µs completes and removes nothing (no
// entry is due: Removed, Moved and Skipped 0; the 200 buckets stay); the first pass started at
// E removes all 200 (Removed 200, nothing else; each key's step 30 at E takes its marker with
// it), rl:e:<id>:* is empty, and after the next epoch reads rl_tracked_buckets =
// rl_live_markers = 0. The op log matches the reference model. A
// second deployment with cleanup_interval 1 s and no RunCleanupPass, same keys and ops: once
// its clock reaches E, rl:e:<id>:* is empty within 2 s.
func TestT_CLN_1(t *testing.T) {
	d := fastDriver(t, 2, nil)
	t0 := d.Clock().NowNs()
	expiry := t0 + d.Policy().ReceiptLifetimeNs
	ks := cln1Keys(t, d, allowed) // its op log is replayed (CheckRefModel)
	waitMetric(t, d, 0, "rl_tracked_buckets", "", epochReads(d), "before cleanup", func(v float64) bool { return v == 200 })
	waitMetric(t, d, 0, "rl_live_markers", "", epochReads(d), "before cleanup", func(v float64) bool { return v == 200 })

	setClock(t, d, expiry-1000)
	if rep := passOK(t, d, 0); rep.Removed != 0 || rep.Moved != 0 || rep.Skipped != 0 || rep.Quarantined != 0 {
		t.Errorf("pass at E − 1 µs: %+v, want nothing handled", rep)
	}
	if n := len(nsKeys(t, d)); n != 2*len(ks)+1 { // k: and mi: per key, and ix
		t.Errorf("after the pass at E − 1 µs: %d keys under rl:e:<id>:, want %d (k and mi per key, ix)", n, 2*len(ks)+1)
	}

	setClock(t, d, expiry)
	if rep := passOK(t, d, 1); rep.Removed != int64(len(ks)) || rep.Moved != 0 || rep.Skipped != 0 || rep.Quarantined != 0 {
		t.Errorf("first pass started at E: %+v, want Removed %d and nothing else", rep, len(ks))
	}
	removed := map[string]bool{}
	for _, st := range d.CleanupSteps() {
		if st.Code == 30 && st.Markers == 1 && st.TUs == expiry/1000 {
			removed[st.Key] = true
		}
	}
	if len(removed) != len(ks) {
		t.Errorf("%d keys removed at E with their marker, want all %d", len(removed), len(ks))
	}
	if left := nsKeys(t, d); len(left) != 0 {
		t.Errorf("after the pass at E: %d keys left under rl:e:<id>:, want none (first: %q)", len(left), left[0])
	}
	waitMetric(t, d, 0, "rl_tracked_buckets", "", epochReads(d), "after cleanup", func(v float64) bool { return v == 0 })
	waitMetric(t, d, 0, "rl_live_markers", "", epochReads(d), "after cleanup", func(v float64) bool { return v == 0 })
	d.CheckRefModel(t)

	// Timed passes only.
	timed := fastDriver(t, 2, func(p *limiter.Policy, _ *limiter.Instance) { p.CleanupIntervalNs = second })
	cln1Keys(t, timed, allowedThroughAOFStall) // setup: a reply an AOF stall the store records delays past D is retried
	if n := len(nsKeys(t, timed)); n != 2*len(ks)+1 {
		t.Fatalf("timed deployment before E: %d keys under rl:e:<id>:, want %d", n, 2*len(ks)+1)
	}
	id := timed.ReadEpoch().ID
	setClock(t, timed, timed.Clock().NowNs()+timed.Policy().ReceiptLifetimeNs)
	deadline := time.Now().Add(2 * time.Second)
	for {
		left := timed.StoreKeys("rl:e:" + id + ":*")
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed passes only: %d keys left under rl:e:<id>: 2 s after the clock reached E, want none", len(left))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestT_CLN_2 (SPEC T-CLN-2; RCP-7): 50 keys, 30 rounds over 10 budget-seconds (a step of
// 333,333 µs before each round), each round one acquire per key finished with done(): every
// acquire is allowed (3 a second per key, under the rate of 10 a second and the burst of 20).
// Successful-only traffic stores no receipt record and no marker: once an epoch read shows the
// 50 tracked buckets, rl_live_markers is 0; every key's inspect shows mc 0 and no marker
// expiry; and the store holds exactly the 50 k: keys, ix, rl:epoch and rl:ctl:time (no mi:, no
// q). The op log matches the reference model.
func TestT_CLN_2(t *testing.T) {
	d := fastDriver(t, 2, nil)
	ks := make([]string, 50)
	for k := range ks {
		ks[k] = fmt.Sprintf("cln2-%02d", k)
	}
	for r := range 30 {
		step(t, d, 333_333_000)
		for k, key := range ks {
			d.Done(allowed(t, d, (r+k)%2, key))
		}
	}
	waitMetric(t, d, 0, "rl_tracked_buckets", "", epochReads(d), "after the traffic", func(v float64) bool { return v == 50 })
	if v := metric(t, d, 0, "rl_live_markers", ""); v != 0 {
		t.Errorf("rl_live_markers = %v with 50 tracked buckets, want 0", v)
	}
	id := d.ReadEpoch().ID
	want := []string{"rl:ctl:time", "rl:e:" + id + ":ix", "rl:epoch"}
	for _, key := range ks {
		if in := d.Inspect(key); in.Present != 1 || in.MC != 0 || len(in.Expiries) != 0 || in.S != 30 {
			t.Errorf("inspect %q: %+v, want present, mc 0, no marker, s 30", key, in)
		}
		want = append(want, "rl:e:"+id+":k:"+b64(key))
	}
	slices.Sort(want)
	if got := d.StoreKeys("rl:*"); !slices.Equal(got, want) {
		t.Errorf("store keys:\n%q\nwant the 50 k: keys, ix, rl:epoch and rl:ctl:time:\n%q", got, want)
	}
	d.CheckRefModel(t)
}

// b64 is a key's enc: unpadded base64url of its bytes (DESIGN §A3.2).
func b64(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

// TestT_CLN_3 (SPEC T-CLN-3; RCP-7, STA-5): a fixed population of 50 keys over 120 steps of one
// budget-second. Each step takes 2 allows per key (100 permits); 5 of them (5 %, rotating over
// the keys) are released and applied, the rest finished with done(); a pass runs after every
// 5th step. Stored state plateaus: nb and nm (driver.Status) over steps 81–120 never exceed
// their maxima over steps 41–80, and nb ≤ 50 at every step. The passes remove expired markers
// (Moved > 0, Removed 0: every key stays active). The op log matches the reference model.
func TestT_CLN_3(t *testing.T) {
	d := fastDriver(t, 2, nil)
	ks := make([]string, 50)
	for k := range ks {
		ks[k] = fmt.Sprintf("cln3-%02d", k)
	}
	var maxNB, maxNM int64 // over steps 41–80
	var moved int64
	for s := 1; s <= 120; s++ {
		step(t, d, second)
		for p := range 2 * len(ks) {
			op := allowed(t, d, p%2, ks[p/2])
			if (p+s)%20 == 0 {
				releaseApplied(t, d, op)
			} else {
				d.Done(op)
			}
		}
		if s%5 == 0 {
			rep := passOK(t, d, s%2)
			if rep.Removed != 0 || rep.Quarantined != 0 {
				t.Errorf("pass after step %d: %+v, want no removal and no quarantine", s, rep)
			}
			moved += rep.Moved
		}
		st := d.Status()
		if st.Code != 40 || st.NB > int64(len(ks)) {
			t.Fatalf("step %d: status %+v, want nb ≤ %d", s, st, len(ks))
		}
		switch {
		case s > 40 && s <= 80:
			maxNB, maxNM = max(maxNB, st.NB), max(maxNM, st.NM)
		case s > 80 && (st.NB > maxNB || st.NM > maxNM):
			t.Errorf("step %d: nb %d, nm %d; want at most the maxima of steps 41–80, nb %d and nm %d", s, st.NB, st.NM, maxNB, maxNM)
		}
	}
	if moved == 0 || maxNM == 0 {
		t.Errorf("the passes moved %d entries and nm peaked at %d over steps 41–80; want markers stored and removed", moved, maxNM)
	}
	d.CheckRefModel(t)
}

// TestT_CLN_4 (SPEC T-CLN-4; STA-2, STA-4): driver instances 1 and 2 (of 0–2). At a frozen
// t0, instance 1 acquires K (generation g1; done) and instance 2 acquires P2 and P3 and keeps
// them OPEN. Instance 1 is killed (FLT-6) and restarted, and serves again; then instance 2's
// release(P2) is applied. Past K's eligibility (t0 + 6 s: every receipt and P2's marker
// expired, the bucket refilled) a pass removes K (its step 30, with P2's marker) and K is gone.
// K returns: at one instant exactly 20 allows, then limited, on a new generation g2 > g1 whose
// first allow has s = 1; release(P3) is receipt_expired. The allows match the reference model.
func TestT_CLN_4(t *testing.T) {
	d := fastDriver(t, 3, nil)
	const key = "cln4"
	t0 := d.Clock().NowNs()
	p1 := allowed(t, d, 1, key)
	g1 := p1.Reply.Elems[4].Int
	d.Done(p1)
	p2, p3 := allowed(t, d, 2, key), allowed(t, d, 2, key)
	d.Kill(1)
	if err := d.Restart(1); err != nil {
		t.Fatal(err)
	}
	if err := d.WaitReady(1, waitMax); err != nil {
		t.Fatal(err)
	}
	d.Done(allowed(t, d, 1, "cln4-other")) // the restarted instance serves
	releaseApplied(t, d, p2)

	setClock(t, d, t0+d.Policy().ReceiptLifetimeNs)
	if rep := passOK(t, d, 1); rep.Removed != 2 || rep.Moved+rep.Skipped+rep.Quarantined != 0 {
		t.Errorf("pass past K's eligibility: %+v, want Removed 2 (K and cln4-other) alone", rep)
	}
	if steps := d.StepsFor(key); len(steps) != 1 || steps[0].Code != 30 || steps[0].Markers != 1 {
		t.Errorf("K's steps %+v, want one 30 removing P2's marker", steps)
	}
	if in := d.Inspect(key); in.Present != 0 {
		t.Errorf("K after the pass: %+v, want absent", in)
	}
	var ops []driver.Op
	for n := 0; ; n++ {
		op := d.Acquire(1+n%2, key)
		if op.Kind != limiter.Allowed {
			if op.Kind != limiter.Limited || n != 20 {
				t.Errorf("K's return: acquire %d is %v after %d allows, want limited after exactly 20", n+1, op.Kind, n)
			}
			break
		}
		ops = append(ops, op)
	}
	if len(ops) > 0 {
		if g2, s := ops[0].Reply.Elems[4].Int, ops[0].Reply.Elems[5].Int; g2 <= g1 || s != 1 {
			t.Errorf("K's new bucket: generation %d, first s %d; want a generation above %d and s 1", g2, s, g1)
		}
	}
	doneAll(ops)
	releaseExpired(t, d, p3)
	d.CheckRefModel(t)
}

// TestT_CLN_5 (SPEC T-CLN-5; STA-3, STA-5): K is acquired once per budget-second from t0 to
// t0 + 19 s, so its bucket never becomes eligible. Two more permits taken at t0 are released
// (markers expiring at t0 + 6 s), and so are the permits of t0 + 3 s and t0 + 8 s (markers
// expiring at t0 + 9 s and t0 + 14 s); the others are done(). A pass runs after each second's
// acquire from t0 + 1 s to t0 + 19 s. Each marker stays until the first pass started at or
// after its expiry, and that pass removes it: the passes at t0 + 6 s, 9 s and 14 s each handle
// K alone (Moved 1; its step 31 with markers_removed 2, 1 and 1), keep the bucket, and move its
// ix score above the step's t; every other pass handles nothing. The op log matches the
// reference model.
func TestT_CLN_5(t *testing.T) {
	d := fastDriver(t, 1, nil)
	const key = "cln5"
	t0 := d.Clock().NowNs()
	life := d.Policy().ReceiptLifetimeNs
	releasedAt := map[int]bool{3: true, 8: true}
	removes := map[int]int64{6: 2, 9: 1, 14: 1} // pass second → markers it removes
	live := []int64{}                           // expiries (µs) of the markers stored
	for s := 0; s <= 19; s++ {
		if s > 0 {
			setClock(t, d, t0+int64(s)*second)
		}
		op := allowed(t, d, 0, key)
		switch {
		case s == 0:
			for range 2 {
				releaseApplied(t, d, allowed(t, d, 0, key))
				live = append(live, (t0+life)/1000)
			}
			d.Done(op)
		case releasedAt[s]:
			releaseApplied(t, d, op)
			live = append(live, (t0+int64(s)*second+life)/1000)
		default:
			d.Done(op)
		}
		if s == 0 {
			continue
		}
		before := len(d.StepsFor(key))
		rep := passOK(t, d, 0)
		now := (t0 + int64(s)*second) / 1000
		var keep []int64
		for _, e := range live {
			if e > now {
				keep = append(keep, e)
			}
		}
		live = keep
		steps := d.StepsFor(key)[before:]
		want, due := removes[s]
		switch {
		case due && (rep.Moved != 1 || rep.Removed+rep.Skipped+rep.Quarantined != 0 || len(steps) != 1 ||
			steps[0].Code != 31 || steps[0].Markers != want || steps[0].TUs != now):
			t.Errorf("pass at t0 + %d s: %+v, steps %+v; want K moved alone, its step 31 at %d removing %d markers", s, rep, steps, now, want)
		case !due && (rep.Removed+rep.Moved+rep.Skipped+rep.Quarantined != 0 || len(steps) != 0):
			t.Errorf("pass at t0 + %d s: %+v, steps %+v; want nothing handled", s, rep, steps)
		}
		in := d.Inspect(key)
		if in.Present != 1 || !slices.Equal(in.Expiries, live) || in.MC != int64(len(live)) {
			t.Errorf("after the pass at t0 + %d s: inspect %+v, want the bucket with markers %v", s, in, live)
		}
		if due && in.IxScore <= now {
			t.Errorf("after the pass at t0 + %d s: ix score %d, want above the step's t %d", s, in.IxScore, now)
		}
	}
	d.CheckRefModel(t)
}

// stepBarrier arms, on instance i's store wrapper, a before_send barrier named name on the
// next cleanup_entry step of key; the scripts are loaded first (LoadScripts), so the barrier
// holds the step itself, not a NOSCRIPT round.
func stepBarrier(t *testing.T, d *driver.Driver, i int, key, name string) {
	t.Helper()
	d.LoadScripts()
	arm(t, d, i, fixtures.Fault{Mode: fixtures.Barrier, Match: "cleanup_entry", Key: key, Count: 1, At: fixtures.BeforeSend, Name: name})
}

// passAsync starts a cleanup pass on instance i, on the test's context bounded by waitMax (a
// pass that would never end stops at its next store call once the bound passes), and returns
// the channel of its report; the test waits for it (passDone) before it returns.
func passAsync(t *testing.T, d *driver.Driver, i int) <-chan limiter.CleanupReport {
	ch := make(chan limiter.CleanupReport, 1)
	l := d.Limiter(i)
	ctx, cancel := context.WithTimeout(t.Context(), waitMax)
	go func() {
		defer cancel()
		ch <- l.RunCleanupPass(ctx)
	}()
	return ch
}

// passDone waits up to waitMax for the report of a pass passAsync started.
func passDone(t *testing.T, ch <-chan limiter.CleanupReport) limiter.CleanupReport {
	t.Helper()
	select {
	case rep := <-ch:
		return rep
	case <-time.After(waitMax):
		t.Fatalf("the cleanup pass did not return within %v", waitMax)
		return limiter.CleanupReport{}
	}
}

// TestT_CLN_6 (SPEC T-CLN-6; STA-4): K, one allow at a frozen t0 (done), is eligible at
// E = t0 + 6 s. A pass on instance 0 at E reads K in its page, and a barrier holds K's
// cleanup_entry step before its send; meanwhile instance 1 acquires K (allowed, s = 2). After
// the release K's step is {33, E, 'not_due'} (the acquire moved K's entry past E), the pass
// completes with Skipped 1 and nothing else, and the bucket is kept with s = 2 and its new
// receipt. No extra allowance: the allows match the reference model, and releasing instance
// 1's permit is applied.
func TestT_CLN_6(t *testing.T) {
	d := fastDriver(t, 2, nil)
	const key = "cln6"
	t0 := d.Clock().NowNs()
	d.Done(allowed(t, d, 0, key))
	e := t0 + d.Policy().ReceiptLifetimeNs
	setClock(t, d, e)
	stepBarrier(t, d, 0, key, "cln6")
	ch := passAsync(t, d, 0)
	reached(t, d, 0, "cln6")
	p := allowed(t, d, 1, key)
	if s := p.Reply.Elems[5].Int; s != 2 {
		t.Errorf("acquire while K's step is held: s %d, want 2", s)
	}
	d.StoreWrapper(0).Release("cln6")
	rep := passDone(t, ch)
	if rep.Status != "complete" || rep.Skipped != 1 || rep.Removed+rep.Moved+rep.Quarantined != 0 {
		t.Errorf("pass: %+v, want complete with Skipped 1 alone", rep)
	}
	if steps := d.StepsFor(key); len(steps) != 1 || steps[0].Code != 33 || steps[0].Reason != "not_due" || steps[0].TUs != e/1000 {
		t.Errorf("K's steps %+v, want one {33, %d, not_due}", steps, e/1000)
	}
	if in := d.Inspect(key); in.Present != 1 || in.S != 2 || in.X != e/1000+d.Policy().ReceiptLifetimeNs/1000 {
		t.Errorf("K after the pass: %+v, want the bucket with s 2 and x %d", in, e/1000+d.Policy().ReceiptLifetimeNs/1000)
	}
	releaseApplied(t, d, p)
	d.CheckRefModel(t)
}

// TestT_CLN_7 (SPEC T-CLN-7; API-1f, STA-5): 2,000 keys, one allow each at a frozen t0 (done),
// all eligible at t0 + 6 s. Instances 0 and 1 run RunCleanupPass at once over the same due
// entries: an after_reply barrier on each pass's first due_entries holds both until both have
// read the same first page of 1,000 keys. Both complete, and each entry is handled once:
// Removed₀ + Removed₁ = 2,000, each key has exactly one removing step, and each of the first
// page's keys was also stepped by the other pass, which found it gone (Skipped₀ + Skipped₁ ≥
// 1,000). No false quarantine (Quarantined 0 in both, the status's quarantined count 0), and
// the namespace is empty. Then SetHold and a pass: the pass stops with state_integrity_error
// and counts 0.
func TestT_CLN_7(t *testing.T) {
	d := fastDriver(t, 2, nil)
	const n = 2000
	t0 := d.Clock().NowNs()
	for k := range n { // setup: a reply an AOF stall the store records delays past D is retried
		d.Done(allowedThroughAOFStall(t, d, k%2, fmt.Sprintf("cln7-%04d", k)))
	}
	setClock(t, d, t0+d.Policy().ReceiptLifetimeNs)
	d.LoadScripts() // neither pass meets a NOSCRIPT round the other did not
	for i := range 2 {
		arm(t, d, i, fixtures.Fault{Mode: fixtures.Barrier, Match: "due_entries", Count: 1, At: fixtures.AfterReply, Name: "read"})
	}
	ch0, ch1 := passAsync(t, d, 0), passAsync(t, d, 1)
	reached(t, d, 0, "read")
	reached(t, d, 1, "read")
	d.StoreWrapper(0).Release("read")
	d.StoreWrapper(1).Release("read")
	r0, r1 := passDone(t, ch0), passDone(t, ch1)
	for i, r := range []limiter.CleanupReport{r0, r1} {
		if r.Status != "complete" || r.Quarantined != 0 || r.Moved != 0 {
			t.Errorf("concurrent pass on instance %d: %+v, want complete, no quarantine, nothing moved", i, r)
		}
	}
	if r0.Removed+r1.Removed != n || r0.Skipped+r1.Skipped < n/2 {
		t.Errorf("Removed %d + %d, Skipped %d + %d; want %d removed in all, and at least the first page's %d keys skipped by the pass that lost each",
			r0.Removed, r1.Removed, r0.Skipped, r1.Skipped, n, n/2)
	}
	removed := map[string]int{}
	for _, s := range d.CleanupSteps() {
		if s.Code == 30 {
			removed[s.Key]++
		}
	}
	if len(removed) != n {
		t.Errorf("%d keys have a removing step, want %d", len(removed), n)
	}
	for key, c := range removed {
		if c != 1 {
			t.Errorf("key %q removed by %d steps, want 1", key, c)
		}
	}
	if st := d.Status(); st.Code != 40 || st.QuarantinedCount != 0 || st.NB != 0 {
		t.Errorf("status after the passes: %+v, want quarantined count 0 and nb 0", st)
	}
	if left := nsKeys(t, d); len(left) != 0 {
		t.Errorf("%d keys left under rl:e:<id>: after the passes, want none", len(left))
	}

	d.SetHold("store_rollback")
	if rep := d.RunCleanupPass(0); rep.Status != "state_integrity_error" || rep.Removed+rep.Moved+rep.Quarantined+rep.Skipped != 0 {
		t.Errorf("pass under the hold: %+v, want state_integrity_error with counts 0", rep)
	}
}

// cln8Setup takes T-CLN-8's state on K at the driver's frozen t0: 20 allows (instance 0), 5 of
// them released and applied, so 5 markers expire with the receipts at x = t0 + 6 s; the rest
// done. It returns x in ns.
func cln8Setup(t *testing.T, d *driver.Driver, key string) int64 {
	t.Helper()
	t0 := d.Clock().NowNs()
	for n := range 20 {
		op := allowed(t, d, 0, key)
		if n < 5 {
			releaseApplied(t, d, op)
		} else {
			d.Done(op)
		}
	}
	x := (t0 + d.Policy().ReceiptLifetimeNs) / 1000
	if in := d.Inspect(key); in.MC != 5 || !slices.Equal(in.Expiries, []int64{x, x, x, x, x}) || in.X != x {
		t.Fatalf("K after the setup: %+v, want x = t0 + 6 s and 5 markers expiring at x", in)
	}
	return x * 1000
}

// noEntryDue checks the STA-4 invariant on the store after a pass at tUs: no marker field of
// K's bucket and no member of its marker index has an expiry at or below tUs.
func noEntryDue(t *testing.T, d *driver.Driver, key string, tUs int64) {
	t.Helper()
	id := d.ReadEpoch().ID
	snap := d.Dump()
	if k, ok := snap["rl:e:"+id+":k:"+b64(key)]; ok {
		for f, v := range k.Fields {
			if !strings.HasPrefix(f, "m:") {
				continue
			}
			exp, err := strconv.ParseInt(strings.SplitN(v, ":", 2)[0], 10, 64)
			if err != nil || exp <= tUs {
				t.Errorf("marker %s = %q kept after the pass at %d µs", f, v, tUs)
			}
		}
	}
	if mi, ok := snap["rl:e:"+id+":mi:"+b64(key)]; ok {
		for m, sc := range mi.Fields {
			if exp, err := strconv.ParseInt(sc, 10, 64); err != nil || exp <= tUs {
				t.Errorf("marker index entry %s (score %s) kept after the pass at %d µs", m, sc, tUs)
			}
		}
	}
}

// TestT_CLN_8 (SPEC T-CLN-8; STA-4, STA-8): at a frozen t0, 20 allows on K and 5 releases, so
// K's 5 markers expire with its latest receipt (x = t0 + 6 s); the clock moves to t0 + 6 s,
// where K is eligible. (i) A pass reads K, a barrier holds K's step before its send, and an
// acquire of K runs meanwhile: K's step is then 31 with markers_removed 5 (the 5 expired
// markers removed, the bucket kept: no longer full) and the pass completes with Moved 1. (ii)
// A pass removes K (30, markers_removed 5) and then an acquire of K makes a new bucket with a
// new generation and s = 1. In both: no quarantine (the status count 0, K not flagged), no
// marker or marker index entry with an expiry at or below the pass's t, and the balances match
// the reference model.
func TestT_CLN_8(t *testing.T) {
	const key = "cln8"
	t.Run("acquire held before the step", func(t *testing.T) {
		d := fastDriver(t, 2, nil)
		x := cln8Setup(t, d, key)
		setClock(t, d, x)
		stepBarrier(t, d, 0, key, "cln8")
		ch := passAsync(t, d, 0)
		reached(t, d, 0, "cln8")
		d.Done(allowed(t, d, 1, key))
		d.StoreWrapper(0).Release("cln8")
		rep := passDone(t, ch)
		if rep.Status != "complete" || rep.Moved != 1 || rep.Removed+rep.Skipped+rep.Quarantined != 0 {
			t.Errorf("pass: %+v, want complete with Moved 1 alone", rep)
		}
		steps := d.StepsFor(key)
		if len(steps) != 1 || steps[0].Code != 31 || steps[0].Markers != 5 || steps[0].TUs != x/1000 {
			t.Fatalf("K's steps %+v, want one 31 at %d removing 5 markers", steps, x/1000)
		}
		if in := d.Inspect(key); in.Present != 1 || in.MC != 0 || in.Quarantine != "" || in.S != 21 {
			t.Errorf("K after the pass: %+v, want the bucket kept with s 21, no marker, no quarantine", in)
		}
		noEntryDue(t, d, key, steps[0].TUs)
		if st := d.Status(); st.QuarantinedCount != 0 {
			t.Errorf("status %+v, want quarantined count 0", st)
		}
		d.CheckRefModel(t)
	})
	t.Run("acquire after the removal", func(t *testing.T) {
		d := fastDriver(t, 2, nil)
		x := cln8Setup(t, d, key)
		g1 := d.Inspect(key).G
		setClock(t, d, x)
		if rep := passOK(t, d, 0); rep.Removed != 1 || rep.Moved+rep.Skipped+rep.Quarantined != 0 {
			t.Errorf("pass: %+v, want Removed 1 alone", rep)
		}
		steps := d.StepsFor(key)
		if len(steps) != 1 || steps[0].Code != 30 || steps[0].Markers != 5 {
			t.Fatalf("K's steps %+v, want one 30 removing 5 markers", steps)
		}
		noEntryDue(t, d, key, steps[0].TUs)
		op := allowed(t, d, 1, key)
		if g, s := op.Reply.Elems[4].Int, op.Reply.Elems[5].Int; g <= g1 || s != 1 {
			t.Errorf("acquire after the removal: g %d, s %d; want a new generation above %d and s 1", g, s, g1)
		}
		d.Done(op)
		if in := d.Inspect(key); in.Present != 1 || in.MC != 0 || in.Quarantine != "" {
			t.Errorf("K's new bucket: %+v, want present, no marker, no quarantine", in)
		}
		if st := d.Status(); st.QuarantinedCount != 0 {
			t.Errorf("status %+v, want quarantined count 0", st)
		}
		d.CheckRefModel(t)
	})
}
