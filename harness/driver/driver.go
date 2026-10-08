//go:build rltest

// Package driver is the direct library driver (DESIGN §C4; HARNESS §9.4; IMPLEMENTATION_PLAN
// S5 steps 10, 11 and 16, S6 steps 10 and 13, S7 step 11, S8 steps 5 and 10): one store per
// driver (the fast profile unless Options.Profile says otherwise), its controlled time and
// epoch, and N limiter instances built through limitertest, each with its own store wrapper,
// signer wrapper, send observer, refund listener and signing-key canary. It logs every acquire,
// permit finish, final refund event, clock step, cleanup step, cleanup page read, epoch start or
// replace, restore, clear and inspect snapshot for the reference model (CheckRefModel),
// captures every refund attempt and cleanup step, reads the store as the ACL users the plan
// names (StartEpoch, ReadEpoch, Status and Inspect as rl_admin; the clock, Fixture and Tamper as
// rl_fixture; Dump, StoreKeys, SetHold and the audit as rl_lib; Info as rl_ops), runs rladmin as
// rl_admin (RLAdmin, ReplaceEpoch, Restore, Clear, LogInspect), restarts the store with its data
// kept, puts an instance behind its own RESP proxy on request (InstanceSpec.ViaProxy, S9 step
// 6), and at teardown, once every instance is closed or killed and while the store still
// runs, checks the commands and refund attempts each instance sent, audits the store and its
// AOF files, and checks BUD-11 on the op log.
//
// The methods that read the store or build instances fail the test through the driver's
// testing.TB, so call them from the test's goroutine; Acquire and the accessors may run on
// any goroutine. Shapes are plan choices (DESIGN §C8).
package driver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ratelimiter/harness/fixtures"
	"ratelimiter/harness/fixtures/respproxy"
	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/internal/testutil/storestate"
	"ratelimiter/limiter"
	"ratelimiter/limitertest"
)

// DefaultStartNs is the controlled time New writes unless Options.StartNs is set (plan
// choice), in ns.
const DefaultStartNs int64 = 1_760_000_000_000_000_000

// FillerPrefix is the key prefix outside the §A3.2 layout that driver tests may fill (S9's
// padding); the audit accepts any key under it.
const FillerPrefix = "rl:pad:"

// readyWait bounds New's wait for the instances it builds to join the epoch (plan choice).
const readyWait = 5 * time.Second

// DefaultPolicy returns the harness policy (DESIGN §A5.10): the SPEC defaults with
// max_tracked_buckets 100,000 and max_live_markers 50,000, on the controlled clock.
func DefaultPolicy() limiter.Policy {
	p := limiter.DefaultPolicy()
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 100_000, 50_000
	p.ClockSource = limiter.Controlled
	return p
}

// DefaultInstance returns the harness instance settings for p (DESIGN §A5.10): P = 5 s,
// margin 1 s, max_held_receipts = max_pending_refunds = 1,152.
func DefaultInstance(p limiter.Policy) limiter.Instance {
	in := limiter.DefaultInstance(p)
	in.MaxReleaseDelayNs, in.MarginNs = 5_000_000_000, 1_000_000_000
	in.MaxHeldReceipts, in.MaxPendingRefunds = 1152, 1152
	return in
}

// Options configure New.
type Options struct {
	Instances int              // instances New builds (0 is allowed: AddInstance adds them)
	Policy    limiter.Policy   // the deployment's policy; the zero value is DefaultPolicy()
	Instance  limiter.Instance // the instance settings; the zero value is DefaultInstance(Policy)
	NoEpoch   bool             // start no epoch (StartEpoch starts it later)
	StartNs   int64            // the first controlled time; 0 is DefaultStartNs
	// Profile is the store's deployment profile: the zero value is storeproc.Fast; durability
	// tests (T-INT-11, T-CLK-5) take storeproc.Durable.
	Profile storeproc.Profile
	// PerInstance holds a spec for each of the first instances New builds.
	PerInstance []InstanceSpec
}

// InstanceSpec overrides the driver's settings for one instance.
type InstanceSpec struct {
	Policy   *limiter.Policy       // nil: the driver's policy
	Instance *limiter.Instance     // nil: the driver's instance settings
	Store    *limiter.StoreOptions // nil: the driver's store as rl_lib
	// KeyReader returns the signing-key source of one build; nil: the instance's canary (plan
	// S6 step 13: SHA-256 of the test name and the instance index), which the teardown audit
	// looks for in the store. Every build (New, AddInstance, Restart) calls it once, so a
	// restarted instance gets a fresh reader.
	KeyReader func() io.Reader
	// Listener, when set, hears every final refund event after the driver's own listener has
	// recorded it (limiter.WithRefundListener takes one listener, and the driver's comes
	// last, so a listener passed in Options would be replaced).
	Listener func(limiter.RefundEvent)
	// Options go to limitertest.New before the driver's own. Hook options compose with the
	// driver's (limitertest): a store or signer wrapper given here sits inside the driver's,
	// nearer the store, and an observer given here hears every send too, so the driver's
	// recorder, fault wrappers, Sent and teardown checks keep running.
	Options []limitertest.Option
	// ViaProxy puts the instance behind a respproxy of its own in front of its store (plan S9
	// step 6): its StoreOptions.Addr is the proxy's address, and Proxy returns the proxy. Each
	// build (New, AddInstance, Restart) gets a new proxy; the driver closes every proxy once
	// every instance is closed or killed.
	ViaProxy bool
}

// Send is one send-observer record (§A5.12): the command (the script name for EVALSHA), the
// bytes written and when the observer heard it.
type Send struct {
	Cmd   string
	Bytes int
	At    time.Time
}

// MetricValue returns the value of the series of family name whose label value is label
// ("" for a family without a label) in s.
func MetricValue(s limiter.MetricsSnapshot, name, label string) (float64, bool) {
	for _, f := range s.Families {
		if f.Name != name {
			continue
		}
		for _, se := range f.Series {
			if se.LabelValue == label {
				return se.Value, true
			}
		}
	}
	return 0, false
}

// Count returns how many of sends wrote bytes of cmd.
func Count(sends []Send, cmd string) int {
	n := 0
	for _, s := range sends {
		if s.Cmd == cmd && s.Bytes > 0 {
			n++
		}
	}
	return n
}

// Op is one acquire through the driver: its result and what its store call returned.
type Op struct {
	limiter.Result
	Instance int
	Key      string
	// Reply is the decoded acquire reply, when one came back (Replied); it gives the op log
	// its u, g, s and expiry_us.
	Reply   store.Reply
	Replied bool
	// NoScript is set when the acquire's response came back with NoScript (a reload ran), and
	// SentCommands counts the acquire commands written for the call (§A5.4).
	NoScript     bool
	SentCommands int
}

// Driver is one deployment under test: its store, its controlled clock and its instances.
type Driver struct {
	t      testing.TB
	st     *storeproc.Store
	policy limiter.Policy
	inst   limiter.Instance
	clock  *fixtures.Clock

	clockConn *swapConn // the clock's rl_fixture connection (RestartStore replaces it)

	adminMu sync.Mutex
	admin   *storeproc.Client // rl_admin

	opsMu sync.Mutex
	ops   *storeproc.Client // rl_ops, opened by Info

	fixMu   sync.Mutex
	fixture *Fixture

	mu       sync.Mutex
	insts    []*instance      // the current instance of each index
	all      []*instance      // every instance ever built, for teardown
	oplog    [][]byte         // op-log lines, without their newline
	logged   int              // acquire, release, done, cleanup, cleanup_scan, restore and clear records in the op log
	policies []limiter.Policy // every policy an epoch was started or replaced with
	damaged  []string         // keys written through Fixture (and so through Tamper)
	restored []string         // the k:<enc> of every key an rladmin restore or clear wrote
	wiped    bool             // a FLUSHALL or FLUSHDB went through Fixture
	adminDir string           // where RLAdmin writes policy files, made on first use
	permits  map[string]*permitRec
	attempts map[string][]RefundAttempt // by deduction_id
	canaries map[int][]byte             // each index's signing-key canary, when the driver chose it
	steps    []CleanupStep              // every cleanup step captured, in completion order
	proxies  []*respproxy.Proxy         // every proxy a ViaProxy build started, closed at teardown
}

// instance is one limiter the driver built, with its wrappers and send records.
type instance struct {
	index int
	spec  InstanceSpec
	l     *limiter.Limiter
	sw    *fixtures.StoreWrapper
	sg    *fixtures.SignerWrapper
	proxy *respproxy.Proxy // nil unless the spec asked for ViaProxy

	mu     sync.Mutex
	sent   []Send
	events []limiter.RefundEvent // the final refund events its driver listener received
}

func (in *instance) observe(cmd string, n int) {
	at := time.Now()
	in.mu.Lock()
	defer in.mu.Unlock()
	in.sent = append(in.sent, Send{Cmd: cmd, Bytes: n, At: at})
}

func (in *instance) sends() []Send {
	in.mu.Lock()
	defer in.mu.Unlock()
	return slices.Clone(in.sent)
}

// New spawns a store of o.Profile, writes the controlled time, starts the epoch (unless
// NoEpoch) and builds o.Instances instances, waiting (≤ 5 s) until each one that shares the
// epoch's fingerprint and the driver's store options is ready. t.Cleanup closes every instance, runs the teardown checks while the
// store still runs (cleanups run last-in, first-out, and the store's was registered first),
// then closes the driver's connections.
func New(t testing.TB, o Options) *Driver {
	t.Helper()
	p := o.Policy
	if p == (limiter.Policy{}) {
		p = DefaultPolicy()
	}
	in := o.Instance
	if in == (limiter.Instance{}) {
		in = DefaultInstance(p)
	}
	start := o.StartNs
	if start == 0 {
		start = DefaultStartNs
	}
	d := &Driver{
		t: t, policy: p, inst: in,
		permits: map[string]*permitRec{}, attempts: map[string][]RefundAttempt{}, canaries: map[int][]byte{},
	}
	d.st = storeproc.Start(t, storeproc.Options{Profile: o.Profile})
	t.Cleanup(d.cleanup)
	cc, err := d.st.Conn("rl_fixture")
	if err != nil {
		t.Fatalf("driver: clock connection: %v", err)
	}
	d.clockConn = &swapConn{c: cc}
	if d.admin, err = d.st.Conn("rl_admin"); err != nil {
		t.Fatalf("driver: admin connection: %v", err)
	}
	d.clock = fixtures.NewClock(d.clockConn, d.logClock)
	if err := d.clock.Set(start); err != nil {
		t.Fatalf("driver: %v", err)
	}
	epochFP := ""
	if !o.NoEpoch {
		d.StartEpoch()
		epochFP = fingerprint.Of(p)
	}
	var wait []int
	for i := range o.Instances {
		var spec InstanceSpec
		if i < len(o.PerInstance) {
			spec = o.PerInstance[i]
		}
		if _, err := d.AddInstance(spec); err != nil {
			t.Fatalf("driver: instance %d: %v", i, err)
		}
		if epochFP != "" && spec.Store == nil && fingerprint.Of(d.policyOf(spec)) == epochFP {
			wait = append(wait, i)
		}
	}
	deadline := time.Now().Add(readyWait)
	for _, i := range wait {
		if err := d.WaitReady(i, time.Until(deadline)); err != nil {
			t.Fatalf("driver: %v", err)
		}
	}
	return d
}

// policyOf returns the policy an instance with spec runs.
func (d *Driver) policyOf(spec InstanceSpec) limiter.Policy {
	if spec.Policy != nil {
		return *spec.Policy
	}
	return d.policy
}

// AddInstance builds one more instance through limitertest.New with the driver's settings,
// overridden by spec, and fresh wrappers, and returns its index and New's error: New never
// returns nil, so an instance whose settings are invalid is kept (permanently not ready).
// It does not wait for the instance to join (WaitReady).
func (d *Driver) AddInstance(spec InstanceSpec) (int, error) {
	d.mu.Lock()
	i := len(d.insts)
	d.insts = append(d.insts, nil)
	d.mu.Unlock()
	inst, err := d.build(i, spec)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.insts[i] = inst
	d.all = append(d.all, inst)
	return i, err
}

// build makes instance i from spec, with a fresh store wrapper (in front of which the driver
// records each acquire's response and each refund attempt, and behind which each refund's
// response, each cleanup step result and each due_entries page before the faults), signer
// wrapper, send observer, refund listener (last, so it is
// the one the limiter keeps), unless spec has a key reader, the index's canary as the signing
// key, and for ViaProxy a new respproxy in front of the store options' address.
func (d *Driver) build(i int, spec InstanceSpec) (*instance, error) {
	p := d.policyOf(spec)
	in := d.inst
	if spec.Instance != nil {
		in = *spec.Instance
	}
	so := limiter.StoreOptions{Addr: d.st.Addr, User: "rl_lib", Password: d.st.PW.Lib}
	if spec.Store != nil {
		so = *spec.Store
	}
	inst := &instance{index: i, spec: spec, sw: fixtures.NewStoreWrapper(), sg: fixtures.NewSignerWrapper()}
	if spec.ViaProxy {
		px, err := respproxy.New(d.t.Context(), so.Addr)
		if err != nil {
			d.t.Fatalf("driver: instance %d: %v", i, err)
			return inst, err
		}
		d.mu.Lock()
		d.proxies = append(d.proxies, px)
		d.mu.Unlock()
		inst.proxy, so.Addr = px, px.Addr()
	}
	opts := slices.Concat(spec.Options, []limitertest.Option{
		limitertest.WithControlledClock(),
		limitertest.WithStoreWrapper(func(a limitertest.Adapter) limitertest.Adapter {
			return recorder{Adapter: inst.sw.Wrap(inner{Adapter: a, d: d, inst: inst}), d: d, inst: inst}
		}),
		limitertest.WithSignerWrapper(inst.sg.Wrap),
		limitertest.WithSendObserver(inst.observe),
		limitertest.With(limiter.WithRefundListener(d.listen(inst))),
	})
	if spec.KeyReader != nil {
		opts = append(opts, limitertest.WithKeyReader(spec.KeyReader()))
	} else {
		c := canaryOf(d.t.Name(), i)
		d.mu.Lock()
		d.canaries[i] = c
		d.mu.Unlock()
		opts = append(opts, limitertest.WithKeyReader(bytes.NewReader(c)))
	}
	l, err := limitertest.New(p, in, so, opts...)
	inst.l = l
	return inst, err
}

// current returns the current instance of index i, or nil after reporting a bad index.
func (d *Driver) current(i int) *instance {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i < 0 || i >= len(d.insts) || d.insts[i] == nil {
		d.t.Errorf("driver: no instance %d (%d built)", i, len(d.insts))
		return nil
	}
	return d.insts[i]
}

// Instances returns the number of instance indexes.
func (d *Driver) Instances() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.insts)
}

// Limiter returns instance i's limiter.
func (d *Driver) Limiter(i int) *limiter.Limiter {
	if in := d.current(i); in != nil {
		return in.l
	}
	return nil
}

// StoreWrapper returns instance i's store wrapper (faults, barriers).
func (d *Driver) StoreWrapper(i int) *fixtures.StoreWrapper {
	if in := d.current(i); in != nil {
		return in.sw
	}
	return nil
}

// SignerWrapper returns instance i's signer wrapper.
func (d *Driver) SignerWrapper(i int) *fixtures.SignerWrapper {
	if in := d.current(i); in != nil {
		return in.sg
	}
	return nil
}

// Proxy returns the respproxy in front of instance i's store (InstanceSpec.ViaProxy), or nil
// for an instance built without one.
func (d *Driver) Proxy(i int) *respproxy.Proxy {
	if in := d.current(i); in != nil {
		return in.proxy
	}
	return nil
}

// Sent returns instance i's send-observer records so far, oldest first (§A5.12): cmd is the
// script name for an EVALSHA ("acquire"), else the command ("SCRIPT LOAD", "AUTH", "PING").
func (d *Driver) Sent(i int) []Send {
	if in := d.current(i); in != nil {
		return in.sends()
	}
	return nil
}

// Clock returns the controlled clock driver. Its steps are logged as clock records.
func (d *Driver) Clock() *fixtures.Clock { return d.clock }

// Store returns the driver's store process.
func (d *Driver) Store() *storeproc.Store { return d.st }

// Policy returns the driver's policy: the one its epochs start with.
func (d *Driver) Policy() limiter.Policy { return d.policy }

// Instance returns the driver's instance settings.
func (d *Driver) Instance() limiter.Instance { return d.inst }

// WaitReady waits up to within until instance i reports ready.
func (d *Driver) WaitReady(i int, within time.Duration) error {
	in := d.current(i)
	if in == nil {
		return fmt.Errorf("driver: no instance %d", i)
	}
	deadline := time.Now().Add(within)
	for {
		h := in.l.Health()
		if h.Status == "ready" {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("driver: instance %d not ready within %v: %s (%s)", i, within, h.Reason, h.StoreError)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// WaitJoined waits up to within until instance i reports ready in epoch id (its health's
// epoch_id): after a replace, an instance stays ready in the old epoch until its next epoch
// read (§A5.7).
func (d *Driver) WaitJoined(i int, id string, within time.Duration) error {
	in := d.current(i)
	if in == nil {
		return fmt.Errorf("driver: no instance %d", i)
	}
	deadline := time.Now().Add(within)
	for {
		h := in.l.Health()
		if h.Status == "ready" && h.EpochID == id {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("driver: instance %d not ready in epoch %s within %v: %s/%s in epoch %q (%s)", i, id, within, h.Status, h.Reason, h.EpochID, h.StoreError)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Kill stops instance i at once (limitertest.Kill), within killWait (killBounded).
func (d *Driver) Kill(i int) {
	if in := d.current(i); in != nil {
		d.killBounded(in)
	}
}

// Close closes instance i gracefully, within its drain timeout plus closeSlack (closeBounded).
func (d *Driver) Close(i int) {
	if in := d.current(i); in != nil {
		d.closeBounded(d.t, in)
	}
}

const (
	// closeSlack is what the driver allows a Close beyond the instance's drain timeout, and
	// killWait bounds a Kill, which drains nothing (plan choice): past the drain, both only
	// stop the instance's goroutines, whose store calls end within D, and close its
	// connections.
	closeSlack = 5 * time.Second
	killWait   = 5 * time.Second
)

// closeBounded closes in and reports, with tb.Errorf, a Close that does not return within the
// instance's drain timeout (Health's DrainTimeoutNs, below 0 being 0 as for Close) plus
// closeSlack, such as one waiting for a goroutine of the instance that never stops (§A5.2).
// The stuck Close is left behind and the caller goes on, so the test's teardown and the
// store's stop still run.
func (d *Driver) closeBounded(tb testing.TB, in *instance) {
	tb.Helper()
	drain := min(max(in.l.Health().Instance.DrainTimeoutNs, 0), math.MaxInt64-int64(closeSlack))
	bound := time.Duration(drain) + closeSlack
	if !returnsWithin(bound, func() { in.l.Close() }) {
		tb.Errorf("instance %d: Close did not return within %v (its drain timeout plus %v)", in.index, bound, closeSlack)
	}
}

// killBounded kills in (limitertest.Kill) and fails the test when Kill does not return within
// killWait; the stuck Kill is left behind, as in closeBounded.
func (d *Driver) killBounded(in *instance) {
	d.t.Helper()
	if !returnsWithin(killWait, func() { limitertest.Kill(in.l) }) {
		d.t.Errorf("driver: instance %d: Kill did not return within %v", in.index, killWait)
	}
}

// returnsWithin runs f on a goroutine of its own and reports whether f returned within bound.
// An f that did not keeps running on that goroutine: a call that never returns, which the
// caller reports as a failure.
func returnsWithin(bound time.Duration, f func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Restart replaces instance i with a new one of the same spec, fresh wrappers and a fresh key
// reader (InstanceSpec.KeyReader), killing the old one first if it still runs (killBounded),
// and returns New's error. It does not wait for the new instance to join (WaitReady).
func (d *Driver) Restart(i int) error {
	old := d.current(i)
	if old == nil {
		return fmt.Errorf("driver: no instance %d", i)
	}
	d.killBounded(old)
	inst, err := d.build(i, old.spec)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.insts[i] = inst
	d.all = append(d.all, inst)
	return err
}

// captureKey is the context key of an acquire's capture.
type captureKey struct{}

// capture holds the response of the acquire call made under its context.
type capture struct {
	mu   sync.Mutex
	r    store.Response
	seen bool
}

func (c *capture) set(r store.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.r, c.seen = r, true
}

func (c *capture) get() (store.Response, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.r, c.seen
}

// canaryOf is the signing-key canary of instance index i in the test named name (plan S6 step
// 13): the 32 bytes of SHA-256(name ‖ 0x00 ‖ i in decimal), distinct per index.
func canaryOf(name string, i int) []byte {
	sum := sha256.Sum256([]byte(name + "\x00" + strconv.Itoa(i)))
	return sum[:]
}

// recorder sits in front of an instance's store wrapper: it hands each acquire response to
// the capture in the call's context (the limiter passes the host's context to the adapter,
// §A5.4), so concurrent acquires each get their own reply, and records each refund attempt
// (refund).
type recorder struct {
	store.Adapter
	d    *Driver
	inst *instance
}

func (r recorder) Call(ctx context.Context, req store.Request) store.Response {
	if isRefund(req) {
		return r.refund(ctx, req)
	}
	res := r.Adapter.Call(ctx, req)
	if c, ok := ctx.Value(captureKey{}).(*capture); ok && req.Script.Name == scripts.Acquire.Name {
		c.set(res)
	}
	return res
}

// Acquire runs one acquire of key on instance i with the test's context and logs it.
func (d *Driver) Acquire(i int, key string) Op { return d.acquire(d.t.Context(), i, key, "") }

// AcquireContext is Acquire with the caller's context (a cancelled or short deadline).
func (d *Driver) AcquireContext(ctx context.Context, i int, key string) Op {
	return d.acquire(ctx, i, key, "")
}

// AcquireTagged is Acquire for an op the host tags with its own request ID, which the
// library does not take (T-COR-8). The op log carries it in the acquire record's instance
// field, "<i> request_id=<id>" (plan choice: the record's fields are fixed, step 11, and
// replay compares every field but instance).
func (d *Driver) AcquireTagged(i int, key, requestID string) Op {
	return d.acquire(d.t.Context(), i, key, requestID)
}

func (d *Driver) acquire(ctx context.Context, i int, key, tag string) Op {
	in := d.current(i)
	if in == nil {
		return Op{Instance: i, Key: key}
	}
	c := &capture{}
	r := in.l.Acquire(context.WithValue(ctx, captureKey{}, c), key)
	op := Op{Result: r, Instance: i, Key: key}
	if res, ok := c.get(); ok {
		op.NoScript, op.SentCommands = res.NoScript, res.SentCommands
		if res.Outcome == store.OK {
			op.Reply, op.Replied = res.Reply, true
		}
	}
	d.logAcquire(in, op, tag)
	return op
}

// StartEpoch starts an epoch with the driver's policy as rl_admin (admin_start_epoch, ADM-1;
// the S8 epoch replaces go through rladmin: ReplaceEpoch): a new 16-byte crypto/rand id,
// fingerprint.Of(policy), the policy's clock source, no replace flag. It logs a policy record
// and the epoch record with origin_ns from the reply's budget time, and returns the id.
func (d *Driver) StartEpoch() string {
	d.t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		d.t.Fatalf("driver: epoch id: %v", err)
		return ""
	}
	id := hex.EncodeToString(raw[:])
	p := d.policy
	rep, ok := d.script(scripts.AdminStartEpoch, []string{keys.EpochKey, keys.CtlTimeKey},
		[]string{id, fingerprint.Of(p), "1", p.ClockSource.String(), "0", ""})
	if !ok {
		return ""
	}
	if rep.Code != 40 || elemStr(rep, 3) != id {
		d.t.Fatalf("driver: admin_start_epoch: code %d %q, new id %q, want 40 and %s", rep.Code, rep.Reason, elemStr(rep, 3), id)
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.policies = append(d.policies, p)
	d.appendLocked(policyRecordOf(p), epochRecord{Type: "epoch", EpochID: id, OriginNs: rep.TUs * 1000})
	return id
}

// Epoch is a decoded read_epoch reply (§A4.9).
type Epoch struct {
	State                     int64 // 0 healthy, 22 an epoch-wide problem
	Reason                    string
	ID, FP                    string // "" when unreadable
	HWM, Gen, ClockUs, NB, NM int64
	Quarantined               string
}

// ReadEpoch runs read_epoch as rl_admin, with the placeholder q (DESIGN §A4.1).
func (d *Driver) ReadEpoch() Epoch {
	d.t.Helper()
	rep, ok := d.script(scripts.ReadEpoch, []string{keys.EpochKey, keys.CtlTimeKey, keys.QuarantinePlaceholder}, nil)
	if !ok {
		return Epoch{}
	}
	return Epoch{
		State: rep.Code, Reason: rep.Reason, ID: elemStr(rep, 3), FP: elemStr(rep, 4),
		HWM: elemInt(rep, 5), Gen: elemInt(rep, 6), ClockUs: elemInt(rep, 7),
		NB: elemInt(rep, 8), NM: elemInt(rep, 9), Quarantined: elemStr(rep, 10),
	}
}

// Status is a decoded admin_status reply (ADM-4, §A4.9).
type Status struct {
	Code             int64  // 40 OK, 41 refused
	Reason           string // the refusal's reason
	EpochID, FP      string
	Hold             string
	HWM              int64 // -1 when the reply shows '' (missing or malformed)
	NB, NM           int64
	QuarantinedCount int64
}

// Status runs admin_status as rl_admin with the expected id from read_epoch, reading it
// again and retrying once on epoch_changed.
func (d *Driver) Status() Status {
	d.t.Helper()
	for try := 0; ; try++ {
		e := d.ReadEpoch()
		rep, ok := d.script(scripts.AdminStatus, []string{keys.EpochKey, quarantineKey(e.ID)}, []string{e.ID})
		if !ok {
			return Status{}
		}
		if rep.Code == 41 && rep.Reason == "epoch_changed" && try == 0 {
			continue
		}
		if rep.Code != 40 {
			return Status{Code: rep.Code, Reason: rep.Reason}
		}
		return Status{
			Code: 40, EpochID: elemStr(rep, 3), FP: elemStr(rep, 4), Hold: elemStr(rep, 5),
			HWM: elemOpt(rep, 6), NB: elemInt(rep, 7), NM: elemInt(rep, 8), QuarantinedCount: elemInt(rep, 9),
		}
	}
}

// Inspect is a decoded admin_inspect reply (ADM-4, §A4.9). Each number is -1 where the reply
// shows ” (missing or unreadable).
type Inspect struct {
	Code       int64  // 40 OK, 41 refused
	Reason     string // the refusal's reason
	Present    int64  // 0 absent, 1 present, 2 not a hash
	U, T, G, S int64
	X, MC      int64 // the latest receipt expiry and the marker count
	IxScore    int64
	Quarantine string
	Expiries   []int64
}

// Inspect runs admin_inspect of key as rl_admin with the expected id from read_epoch,
// reading it again and retrying once on epoch_changed. SPEC per-key reads use it (§10).
func (d *Driver) Inspect(key string) Inspect {
	d.t.Helper()
	enc, err := keys.Encode(key)
	if err != nil {
		d.t.Fatalf("driver: Inspect of an invalid key: %v", err)
		return Inspect{}
	}
	for try := 0; ; try++ {
		e := d.ReadEpoch()
		id := e.ID
		if id == "" {
			id = "!" // no epoch: the record check refuses before the KEYS are used
		}
		rep, ok := d.script(scripts.AdminInspect,
			[]string{keys.EpochKey, keys.IndexKey(id), keys.QuarantineKey(id), keys.BucketKey(id, enc), keys.MarkerIndexKey(id, enc)},
			[]string{e.ID, enc})
		if !ok {
			return Inspect{}
		}
		if rep.Code == 41 && rep.Reason == "epoch_changed" && try == 0 {
			continue
		}
		if rep.Code != 40 {
			return Inspect{Code: rep.Code, Reason: rep.Reason}
		}
		out := Inspect{
			Code: 40, Present: elemInt(rep, 3),
			U: elemOpt(rep, 4), T: elemOpt(rep, 5), G: elemOpt(rep, 6), S: elemOpt(rep, 7),
			X: elemOpt(rep, 8), MC: elemOpt(rep, 9), IxScore: elemOpt(rep, 10), Quarantine: elemStr(rep, 11),
		}
		for j := 13; j < len(rep.Elems); j++ { // the decoder checked the length, 13 + n
			out.Expiries = append(out.Expiries, elemOpt(rep, j))
		}
		return out
	}
}

// quarantineKey is the q of epoch id, or the placeholder when there is none.
func quarantineKey(id string) string {
	if id == "" {
		return keys.QuarantinePlaceholder
	}
	return keys.QuarantineKey(id)
}

// Dump returns a snapshot of every rl:* key, read as rl_lib (storestate.Snapshot).
func (d *Driver) Dump() storestate.State {
	d.t.Helper()
	c, err := d.st.Conn("rl_lib")
	if err != nil {
		d.t.Fatalf("driver: Dump: %v", err)
		return nil
	}
	defer func() { _ = c.Close() }()
	s, err := storestate.Snapshot(c)
	if err != nil {
		d.t.Fatalf("driver: Dump: %v", err)
		return nil
	}
	return s
}

// Info returns an INFO section, read as rl_ops on one connection the driver keeps.
func (d *Driver) Info(section string) map[string]string {
	d.t.Helper()
	d.opsMu.Lock()
	defer d.opsMu.Unlock()
	if d.ops == nil {
		c, err := d.st.Conn("rl_ops")
		if err != nil {
			d.t.Fatalf("driver: Info: %v", err)
			return nil
		}
		d.ops = c
	}
	r, err := d.ops.Do("INFO", section)
	if err != nil || r.Kind != '$' {
		d.t.Fatalf("driver: INFO %s: %v %s", section, err, r)
		return nil
	}
	return storeproc.ParseInfo(r.Str)
}

// script runs s as rl_admin on the driver's admin connection (scriptOn).
func (d *Driver) script(s scripts.Script, ks, args []string) (store.Reply, bool) {
	d.t.Helper()
	d.adminMu.Lock()
	defer d.adminMu.Unlock()
	return d.scriptOn(d.admin, "rl_admin", s, ks, args)
}

// scriptOn runs s on c, a connection authenticated as user: EVALSHA, and after NOSCRIPT one
// SCRIPT LOAD and one resend. The reply must pass store.Decode against its §A4.9 layout; any
// failure fails the test and reports false. The caller owns c for the call.
func (d *Driver) scriptOn(c *storeproc.Client, user string, s scripts.Script, ks, args []string) (store.Reply, bool) {
	d.t.Helper()
	cmd := append([]string{"EVALSHA", s.SHA, strconv.Itoa(len(ks))}, ks...)
	cmd = append(cmd, args...)
	r, err := c.Do(cmd...)
	if err == nil && r.Kind == '-' && strings.HasPrefix(r.Str, "NOSCRIPT") {
		var lr storeproc.Reply
		if lr, err = c.Do("SCRIPT", "LOAD", s.Text); err == nil && (lr.Kind == '-' || lr.Str != s.SHA) {
			err = fmt.Errorf("SCRIPT LOAD replied %s", lr)
		}
		if err == nil {
			r, err = c.Do(cmd...)
		}
	}
	if err != nil {
		d.t.Fatalf("driver: %s as %s: %v", s.Name, user, err)
		return store.Reply{}, false
	}
	rep, err := store.Decode(s.Name, toValue(r), store.LimitsFor(d.policy))
	if err != nil {
		d.t.Fatalf("driver: %s as %s: %v (reply %s)", s.Name, user, err, r)
		return store.Reply{}, false
	}
	return rep, true
}

// toValue converts a storeproc reply to the RESP value store.Decode takes.
func toValue(r storeproc.Reply) resp.Value {
	switch r.Kind {
	case '+':
		return resp.Value{Kind: resp.Simple, Str: r.Str}
	case '-':
		return resp.Value{Kind: resp.Error, Str: r.Str}
	case ':':
		return resp.Value{Kind: resp.Int, Int: r.Int}
	case '$':
		return resp.Value{Kind: resp.Bulk, Str: r.Str, Null: r.Nil}
	case '*':
		v := resp.Value{Kind: resp.Array, Null: r.Nil}
		for _, e := range r.Arr {
			v.Elems = append(v.Elems, toValue(e))
		}
		return v
	}
	return resp.Value{}
}

// elemInt returns element i of rep when it is an integer, else 0.
func elemInt(rep store.Reply, i int) int64 {
	if i < len(rep.Elems) && rep.Elems[i].Kind == resp.Int {
		return rep.Elems[i].Int
	}
	return 0
}

// elemOpt returns element i of rep when it is an integer, else -1 (the reply's ”).
func elemOpt(rep store.Reply, i int) int64 {
	if i < len(rep.Elems) && rep.Elems[i].Kind == resp.Int {
		return rep.Elems[i].Int
	}
	return -1
}

// elemStr returns element i of rep when it is a string, else "".
func elemStr(rep store.Reply, i int) string {
	if i < len(rep.Elems) && (rep.Elems[i].Kind == resp.Bulk || rep.Elems[i].Kind == resp.Simple) {
		return rep.Elems[i].Str
	}
	return ""
}

// Fixture is the driver's rl_fixture connection for tests that need damaged state (plan S5
// step 16): every key a write through it names is declared Damaged for the teardown audit,
// except rl:ctl:time, whose writes are normal (DESIGN §C4). It is safe for concurrent use.
type Fixture struct {
	d  *Driver
	mu sync.Mutex
	c  *storeproc.Client
}

// Fixture returns the driver's recording rl_fixture connection, opening it on first use.
func (d *Driver) Fixture() *Fixture {
	d.t.Helper()
	d.fixMu.Lock()
	defer d.fixMu.Unlock()
	if d.fixture == nil {
		c, err := d.st.Conn("rl_fixture")
		if err != nil {
			d.t.Fatalf("driver: Fixture: %v", err)
			return nil
		}
		d.fixture = &Fixture{d: d, c: c}
	}
	return d.fixture
}

// Do declares the keys the command writes, then sends it.
func (f *Fixture) Do(args ...string) (storeproc.Reply, error) {
	f.d.declare(writtenKeys(args))
	f.d.noteWipe(args)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.Do(args...)
}

// Pipeline declares the keys every command writes, then sends them in one write.
func (f *Fixture) Pipeline(cmds [][]string) ([]storeproc.Reply, error) {
	for _, c := range cmds {
		f.d.declare(writtenKeys(c))
		f.d.noteWipe(c)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.Pipeline(cmds)
}

// declare adds keys to the audit's Damaged list.
func (d *Driver) declare(ks []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range ks {
		if k != keys.CtlTimeKey && !slices.Contains(d.damaged, k) {
			d.damaged = append(d.damaged, k)
		}
	}
}

// noteWipe records a FLUSHALL or FLUSHDB sent through Fixture: it leaves no key to declare,
// but the test, like one that declared damage, is exempt from the teardown BUD-11 check.
func (d *Driver) noteWipe(args []string) {
	if len(args) == 0 {
		return
	}
	if c := strings.ToUpper(args[0]); c == "FLUSHALL" || c == "FLUSHDB" {
		d.mu.Lock()
		d.wiped = true
		d.mu.Unlock()
	}
}

// writtenKeys returns the keys a fixture command writes: none for reads and PING, every key
// of DEL, UNLINK and EVAL, the first argument of the other writes. FLUSHALL leaves no key to
// audit.
func writtenKeys(args []string) []string {
	if len(args) < 2 {
		return nil
	}
	switch strings.ToUpper(args[0]) {
	case "GET", "TYPE", "PING", "FLUSHALL", "HGETALL", "HGET", "ZRANGE", "ZSCORE", "EXISTS":
		return nil
	case "DEL", "UNLINK":
		return args[1:]
	case "EVAL", "EVALSHA":
		if len(args) < 3 {
			return nil
		}
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 0 || n > len(args)-3 {
			return nil
		}
		return args[3 : 3+n]
	}
	return args[1:2]
}

// cleanup is the driver's t.Cleanup: the teardown checks (which close every instance), then
// the instances' proxies and the driver's connections.
func (d *Driver) cleanup() {
	d.teardownChecks(d.t)
	d.mu.Lock()
	proxies := slices.Clone(d.proxies)
	d.mu.Unlock()
	for _, px := range proxies {
		px.Close()
	}
	d.fixMu.Lock()
	if d.fixture != nil {
		_ = d.fixture.c.Close()
	}
	d.fixMu.Unlock()
	if d.clockConn != nil {
		_ = d.clockConn.Close()
	}
	for _, c := range []*storeproc.Client{d.admin, d.ops} {
		if c != nil {
			_ = c.Close()
		}
	}
}
