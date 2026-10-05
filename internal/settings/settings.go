// Package settings declares the limiter's settings (DESIGN §A5.2, §A5.10; SPEC CFG-8)
// and validates them (validate.go: CFG-4, §A3.1, RCP-8, §A5.6). The host builds them,
// usually from DefaultPolicy and DefaultInstance, and passes them to New; the library
// never reads settings from the environment, files or flags.
package settings

import "strconv"

// FailureMode decides what Acquire returns when the store cannot be reached and nothing
// was sent (API-20a, API-20b). The zero value is Closed.
type FailureMode int

const (
	// Closed refuses: such an acquire returns unavailable (the default).
	Closed FailureMode = iota
	// Open admits: such an acquire returns degraded with a permit that holds no receipt.
	Open
)

// String returns the SPEC name: closed or open.
func (m FailureMode) String() string {
	switch m {
	case Closed:
		return "closed"
	case Open:
		return "open"
	}
	return "FailureMode(" + strconv.Itoa(int(m)) + ")"
}

// ClockSource is where budget time comes from (CLK-2). The zero value is Store.
type ClockSource int

const (
	// Store is the store's own clock (TIME), the default and the only production source.
	Store ClockSource = iota
	// Controlled is the rl:ctl:time value that a test driver advances; only the test-only
	// entry point accepts it (CMP-9).
	Controlled
)

// String returns the SPEC name: store or controlled.
func (c ClockSource) String() string {
	switch c {
	case Store:
		return "store"
	case Controlled:
		return "controlled"
	}
	return "ClockSource(" + strconv.Itoa(int(c)) + ")"
}

// Policy holds the shared settings: every instance of a deployment gets the same values,
// and all of them are fingerprinted (CFG-1, CFG-8). Times are in nanoseconds and must be
// whole microseconds (§A3.1).
type Policy struct {
	RateTokens, RatePeriodNs, Capacity   int64       // 10 tokens per 1 s, capacity 20
	FailureMode                          FailureMode // Closed (default) | Open
	MaxTrackedBuckets, MaxLiveMarkers    int64       // required
	MaxMarkersPerKey                     int64       // default 1000
	ReceiptLifetimeNs, RefundDeadlineNs  int64       // 60 s; 30 s
	OutageToleranceNs, CleanupIntervalNs int64       // 5 s; 5 s
	ClockSource                          ClockSource // Store; Controlled only through limitertest
}

// Instance holds the per-instance settings, validated locally and reported by Health
// (CFG-8, §A5.10). Times are in nanoseconds and must be whole microseconds (§A3.1).
type Instance struct {
	MaxReleaseDelayNs, MarginNs                                           int64 // required
	ClockRateBoundPPM                                                     int64 // default 1000
	DependencyDeadlineNs                                                  int64 // default 2 s
	EpochRereadIntervalNs, DrainTimeoutNs                                 int64 // 1 s; refund_deadline + outage_tolerance
	MaxHeldReceipts, MaxPendingRefunds                                    int   // required
	ListenerQueueSize, RefundWorkers                                      int   // 10000; 128
	RefundBackoffInitialNs, RefundBackoffMaxNs                            int64 // 50 ms; 1 s
	StoreConns, RefundConns, CleanupBatch, CleanupPipelines, ReclaimBatch int   // 8; 2; 1000; 4; 1000
}

// StoreOptions are the store connection details the host supplies (CFG-7). The password
// comes from the host and is never formatted into an error, Diag, Health or metric.
type StoreOptions struct{ Addr, User, Password string }

// ReconnectBackoffMaxNs caps the store reconnect backoff, min(1 s, 50 ms × 2^n) (§A5.4),
// and is the reconnect term of the §A5.6 startup inequality.
const ReconnectBackoffMaxNs = 1_000_000_000

// DefaultPolicy returns the SPEC defaults (CFG-8, §A5.10). MaxTrackedBuckets and
// MaxLiveMarkers are required and left 0, so they fail validation until the host sets them.
func DefaultPolicy() Policy {
	return Policy{
		RateTokens:        10,
		RatePeriodNs:      1_000_000_000,
		Capacity:          20,
		FailureMode:       Closed,
		MaxMarkersPerKey:  1_000,
		ReceiptLifetimeNs: 60_000_000_000,
		RefundDeadlineNs:  30_000_000_000,
		OutageToleranceNs: 5_000_000_000,
		CleanupIntervalNs: 5_000_000_000,
		ClockSource:       Store,
	}
}

// DefaultInstance returns the DESIGN defaults (§A5.2, §A5.10) for policy p:
// DrainTimeoutNs = p.RefundDeadlineNs + p.OutageToleranceNs. MaxReleaseDelayNs, MarginNs,
// MaxHeldReceipts and MaxPendingRefunds are required and left 0.
func DefaultInstance(p Policy) Instance {
	return Instance{
		ClockRateBoundPPM:      1_000,
		DependencyDeadlineNs:   2_000_000_000,
		EpochRereadIntervalNs:  1_000_000_000,
		DrainTimeoutNs:         drainDefault(p.RefundDeadlineNs, p.OutageToleranceNs),
		ListenerQueueSize:      10_000,
		RefundWorkers:          128,
		RefundBackoffInitialNs: 50_000_000,
		RefundBackoffMaxNs:     1_000_000_000,
		StoreConns:             8,
		RefundConns:            2,
		CleanupBatch:           1_000,
		CleanupPipelines:       4,
		ReclaimBatch:           1_000,
	}
}

// drainDefault is refundDeadline + outageTolerance. A sum past int64 (a policy that fails
// RCP-8 anyway) becomes the largest whole-microsecond int64 rather than a negative wrap, so
// validation names RCP-8, not DrainTimeoutNs.
func drainDefault(refundDeadline, outageTolerance int64) int64 {
	const maxMicros = 9_223_372_036_854_775_000 // the largest multiple of 1,000 in int64
	if refundDeadline > 0 && outageTolerance > maxMicros-refundDeadline {
		return maxMicros
	}
	return refundDeadline + outageTolerance
}
