package core

import "context"

type Limiter struct{ closed bool }

type Health struct{ Status, Reason string }

type MetricsSnapshot struct{ Families []string }

type Option func(*options)

type options struct{}

func New(p Policy, in Instance, st StoreOptions, opts ...Option) (*Limiter, error) {
	return &Limiter{}, nil
}

func (l *Limiter) Acquire(ctx context.Context, key string) Result { return Result{} }

func (l *Limiter) Close() { l.closed = true } // want: S5's staged func() fails once S6 emptied the interim list

func (l *Limiter) Health() Health { return Health{} }

func (l *Limiter) Metrics() MetricsSnapshot { return MetricsSnapshot{} }

func (l *Limiter) shutdown() {}
