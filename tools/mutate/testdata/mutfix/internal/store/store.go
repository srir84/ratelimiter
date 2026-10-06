// Package store is a sweep fixture in a Go operator directory whose unmutated test fails, so a
// sweep over it stops at the baseline.
package store

// Positive reports whether n > 0.
func Positive(n int) bool {
	return n > 0
}
