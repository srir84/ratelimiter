package core

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/srir84/ratelimiter/internal/metrics"
	"github.com/srir84/ratelimiter/internal/store"
)

// passStatus is how a cleanup pass ended (SPEC API-1f, DESIGN §A5.8): complete, or the result
// that stopped it. The values follow the label order of rl_cleanup_passes_total.
type passStatus int

const (
	passComplete passStatus = iota
	passStateIntegrityError
	passConfigError
	passClockError
	passUnavailable
	passOutcomeUnknown
)

// String returns CleanupReport.Status: complete, or the SPEC name of the stopping result.
func (s passStatus) String() string {
	switch s {
	case passComplete:
		return "complete"
	case passStateIntegrityError:
		return StateIntegrityError.String()
	case passConfigError:
		return ConfigError.String()
	case passClockError:
		return ClockError.String()
	case passUnavailable:
		return Unavailable.String()
	case passOutcomeUnknown:
		return OutcomeUnknown.String()
	}
	return OutcomeUnknown.String() // no other status exists; fail closed
}

// labels returns the status's rl_cleanup_passes_total label and, for a stop, its
// rl_cleanup_stops_total label (§A5.11).
func (s passStatus) labels() (pass, stop int, stopped bool) {
	switch s {
	case passComplete:
		return metrics.PassComplete, 0, false
	case passStateIntegrityError:
		return metrics.PassStateIntegrityError, metrics.StopStateIntegrityError, true
	case passConfigError:
		return metrics.PassConfigError, metrics.StopConfigError, true
	case passClockError:
		return metrics.PassClockError, metrics.StopClockError, true
	case passUnavailable:
		return metrics.PassUnavailable, metrics.StopUnavailable, true
	case passOutcomeUnknown:
		return metrics.PassOutcomeUnknown, metrics.StopOutcomeUnknown, true
	}
	return metrics.PassOutcomeUnknown, metrics.StopOutcomeUnknown, true
}

// cleanupPeriod is the wait before the next timed pass, cleanup_interval × U[0.9, 1.0]
// (DESIGN §A5.1) for u in [0, 1): interval − ⌊interval / 10⌋ plus u of that tenth, so never
// below 0.9 × interval nor above interval.
func cleanupPeriod(interval time.Duration, u float64) time.Duration {
	span := interval / 10
	d := time.Duration(float64(span) * u)
	if d > span || d < 0 { // only a u outside [0, 1) gets here; keep the bounds all the same
		d = span
	}
	return interval - span + d
}

// jitterDraw is the production jitter draw of the cleanup runner, in [0, 1) from the global
// math/rand/v2 functions (AGENTS.md §1, Randomness). New and NewWithHooks put it into
// hookSet.cleanupDraw; only this package's tests pass another.
func jitterDraw() float64 { return rand.Float64() }

// runCleanup is the cleanup runner goroutine (DESIGN §A5.1, §A5.8; SPEC STA-5): a pass every
// cleanup_interval × U[0.9, 1.0] (one draw of l.cleanupDraw per period: jitterDraw, unless a
// package test passed another), until ctx, the instance lifetime, ends. The lifetime ends
// after Close's drain, before the connections close (API-1e), and at once in Kill (§A5.12); a
// pass in progress then ends with its calls.
func (l *Limiter) runCleanup(ctx context.Context) {
	interval := time.Duration(l.p.CleanupIntervalNs)
	timer := time.NewTimer(cleanupPeriod(interval, l.cleanupDraw()))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		l.runPass(ctx)
		timer.Reset(cleanupPeriod(interval, l.cleanupDraw()))
	}
}

// RunCleanupPass runs one full cleanup pass now (SPEC API-1f, STA-5; DESIGN §A5.8), with
// exactly the checks of a timed pass, and returns its status and confirmed counts. Passes of
// one instance run one at a time (plan choice): a ready instance waits for a pass in progress,
// and when ctx ends first it returns unavailable, with nothing sent; an instance that is not
// ready returns its status at once, without waiting. ctx bounds every store call of the pass,
// as the host deadline of each (§A5.4).
func (l *Limiter) RunCleanupPass(ctx context.Context) CleanupReport {
	return l.runPass(ctx)
}

// runPass takes the pass lock, runs a pass and counts it in the cleanup metrics (§A5.11). An
// instance that is not ready returns at once, nothing sent (§A5.8): its readiness is looked at
// before waiting for the pass lock, and again under it (pass).
func (l *Limiter) runPass(ctx context.Context) CleanupReport {
	var rep CleanupReport
	status := passUnavailable
	if ctx.Err() == nil {
		if rd := l.readiness(); !rd.ready {
			status, rep.Diag = notReadyStop(rd)
		} else {
			select {
			case l.passLock <- struct{}{}:
				rep, status = l.pass(ctx)
				<-l.passLock
			case <-ctx.Done():
			}
		}
	}
	rep.Status = status.String()
	pass, stop, stopped := status.labels()
	l.reg.Inc(metrics.CleanupPasses, pass)
	if stopped {
		l.reg.Inc(metrics.CleanupStops, stop)
	}
	for _, c := range []struct {
		action int
		n      int64
	}{
		{metrics.ActionRemoved, rep.Removed}, {metrics.ActionMoved, rep.Moved},
		{metrics.ActionQuarantined, rep.Quarantined}, {metrics.ActionSkipped, rep.Skipped},
	} {
		if c.n > 0 {
			l.reg.Add(metrics.CleanupEntries, c.action, uint64(c.n))
		}
	}
	return rep
}

// pass is the pass procedure (DESIGN §A4.5, §A5.8), under the pass lock. An instance that is
// not ready returns at once, touching nothing: config_error for settings_invalid and
// fingerprint_mismatch, epoch-wide state_integrity_error for no_epoch, state_lost and
// integrity_hold, unavailable once Close or Kill began. Otherwise, under the epoch the watch
// joined and its floors: due_entries with cutoff 0, its T0 kept as the pass's cutoff (and
// rl_cleanup_lag_seconds set from that first page); the page's steps; the next page with that
// cutoff only after the page completes; until a page is empty. The first failed call stops
// the pass with its status; the confirmed steps are counted, the others not, and an epoch-wide
// config_error or state_integrity_error reply asks the watch for an early read (epochWideStop).
// After a complete pass, and only then, the reclaimer runs (§A4.7).
func (l *Limiter) pass(ctx context.Context) (CleanupReport, passStatus) {
	var rep CleanupReport
	rd := l.readiness()
	if !rd.ready {
		status, d := notReadyStop(rd)
		rep.Diag = d
		return rep, status
	}
	st := rd.st
	c := store.Cleanup{
		EpochID: st.id, FP: l.fp, FloorHwm: st.floorHwm, FloorGen: st.floorGen,
		CapUnits: l.capUnits, RateTokens: l.p.RateTokens, PushUs: l.lifetimeUs, Pipelines: l.in.CleanupPipelines,
	}
	cutoff := int64(0)
	for first := true; ; first = false {
		page, status, d := l.duePage(ctx, c, cutoff)
		if status != passComplete {
			l.epochWideStop(status)
			rep.Diag = d
			return rep, status
		}
		if first {
			cutoff = page.t0
			lag := 0.0
			if len(page.members) > 0 {
				lag = float64(page.t0-page.oldest) / 1e6
			}
			l.reg.Set(metrics.CleanupLagSeconds, 0, lag)
		}
		if len(page.members) == 0 {
			break
		}
		if status, d := l.steps(store.CleanupEntries(ctx, l.adapter, c, page.members), &rep); status != passComplete {
			l.epochWideStop(status)
			rep.Diag = d
			return rep, status
		}
	}
	l.reclaim(ctx, st.id)
	return rep, passComplete
}

// epochWideStop asks the epoch watch for an early read (§A5.7), which it takes at most once per
// interval, when a reply stopped the pass with an epoch-wide config_error (20) or
// state_integrity_error (22): an epoch replaced, lost or held is joined or reported without
// waiting for the next timed read, and the next pass runs under it (plan S8 step 2). The
// readiness stops of pass send nothing and ask for nothing, as acquire's local refusals.
func (l *Limiter) epochWideStop(s passStatus) {
	switch s {
	case passConfigError, passStateIntegrityError:
		l.watch.trigger()
	case passComplete, passClockError, passUnavailable, passOutcomeUnknown:
	}
}

// notReadyStop maps a refused readiness look to the pass's status and Diag (§A5.8). A health
// reason outside the not-ready ones (§A5.11) is a library defect, reported in Diag (the
// library has no logger): outcome_unknown, failing closed.
func notReadyStop(rd readiness) (passStatus, Diag) {
	switch rd.health {
	case reasonSettingsInvalid, reasonMismatch:
		return passConfigError, rd.diag
	case reasonClosing:
		return passUnavailable, rd.diag
	case reasonNoEpoch:
		return passStateIntegrityError, Diag{Scope: scopeEpoch, Reason: reasonNoEpoch}
	case reasonStateLost, reasonIntegrityHold: // scope epoch, with the watch's reason
		return passStateIntegrityError, rd.diag
	}
	return passOutcomeUnknown, Diag{Reason: rd.health}
}

// page is one decoded due_entries page: T0, the oldest due score (0 when it failed is_score)
// and the members.
type page struct {
	t0, oldest int64
	members    []string
}

// duePage runs due_entries for one page and decodes it (§A4.9). Any other reply or outcome is
// the pass's stop.
func (l *Limiter) duePage(ctx context.Context, c store.Cleanup, cutoff int64) (page, passStatus, Diag) {
	r := store.DueEntries(ctx, l.adapter, c, l.in.CleanupBatch, cutoff)
	if r.StoreError != "" {
		l.setStoreError(r.StoreError)
	}
	if r.Outcome != store.OK {
		status, d := outcomeStop(r)
		return page{}, status, d
	}
	rep := r.Reply
	switch rep.Code {
	case 0: // a page: {0, T0, '', oldest_due_or_0, member…}
		oldest, ok := elemInt(rep.Elems, 3)
		if !ok || rep.TUs < 0 || rep.TUs >= tBound || oldest < 0 || oldest > rep.TUs || len(rep.Elems)-4 > l.in.CleanupBatch {
			break // the decoder checks all of it; fail closed all the same
		}
		p := page{t0: rep.TUs, oldest: oldest, members: make([]string, 0, len(rep.Elems)-4)}
		for i := 4; i < len(rep.Elems); i++ {
			m, isStr := elemStr(rep.Elems, i)
			if !isStr {
				return page{}, passOutcomeUnknown, Diag{StoreError: r.Raw}
			}
			p.members = append(p.members, m)
		}
		return p, passComplete, Diag{}
	case 20, 21, 22:
		status, d := refusalStop(r)
		return page{}, status, d
	}
	return page{}, passOutcomeUnknown, Diag{StoreError: r.Raw}
}

// steps counts a page's cleanup_entry responses into rep (§A4.9, §A5.11): 30 removed, 31
// moved (also the push of a quarantined key), 32 quarantined, 33 skipped; every confirmed step
// counts. It returns the status of the first response, in member order, that is no confirmed
// step, which stops the pass.
func (l *Limiter) steps(rs []store.Response, rep *CleanupReport) (passStatus, Diag) {
	status, diag := passComplete, Diag{}
	stop := func(s passStatus, d Diag) {
		if status == passComplete {
			status, diag = s, d
		}
	}
	for _, r := range rs {
		if r.StoreError != "" {
			l.setStoreError(r.StoreError)
		}
		if r.Outcome != store.OK {
			stop(outcomeStop(r))
			continue
		}
		switch r.Reply.Code {
		case 30: // E_REMOVED
			rep.Removed++
		case 31: // E_MOVED
			rep.Moved++
		case 32: // E_QUARANTINED
			rep.Quarantined++
		case 33: // E_SKIPPED
			rep.Skipped++
		case 20, 21, 22:
			stop(refusalStop(r))
		case 25: // E_BAD_CALL: a library defect, reported in Diag (the library has no logger)
			stop(passOutcomeUnknown, Diag{Reason: r.Reply.Reason, StoreError: r.Raw})
		default: // the decoder admits no other cleanup_entry code; fail closed
			stop(passOutcomeUnknown, Diag{StoreError: r.Raw})
		}
	}
	return status, diag
}

// refusalStop is the stop of an epoch-wide refusal reply in r: 20 config_error, 21
// clock_error, 22 state_integrity_error (scope epoch). Any other code is a library defect
// (the callers pass only these three), reported in Diag with the reason and the raw reply (the
// library has no logger): outcome_unknown, failing closed.
func refusalStop(r store.Response) (passStatus, Diag) {
	rep := r.Reply
	switch rep.Code {
	case 20: // E_CONFIG
		return passConfigError, Diag{Reason: rep.Reason}
	case 21: // E_CLOCK
		return passClockError, Diag{Reason: rep.Reason}
	case 22: // E_INTEGRITY_EPOCH
		return passStateIntegrityError, Diag{Scope: scopeEpoch, Reason: rep.Reason}
	}
	return passOutcomeUnknown, Diag{Reason: rep.Reason, StoreError: r.Raw}
}

// outcomeStop is the stop of a call with no decoded reply (API-1f): unavailable when nothing
// was sent (cancelled too: the pass's statuses have no cancelled, plan choice), else
// outcome_unknown, with the error reply or raw reply for Diag.
func outcomeStop(r store.Response) (passStatus, Diag) {
	raw := r.StoreError
	if raw == "" {
		raw = r.Raw
	}
	switch r.Outcome {
	case store.Unavailable, store.Cancelled:
		return passUnavailable, Diag{StoreError: raw}
	case store.OK, store.OutcomeUnknown, store.CapacityError:
	}
	return passOutcomeUnknown, Diag{StoreError: raw}
}
