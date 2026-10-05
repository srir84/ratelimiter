//go:build rltest

// Package pgtest is a test-helper package (harness/pgtest): every file is checked.
package pgtest

var dsn string

// Use writes a package-level variable.
func Use(s string) {
	dsn = s // want
}
