// Package core is the limiter (DESIGN §A5): the public types, the acquire flow, the permit
// state machine, slots, refund workers, the epoch watch and the cleanup runner. Package
// limiter re-exports its types as aliases. Types owned by a lower internal package are
// aliased here (§A5.2 "Where things are declared").
package core

import (
	"strconv"

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
