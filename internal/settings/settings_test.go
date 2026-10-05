package settings

import (
	"errors"
	"math"
	"testing"
)

func TestDefaults(t *testing.T) {
	// SPEC CFG-8 and DESIGN §A5.2/§A5.10 defaults; required fields stay 0.
	wantPolicy := Policy{
		RateTokens:        10,
		RatePeriodNs:      1_000_000_000,
		Capacity:          20,
		FailureMode:       Closed,
		MaxTrackedBuckets: 0,
		MaxLiveMarkers:    0,
		MaxMarkersPerKey:  1_000,
		ReceiptLifetimeNs: 60_000_000_000,
		RefundDeadlineNs:  30_000_000_000,
		OutageToleranceNs: 5_000_000_000,
		CleanupIntervalNs: 5_000_000_000,
		ClockSource:       Store,
	}
	if got := DefaultPolicy(); got != wantPolicy {
		t.Errorf("DefaultPolicy() = %+v\nwant %+v", got, wantPolicy)
	}
	wantInstance := Instance{
		MaxReleaseDelayNs:      0,
		MarginNs:               0,
		ClockRateBoundPPM:      1_000,
		DependencyDeadlineNs:   2_000_000_000,
		EpochRereadIntervalNs:  1_000_000_000,
		DrainTimeoutNs:         35_000_000_000,
		MaxHeldReceipts:        0,
		MaxPendingRefunds:      0,
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
	if got := DefaultInstance(DefaultPolicy()); got != wantInstance {
		t.Errorf("DefaultInstance(DefaultPolicy()) = %+v\nwant %+v", got, wantInstance)
	}
	if got := DefaultInstance(DefaultPolicy()).DrainTimeoutNs; got != 35_000_000_000 {
		t.Errorf("DefaultInstance(DefaultPolicy()).DrainTimeoutNs = %d, want 35,000,000,000", got)
	}
	if ReconnectBackoffMaxNs != 1_000_000_000 {
		t.Errorf("ReconnectBackoffMaxNs = %d, want 1 s", ReconnectBackoffMaxNs)
	}

	// The drain default follows the policy it is given.
	p := DefaultPolicy()
	p.RefundDeadlineNs, p.OutageToleranceNs = 3_000_000_000, 500_000_000
	if got := DefaultInstance(p).DrainTimeoutNs; got != 3_500_000_000 {
		t.Errorf("DrainTimeoutNs for 3 s + 0.5 s = %d, want 3,500,000,000", got)
	}

	// The required fields fail validation while 0, the first of them first.
	err := Validate(DefaultPolicy(), DefaultInstance(DefaultPolicy()), false)
	if se := asError(t, err); se.Field != "MaxTrackedBuckets" {
		t.Errorf("defaults alone: failing field %q, want MaxTrackedBuckets", se.Field)
	}

	// A drain default past int64 does not wrap negative: it saturates at the largest whole
	// microsecond, and validation reports the RCP-8 bound it breaks.
	p = harnessPolicy()
	p.RefundDeadlineNs, p.OutageToleranceNs = 4_611_686_018_427_388_000, 4_611_686_018_427_388_000
	in := DefaultInstance(p)
	if in.DrainTimeoutNs != 9_223_372_036_854_775_000 {
		t.Errorf("overflowing drain default = %d, want 9,223,372,036,854,775,000", in.DrainTimeoutNs)
	}
	in.MaxReleaseDelayNs, in.MarginNs, in.MaxHeldReceipts, in.MaxPendingRefunds = 5_000_000_000, 1_000_000_000, 1152, 1152
	if se := asError(t, Validate(p, in, false)); se.Field != "ReceiptLifetimeNs" {
		t.Errorf("overflowing drain default: failing field %q, want ReceiptLifetimeNs (RCP-8)", se.Field)
	}
	// A sum that fits stays the plain sum.
	p.RefundDeadlineNs, p.OutageToleranceNs = 0, math.MaxInt64
	if got := DefaultInstance(p).DrainTimeoutNs; got != math.MaxInt64 {
		t.Errorf("drain default for 0 + MaxInt64 = %d, want MaxInt64", got)
	}
}

func TestEnumNames(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{Closed.String(), "closed"},
		{Open.String(), "open"},
		{FailureMode(2).String(), "FailureMode(2)"},
		{Store.String(), "store"},
		{Controlled.String(), "controlled"},
		{ClockSource(-1).String(), "ClockSource(-1)"},
	} {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
	if Closed != 0 || Store != 0 {
		t.Errorf("zero values: Closed = %d, Store = %d; both must be 0 so a zero Policy is closed and store-clocked", Closed, Store)
	}
}

// asError returns the *Error err holds, failing the test when err is nil, another type,
// or has an empty Field or Rule.
func asError(t *testing.T, err error) Error {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("got error %v, want a *settings.Error", err)
	}
	if se.Field == "" || se.Rule == "" {
		t.Fatalf("*Error %+v has an empty Field or Rule", *se)
	}
	return *se
}
