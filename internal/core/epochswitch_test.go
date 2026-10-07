package core

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// The S8 epoch tests' values (IMPLEMENTATION_PLAN S8 Tests (a)): epoch A is clEpoch, joined with
// hwm clT0 − 9 and gen 4; its replacement B is clOther, whose record starts with gen 0 and an
// hwm below A's, so A's floors would hold B at once (§A5.7, plan S8 Watch out); later reads in
// B raise its floors. encK is the budget key "k" (base64url "aw").
const (
	s8HwmA, s8GenA   = clT0 - 9, 4
	s8HwmB, s8GenB   = clT0 - 5_000_000, 0
	s8HwmB2, s8GenB2 = clT0 + 7, 3
	encK             = "aw"
	s8Quiet          = 200 * time.Millisecond // how long a read that must not come is waited for
)

// epochFake is the store adapter of the S8 epoch tests (hookSet.storeWrapper): read_epoch
// answers with the response set last, and the time of each call is kept; acquire, refund,
// due_entries and the cleanup_entry pipelines go to the test's functions (unavailable, nothing
// sent, when unset); SCAN answers with the keys set, in one finished iteration; reclaim
// unlinks every candidate. It keeps every request but read_epoch's, in order.
type epochFake struct {
	mu                         sync.Mutex
	epoch                      store.Response
	reads                      []time.Time // when each read_epoch call came, in order
	acquire, refund, due, step func(store.Request) store.Response
	scanned                    []string
	reqs                       []store.Request
}

func newEpochFake(epoch store.Response) *epochFake { return &epochFake{epoch: epoch} }

func (f *epochFake) wrap(inner store.Adapter) store.Adapter {
	inner.Close()
	return f
}

func (f *epochFake) set(fn func(f *epochFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *epochFake) Call(_ context.Context, req store.Request) store.Response {
	f.mu.Lock()
	if req.Script.Name == "read_epoch" {
		f.reads = append(f.reads, time.Now())
		r := f.epoch
		f.mu.Unlock()
		return r
	}
	f.reqs = append(f.reqs, req)
	var fn func(store.Request) store.Response
	switch req.Script.Name {
	case "acquire":
		fn = f.acquire
	case "refund":
		fn = f.refund
	case "due_entries":
		fn = f.due
	case "reclaim":
		f.mu.Unlock()
		return clOK(40, 0, "", clInt(int64(len(req.Keys)-1)))
	}
	f.mu.Unlock()
	if fn == nil {
		return store.Response{Outcome: store.Unavailable}
	}
	return fn(req)
}

func (f *epochFake) CallBatch(_ context.Context, reqs []store.Request) []store.Response {
	f.mu.Lock()
	f.reqs = append(f.reqs, reqs...)
	step := f.step
	f.mu.Unlock()
	out := make([]store.Response, len(reqs))
	for i, r := range reqs {
		out[i] = store.Response{Outcome: store.Unavailable}
		if step != nil {
			out[i] = step(r)
		}
	}
	return out
}

func (f *epochFake) Do(_ context.Context, _ store.Kind, args ...string) (resp.Value, store.Response) {
	f.mu.Lock()
	found := slices.Clone(f.scanned)
	f.mu.Unlock()
	if len(args) > 0 && args[0] == "SCAN" {
		return scanValue("0", found...), store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
	}
	return resp.Value{}, store.Response{Outcome: store.Unavailable}
}

func (f *epochFake) ReadyConns(store.Kind) int { return 0 }

// Close has nothing to stop: wrap closed the real adapter.
func (f *epochFake) Close() {}

// requests returns the kept requests of one script, in order.
func (f *epochFake) requests(script string) []store.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Request
	for _, r := range f.reqs {
		if r.Script.Name == script {
			out = append(out, r)
		}
	}
	return out
}

// refundsOf returns the kept refund requests for the receipt with sequence s (ARGV 6), in
// order: one per attempt of its refund.
func (f *epochFake) refundsOf(s int64) []store.Request {
	var out []store.Request
	for _, r := range f.requests("refund") {
		if len(r.Args) > 5 && r.Args[5] == num(s) {
			out = append(out, r)
		}
	}
	return out
}

// readCount returns the number of read_epoch calls so far.
func (f *epochFake) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

// readTime waits until read_epoch has been called more than n times and returns when call n
// came (call 0 is the watch's startup read); it fails the test unless that happens within
// effectsWait.
func (f *epochFake) readTime(t *testing.T, n int, what string) time.Time {
	t.Helper()
	pollUntil(t, what, func() bool { return f.readCount() > n })
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[n]
}

// gateSigner is the signer hook of TestEpochSwitch_ResetsFloors (hookSet.signerWrapper): it
// passes every call to the signer it wraps, except that each Verify of the receipt with
// sequence s after its first, which starts a retry of that receipt's refund before the attempt
// loads the joined epoch (refundAttempt; API-2a step 1), first waits until open is closed or
// ctx ends. So a retry whose backoff ends before the test's epoch switch still builds its
// command after the switch.
type gateSigner struct {
	receipt.Signer
	ctx      context.Context
	s        uint64
	open     <-chan struct{}
	verifies atomic.Int32
}

// wrap is the hook: it keeps the limiter's signer and stands in its place.
func (g *gateSigner) wrap(s receipt.Signer) receipt.Signer {
	g.Signer = s
	return g
}

func (g *gateSigner) Verify(rc receipt.Receipt) bool {
	if rc.S == g.s && g.verifies.Add(1) > 1 {
		select {
		case <-g.open:
		case <-g.ctx.Done():
		}
	}
	return g.Signer.Verify(rc)
}

// epochLimiter builds a limiter on f with the cleanup tests' settings (a 1 h re-read interval
// and cleanup interval: the watch reads at startup, then only when a test reads or triggers)
// and waits until it has joined.
func epochLimiter(t *testing.T, f *epochFake) *Limiter {
	t.Helper()
	p, in := cleanSettings()
	return cleanLimiterHooks(t, p, in, reasonReady, hookSet{storeWrapper: f.wrap})
}

// allowedAt is an E_ALLOWED acquire reply at t (generation 1, sequence 1).
func allowedAt(t int64) store.Response { return allowedSeq(t, 1) }

// allowedSeq is an E_ALLOWED acquire reply at t with generation 1 and sequence s: the receipt
// it gives is told apart by s, which its refund carries (ARGV 6).
func allowedSeq(t, s int64) store.Response {
	return clOK(1, t, "", clInt(19_000_000_000), clInt(1), clInt(s), clInt(t+60_000_000))
}

// pages answers due_entries as a pass that finds one due key: the first page (cutoff 0) holds
// encK, the next one is empty.
func pages(req store.Request) store.Response {
	if req.Args[3] == "0" {
		return clPage(clT0, clT0-5, encK)
	}
	return clPage(clT0, 0)
}

// always answers every request with r.
func always(r store.Response) func(store.Request) store.Response {
	return func(store.Request) store.Response { return r }
}

// passFake sets f's cleanup answers: pages, and each step skipped as gone.
func passFake(f *epochFake) {
	f.due, f.step = pages, always(clStep(33, "gone"))
}

// runPass sets f's cleanup answers to due and step, runs a pass on l and fails the test
// unless it ends with status.
func runPass(t *testing.T, l *Limiter, f *epochFake, due, step func(store.Request) store.Response, status string) {
	t.Helper()
	f.set(func(f *epochFake) { f.due, f.step = due, step })
	if rep := l.RunCleanupPass(t.Context()); rep.Status != status {
		t.Fatalf("pass: %+v, want status %s", rep, status)
	}
}

// checkCmd fails the test unless req's KEYS are want and its ARGV hold each value of args at
// its position.
func checkCmd(t *testing.T, what string, req store.Request, want []string, args map[int]string) {
	t.Helper()
	if !slices.Equal(req.Keys, want) {
		t.Errorf("%s: KEYS %q, want %q", what, req.Keys, want)
	}
	for i, v := range args {
		if i >= len(req.Args) || req.Args[i] != v {
			t.Errorf("%s: ARGV %q, want %q at position %d", what, req.Args, v, i)
		}
	}
}

// checkPass checks the commands of the latest cleanup pass (two due_entries pages and one
// cleanup_entry step) against epoch id and its floors (§A4.1: due_entries KEYS epoch, ctl, ix;
// ARGV epoch_id, fp, limit, cutoff, floor_hwm, floor_gen; cleanup_entry KEYS epoch, ctl, ix, q,
// k:<member>, mi:<member>; ARGV epoch_id, fp, member, cap_units, rate_tokens, push_us,
// floor_hwm, floor_gen).
func checkPass(t *testing.T, what string, f *epochFake, fp, id string, hwm, gen int64) {
	t.Helper()
	due, steps := f.requests("due_entries"), f.requests("cleanup_entry")
	if len(due) < 2 || len(steps) < 1 {
		t.Fatalf("%s: %d due_entries and %d cleanup_entry requests, want at least 2 and 1", what, len(due), len(steps))
	}
	for _, d := range due[len(due)-2:] {
		checkCmd(t, what+": due_entries", d, []string{keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(id)},
			map[int]string{0: id, 1: fp, 4: num(hwm), 5: num(gen)})
	}
	checkCmd(t, what+": cleanup_entry", steps[len(steps)-1], []string{
		keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(id), keys.QuarantineKey(id),
		keys.BucketKey(id, encK), keys.MarkerIndexKey(id, encK),
	}, map[int]string{0: id, 1: fp, 2: encK, 6: num(hwm), 7: num(gen)})
}

// num formats an integer as a request argument holds it.
func num(v int64) string { return strconv.FormatInt(v, 10) }

// TestEpochSwitch_ResetsFloors (plan S8, L1; DESIGN §A4.1, §A5.7; SPEC ADM-1, CFG-6): once the
// watch joins a replacement epoch B, whose record starts with gen 0 and an hwm below A's, every
// later command is built under B with B's floors, never A's (which would hold B at once). A
// refund of a receipt from A has B's KEYS and floors and A's epoch_id only as the receipt's
// (ARGV 4), whenever its job started: released after the switch (receipt 1); in flight across
// it (receipt 2: its first attempt, built under A, is answered 20 once the watch has joined B,
// and its retry is built under B); or waiting in backoff across it (receipt 3: its first
// attempt, under A, is answered clock_error, and its retry is built under B). The next cleanup
// pass's due_entries and cleanup_entry carry B's namespace and floors, and its reclaimer takes
// A's keys as candidates and leaves B's; an acquire carries B's too. Later reads in B raise its
// floors, and the next commands carry the raised ones.
func TestEpochSwitch_ResetsFloors(t *testing.T) {
	p, in := cleanSettings()
	fp := fingerprint.Of(p)
	aKey, bKey := keys.BucketKey(clEpoch, encK), keys.BucketKey(clOther, encK)
	f := newEpochFake(clEpochResp(clEpoch, fp, "", s8HwmA, s8GenA))
	var seq atomic.Int64 // each allowed acquire takes the next sequence, which names its receipt
	f.set(func(f *epochFake) {
		f.acquire = func(store.Request) store.Response { return allowedSeq(clT0, seq.Add(1)) }
		f.scanned = []string{aKey}
		passFake(f)
	})
	switched := make(chan struct{}) // closed once the watch has joined B
	gate := &gateSigner{ctx: t.Context(), s: 3, open: switched}
	l := cleanLimiterHooks(t, p, in, reasonReady, hookSet{storeWrapper: f.wrap, signerWrapper: gate.wrap})
	ctx := t.Context()
	// The refund answers: receipt 2's first attempt switches the fake's record to B, lets the
	// watch join it, then answers 20; receipt 3's first attempt is answered clock_error (retried,
	// no early read); every other attempt is final receipt_invalid, as B's store answers a
	// receipt from A.
	f.set(func(f *epochFake) {
		f.refund = func(req store.Request) store.Response {
			switch { // the fake keeps each request before it answers: refundsOf counts this one
			case req.Args[5] == "2" && len(f.refundsOf(2)) == 1:
				f.set(func(f *epochFake) {
					f.epoch = clEpochResp(clOther, fp, "", s8HwmB, s8GenB)
					f.scanned = []string{aKey, bKey}
				})
				l.watch.read(ctx)
				close(switched)
				return clOK(20, 0, "epoch_or_fingerprint")
			case req.Args[5] == "3" && len(f.refundsOf(3)) == 1:
				return clOK(21, 0, "clock")
			}
			return clOK(10, clT0+1, "epoch")
		}
	})
	// refundKeys and refundArgs are a refund's KEYS and checked ARGV under the epoch id with
	// its floors, for a receipt from A (§A4.1: ARGV epoch_id, fp, enc, the receipt's epoch_id,
	// …, floor_hwm, floor_gen).
	refundKeys := func(id string) []string {
		return []string{
			keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(id), keys.QuarantineKey(id),
			keys.BucketKey(id, encK), keys.MarkerIndexKey(id, encK),
		}
	}
	refundArgs := func(id string, hwm, gen int64) map[int]string {
		return map[int]string{0: id, 1: fp, 2: encK, 3: clEpoch, 12: num(hwm), 13: num(gen)}
	}

	// In A: three allowed acquires (receipts 1, 2 and 3 name A) and a pass under A's floors,
	// which reclaims nothing.
	var permits [3]*Permit
	for i := range permits {
		res := l.Acquire(ctx, "k")
		if res.Kind != Allowed {
			t.Fatalf("acquire %d in A: %v (%+v), want allowed", i+1, res.Kind, res.Diag)
		}
		permits[i] = res.Permit
	}
	checkCmd(t, "acquire in A", f.requests("acquire")[0], []string{
		keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(clEpoch), keys.QuarantineKey(clEpoch), aKey, keys.MarkerIndexKey(clEpoch, encK),
	}, map[int]string{0: clEpoch, 8: num(s8HwmA), 9: num(s8GenA)})
	if rep := l.RunCleanupPass(ctx); rep != (CleanupReport{Status: "complete", Skipped: 1}) {
		t.Fatalf("pass in A: %+v", rep)
	}
	checkPass(t, "pass in A", f, fp, clEpoch, s8HwmA, s8GenA)
	if n := len(f.requests("reclaim")); n != 0 {
		t.Fatalf("pass in A: %d reclaim calls for A's own key, want 0", n)
	}

	// Receipt 3's refund goes into backoff before the switch: its first attempt was answered and
	// its retry is armed (or, if the backoff already ended, held at its start by the gate).
	permits[2].Release()
	pollUntil(t, "receipt 3's refund in backoff", func() bool {
		l.refunds.mu.Lock()
		defer l.refunds.mu.Unlock()
		return permits[2].retries > 0
	})

	// The switch, while receipt 2's first attempt is in flight: the watch reads B and joins it
	// with B's floors.
	permits[1].Release()
	pollUntil(t, "receipt 2's retry", func() bool { return len(f.refundsOf(2)) == 2 })
	if h := l.Health(); h.Reason != reasonReady || h.EpochID != clOther {
		t.Fatalf("after the switch: health %q in epoch %q, want ready in %s", h.Reason, h.EpochID, clOther)
	}
	pollUntil(t, "receipt 3's retry", func() bool { return len(f.refundsOf(3)) == 2 })
	for _, s := range []int64{2, 3} {
		rs := f.refundsOf(s)
		checkCmd(t, "receipt "+num(s)+"'s first attempt, before the switch", rs[0], refundKeys(clEpoch), refundArgs(clEpoch, s8HwmA, s8GenA))
		checkCmd(t, "receipt "+num(s)+"'s retry, after the switch", rs[1], refundKeys(clOther), refundArgs(clOther, s8HwmB, s8GenB))
	}

	// The refund of receipt 1, released after the switch: B's KEYS and floors, A's epoch_id
	// only as the receipt's.
	permits[0].Release()
	pollUntil(t, "receipt 1's refund attempt", func() bool { return len(f.refundsOf(1)) == 1 })
	checkCmd(t, "refund after the switch", f.refundsOf(1)[0], refundKeys(clOther), refundArgs(clOther, s8HwmB, s8GenB))
	// Each job ends on its final receipt_invalid, whose count is the last effect of its end:
	// receipt 1 after one attempt, 2 and 3 after two.
	pollUntil(t, "three receipt_invalid results", func() bool {
		return gaugeValue(t, l.reg.Snapshot(), "rl_refund_results_total", "receipt_invalid") == 3
	})
	for i, want := range []int64{1, 2, 2} {
		if st, n := permits[i].life.load(), permits[i].attempts.Load(); st != stateFinal || n != want {
			t.Errorf("receipt %d's refund: state %v after %d attempts, want FINAL after %d", i+1, st, n, want)
		}
	}

	// The next pass: B's namespace and floors; the reclaimer sends A's key, not B's.
	if rep := l.RunCleanupPass(ctx); rep != (CleanupReport{Status: "complete", Skipped: 1}) {
		t.Fatalf("pass in B: %+v", rep)
	}
	checkPass(t, "pass in B", f, fp, clOther, s8HwmB, s8GenB)
	rec := f.requests("reclaim")
	if len(rec) != 1 || !slices.Equal(rec[0].Keys, []string{keys.EpochKey, aKey}) {
		t.Fatalf("pass in B: reclaim calls %v, want one with rl:epoch and A's key", rec)
	}

	// Later reads in B raise the floors; the next commands carry the raised ones.
	f.set(func(f *epochFake) { f.epoch = clEpochResp(clOther, fp, "", s8HwmB2, s8GenB2) })
	l.watch.read(ctx)
	if res := l.Acquire(ctx, "k"); res.Kind != Allowed {
		t.Fatalf("acquire in B: %v, want allowed", res.Kind)
	} else {
		res.Permit.Done()
	}
	acq := f.requests("acquire")
	checkCmd(t, "acquire in B", acq[len(acq)-1], []string{
		keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(clOther), keys.QuarantineKey(clOther), bKey, keys.MarkerIndexKey(clOther, encK),
	}, map[int]string{0: clOther, 8: num(s8HwmB2), 9: num(s8GenB2)})
	if rep := l.RunCleanupPass(ctx); rep.Status != "complete" {
		t.Fatalf("second pass in B: %+v", rep)
	}
	checkPass(t, "second pass in B", f, fp, clOther, s8HwmB2, s8GenB2)
}

// TestEpochWatch_EarlyRereadFromRefundAndCleanup (plan S8, L1; DESIGN §A5.7): an epoch-wide
// config_error (20) or state_integrity_error (22) reply to a refund attempt or to a cleanup
// pass (from due_entries or from cleanup_entry) asks the watch for an early read, which it takes
// at most once per interval (1 h here): the first such reply, each code from each source in
// turn, reads once, and every later one within the interval — the other source's, and the other
// code's — adds none. Per-key (23), clock (21), capacity (24), bad-call (25) and unknown-outcome
// replies, and passes that stop otherwise or complete, ask for none. With a 1 s interval, a
// reply an interval after the last early read reads early again, while one within it does not.
func TestEpochWatch_EarlyRereadFromRefundAndCleanup(t *testing.T) {
	p, _ := cleanSettings()
	fp := fingerprint.Of(p)
	joinedA := clEpochResp(clEpoch, fp, "", s8HwmA, s8GenA)
	// refunds answers a released permit's attempts with rs in turn, then with a final
	// receipt_invalid, and returns how many attempts came.
	refunds := func(f *epochFake, rs ...store.Response) *atomic.Int32 {
		var n atomic.Int32
		f.set(func(f *epochFake) {
			f.acquire = func(store.Request) store.Response { return allowedAt(clT0) }
			f.refund = func(store.Request) store.Response {
				if i := int(n.Add(1)); i <= len(rs) {
					return rs[i-1]
				}
				return clOK(10, clT0+1, "epoch")
			}
		})
		return &n
	}
	release := func(t *testing.T, l *Limiter, n *atomic.Int32, want int32) {
		t.Helper()
		res := l.Acquire(t.Context(), "k")
		if res.Kind != Allowed {
			t.Fatalf("acquire: %v, want allowed", res.Kind)
		}
		res.Permit.Release()
		pollUntil(t, "the refund's final attempt", func() bool { return n.Load() >= want })
	}
	quiet := func(t *testing.T, f *epochFake, want int, what string) {
		t.Helper()
		time.Sleep(s8Quiet)
		if n := f.readCount(); n != want {
			t.Fatalf("%s: %d epoch reads, want %d", what, n, want)
		}
	}
	none := always(store.Response{Outcome: store.Unavailable}) // a step that never comes

	t.Run("a refund's 20 first", func(t *testing.T) {
		f := newEpochFake(joinedA)
		l := epochLimiter(t, f)
		reads := f.readCount()
		n := refunds(f, clOK(20, 0, "epoch_or_fingerprint"), clOK(22, 0, "integrity_hold"))
		release(t, l, n, 3)
		pollUntil(t, "an early read after the refund's code 20", func() bool { return f.readCount() == reads+1 })
		runPass(t, l, f, always(clOK(22, 0, "hwm_missing")), none, "state_integrity_error")
		runPass(t, l, f, pages, always(clOK(20, 0, "epoch_or_fingerprint")), "config_error")
		quiet(t, f, reads+1, "a refund's 22 and a pass's 22 and 20 within the interval")
	})
	t.Run("a refund's 22 first", func(t *testing.T) {
		f := newEpochFake(joinedA)
		l := epochLimiter(t, f)
		reads := f.readCount()
		n := refunds(f, clOK(22, 0, "hwm_missing"), clOK(20, 0, "epoch_or_fingerprint"))
		release(t, l, n, 3)
		pollUntil(t, "an early read after the refund's code 22", func() bool { return f.readCount() == reads+1 })
		quiet(t, f, reads+1, "a refund's 20 within the interval")
	})
	t.Run("a cleanup_entry 22 first", func(t *testing.T) {
		f := newEpochFake(joinedA)
		l := epochLimiter(t, f)
		reads := f.readCount()
		runPass(t, l, f, pages, always(clOK(22, 0, "integrity_hold")), "state_integrity_error")
		pollUntil(t, "an early read after the pass's code 22", func() bool { return f.readCount() == reads+1 })
		runPass(t, l, f, always(clOK(20, 0, "epoch_or_fingerprint")), none, "config_error")
		n := refunds(f, clOK(22, 0, "hwm_malformed"), clOK(20, 0, "epoch_or_fingerprint"))
		release(t, l, n, 3)
		quiet(t, f, reads+1, "a pass's 20 and a refund's 22 and 20 within the interval")
	})
	t.Run("a due_entries 20 first", func(t *testing.T) {
		f := newEpochFake(joinedA)
		l := epochLimiter(t, f)
		reads := f.readCount()
		runPass(t, l, f, always(clOK(20, 0, "epoch_or_fingerprint")), none, "config_error")
		pollUntil(t, "an early read after the pass's code 20", func() bool { return f.readCount() == reads+1 })
		runPass(t, l, f, pages, always(clOK(22, 0, "integrity_hold")), "state_integrity_error")
		quiet(t, f, reads+1, "a pass's 22 within the interval")
	})
	t.Run("other replies ask for none", func(t *testing.T) {
		f := newEpochFake(joinedA)
		l := epochLimiter(t, f)
		reads := f.readCount()
		n := refunds(f, clOK(23, clT0, "quarantined"), clOK(21, 0, "clock"), clOK(24, clT0, "key_markers"),
			clOK(25, 0, "keys_mismatch"), store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1})
		release(t, l, n, 6)
		runPass(t, l, f, always(clOK(21, 0, "clock")), none, "clock_error")
		runPass(t, l, f, always(store.Response{Outcome: store.Unavailable}), none, "unavailable")
		runPass(t, l, f, pages, always(clOK(25, 0, "keys_mismatch")), "outcome_unknown")
		runPass(t, l, f, pages, always(clStep(32, "malformed_bucket")), "complete")
		quiet(t, f, reads, "per-key, clock, capacity and bad-call replies")
		// The limiter does read early: a pass's 22 now reads once.
		runPass(t, l, f, always(clOK(22, 0, "integrity_hold")), none, "state_integrity_error")
		pollUntil(t, "an early read after the pass's code 22", func() bool { return f.readCount() == reads+1 })
	})
	// Once an interval has passed since the last early read, the next trigger reads early
	// again: at most once per interval, not once per instance. The watch starts its ticker after
	// the startup read r0, so its k-th timed read comes no earlier than r0 + k intervals, and a
	// read the fake records before that bound is an early one (no fake clock: real time, with
	// an interval long enough that only a stall of nearly one interval could delay an early
	// read past the next tick).
	t.Run("again an interval after the last early read", func(t *testing.T) {
		const interval = time.Second
		p, in := cleanSettings()
		in.EpochRereadIntervalNs = interval.Nanoseconds()
		f := newEpochFake(joinedA)
		l := cleanLimiterHooks(t, p, in, reasonReady, hookSet{storeWrapper: f.wrap})
		r0 := f.readTime(t, 0, "the startup read")
		tick := func(k int) time.Time { return r0.Add(time.Duration(k) * interval) }
		// Right after the first timed read, a pass's 22 reads early, before the second tick.
		f.readTime(t, 1, "the first timed read")
		runPass(t, l, f, always(clOK(22, 0, "hwm_missing")), none, "state_integrity_error")
		early := f.readTime(t, 2, "an early read after the pass's code 22")
		if !early.Before(tick(2)) {
			t.Fatalf("the read after the pass's code 22 came at r0 + %v, not before the second tick at r0 + %v",
				early.Sub(r0), 2*interval)
		}
		// A pass's 20 at once, within the interval, adds none: the next read is the second tick's.
		runPass(t, l, f, always(clOK(20, 0, "epoch_or_fingerprint")), none, "config_error")
		if r := f.readTime(t, 3, "the second timed read"); r.Before(tick(2)) {
			t.Fatalf("a pass's code 20 within the interval read early, at r0 + %v", r.Sub(r0))
		}
		// An interval after the early read (and after the second tick), a pass's 20 reads early
		// again, before the third tick. The watch took that early read at or before the time the
		// fake recorded it, so the interval has passed when the trigger comes.
		time.Sleep(time.Until(early.Add(interval)))
		runPass(t, l, f, always(clOK(20, 0, "epoch_or_fingerprint")), none, "config_error")
		if r := f.readTime(t, 4, "an early read an interval after the first"); !r.Before(tick(3)) {
			t.Fatalf("the read after a pass's code 20 an interval after the last early read came at r0 + %v, not before the third tick at r0 + %v",
				r.Sub(r0), 3*interval)
		}
	})
}

// TestIntegrityScope (plan S8, L1; DESIGN §A5.11, §A4.9; SPEC STA-9): a state_integrity_error
// carries its scope: on acquire, code 22 gives Diag.Scope epoch and code 23 key, Diag.Reason
// the reply's reason; on a cleanup pass, a 22 from due_entries or cleanup_entry stops the pass
// with scope epoch and the reply's reason, while a per-key result (code 32) is counted and
// stops nothing. A refund's results reach no Diag (§A5.2: RefundEvent has none; plan S8
// conflict in the slice report): there the scope decides the early epoch read, so 22 is
// epoch-wide and 23 per key, both retried, never final.
func TestIntegrityScope(t *testing.T) {
	p, _ := cleanSettings()
	fp := fingerprint.Of(p)
	f := newEpochFake(clEpochResp(clEpoch, fp, "", s8HwmA, s8GenA))
	l := epochLimiter(t, f)
	ctx := t.Context()
	t.Run("acquire", func(t *testing.T) {
		for _, c := range []struct {
			reply store.Response
			want  Diag
		}{
			{clOK(22, 0, "integrity_hold"), Diag{Scope: scopeEpoch, Reason: "integrity_hold"}},
			{clOK(22, 0, "hwm_malformed"), Diag{Scope: scopeEpoch, Reason: "hwm_malformed"}},
			{clOK(23, clT0, "quarantined"), Diag{Scope: scopeKey, Reason: "quarantined"}},
			{clOK(23, clT0, "marker_index_mismatch"), Diag{Scope: scopeKey, Reason: "marker_index_mismatch"}},
		} {
			f.set(func(f *epochFake) { f.acquire = func(store.Request) store.Response { return c.reply } })
			if r := l.Acquire(ctx, "k"); r.Kind != StateIntegrityError || r.Diag != c.want || r.Permit != nil {
				t.Errorf("acquire answered %d %q: %v %+v, want state_integrity_error %+v and no permit",
					c.reply.Reply.Code, c.reply.Reply.Reason, r.Kind, r.Diag, c.want)
			}
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		for _, c := range []struct {
			name      string
			due, step func(store.Request) store.Response
			want      CleanupReport
		}{
			{"due_entries 22", always(clOK(22, 0, "hwm_missing")), always(clStep(33, "gone")),
				CleanupReport{Status: "state_integrity_error", Diag: Diag{Scope: scopeEpoch, Reason: "hwm_missing"}}},
			{"cleanup_entry 22", pages, always(clOK(22, 0, "integrity_hold")),
				CleanupReport{Status: "state_integrity_error", Diag: Diag{Scope: scopeEpoch, Reason: "integrity_hold"}}},
			{"cleanup_entry 32, per key", pages, always(clStep(32, "index_without_bucket")),
				CleanupReport{Status: "complete", Quarantined: 1}},
		} {
			f.set(func(f *epochFake) { f.due, f.step = c.due, c.step })
			if rep := l.RunCleanupPass(ctx); rep != c.want {
				t.Errorf("%s: report %+v, want %+v", c.name, rep, c.want)
			}
		}
	})
	t.Run("refund", func(t *testing.T) {
		const expiry = clT0 + 60_000_000
		for _, c := range []struct {
			reply     store.Response
			epochWide bool
		}{
			{clOK(22, 0, "integrity_hold"), true},
			{clOK(22, 0, "epoch_record_missing"), true},
			{clOK(23, clT0, "refund_bucket_missing"), false},
			{clOK(23, clT0, "quarantined"), false},
		} {
			got := classifyRefund(c.reply, p.RatePeriodNs, expiry)
			if got.final || got.epochWide != c.epochWide || got.sent != 1 {
				t.Errorf("refund answered %d %q: %+v, want retried (not final) with epochWide %v",
					c.reply.Reply.Code, c.reply.Reply.Reason, got, c.epochWide)
			}
		}
	})
}
