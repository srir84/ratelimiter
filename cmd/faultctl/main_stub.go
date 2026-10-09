//go:build !rltest

// Command faultctl needs the test build: its faults live in harness/fixtures, which only
// -tags rltest builds contain (DESIGN §B1, §B6; IMPLEMENTATION_PLAN H4 steps 3 and 8), so
// bin/test/faultctl is the real tool. This stub keeps untagged builds of ./... compiling: it
// prints why and exits 2.
package main

import (
	"io"
	"os"
)

// stubMessage is what the untagged faultctl prints.
const stubMessage = "faultctl needs -tags rltest\n"

func main() {
	os.Exit(run(os.Stderr))
}

// run prints stubMessage to stderr and returns the exit code for bad arguments, 2.
func run(stderr io.Writer) int {
	_, _ = io.WriteString(stderr, stubMessage) // nothing else can be done when stderr fails
	return 2
}
