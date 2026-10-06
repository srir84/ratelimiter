//go:build rltest

package core

type Hooks struct{ ControlledClock bool }

func NewWithHooks(p Policy, in Instance, st StoreOptions, h Hooks, opts ...Option) (*Limiter, error) {
	return New(p, in, st, opts...)
}

func Kill(l *Limiter) { l.shutdown() }
