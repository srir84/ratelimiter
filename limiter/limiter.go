package limiter

import (
	"time"

	"ratelimiter/internal/core"
	"ratelimiter/internal/settings"
)

// The public types (DESIGN §A5.2, §A5.11).
type (
	Limiter          = core.Limiter
	Policy           = core.Policy
	Instance         = core.Instance
	StoreOptions     = core.StoreOptions
	FailureMode      = core.FailureMode
	ClockSource      = core.ClockSource
	Result           = core.Result
	ResultKind       = core.ResultKind
	Diag             = core.Diag
	Permit           = core.Permit
	RefundEvent      = core.RefundEvent
	RefundResult     = core.RefundResult
	UnresolvedReason = core.UnresolvedReason
	Health           = core.Health
	CleanupReport    = core.CleanupReport
	MetricsSnapshot  = core.MetricsSnapshot
	Option           = core.Option
	CloseOption      = core.CloseOption
)

// FailureMode and ClockSource values.
const (
	Closed     = core.Closed     // FailureMode: an unreachable store refuses (the default)
	Open       = core.Open       // FailureMode: an unreachable store admits as degraded
	Store      = core.Store      // ClockSource: the store's clock (the default, and production's only source)
	Controlled = core.Controlled // ClockSource: the test clock, accepted only through limitertest
)

// ResultKind values (SPEC API-4 … API-12a).
const (
	Allowed             = core.Allowed
	Degraded            = core.Degraded
	Limited             = core.Limited
	ValidationError     = core.ValidationError
	ClockError          = core.ClockError
	Busy                = core.Busy
	Cancelled           = core.Cancelled
	ConfigError         = core.ConfigError
	NotReady            = core.NotReady
	StateIntegrityError = core.StateIntegrityError
	CapacityError       = core.CapacityError
	Unavailable         = core.Unavailable
	OutcomeUnknown      = core.OutcomeUnknown
	ReceiptError        = core.ReceiptError
)

// RefundResult and UnresolvedReason values (SPEC API-1d).
const (
	Applied             = core.Applied
	AlreadyApplied      = core.AlreadyApplied
	ReceiptInvalid      = core.ReceiptInvalid
	ReceiptExpired      = core.ReceiptExpired
	Unresolved          = core.Unresolved
	UnresolvedClosed    = core.UnresolvedClosed
	UnresolvedRetention = core.UnresolvedRetention
	UnresolvedExpired   = core.UnresolvedExpired
)

// DefaultPolicy returns the SPEC defaults (CFG-8); the required fields MaxTrackedBuckets and
// MaxLiveMarkers are left 0.
func DefaultPolicy() Policy { return settings.DefaultPolicy() }

// DefaultInstance returns the DESIGN defaults for policy p (§A5.10), with DrainTimeoutNs =
// p.RefundDeadlineNs + p.OutageToleranceNs; the required fields MaxReleaseDelayNs, MarginNs,
// MaxHeldReceipts and MaxPendingRefunds are left 0.
func DefaultInstance(p Policy) Instance { return settings.DefaultInstance(p) }

// WithRefundListener has f receive each released permit's one final refund event (SPEC
// API-1d): Applied, AlreadyApplied, ReceiptInvalid, ReceiptExpired or Unresolved (Closed,
// Retention, Expired). Events reach f through a bounded queue and one goroutine; f may block or
// panic without affecting the limiter, an event that finds the queue full is dropped and
// counted (Health.ListenerDrops), and f must never call back into the limiter (HOST-8).
func WithRefundListener(f func(RefundEvent)) Option { return core.WithRefundListener(f) }

// WithDrainTimeout makes Close drain pending refunds for d instead of the instance's
// DrainTimeoutNs (SPEC API-1e).
func WithDrainTimeout(d time.Duration) CloseOption { return core.WithDrainTimeout(d) }

// New builds an instance (production construction, DESIGN §A5.2). It never returns nil:
// invalid settings, ClockSource = Controlled (CMP-9) or a failed signing-key generation give
// an instance that is permanently not ready (settings_invalid), which contacts no store, and
// an error for the host to log. A valid instance joins the store's epoch in the background
// (CFG-6). Until it has, Acquire returns not_ready (Health reports no_epoch) while no epoch
// record exists, or no read has succeeded yet; config_error (fingerprint_mismatch) when the
// record's fingerprint differs; and an epoch-wide state_integrity_error (state_lost or
// integrity_hold) when the record is malformed or held, or the high-water mark is missing or
// malformed.
func New(p Policy, in Instance, st StoreOptions, opts ...Option) (*Limiter, error) {
	return core.New(p, in, st, opts...)
}
