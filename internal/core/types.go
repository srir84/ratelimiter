// Package core is the limiter (DESIGN §A5): the public types, the acquire flow, the permit
// state machine, slots, refund workers, the epoch watch and the cleanup runner. Package
// limiter re-exports its types as aliases. Types owned by a lower internal package are
// aliased here (§A5.2 "Where things are declared").
package core

import (
	"strconv"
	"time"

	"ratelimiter/internal/metrics"
	"ratelimiter/internal/settings"
)

// Settings and metrics types from the leaf packages (§A5.2).
type (
	Policy          = settings.Policy
	Instance        = settings.Instance
	StoreOptions    = settings.StoreOptions
	FailureMode     = settings.FailureMode
	ClockSource     = settings.ClockSource
	MetricsSnapshot = metrics.MetricsSnapshot
)

// FailureMode and ClockSource values.
const (
	Closed     = settings.Closed
	Open       = settings.Open
	Store      = settings.Store
	Controlled = settings.Controlled
)

// ResultKind is the outcome of Acquire, a closed set (SPEC API-4 … API-12a). The values
// follow §A5.2's order, which is also the label order of rl_acquire_results_total.
type ResultKind int

const (
	Allowed ResultKind = iota
	Degraded
	Limited
	ValidationError
	ClockError
	Busy
	Cancelled
	ConfigError
	NotReady
	StateIntegrityError
	CapacityError
	Unavailable
	OutcomeUnknown
	ReceiptError
)

// String returns the SPEC name, e.g. state_integrity_error.
func (k ResultKind) String() string {
	switch k {
	case Allowed:
		return "allowed"
	case Degraded:
		return "degraded"
	case Limited:
		return "limited"
	case ValidationError:
		return "validation_error"
	case ClockError:
		return "clock_error"
	case Busy:
		return "busy"
	case Cancelled:
		return "cancelled"
	case ConfigError:
		return "config_error"
	case NotReady:
		return "not_ready"
	case StateIntegrityError:
		return "state_integrity_error"
	case CapacityError:
		return "capacity_error"
	case Unavailable:
		return "unavailable"
	case OutcomeUnknown:
		return "outcome_unknown"
	case ReceiptError:
		return "receipt_error"
	}
	return "ResultKind(" + strconv.Itoa(int(k)) + ")"
}

// RefundResult is the final result of a released permit's refund, as the listener sees it
// (API-1d). The values are the label order of rl_refund_results_total.
type RefundResult int

const (
	Applied RefundResult = iota
	AlreadyApplied
	ReceiptInvalid
	ReceiptExpired
	Unresolved
)

// String returns the SPEC name, e.g. already_applied.
func (r RefundResult) String() string {
	switch r {
	case Applied:
		return "applied"
	case AlreadyApplied:
		return "already_applied"
	case ReceiptInvalid:
		return "receipt_invalid"
	case ReceiptExpired:
		return "receipt_expired"
	case Unresolved:
		return "unresolved"
	}
	return "RefundResult(" + strconv.Itoa(int(r)) + ")"
}

// UnresolvedReason says why a refund ended unresolved (API-1d, REF-4a). It is set only on
// Unresolved events; the zero value is no reason and prints as "". The Go names carry the
// Unresolved prefix because Closed is FailureMode's.
type UnresolvedReason int

const (
	UnresolvedClosed    UnresolvedReason = iota + 1 // the drain of Close ended, or release() after Close began
	UnresolvedRetention                             // the retention timer fired while the refund was pending
	UnresolvedExpired                               // release() on a permit whose retention had already ended
)

// String returns the SPEC name: closed, retention or expired ("" for the zero value).
func (u UnresolvedReason) String() string {
	switch u {
	case UnresolvedClosed:
		return "closed"
	case UnresolvedRetention:
		return "retention"
	case UnresolvedExpired:
		return "expired"
	}
	if u == 0 {
		return ""
	}
	return "UnresolvedReason(" + strconv.Itoa(int(u)) + ")"
}

// Result is what Acquire returns (DESIGN §A5.2; SPEC API-4 … API-12a).
type Result struct {
	Kind         ResultKind // the outcome, a closed set
	DecisionNs   int64      // allowed, limited: the decision's budget time, t_us × 1000
	RetryAfterNs int64      // limited: the exact wait, rounded up to the next whole ns
	Permit       *Permit    // allowed, degraded; nil for every other kind
	Diag         Diag       // why, for operators
}

// Diag explains a result for operators (DESIGN §A5.11). Scope is "key" or "epoch" for a
// state_integrity_error and "" otherwise. Reason is a reason string of §A3.3 or §A4.9 from
// the store, or a local check's (plan choice): a not-ready health reason (no_epoch,
// settings_invalid, closing), fingerprint_mismatch, signer_unusable or signing_failed.
// StoreError is the raw error reply text, or the raw reply that failed its §A4.9 checks. No
// field ever holds a password, a signing key or a budget key.
type Diag struct {
	Scope      string
	Reason     string
	StoreError string
}

// RefundEvent is the one final event of a released permit's refund (API-1d), which the
// refund listener receives. ReleaseMonoNs and FinalMonoNs are ns since the limiter was built,
// on the monotonic clock (plan choice).
type RefundEvent struct {
	DeductionID                string
	Result                     RefundResult     // Applied, AlreadyApplied, ReceiptInvalid, ReceiptExpired, Unresolved
	Reason                     UnresolvedReason // Closed, Retention, Expired (Unresolved only)
	Attempts                   int              // refund commands actually sent (API-1d)
	CreditDecisionNs           int64            // 0 when unknown
	ReleaseMonoNs, FinalMonoNs int64            // monotonic times of release() and of the final result (REF-5 evidence)
}

// Health statuses (DESIGN §A5.11, SPEC API-1g).
const (
	statusReady    = "ready"
	statusNotReady = "not_ready"
)

// Health is an instance's readiness and settings (DESIGN §A5.11, SPEC API-1g); the harness
// serves it as JSON.
type Health struct {
	Status        string   `json:"status"`         // "ready" or "not_ready"
	Reason        string   `json:"reason"`         // "ready", or no_epoch, fingerprint_mismatch, state_lost, integrity_hold, closing, settings_invalid
	EpochID       string   `json:"epoch_id"`       // the epoch joined last; "" before the first join
	ClockSource   string   `json:"clock_source"`   // "store" or "controlled"
	Instance      Instance `json:"instance"`       // the per-instance settings
	ListenerDrops uint64   `json:"listener_drops"` // refund events dropped by the listener queue
	StoreError    string   `json:"store_error"`    // the last store error; "" once an epoch read succeeds (plan choice)
	Fixtures      bool     `json:"fixtures"`       // true only for limiters built through NewWithHooks
}

// CleanupReport is the outcome of one cleanup pass (SPEC API-1f), which RunCleanupPass
// returns from S7 on.
type CleanupReport struct {
	Status                               string // "complete", or the SPEC name of the result that stopped the pass
	Removed, Moved, Quarantined, Skipped int64  // confirmed counts
	Diag                                 Diag
}

// Option configures New (DESIGN §A5.2): WithRefundListener.
type Option func(*options)

// options are what Options set.
type options struct {
	listener func(RefundEvent) // the refund listener; nil for none
}

// CloseOption configures Close (DESIGN §A5.2): WithDrainTimeout.
type CloseOption func(*closeOptions)

// closeOptions are what CloseOptions set: the drain timeout, DrainTimeoutNs unless a
// WithDrainTimeout overrides it.
type closeOptions struct {
	drain time.Duration
}
