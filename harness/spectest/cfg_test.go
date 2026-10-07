//go:build rltest

package spectest

import (
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storestate"
	"ratelimiter/limiter"
)

// serve runs one acquire on each of instances 0 and 1, one at a time, 30 ms of controlled
// time apart, and finishes the permits.
func serve(t *testing.T, d *driver.Driver, key string, rounds int) {
	t.Helper()
	for range rounds {
		for i := range 2 {
			step(t, d, 30_000_000)
			op := d.Acquire(i, key)
			if op.Kind != limiter.Allowed && op.Kind != limiter.Limited {
				t.Errorf("instance %d: %v (%+v), want allowed or limited", i, op.Kind, op.Diag)
			}
			doneAll([]driver.Op{op})
		}
	}
}

// TestT_CFG_1 (SPEC T-CFG-1; CFG-1, CFG-3, API-8, CFG-7, CFG-8, HOST-5, STA-16): while
// instances 0 and 1 serve, an instance with failure_mode open and one with the store clock
// join: each reports not_ready (fingerprint_mismatch) within one re-read interval and returns
// config_error; instances 0 and 1 keep matching the reference model and send no SCRIPT LOAD
// after the new instances start (their scripts are not replaced).
func TestT_CFG_1(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 2})
	serve(t, d, "cfg1", 12)
	n0, n1 := len(d.Sent(0)), len(d.Sent(1))
	open, storeClock := d.Policy(), d.Policy()
	open.FailureMode = limiter.Open
	storeClock.ClockSource = limiter.Store
	start := time.Now()
	var added []int
	for _, p := range []limiter.Policy{open, storeClock} {
		i, err := d.AddInstance(driver.InstanceSpec{Policy: &p})
		if err != nil {
			t.Fatal(err)
		}
		added = append(added, i)
	}
	for _, i := range added {
		if err := waitHealth(d.Limiter(i), "not_ready", "fingerprint_mismatch", start.Add(interval(d))); err != nil {
			t.Errorf("instance %d within one re-read interval: %v", i, err)
		}
		if op := d.Acquire(i, "cfg1"); op.Kind != limiter.ConfigError || op.Permit != nil {
			t.Errorf("instance %d: %v, want config_error", i, op.Kind)
		}
	}
	serve(t, d, "cfg1", 12)
	serve(t, d, "cfg1-other", 3)
	d.CheckRefModel(t)
	if c := driver.Count(d.Sent(0)[n0:], "SCRIPT LOAD") + driver.Count(d.Sent(1)[n1:], "SCRIPT LOAD"); c != 0 {
		t.Errorf("instances 0 and 1 sent %d SCRIPT LOAD after the new instances started, want 0", c)
	}
}

// TestT_CFG_2 (SPEC T-CFG-2; CFG-2, CFG-6, API-8a, ADM-1): an instance started before the epoch
// is not_ready (no_epoch) and refuses; after 3 re-read intervals it still is, and the store
// still has no epoch record (Status: epoch_record_missing). Once the epoch starts, the
// instance is ready within one re-read interval + 100 ms (plan choice) and allows; the op log
// matches the reference model.
func TestT_CFG_2(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 1, NoEpoch: true})
	l := d.Limiter(0)
	if err := waitHealth(l, "not_ready", "no_epoch", time.Now()); err != nil {
		t.Error(err)
	}
	if op := d.Acquire(0, "cfg2"); op.Kind != limiter.NotReady {
		t.Errorf("before the epoch: %v, want not_ready", op.Kind)
	}
	time.Sleep(3 * interval(d))
	if err := waitHealth(l, "not_ready", "no_epoch", time.Now()); err != nil {
		t.Errorf("after 3 intervals: %v", err)
	}
	if op := d.Acquire(0, "cfg2"); op.Kind != limiter.NotReady {
		t.Errorf("after 3 intervals: %v, want not_ready", op.Kind)
	}
	if st := d.Status(); st.Code != 41 || st.Reason != "epoch_record_missing" {
		t.Errorf("Status after 3 intervals: %+v, want refused epoch_record_missing", st)
	}
	if _, ok := d.Dump()[keys.EpochKey]; ok {
		t.Error("an instance created the epoch record")
	}
	start := time.Now()
	d.StartEpoch()
	if err := d.WaitReady(0, interval(d)+100*time.Millisecond-time.Since(start)); err != nil {
		t.Errorf("after StartEpoch: %v", err)
	}
	op := d.Acquire(0, "cfg2")
	if op.Kind != limiter.Allowed {
		t.Errorf("after the epoch started: %v, want allowed", op.Kind)
	}
	doneAll([]driver.Op{op})
	d.CheckRefModel(t)
}

// keyErrReader fails every read: signing-key generation fails (T-CFG-3 d).
type keyErrReader struct{}

func (keyErrReader) Read([]byte) (int, error) { return 0, errors.New("test: no key bytes") }

// countingListener accepts and counts TCP connections until closed.
type countingListener struct {
	ln      net.Listener
	accepts atomic.Int64
	wg      sync.WaitGroup
}

func listen(t *testing.T) *countingListener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &countingListener{ln: ln}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.accepts.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		c.wg.Wait()
	})
	return c
}

// TestT_CFG_3 (SPEC T-CFG-3; RCP-8, CFG-4, CMP-9, CLK-2, RCP-4, API-8a): (a) a release delay
// of 25 s (61 s > the 60 s lifetime) and a margin of 59 ms (< 60 s × 1,000 ppm = 60 ms); (b)
// cap_units ≥ 2^52, a capacity × rate_period_ns past int64, rate_tokens = 2^31; (c) production
// construction with the controlled clock; (d) a signing-key reader that fails. Each instance
// comes back non-nil with an error, reports not_ready (settings_invalid), still returns
// not_ready after 3 re-read intervals and sent nothing (the send observer for a, b and d; for
// c a counting listener that accepted no connection). The valid side of each numeric bound
// (24 s, 60 ms, cap_units just below 2^52, rate_tokens 2^31 − 1) is accepted, and instance 0
// still allows.
func TestT_CFG_3(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 1})
	base, inst := d.Policy(), d.Instance()
	withInstance := func(f func(*limiter.Instance)) driver.InstanceSpec {
		in := inst
		f(&in)
		return driver.InstanceSpec{Instance: &in}
	}
	withPolicy := func(f func(*limiter.Policy)) driver.InstanceSpec {
		p := base
		f(&p)
		return driver.InstanceSpec{Policy: &p}
	}
	cases := []struct {
		name    string
		spec    driver.InstanceSpec
		invalid bool
	}{
		{"a: release delay 25 s", withInstance(func(in *limiter.Instance) { in.MaxReleaseDelayNs = 25_000_000_000 }), true},
		{"a: release delay 24 s", withInstance(func(in *limiter.Instance) { in.MaxReleaseDelayNs = 24_000_000_000 }), false},
		{"a: margin 59 ms", withInstance(func(in *limiter.Instance) { in.MarginNs = 59_000_000 }), true},
		{"a: margin 60 ms", withInstance(func(in *limiter.Instance) { in.MarginNs = 60_000_000 }), false},
		{"b: cap_units 2^52 or more", withPolicy(func(p *limiter.Policy) { p.Capacity = 4_503_600 }), true},
		{"b: cap_units below 2^52", withPolicy(func(p *limiter.Policy) { p.Capacity = 4_503_599 }), false},
		{"b: int64 overflow", withPolicy(func(p *limiter.Policy) {
			p.Capacity, p.RatePeriodNs = 10_000_000_000, 1_000_000_000_000
		}), true},
		{"b: rate_tokens 2^31", withPolicy(func(p *limiter.Policy) { p.RateTokens = 1 << 31 }), true},
		{"b: rate_tokens 2^31 − 1", withPolicy(func(p *limiter.Policy) { p.RateTokens = 1<<31 - 1 }), false},
		{"d: key reader fails", driver.InstanceSpec{KeyReader: func() io.Reader { return keyErrReader{} }}, true},
	}
	idx := make([]int, len(cases))
	for n, c := range cases {
		i, err := d.AddInstance(c.spec)
		idx[n] = i
		if l := d.Limiter(i); l == nil {
			t.Fatalf("%s: nil limiter", c.name)
		}
		if c.invalid != (err != nil) {
			t.Errorf("%s: New error %v, want an error: %v", c.name, err, c.invalid)
		}
		if h := d.Limiter(i).Health(); c.invalid && (h.Status != "not_ready" || h.Reason != "settings_invalid") {
			t.Errorf("%s: health %s/%s, want not_ready/settings_invalid", c.name, h.Status, h.Reason)
		}
	}
	// (c) production construction with the controlled clock, pointed at a counting listener.
	ln := listen(t)
	prod := base // ClockSource = Controlled
	pl, perr := limiter.New(prod, inst, limiter.StoreOptions{Addr: ln.ln.Addr().String(), User: "rl_lib", Password: d.Store().PW.Lib})
	if pl == nil || perr == nil {
		t.Fatalf("c: limiter.New with the controlled clock: %v, %v; want a limiter and an error", pl, perr)
	}
	t.Cleanup(func() { pl.Close() })

	time.Sleep(3 * interval(d))
	for n, c := range cases {
		if !c.invalid {
			continue
		}
		i := idx[n]
		if op := d.Acquire(i, "cfg3"); op.Kind != limiter.NotReady {
			t.Errorf("%s after 3 intervals: %v, want not_ready", c.name, op.Kind)
		}
		if h := d.Limiter(i).Health(); h.Status != "not_ready" || h.Reason != "settings_invalid" {
			t.Errorf("%s after 3 intervals: health %s/%s, want not_ready/settings_invalid", c.name, h.Status, h.Reason)
		}
		if sent := d.Sent(i); len(sent) != 0 {
			t.Errorf("%s: sent %v, want nothing", c.name, sent)
		}
	}
	if r := pl.Acquire(t.Context(), "cfg3"); r.Kind != limiter.NotReady {
		t.Errorf("c after 3 intervals: %v, want not_ready", r.Kind)
	}
	if h := pl.Health(); h.Status != "not_ready" || h.Reason != "settings_invalid" {
		t.Errorf("c: health %s/%s, want not_ready/settings_invalid", h.Status, h.Reason)
	}
	if n := ln.accepts.Load(); n != 0 {
		t.Errorf("c: the listener accepted %d connections, want 0", n)
	}
	op := d.Acquire(0, "cfg3")
	if op.Kind != limiter.Allowed {
		t.Errorf("instance 0: %v, want allowed", op.Kind)
	}
	doneAll([]driver.Op{op})
}

// nsDump returns the keys of epoch id's namespace (rl:e:<id>:) in a store snapshot.
func nsDump(d *driver.Driver, id string) storestate.State {
	out := storestate.State{}
	for k, v := range d.Dump() {
		if strings.HasPrefix(k, "rl:e:"+id+":") {
			out[k] = v
		}
	}
	return out
}

// holdAcquire arms a before_send barrier named name on instance i's next acquire and starts an
// acquire of key there, whose command (its epoch_id included) is built before the barrier; it
// returns once the call waits at the barrier, with the channel its result arrives on after
// Release(name).
func holdAcquire(t *testing.T, d *driver.Driver, i int, key, name string) <-chan driver.Op {
	t.Helper()
	arm(t, d, i, fixtures.Fault{Mode: fixtures.Barrier, Match: "acquire", Count: 1, At: fixtures.BeforeSend, Name: name})
	ch := make(chan driver.Op, 1)
	go func() { ch <- d.Acquire(i, key) }()
	reached(t, d, i, name)
	return ch
}

// releaseHeld releases barrier name of instance i and waits up to waitMax for the held acquire.
func releaseHeld(t *testing.T, d *driver.Driver, i int, name string, ch <-chan driver.Op) driver.Op {
	t.Helper()
	d.StoreWrapper(i).Release(name)
	select {
	case op := <-ch:
		return op
	case <-time.After(waitMax):
		t.Fatalf("the acquire held at barrier %s did not return within %v of its release", name, waitMax)
		return driver.Op{}
	}
}

// TestT_CFG_5 (SPEC T-CFG-5; CFG-3, CFG-5, CFG-6, ADM-1, BUD-10): three instances with
// max_tracked_buckets = 1,000 (timed passes off) and one new-settings instance (capacity 30),
// which waits not_ready (fingerprint_mismatch). Epoch A is filled to 990 buckets, three permits
// open, and each instance's epoch read sees them (rl_tracked_buckets 990), so A's grown gen
// reaches its floors. With an acquire held at a before-send barrier, ReplaceEpoch (same
// settings) starts B: the three instances join B within the join tolerance, and only then is the
// held command, built for A, released: it gets config_error; start-epoch with no flag, or with
// the stale ID of A, exits 2 and leaves B's record untouched; A's open receipts end
// receipt_invalid. B then creates 1,000 buckets, each starting full (19 tokens left after its
// first allow), with no capacity_error, and status counts only B's (nb 1,000). After at most 3
// completed passes per instance, rl:e:<A>:* is empty, the instances' rl_reclaim_keys_total add
// up to A's key count, and B's keys did not change during reclamation. A replace with the new
// settings (C) lets the new-settings instance join within the join tolerance and serve (25
// allows of one key at the frozen clock, within C's capacity of 30), while the old-settings
// instances report not_ready (fingerprint_mismatch) and return config_error on acquire, refund
// (code 20) and cleanup.
func TestT_CFG_5(t *testing.T) {
	d := fastDriver(t, 3, func(p *limiter.Policy, _ *limiter.Instance) { p.MaxTrackedBuckets = 1000 })
	p := d.Policy()
	p2 := p
	p2.Capacity = 30
	start := time.Now()
	ni, err := d.AddInstance(driver.InstanceSpec{Policy: &p2})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitHealth(d.Limiter(ni), "not_ready", "fingerprint_mismatch", start.Add(joinTolerance(d))); err != nil {
		t.Errorf("new-settings instance before the replace: %v", err)
	}
	keyOf := func(k int) string { return fmt.Sprintf("cfg5-%04d", k) }
	idA := d.ReadEpoch().ID
	var open []driver.Op
	for k := range 990 {
		op := allowed(t, d, k%3, keyOf(k))
		if k < 3 {
			open = append(open, op)
		} else {
			d.Done(op)
		}
	}
	filled := time.Now() // A's last allow
	if st := d.Status(); st.NB != 990 {
		t.Fatalf("epoch A: status %+v, want 990 tracked buckets", st)
	}
	aKeys := len(d.StoreKeys("rl:e:" + idA + ":*"))
	for i := range 3 { // an epoch read sees A's traffic, so A's grown gen reaches each instance's floors (§A5.7)
		waitMetric(t, d, i, "rl_tracked_buckets", "", time.Until(filled.Add(epochReads(d))), "after epoch A's 990 keys", func(v float64) bool { return v == 990 })
	}

	held := holdAcquire(t, d, 0, keyOf(3), "held")
	start = time.Now()
	idB := d.ReplaceEpoch()
	waitAllJoinedOf(t, d, []int{0, 1, 2}, idB, start.Add(joinTolerance(d)))
	// released only now that instance 0 is in B: the command was built for A before the barrier
	if op := releaseHeld(t, d, 0, "held", held); op.Kind != limiter.ConfigError || op.Permit != nil {
		t.Errorf("acquire held across the replace: %v %+v, want config_error", op.Kind, op.Diag)
	}
	rec := d.Dump()[keys.EpochKey]
	adminCode(t, "start-epoch without --replace", d.RLAdmin("start-epoch"), 2)
	adminCode(t, "start-epoch with the stale ID", d.RLAdmin("start-epoch", "--replace", "--expect-epoch", idA), 2)
	if now := d.Dump()[keys.EpochKey]; !reflect.DeepEqual(now, rec) {
		t.Errorf("record after the refused starts: %v, want untouched %v", now, rec)
	}
	for _, op := range open {
		if ev := event(t, d, releaseOp(d, op)); ev.Result != limiter.ReceiptInvalid {
			t.Errorf("release of epoch-A permit %s: %+v, want receipt_invalid", op.Permit.DeductionID(), ev)
		}
	}

	full := p.Capacity*token(p) - token(p)
	for k := range 1000 {
		op := d.Acquire(k%3, keyOf(k))
		if op.Kind != limiter.Allowed || replyU(t, op) != full {
			t.Fatalf("epoch B, key %d of 1,000: %v %+v, want allowed from a full bucket (u %d after it)", k, op.Kind, op.Diag, full)
		}
		d.Done(op)
	}
	if st := d.Status(); st.EpochID != idB || st.NB != 1000 {
		t.Errorf("epoch B: status %+v, want 1,000 tracked buckets in %s", st, idB)
	}
	bState := nsDump(d, idB)
	sample := map[string]driver.Inspect{}
	for _, k := range []int{0, 500, 999} {
		sample[keyOf(k)] = d.Inspect(keyOf(k))
	}
	for round := 0; round < 3 && len(d.StoreKeys("rl:e:"+idA+":*")) > 0; round++ {
		for i := range 3 {
			passOK(t, d, i)
		}
	}
	if left := d.StoreKeys("rl:e:" + idA + ":*"); len(left) != 0 {
		t.Errorf("after 3 passes per instance %d keys of epoch A are left, want none", len(left))
	}
	reclaimed := 0.0
	for i := range d.Instances() {
		reclaimed += metric(t, d, i, "rl_reclaim_keys_total", "")
	}
	if reclaimed != float64(aKeys) {
		t.Errorf("rl_reclaim_keys_total over the instances: %v, want epoch A's %d keys", reclaimed, aKeys)
	}
	if now := nsDump(d, idB); !reflect.DeepEqual(now, bState) {
		t.Error("epoch B's keys changed during reclamation")
	}
	for k, in := range sample {
		if now := d.Inspect(k); !reflect.DeepEqual(now, in) {
			t.Errorf("%s after reclamation: %+v, want %+v", k, now, in)
		}
	}

	pending := allowed(t, d, 0, keyOf(1)) // B is at its 1,000 buckets: a key it tracks
	d.LoadScripts()
	start = time.Now()
	idC := d.ReplaceEpochWith(p2)
	if err := d.WaitJoined(ni, idC, joinTolerance(d)-time.Since(start)); err != nil {
		t.Errorf("new-settings instance after the replace with its settings: %v", err)
	}
	for i := range 3 {
		if err := waitHealth(d.Limiter(i), "not_ready", "fingerprint_mismatch", start.Add(joinTolerance(d))); err != nil {
			t.Errorf("old-settings instance %d: %v", i, err)
		}
		if op := d.Acquire(i, keyOf(0)); op.Kind != limiter.ConfigError {
			t.Errorf("old-settings instance %d: acquire %v, want config_error", i, op.Kind)
		}
		if rep := d.RunCleanupPass(i); rep.Status != "config_error" {
			t.Errorf("old-settings instance %d: pass %+v, want config_error", i, rep)
		}
	}
	d.Release(pending)
	storeReply(t, "old-settings refund", firstAttempt(t, d, pending), 20, "epoch_or_fingerprint")
	for range 25 { // more than the old capacity of 20: the teardown BUD-11 check judges C by its own policy
		d.Done(allowed(t, d, ni, keyOf(0)))
	}
	d.Kill(0) // its refund retries config_error until its retention timer
}

// waitAllJoinedOf waits until each instance in idx is ready in epoch id, by deadline.
func waitAllJoinedOf(t *testing.T, d *driver.Driver, idx []int, id string, deadline time.Time) {
	t.Helper()
	for _, i := range idx {
		if err := d.WaitJoined(i, id, time.Until(deadline)); err != nil {
			t.Error(err)
		}
	}
}

// TestT_CFG_6 (SPEC T-CFG-6; ADM-1, RCP-2, CFG-3): in epoch A an instance holds a permit and an
// acquire waits at a before-send barrier; the store is wiped, the controlled time written again
// (a wipe deletes rl:ctl:time) and rladmin start-epoch with no flag starts B with the same
// settings. The old instance re-joins B within the join tolerance; the held acquire, built for
// A, gets config_error; the pre-wipe permit's release ends receipt_invalid with no credit (the
// key has no bucket in B and status shows no marker).
func TestT_CFG_6(t *testing.T) {
	d := fastDriver(t, 1, nil)
	const key = "cfg6"
	open := allowed(t, d, 0, key)
	d.LoadScripts()
	held := holdAcquire(t, d, 0, key, "held")
	if err := d.Tamper().Wipe(); err != nil {
		t.Fatal(err)
	}
	setClock(t, d, d.Clock().NowNs())
	start := time.Now()
	r := d.RLAdmin("start-epoch")
	adminCode(t, "start-epoch after the wipe", r, 0)
	idB := r.Str("epoch_id")
	if err := d.WaitJoined(0, idB, joinTolerance(d)-time.Since(start)); err != nil {
		t.Error(err)
	}
	if op := releaseHeld(t, d, 0, "held", held); op.Kind != limiter.ConfigError || op.Diag.Reason != "epoch_or_fingerprint" {
		t.Errorf("acquire held across the wipe: %v %+v, want config_error", op.Kind, op.Diag)
	}
	if ev := event(t, d, releaseOp(d, open)); ev.Result != limiter.ReceiptInvalid {
		t.Errorf("release of the pre-wipe permit: %+v, want receipt_invalid", ev)
	}
	if in, st := d.Inspect(key), d.Status(); in.Present != 0 || st.EpochID != idB || st.NM != 0 {
		t.Errorf("epoch B: inspect %+v, status %+v; want no bucket and no marker", in, st)
	}
}
