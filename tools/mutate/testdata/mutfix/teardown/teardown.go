// Package teardown is a fixture whose TestMain fails the run after its tests pass when Clean
// says something was left behind, as testutil.VerifyNoLeaks does.
package teardown

// Clean reports whether the tests left nothing behind.
func Clean() bool {
	return true
}

// One returns 1.
func One() int {
	return 1
}
