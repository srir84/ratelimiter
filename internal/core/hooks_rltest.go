//go:build rltest

package core

import (
	"encoding/hex"
	"io"

	"ratelimiter/internal/receipt"
	"ratelimiter/internal/store"
)

// Hooks are the test hooks of NewWithHooks (DESIGN §A5.12; shape: plan choice). The zero
// value adds none, except that the limiter reports Health.Fixtures = true.
type Hooks struct {
	ControlledClock bool                                // accept ClockSource = Controlled
	StoreWrapper    func(store.Adapter) store.Adapter   // wraps the store adapter
	SignerWrapper   func(receipt.Signer) receipt.Signer // wraps the signer
	SendObserver    func(cmd string, bytesWritten int)  // hears the bytes written per command (§A5.4)
	KeyReader       io.Reader                           // source of the signing key bytes, instead of crypto/rand
}

// NewWithHooks is the only hook entry point (DESIGN §A5.12), compiled with -tags rltest
// only; limitertest wraps it. It never returns nil.
func NewWithHooks(p Policy, in Instance, st StoreOptions, h Hooks, opts ...Option) (*Limiter, error) {
	return newLimiter(p, in, st, hookSet{
		controlledClock: h.ControlledClock,
		storeWrapper:    h.StoreWrapper,
		signerWrapper:   h.SignerWrapper,
		sendObserver:    h.SendObserver,
		keyReader:       h.KeyReader,
		cleanupDraw:     jitterDraw,
		fixtures:        true,
	}, opts)
}

// Kill stops l at once, as a process kill would (DESIGN §A5.12): the listener dispatcher
// stops first, so no event is delivered after it begins; then the refund workers and every
// backoff and retention timer stop, and the epoch watch, the cleanup runner (a pass in
// progress ends with its calls) and every connection, with no drain.
// Refunds still pending are lost and emit nothing. Afterwards Acquire returns not_ready, Health
// reports closing, and Close returns at once (plan choice).
func Kill(l *Limiter) {
	if l == nil {
		return
	}
	l.closeStarted.Store(true)
	l.closing.Store(true)
	if l.lis != nil {
		l.lis.stop()
	}
	if l.refunds != nil {
		l.refunds.stop()
	}
	l.shutdown()
}

// ListenerQueueLen returns how many refund events wait in l's listener queue, not yet taken by
// the dispatcher (limitertest.ListenerQueueLen, for hitapi's /health listener_queue_len in
// test builds); 0 for a nil limiter or one without a listener. Health stays as §A5.11
// defines it.
func ListenerQueueLen(l *Limiter) int {
	if l == nil || l.lis == nil {
		return 0
	}
	return len(l.lis.ch)
}

// PermitEpoch returns the epoch_id of an allowed permit's receipt in lowercase hex, and ""
// for a degraded or nil permit (limitertest.PermitEpoch, for the driver's op log).
func PermitEpoch(p *Permit) string {
	if p == nil || p.id == "" {
		return ""
	}
	return hex.EncodeToString(p.receipt.EpochID[:])
}
