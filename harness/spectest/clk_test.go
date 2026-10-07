//go:build rltest

package spectest

import (
	"strconv"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// busyEval busy-waits 300 ms of CPU time inside the store and calls no command: rl_fixture
// scripts cannot call SET, so the clock step follows it as a plain SET in the same pipeline.
const busyEval = "local s = os.clock() while os.clock() - s < 0.3 do end return 1"

// TestT_CLK_1 (SPEC T-CLK-1; CLK-1, CLK-2, API-3, API-1g): with the clock at T1, one rl_fixture
// write pipelines a 300 ms busy EVAL and SET rl:ctl:time T2; an acquire sent 50 ms later is
// written before the EVAL can reply, waits behind it and the buffered SET, and decides at T2:
// the reading taken inside the atomic step. One acquire of another key loads the acquire
// script first: on the driver's fresh store the first EVALSHA gets NOSCRIPT, and the
// adapter's resend would be written only after the EVAL and the SET replied. So the timed
// acquire saw no NOSCRIPT and wrote one EVALSHA, the one written before the EVAL replied.
// Health reports the clock source: controlled, and store for an added store-clock instance.
func TestT_CLK_1(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 1})
	warm := d.Acquire(0, "clk-warm")
	if warm.Kind != limiter.Allowed {
		t.Fatalf("warm-up acquire: %v, want allowed", warm.Kind)
	}
	warm.Permit.Done()
	t1 := d.Clock().NowNs()
	t2 := t1 + 7_000_000_000
	type piped struct {
		replies []storeproc.Reply
		err     error
		at      time.Time
	}
	ch := make(chan piped, 1)
	fx := d.Fixture()
	sent := time.Now() // the pipeline is written after this instant
	go func() {
		rs, err := fx.Pipeline([][]string{
			{"EVAL", busyEval, "0"},
			{"SET", keys.CtlTimeKey, strconv.FormatInt(t2/1000, 10)},
		})
		ch <- piped{rs, err, time.Now()}
	}()
	time.Sleep(50 * time.Millisecond)
	n := len(d.Sent(0))
	op := d.Acquire(0, "clk")
	defer doneAll([]driver.Op{op})
	var p piped
	select {
	case p = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the pipelined EVAL and SET did not return within 10s")
	}
	if p.err != nil || len(p.replies) != 2 || p.replies[0].Kind != ':' || p.replies[1].Str != "OK" {
		t.Fatalf("pipeline: %v %v, want the EVAL's 1 and OK", p.replies, p.err)
	}
	if op.Kind != limiter.Allowed || op.DecisionNs != t2 {
		t.Errorf("acquire: %v at %d, want allowed at T2 = %d (T1 = %d)", op.Kind, op.DecisionNs, t2, t1)
	}
	if op.NoScript || op.SentCommands != 1 {
		t.Errorf("acquire: NoScript %v, %d acquire commands written; want no NOSCRIPT and one EVALSHA", op.NoScript, op.SentCommands)
	}
	var written []time.Time
	for _, s := range d.Sent(0)[n:] {
		if s.Cmd == "acquire" && s.Bytes > 0 {
			written = append(written, s.At)
		}
	}
	// The EVAL replies at least 300 ms after the pipeline was written.
	if len(written) != 1 || !written[0].Before(sent.Add(300*time.Millisecond)) || !written[0].Before(p.at) {
		after := make([]time.Duration, len(written))
		for j, w := range written {
			after[j] = w.Sub(sent)
		}
		t.Errorf("acquire written at %v after the pipeline (EVAL reply by %v), want once, before the EVAL could reply (300ms)",
			after, p.at.Sub(sent))
	}
	if h := d.Limiter(0).Health(); h.ClockSource != "controlled" {
		t.Errorf("health clock_source %q, want controlled", h.ClockSource)
	}
	sp := d.Policy()
	sp.ClockSource = limiter.Store
	i, err := d.AddInstance(driver.InstanceSpec{Policy: &sp})
	if err != nil {
		t.Fatal(err)
	}
	if h := d.Limiter(i).Health(); h.ClockSource != "store" {
		t.Errorf("store-clock instance: health clock_source %q, want store", h.ClockSource)
	}
}

// TestT_CLK_2 (SPEC T-CLK-2; CLK-4, CLK-5): R1 and R2 on K at a frozen t0 expire at E = t0 +
// 6 s. R1's release is applied at t0 with its reply dropped, and its retry is held at a
// barrier. At E + 1 s a pass removes K with R1's marker (step 30, markers_removed 1). The clock
// goes back to E − 1 s, 2 s below the high-water mark E + 1 s. R1's retry is then
// receipt_expired (its budget time is the high-water mark, past E: the expired receipt stays
// expired with its marker gone), and so is R2's release (same E). Later acquires of K decide
// at the high-water mark: a new bucket with exactly 20 allows at decision_ns = E + 1 s, as the
// reference model gives (no extra allowance). After the next epoch read
// rl_clock_lag_microseconds ≥ 1,900,000 (hwm − clock = 2 s).
func TestT_CLK_2(t *testing.T) {
	d := fastDriver(t, 2, nil)
	d.LoadScripts() // the faults meet R1's refund calls, not a NOSCRIPT round
	const key = "clk2"
	r1, r2 := allowed(t, d, 0, key), allowed(t, d, 0, key)
	e := expiryOf(r1) * 1000
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1})
	arm(t, d, 0, fixtures.Fault{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.BeforeSend, Name: "r1"})
	d.Release(r1)
	reached(t, d, 0, "r1")
	if as := d.RefundAttempts(r1); len(as) != 1 || !as[0].Reached || as[0].Store.Outcome != store.OK || as[0].Store.Reply.Code != 3 {
		t.Fatalf("R1's first refund: %+v, want applied at the store with its reply lost", as)
	}
	if in := d.Inspect(key); in.MC != 1 {
		t.Fatalf("K before the pass: %+v, want R1's marker", in)
	}

	hwm := e + second
	setClock(t, d, hwm)
	if rep := passOK(t, d, 1); rep.Removed != 1 {
		t.Errorf("pass at E + 1 s: %+v, want Removed 1", rep)
	}
	if steps := d.StepsFor(key); len(steps) != 1 || steps[0].Code != 30 || steps[0].Markers != 1 {
		t.Fatalf("K's steps %+v, want one 30 removing R1's marker", steps)
	}

	setClock(t, d, e-second)
	d.StoreWrapper(0).Release("r1")
	if ev := event(t, d, r1); ev.Result != limiter.ReceiptExpired {
		t.Errorf("R1's retry after the regression: %+v, want receipt_expired", ev)
	}
	releaseExpired(t, d, r2)
	ops := burst(t, d, key, 20)
	for _, op := range ops {
		if op.DecisionNs != hwm {
			t.Errorf("acquire after the regression decided at %d, want the high-water mark %d", op.DecisionNs, hwm)
			break
		}
	}
	doneAll(ops)
	waitMetric(t, d, 0, "rl_clock_lag_microseconds", "", epochReads(d), "after the regression", func(v float64) bool { return v >= 1_900_000 })
	d.CheckRefModel(t)
}
