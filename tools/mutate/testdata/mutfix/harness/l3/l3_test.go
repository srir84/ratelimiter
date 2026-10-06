// Package l3 stands for an L3 package under harness/, whose tests mutants never run.
package l3

import "testing"

func TestL3(t *testing.T) {
	if 1+1 != 2 {
		t.Error("1 + 1 != 2")
	}
}
