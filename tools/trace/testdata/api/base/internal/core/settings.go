// Package core is the TestAPI fixture's internal/core: the DESIGN §A5.2 types as S5 stages
// them (no RefundEvent, WithRefundListener, RunCleanupPass or Release yet; Close without
// options).
package core

type FailureMode int

const (
	Closed FailureMode = iota
	Open
)

type ClockSource int

const (
	Store ClockSource = iota
	Controlled
)

type Policy struct {
	RateTokens, RatePeriodNs, Capacity   int64
	FailureMode                          FailureMode
	MaxTrackedBuckets, MaxLiveMarkers    int64
	MaxMarkersPerKey                     int64
	ReceiptLifetimeNs, RefundDeadlineNs  int64
	OutageToleranceNs, CleanupIntervalNs int64
	ClockSource                          ClockSource
}

type Instance struct {
	MaxReleaseDelayNs, MarginNs                                           int64
	ClockRateBoundPPM                                                     int64
	DependencyDeadlineNs                                                  int64
	EpochRereadIntervalNs, DrainTimeoutNs                                 int64
	MaxHeldReceipts, MaxPendingRefunds                                    int
	ListenerQueueSize, RefundWorkers                                      int
	RefundBackoffInitialNs, RefundBackoffMaxNs                            int64
	StoreConns, RefundConns, CleanupBatch, CleanupPipelines, ReclaimBatch int
}

type StoreOptions struct{ Addr, User, Password string }
