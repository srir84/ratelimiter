// Package core mirrors DESIGN §A5.2: a Result holds its Permit through a pointer, and a
// Permit keeps its state in an atomic.Int32 (plan S2 step 9).
package core

import "sync/atomic"

type ResultKind int

type Diag struct {
	Scope, Reason, StoreError string
}

type Permit struct {
	state atomic.Int32
}

type Result struct {
	Kind         ResultKind
	DecisionNs   int64
	RetryAfterNs int64
	Permit       *Permit
	Diag         Diag
}

// Done is one of the Permit methods that change its state.
func (p *Permit) Done() { p.state.Store(1) }
