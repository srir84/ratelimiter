//go:build rltest

// Package e2e is a test-helper package (harness/e2e): every file is checked.
package e2e

var started int

// Start writes a package-level variable.
func Start() {
	started++ // want
}
