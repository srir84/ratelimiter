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
		fixtures:        true,
	}, opts)
}

// Kill stops l at once, as a process kill would (DESIGN §A5.12): the goroutines stop and
// every connection closes, with no drain; refunds still pending are lost (S6). Afterwards
// Acquire returns not_ready and Health reports closing (plan choice).
func Kill(l *Limiter) {
	if l != nil {
		l.shutdown()
	}
}

// PermitEpoch returns the epoch_id of an allowed permit's receipt in lowercase hex, and ""
// for a degraded or nil permit (limitertest.PermitEpoch, for the driver's op log).
func PermitEpoch(p *Permit) string {
	if p == nil || p.id == "" {
		return ""
	}
	return hex.EncodeToString(p.receipt.EpochID[:])
}
