package a

import (
	"testing"

	. "ratelimiter/harness/driver"
)

// TestDotDriver: under a dot import, a test-helper package's variable goes by its bare
// name.
func TestDotDriver(t *testing.T) {
	Live.Add("z")    // want
	_ = Live.Count() // a value receiver: a read
}
