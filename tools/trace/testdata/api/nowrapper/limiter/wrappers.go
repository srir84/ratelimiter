package limiter

import "ratelimiter/internal/core"

// No WithDrainTimeout wrapper.
func WithRefundListener(f func(RefundEvent)) Option { return core.WithRefundListener(f) }
