// Package core is the limiter (DESIGN §A5): the public types, the acquire flow, the permit
// state machine, slots, refund workers, the epoch watch, the cleanup runner and the old-epoch
// reclaimer (§A5.1–§A5.8), and the listener dispatcher, health and metrics (§A5.11). Package
// limiter re-exports its types as aliases. Types owned by a lower internal package are
// aliased here (§A5.2 "Where things are declared"). It reaches the store only through
// internal/store. NewWithHooks, the only hook entry point, and Kill exist only in -tags rltest
// builds (hooks_rltest.go, §A5.12), which limitertest wraps.
package core
