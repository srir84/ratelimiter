// Package calc is the Go fixture of tools/mutate's tests: each entry plants one bug here.
package calc

import "time"

// Add returns a + b.
func Add(a, b int) int {
	return a + b
}

// At returns xs[i].
func At(xs []int, i int) int {
	return xs[i]
}

// Pause sleeps for d (no time at all) and returns 1.
func Pause() int {
	d := time.Duration(0)
	time.Sleep(d)
	return 1
}
