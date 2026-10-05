//go:build rltest

package e2e

import "sync/atomic"

// Hook names outside their own packages get no exemption, for writes or declarations.
var (
	measurements = make(chan int) // want
	maxInFlight  atomic.Int64     // want
)
