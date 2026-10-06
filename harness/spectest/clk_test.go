//go:build rltest

package spectest

import (
	"strconv"
	"testing"
	"time"

	"ratelimiter/harness/driver"
	"ratelimiter/internal/keys"
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
