//go:build rltest

// Package fixtures is a test-helper package (harness/fixtures): every file is checked.
package fixtures

var loaded []string

// Load writes a package-level variable.
func Load(name string) {
	loaded = append(loaded, name) // want
}
