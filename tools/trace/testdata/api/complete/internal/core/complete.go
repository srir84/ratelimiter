package core

import (
	"context"
	"time"
)

// The rest of the DESIGN §A5.2 surface: with this overlay the TestAPI fixture is complete.

const (
	Degraded ResultKind = 2 + iota
	ValidationError
	ClockError
	Busy
	Cancelled
	ConfigError
	NotReady
	StateIntegrityError
	CapacityError
	Unavailable
	OutcomeUnknown
	ReceiptError
)

type RefundResult int

const (
	Applied RefundResult = iota
	AlreadyApplied
	ReceiptInvalid
	ReceiptExpired
	Unresolved
)

type UnresolvedReason int

const (
	UnresolvedClosed UnresolvedReason = iota
	UnresolvedRetention
	UnresolvedExpired
)

type RefundEvent struct {
	DeductionID                string
	Result                     RefundResult
	Reason                     UnresolvedReason
	Attempts                   int
	CreditDecisionNs           int64
	ReleaseMonoNs, FinalMonoNs int64
}

type CleanupReport struct{ Status string }

func (l *Limiter) RunCleanupPass(ctx context.Context) CleanupReport { return CleanupReport{} }

func (p *Permit) Release() {}

func WithRefundListener(f func(RefundEvent)) Option { return func(*options) {} }

func WithDrainTimeout(d time.Duration) CloseOption { return func(*closeOptions) {} }
