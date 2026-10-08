package limiter

import (
	"time"

	"ratelimiter/internal/core"
)

func WithRefundListener(f func(RefundEvent)) Option { return core.WithRefundListener(f) }

func WithDrainTimeout(d time.Duration) CloseOption { return core.WithDrainTimeout(d) }
