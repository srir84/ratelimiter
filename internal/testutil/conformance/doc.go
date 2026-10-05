//go:build conformance

// Package conformance is the store conformance suite (DESIGN §C2, IMPLEMENTATION_PLAN
// S1, layer L0): it re-checks DESIGN §A3.0 rows 1–8, 10–12, 15 and 16 on the pinned
// Valkey as assertions, records rows 9, 13 and 14 and the F_FULLFSYNC floor as
// measurements, and writes them to .data/conformance.json on an unfiltered run.
//
// Every file carries the conformance build tag (plan choice), so the long suite stays
// out of ./... runs; make test-store runs it. Probe scripts live in testdata/, never in
// internal/scripts/.
package conformance
