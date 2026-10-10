//go:build !rltest

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
