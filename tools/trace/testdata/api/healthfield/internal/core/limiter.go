package core

import "context"

type Limiter struct{ closed bool }

type Health struct {
	Status, Reason string
	Zork           int // want: no doc names Zork, and §A5.2 lists no fields of Health
}

type MetricsSnapshot struct{ Families []string }

type Option func(*options)

type options struct{}

type CloseOption func(*closeOptions)

type closeOptions struct{}

func New(p Policy, in Instance, st StoreOptions, opts ...Option) (*Limiter, error) {
	return &Limiter{}, nil
}

func (l *Limiter) Acquire(ctx context.Context, key string) Result { return Result{} }

func (l *Limiter) Close(opts ...CloseOption) { l.closed = true }

func (l *Limiter) Health() Health { return Health{} }

func (l *Limiter) Metrics() MetricsSnapshot { return MetricsSnapshot{} }

func (l *Limiter) shutdown() {}
