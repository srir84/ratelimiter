package a

import (
	"sort"
	"testing"
)

// TestConversions writes package-level variables through conversions that keep their
// storage: to a type expression, the predeclared any, a type the package declares (a
// generic one with its type arguments), or a sort slice type.
func TestConversions(t *testing.T) {
	([]int)(list)[0] = 7                 // want
	[]int(list)[0] = 7                   // want
	(map[string]int)(table)["k"] = 1     // want
	(*box)(pbox).n = 1                   // want
	(*box)(&v).n = 1                     // want (once: &v)
	interface{}(boxed).(*box).n = 1      // want
	(chan string)(events) <- "x"         // want
	ints(list)[0] = 1                    // want
	sort.IntSlice(list)[0] = 1           // want
	clear(([]int)(list))                 // want
	copy(ints(list), []int{1})           // want
	(*struct{ n int })(&cfg).n = 1       // want (once: &cfg)
	any(pbox).(*box).n = 1               // want
	vec[int](list)[0] = 1                // want
	(vec[int])(list)[0] = 1              // want
	_ = func(ints []int) { ints[0] = 1 } // a parameter named like the type is a local
}
