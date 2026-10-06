//go:build rltest

package spectest

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/internal/keys"
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
	t.Cleanup(pl.Close)

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
