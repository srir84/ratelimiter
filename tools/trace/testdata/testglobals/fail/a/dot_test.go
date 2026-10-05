package a

import (
	. "slices"
	. "sync"
	"testing"
)

var wg WaitGroup // want

// TestDot: a dot import still names the mutable type and the in-place function.
func TestDot(t *testing.T) {
	Sort(list)          // want
	Sort[[]int](list)   // want
	(Sort[[]int])(list) // want
	wg.Wait()
}
