package settings

import (
	"math"
	"math/bits"
)

// Error names the first settings rule that failed. Field is the Go field name the rule
// bounds; Rule says what it must satisfy. Neither holds a setting's value.
type Error struct {
	Field string
	Rule  string
}

func (e *Error) Error() string { return "invalid setting " + e.Field + ": " + e.Rule }

func fail(field, rule string) error { return &Error{Field: field, Rule: rule} }

// Rule texts shared by several fields.
const (
	ruleMicros   = "must be a multiple of 1,000 ns (whole microseconds, §A3.1)"
	rulePositive = "must be > 0"
	ruleNonNeg   = "must be ≥ 0"
	ruleCount    = "must be in 1 … 2^31−1"
)

// Bounds from DESIGN §A3.1.
const (
	capUnitsLimit = 1 << 52 // Capacity × RatePeriodNs < 2^52
	lifetimeLimit = 1 << 40 // ReceiptLifetimeNs / 1000 < 2^40
)

// countMax bounds every count setting to 1 … 2^31−1, the RateTokens range (an owner
// decision added to the plan's rule list, 2026-10-05). Counts become script ARGV values,
// which stay below 2^53 (§A3.1), and queue capacities and worker counts.
const countMax = math.MaxInt32

// namedValue is one setting in a rule that loops over several fields.
type namedValue struct {
	name string
	v    int64
}

// positiveValue is one setting of the instance's > 0 rule; a count must also be ≤ countMax.
type positiveValue struct {
	name  string
	v     int64
	count bool
}

// ValidatePolicy checks the policy-only rules (CFG-4, CFG-8, §A3.1, §A5.10, CMP-9) in a
// fixed order and returns the first failure as *Error. allowControlled is true only for
// the test-only entry point and rladmin. It never panics: every product is computed
// exactly in 128 bits.
func ValidatePolicy(p Policy, allowControlled bool) error {
	if p.RateTokens < 1 || p.RateTokens > math.MaxInt32 {
		return fail("RateTokens", "must be in 1 … 2^31−1 (§A3.1)")
	}
	if p.RatePeriodNs <= 0 {
		return fail("RatePeriodNs", rulePositive)
	}
	if p.Capacity < 1 {
		return fail("Capacity", "must be ≥ 1")
	}
	if hi, lo := bits.Mul64(u64(p.Capacity), u64(p.RatePeriodNs)); hi != 0 || lo >= capUnitsLimit {
		return fail("Capacity", "Capacity × RatePeriodNs must be < 2^52 (§A3.1)")
	}
	for _, f := range []namedValue{
		{"RatePeriodNs", p.RatePeriodNs},
		{"ReceiptLifetimeNs", p.ReceiptLifetimeNs},
		{"RefundDeadlineNs", p.RefundDeadlineNs},
		{"OutageToleranceNs", p.OutageToleranceNs},
		{"CleanupIntervalNs", p.CleanupIntervalNs},
	} {
		if f.v%1000 != 0 {
			return fail(f.name, ruleMicros)
		}
	}
	switch p.FailureMode {
	case Closed, Open:
	default:
		return fail("FailureMode", "must be Closed or Open")
	}
	switch p.ClockSource {
	case Store:
	case Controlled:
		if !allowControlled {
			return fail("ClockSource", "Controlled is accepted only through the test-only entry point (CMP-9)")
		}
	default:
		return fail("ClockSource", "must be Store or Controlled")
	}
	for _, f := range []namedValue{
		{"MaxTrackedBuckets", p.MaxTrackedBuckets},
		{"MaxLiveMarkers", p.MaxLiveMarkers},
		{"MaxMarkersPerKey", p.MaxMarkersPerKey},
	} {
		if f.v <= 0 {
			return fail(f.name, rulePositive)
		}
		if f.v > countMax {
			return fail(f.name, ruleCount)
		}
	}
	if p.MaxMarkersPerKey > p.MaxLiveMarkers {
		return fail("MaxMarkersPerKey", "must be ≤ MaxLiveMarkers (§A5.10)")
	}
	if p.CleanupIntervalNs <= 0 {
		return fail("CleanupIntervalNs", rulePositive)
	}
	if p.ReceiptLifetimeNs <= 0 {
		return fail("ReceiptLifetimeNs", rulePositive)
	}
	if p.ReceiptLifetimeNs/1000 >= lifetimeLimit {
		return fail("ReceiptLifetimeNs", "ReceiptLifetimeNs / 1000 must be < 2^40 (§A3.1)")
	}
	if p.RefundDeadlineNs < 0 {
		return fail("RefundDeadlineNs", ruleNonNeg)
	}
	if p.OutageToleranceNs < 0 {
		return fail("OutageToleranceNs", ruleNonNeg)
	}
	return nil
}

// Validate checks every rule: ValidatePolicy first, then the instance rules (CFG-8,
// §A5.10, RCP-8, §A5.6) in a fixed order, and returns the first failure as *Error. An
// instance whose settings fail never becomes ready (settings_invalid). It never panics:
// sums and products are computed exactly in 128 bits.
func Validate(p Policy, in Instance, allowControlled bool) error {
	if err := ValidatePolicy(p, allowControlled); err != nil {
		return err
	}
	for _, f := range []namedValue{
		{"MaxReleaseDelayNs", in.MaxReleaseDelayNs},
		{"MarginNs", in.MarginNs},
		{"DependencyDeadlineNs", in.DependencyDeadlineNs},
		{"EpochRereadIntervalNs", in.EpochRereadIntervalNs},
		{"DrainTimeoutNs", in.DrainTimeoutNs},
		{"RefundBackoffInitialNs", in.RefundBackoffInitialNs},
		{"RefundBackoffMaxNs", in.RefundBackoffMaxNs},
	} {
		if f.v%1000 != 0 {
			return fail(f.name, ruleMicros)
		}
	}
	for _, f := range []positiveValue{
		{"MaxReleaseDelayNs", in.MaxReleaseDelayNs, false},
		{"MarginNs", in.MarginNs, false},
		{"DependencyDeadlineNs", in.DependencyDeadlineNs, false},
		{"EpochRereadIntervalNs", in.EpochRereadIntervalNs, false},
		{"MaxHeldReceipts", int64(in.MaxHeldReceipts), true},
		{"MaxPendingRefunds", int64(in.MaxPendingRefunds), true},
		{"ListenerQueueSize", int64(in.ListenerQueueSize), true},
		{"RefundWorkers", int64(in.RefundWorkers), true},
		{"RefundBackoffInitialNs", in.RefundBackoffInitialNs, false},
		{"RefundBackoffMaxNs", in.RefundBackoffMaxNs, false},
		{"StoreConns", int64(in.StoreConns), true},
		{"RefundConns", int64(in.RefundConns), true},
		{"CleanupBatch", int64(in.CleanupBatch), true},
		{"CleanupPipelines", int64(in.CleanupPipelines), true},
		{"ReclaimBatch", int64(in.ReclaimBatch), true},
	} {
		if f.v <= 0 {
			return fail(f.name, rulePositive)
		}
		if f.count && f.v > countMax {
			return fail(f.name, ruleCount)
		}
	}
	if in.DrainTimeoutNs < 0 {
		return fail("DrainTimeoutNs", ruleNonNeg)
	}
	if in.ClockRateBoundPPM < 0 {
		return fail("ClockRateBoundPPM", ruleNonNeg)
	}
	// RCP-8. Every term is ≥ 0 here: the policy and positivity rules above passed.
	if !sumAtMost(u64(p.ReceiptLifetimeNs),
		u64(in.MaxReleaseDelayNs), u64(p.RefundDeadlineNs), u64(p.OutageToleranceNs), u64(in.MarginNs)) {
		return fail("ReceiptLifetimeNs", "MaxReleaseDelayNs + RefundDeadlineNs + OutageToleranceNs + MarginNs must be ≤ ReceiptLifetimeNs (RCP-8)")
	}
	mHi, mLo := bits.Mul64(u64(in.MarginNs), 1_000_000)
	lHi, lLo := bits.Mul64(u64(p.ReceiptLifetimeNs), u64(in.ClockRateBoundPPM))
	if mHi < lHi || mHi == lHi && mLo < lLo {
		return fail("MarginNs", "MarginNs × 1,000,000 must be ≥ ReceiptLifetimeNs × ClockRateBoundPPM (RCP-8)")
	}
	// §A5.6 (REF-5 by construction): N and W are > 0 here.
	n, w := u64(int64(in.MaxPendingRefunds)), u64(int64(in.RefundWorkers))
	if w == 0 { // never true after the > 0 rule; keeps n / w from dividing by zero
		return fail("RefundWorkers", rulePositive)
	}
	rounds := n / w // ceil(N / W), then + 3: below 2^63 + 4, so no wrap
	if n%w != 0 {
		rounds++
	}
	waitHi, wait := bits.Mul64(rounds+3, u64(in.DependencyDeadlineNs))
	if waitHi != 0 || !sumAtMost(u64(p.RefundDeadlineNs), u64(in.RefundBackoffMaxNs), ReconnectBackoffMaxNs, wait) {
		return fail("RefundDeadlineNs", "RefundBackoffMaxNs + 1 s reconnect backoff + (ceil(MaxPendingRefunds / RefundWorkers) + 3) × DependencyDeadlineNs must be ≤ RefundDeadlineNs (§A5.6)")
	}
	return nil
}

// sumAtMost reports whether the exact sum of terms is ≤ limit. The partial sums only grow,
// so a carry out of 64 bits or a partial sum above limit decides false.
func sumAtMost(limit uint64, terms ...uint64) bool {
	var acc uint64
	for _, t := range terms {
		var carry uint64
		acc, carry = bits.Add64(acc, t, 0)
		if carry != 0 || acc > limit {
			return false
		}
	}
	return true
}

// u64 converts a value the caller has already checked to be ≥ 0; a negative value (never
// passed) becomes 0 instead of wrapping to a huge number.
func u64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
