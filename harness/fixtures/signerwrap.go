//go:build rltest

package fixtures

import (
	"fmt"
	"slices"
	"sync"

	"ratelimiter/internal/receipt"
)

// SignerMode is a signer fault (DESIGN §B6 FLT-10).
type SignerMode string

// The signer fault modes.
const (
	// SignFail makes Sign return ErrSignFailed after a confirmed deduction: receipt_error
	// (API-12a).
	SignFail SignerMode = "fail"
	// SignUnusable makes Usable report false: config_error with no charge (API-1a).
	SignUnusable SignerMode = "unusable"
	// SignCorrupt alters one authenticated field of a signed receipt (SignerFault.Field) before
	// the signer verifies it, keeping its MAC, so the refund's local verification fails:
	// receipt_invalid with nothing sent (SPEC T-RCP-1; plan S6 step 10). Plan choice: the
	// alteration is made on the receipt Verify receives, after signing, so the permit keeps
	// the receipt the store charged and its deduction_id and epoch_id stay the store's (the op
	// log's acquire record).
	SignCorrupt SignerMode = "corrupt"
)

// The 7 authenticated receipt fields of DESIGN §A5.9, as SignerFault.Field and CorruptField
// name them (plan choice: the §A5.9 names).
const (
	FieldEpochID  = "epoch_id"
	FieldEnc      = "enc"
	FieldG        = "g"
	FieldS        = "s"
	FieldCost     = "cost"
	FieldIssueUs  = "issue_us"
	FieldExpiryUs = "expiry_us"
)

// ReceiptFields returns the 7 authenticated receipt fields in MAC order (§A5.9).
func ReceiptFields() []string {
	return []string{FieldEpochID, FieldEnc, FieldG, FieldS, FieldCost, FieldIssueUs, FieldExpiryUs}
}

// corrupt returns r with field altered and its MAC kept: the epoch_id's last byte and the
// enc's last byte with their low bit flipped, every number one more. Each change alters the
// MAC input (§A5.9), whose fields sit at fixed places, so the MAC no longer verifies. The
// caller has checked field.
func corrupt(r receipt.Receipt, field string) receipt.Receipt {
	switch field {
	case FieldEpochID:
		r.EpochID[len(r.EpochID)-1] ^= 1
	case FieldEnc:
		if n := len(r.Enc); n > 0 {
			r.Enc = r.Enc[:n-1] + string([]byte{r.Enc[n-1] ^ 1})
		} else {
			r.Enc = "x"
		}
	case FieldG:
		r.G++
	case FieldS:
		r.S++
	case FieldCost:
		r.Cost++
	case FieldIssueUs:
		r.IssueUs++
	case FieldExpiryUs:
		r.ExpiryUs++
	}
	return r
}

// ErrSignFailed is the error a SignFail fault returns from Sign.
const ErrSignFailed Error = "fixtures: signing failed (signer fault)"

// SignerFault is one armed signer fault (plan choice).
type SignerFault struct {
	Mode SignerMode
	// Count is how many calls the fault answers (Sign calls for SignFail, Usable calls for
	// SignUnusable, Verify calls for SignCorrupt); 0 is every call until Disarm.
	Count int
	// Field is the receipt field a SignCorrupt fault alters: one of ReceiptFields.
	Field string
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

// Arm arms f. It fails on an unknown mode, a negative count, and a corrupt fault whose Field
// is not one of ReceiptFields (or a Field on another mode).
func (w *SignerWrapper) Arm(f SignerFault) error {
	switch f.Mode {
	case SignFail, SignUnusable:
		if f.Field != "" {
			return fmt.Errorf("fixtures: signer fault %q takes no field", f.Mode)
		}
	case SignCorrupt:
		if !slices.Contains(ReceiptFields(), f.Field) {
			return fmt.Errorf("fixtures: signer fault %q: unknown receipt field %q", f.Mode, f.Field)
		}
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

// CorruptField arms a corrupt fault on field for every Verify until Disarm (plan S6 step 10;
// DESIGN §B6 FLT-10 corrupt): each refund's local verification sees the receipt with field
// altered after signing, so it ends receipt_invalid and sends nothing (T-RCP-1).
func (w *SignerWrapper) CorruptField(name string) error {
	return w.Arm(SignerFault{Mode: SignCorrupt, Field: name})
}

// Disarm removes every armed signer fault.
func (w *SignerWrapper) Disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.faults = nil
}

// take returns the first armed fault of mode m, using up one of its calls; false when none
// answers this call.
func (w *SignerWrapper) take(m SignerMode) (SignerFault, bool) {
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
		return a.f, true
	}
	return SignerFault{}, false
}

// wrappedSigner is the signer Wrap returns.
type wrappedSigner struct {
	w    *SignerWrapper
	next receipt.Signer
}

func (s *wrappedSigner) Usable() bool {
	if _, ok := s.w.take(SignUnusable); ok {
		return false
	}
	return s.next.Usable()
}

func (s *wrappedSigner) Sign(r receipt.Receipt) (receipt.Receipt, error) {
	if _, ok := s.w.take(SignFail); ok {
		return receipt.Receipt{}, ErrSignFailed
	}
	return s.next.Sign(r)
}

// Verify verifies r, with the field of an armed corrupt fault altered first.
func (s *wrappedSigner) Verify(r receipt.Receipt) bool {
	if f, ok := s.w.take(SignCorrupt); ok {
		r = corrupt(r, f.Field)
	}
	return s.next.Verify(r)
}
