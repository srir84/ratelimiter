// Package testutil holds test helpers shared by the library's packages (DESIGN §A1): the
// goroutine leak check that TestMain runs (AGENTS.md §2), with storeproc, fakeresp and the
// later helpers in its subpackages. It is test-only: production code never imports it.
package testutil

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// leakWait is how long VerifyNoLeaks lets goroutines finish after the tests pass.
const leakWait = 5 * time.Second

// VerifyNoLeaks runs the tests and, when they pass, waits up to 5 s for every other
// goroutine whose stack has a frame in ratelimiter/internal/core or
// ratelimiter/internal/store to end. If any is still running it prints their stacks to
// stderr and returns 1; otherwise it returns the tests' exit code. A package's TestMain
// calls it: os.Exit(testutil.VerifyNoLeaks(m)).
func VerifyNoLeaks(m *testing.M) int {
	code := m.Run()
	if code != 0 {
		return code
	}
	prefixes := []string{"ratelimiter/internal/core", "ratelimiter/internal/store"}
	deadline := time.Now().Add(leakWait)
	for {
		left := leaked(prefixes)
		if len(left) == 0 {
			return 0
		}
		if time.Now().After(deadline) {
			_, _ = fmt.Fprintf(os.Stderr, "VerifyNoLeaks: %d goroutine(s) still running %v after the tests:\n\n%s\n",
				len(left), leakWait, strings.Join(left, "\n\n"))
			return 1
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// leaked returns the stack of every goroutine that has a frame in a package at or under
// one of prefixes (import paths), except the calling goroutine: the first stack that
// runtime.Stack(buf, true) returns, since TestMain may live in a checked package.
func leaked(prefixes []string) []string {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	stacks := strings.Split(strings.TrimSpace(string(buf)), "\n\n")
	var out []string
	for _, g := range stacks[1:] {
		if hasFrame(g, prefixes) {
			out = append(out, g)
		}
	}
	return out
}

// hasFrame reports whether one goroutine's stack text has a function frame in a package at
// or under one of prefixes. Frames are the lines after the "goroutine N [...]:" header that
// do not start with a tab (those give file and line); the "created by" line names the
// creator, not a frame of this goroutine.
func hasFrame(stack string, prefixes []string) bool {
	lines := strings.Split(stack, "\n")
	for _, line := range lines[1:] {
		if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "created by ") {
			continue
		}
		for _, p := range prefixes {
			rest, ok := strings.CutPrefix(line, p)
			if ok && (strings.HasPrefix(rest, ".") || strings.HasPrefix(rest, "/")) {
				return true
			}
		}
	}
	return false
}
