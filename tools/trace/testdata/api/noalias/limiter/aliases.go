package limiter

import "ratelimiter/internal/core"

// No CleanupReport alias: Limiter.RunCleanupPass returns a type limiter does not export.
type (
	CloseOption      = core.CloseOption
	RefundEvent      = core.RefundEvent
	RefundResult     = core.RefundResult
	UnresolvedReason = core.UnresolvedReason
)
