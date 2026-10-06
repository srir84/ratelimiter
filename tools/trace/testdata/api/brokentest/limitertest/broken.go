//go:build rltest

package limitertest

// A type error in the rltest build: limitertest does not load, so api fails closed.
var broken int = "not an int"
