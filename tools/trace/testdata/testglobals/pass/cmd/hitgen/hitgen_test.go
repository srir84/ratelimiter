package main

import "testing"

func TestCap(t *testing.T) {
	maxInFlight = 2
	t.Cleanup(func() { maxInFlight = 1024 })
}
