package fingerprint

import (
	"reflect"
	"testing"

	"ratelimiter/internal/settings"
)

// harnessPolicy is the DESIGN §B2 [policy] table, written out.
func harnessPolicy() settings.Policy {
	return settings.Policy{
		RateTokens:        10,
		RatePeriodNs:      1_000_000_000,
		Capacity:          20,
		FailureMode:       settings.Closed,
		MaxTrackedBuckets: 100_000,
		MaxLiveMarkers:    50_000,
		MaxMarkersPerKey:  1_000,
		ReceiptLifetimeNs: 60_000_000_000,
		RefundDeadlineNs:  30_000_000_000,
		OutageToleranceNs: 5_000_000_000,
		CleanupIntervalNs: 5_000_000_000,
		ClockSource:       settings.Store,
	}
}

func TestFingerprintGolden(t *testing.T) {
	// The 14 sorted lines; `shasum -a 256 | cut -c1-32` of this text prints the golden value.
	const wantLines = "capacity=20\n" +
		"cleanup_interval_ns=5000000000\n" +
		"clock_source=store\n" +
		"failure_mode=closed\n" +
		"max_live_markers=50000\n" +
		"max_markers_per_key=1000\n" +
		"max_tracked_buckets=100000\n" +
		"outage_tolerance_ns=5000000000\n" +
		"rate_period_ns=1000000000\n" +
		"rate_tokens=10\n" +
		"receipt_lifetime_ns=60000000000\n" +
		"refund_deadline_ns=30000000000\n" +
		"store_schema_version=1\n" +
		"time_origin=unix_us\n"
	p := harnessPolicy()
	if got := Lines(p); got != wantLines {
		t.Errorf("Lines(§B2 policy) =\n%s\nwant\n%s", got, wantLines)
	}
	if got := Of(p); got != "7533fc8b745c12286d07d96adc14adf6" {
		t.Errorf("Of(§B2 policy) = %s, want 7533fc8b745c12286d07d96adc14adf6", got)
	}
	p.FailureMode = settings.Open
	if got := Of(p); got != "2e07f09652c01898dcf63e812aef5053" {
		t.Errorf("Of(§B2 policy, failure_mode=open) = %s, want 2e07f09652c01898dcf63e812aef5053", got)
	}
}

func TestFingerprintCoversEveryShared(t *testing.T) {
	// One change per Policy field; every field of CFG-8 marked "fingerprinted" is in Policy.
	changes := map[string]func(p *settings.Policy){
		"RateTokens":        func(p *settings.Policy) { p.RateTokens++ },
		"RatePeriodNs":      func(p *settings.Policy) { p.RatePeriodNs += 1_000 },
		"Capacity":          func(p *settings.Policy) { p.Capacity++ },
		"FailureMode":       func(p *settings.Policy) { p.FailureMode = settings.Open },
		"MaxTrackedBuckets": func(p *settings.Policy) { p.MaxTrackedBuckets++ },
		"MaxLiveMarkers":    func(p *settings.Policy) { p.MaxLiveMarkers++ },
		"MaxMarkersPerKey":  func(p *settings.Policy) { p.MaxMarkersPerKey++ },
		"ReceiptLifetimeNs": func(p *settings.Policy) { p.ReceiptLifetimeNs += 1_000 },
		"RefundDeadlineNs":  func(p *settings.Policy) { p.RefundDeadlineNs += 1_000 },
		"OutageToleranceNs": func(p *settings.Policy) { p.OutageToleranceNs += 1_000 },
		"CleanupIntervalNs": func(p *settings.Policy) { p.CleanupIntervalNs += 1_000 },
		"ClockSource":       func(p *settings.Policy) { p.ClockSource = settings.Controlled },
	}
	typ := reflect.TypeFor[settings.Policy]()
	if typ.NumField() != 12 || len(changes) != 12 {
		t.Fatalf("Policy has %d fields and the test changes %d, want 12 each", typ.NumField(), len(changes))
	}
	base := Of(harnessPolicy())
	seen := map[string]bool{base: true}
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		change, ok := changes[name]
		if !ok {
			t.Errorf("Policy field %s has no change in this test", name)
			continue
		}
		p := harnessPolicy()
		change(&p)
		got := Of(p)
		if got == base {
			t.Errorf("changing %s leaves the fingerprint at %s", name, base)
		}
		if seen[got] {
			t.Errorf("changing %s gives a fingerprint already seen: %s", name, got)
		}
		seen[got] = true
	}
}
