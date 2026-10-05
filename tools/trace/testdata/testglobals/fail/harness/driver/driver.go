// Package driver is a test-helper package: every file is checked.
package driver

import "os"

var (
	state = map[string]int{}
	opts  struct{ n int }
	runs  int
)

func Reset() {
	state = nil // want
}

func Set(k string) {
	state[k] = 1 // want
	opts.n = 2   // want
	runs++       // want
}

func Ref() *map[string]int {
	return &state // want
}

func Env() {
	os.Args[0] = "x" // want
}

func Later() func() {
	return func() {
		runs = 0 // want
	}
}
