package metrics

import (
	"math"
	"sync/atomic"
	"time"
)

// Metric names one family; the families appear in Snapshot in this order.
type Metric int

const (
	SlotsInUse           Metric = iota // rl_slots_in_use{kind}: gauge
	PendingRefunds                     // rl_pending_refunds: gauge
	RefundResults                      // rl_refund_results_total{result}: counter
	CleanupPasses                      // rl_cleanup_passes_total{status}: counter
	CleanupEntries                     // rl_cleanup_entries_total{action}: counter
	CleanupStops                       // rl_cleanup_stops_total{result}: counter
	CleanupLagSeconds                  // rl_cleanup_lag_seconds: gauge
	ClockLagMicroseconds               // rl_clock_lag_microseconds: gauge
	ListenerDrops                      // rl_listener_drops_total: counter
	IntegrityHold                      // rl_integrity_hold: gauge, 0 or 1
	ReclaimKeys                        // rl_reclaim_keys_total: counter
	AcquireResults                     // rl_acquire_results_total{result}: counter
	StoreCallSeconds                   // rl_store_call_seconds{script}: histogram
	TrackedBuckets                     // rl_tracked_buckets: gauge
	LiveMarkers                        // rl_live_markers: gauge
	QuarantinedKeys                    // rl_quarantined_keys: gauge
	StoreConnections                   // rl_store_connections{kind}: gauge
)

// Label indexes of rl_slots_in_use (kind).
const (
	SlotReceipt = iota
	SlotRefund
)

// Label indexes of rl_refund_results_total (result), in RefundResult order.
const (
	RefundApplied = iota
	RefundAlreadyApplied
	RefundReceiptInvalid
	RefundReceiptExpired
	RefundUnresolved
)

// Label indexes of rl_cleanup_passes_total (status): complete, then the stop results in the
// order of rl_cleanup_stops_total.
const (
	PassComplete = iota
	PassStateIntegrityError
	PassConfigError
	PassClockError
	PassUnavailable
	PassOutcomeUnknown
)

// Label indexes of rl_cleanup_entries_total (action), from the cleanup_entry reply codes.
const (
	ActionRemoved     = iota // code 30
	ActionMoved              // code 31, including the push of an already-quarantined key
	ActionQuarantined        // code 32
	ActionSkipped            // code 33
)

// Label indexes of rl_cleanup_stops_total (result): what stopped a pass (API-1f).
const (
	StopStateIntegrityError = iota
	StopConfigError
	StopClockError
	StopUnavailable
	StopOutcomeUnknown
)

// Label indexes of rl_store_call_seconds (script): the seven scripts rl_lib runs (§A2).
const (
	ScriptAcquire = iota
	ScriptRefund
	ScriptDueEntries
	ScriptCleanupEntry
	ScriptReclaim
	ScriptReadEpoch
	ScriptHold
)

// Label indexes of rl_store_connections (kind): ready connections per kind (§A5.4).
const (
	ConnCall = iota
	ConnRefund
	ConnMaintenance
)

// The acquire result labels are the 14 ResultKind values in order (internal/core).

// Family types.
const (
	typeCounter   = "counter"
	typeGauge     = "gauge"
	typeHistogram = "histogram"
)

// MetricsSnapshot is a point-in-time copy of every series (Limiter.Metrics). The harness
// renders it as Prometheus text.
type MetricsSnapshot struct {
	Families []Family
}

// Family is one metric family with all of its series.
type Family struct {
	Name      string   // e.g. rl_acquire_results_total
	Type      string   // counter, gauge or histogram
	LabelName string   // "" for a family without a label
	Series    []Series // one per label value, in the label table's order
}

// Series is one series of a family.
type Series struct {
	LabelValue string   // "" for a family without a label
	Value      float64  // a counter's count or a gauge's value; 0 for a histogram
	Buckets    []Bucket // histogram only: cumulative counts, upper bounds ascending
	Sum        float64  // histogram only: seconds observed in total
	Count      uint64   // histogram only: observations, the +Inf bucket
}

// Bucket counts the observations at or below UpperBound seconds (Prometheus "le").
type Bucket struct {
	UpperBound float64
	Count      uint64
}

// Registry holds the fixed families. The zero value has no families; use NewRegistry.
type Registry struct {
	families []family
	bounds   []time.Duration // histogram bucket upper bounds
}

type family struct {
	name, typ, label string
	values           []string // label values; one empty value for a family without a label
	series           []series
}

type series struct {
	value   atomic.Uint64   // counter: the count; gauge: math.Float64bits of the value
	buckets []atomic.Uint64 // histogram: per-bucket counts, the last one +Inf
	sumNs   atomic.Int64    // histogram: nanoseconds observed in total
}

// NewRegistry returns a registry with every family and label value registered and all
// values 0.
func NewRegistry() *Registry {
	stops := []string{"state_integrity_error", "config_error", "clock_error", "unavailable", "outcome_unknown"}
	none := []string{""}
	specs := []struct {
		name, typ, label string
		values           []string
	}{
		SlotsInUse:           {"rl_slots_in_use", typeGauge, "kind", []string{"receipt", "refund"}},
		PendingRefunds:       {"rl_pending_refunds", typeGauge, "", none},
		RefundResults:        {"rl_refund_results_total", typeCounter, "result", []string{"applied", "already_applied", "receipt_invalid", "receipt_expired", "unresolved"}},
		CleanupPasses:        {"rl_cleanup_passes_total", typeCounter, "status", append([]string{"complete"}, stops...)},
		CleanupEntries:       {"rl_cleanup_entries_total", typeCounter, "action", []string{"removed", "moved", "quarantined", "skipped"}},
		CleanupStops:         {"rl_cleanup_stops_total", typeCounter, "result", stops},
		CleanupLagSeconds:    {"rl_cleanup_lag_seconds", typeGauge, "", none},
		ClockLagMicroseconds: {"rl_clock_lag_microseconds", typeGauge, "", none},
		ListenerDrops:        {"rl_listener_drops_total", typeCounter, "", none},
		IntegrityHold:        {"rl_integrity_hold", typeGauge, "", none},
		ReclaimKeys:          {"rl_reclaim_keys_total", typeCounter, "", none},
		AcquireResults: {"rl_acquire_results_total", typeCounter, "result", []string{
			"allowed", "degraded", "limited", "validation_error", "clock_error", "busy", "cancelled",
			"config_error", "not_ready", "state_integrity_error", "capacity_error", "unavailable",
			"outcome_unknown", "receipt_error",
		}},
		StoreCallSeconds: {"rl_store_call_seconds", typeHistogram, "script", []string{
			"acquire", "refund", "due_entries", "cleanup_entry", "reclaim", "read_epoch", "hold",
		}},
		TrackedBuckets:   {"rl_tracked_buckets", typeGauge, "", none},
		LiveMarkers:      {"rl_live_markers", typeGauge, "", none},
		QuarantinedKeys:  {"rl_quarantined_keys", typeGauge, "", none},
		StoreConnections: {"rl_store_connections", typeGauge, "kind", []string{"call", "refund", "maintenance"}},
	}
	r := &Registry{
		bounds: []time.Duration{
			time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
			20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
			500 * time.Millisecond, time.Second, 2 * time.Second,
		},
		families: make([]family, len(specs)),
	}
	for i, sp := range specs {
		f := &r.families[i]
		f.name, f.typ, f.label, f.values = sp.name, sp.typ, sp.label, sp.values
		f.series = make([]series, len(sp.values))
		if sp.typ == typeHistogram {
			for j := range f.series {
				f.series[j].buckets = make([]atomic.Uint64, len(r.bounds)+1)
			}
		}
	}
	return r
}

// at returns the series of metric m at label index i when m has type typ; nil (ignore the
// call) for an unknown metric, an index out of range or another type.
func (r *Registry) at(m Metric, i int, typ string) *series {
	if r == nil || m < 0 || int(m) >= len(r.families) {
		return nil
	}
	f := &r.families[m]
	if f.typ != typ || i < 0 || i >= len(f.series) {
		return nil
	}
	return &f.series[i]
}

// Add adds n to the counter m at label index i (0 for a family without a label).
func (r *Registry) Add(m Metric, i int, n uint64) {
	if s := r.at(m, i, typeCounter); s != nil {
		s.value.Add(n)
	}
}

// Inc adds 1 to the counter m at label index i.
func (r *Registry) Inc(m Metric, i int) { r.Add(m, i, 1) }

// Set sets the gauge m at label index i to v.
func (r *Registry) Set(m Metric, i int, v float64) {
	if s := r.at(m, i, typeGauge); s != nil {
		s.value.Store(math.Float64bits(v))
	}
}

// AddGauge adds d (which may be negative) to the gauge m at label index i.
func (r *Registry) AddGauge(m Metric, i int, d float64) {
	s := r.at(m, i, typeGauge)
	if s == nil {
		return
	}
	for {
		old := s.value.Load()
		if s.value.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

// Observe records one duration in the histogram m at label index i: it lands in the first
// bucket whose upper bound is ≥ d (+Inf after 2 s). A negative d counts as 0.
func (r *Registry) Observe(m Metric, i int, d time.Duration) {
	s := r.at(m, i, typeHistogram)
	if s == nil {
		return
	}
	d = max(d, 0)
	b := len(r.bounds) // +Inf
	for j, ub := range r.bounds {
		if d <= ub {
			b = j
			break
		}
	}
	s.buckets[b].Add(1)
	s.sumNs.Add(int64(d))
}

// Snapshot copies every series. Each value is read atomically; the snapshot as a whole is
// not one instant. A histogram's Count is the sum of its buckets, so the cumulative counts
// never exceed it.
func (r *Registry) Snapshot() MetricsSnapshot {
	if r == nil {
		return MetricsSnapshot{}
	}
	out := MetricsSnapshot{Families: make([]Family, len(r.families))}
	for i := range r.families {
		f := &r.families[i]
		fs := Family{Name: f.name, Type: f.typ, LabelName: f.label, Series: make([]Series, len(f.series))}
		for j := range f.series {
			s := &f.series[j]
			ss := Series{LabelValue: f.values[j]}
			switch f.typ {
			case typeCounter:
				ss.Value = float64(s.value.Load())
			case typeGauge:
				ss.Value = math.Float64frombits(s.value.Load())
			case typeHistogram:
				ss.Buckets = make([]Bucket, len(r.bounds))
				var cum uint64
				for k := range s.buckets {
					cum += s.buckets[k].Load()
					if k < len(r.bounds) {
						ss.Buckets[k] = Bucket{UpperBound: r.bounds[k].Seconds(), Count: cum}
					}
				}
				ss.Count = cum
				ss.Sum = time.Duration(s.sumNs.Load()).Seconds()
			}
			fs.Series[j] = ss
		}
		out.Families[i] = fs
	}
	return out
}
