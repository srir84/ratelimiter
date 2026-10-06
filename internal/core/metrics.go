package core

// Metrics returns a snapshot of the instance's fixed metric registry (DESIGN §A5.11, SPEC
// API-1g). In S5 these move: rl_slots_in_use (slots.go), rl_acquire_results_total
// (acquire.go), rl_store_call_seconds and rl_store_connections (the adapter, which gets the
// registry), and the gauges of every epoch read (epochwatch.go); S6 and S9 complete the set.
func (l *Limiter) Metrics() MetricsSnapshot { return l.reg.Snapshot() }
