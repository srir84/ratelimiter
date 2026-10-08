package limiter

import "ratelimiter/internal/core"

// CleanupReport names another type than the one Limiter.RunCleanupPass returns.
type (
	CloseOption      = core.CloseOption
	CleanupReport    = core.Health
	RefundEvent      = core.RefundEvent
	RefundResult     = core.RefundResult
	UnresolvedReason = core.UnresolvedReason
)
