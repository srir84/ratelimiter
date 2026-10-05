//go:build rltest

// Package spectest is a test-helper package (harness/spectest): every file is checked.
package spectest

var cases = map[string]bool{}

// Seen writes a package-level variable.
func Seen(name string) {
	cases[name] = true // want
}
