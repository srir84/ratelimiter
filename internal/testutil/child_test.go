package testutil

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a testing.TB whose Errorf only records, so a test can check what InChild
// reports about a failing child without failing itself.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errs...)
}

// TestInChildPasses: a child copy that passes leaves the parent passing; the body runs only
// in the child.
func TestInChildPasses(t *testing.T) {
	if !InChild(t, 20*time.Second) {
		return
	}
	if os.Getenv(childEnv) != t.Name() {
		t.Errorf("the body ran outside the child copy (%s=%q)", childEnv, os.Getenv(childEnv))
	}
}

// TestInChildReportsFailures (must-fail fixtures): a child copy that fails an assertion, that
// crashes in a goroutine the test does not own, that hangs past its wait, or that exits 0
// without passing makes InChild fail the parent, once, with the child's last lines marked so
// that none reads as the parent's own output. A crash or a hang is a crash in the parent too
// (DESIGN §C9: never a kill): InChild writes the child's "panic: " line, and only that line,
// unindented to the parent's crash output; an assertion failure writes nothing there.
func TestInChildReportsFailures(t *testing.T) {
	cases := []struct {
		name  string
		wait  time.Duration
		body  func(t *testing.T)
		want  string // in the parent's error
		crash string // the line written to the parent's crash output, or "" for none
	}{
		{"fails", 20 * time.Second, func(t *testing.T) { t.Error("the child fails (test)") }, "failed (exit status 1)", ""},
		{"crashes", 20 * time.Second, func(*testing.T) {
			crashed := make(chan struct{})
			go func() {
				defer close(crashed)
				panic("testutil: a goroutine the test does not own crashes (test)")
			}()
			<-crashed
			time.Sleep(time.Minute) // the crash ends the process first
		}, "| panic: testutil: a goroutine the test does not own crashes (test)",
			"panic: testutil: a goroutine the test does not own crashes (test)"},
		{"hangs", time.Second, func(*testing.T) { time.Sleep(time.Minute) }, "| panic: test timed out after 1s",
			"panic: test timed out after 1s"},
		{"exits 0", 20 * time.Second, func(*testing.T) { os.Exit(0) }, "reported no pass", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{TB: t}
			var crashOut strings.Builder
			if inChild(rec, tc.wait, &crashOut) {
				tc.body(t)
				return
			}
			errs := rec.all()
			if len(errs) != 1 {
				t.Fatalf("InChild reported %d errors, want 1: %q", len(errs), errs)
			}
			if !strings.Contains(errs[0], tc.want) {
				t.Errorf("InChild reported %q, want it to contain %q", errs[0], tc.want)
			}
			for _, line := range strings.Split(errs[0], "\n") {
				if strings.HasPrefix(line, "panic: ") {
					t.Errorf("InChild's report has an unmarked line %q", line)
				}
			}
			want := ""
			if tc.crash != "" {
				want = tc.crash + "\n"
			}
			if got := crashOut.String(); got != want {
				t.Errorf("InChild wrote %q to the parent's crash output, want %q", got, want)
			}
		})
	}
}

// TestCrashLine: only a line that starts like a Go crash counts, and the first one wins; a
// test's indented log line that mentions a panic does not.
func TestCrashLine(t *testing.T) {
	for _, c := range []struct{ out, want string }{
		{"=== RUN   TestX\n    x_test.go:9: panic: in a log line\n--- FAIL: TestX (0.00s)\n", ""},
		{"=== RUN   TestX\npanic: boom\n\ngoroutine 7 [running]:\npanic: again\n", "panic: boom"},
		{"fatal error: all goroutines are asleep - deadlock!\r\n", "fatal error: all goroutines are asleep - deadlock!"},
		{"", ""},
	} {
		if got := crashLine([]byte(c.out)); got != c.want {
			t.Errorf("crashLine(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}
