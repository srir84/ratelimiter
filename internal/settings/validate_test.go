package settings

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
)

// harnessPolicy is the DESIGN §B2 [policy] table, written out.
func harnessPolicy() Policy {
	return Policy{
		RateTokens:        10,
		RatePeriodNs:      1_000_000_000,
		Capacity:          20,
		FailureMode:       Closed,
		MaxTrackedBuckets: 100_000,
		MaxLiveMarkers:    50_000,
		MaxMarkersPerKey:  1_000,
		ReceiptLifetimeNs: 60_000_000_000,
		RefundDeadlineNs:  30_000_000_000,
		OutageToleranceNs: 5_000_000_000,
		CleanupIntervalNs: 5_000_000_000,
		ClockSource:       Store,
	}
}

// harnessInstance is the DESIGN §B2 [instance] table over the §A5.10 defaults, written out.
func harnessInstance() Instance {
	return Instance{
		MaxReleaseDelayNs:      5_000_000_000,
		MarginNs:               1_000_000_000,
		ClockRateBoundPPM:      1_000,
		DependencyDeadlineNs:   2_000_000_000,
		EpochRereadIntervalNs:  1_000_000_000,
		DrainTimeoutNs:         35_000_000_000,
		MaxHeldReceipts:        1152,
		MaxPendingRefunds:      1152,
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

func TestValidateHarnessValuesPass(t *testing.T) {
	// RCP-8: 5 + 30 + 5 + 1 = 41 s ≤ 60 s, and 1 s ≥ 60 s × 1,000 ppm = 60 ms.
	// §A5.6: 1 s + 1 s + (ceil(1,152 / 128) + 3) × 2 s = 1 + 1 + (9 + 3) × 2 = 26 s ≤ 30 s.
	if err := Validate(harnessPolicy(), harnessInstance(), false); err != nil {
		t.Fatalf("Validate(§B2 values) = %v, want nil", err)
	}
	if err := ValidatePolicy(harnessPolicy(), false); err != nil {
		t.Fatalf("ValidatePolicy(§B2 policy) = %v, want nil", err)
	}
}

// ruleCase changes the §B2 settings and names the field of the expected *Error ("" = pass).
// rule, when set, is the expected Rule text. policy marks the cases of policy rules, which
// ValidatePolicy must decide the same way.
type ruleCase struct {
	name   string
	edit   func(p *Policy, in *Instance)
	allow  bool
	field  string
	rule   string
	policy bool
}

// maxMicros is the largest multiple of 1,000 in int64.
const maxMicros = 9_223_372_036_854_775_000

func ruleCases() []ruleCase {
	pol := func(f func(p *Policy)) func(*Policy, *Instance) { return func(p *Policy, _ *Instance) { f(p) } }
	ins := func(f func(in *Instance)) func(*Policy, *Instance) { return func(_ *Policy, in *Instance) { f(in) } }
	cases := []ruleCase{
		// RateTokens in 1 … 2^31−1.
		{name: "RateTokens=2147483647", edit: pol(func(p *Policy) { p.RateTokens = 2_147_483_647 }), policy: true},
		{name: "RateTokens=2147483648", edit: pol(func(p *Policy) { p.RateTokens = 2_147_483_648 }), field: "RateTokens", policy: true},
		{name: "RateTokens=0", edit: pol(func(p *Policy) { p.RateTokens = 0 }), field: "RateTokens", policy: true},
		{name: "RateTokens=-1", edit: pol(func(p *Policy) { p.RateTokens = -1 }), field: "RateTokens", policy: true},
		// RatePeriodNs > 0 and a whole microsecond.
		{name: "RatePeriodNs=1000", edit: pol(func(p *Policy) { p.RatePeriodNs = 1_000 }), policy: true},
		{name: "RatePeriodNs=1500", edit: pol(func(p *Policy) { p.RatePeriodNs = 1_500 }), field: "RatePeriodNs", policy: true},
		{name: "RatePeriodNs=0", edit: pol(func(p *Policy) { p.RatePeriodNs = 0 }), field: "RatePeriodNs", policy: true},
		// Capacity ≥ 1 and cap_units = Capacity × RatePeriodNs < 2^52 = 4,503,599,627,370,496.
		{name: "Capacity=4503599@1s", edit: pol(func(p *Policy) { p.Capacity = 4_503_599 }), policy: true},
		{name: "Capacity=4503600@1s", edit: pol(func(p *Policy) { p.Capacity = 4_503_600 }), field: "Capacity", policy: true},
		{name: "Capacity=2^60@1000ns", edit: pol(func(p *Policy) { p.Capacity, p.RatePeriodNs = 1<<60, 1_000 }), field: "Capacity", policy: true},
		{name: "Capacity=MaxInt64@maxMicros", edit: pol(func(p *Policy) { p.Capacity, p.RatePeriodNs = math.MaxInt64, maxMicros }), field: "Capacity", policy: true},
		// 2^32 × (2^32 × 1,000) = 1,000 × 2^64: the low word is 0 < 2^52, so only the high
		// word shows the overflow.
		{name: "Capacity=2^32@2^32us", edit: pol(func(p *Policy) { p.Capacity, p.RatePeriodNs = 1<<32, (1<<32)*1_000 }), field: "Capacity", policy: true},
		{name: "Capacity=0", edit: pol(func(p *Policy) { p.Capacity = 0 }), field: "Capacity", policy: true},
		{name: "Capacity=1", edit: pol(func(p *Policy) { p.Capacity = 1 }), policy: true},
		// cap_units is checked before the microsecond rule: exactly 2^52 fails on Capacity,
		// 2^52 − 1 passes it and fails on RatePeriodNs.
		{name: "CapUnits=2^52", edit: pol(func(p *Policy) { p.Capacity, p.RatePeriodNs = 1, 1<<52 }), field: "Capacity", policy: true},
		{name: "CapUnits=2^52-1", edit: pol(func(p *Policy) { p.Capacity, p.RatePeriodNs = 1, 1<<52-1 }), field: "RatePeriodNs", policy: true},
		// ReceiptLifetimeNs > 0 and ReceiptLifetimeNs / 1000 < 2^40 (no clock-rate margin needed).
		{name: "Lifetime=(2^40-1)us", edit: func(p *Policy, in *Instance) {
			p.ReceiptLifetimeNs, in.ClockRateBoundPPM = (1<<40-1)*1_000, 0
		}, policy: true},
		{name: "Lifetime=2^40us", edit: func(p *Policy, in *Instance) {
			p.ReceiptLifetimeNs, in.ClockRateBoundPPM = (1<<40)*1_000, 0
		}, field: "ReceiptLifetimeNs", policy: true},
		{name: "Lifetime=0", edit: pol(func(p *Policy) { p.ReceiptLifetimeNs = 0 }), field: "ReceiptLifetimeNs", policy: true},
		// MaxMarkersPerKey > 0 and ≤ MaxLiveMarkers (50,000).
		{name: "MaxMarkersPerKey=50000", edit: pol(func(p *Policy) { p.MaxMarkersPerKey = 50_000 }), policy: true},
		{name: "MaxMarkersPerKey=50001", edit: pol(func(p *Policy) { p.MaxMarkersPerKey = 50_001 }), field: "MaxMarkersPerKey", policy: true},
		{name: "MaxMarkersPerKey=0", edit: pol(func(p *Policy) { p.MaxMarkersPerKey = 0 }), field: "MaxMarkersPerKey", policy: true},
		{name: "MaxTrackedBuckets=0", edit: pol(func(p *Policy) { p.MaxTrackedBuckets = 0 }), field: "MaxTrackedBuckets", policy: true},
		{name: "MaxLiveMarkers=0", edit: pol(func(p *Policy) { p.MaxLiveMarkers = 0 }), field: "MaxLiveMarkers", policy: true},
		{name: "CleanupIntervalNs=0", edit: pol(func(p *Policy) { p.CleanupIntervalNs = 0 }), field: "CleanupIntervalNs", policy: true},
		{name: "RefundDeadlineNs=-1", edit: pol(func(p *Policy) { p.RefundDeadlineNs = -1 }), field: "RefundDeadlineNs", policy: true},
		{name: "RefundDeadlineNs=-1000", edit: pol(func(p *Policy) { p.RefundDeadlineNs = -1_000 }), field: "RefundDeadlineNs", policy: true},
		{name: "OutageToleranceNs=0", edit: pol(func(p *Policy) { p.OutageToleranceNs = 0 }), policy: true},
		{name: "OutageToleranceNs=-1", edit: pol(func(p *Policy) { p.OutageToleranceNs = -1 }), field: "OutageToleranceNs", policy: true},
		{name: "OutageToleranceNs=-1000", edit: pol(func(p *Policy) { p.OutageToleranceNs = -1_000 }), field: "OutageToleranceNs", policy: true},
		// FailureMode and ClockSource (CMP-9).
		{name: "FailureMode=Open", edit: pol(func(p *Policy) { p.FailureMode = Open }), policy: true},
		{name: "FailureMode=2", edit: pol(func(p *Policy) { p.FailureMode = 2 }), field: "FailureMode", policy: true},
		{name: "FailureMode=-1", edit: pol(func(p *Policy) { p.FailureMode = -1 }), field: "FailureMode", policy: true},
		{name: "ClockSource=Controlled/production", edit: pol(func(p *Policy) { p.ClockSource = Controlled }), field: "ClockSource", policy: true},
		{name: "ClockSource=Controlled/test-only", edit: pol(func(p *Policy) { p.ClockSource = Controlled }), allow: true, policy: true},
		{name: "ClockSource=2", edit: pol(func(p *Policy) { p.ClockSource = 2 }), allow: true, field: "ClockSource", policy: true},

		// RCP-8: MaxReleaseDelayNs + RefundDeadlineNs + OutageToleranceNs + MarginNs ≤ lifetime.
		{name: "RCP8/Lifetime=41s", edit: pol(func(p *Policy) { p.ReceiptLifetimeNs = 41_000_000_000 })},
		{name: "RCP8/Lifetime=40999999000", edit: pol(func(p *Policy) { p.ReceiptLifetimeNs = 40_999_999_000 }), field: "ReceiptLifetimeNs"},
		{name: "RCP8/SumOverflows", edit: ins(func(in *Instance) { in.MaxReleaseDelayNs = maxMicros }), field: "ReceiptLifetimeNs"},
		// RCP-8: MarginNs × 1,000,000 ≥ lifetime × ClockRateBoundPPM (60 s × 1,000 ppm = 60 ms).
		{name: "RCP8/Margin=60000000", edit: ins(func(in *Instance) { in.MarginNs = 60_000_000 })},
		{name: "RCP8/Margin=59999000", edit: ins(func(in *Instance) { in.MarginNs = 59_999_000 }), field: "MarginNs"},
		{name: "RCP8/Margin=0", edit: ins(func(in *Instance) { in.MarginNs = 0 }), field: "MarginNs"},
		// MarginNs > 0 on its own: at 0 ppm the margin rule holds (0 ≥ 0), so only the
		// positivity rule can reject MarginNs 0.
		{name: "MarginNs=0@PPM=0", edit: ins(func(in *Instance) { in.MarginNs, in.ClockRateBoundPPM = 0, 0 }), field: "MarginNs", rule: "must be > 0"},
		{name: "RCP8/PPM=MaxInt64", edit: ins(func(in *Instance) { in.ClockRateBoundPPM = math.MaxInt64 }), field: "MarginNs"},
		// The 128-bit compare decides on the high words: MarginNs × 10^6 = 2 × 10^19 = 2^64 +
		// 1,553,255,926,290,448,384 (high word 1) ≥ 10^15 × 10,000 = 10^19 (high word 0),
		// although its low word is below 10^19. Sum: 5 + 30 + 5 + 20,000 = 20,040 s ≤
		// 1,000,000 s; lifetime / 1000 = 10^12 < 2^40.
		{name: "RCP8/MarginProductOver2^64", edit: func(p *Policy, in *Instance) {
			p.ReceiptLifetimeNs, in.MarginNs, in.ClockRateBoundPPM = 1_000_000_000_000_000, 20_000_000_000_000, 10_000
		}},
		// §A5.6: 1 s + 1 s + (ceil(1,152 / 128) + 3) × 2 s = 26 s ≤ RefundDeadlineNs.
		{name: "A56/RefundDeadline=26s", edit: pol(func(p *Policy) { p.RefundDeadlineNs = 26_000_000_000 })},
		{name: "A56/RefundDeadline=25999999000", edit: pol(func(p *Policy) { p.RefundDeadlineNs = 25_999_999_000 }), field: "RefundDeadlineNs"},
		{name: "A56/CeilRoundsUp", edit: func(p *Policy, in *Instance) { // ceil(1,153 / 128) = 10: 28 s > 26 s
			p.RefundDeadlineNs, in.MaxPendingRefunds = 26_000_000_000, 1_153
		}, field: "RefundDeadlineNs"},
		// (ceil((2^31 − 3) / 1) + 3) × 2^33 × 125 ns = 125 × 2^64: the low word is 0, so only
		// the high word shows the overflow (2^33 × 125 = 1,073,741,824,000, a whole µs).
		{name: "A56/ProductOverflows", edit: ins(func(in *Instance) {
			in.MaxPendingRefunds, in.RefundWorkers, in.DependencyDeadlineNs = 2_147_483_645, 1, 1_073_741_824_000
		}), field: "RefundDeadlineNs"},
		// The sum carries out of 64 bits. wait = (ceil((2^31 − 1) / 1) + 3) × 8,589,934,000 =
		// 2,147,483,650 × 8,589,934,000 = 18,446,742,819,579,100,000 = 2^64 − 1,254,130,451,616.
		// RefundBackoffMaxNs 1,254 s + the 1 s reconnect backoff = 1,255 s = RefundDeadlineNs,
		// so the partial sums stay within it; adding wait gives 2^64 + 869,548,384, which a
		// dropped carry would read as 869,548,384 ns ≤ 1,255 s. RCP-8: 5 + 1,255 + 5 + 2 =
		// 1,267 s ≤ 1,500 s, and 2 s × 10^6 = 2 × 10^15 ≥ 1,500 s × 1,000 = 1.5 × 10^15.
		{name: "A56/SumCarries", edit: func(p *Policy, in *Instance) {
			p.ReceiptLifetimeNs, p.RefundDeadlineNs, in.MarginNs = 1_500_000_000_000, 1_255_000_000_000, 2_000_000_000
			in.MaxPendingRefunds, in.RefundWorkers, in.DependencyDeadlineNs = 2_147_483_647, 1, 8_589_934_000
			in.RefundBackoffMaxNs = 1_254_000_000_000
		}, field: "RefundDeadlineNs"},

		// Zero is valid only for DrainTimeoutNs and ClockRateBoundPPM.
		{name: "DrainTimeoutNs=0", edit: ins(func(in *Instance) { in.DrainTimeoutNs = 0 })},
		{name: "DrainTimeoutNs=-1", edit: ins(func(in *Instance) { in.DrainTimeoutNs = -1 }), field: "DrainTimeoutNs"},
		{name: "DrainTimeoutNs=-1000", edit: ins(func(in *Instance) { in.DrainTimeoutNs = -1_000 }), field: "DrainTimeoutNs"},
		{name: "ClockRateBoundPPM=0", edit: ins(func(in *Instance) { in.ClockRateBoundPPM = 0 })},
		{name: "ClockRateBoundPPM=-1", edit: ins(func(in *Instance) { in.ClockRateBoundPPM = -1 }), field: "ClockRateBoundPPM"},
	}

	// Each required or positive instance field at 0 fails, naming that field.
	for _, z := range []struct {
		field string
		zero  func(in *Instance)
	}{
		{"MaxReleaseDelayNs", func(in *Instance) { in.MaxReleaseDelayNs = 0 }},
		{"MarginNs", func(in *Instance) { in.MarginNs = 0 }},
		{"DependencyDeadlineNs", func(in *Instance) { in.DependencyDeadlineNs = 0 }},
		{"EpochRereadIntervalNs", func(in *Instance) { in.EpochRereadIntervalNs = 0 }},
		{"MaxHeldReceipts", func(in *Instance) { in.MaxHeldReceipts = 0 }},
		{"MaxPendingRefunds", func(in *Instance) { in.MaxPendingRefunds = 0 }},
		{"ListenerQueueSize", func(in *Instance) { in.ListenerQueueSize = 0 }},
		{"RefundWorkers", func(in *Instance) { in.RefundWorkers = 0 }},
		{"RefundBackoffInitialNs", func(in *Instance) { in.RefundBackoffInitialNs = 0 }},
		{"RefundBackoffMaxNs", func(in *Instance) { in.RefundBackoffMaxNs = 0 }},
		{"StoreConns", func(in *Instance) { in.StoreConns = 0 }},
		{"RefundConns", func(in *Instance) { in.RefundConns = 0 }},
		{"CleanupBatch", func(in *Instance) { in.CleanupBatch = 0 }},
		{"CleanupPipelines", func(in *Instance) { in.CleanupPipelines = 0 }},
		{"ReclaimBatch", func(in *Instance) { in.ReclaimBatch = 0 }},
	} {
		cases = append(cases, ruleCase{name: z.field + "=0", edit: ins(z.zero), field: z.field})
	}

	// Every count is in 1 … 2^31−1 like RateTokens (owner decision, 2026-10-05): 2^31−1
	// passes and 2^31 fails on that field's count rule. MaxMarkersPerKey runs with
	// MaxLiveMarkers 2^31−1, so 2^31−1 meets ≤ MaxLiveMarkers, and at 2^31 the expected Rule
	// tells the count rule from that rule, which names the same field. MaxPendingRefunds runs
	// with RefundWorkers 2^31−1, which keeps §A5.6 at 1 + 1 + (1 + 3) × 2 = 10 s ≤ 30 s.
	for _, c := range []struct {
		field  string
		policy bool
		set    func(p *Policy, in *Instance, v int64)
	}{
		{"MaxTrackedBuckets", true, func(p *Policy, _ *Instance, v int64) { p.MaxTrackedBuckets = v }},
		{"MaxLiveMarkers", true, func(p *Policy, _ *Instance, v int64) { p.MaxLiveMarkers = v }},
		{"MaxMarkersPerKey", true, func(p *Policy, _ *Instance, v int64) { p.MaxLiveMarkers, p.MaxMarkersPerKey = 2_147_483_647, v }},
		{"MaxHeldReceipts", false, func(_ *Policy, in *Instance, v int64) { in.MaxHeldReceipts = int(v) }},
		{"MaxPendingRefunds", false, func(_ *Policy, in *Instance, v int64) {
			in.MaxPendingRefunds, in.RefundWorkers = int(v), 2_147_483_647
		}},
		{"ListenerQueueSize", false, func(_ *Policy, in *Instance, v int64) { in.ListenerQueueSize = int(v) }},
		{"RefundWorkers", false, func(_ *Policy, in *Instance, v int64) { in.RefundWorkers = int(v) }},
		{"StoreConns", false, func(_ *Policy, in *Instance, v int64) { in.StoreConns = int(v) }},
		{"RefundConns", false, func(_ *Policy, in *Instance, v int64) { in.RefundConns = int(v) }},
		{"CleanupBatch", false, func(_ *Policy, in *Instance, v int64) { in.CleanupBatch = int(v) }},
		{"CleanupPipelines", false, func(_ *Policy, in *Instance, v int64) { in.CleanupPipelines = int(v) }},
		{"ReclaimBatch", false, func(_ *Policy, in *Instance, v int64) { in.ReclaimBatch = int(v) }},
	} {
		cases = append(cases,
			ruleCase{name: c.field + "=2147483647", edit: func(p *Policy, in *Instance) { c.set(p, in, 2_147_483_647) }, policy: c.policy},
			ruleCase{
				name: c.field + "=2147483648", edit: func(p *Policy, in *Instance) { c.set(p, in, 2_147_483_648) },
				field: c.field, rule: "must be in 1 … 2^31−1", policy: c.policy,
			})
	}

	// Every *Ns field 1 ns off a whole microsecond fails, naming that field.
	for _, f := range []struct {
		field string
		bump  func(p *Policy, in *Instance)
		pol   bool
	}{
		{"RatePeriodNs", func(p *Policy, _ *Instance) { p.RatePeriodNs++ }, true},
		{"ReceiptLifetimeNs", func(p *Policy, _ *Instance) { p.ReceiptLifetimeNs++ }, true},
		{"RefundDeadlineNs", func(p *Policy, _ *Instance) { p.RefundDeadlineNs++ }, true},
		{"OutageToleranceNs", func(p *Policy, _ *Instance) { p.OutageToleranceNs++ }, true},
		{"CleanupIntervalNs", func(p *Policy, _ *Instance) { p.CleanupIntervalNs++ }, true},
		{"MaxReleaseDelayNs", func(_ *Policy, in *Instance) { in.MaxReleaseDelayNs++ }, false},
		{"MarginNs", func(_ *Policy, in *Instance) { in.MarginNs++ }, false},
		{"DependencyDeadlineNs", func(_ *Policy, in *Instance) { in.DependencyDeadlineNs++ }, false},
		{"EpochRereadIntervalNs", func(_ *Policy, in *Instance) { in.EpochRereadIntervalNs++ }, false},
		{"DrainTimeoutNs", func(_ *Policy, in *Instance) { in.DrainTimeoutNs++ }, false},
		{"RefundBackoffInitialNs", func(_ *Policy, in *Instance) { in.RefundBackoffInitialNs++ }, false},
		{"RefundBackoffMaxNs", func(_ *Policy, in *Instance) { in.RefundBackoffMaxNs++ }, false},
	} {
		cases = append(cases, ruleCase{name: f.field + "+1ns", edit: f.bump, field: f.field, policy: f.pol})
	}
	return cases
}

func TestValidateRules(t *testing.T) {
	for _, c := range ruleCases() {
		t.Run(c.name, func(t *testing.T) {
			p, in := harnessPolicy(), harnessInstance()
			c.edit(&p, &in)
			err := Validate(p, in, c.allow)
			if c.field == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			se := asError(t, err)
			if se.Field != c.field {
				t.Fatalf("Validate = %v: field %q, want %q", err, se.Field, c.field)
			}
			if c.rule != "" && se.Rule != c.rule {
				t.Fatalf("Validate = %v: rule %q, want %q", err, se.Rule, c.rule)
			}
		})
	}
}

func TestValidatePolicy(t *testing.T) {
	if err := ValidatePolicy(harnessPolicy(), false); err != nil {
		t.Fatalf("ValidatePolicy(§B2 policy, false) = %v, want nil", err)
	}
	n := 0
	for _, c := range ruleCases() {
		if !c.policy {
			continue
		}
		n++
		t.Run(c.name, func(t *testing.T) {
			p, in := harnessPolicy(), harnessInstance()
			c.edit(&p, &in)
			perr, err := ValidatePolicy(p, c.allow), Validate(p, in, c.allow)
			if (perr == nil) != (err == nil) {
				t.Fatalf("ValidatePolicy = %v but Validate = %v", perr, err)
			}
			if perr == nil {
				return
			}
			pe, ve := asError(t, perr), asError(t, err)
			if pe != ve || pe.Field != c.field {
				t.Fatalf("ValidatePolicy = %+v, Validate = %+v, want both with field %q", pe, ve, c.field)
			}
			if c.rule != "" && pe.Rule != c.rule {
				t.Fatalf("ValidatePolicy = %+v, want rule %q", pe, c.rule)
			}
		})
	}
	if n < 30 {
		t.Errorf("only %d policy cases ran", n)
	}

	// RefundDeadlineNs 0 meets its policy rule (≥ 0); only the instance's §A5.6 rule fails.
	p, in := harnessPolicy(), harnessInstance()
	p.RefundDeadlineNs = 0
	if err := ValidatePolicy(p, false); err != nil {
		t.Errorf("ValidatePolicy(RefundDeadlineNs 0) = %v, want nil", err)
	}
	if se := asError(t, Validate(p, in, false)); se.Field != "RefundDeadlineNs" || !strings.Contains(se.Rule, "§A5.6") {
		t.Errorf("Validate(RefundDeadlineNs 0) = %+v, want the §A5.6 rule", se)
	}

	// One policy rule and one instance rule fail: Validate reports the policy rule.
	p, in = harnessPolicy(), harnessInstance()
	p.RateTokens, in.MaxHeldReceipts = 0, 0
	pe, ve := asError(t, ValidatePolicy(p, false)), asError(t, Validate(p, in, false))
	if pe != ve || ve.Field != "RateTokens" {
		t.Errorf("policy and instance rule failing: Validate = %+v, ValidatePolicy = %+v, want RateTokens from both", ve, pe)
	}
}

// refValid decides every rule independently, with math/big for every sum and product.
func refValid(p Policy, in Instance, allow bool) bool {
	b := big.NewInt
	mul := func(x, y int64) *big.Int { return new(big.Int).Mul(b(x), b(y)) }
	sum := func(xs ...int64) *big.Int {
		s := new(big.Int)
		for _, x := range xs {
			s.Add(s, b(x))
		}
		return s
	}
	for _, v := range []int64{p.RatePeriodNs, p.ReceiptLifetimeNs, p.RefundDeadlineNs, p.OutageToleranceNs, p.CleanupIntervalNs,
		in.MaxReleaseDelayNs, in.MarginNs, in.DependencyDeadlineNs, in.EpochRereadIntervalNs, in.DrainTimeoutNs,
		in.RefundBackoffInitialNs, in.RefundBackoffMaxNs} {
		if v%1000 != 0 {
			return false
		}
	}
	for _, v := range []int64{p.RatePeriodNs, p.Capacity, p.MaxTrackedBuckets, p.MaxLiveMarkers, p.MaxMarkersPerKey,
		p.CleanupIntervalNs, p.ReceiptLifetimeNs, in.MaxReleaseDelayNs, in.MarginNs, in.DependencyDeadlineNs,
		in.EpochRereadIntervalNs, int64(in.MaxHeldReceipts), int64(in.MaxPendingRefunds), int64(in.ListenerQueueSize),
		int64(in.RefundWorkers), in.RefundBackoffInitialNs, in.RefundBackoffMaxNs, int64(in.StoreConns),
		int64(in.RefundConns), int64(in.CleanupBatch), int64(in.CleanupPipelines), int64(in.ReclaimBatch)} {
		if v <= 0 {
			return false
		}
	}
	// Every count is ≤ 2^31−1 (owner decision, 2026-10-05).
	for _, v := range []int64{p.MaxTrackedBuckets, p.MaxLiveMarkers, p.MaxMarkersPerKey, int64(in.MaxHeldReceipts),
		int64(in.MaxPendingRefunds), int64(in.ListenerQueueSize), int64(in.RefundWorkers), int64(in.StoreConns),
		int64(in.RefundConns), int64(in.CleanupBatch), int64(in.CleanupPipelines), int64(in.ReclaimBatch)} {
		if v > 1<<31-1 {
			return false
		}
	}
	two := func(n uint) *big.Int { return new(big.Int).Lsh(b(1), n) }
	switch {
	case p.RateTokens < 1 || p.RateTokens > 1<<31-1,
		mul(p.Capacity, p.RatePeriodNs).Cmp(two(52)) >= 0,
		p.FailureMode != Closed && p.FailureMode != Open,
		p.ClockSource != Store && (p.ClockSource != Controlled || !allow),
		p.MaxMarkersPerKey > p.MaxLiveMarkers,
		mul(two(40).Int64(), 1000).Cmp(b(p.ReceiptLifetimeNs)) <= 0,
		p.RefundDeadlineNs < 0, p.OutageToleranceNs < 0, in.DrainTimeoutNs < 0, in.ClockRateBoundPPM < 0,
		sum(in.MaxReleaseDelayNs, p.RefundDeadlineNs, p.OutageToleranceNs, in.MarginNs).Cmp(b(p.ReceiptLifetimeNs)) > 0,
		mul(in.MarginNs, 1_000_000).Cmp(mul(p.ReceiptLifetimeNs, in.ClockRateBoundPPM)) < 0:
		return false
	}
	n, w := b(int64(in.MaxPendingRefunds)), b(int64(in.RefundWorkers))
	rounds := new(big.Int).Div(new(big.Int).Add(n, new(big.Int).Sub(w, b(1))), w) // ceil(N / W), N, W > 0
	wait := new(big.Int).Mul(rounds.Add(rounds, b(3)), b(in.DependencyDeadlineNs))
	total := sum(in.RefundBackoffMaxNs, 1_000_000_000)
	return total.Add(total, wait).Cmp(b(p.RefundDeadlineNs)) <= 0
}

func FuzzValidate(f *testing.F) {
	add := func(p Policy, in Instance, allow bool) {
		f.Add(p.RateTokens, p.RatePeriodNs, p.Capacity, int64(p.FailureMode), p.MaxTrackedBuckets, p.MaxLiveMarkers,
			p.MaxMarkersPerKey, p.ReceiptLifetimeNs, p.RefundDeadlineNs, p.OutageToleranceNs, p.CleanupIntervalNs,
			int64(p.ClockSource), in.MaxReleaseDelayNs, in.MarginNs, in.ClockRateBoundPPM, in.DependencyDeadlineNs,
			in.EpochRereadIntervalNs, in.DrainTimeoutNs, int64(in.MaxHeldReceipts), int64(in.MaxPendingRefunds),
			int64(in.ListenerQueueSize), int64(in.RefundWorkers), in.RefundBackoffInitialNs, in.RefundBackoffMaxNs,
			int64(in.StoreConns), int64(in.RefundConns), int64(in.CleanupBatch), int64(in.CleanupPipelines),
			int64(in.ReclaimBatch), allow)
	}
	add(harnessPolicy(), harnessInstance(), false)
	for _, c := range ruleCases() {
		p, in := harnessPolicy(), harnessInstance()
		c.edit(&p, &in)
		add(p, in, c.allow)
	}
	p, in := harnessPolicy(), harnessInstance()
	p.Capacity, p.RatePeriodNs, p.ReceiptLifetimeNs, p.RefundDeadlineNs = math.MaxInt64, math.MaxInt64, math.MinInt64, math.MaxInt64
	in.MarginNs, in.ClockRateBoundPPM, in.MaxPendingRefunds = math.MaxInt64, math.MaxInt64, math.MaxInt
	add(p, in, true)

	f.Fuzz(func(t *testing.T, rateTokens, ratePeriod, capacity, failureMode, maxTracked, maxLive, maxPerKey,
		lifetime, refundDeadline, outage, cleanup, clock, maxRelease, margin, ppm, dependency, reread, drain,
		held, pending, queue, workers, backoffInitial, backoffMax, storeConns, refundConns, cleanupBatch,
		pipelines, reclaim int64, allow bool,
	) {
		p := Policy{
			RateTokens: rateTokens, RatePeriodNs: ratePeriod, Capacity: capacity, FailureMode: FailureMode(failureMode),
			MaxTrackedBuckets: maxTracked, MaxLiveMarkers: maxLive, MaxMarkersPerKey: maxPerKey,
			ReceiptLifetimeNs: lifetime, RefundDeadlineNs: refundDeadline, OutageToleranceNs: outage,
			CleanupIntervalNs: cleanup, ClockSource: ClockSource(clock),
		}
		in := Instance{
			MaxReleaseDelayNs: maxRelease, MarginNs: margin, ClockRateBoundPPM: ppm, DependencyDeadlineNs: dependency,
			EpochRereadIntervalNs: reread, DrainTimeoutNs: drain, MaxHeldReceipts: int(held),
			MaxPendingRefunds: int(pending), ListenerQueueSize: int(queue), RefundWorkers: int(workers),
			RefundBackoffInitialNs: backoffInitial, RefundBackoffMaxNs: backoffMax, StoreConns: int(storeConns),
			RefundConns: int(refundConns), CleanupBatch: int(cleanupBatch), CleanupPipelines: int(pipelines),
			ReclaimBatch: int(reclaim),
		}
		err := Validate(p, in, allow)
		if want := refValid(p, in, allow); (err == nil) != want {
			t.Fatalf("Validate(%+v, %+v, %v) = %v, but the math/big check says valid=%v", p, in, allow, err, want)
		}
		if err != nil {
			asError(t, err)
		}
		if perr := ValidatePolicy(p, allow); perr != nil {
			if pe, ve := asError(t, perr), asError(t, err); pe != ve {
				t.Fatalf("ValidatePolicy = %+v but Validate = %+v", pe, ve)
			}
		}
	})
}

func TestDerivedBounds(t *testing.T) {
	// The largest valid settings for the §A3.1 derived times: one token per period, the
	// largest cap_units (a whole number of microsecond periods, below 2^52) and the largest
	// lifetime, which needs ClockRateBoundPPM 0 to keep RCP-8's margin at 1 s.
	p, in := harnessPolicy(), harnessInstance()
	p.RateTokens, p.RatePeriodNs, p.Capacity = 1, 1_000, 4_503_599_627_370 // cap_units 4,503,599,627,370,000
	p.ReceiptLifetimeNs, in.ClockRateBoundPPM = (1<<40-1)*1_000, 0
	if err := Validate(p, in, false); err != nil {
		t.Fatalf("largest settings: Validate = %v, want nil", err)
	}
	more := p
	more.Capacity++
	if se := asError(t, Validate(more, in, false)); se.Field != "Capacity" {
		t.Errorf("one more token of capacity: field %q, want Capacity", se.Field)
	}
	more = p
	more.ReceiptLifetimeNs += 1_000
	if se := asError(t, Validate(more, in, false)); se.Field != "ReceiptLifetimeNs" {
		t.Errorf("one more µs of lifetime: field %q, want ReceiptLifetimeNs", se.Field)
	}

	two := func(n uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), n) }
	tbound := new(big.Int).Sub(two(53), two(43)) // 2^53 − 2^43
	lifetimeUs := big.NewInt(p.ReceiptLifetimeNs / 1_000)
	latest := new(big.Int).Add(new(big.Int).Sub(tbound, big.NewInt(1)), lifetimeUs)
	if latest.Cmp(two(53)) >= 0 {
		t.Errorf("TBOUND − 1 + lifetime_us = %v, want < 2^53", latest)
	}
	ceil := func(a, d *big.Int) *big.Int {
		q, r := new(big.Int).QuoRem(a, d, new(big.Int))
		if r.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
		return q
	}
	capUnits := new(big.Int).Mul(big.NewInt(p.Capacity), big.NewInt(p.RatePeriodNs))
	perUs := new(big.Int).Mul(big.NewInt(p.RateTokens), big.NewInt(1_000))
	if q := ceil(capUnits, perUs); q.Cmp(two(43)) >= 0 {
		t.Errorf("ceil(cap_units / (rate_tokens × 1000)) = %v, want < 2^43", q)
	}
	// No valid setting does worse: cap_units < 2^52 and rate_tokens ≥ 1.
	if q := ceil(new(big.Int).Sub(two(52), big.NewInt(1)), big.NewInt(1_000)); q.Cmp(two(43)) >= 0 {
		t.Errorf("worst case ceil((2^52 − 1) / 1000) = %v, want < 2^43", q)
	}
}

// TestErrorText keeps the message readable and free of setting values.
func TestErrorText(t *testing.T) {
	p := harnessPolicy()
	p.Capacity = 4_503_600
	err := Validate(p, harnessInstance(), false)
	want := "invalid setting Capacity: Capacity × RatePeriodNs must be < 2^52 (§A3.1)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a *Error", err)
	}
}
