//go:build rltest

package driver

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
	"ratelimiter/limiter"
	"ratelimiter/limitertest"
)

// recTB is a testing.TB that records failures and skips instead of acting on them, so a
// test can check what a driver check reports. Its Fatal methods return: the driver returns
// right after each. While exit is set (setExit), they end the calling goroutine after
// recording (runtime.Goexit), as testing.T's do, so a test can check that no code after a
// driver call runs.
type recTB struct {
	testing.TB
	mu      sync.Mutex
	errs    []string
	fatals  []string
	skipped bool
	exit    bool
}

func (r *recTB) add(to *[]string, s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*to = append(*to, s)
}

func (r *recTB) fatal(s string) {
	r.mu.Lock()
	r.fatals = append(r.fatals, s)
	exit := r.exit
	r.mu.Unlock()
	if exit {
		runtime.Goexit()
	}
}

func (r *recTB) setExit(on bool)              { r.mu.Lock(); r.exit = on; r.mu.Unlock() }
func (r *recTB) Helper()                      {}
func (r *recTB) Error(args ...any)            { r.add(&r.errs, fmt.Sprint(args...)) }
func (r *recTB) Errorf(f string, args ...any) { r.add(&r.errs, fmt.Sprintf(f, args...)) }
func (r *recTB) Fail()                        { r.add(&r.errs, "Fail") }
func (r *recTB) Fatal(args ...any)            { r.fatal(fmt.Sprint(args...)) }
func (r *recTB) Fatalf(f string, args ...any) { r.fatal(fmt.Sprintf(f, args...)) }
func (r *recTB) FailNow()                     { r.fatal("FailNow") }
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
// which turns the counter rules off (plan S5 Watch out). S6 (plan step 13): a refunded release
// passes the refund-sends check, while a final event whose 1 attempt is one more than the 0
// refund sends, or one refund send more than the attempts when every release got a final
// event, fail it, also when another permit was released after its done() (no release: the
// first finish decides); an op log with 20 allows of one key at one instant passes check_bud11,
// one with 21 (one allow over BUD-11) fails it with exit status 1, a malformed one (a second
// refund record for one permit) with exit status 2, and with no python3 on PATH the check
// fails naming it; a signing-key canary in hex in a store value fails both the audit and the
// AOF search, and one written and deleted again, so only the AOF holds it, fails the AOF
// search alone.
func TestDriverTeardownChecks(t *testing.T) {
	cases := []struct {
		name   string
		inject func(t *testing.T, d *Driver) (undo func())
		want   []string // substrings every report must contain; nil (and no reports): a clean pass
		// reports, when set, wants exactly one report per element, in order, each holding every
		// substring of its element.
		reports [][]string
		serial  bool // not run in parallel: it sets the environment (t.Setenv)
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
		{name: "a release refunded", inject: func(t *testing.T, d *Driver) func() {
			refunded(t, d)
			return func() {}
		}},
		{name: "attempts over refund sends", inject: func(_ *testing.T, d *Driver) func() {
			return injectEvent(d, limiter.RefundEvent{DeductionID: "d1_injected", Result: limiter.Applied, Attempts: 1})
		}, want: []string{"refund sends: instance 0: the 1 final events received count 1 attempts, more than the 0 refund sends"}},
		{name: "a refund send beyond the attempts", inject: func(t *testing.T, d *Driver) func() {
			refunded(t, d)
			return injectSend(d, "refund", 200)
		}, want: []string{"refund sends: instance 0: ", " attempts over 1 final events (one for each of its 1 releases", " refund sends with bytes written"}},
		{name: "a refund send beyond the attempts, another permit released after done", inject: func(t *testing.T, d *Driver) func() {
			refunded(t, d)
			op := d.Acquire(0, "k")
			if op.Kind != limiter.Allowed {
				t.Fatalf("acquire: %v, want allowed", op.Kind)
			}
			d.Done(op)
			d.Release(op) // after done(): no release, so no final event is missing
			return injectSend(d, "refund", 200)
		}, want: []string{"refund sends: instance 0: ", " attempts over 1 final events (one for each of its 1 releases", " refund sends with bytes written"}},
		{name: "20 allows at one instant", inject: func(t *testing.T, d *Driver) func() {
			return extraAllows(t, d, 19)
		}},
		{name: "one allow over BUD-11", inject: func(t *testing.T, d *Driver) func() {
			return extraAllows(t, d, 20)
		}, want: []string{"BUD-11: refmodel.check_bud11 rejects the op log (exit status 1)", "exceeds capacity"}},
		{name: "a malformed op log: a second refund record", inject: func(t *testing.T, d *Driver) func() {
			refunded(t, d)
			return repeatLine(t, d, lastLine(t, d, `"type":"refund"`))
		}, reports: [][]string{{"BUD-11: refmodel.check_bud11 rejects the op log (exit status 2)", "duplicate final refund"}}},
		{name: "no python3 for BUD-11", serial: true, inject: func(t *testing.T, _ *Driver) func() {
			t.Setenv("PATH", "") // restored when the case ends, before the driver's own teardown
			return func() {}
		}, reports: [][]string{{"BUD-11: ", "python3", "(a failure, never a skip)"}}},
		{name: "canary in a value and the AOF", inject: func(t *testing.T, d *Driver) func() {
			undo := addCanary(d)
			raw(t, d, "SET", FillerPrefix+"canary", hex.EncodeToString([]byte(testCanary)))
			return func() {
				undo()
				raw(t, d, "DEL", FillerPrefix+"canary")
			}
		}, want: []string{"canary 1 (hex)"}, reports: [][]string{
			{"store audit: ", `"rl:pad:canary"`, "canary 1 (hex)"},
			{"canaries: AOF file ", "canary 1 (hex)"},
		}},
		{name: "canary in the AOF alone", inject: func(t *testing.T, d *Driver) func() {
			undo := addCanary(d)
			raw(t, d, "SET", FillerPrefix+"canary", hex.EncodeToString([]byte(testCanary)))
			raw(t, d, "DEL", FillerPrefix+"canary") // gone from the store, kept in the AOF
			return undo
		}, reports: [][]string{{"canaries: AOF file ", "canary 1 (hex)"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.serial {
				t.Parallel() // each case has its own driver and store
			}
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
			if c.want == nil && c.reports == nil {
				if len(errs) != 0 {
					t.Errorf("reports %q, want a clean pass", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("no report, want %q %q", c.want, c.reports)
			}
			for _, e := range errs {
				for _, w := range c.want {
					if !strings.Contains(e, w) {
						t.Errorf("report %q lacks %q", e, w)
					}
				}
			}
			if c.reports == nil {
				return
			}
			if len(errs) != len(c.reports) {
				t.Fatalf("%d reports %q, want %d: %q", len(errs), errs, len(c.reports), c.reports)
			}
			for i, ws := range c.reports {
				for _, w := range ws {
					if !strings.Contains(errs[i], w) {
						t.Errorf("report %d %q lacks %q", i, errs[i], w)
					}
				}
			}
		})
	}
}

// testCanary is the signing-key canary the teardown-check cases plant (32 bytes).
const testCanary = "teardown-check-canary-0123456789"

// addCanary gives the driver testCanary as the canary of index 99, after instance 0's (so it
// is canary 1 of the audit's list), and returns the undo, which removes it.
func addCanary(d *Driver) func() {
	d.mu.Lock()
	d.canaries[99] = []byte(testCanary)
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.canaries, 99)
	}
}

// repeatLine appends a copy of op-log line i (0-based) and returns the undo, which restores
// the op log.
func repeatLine(t *testing.T, d *Driver, i int) func() {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	orig := slices.Clone(d.oplog)
	d.oplog = append(d.oplog, slices.Clone(d.oplog[i]))
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.oplog = orig
	}
}

// refunded acquires one more permit of "k" on instance 0, releases it through the driver and
// waits for its final event: applied, with 1 attempt, or 2 when the store did not have the
// refund script yet (NOSCRIPT, then the reload and the resend).
func refunded(t *testing.T, d *Driver) {
	t.Helper()
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire: %v, want allowed", op.Kind)
	}
	d.Release(op)
	ev, err := d.WaitRefund(op, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Result != limiter.Applied || ev.Attempts < 1 || ev.Attempts > 2 {
		t.Fatalf("refund event %+v, want applied with 1 or 2 attempts", ev)
	}
}

// injectEvent appends a final event to instance 0's received events and returns its undo,
// which deletes exactly that event.
func injectEvent(d *Driver, ev limiter.RefundEvent) func() {
	in := d.current(0)
	in.mu.Lock()
	at := len(in.events)
	in.events = append(in.events, ev)
	in.mu.Unlock()
	return func() {
		in.mu.Lock()
		defer in.mu.Unlock()
		in.events = slices.Delete(in.events, at, at+1)
	}
}

// extraAllows appends n copies of the op log's last allowed acquire record, each with a fresh
// deduction_id, and returns the undo, which restores the op log.
func extraAllows(t *testing.T, d *Driver, n int) func() {
	t.Helper()
	line := lastLine(t, d, `"result":"allowed"`)
	d.mu.Lock()
	defer d.mu.Unlock()
	orig := slices.Clone(d.oplog)
	dec := json.NewDecoder(bytes.NewReader(d.oplog[line]))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	for k := range n {
		m["deduction_id"] = fmt.Sprintf("d1_extra%014d", k)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		d.oplog = append(d.oplog, b)
	}
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.oplog = orig
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

// refundAttemptsJSON returns the attempts of the op log's refund record of deduction id as the
// driver wrote them (compact JSON), failing the test unless there is exactly one such record.
func refundAttemptsJSON(t *testing.T, d *Driver, id string) string {
	t.Helper()
	recs := refundRecordsOf(t, d, id)
	if len(recs) != 1 {
		t.Fatalf("%d refund records of %s %q, want 1", len(recs), id, recs)
	}
	return recs[0]
}

// refundRecordsOf returns the attempts of every refund record of deduction id in the op log.
func refundRecordsOf(t *testing.T, d *Driver, id string) []string {
	t.Helper()
	var out []string
	for _, l := range d.OpLog() {
		var r struct {
			Type        string          `json:"type"`
			DeductionID string          `json:"deduction_id"`
			Attempts    json.RawMessage `json:"attempts"`
		}
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("op-log record %s: %v", l, err)
		}
		if r.Type == "refund" && r.DeductionID == id {
			out = append(out, string(r.Attempts))
		}
	}
	return out
}

// Attempt entries of refund records, written by hand (plan S6 step 10): a refused call, and
// one whose outcome the limiter did not learn.
const (
	refusedEntry = `{"code":"unavailable","t_us":0,"credited":0}`
	unknownEntry = `{"code":"outcome_unknown","t_us":0,"credited":0}`
)

// attemptsList is the JSON list of entries.
func attemptsList(entries ...string) string { return "[" + strings.Join(entries, ",") + "]" }

// fastDriver returns a one-instance driver on FastPolicy and FastInstance with spec.
func fastDriver(t *testing.T, spec InstanceSpec) *Driver {
	t.Helper()
	p := FastPolicy()
	return New(t, Options{Instances: 1, Policy: p, Instance: FastInstance(p), PerInstance: []InstanceSpec{spec}})
}

// TestDriverRefundRecordAttempts (plan S6 step 10; HARNESS §9.4): the refund record lists each
// refund call with what the limiter got, by hand for the first permit of a new key at the
// driver's frozen start, t = 1,760,000,000,000,000 µs (u 19 tokens of 10^9 units after the
// allow): two calls refused, then E_APPLIED crediting one token, are [unavailable, unavailable,
// {3, t, 10^9}]; a first reply dropped after the store applied it, then the retry's
// E_ALREADY_APPLIED, are [outcome_unknown, {4, t, credit_us = t}].
func TestDriverRefundRecordAttempts(t *testing.T) {
	for _, c := range []struct {
		name   string
		fault  fixtures.Fault
		result limiter.RefundResult
		want   string
	}{
		{"refused twice, then applied", fixtures.Fault{Mode: fixtures.Refuse, Match: "refund", Count: 2}, limiter.Applied,
			attemptsList(refusedEntry, refusedEntry, `{"code":3,"t_us":1760000000000000,"credited":1000000000}`)},
		{"first reply dropped, then already applied", fixtures.Fault{Mode: fixtures.DropReply, Match: "refund", Count: 1}, limiter.AlreadyApplied,
			attemptsList(unknownEntry, `{"code":4,"t_us":1760000000000000,"credited":1760000000000000}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel() // each case has its own driver and store
			d := fastDriver(t, InstanceSpec{})
			op := d.Acquire(0, "k")
			if op.Kind != limiter.Allowed {
				t.Fatalf("acquire: %v, want allowed", op.Kind)
			}
			if err := d.StoreWrapper(0).Arm(c.fault); err != nil {
				t.Fatal(err)
			}
			d.Release(op)
			ev, err := d.WaitRefund(op, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Result != c.result {
				t.Errorf("event %+v, want %v", ev, c.result)
			}
			if got := refundAttemptsJSON(t, d, op.Permit.DeductionID()); got != c.want {
				t.Errorf("refund record attempts\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestDriverReleaseOutsideDriver (plan S6 steps 10 and 12): a permit acquired through the driver
// but released with its own Release(), not the driver's, gets its release record
// {"type":"release","instance":"0","deduction_id":…} when its final event arrives, once and
// before its refund record, so check_bud11 joins the refund to a release ("refund without
// release" would exit 2) and the teardown checks pass.
func TestDriverReleaseOutsideDriver(t *testing.T) {
	d := New(t, Options{Instances: 1})
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire: %v, want allowed", op.Kind)
	}
	op.Permit.Release()
	if ev, err := d.WaitRefund(op, 5*time.Second); err != nil || ev.Result != limiter.Applied {
		t.Fatalf("refund event %+v (%v), want applied", ev, err)
	}
	id := op.Permit.DeductionID()
	want := `{"type":"release","instance":"0","deduction_id":"` + id + `"}`
	var releases, refunds []int
	for i, l := range d.OpLog() {
		switch {
		case strings.Contains(l, `"type":"release"`):
			if l != want {
				t.Errorf("release record %s, want %s", l, want)
			}
			releases = append(releases, i)
		case strings.Contains(l, `"type":"refund"`):
			refunds = append(refunds, i)
		}
	}
	if len(releases) != 1 || len(refunds) != 1 || releases[0] > refunds[0] {
		t.Errorf("release records at %v and refund records at %v, want one of each, the release first", releases, refunds)
	}
	f := &recTB{TB: t}
	d.teardownChecks(f)
	if errs, fatals := f.reports(); len(errs)+len(fatals) != 0 {
		t.Errorf("teardown reports %q, fatal %q; want a clean pass", errs, fatals)
	}
}

// TestDriverLateRefundAttempt (orchestrator decision, S6 task 2; SPEC API-1d): the retention
// timer (FastPolicy: 6 s after the acquire) finalizes a released permit, unresolved, while its
// refund call is in flight: the refusal lifts 1 s before, the next call reaches the store, which
// applies it, and an after_reply barrier holds the E_APPLIED reply past the timer. The timer
// cancels the call, which the barrier then lets return, and the event waits for it: it counts
// the command the call sent (Attempts 1, API-1d), and the call is captured before the event
// reaches the driver. The limiter does not act on that outcome (the timer won the permit), so
// the refund record gets the call as a sent {"outcome_unknown", 0, 0} after the refused calls:
// written with the record when the event reaches a free listener, and so too when the event
// first waits in the listener queue behind a blocked callback. Either way the record stays
// well-formed and check_bud11 counts the possible credit: the key's 21st allow at the frozen
// instant, which only that credit allows, passes the teardown checks (a record of refused
// calls alone fails BUD-11 with exit status 1, one ending with code 3 under unresolved with
// exit status 2).
func TestDriverLateRefundAttempt(t *testing.T) {
	for _, c := range []struct {
		name       string
		eventFirst bool
	}{
		{"the event waits for the held call", true},
		{"the call returns before the event", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel() // each case has its own driver and store
			lateRefund(t, c.eventFirst)
		})
	}
}

// lateRefund runs one case of TestDriverLateRefundAttempt.
func lateRefund(t *testing.T, eventFirst bool) {
	const wait = 10 * time.Second // bounds each wait for a state the test drives to
	hold := make(chan struct{})
	var holdOnce sync.Once
	unblock := func() { holdOnce.Do(func() { close(hold) }) }
	var calls atomic.Int64
	spec := InstanceSpec{}
	if !eventFirst {
		spec.Listener = func(limiter.RefundEvent) {
			if calls.Add(1) == 1 {
				<-hold // the warm-up's event: the later ones wait in the listener queue
			}
		}
	}
	d := fastDriver(t, spec)
	t.Cleanup(unblock) // runs before the driver's teardown
	warm := d.Acquire(0, "warm")
	if warm.Kind != limiter.Allowed {
		t.Fatalf("warm-up acquire: %v, want allowed", warm.Kind)
	}
	d.Release(warm) // loads the refund script, so the late call sends one command
	if _, err := d.WaitRefund(warm, wait); err != nil {
		t.Fatal(err)
	}

	lifetime := time.Duration(d.Policy().ReceiptLifetimeNs)
	begun := time.Now() // the retention timer fires lifetime after the acquire's send, so after begun + lifetime
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire: %v, want allowed", op.Kind)
	}
	id := op.Permit.DeductionID()
	for n := range 19 { // the rest of the full bucket at the frozen instant
		o := d.Acquire(0, "k")
		if o.Kind != limiter.Allowed {
			t.Fatalf("acquire %d: %v, want allowed", n+2, o.Kind)
		}
		d.Done(o)
	}
	sw := d.StoreWrapper(0)
	for _, f := range []fixtures.Fault{
		{Mode: fixtures.Refuse, Match: "refund", Duration: lifetime - time.Second},
		{Mode: fixtures.Barrier, Match: "refund", Count: 1, At: fixtures.AfterReply, Name: "late"},
	} {
		if err := sw.Arm(f); err != nil {
			t.Fatal(err)
		}
	}
	d.Release(op)
	retention := begun.Add(lifetime)
	select {
	case <-sw.Reached("late"):
	case <-time.After(time.Until(retention.Add(wait))):
		t.Fatal("no refund call got past the refusal to the after_reply barrier")
	}
	refused := len(d.RefundAttempts(op)) // the held call is captured when it returns
	finalized := func() bool {
		v, _ := MetricValue(d.Limiter(0).Metrics(), "rl_refund_results_total", "unresolved")
		return v == 1
	}
	for !finalized() {
		if time.Now().After(retention.Add(wait)) {
			t.Fatalf("no unresolved result counted within %v of the retention timer: the timer cancels the held call, and the permit's event follows once that call has returned", wait)
		}
		time.Sleep(2 * time.Millisecond)
	}
	early := slices.Repeat([]string{refusedEntry}, refused)
	if eventFirst {
		// The event reaches the free listener only once the held call has returned (the timer
		// cancelled it): the call is already captured, and the record is written with it,
		// before the barrier is released.
		if _, err := d.WaitRefund(op, wait); err != nil {
			t.Fatal(err)
		}
		if n := len(d.RefundAttempts(op)); n != refused+1 {
			t.Fatalf("%d refund calls captured when the event arrived, want %d: the held call returns before the event", n, refused+1)
		}
		if got, want := refundAttemptsJSON(t, d, id), attemptsList(append(early, unknownEntry)...); got != want {
			t.Fatalf("refund record when the event arrived\n got %s\nwant %s", got, want)
		}
	}
	sw.Release("late")
	returned := time.Now().Add(wait)
	for len(d.RefundAttempts(op)) != refused+1 {
		if time.Now().After(returned) {
			t.Fatal("the held refund call did not return")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if r := d.RefundAttempts(op)[refused].Response; r.Outcome != store.OK || r.Reply.Code != 3 || r.SentCommands != 1 {
		t.Fatalf("the late call: %v, code %d, %d commands sent; want E_APPLIED (code 3) with 1 command", r.Outcome, r.Reply.Code, r.SentCommands)
	}
	if !eventFirst {
		if recs := refundRecordsOf(t, d, id); len(recs) != 0 {
			t.Fatalf("refund records %q before the event left the listener queue, want none", recs)
		}
		unblock()
	}
	ev, err := d.WaitRefund(op, wait)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Result != limiter.Unresolved || ev.Reason != limiter.UnresolvedRetention || ev.Attempts != 1 {
		t.Errorf("event %+v, want unresolved (retention) with 1 attempt: the held call sent its command (API-1d)", ev)
	}
	if got, want := refundAttemptsJSON(t, d, id), attemptsList(append(early, unknownEntry)...); got != want {
		t.Errorf("refund record attempts\n got %s\nwant %s", got, want)
	}
	extra := d.Acquire(0, "k") // the 21st at the frozen instant: the late credit allows it
	if extra.Kind != limiter.Allowed {
		t.Fatalf("acquire after the late credit: %v, want allowed", extra.Kind)
	}
	d.Done(extra)
	f := &recTB{TB: t}
	d.teardownChecks(f)
	if errs, fatals := f.reports(); len(errs)+len(fatals) != 0 {
		t.Errorf("teardown reports %q, fatal %q; want a clean pass", errs, fatals)
	}
}

// capturedReply is a refund call's response with a store reply of §A4.9 code whose elements
// are code, t (µs), reason and then extra; one command sent.
func capturedReply(code, tUs int64, reason string, extra ...int64) store.Response {
	elems := []resp.Value{{Kind: resp.Int, Int: code}, {Kind: resp.Int, Int: tUs}, {Kind: resp.Bulk, Str: reason}}
	for _, v := range extra {
		elems = append(elems, resp.Value{Kind: resp.Int, Int: v})
	}
	return store.Response{Outcome: store.OK, Sent: true, SentCommands: 1, Reply: store.Reply{Code: code, TUs: tUs, Reason: reason, Elems: elems}}
}

// TestDriverAttemptRecords (orchestrator decisions, S6 task 2 and review round 1): the attempts
// of a refund record, by hand, from synthetic captured calls at t = 1,760,000,000,000,000 µs.
// The refund worker counts a call's commands (API-1d) before it ends the permit, so the
// retention timer can win the permit between the two: the unresolved event then counts a call
// that got a final reply. Every captured call with a final code (3, 4, 10, 11) under an
// unresolved event, whatever its reason, is a sent {"outcome_unknown", 0, 0}, as a call the
// event did not count is, whatever its code (attemptRecords' guard; the code 22 row pins it,
// and the row of counted codes 21 and 24 its other side); counted calls with other codes keep
// theirs, and so does every call of a final event. With a final code kept, the record ends
// with it under unresolved, which check_bud11 rejects with exit status 2.
func TestDriverAttemptRecords(t *testing.T) {
	const tUs = 1_760_000_000_000_000
	refused := store.Response{Outcome: store.Unavailable}
	dropped := store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1}
	applied := capturedReply(3, tUs, "", 1_000_000_000)
	unresolved := func(reason limiter.UnresolvedReason, attempts int) limiter.RefundEvent {
		return limiter.RefundEvent{Result: limiter.Unresolved, Reason: reason, Attempts: attempts}
	}
	for _, c := range []struct {
		name  string
		calls []store.Response
		ev    limiter.RefundEvent
		want  string
	}{
		{"E_APPLIED counted, then the retention timer won", []store.Response{refused, applied},
			unresolved(limiter.UnresolvedRetention, 1), attemptsList(refusedEntry, unknownEntry)},
		{"E_ALREADY_APPLIED counted, under unresolved (closed) too: the rule ignores the reason", []store.Response{dropped, capturedReply(4, tUs, "", tUs)},
			unresolved(limiter.UnresolvedClosed, 2), attemptsList(unknownEntry, unknownEntry)},
		{"E_RECEIPT_INVALID counted, then the retention timer won", []store.Response{capturedReply(10, tUs, "epoch")},
			unresolved(limiter.UnresolvedRetention, 1), attemptsList(unknownEntry)},
		{"E_RECEIPT_EXPIRED counted, then the retention timer won", []store.Response{capturedReply(11, tUs, "expired")},
			unresolved(limiter.UnresolvedRetention, 1), attemptsList(unknownEntry)},
		{"E_APPLIED not counted by the event (its final code rewrites it even without the guard)", []store.Response{refused, applied},
			unresolved(limiter.UnresolvedRetention, 0), attemptsList(refusedEntry, unknownEntry)},
		{"code 22, sent but not counted by the event (the guard: past the event's count, a code that is not final is rewritten too)", []store.Response{capturedReply(22, 0, "hwm_missing")},
			unresolved(limiter.UnresolvedRetention, 0), attemptsList(unknownEntry)},
		{"counted calls with codes that are not final keep them", []store.Response{capturedReply(21, 0, "clock"), capturedReply(24, tUs, "key_markers")},
			unresolved(limiter.UnresolvedRetention, 2),
			attemptsList(`{"code":21,"t_us":0,"credited":0}`, `{"code":24,"t_us":1760000000000000,"credited":0}`)},
		{"a final event keeps every code", []store.Response{refused, applied},
			limiter.RefundEvent{Result: limiter.Applied, Attempts: 1},
			attemptsList(refusedEntry, `{"code":3,"t_us":1760000000000000,"credited":1000000000}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			as := make([]RefundAttempt, 0, len(c.calls))
			for _, r := range c.calls {
				as = append(as, RefundAttempt{DeductionID: "d", Response: r, Store: r, Reached: r.Sent})
			}
			b, err := json.Marshal(attemptRecords(as, c.ev))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(b); got != c.want {
				t.Errorf("attempts of the %v event\n got %s\nwant %s", c.ev.Result, got, c.want)
			}
		})
	}
}

// TestDriverCountedLateRefundAttempt (orchestrator decision, S6 review round 1): the refund
// record of an unresolved event that counted a call with a final reply passes the teardown
// checks. The library's window between counting the call and losing the permit is microseconds
// wide, so the test takes a real captured call: the first permit of a new key at the frozen
// instant, after the rest of the full bucket, is refunded (E_APPLIED, one token), and the record
// the driver writes for an unresolved (retention) event with the same count replaces the applied
// record in the op log. The call is a sent {"outcome_unknown", 0, 0}, and the key's 21st allow
// at the frozen instant, which only that credit allows, passes check_bud11 (with code 3 kept
// under unresolved it exits with status 2).
func TestDriverCountedLateRefundAttempt(t *testing.T) {
	d := fastDriver(t, InstanceSpec{})
	op := d.Acquire(0, "k")
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire: %v, want allowed", op.Kind)
	}
	for n := range 19 { // the rest of the full bucket at the frozen instant
		o := d.Acquire(0, "k")
		if o.Kind != limiter.Allowed {
			t.Fatalf("acquire %d: %v, want allowed", n+2, o.Kind)
		}
		d.Done(o)
	}
	d.Release(op)
	ev, err := d.WaitRefund(op, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Result != limiter.Applied || ev.Attempts < 1 {
		t.Fatalf("event %+v, want applied with its attempts counted", ev)
	}
	id := op.Permit.DeductionID()
	lost := limiter.RefundEvent{DeductionID: id, Result: limiter.Unresolved, Reason: limiter.UnresolvedRetention, Attempts: ev.Attempts}
	records := attemptRecords(d.RefundAttempts(op), lost)
	editRecord(t, d, lastLine(t, d, `"type":"refund"`), func(m map[string]any) {
		m["attempts"], m["result"] = records, limiter.Unresolved.String()
	})
	if got, want := refundAttemptsJSON(t, d, id), attemptsList(unknownEntry); got != want {
		t.Errorf("refund record attempts of the unresolved event\n got %s\nwant %s", got, want)
	}
	extra := d.Acquire(0, "k") // the 21st at the frozen instant: the credit allows it
	if extra.Kind != limiter.Allowed {
		t.Fatalf("acquire after the credit: %v, want allowed", extra.Kind)
	}
	d.Done(extra)
	f := &recTB{TB: t}
	d.teardownChecks(f)
	if errs, fatals := f.reports(); len(errs)+len(fatals) != 0 {
		t.Errorf("teardown reports %q, fatal %q; want a clean pass", errs, fatals)
	}
}
