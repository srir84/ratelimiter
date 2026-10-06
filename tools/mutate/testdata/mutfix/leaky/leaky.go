// Package leaky is a fixture whose test passes while its TestMain fails the run afterwards, as
// a goroutine leak check does.
package leaky

// One returns 1.
func One() int {
	return 1
}
