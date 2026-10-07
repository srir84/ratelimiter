//go:build rltest

package driver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"ratelimiter/internal/receipt"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/limiter"
)

// The driver's refund support (IMPLEMENTATION_PLAN S6 steps 10 and 13; HARNESS §9.4): the
// fast test profile, a refund listener on every instance that records each final event and
// writes the op log's refund record, the per-attempt refund capture, and the permit finishes
// (Release, Done) that write the op log's release and done records.

// FastPolicy returns the S6 test policy (plan choice, S6 step 10): DefaultPolicy with a 6 s
// receipt lifetime, a 3 s refund deadline, a 0.5 s outage tolerance and a 3,600 s cleanup
// interval (max_tracked_buckets 100,000 and max_live_markers 50,000 as DefaultPolicy). Its
// instance settings are FastInstance's.
func FastPolicy() limiter.Policy {
	p := DefaultPolicy()
	p.ReceiptLifetimeNs = 6_000_000_000
	p.RefundDeadlineNs = 3_000_000_000
	p.OutageToleranceNs = 500_000_000
	p.CleanupIntervalNs = 3_600_000_000_000
	return p
}

// FastInstance returns the instance settings that go with FastPolicy (plan choice, S6 step
// 10): max release delay 0.5 s, margin 0.1 s, D 200 ms, refund backoff 20 ms → 200 ms, and
// N = max_held_receipts = refund workers = 128, the drain timeout refund_deadline +
// outage_tolerance (DefaultInstance). The startup checks hold: §A5.6 0.2 + 1 + (⌈128 / 128⌉
// + 3) × 0.2 = 2.0 s ≤ 3 s; RCP-8 0.5 + 3 + 0.5 + 0.1 = 4.1 s ≤ 6 s and 0.1 s ≥ 6 s × 0.001.
func FastInstance(p limiter.Policy) limiter.Instance {
	in := limiter.DefaultInstance(p)
	in.MaxReleaseDelayNs, in.MarginNs = 500_000_000, 100_000_000
	in.DependencyDeadlineNs = 200_000_000
	in.RefundBackoffInitialNs, in.RefundBackoffMaxNs = 20_000_000, 200_000_000
	in.MaxHeldReceipts, in.MaxPendingRefunds, in.RefundWorkers = 128, 128, 128
	return in
}

// RefundAttempt is one refund call an instance made (the per-attempt refund capture, HARNESS
// §9.4): what the limiter got back, after the store wrapper's faults, and what the store
// adapter returned before them.
type RefundAttempt struct {
	Instance    int
	DeductionID string // the receipt's, from the call's ARGV (§A4.1, §A5.9)
	// Response is what the limiter got: a refused call is unavailable, a dropped reply
	// outcome_unknown.
	Response store.Response
	// Store is what the store adapter returned before any fault, such as the reply a
	// drop_reply fault discarded; Reached is false when the call never got there (refused).
	Store   store.Response
	Reached bool
}

// Code is the attempt's code in the op log's refund record (plan choice): the §A4.9 code of a
// store reply the limiter got (3, 4, 10, 11, 20–25), else the outcome's name (unavailable,
// cancelled, outcome_unknown, capacity_error for an OOM refusal).
func (a RefundAttempt) Code() any {
	switch a.Response.Outcome {
	case store.OK:
		return a.Response.Reply.Code
	case store.Unavailable:
		return "unavailable"
	case store.Cancelled:
		return "cancelled"
	case store.CapacityError:
		return "capacity_error"
	case store.OutcomeUnknown:
		return "outcome_unknown"
	}
	return "outcome_unknown"
}

// record is the attempt's entry in the op log's refund record.
func (a RefundAttempt) record() attemptRecord {
	return attemptRecord{Code: a.Code(), TUs: a.TUs(), Credited: a.Credited()}
}

// lateRecord is the entry of a refund call whose outcome the limiter did not act on: it had
// already finalized the permit when the call returned (the retention timer fired while it was
// in flight; orchestrator decision, S6 task 2), or it counted the call and then lost the permit
// to the retention timer before ending it with the call's final reply (orchestrator decision,
// S6 review round 1). A call that sent a command, as every call with a store reply did, is
// {"outcome_unknown", 0, 0}, so check_bud11 counts the credit it may have made; one that sent
// nothing keeps its own code (unavailable, cancelled): it changed nothing. Either way the
// record still ends with a code that is not final, as its result, unresolved, needs.
func (a RefundAttempt) lateRecord() attemptRecord {
	if a.Response.SentCommands > 0 {
		return attemptRecord{Code: "outcome_unknown"}
	}
	return a.record()
}

// final reports whether the limiter got a final store reply: §A4.9 code 3, 4, 10 or 11
// (applied, already_applied, receipt_invalid, receipt_expired), which the op log's grammar
// allows only as the last attempt of a record with that result.
func (a RefundAttempt) final() bool {
	if a.Response.Outcome != store.OK {
		return false
	}
	switch a.Response.Reply.Code {
	case 3, 4, 10, 11:
		return true
	}
	return false
}

// TUs is the budget time of the reply the limiter got, in µs; 0 without one (and for the
// codes whose t is 0: 20, 21, 22, 25).
func (a RefundAttempt) TUs() int64 {
	if a.Response.Outcome != store.OK {
		return 0
	}
	return a.Response.Reply.TUs
}

// Credited is the 4th element of an E_APPLIED (credited units) or E_ALREADY_APPLIED
// (credit_us) reply the limiter got; 0 for every other attempt.
func (a RefundAttempt) Credited() int64 {
	if a.Response.Outcome != store.OK || (a.Response.Reply.Code != 3 && a.Response.Reply.Code != 4) {
		return 0
	}
	return elemInt(a.Response.Reply, 3)
}

// refundKey is the context key of a refund call's capture.
type refundKey struct{}

// refundCall holds the store adapter's response to the refund call made under its context.
type refundCall struct {
	mu   sync.Mutex
	r    store.Response
	seen bool
}

func (c *refundCall) set(r store.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.r, c.seen = r, true
}

func (c *refundCall) get() (store.Response, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.r, c.seen
}

// inner sits between an instance's store wrapper and its store adapter: it hands the
// adapter's response to a refund call, before any fault acts on it, to the capture in the
// call's context (the wrapper passes the context on), and each due_entries page and cleanup
// step result to the driver's op log (cleanup.go).
type inner struct {
	store.Adapter
	d    *Driver
	inst *instance
}

func (a inner) Call(ctx context.Context, req store.Request) store.Response {
	res := a.Adapter.Call(ctx, req)
	if c, ok := ctx.Value(refundKey{}).(*refundCall); ok {
		c.set(res)
	}
	if a.d != nil && req.Script.Name == scripts.DueEntries.Name {
		a.d.logScan(res)
	}
	return res
}

// deductionOf is the deduction_id of a refund call's receipt (§A5.9), from its ARGV (§A4.1:
// enc, receipt epoch_id, g, s at 3 … 6); "" when they do not parse.
func deductionOf(req store.Request) string {
	if len(req.Args) < 6 {
		return ""
	}
	id, err := receipt.ParseEpochID(req.Args[3])
	if err != nil {
		return ""
	}
	g, errG := strconv.ParseUint(req.Args[4], 10, 64)
	s, errS := strconv.ParseUint(req.Args[5], 10, 64)
	if errG != nil || errS != nil {
		return ""
	}
	return receipt.DeductionID(receipt.Receipt{EpochID: id, Enc: req.Args[2], G: g, S: s})
}

// refund runs one refund call through the store wrapper and records the attempt. A call that
// returns after its permit's first final event reached the driver goes into that event's
// refund record too (lateLocked).
func (r recorder) refund(ctx context.Context, req store.Request) store.Response {
	c := &refundCall{}
	res := r.Adapter.Call(context.WithValue(ctx, refundKey{}, c), req)
	a := RefundAttempt{Instance: r.inst.index, DeductionID: deductionOf(req), Response: res}
	a.Store, a.Reached = c.get()
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	r.d.attempts[a.DeductionID] = append(r.d.attempts[a.DeductionID], a)
	if rec := r.d.permits[a.DeductionID]; rec != nil && rec.refund != nil {
		r.d.lateLocked(rec, a)
	}
	return res
}

// Finishes of a permit through the driver (permitRec.finish): the first one decides, as the
// permit's own first-finish rule does (API-1b, API-1c) and as check_bud11 reads the op log.
const (
	finishRelease = "release"
	finishDone    = "done"
)

// permitRec is what the driver knows of a permit it handed out: from its acquire record, for
// the release and refund records, and its finishes and events.
type permitRec struct {
	inst      *instance
	instance  string // the acquire record's instance field
	keyB64URL string
	g, s      int64
	expiryUs  int64
	// finish is the first finish the op log holds for it: "" (none), finishRelease (Release,
	// or a final event for a permit released outside the driver) or finishDone (Done). A
	// later finish changes nothing: the permit ignores it.
	finish string
	events int // final events the driver's listener received for it
	event  limiter.RefundEvent
	final  chan struct{} // closed at the first event
	// refund is the refund record of the first event, once written, and refundAt its index
	// in the op log, where a late call's entry is added (lateLocked).
	refund   *refundRecord
	refundAt int
}

// released reports whether its first finish was a release.
func (rec *permitRec) released() bool { return rec.finish == finishRelease }

// trackLocked records the permit of an allowed acquire record; d.mu is held.
func (d *Driver) trackLocked(in *instance, rec acquireRecord) {
	if rec.Result != limiter.Allowed.String() || rec.DeductionID == "" || in == nil {
		return
	}
	d.permits[rec.DeductionID] = &permitRec{
		inst: in, instance: rec.Instance, keyB64URL: rec.KeyB64URL,
		g: rec.G, s: rec.S, expiryUs: rec.ExpiryUs, final: make(chan struct{}),
	}
}

// listen is the refund listener the driver gives instance in: it records each final event
// (RefundEvents, the teardown's refund-sends check), writes the op log's refund record, and
// then calls the test's own listener (InstanceSpec.Listener), if any.
func (d *Driver) listen(in *instance) func(limiter.RefundEvent) {
	return func(ev limiter.RefundEvent) {
		d.noteEvent(in, ev)
		if f := in.spec.Listener; f != nil {
			f(ev)
		}
	}
}

// noteEvent records ev, a final event of instance in, and writes its refund record when the
// permit is one the driver handed out: every event gets one, so a second final event for one
// permit shows in the op log (check_bud11 rejects it). A permit with no finish logged was
// released outside the driver (its own Release): the event proves the release, so its release
// record is written first, as Release would have.
func (d *Driver) noteEvent(in *instance, ev limiter.RefundEvent) {
	in.mu.Lock()
	in.events = append(in.events, ev)
	in.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	rec := d.permits[ev.DeductionID]
	if rec == nil {
		return
	}
	if rec.finish == "" {
		rec.finish = finishRelease
		d.logged++
		d.appendLocked(releaseRecord{Type: "release", Instance: rec.instance, DeductionID: ev.DeductionID})
	}
	r := &refundRecord{
		Type: "refund", DeductionID: ev.DeductionID, KeyB64URL: rec.keyB64URL,
		G: rec.g, S: rec.s, ExpiryUs: rec.expiryUs, Attempts: attemptRecords(d.attempts[ev.DeductionID], ev),
		Result: ev.Result.String(),
	}
	rec.events++
	if rec.events == 1 {
		rec.event = ev
		rec.refund, rec.refundAt = r, len(d.oplog)
		defer close(rec.final)
	}
	d.appendLocked(r)
}

// attemptRecords is the attempts list of ev's refund record: the permit's captured calls in
// order. For an unresolved event, a call whose sent commands take the running total past
// ev.Attempts is one the limiter had not counted when it finalized the permit: it was still in
// flight when the retention timer fired, and returned before the event reached the driver, so
// it gets its lateRecord (orchestrator decision, S6 task 2). So does a call with a final reply
// the event counted: the worker counts a call's commands before it ends the permit, and the
// retention timer won the permit in between (orchestrator decision, S6 review round 1). Any
// other event counts every call it follows (API-1d), and its calls keep their codes.
func attemptRecords(as []RefundAttempt, ev limiter.RefundEvent) []attemptRecord {
	out := make([]attemptRecord, 0, len(as))
	sent := 0
	for _, a := range as {
		sent += max(a.Response.SentCommands, 0)
		if ev.Result == limiter.Unresolved && (sent > ev.Attempts || a.final()) {
			out = append(out, a.lateRecord())
			continue
		}
		out = append(out, a.record())
	}
	return out
}

// lateLocked adds a, a refund call that returned after its permit's first final event reached
// the driver, to that event's refund record, which the op log keeps until teardown: the
// limiter had finalized the permit before it learned the call's outcome, so the entry is the
// call's lateRecord (orchestrator decision, S6 task 2). d.mu is held.
func (d *Driver) lateLocked(rec *permitRec, a RefundAttempt) {
	rec.refund.Attempts = append(rec.refund.Attempts, a.lateRecord())
	b, err := json.Marshal(rec.refund)
	if err != nil {
		d.t.Errorf("driver: op log record %T: %v", rec.refund, err)
		return
	}
	d.oplog[rec.refundAt] = b
}

// Release finishes op's permit with release() (API-1b), after writing the op log's release
// record, so the record comes before the refund record of the final event; a degraded permit
// has an empty deduction_id, which adds nothing (plan S6 step 12). After a Done of the permit
// it is no release (the first finish decides), though its record is still written: check_bud11
// applies the same rule. It does nothing for an op without a permit. Concurrent Release and
// Done of one permit may log in another order than the permit's first-finish rule decided, and
// so may a mix of these and the permit's own Release and Done, so tests do neither.
func (d *Driver) Release(op Op) {
	if op.Permit == nil {
		return
	}
	id := op.Permit.DeductionID()
	d.mu.Lock()
	instance := strconv.Itoa(op.Instance)
	if rec := d.permits[id]; rec != nil {
		instance = rec.instance
		if rec.finish == "" {
			rec.finish = finishRelease
		}
	}
	d.logged++
	d.appendLocked(releaseRecord{Type: "release", Instance: instance, DeductionID: id})
	d.mu.Unlock()
	op.Permit.Release()
}

// Done finishes op's permit with done() (API-1c), after writing the op log's done record. It
// does nothing for an op without a permit.
func (d *Driver) Done(op Op) {
	if op.Permit == nil {
		return
	}
	id := op.Permit.DeductionID()
	d.mu.Lock()
	if rec := d.permits[id]; rec != nil && rec.finish == "" {
		rec.finish = finishDone
	}
	d.logged++
	d.appendLocked(doneRecord{Type: "done", DeductionID: id})
	d.mu.Unlock()
	op.Permit.Done()
}

// WaitRefund waits up to within for the driver's listener to receive the first final event
// of op's permit, and returns it.
func (d *Driver) WaitRefund(op Op, within time.Duration) (limiter.RefundEvent, error) {
	if op.Permit == nil || op.Permit.DeductionID() == "" {
		return limiter.RefundEvent{}, fmt.Errorf("driver: WaitRefund: op %s of %q has no receipt", op.Kind, op.Key)
	}
	id := op.Permit.DeductionID()
	d.mu.Lock()
	rec := d.permits[id]
	d.mu.Unlock()
	if rec == nil {
		return limiter.RefundEvent{}, fmt.Errorf("driver: WaitRefund: permit %s was not acquired through the driver", id)
	}
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case <-rec.final:
	case <-timer.C:
		return limiter.RefundEvent{}, fmt.Errorf("driver: no final refund event for permit %s within %v", id, within)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return rec.event, nil
}

// RefundEvents returns the final refund events instance i's driver listener received, in
// order.
func (d *Driver) RefundEvents(i int) []limiter.RefundEvent {
	in := d.current(i)
	if in == nil {
		return nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	return slices.Clone(in.events)
}

// RefundAttempts returns the refund calls captured so far for op's permit, in order.
func (d *Driver) RefundAttempts(op Op) []RefundAttempt {
	if op.Permit == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.attempts[op.Permit.DeductionID()])
}

// keyB64URL is a key's op-log form: unpadded base64url of its bytes (plan S5 step 11).
func keyB64URL(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

// isRefund reports whether req is a refund call.
func isRefund(req store.Request) bool { return req.Script.Name == scripts.Refund.Name }
