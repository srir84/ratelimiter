package testutil

import (
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// childEnv names the test that a child copy of a test binary runs (InChild).
const childEnv = "RL_TESTUTIL_CHILD"

// childTail is how many of a failed child's last output lines InChild reports.
const childTail = 40

// childKillGrace is how long InChild lets the child run past its own test timeout before
// killing it.
const childKillGrace = 5 * time.Second

// InChild runs the calling test again, alone, in a child copy of the test binary, and reports
// whether the caller is that copy. In the parent it runs the child with a test timeout of wait
// (killing it 5 s after that), fails t unless the child passed, and returns false: the test
// then returns at once. In the child it returns true, and the test body runs there.
//
// The child's own assertion failures fail the parent as ordinary failures, with the child's
// last output lines. A crash of the child (a panic, a fatal error, or its test timeout, which
// panics) is a crash, never a failure by assertion (DESIGN §C9: neither a panic nor a timeout
// is a kill): InChild writes the child's "panic: " or "fatal error: " line unindented to the
// parent's stderr, where go test and bin/mutate look for one, then fails t. A child it has to
// kill gets a "panic: test timed out" line of InChild's own. The parent's other tests still
// run after a crash in the child.
func InChild(t testing.TB, wait time.Duration) bool {
	t.Helper()
	return inChild(t, wait, os.Stderr)
}

// inChild is InChild with the parent's crash output going to crashOut, so a test of InChild
// can read what it writes there.
func inChild(t testing.TB, wait time.Duration, crashOut io.Writer) bool {
	t.Helper()
	name := t.Name()
	if os.Getenv(childEnv) == name {
		return true
	}
	bin, err := os.Executable()
	if err != nil {
		t.Errorf("InChild: finding the test binary: %v", err)
		return false
	}
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	// The child's own timeout comes first, so a hang ends with its goroutines' stacks.
	ctx, cancel := context.WithTimeout(t.Context(), wait+childKillGrace)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run="+strings.Join(parts, "/"), "-test.count=1", "-test.v",
		"-test.timeout="+wait.String())
	cmd.Env = append(os.Environ(), childEnv+"="+name)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	crash := crashLine(out)
	if crash == "" && err != nil && ctx.Err() != nil {
		crash = "panic: test timed out: InChild killed the child copy of " + name + ", still running " +
			childKillGrace.String() + " after its own " + wait.String() + " test timeout"
	}
	switch {
	case crash != "":
		if _, werr := io.WriteString(crashOut, crash+"\n"); werr != nil {
			t.Errorf("InChild: writing the child's crash line: %v", werr)
		}
		t.Errorf("InChild: the child copy of %s crashed (%v); its last lines:\n%s", name, err, tail(out))
	case err != nil:
		t.Errorf("InChild: the child copy of %s failed (%v); its last lines:\n%s", name, err, tail(out))
	case !strings.Contains(string(out), "--- PASS: "+name+" ("):
		t.Errorf("InChild: the child copy of %s reported no pass; its last lines:\n%s", name, tail(out))
	}
	return false
}

// crashLine returns the first line of out that starts like a crash of a Go process ("panic: "
// or "fatal error: "), or "". In the child's -test.v output every line a test logs is
// indented, so only the runtime writes such a line.
func crashLine(out []byte) string {
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "panic: ") || strings.HasPrefix(l, "fatal error: ") {
			return strings.TrimRight(l, "\r")
		}
	}
	return ""
}

// tail returns out's last childTail lines, after the line that names a crash when the
// crash's goroutine stacks pushed it out of them, each marked "| ", so no line of the child's
// output (its "--- FAIL" lines, say) reads as the parent's own.
func tail(out []byte) string {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	from := max(len(lines)-childTail, 0)
	var keep []string
	for _, l := range lines[:from] {
		if strings.HasPrefix(l, "panic: ") || strings.HasPrefix(l, "fatal error: ") {
			keep = append(keep, l, "…")
			break
		}
	}
	keep = append(keep, lines[from:]...)
	for i, l := range keep {
		keep[i] = "| " + l
	}
	return strings.Join(keep, "\n")
}
