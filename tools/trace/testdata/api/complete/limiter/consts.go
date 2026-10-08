package limiter

import "ratelimiter/internal/core"

const (
	Degraded            = core.Degraded
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
