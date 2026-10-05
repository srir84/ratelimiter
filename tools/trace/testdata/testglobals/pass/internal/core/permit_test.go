package core

import "testing"

// These pass although a Permit holds an atomic.Int32: a map's element type is never
// followed, so permitsByKey alone pins that rule, and at most one top-level pointer is
// removed, from the declared type (permitPtrPtr) or from what new returns (permitBox).
// The test reads them without copying a Permit.
var (
	permitsByKey = map[string]Permit{}
	permitPtrPtr **Permit
	permitBox    = new(*Permit)
)

func TestPermitTables(t *testing.T) {
	if len(permitsByKey) != 0 || permitPtrPtr != nil || *permitBox != nil {
		t.Error("permit tables")
	}
}
