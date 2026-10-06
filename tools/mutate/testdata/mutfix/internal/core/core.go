// Package core is the sweep fixture of tools/mutate's tests (a Go operator directory).
package core

import "errors"

// ErrNegative is Check's error.
var ErrNegative = errors.New("negative")

// Check returns ErrNegative for n < 0.
func Check(n int) error {
	if n < 0 {
		return ErrNegative
	}
	return nil
}

// ErrRange is Pick's error.
var ErrRange = errors.New("out of range")

// Pick returns xs[i], or ErrRange when i is past the end.
func Pick(xs []int, i int) (int, error) {
	if i >= len(xs) {
		return 0, ErrRange
	}
	return xs[i], nil
}

// Small reports whether n is at most 10.
func Small(n int) bool {
	return n <= 10
}

// Big reports whether n is above 100.
func Big(n int) bool {
	return n > 100
}

// Same reports whether a and b are equal.
func Same(a, b int) bool {
	return a == b
}

// Differ reports whether a and b differ; only the rltest build tests it.
func Differ(a, b int) bool {
	return a != b
}
