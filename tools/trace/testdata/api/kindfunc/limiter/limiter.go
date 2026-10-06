// Package limiter is the TestAPI fixture's public API: aliases of internal/core.
package limiter

import "ratelimiter/internal/core"

type (
	Limiter         = core.Limiter
	Policy          = core.Policy
	Instance        = core.Instance
	StoreOptions    = core.StoreOptions
	FailureMode     = core.FailureMode
	ClockSource     = core.ClockSource
	Result          = core.Result
	ResultKind      = core.ResultKind
	Diag            = core.Diag
	Permit          = core.Permit
	Health          = core.Health
	MetricsSnapshot = core.MetricsSnapshot
	Option          = core.Option
)

const (
	Closed     = core.Closed
	Open       = core.Open
	Store      = core.Store
	Controlled = core.Controlled
	Allowed    = core.Allowed
	Limited    = core.Limited
)

var DefaultPolicy = func() Policy { return Policy{} } // want: §A5.2 declares the function DefaultPolicy

func DefaultInstance(p Policy) Instance { return Instance{} }

func New(p Policy, in Instance, st StoreOptions, opts ...Option) (*Limiter, error) {
	return core.New(p, in, st, opts...)
}
