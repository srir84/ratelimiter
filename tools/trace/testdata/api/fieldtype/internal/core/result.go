package core

type ResultKind int

const (
	Allowed ResultKind = iota
	Limited
)

type Diag struct{ Scope, Reason, StoreError string }

type Result struct {
	Kind         ResultKind
	DecisionNs   int32 // want: §A5.2 lists int64
	RetryAfterNs int64
	Permit       *Permit
	Diag         Diag
}

type Permit struct{ id string }

func (p *Permit) Done() {}

func (p *Permit) DeductionID() string { return p.id }
