//go:build rltest

package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/limiter"
	"ratelimiter/limitertest"
)

// recTB is a testing.TB that records failures and skips instead of acting on them, so a
// test can check what a driver check reports. Its Fatal methods return: the driver returns
// right after each.
type recTB struct {
	testing.TB
	mu      sync.Mutex
	errs    []string
	fatals  []string
	skipped bool
}

func (r *recTB) add(to *[]string, s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*to = append(*to, s)
}

func (r *recTB) Helper()                      {}
func (r *recTB) Error(args ...any)            { r.add(&r.errs, fmt.Sprint(args...)) }
func (r *recTB) Errorf(f string, args ...any) { r.add(&r.errs, fmt.Sprintf(f, args...)) }
func (r *recTB) Fail()                        { r.add(&r.errs, "Fail") }
func (r *recTB) Fatal(args ...any)            { r.add(&r.fatals, fmt.Sprint(args...)) }
func (r *recTB) Fatalf(f string, args ...any) { r.add(&r.fatals, fmt.Sprintf(f, args...)) }
func (r *recTB) FailNow()                     { r.add(&r.fatals, "FailNow") }
func (r *recTB) Skip(...any)                  { r.skip() }
func (r *recTB) Skipf(string, ...any)         { r.skip() }
func (r *recTB) SkipNow()                     { r.skip() }
func (r *recTB) skip()                        { r.mu.Lock(); r.skipped = true; r.mu.Unlock() }
func (r *recTB) Skipped() bool                { r.mu.Lock(); defer r.mu.Unlock(); return r.skipped }
func (r *recTB) Failed() bool                 { r.mu.Lock(); defer r.mu.Unlock(); return len(r.errs)+len(r.fatals) > 0 }
func (r *recTB) reports() (errs, fatals []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.errs), slices.Clone(r.fatals)
}

// editRecord rewrites op-log line i (0-based) with edit applied to its decoded object, whose
// numbers stay exact (json.Number).
func editRecord(t *testing.T, d *Driver, i int, edit func(map[string]any)) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(d.oplog[i]))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	d.oplog[i] = b
}

// lastLine returns the index of the last op-log record whose JSON holds frag.
func lastLine(t *testing.T, d *Driver, frag string) int {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.oplog) - 1; i >= 0; i-- {
		if strings.Contains(string(d.oplog[i]), frag) {
			return i
		}
	}
	t.Fatalf("no op-log record holds %s", frag)
	return -1
}

// TestBridgeFailsOnMismatch (DESIGN §C4, §C6): the bridge passes the true log of 20 allows and
// a limited acquire; an edited log fails a fake testing.TB through t.Fatalf with replay's
// output: one unit more in an allowed record's u, or the limited record turned allowed (exit
// 1, naming the line), and a record missing a field (exit 2).
func TestBridgeFailsOnMismatch(t *testing.T) {
	d := New(t, Options{Instances: 1})
	for range 21 {
		d.Acquire(0, "k")
	}
	d.CheckRefModel(t)
	d.mu.Lock()
	orig := slices.Clone(d.oplog)
	d.mu.Unlock()
	restore := func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.oplog = slices.Clone(orig)
	}
	allowed := lastLine(t, d, `"result":"allowed"`)
	limited := lastLine(t, d, `"result":"limited"`)
	for _, c := range []struct {
		name string
		line int
		edit func(map[string]any)
		want []string
	}{
		{"u plus one unit", allowed, func(m map[string]any) {
			u, err := m["u"].(json.Number).Int64()
			if err != nil {
				t.Fatal(err)
			}
			m["u"] = json.Number(strconv.FormatInt(u+1, 10))
		},
			[]string{"line " + strconv.Itoa(allowed+1) + ": u: expected", "exit status 1"}},
		{"limited turned allowed", limited, func(m map[string]any) { m["result"] = "allowed" },
			[]string{"line " + strconv.Itoa(limited+1) + ": result: expected 'limited', actual 'allowed'", "exit status 1"}},
		{"a field missing", allowed, func(m map[string]any) { delete(m, "g") }, []string{"exit status 2"}},
	} {
		editRecord(t, d, c.line, c.edit)
		f := &recTB{TB: t}
		d.CheckRefModel(f)
		errs, fatals := f.reports()
		if len(fatals) != 1 || len(errs) != 0 || f.Skipped() {
			t.Errorf("%s: fatals %q, errors %q, skipped %v; want one fatal", c.name, fatals, errs, f.Skipped())
		} else {
			for _, w := range c.want {
				if !strings.Contains(fatals[0], w) {
					t.Errorf("%s: the failure lacks %q:\n%s", c.name, w, fatals[0])
				}
			}
		}
		restore()
	}
	d.CheckRefModel(t)
}

// TestBridgeNoPython (DESIGN §C4): with no python3 on PATH the bridge fails the test with
// t.Fatalf; it never skips.
func TestBridgeNoPython(t *testing.T) {
	d := New(t, Options{Instances: 1})
	d.Acquire(0, "k")
	t.Setenv("PATH", t.TempDir())
	f := &recTB{TB: t}
	d.CheckRefModel(f)
	errs, fatals := f.reports()
	if len(fatals) != 1 || !strings.Contains(fatals[0], "python3") || len(errs) != 0 || f.Skipped() {
		t.Errorf("without python3: fatals %q, errors %q, skipped %v; want one fatal naming python3 and no skip", fatals, errs, f.Skipped())
	}
}

// TestDriverTeardownChecks (DESIGN §C4; plan S5 step 16): a clean run passes; an observer
// record HGETALL, or a SCRIPT LOAD with no NoScript response, fails the allowed-commands
// check; an undeclared stray key and undeclared damage fail the audit, while the same damage
// through Fixture is declared and passes; a store that keeps changing fails "store not
// quiet", and its rl:pad: filler passes the audit. An undeclared nb change, which only the
// audit's counter rules see (they run while Damaged is empty), fails the audit alone, after a
// clock write through Fixture and after a keyless EVAL through Fixture (T-CLK-1's pipeline:
// neither declares a key), and passes once a Fixture write has declared damage elsewhere,
// which turns the counter rules off (plan S5 Watch out).
func TestDriverTeardownChecks(t *testing.T) {
	cases := []struct {
		name   string
		inject func(t *testing.T, d *Driver) (undo func())
		want   []string // one substring per expected report, in order; nil: a clean pass
	}{
		{name: "clean", inject: func(*testing.T, *Driver) func() { return func() {} }},
		{name: "HGETALL sent", inject: func(_ *testing.T, d *Driver) func() {
			return injectSend(d, "HGETALL", 31)
		}, want: []string{`allowed commands: instance 0 sent "HGETALL" (31 bytes)`}},
		{name: "HGETALL of no bytes", inject: func(_ *testing.T, d *Driver) func() {
			return injectSend(d, "HGETALL", 0)
		}},
		{name: "SCRIPT LOAD without NoScript", inject: func(_ *testing.T, d *Driver) func() {
			return injectSend(d, "SCRIPT LOAD", 4000)
		}, want: []string{"allowed commands: instance 0 sent ", " SCRIPT LOAD, more than the ", " responses with NoScript"}},
		{name: "stray key", inject: func(t *testing.T, d *Driver) func() {
			raw(t, d, "SET", "rl:stray", "x")
			return func() { raw(t, d, "DEL", "rl:stray") }
		}, want: []string{"store audit: ", "rl:stray"}},
		{name: "damage declared through Fixture", inject: func(t *testing.T, d *Driver) func() {
			if _, err := d.Fixture().Do("HDEL", bucketOf(t, d, "k"), "u"); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}},
		{name: "undeclared damage", inject: func(t *testing.T, d *Driver) func() {
			k := bucketOf(t, d, "k")
			u := d.Dump()[k].Fields["u"]
			raw(t, d, "HDEL", k, "u")
			return func() { raw(t, d, "HSET", k, "u", u) }
		}, want: []string{"store audit: ", bucketOfName("k")}},
		{name: "store not quiet", inject: func(t *testing.T, d *Driver) func() {
			return churn(t, d)
		}, want: []string{"store audit: store not quiet"}},
		// One bucket after the first acquire: nb 1 raised to 2.
		{name: "undeclared nb change", inject: bumpNB, want: []string{nbReport}},
		{name: "undeclared nb change after a clock write through Fixture", inject: func(t *testing.T, d *Driver) func() {
			fixtureDo(t, d, "SET", keys.CtlTimeKey, strconv.FormatInt(d.Clock().NowNs()/1000, 10))
			return bumpNB(t, d)
		}, want: []string{nbReport}},
		{name: "undeclared nb change after a keyless EVAL through Fixture", inject: func(t *testing.T, d *Driver) func() {
			fixtureDo(t, d, "EVAL", "return 1", "0")
			return bumpNB(t, d)
		}, want: []string{nbReport}},
		{name: "undeclared nb change with damage declared", inject: func(t *testing.T, d *Driver) func() {
			fixtureDo(t, d, "HDEL", bucketOf(t, d, "k"), "u")
			return bumpNB(t, d)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel() // each case has its own driver and store
			d := New(t, Options{Instances: 1})
			if op := d.Acquire(0, "k"); op.Kind != limiter.Allowed || !op.NoScript {
				t.Fatalf("first acquire: %v, NoScript %v; want allowed after a reload", op.Kind, op.NoScript)
			}
			undo := c.inject(t, d)
			f := &recTB{TB: t}
			d.teardownChecks(f)
			undo()
			errs, fatals := f.reports()
			t.Logf("reports: %q", errs)
			if len(fatals) != 0 {
				t.Errorf("fatal reports %q", fatals)
			}
			if c.want == nil {
				if len(errs) != 0 {
					t.Errorf("reports %q, want a clean pass", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("no report, want %q", c.want)
			}
			for _, e := range errs {
				for _, w := range c.want {
					if !strings.Contains(e, w) {
						t.Errorf("report %q lacks %q", e, w)
					}
				}
			}
		})
	}
}

// injectSend appends a send record to instance 0 and returns its undo, which deletes exactly
// that record. Other records can land after it (New returns once one call connection is
// ready, so another connection's AUTH may come later), and records are only ever appended, so
// the index taken with the append stays the injected record's.
func injectSend(d *Driver, cmd string, n int) func() {
	in := d.current(0)
	in.mu.Lock()
	at := len(in.sent)
	in.sent = append(in.sent, Send{Cmd: cmd, Bytes: n, At: time.Now()})
	in.mu.Unlock()
	return func() {
		in.mu.Lock()
		defer in.mu.Unlock()
		in.sent = slices.Delete(in.sent, at, at+1)
	}
}

// nbReport is the audit's report of rl:epoch's nb raised from 1 to 2 over one bucket key.
const nbReport = `store audit: "rl:epoch": nb 2, want 1 … 1`

// bumpNB raises rl:epoch's nb by one through a raw rl_fixture connection, so nothing is
// declared, and returns its undo.
func bumpNB(t *testing.T, d *Driver) func() {
	t.Helper()
	nb := d.Dump()[keys.EpochKey].Fields["nb"]
	n, err := strconv.ParseInt(nb, 10, 64)
	if err != nil {
		t.Fatalf("rl:epoch nb %q: %v", nb, err)
	}
	raw(t, d, "HSET", keys.EpochKey, "nb", strconv.FormatInt(n+1, 10))
	return func() { raw(t, d, "HSET", keys.EpochKey, "nb", nb) }
}

// fixtureDo sends one command through the driver's recording Fixture connection.
func fixtureDo(t *testing.T, d *Driver, args ...string) {
	t.Helper()
	if r, err := d.Fixture().Do(args...); err != nil || r.Kind == '-' {
		t.Fatalf("Fixture %v: %v %s", args, err, r)
	}
}

// raw sends one command as rl_fixture outside Fixture, so nothing is declared.
func raw(t *testing.T, d *Driver, args ...string) {
	t.Helper()
	c, err := d.st.Conn("rl_fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if r, err := c.Do(args...); err != nil || r.Kind == '-' {
		t.Fatalf("%v: %v %s", args, err, r)
	}
}

// bucketOf returns the bucket key of key in the current epoch.
func bucketOf(t *testing.T, d *Driver, key string) string {
	t.Helper()
	enc, err := keys.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	return keys.BucketKey(d.ReadEpoch().ID, enc)
}

// bucketOfName is the epoch-independent tail of key's bucket key name.
func bucketOfName(key string) string {
	enc, _ := keys.Encode(key)
	return ":k:" + enc
}

// churn keeps writing an rl:pad: filler key until its undo, so no two snapshots match.
func churn(t *testing.T, d *Driver) func() {
	t.Helper()
	c, err := d.st.Conn("rl_fixture")
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = c.Close() }()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := c.Do("SET", FillerPrefix+"churn", strconv.Itoa(n)); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// TestDriverInspectStatus (SPEC ADM-4, §10): after one allow, Inspect shows the bucket as the
// acquire reply left it (u, t, g, s and the receipt expiry, also as hand-computed values) and
// Status shows nb 1 under the epoch read_epoch names; a key never acquired is absent.
func TestDriverInspectStatus(t *testing.T) {
	d := New(t, Options{Instances: 1})
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed || !op.Replied {
		t.Fatalf("acquire: %v (replied %v), want allowed", op.Kind, op.Replied)
	}
	const tUs = DefaultStartNs / 1000
	rep := op.Reply
	want := Inspect{
		Code: 40, Present: 1,
		U: elemInt(rep, 3), T: rep.TUs, G: elemInt(rep, 4), S: elemInt(rep, 5), X: elemInt(rep, 6),
		MC: 0,
	}
	// By hand: a new bucket is full (20 tokens = 20 × 10^9 units) and one token is spent.
	if want.U != 19_000_000_000 || want.T != tUs || want.G != 1 || want.S != 1 || want.X != tUs+60_000_000 {
		t.Errorf("the reply's u, t, g, s, x = %d %d %d %d %d, want 19000000000 %d 1 1 %d", want.U, want.T, want.G, want.S, want.X, tUs, tUs+60_000_000)
	}
	got := d.Inspect("k")
	if got.Code != want.Code || got.Present != want.Present || got.U != want.U || got.T != want.T || got.G != want.G ||
		got.S != want.S || got.X != want.X || got.MC != want.MC || got.Quarantine != "" || len(got.Expiries) != 0 {
		t.Errorf("Inspect(k) = %+v, want %+v (no quarantine, no markers)", got, want)
	}
	if got := d.Inspect("never"); got.Code != 40 || got.Present != 0 || got.U != -1 {
		t.Errorf("Inspect(never) = %+v, want present 0", got)
	}
	e := d.ReadEpoch()
	st := d.Status()
	if st.Code != 40 || st.NB != 1 || st.NM != 0 || st.EpochID != e.ID || st.FP != fingerprint.Of(d.Policy()) || st.Hold != "" || st.QuarantinedCount != 0 {
		t.Errorf("Status() = %+v, want nb 1 in epoch %s", st, e.ID)
	}
}

// clients returns connected_clients from INFO clients, read as rl_ops.
func clients(t *testing.T, d *Driver) int {
	t.Helper()
	n, err := strconv.Atoi(d.Info("clients")["connected_clients"])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// waitClients waits until connected_clients is want, by deadline.
func waitClients(t *testing.T, d *Driver, want int, deadline time.Time) error {
	t.Helper()
	for {
		n := clients(t, d)
		if n == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("connected_clients %d, want %d", n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDriverKillRestart (DESIGN §A5.4, §A5.12): each instance adds 11 store connections (8
// call, 2 refund, 1 maintenance); Kill(1) returns within 100 ms and its 11 connections close
// within 1 s, after which it is not ready (closing); Restart(1) is ready within one re-read
// interval + 100 ms, gets allowed and has its 11 connections again.
func TestDriverKillRestart(t *testing.T) {
	d := New(t, Options{Instances: 0})
	base := clients(t, d) // the rl_ops connection is open from here on
	for k := 1; k <= 3; k++ {
		i, err := d.AddInstance(InstanceSpec{})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.WaitReady(i, readyWait); err != nil {
			t.Fatal(err)
		}
		if err := waitClients(t, d, base+11*k, time.Now().Add(5*time.Second)); err != nil {
			t.Fatalf("after %d instances: %v", k, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := clients(t, d); n != base+33 {
		t.Fatalf("3 instances: connected_clients %d, want %d", n, base+33)
	}
	start := time.Now()
	d.Kill(1)
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("Kill(1) took %v, want ≤ 100ms", el)
	}
	if err := waitClients(t, d, base+22, start.Add(time.Second)); err != nil {
		t.Errorf("1 s after Kill(1): %v", err)
	}
	if op := d.Acquire(1, "k"); op.Kind != limiter.NotReady {
		t.Errorf("acquire on the killed instance: %v, want not_ready", op.Kind)
	}
	if h := d.Limiter(1).Health(); h.Status != "not_ready" || h.Reason != "closing" {
		t.Errorf("killed instance health %s/%s, want not_ready/closing", h.Status, h.Reason)
	}
	interval := time.Duration(d.inst.EpochRereadIntervalNs)
	start = time.Now()
	if err := d.Restart(1); err != nil {
		t.Fatal(err)
	}
	if err := d.WaitReady(1, interval+100*time.Millisecond-time.Since(start)); err != nil {
		t.Fatalf("Restart(1): %v", err)
	}
	if op := d.Acquire(1, "k"); op.Kind != limiter.Allowed {
		t.Errorf("acquire after Restart(1): %v, want allowed", op.Kind)
	}
	if err := waitClients(t, d, base+33, time.Now().Add(5*time.Second)); err != nil {
		t.Errorf("after Restart(1): %v", err)
	}
}

// TestDriverOpenModeRefuse (SPEC API-20b, API-5; DESIGN §B6 FLT-1 b): under an Open policy a
// refused acquire is degraded, with a permit that carries no deduction and nothing written,
// and rl_acquire_results_total{result="degraded"} counts 1; the next acquire reaches the store.
func TestDriverOpenModeRefuse(t *testing.T) {
	p := DefaultPolicy()
	p.FailureMode = limiter.Open
	d := New(t, Options{Instances: 1, Policy: p})
	if err := d.StoreWrapper(0).Arm(fixtures.Fault{Mode: fixtures.Refuse, Match: "acquire", Count: 1}); err != nil {
		t.Fatal(err)
	}
	n := len(d.Sent(0))
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Degraded || op.Permit == nil || op.Permit.DeductionID() != "" {
		t.Fatalf("refused acquire under Open: %v, permit %v; want degraded with a permit and no deduction", op.Kind, op.Permit)
	}
	if sent := d.Sent(0)[n:]; Count(sent, "acquire") != 0 {
		t.Errorf("a refused acquire wrote %v, want 0 acquire bytes", sent)
	}
	if v, ok := MetricValue(d.Limiter(0).Metrics(), "rl_acquire_results_total", "degraded"); !ok || v != 1 {
		t.Errorf(`rl_acquire_results_total{result="degraded"} = %v (found %v), want 1`, v, ok)
	}
	op.Permit.Done() // a no-op for a degraded permit
	if op := d.Acquire(0, "k"); op.Kind != limiter.Allowed {
		t.Errorf("the next acquire: %v, want allowed", op.Kind)
	}
}

// TestDriverReceiptErrorRecord (plan S5 step 11; SPEC API-12a): a receipt_error keeps its
// charge, so its acquire record carries the E_ALLOWED reply's u, g, s and expiry_us and
// decision_ns = the reply's t × 1000, as a capacity_error record does; deduction_id and
// epoch_id stay empty (no permit). By hand, for the first acquire of a key at the driver's start
// time (1,760,000,000,000,000,000 ns, frozen): t = 1,760,000,000,000,000 µs; the new bucket is
// full, so u = 20 × 10^9 − 10^9; g = s = 1; expiry_us = t + 60 s.
func TestDriverReceiptErrorRecord(t *testing.T) {
	d := New(t, Options{Instances: 1})
	if err := d.SignerWrapper(0).Arm(fixtures.SignerFault{Mode: fixtures.SignFail, Count: 1}); err != nil {
		t.Fatal(err)
	}
	op := d.Acquire(0, "rcp")
	if op.Kind != limiter.ReceiptError || op.Permit != nil || !op.Replied || op.Reply.Code != 1 || op.Reply.TUs != 1_760_000_000_000_000 {
		t.Fatalf("signing failed after the deduction: %v, permit %v, reply %v code %d t %d; want receipt_error, no permit, E_ALLOWED at t 1760000000000000",
			op.Kind, op.Permit != nil, op.Replied, op.Reply.Code, op.Reply.TUs)
	}
	lines := d.OpLog()
	dec := json.NewDecoder(strings.NewReader(lines[len(lines)-1]))
	dec.DisallowUnknownFields()
	var rec acquireRecord
	if err := dec.Decode(&rec); err != nil {
		t.Fatalf("the last op-log record %s: %v", lines[len(lines)-1], err)
	}
	if rec.DecisionNs != op.Reply.TUs*1000 {
		t.Errorf("decision_ns %d, want the reply's t × 1000 = %d", rec.DecisionNs, op.Reply.TUs*1000)
	}
	want := acquireRecord{
		Type: "acquire", Instance: "0", KeyB64URL: "cmNw", Result: "receipt_error",
		DecisionNs: 1_760_000_000_000_000_000, U: 19_000_000_000, G: 1, S: 1, ExpiryUs: 1_760_000_060_000_000,
	}
	if rec != want {
		t.Errorf("receipt_error record\n got %+v\nwant %+v", rec, want)
	}
}

// TestDriverRestartKeyReader (plan S5 step 10): Restart is a new instance with the same
// settings, so it reads a fresh signing key: an instance whose test-supplied key reader yields
// one key's 32 bytes, once, comes back ready and allowed after Kill and Restart, each build
// having called InstanceSpec.KeyReader once.
func TestDriverRestartKeyReader(t *testing.T) {
	var mu sync.Mutex
	builds := 0
	key := func() io.Reader {
		mu.Lock()
		defer mu.Unlock()
		builds++
		return bytes.NewReader(bytes.Repeat([]byte{7}, 32))
	}
	d := New(t, Options{Instances: 1, PerInstance: []InstanceSpec{{KeyReader: key}}})
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire before Restart: %v, want allowed", op.Kind)
	}
	op.Permit.Done()
	d.Kill(0)
	if err := d.Restart(0); err != nil {
		t.Fatalf("Restart(0): %v", err)
	}
	if err := d.WaitReady(0, readyWait); err != nil {
		t.Fatalf("Restart(0): %v", err)
	}
	op = d.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Errorf("acquire after Restart: %v, want allowed", op.Kind)
	}
	if op.Permit != nil {
		op.Permit.Done()
	}
	mu.Lock()
	defer mu.Unlock()
	if builds != 2 {
		t.Errorf("InstanceSpec.KeyReader called %d times, want 2: once per build", builds)
	}
}

// countingAdapter is a test's own store wrapper: it counts the script calls that reach it.
type countingAdapter struct {
	store.Adapter
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingAdapter) Call(ctx context.Context, req store.Request) store.Response {
	c.mu.Lock()
	c.calls[req.Script.Name]++
	c.mu.Unlock()
	return c.Adapter.Call(ctx, req)
}

func (c *countingAdapter) count(script string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[script]
}

// dropSends removes instance i's send records of cmd, so the driver's own teardown passes.
func dropSends(d *Driver, i int, cmd string) {
	in := d.current(i)
	in.mu.Lock()
	defer in.mu.Unlock()
	in.sent = slices.DeleteFunc(in.sent, func(s Send) bool { return s.Cmd == cmd })
}

// TestDriverHookOptionsCompose (plan S5 steps 10 and 16): hook options a test passes in
// InstanceSpec.Options run alongside the driver's instead of replacing them. The test's
// observer hears the sends and its store and signer wrappers are installed, inside the
// driver's; the driver still records every send (Sent) and each acquire's reply, applies the
// faults armed on its own wrappers before the test's wrapper sees the call, and its teardown
// allowed-commands check (a fake testing.TB) reports a command sent from below its wrappers.
func TestDriverHookOptionsCompose(t *testing.T) {
	var mu sync.Mutex
	var heard []string
	var ca *countingAdapter
	signers := 0
	spec := InstanceSpec{Options: []limitertest.Option{
		limitertest.WithSendObserver(func(cmd string, n int) {
			mu.Lock()
			defer mu.Unlock()
			if n > 0 {
				heard = append(heard, cmd)
			}
		}),
		limitertest.WithStoreWrapper(func(a limitertest.Adapter) limitertest.Adapter {
			mu.Lock()
			defer mu.Unlock()
			ca = &countingAdapter{Adapter: a, calls: map[string]int{}}
			return ca
		}),
		limitertest.WithSignerWrapper(func(s limitertest.Signer) limitertest.Signer {
			mu.Lock()
			defer mu.Unlock()
			signers++
			return s
		}),
	}}
	d := New(t, Options{Instances: 1, PerInstance: []InstanceSpec{spec}})
	n := len(d.Sent(0))
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed || !op.Replied {
		t.Fatalf("acquire: %v (reply recorded: %v), want allowed with its reply recorded", op.Kind, op.Replied)
	}
	op.Permit.Done()
	if sent := d.Sent(0)[n:]; Count(sent, "acquire") == 0 {
		t.Errorf("Sent(0) after one acquire: %v, want the acquire written", sent)
	}
	mu.Lock()
	heardAcquire, wrapper, signerWrappers := slices.Contains(heard, "acquire"), ca, signers
	mu.Unlock()
	if !heardAcquire || wrapper == nil || signerWrappers != 1 {
		t.Fatalf("the test's hooks: observer heard acquire %v, store wrapper installed %v, signer wrapper installed %d times; want true, true, 1",
			heardAcquire, wrapper != nil, signerWrappers)
	}
	if got := wrapper.count("acquire"); got != 1 {
		t.Errorf("the test's store wrapper saw %d acquire calls, want 1", got)
	}

	// The driver's store wrapper sits outside the test's: a refused acquire never reaches it.
	if err := d.StoreWrapper(0).Arm(fixtures.Fault{Mode: fixtures.Refuse, Match: "acquire", Count: 1}); err != nil {
		t.Fatal(err)
	}
	if op := d.Acquire(0, "k"); op.Kind != limiter.Unavailable {
		t.Errorf("acquire refused by the driver's store wrapper: %v, want unavailable", op.Kind)
	}
	if got := wrapper.count("acquire"); got != 1 {
		t.Errorf("after a refused acquire the test's store wrapper saw %d acquire calls, want still 1", got)
	}
	if err := d.SignerWrapper(0).Arm(fixtures.SignerFault{Mode: fixtures.SignUnusable, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if op := d.Acquire(0, "k"); op.Kind != limiter.ConfigError {
		t.Errorf("acquire with the driver's signer wrapper unusable: %v, want config_error", op.Kind)
	}

	// HGETALL sent through the test's wrapper, below the driver's, still reaches the driver's
	// observer, so its allowed-commands check reports it.
	if _, r := wrapper.Do(t.Context(), store.Call, "HGETALL", keys.EpochKey); !r.Sent {
		t.Fatalf("HGETALL through the test's store wrapper: %v, nothing sent", r.Outcome)
	}
	f := &recTB{TB: t}
	d.teardownChecks(f)
	dropSends(d, 0, "HGETALL")
	errs, fatals := f.reports()
	if len(fatals) != 0 || len(errs) != 1 || !strings.Contains(errs[0], `allowed commands: instance 0 sent "HGETALL"`) {
		t.Errorf("teardown reports %q, fatal %q; want one allowed-commands report of HGETALL", errs, fatals)
	}
}
