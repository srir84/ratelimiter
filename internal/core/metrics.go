package core

// Metrics returns a snapshot of the instance's fixed metric registry (DESIGN §A5.11, SPEC
// API-1g). These move: rl_slots_in_use (slots.go, freed by done(), refunds and retention),
// rl_acquire_results_total (acquire.go), rl_store_call_seconds and rl_store_connections (the
// adapter, which gets the registry), the gauges of every epoch read (epochwatch.go),
// rl_pending_refunds and rl_refund_results_total (refund.go), rl_listener_drops_total
// (listener.go), rl_cleanup_passes_total, rl_cleanup_entries_total, rl_cleanup_stops_total and
// rl_cleanup_lag_seconds (cleanup.go) and rl_reclaim_keys_total (reclaim.go).
func (l *Limiter) Metrics() MetricsSnapshot { return l.reg.Snapshot() }
