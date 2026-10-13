package metrics

import (
	"math"
	"slices"
	"sync"
	"testing"
	"time"
)

type wantFamily struct {
	name, typ, label string
	values           []string
}

// wantFamilies is the DESIGN §A5.11 metrics table, written out.
func wantFamilies() []wantFamily {
	stops := []string{"state_integrity_error", "config_error", "clock_error", "unavailable", "outcome_unknown"}
	return []wantFamily{
		{"rl_slots_in_use", "gauge", "kind", []string{"receipt", "refund"}},
		{"rl_pending_refunds", "gauge", "", nil},
		{"rl_refund_results_total", "counter", "result", []string{"applied", "already_applied", "receipt_invalid", "receipt_expired", "unresolved"}},
		{"rl_cleanup_passes_total", "counter", "status", append([]string{"complete"}, stops...)},
		{"rl_cleanup_entries_total", "counter", "action", []string{"removed", "moved", "quarantined", "skipped"}},
		{"rl_cleanup_stops_total", "counter", "result", stops},
		{"rl_cleanup_lag_seconds", "gauge", "", nil},
		{"rl_clock_lag_microseconds", "gauge", "", nil},
		{"rl_listener_drops_total", "counter", "", nil},
		{"rl_integrity_hold", "gauge", "", nil},
		{"rl_reclaim_keys_total", "counter", "", nil},
		{"rl_acquire_results_total", "counter", "result", []string{
			"allowed", "degraded", "limited", "validation_error", "clock_error", "busy", "cancelled",
			"config_error", "not_ready", "state_integrity_error", "capacity_error", "unavailable",
			"outcome_unknown", "receipt_error",
		}},
		{"rl_store_call_seconds", "histogram", "script", []string{"acquire", "refund", "due_entries", "cleanup_entry", "reclaim", "read_epoch", "hold"}},
		{"rl_tracked_buckets", "gauge", "", nil},
		{"rl_live_markers", "gauge", "", nil},
		{"rl_quarantined_keys", "gauge", "", nil},
		{"rl_store_connections", "gauge", "kind", []string{"call", "refund", "maintenance"}},
	}
}

func labelValues(f Family) []string {
	var out []string
	for _, s := range f.Series {
		if s.LabelValue != "" || f.LabelName != "" {
			out = append(out, s.LabelValue)
		}
	}
	return out
}

func TestRegistryFamilies(t *testing.T) {
	snap := NewRegistry().Snapshot()
	want := wantFamilies()
	if len(snap.Families) != 17 || len(want) != 17 {
		t.Fatalf("%d families, want 17", len(snap.Families))
	}
	for i, w := range want {
		f := snap.Families[i]
		if f.Name != w.name || f.Type != w.typ || f.LabelName != w.label {
			t.Errorf("family %d = %s %s {%s}, want %s %s {%s}", i, f.Name, f.Type, f.LabelName, w.name, w.typ, w.label)
		}
		if w.label == "" && len(f.Series) != 1 {
			t.Errorf("%s: %d series, want 1 unlabeled", f.Name, len(f.Series))
		}
		if got := labelValues(f); !slices.Equal(got, w.values) {
			t.Errorf("%s label values = %q, want %q", f.Name, got, w.values)
		}
		for _, s := range f.Series {
			if s.Value != 0 || s.Count != 0 || s.Sum != 0 {
				t.Errorf("%s{%s} starts at %v / %d / %v, want 0", f.Name, s.LabelValue, s.Value, s.Count, s.Sum)
			}
			if (len(s.Buckets) != 0) != (w.typ == "histogram") {
				t.Errorf("%s{%s}: %d buckets", f.Name, s.LabelValue, len(s.Buckets))
			}
			if w.typ == "histogram" && !slices.Equal(s.Buckets, []Bucket{
				{0.001, 0}, {0.002, 0}, {0.005, 0}, {0.01, 0}, {0.02, 0}, {0.05, 0}, {0.1, 0}, {0.2, 0}, {0.5, 0}, {1, 0}, {2, 0},
			}) {
				t.Errorf("%s{%s}: buckets %v, want le 0.001 … 2 s, each holding 0", f.Name, s.LabelValue, s.Buckets)
			}
		}
	}
	// The Metric constants name the families in this order.
	for m, name := range map[Metric]string{
		SlotsInUse: "rl_slots_in_use", PendingRefunds: "rl_pending_refunds", RefundResults: "rl_refund_results_total",
		CleanupPasses: "rl_cleanup_passes_total", CleanupEntries: "rl_cleanup_entries_total", CleanupStops: "rl_cleanup_stops_total",
		CleanupLagSeconds: "rl_cleanup_lag_seconds", ClockLagMicroseconds: "rl_clock_lag_microseconds",
		ListenerDrops: "rl_listener_drops_total", IntegrityHold: "rl_integrity_hold", ReclaimKeys: "rl_reclaim_keys_total",
		AcquireResults: "rl_acquire_results_total", StoreCallSeconds: "rl_store_call_seconds", TrackedBuckets: "rl_tracked_buckets",
		LiveMarkers: "rl_live_markers", QuarantinedKeys: "rl_quarantined_keys", StoreConnections: "rl_store_connections",
	} {
		if got := snap.Families[m].Name; got != name {
			t.Errorf("Metric %d names %s, want %s", m, got, name)
		}
	}
	// The label index constants point at their label values.
	for _, c := range []struct {
		m     Metric
		i     int
		value string
	}{
		{SlotsInUse, SlotReceipt, "receipt"}, {SlotsInUse, SlotRefund, "refund"},
		{RefundResults, RefundApplied, "applied"}, {RefundResults, RefundAlreadyApplied, "already_applied"},
		{RefundResults, RefundReceiptInvalid, "receipt_invalid"}, {RefundResults, RefundReceiptExpired, "receipt_expired"},
		{RefundResults, RefundUnresolved, "unresolved"},
		{CleanupPasses, PassComplete, "complete"}, {CleanupPasses, PassStateIntegrityError, "state_integrity_error"},
		{CleanupPasses, PassConfigError, "config_error"}, {CleanupPasses, PassClockError, "clock_error"},
		{CleanupPasses, PassUnavailable, "unavailable"}, {CleanupPasses, PassOutcomeUnknown, "outcome_unknown"},
		{CleanupEntries, ActionRemoved, "removed"}, {CleanupEntries, ActionMoved, "moved"},
		{CleanupEntries, ActionQuarantined, "quarantined"}, {CleanupEntries, ActionSkipped, "skipped"},
		{CleanupStops, StopStateIntegrityError, "state_integrity_error"}, {CleanupStops, StopConfigError, "config_error"},
		{CleanupStops, StopClockError, "clock_error"}, {CleanupStops, StopUnavailable, "unavailable"},
		{CleanupStops, StopOutcomeUnknown, "outcome_unknown"},
		{StoreCallSeconds, ScriptAcquire, "acquire"}, {StoreCallSeconds, ScriptRefund, "refund"},
		{StoreCallSeconds, ScriptDueEntries, "due_entries"}, {StoreCallSeconds, ScriptCleanupEntry, "cleanup_entry"},
		{StoreCallSeconds, ScriptReclaim, "reclaim"}, {StoreCallSeconds, ScriptReadEpoch, "read_epoch"},
		{StoreCallSeconds, ScriptHold, "hold"},
		{StoreConnections, ConnCall, "call"}, {StoreConnections, ConnRefund, "refund"},
		{StoreConnections, ConnMaintenance, "maintenance"},
	} {
		if got := snap.Families[c.m].Series[c.i].LabelValue; got != c.value {
			t.Errorf("%s label index %d = %q, want %q", snap.Families[c.m].Name, c.i, got, c.value)
		}
	}
}

// TestRegistryRecords: each method records into a series of its own type, in range. It runs
// before TestSeriesBounded, which passes out-of-range indexes and a nil registry before it
// asserts, so a method that drops in-range calls and crashes on those fails here by assertion
// first (DESIGN §C9: a crash is no kill); each call runs on a goroutine of its own with a
// bounded wait, so one that never returns fails here too.
func TestRegistryRecords(t *testing.T) {
	r := NewRegistry()
	for _, c := range []struct {
		what string
		call func()
	}{
		{"Inc", func() { r.Inc(AcquireResults, 1) }},
		{"Add", func() { r.Add(RefundResults, RefundUnresolved, 5) }},
		{"Set", func() { r.Set(TrackedBuckets, 0, 7) }},
		{"AddGauge", func() { r.AddGauge(SlotsInUse, SlotReceipt, 2.5) }},
		{"Observe", func() { r.Observe(StoreCallSeconds, ScriptHold, 3*time.Millisecond) }},
	} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.call()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s still running 5 s later", c.what)
		}
	}
	snap := r.Snapshot()
	for _, c := range []struct {
		m    Metric
		i    int
		want float64
	}{{AcquireResults, 1, 1}, {RefundResults, RefundUnresolved, 5}, {TrackedBuckets, 0, 7}, {SlotsInUse, SlotReceipt, 2.5}} {
		if s := snap.Families[c.m].Series[c.i]; s.Value != c.want {
			t.Errorf("%s{%s} = %v, want %v", snap.Families[c.m].Name, s.LabelValue, s.Value, c.want)
		}
	}
	h := snap.Families[StoreCallSeconds].Series[ScriptHold]
	if h.Count != 1 || h.Sum != 0.003 || h.Buckets[1].Count != 0 || h.Buckets[2].Count != 1 {
		t.Errorf("rl_store_call_seconds{hold} = count %d, sum %v, buckets %v; want 1, 0.003, from the 5 ms bucket on", h.Count, h.Sum, h.Buckets)
	}
}

// hit records into metric m at label index i with the method of its type: +1, set to v, or
// one 1 ms observation.
func hit(r *Registry, typ string, m Metric, i int, v float64) {
	switch typ {
	case "counter":
		r.Inc(m, i)
	case "gauge":
		r.Set(m, i, v)
	case "histogram":
		r.Observe(m, i, time.Millisecond)
	}
}

func TestSeriesBounded(t *testing.T) {
	r := NewRegistry()
	want := wantFamilies()
	for m, w := range want {
		n := max(len(w.values), 1)
		for i := -1; i <= n; i++ { // every index, including −1 and len
			hit(r, w.typ, Metric(m), i, 7)
			// The other types' methods leave the family alone.
			r.Inc(Metric(m), i)
			r.Add(Metric(m), i, 5)
			r.Set(Metric(m), i, 99)
			r.AddGauge(Metric(m), i, 1)
			r.Observe(Metric(m), i, time.Second)
		}
	}
	for _, m := range []Metric{-1, 17, 1000} { // unknown metrics are ignored
		r.Inc(m, 0)
		r.Set(m, 0, 1)
		r.AddGauge(m, 0, 1)
		r.Observe(m, 0, time.Second)
	}
	var nilReg *Registry
	nilReg.Inc(AcquireResults, 0)
	if len(nilReg.Snapshot().Families) != 0 {
		t.Error("a nil registry has families")
	}

	snap := r.Snapshot()
	total := 0
	for i, f := range snap.Families {
		total += len(f.Series)
		for _, s := range f.Series {
			// Each in-range series got exactly its own type's calls: a counter 1 + 1 + 5,
			// a gauge set to 7, then 99, then +1; a histogram 1 ms and 1 s.
			switch want[i].typ {
			case "counter":
				if s.Value != 7 {
					t.Errorf("%s{%s} = %v, want 7", f.Name, s.LabelValue, s.Value)
				}
			case "gauge":
				if s.Value != 100 {
					t.Errorf("%s{%s} = %v, want 100", f.Name, s.LabelValue, s.Value)
				}
			case "histogram":
				if s.Count != 2 || s.Sum != 1.001 || s.Buckets[0].Count != 1 || s.Buckets[9].Count != 2 {
					t.Errorf("%s{%s} = count %d, sum %v, buckets %v", f.Name, s.LabelValue, s.Count, s.Sum, s.Buckets)
				}
			}
		}
	}
	if total != 55 {
		t.Errorf("%d series, want exactly 55", total)
	}
}

func TestHistogramBuckets(t *testing.T) {
	bounds := []float64{0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2}
	r := NewRegistry()
	r.Observe(StoreCallSeconds, ScriptAcquire, 1500*time.Microsecond)
	s := r.Snapshot().Families[StoreCallSeconds].Series[ScriptAcquire]
	if len(s.Buckets) != len(bounds) {
		t.Fatalf("%d buckets, want %d", len(s.Buckets), len(bounds))
	}
	for i, b := range s.Buckets {
		if b.UpperBound != bounds[i] {
			t.Errorf("bucket %d upper bound = %v, want %v", i, b.UpperBound, bounds[i])
		}
		want := uint64(1) // 1.5 ms lands in the 0.002 bucket: cumulative from there on
		if i == 0 {
			want = 0
		}
		if b.Count != want {
			t.Errorf("bucket le=%v holds %d, want %d", b.UpperBound, b.Count, want)
		}
	}
	if s.Count != 1 || s.Sum != 0.0015 {
		t.Errorf("count %d, sum %v; want 1, 0.0015", s.Count, s.Sum)
	}

	// Bounds are inclusive; past 2 s only the +Inf count grows; a negative duration counts as 0.
	for _, c := range []struct {
		d     time.Duration
		first int // index of the first bucket that counts it; len(bounds) = +Inf only
	}{
		{0, 0}, {-time.Second, 0}, {time.Millisecond, 0}, {time.Millisecond + 1, 1}, {2 * time.Millisecond, 1},
		{2*time.Millisecond + 1, 2}, {time.Second, 9}, {2 * time.Second, 10}, {2*time.Second + 1, 11}, {time.Hour, 11},
	} {
		r := NewRegistry()
		r.Observe(StoreCallSeconds, ScriptHold, c.d)
		s := r.Snapshot().Families[StoreCallSeconds].Series[ScriptHold]
		for i, b := range s.Buckets {
			want := uint64(0)
			if i >= c.first {
				want = 1
			}
			if b.Count != want {
				t.Errorf("Observe(%v): bucket le=%v holds %d, want %d", c.d, b.UpperBound, b.Count, want)
			}
		}
		if s.Count != 1 {
			t.Errorf("Observe(%v): count %d, want 1", c.d, s.Count)
		}
		if c.d <= 0 && s.Sum != 0 {
			t.Errorf("Observe(%v): sum %v, want 0", c.d, s.Sum)
		}
	}
}

// TestHistogramSumRange: a series' sum stays positive and right past 2^63 ns of observed time.
// Two observations of the longest duration sum to 2 × (2^63 − 1) ns, about 1.8 × 10^10 s,
// which an int64 count of nanoseconds cannot hold.
func TestHistogramSumRange(t *testing.T) {
	r := NewRegistry()
	r.Observe(StoreCallSeconds, ScriptRefund, time.Duration(math.MaxInt64))
	r.Observe(StoreCallSeconds, ScriptRefund, time.Duration(math.MaxInt64))
	s := r.Snapshot().Families[StoreCallSeconds].Series[ScriptRefund]
	const want = 2 * float64(math.MaxInt64) / 1e9 // seconds
	if s.Count != 2 || s.Sum <= 0 || math.Abs(s.Sum-want) > 1e-6*want {
		t.Errorf("count %d, sum %v; want 2, %v (within 1e-6 of it)", s.Count, s.Sum, want)
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				r.Inc(AcquireResults, 0)
				r.AddGauge(SlotsInUse, SlotReceipt, 1)
				r.AddGauge(SlotsInUse, SlotReceipt, -1)
				r.AddGauge(StoreConnections, ConnCall, 1)
				r.Observe(StoreCallSeconds, ScriptAcquire, 3*time.Millisecond)
				_ = r.Snapshot()
			}
		}()
	}
	wg.Wait()
	snap := r.Snapshot()
	if v := snap.Families[AcquireResults].Series[0].Value; v != 8000 {
		t.Errorf("acquire allowed = %v, want 8000", v)
	}
	if v := snap.Families[SlotsInUse].Series[SlotReceipt].Value; v != 0 {
		t.Errorf("slots in use = %v, want 0", v)
	}
	if v := snap.Families[StoreConnections].Series[ConnCall].Value; v != 8000 {
		t.Errorf("connections = %v, want 8000", v)
	}
	h := snap.Families[StoreCallSeconds].Series[ScriptAcquire]
	if h.Count != 8000 || math.Abs(h.Sum-24) > 1e-9 || h.Buckets[2].Count != 8000 || h.Buckets[1].Count != 0 {
		t.Errorf("histogram count %d, sum %v, buckets %v", h.Count, h.Sum, h.Buckets)
	}
}
