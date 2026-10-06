//go:build rltest

package fixtures

import (
	"fmt"
	"sync"

	"ratelimiter/internal/receipt"
)

// SignerMode is a signer fault (DESIGN §B6 FLT-10); corrupt comes in S6.
type SignerMode string

// The signer fault modes.
const (
	// SignFail makes Sign return ErrSignFailed after a confirmed deduction: receipt_error
	// (API-12a).
	SignFail SignerMode = "fail"
	// SignUnusable makes Usable report false: config_error with no charge (API-1a).
	SignUnusable SignerMode = "unusable"
)

// ErrSignFailed is the error a SignFail fault returns from Sign.
const ErrSignFailed Error = "fixtures: signing failed (signer fault)"

// SignerFault is one armed signer fault (plan choice).
type SignerFault struct {
	Mode SignerMode
	// Count is how many calls the fault answers (Sign calls for SignFail, Usable calls for
	// SignUnusable); 0 is every call until Disarm.
	Count int
}

// SignerWrapper stands between the limiter and its signer (DESIGN §A5.12, §B6) and applies
// the armed faults. It is safe for concurrent use.
type SignerWrapper struct {
	mu     sync.Mutex
	faults []*armedSigner
}

type armedSigner struct {
	f    SignerFault
	left int
}

// NewSignerWrapper returns a wrapper with no fault armed.
func NewSignerWrapper() *SignerWrapper { return &SignerWrapper{} }

// Wrap returns inner behind the wrapper; limitertest.WithSignerWrapper takes it.
func (w *SignerWrapper) Wrap(inner receipt.Signer) receipt.Signer {
	return &wrappedSigner{w: w, next: inner}
}

// Arm arms f. It fails on an unknown mode or a negative count.
func (w *SignerWrapper) Arm(f SignerFault) error {
	switch f.Mode {
	case SignFail, SignUnusable:
	default:
		return fmt.Errorf("fixtures: unknown signer fault mode %q", f.Mode)
	}
	if f.Count < 0 {
		return fmt.Errorf("fixtures: signer fault %q: negative count", f.Mode)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.faults = append(w.faults, &armedSigner{f: f, left: f.Count})
	return nil
}

// Disarm removes every armed signer fault.
func (w *SignerWrapper) Disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.faults = nil
}

// take reports whether an armed fault of mode m answers this call, using up one of its calls.
func (w *SignerWrapper) take(m SignerMode) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, a := range w.faults {
		if a.f.Mode != m {
			continue
		}
		if a.f.Count > 0 {
			if a.left--; a.left == 0 {
				w.faults = append(w.faults[:i], w.faults[i+1:]...)
			}
		}
		return true
	}
	return false
}

// wrappedSigner is the signer Wrap returns.
type wrappedSigner struct {
	w    *SignerWrapper
	next receipt.Signer
}

func (s *wrappedSigner) Usable() bool {
	if s.w.take(SignUnusable) {
		return false
	}
	return s.next.Usable()
}

func (s *wrappedSigner) Sign(r receipt.Receipt) (receipt.Receipt, error) {
	if s.w.take(SignFail) {
		return receipt.Receipt{}, ErrSignFailed
	}
	return s.next.Sign(r)
}

func (s *wrappedSigner) Verify(r receipt.Receipt) bool { return s.next.Verify(r) }
