package core

import (
	"context"
	"strconv"
	"time"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
)

// tBound is TBOUND = 2^53 − 2^43 (§A3.1): every budget time t_us lies below it.
const tBound = 1<<53 - 1<<43

// Diag reasons of the local signer checks (plan choice).
const (
	reasonSignerUnusable = "signer_unusable"
	reasonSigningFailed  = "signing_failed"
)

// Acquire takes one token from key's budget (DESIGN §A5.3; SPEC API-1, API-1a). ctx's
// deadline is the host deadline and its cancellation the host's cancel; the adapter ends
// each call at min(ctx deadline, start + D). The result is counted in
// rl_acquire_results_total.
func (l *Limiter) Acquire(ctx context.Context, key string) Result {
	r := l.acquire(ctx, key)
	l.reg.Inc(metrics.AcquireResults, int(r.Kind))
	return r
}

// acquire runs the local checks in §A5.3's order (key, ctx, epoch-watch state, signer,
// slots), each refusing before anything is sent, then sends one acquire. Both slots are
// freed on every return but allowed.
func (l *Limiter) acquire(ctx context.Context, key string) Result {
	enc, err := keys.Encode(key)
	if err != nil {
		return Result{Kind: ValidationError}
	}
	if ctx.Err() != nil {
		return Result{Kind: Cancelled}
	}
	rd := l.readiness()
	if !rd.ready {
		return Result{Kind: rd.kind, Diag: rd.diag}
	}
	if !l.signer.Usable() { // config_error with no charge (API-1a)
		return Result{Kind: ConfigError, Diag: Diag{Reason: reasonSignerUnusable}}
	}
	if !l.slots.tryAcquire() {
		return Result{Kind: Busy}
	}
	r := l.send(ctx, enc, rd.st)
	if r.Kind != Allowed {
		l.slots.release()
	}
	return r
}

// send runs EVALSHA acquire once with the §A4.1 KEYS and ARGV under the held epoch and its
// floors, and maps the outcome (§A4.9, §A5.3). Nothing retries it: an outcome_unknown may
// already have deducted (API-12, API-24).
func (l *Limiter) send(ctx context.Context, enc string, st *epochState) Result {
	req := l.acquireRequest(enc, st)
	sentAt := time.Now() // the retention timer starts just before the send (§A5.5, REF-4a)
	resp := l.adapter.Call(ctx, req)
	if resp.StoreError != "" {
		l.setStoreError(resp.StoreError)
	}
	switch resp.Outcome {
	case store.OK:
		return l.decide(enc, st, sentAt, resp)
	case store.Unavailable:
		// Nothing was sent. When Close or Kill began meanwhile, the instance's own shutdown
		// closed the adapter under this acquire, not a store found unavailable: a closing
		// instance refuses in both modes (API-1e, API-8a, API-21). shutdown marks closing
		// before it ends the lifetime and closes the adapter, so this load sees it.
		if l.closing.Load() {
			return Result{Kind: NotReady, Diag: Diag{Reason: reasonClosing}}
		}
		// The one failure-mode branch (API-20a, API-20b, API-21).
		if l.p.FailureMode == Open {
			return Result{Kind: Degraded, Permit: newDegradedPermit(), Diag: Diag{StoreError: resp.StoreError}}
		}
		return Result{Kind: Unavailable, Diag: Diag{StoreError: resp.StoreError}}
	case store.Cancelled:
		return Result{Kind: Cancelled}
	case store.OutcomeUnknown:
		return unknown("", resp)
	case store.CapacityError:
		return Result{Kind: CapacityError, Diag: Diag{StoreError: resp.StoreError}}
	}
	return unknown("", resp) // no other outcome exists; fail closed
}

// acquireRequest is the acquire call of §A4.1: KEYS epoch, ctl, ix, q, k:<enc>, mi:<enc>;
// ARGV epoch_id, fp, enc, cap_units, rate_period_ns, rate_tokens, lifetime_us, max_tracked,
// floor_hwm, floor_gen.
func (l *Limiter) acquireRequest(enc string, st *epochState) store.Request {
	num := func(v int64) string { return strconv.FormatInt(v, 10) }
	return store.Request{
		Kind:   store.Call,
		Script: scripts.Acquire,
		Keys: []string{
			keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(st.id), keys.QuarantineKey(st.id),
			keys.BucketKey(st.id, enc), keys.MarkerIndexKey(st.id, enc),
		},
		Args: []string{
			st.id, l.fp, enc, num(l.capUnits), num(l.p.RatePeriodNs), num(l.p.RateTokens),
			num(l.lifetimeUs), num(l.p.MaxTrackedBuckets), num(st.floorHwm), num(st.floorGen),
		},
	}
}

// decide maps a decoded acquire reply to its result (§A4.9). An epoch-wide config_error or
// state_integrity_error asks the watch for an early read (§A5.7).
func (l *Limiter) decide(enc string, st *epochState, sentAt time.Time, resp store.Response) Result {
	rep := resp.Reply
	if rep.TUs < 0 || rep.TUs >= tBound { // the decoder checks it; fail closed all the same
		return unknown("", resp)
	}
	switch rep.Code {
	case 1: // E_ALLOWED
		return l.allowed(enc, st, sentAt, resp)
	case 2: // E_LIMITED
		u, ok := elemInt(rep.Elems, 3)
		if !ok || u < 0 || u >= l.p.RatePeriodNs {
			return unknown("", resp)
		}
		return Result{Kind: Limited, DecisionNs: DecisionNs(rep.TUs), RetryAfterNs: RetryAfterNs(u, l.p.RatePeriodNs, l.p.RateTokens)}
	case 20: // E_CONFIG
		l.watch.trigger()
		return Result{Kind: ConfigError, Diag: Diag{Reason: rep.Reason}}
	case 21: // E_CLOCK
		return Result{Kind: ClockError, Diag: Diag{Reason: rep.Reason}}
	case 22: // E_INTEGRITY_EPOCH
		l.watch.trigger()
		return Result{Kind: StateIntegrityError, Diag: Diag{Scope: scopeEpoch, Reason: rep.Reason}}
	case 23: // E_INTEGRITY_KEY
		return Result{Kind: StateIntegrityError, Diag: Diag{Scope: scopeKey, Reason: rep.Reason}}
	case 24: // E_CAPACITY
		return Result{Kind: CapacityError, Diag: Diag{Reason: rep.Reason}}
	case 25: // E_BAD_CALL: a library defect, reported in Diag (the library has no logger)
		return unknown(rep.Reason, resp)
	}
	return unknown("", resp) // the decoder admits no other acquire code
}

// allowed builds the receipt of a confirmed deduction (§A5.9, RCP-1, RCP-3) and its OPEN
// permit. A signing error is receipt_error: no permit, and the charge stays (API-12a).
func (l *Limiter) allowed(enc string, st *epochState, sentAt time.Time, resp store.Response) Result {
	e := resp.Reply.Elems
	g, okG := elemInt(e, 4)
	s, okS := elemInt(e, 5)
	if !okG || !okS || g < 1 || s < 1 { // the decoder checks both; fail closed all the same
		return unknown("", resp)
	}
	gu, su := uint64(g), uint64(s)
	rc, err := l.signer.Sign(receipt.New(st.idBytes, enc, gu, su, resp.Reply.TUs, l.lifetimeUs))
	if err != nil {
		return Result{Kind: ReceiptError, Diag: Diag{Reason: reasonSigningFailed}}
	}
	p := newPermit(l.slots, rc, receipt.DeductionID(rc), sentAt)
	return Result{Kind: Allowed, DecisionNs: DecisionNs(resp.Reply.TUs), Permit: p}
}

// unknown is an outcome_unknown result: Diag carries reason, and the error reply's text or
// else the raw reply that failed its checks (§A5.4).
func unknown(reason string, resp store.Response) Result {
	raw := resp.StoreError
	if raw == "" {
		raw = resp.Raw
	}
	return Result{Kind: OutcomeUnknown, Diag: Diag{Reason: reason, StoreError: raw}}
}
