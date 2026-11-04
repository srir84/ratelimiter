package httpapi

import (
	"context"

	"github.com/srir84/ratelimiter/limiter"
)

// FromLimiter adapts the library's limiter to Limiter.
func FromLimiter(l *limiter.Limiter) Limiter { return libLimiter{l: l} }

// libLimiter is a *limiter.Limiter as a Limiter.
type libLimiter struct{ l *limiter.Limiter }

// Acquire calls the library's Acquire. The result's permit becomes Decision.Permit only for
// the allowed and degraded kinds: a nil *limiter.Permit stored in the Permit interface would
// not be nil.
func (a libLimiter) Acquire(ctx context.Context, key string) Decision {
	res := a.l.Acquire(ctx, key)
	d := Decision{Kind: res.Kind, DecisionNs: res.DecisionNs, RetryAfterNs: res.RetryAfterNs}
	if (res.Kind == limiter.Allowed || res.Kind == limiter.Degraded) && res.Permit != nil {
		d.Permit = res.Permit
	}
	return d
}

func (a libLimiter) Health() limiter.Health { return a.l.Health() }

func (a libLimiter) Metrics() limiter.MetricsSnapshot { return a.l.Metrics() }

// Close closes the limiter with the default drain (DrainTimeoutNs; HAR-15).
func (a libLimiter) Close() { a.l.Close() }
