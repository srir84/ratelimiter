//go:build rltest

// Package limitertest is the TestAPI fixture's test-only entry point.
package limitertest

import (
	"ratelimiter/internal/core"
	"ratelimiter/internal/store"
	"ratelimiter/limiter"
)

type Adapter = store.Adapter

type Option func(*core.Hooks)

func New(p limiter.Policy, in limiter.Instance, st limiter.StoreOptions, opts ...Option) (*limiter.Limiter, error) {
	var h core.Hooks
	for _, o := range opts {
		o(&h)
	}
	return core.NewWithHooks(p, in, st, h)
}

func WithControlledClock() Option { return func(h *core.Hooks) { h.ControlledClock = true } }

func Kill(l *limiter.Limiter) { core.Kill(l) }
