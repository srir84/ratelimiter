package core

// Health reasons (SPEC API-1g, CFG-6; DESIGN §A5.7, §A5.11).
const (
	reasonReady           = "ready"
	reasonNoEpoch         = "no_epoch"
	reasonSettingsInvalid = "settings_invalid"
	reasonClosing         = "closing"
	reasonMismatch        = "fingerprint_mismatch"
	reasonStateLost       = "state_lost"
)

// Diag scopes of a state_integrity_error (DESIGN §A5.11).
const (
	scopeKey   = "key"
	scopeEpoch = "epoch"
)

// readiness is the epoch-watch step of Acquire's local checks (DESIGN §A5.3, CFG-6) and
// Health's status, from one look at the instance.
type readiness struct {
	ready  bool
	kind   ResultKind  // the refusal when not ready
	diag   Diag        // the refusal's Diag
	health string      // Health.Reason
	st     *epochState // the watch snapshot the look used
}

// readiness maps the instance's state to the not_ready, config_error and epoch-wide
// state_integrity_error refusals: settings_invalid first (permanent, plan choice), then
// closing, then the watch's state.
func (l *Limiter) readiness() readiness {
	st := l.epoch.Load()
	notReady := func(reason string) readiness {
		return readiness{kind: NotReady, diag: Diag{Reason: reason}, health: reason, st: st}
	}
	switch {
	case l.invalid:
		return notReady(reasonSettingsInvalid)
	case l.closing.Load():
		return notReady(reasonClosing)
	}
	switch st.state {
	case wsJoined:
		return readiness{ready: true, health: reasonReady, st: st}
	case wsNoEpoch:
		return notReady(reasonNoEpoch)
	case wsMismatch:
		return readiness{kind: ConfigError, diag: Diag{Reason: reasonMismatch}, health: reasonMismatch, st: st}
	case wsLost:
		return readiness{kind: StateIntegrityError, diag: Diag{Scope: scopeEpoch, Reason: st.reason}, health: reasonStateLost, st: st}
	case wsHeld:
		return readiness{kind: StateIntegrityError, diag: Diag{Scope: scopeEpoch, Reason: reasonIntegrityHold}, health: reasonIntegrityHold, st: st}
	}
	return notReady(reasonNoEpoch) // no other state exists; fail closed
}

// Health returns the instance's readiness and settings (DESIGN §A5.11, SPEC API-1g).
func (l *Limiter) Health() Health {
	rd := l.readiness()
	h := Health{
		Status:        statusNotReady,
		Reason:        rd.health,
		EpochID:       rd.st.id,
		ClockSource:   l.p.ClockSource.String(),
		Instance:      l.in,
		ListenerDrops: l.listenerDrops(),
		StoreError:    l.storeError(),
		Fixtures:      l.fixtures,
	}
	if rd.ready {
		h.Status = statusReady
	}
	return h
}
