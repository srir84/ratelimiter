// Package helper is a test-helper package (internal/testutil): every file is checked.
package helper

import "os"

type box struct{ n int }

var (
	slots           = make([]int, 8)
	boxed       any = &box{}
	maxInFlight     = 4 // a hook name, but only cmd/hitgen's maxInFlight may be written
)

// Writes through a reslice or a type assertion still write the variable.
func Writes(src []int) {
	slots[1:][0] = 5     // want
	copy(slots[2:], src) // want
	clear(slots[:4])     // want
	os.Args[1:][0] = "x" // want
	boxed.(*box).n = 1   // want
	maxInFlight = 8      // want
	_ = slots[1:]        // a read
	copy(src, slots[1:]) // a read: only copy's first argument is written
}
