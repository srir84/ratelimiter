package testutil

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// parkUntil blocks in a testutil frame until release is closed.
func parkUntil(started chan<- struct{}, release <-chan struct{}) {
	close(started)
	<-release
}

// selfHeader returns the calling goroutine's "goroutine N [" prefix.
func selfHeader() string {
	buf := make([]byte, 64)
	n := runtime.Stack(buf, false)
	id, _, _ := strings.Cut(string(buf[:n]), " [")
	return id + " ["
}

func TestLeakDetector(t *testing.T) {
	prefixes := []string{"github.com/srir84/ratelimiter/internal/testutil"}
	self := selfHeader()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		parkUntil(started, release)
	}()
	<-started

	stacks := leaked(prefixes)
	parked := 0
	for _, s := range stacks {
		if strings.HasPrefix(s, self) {
			t.Errorf("leaked reported the calling goroutine:\n%s", s)
		}
		if strings.Contains(s, "github.com/srir84/ratelimiter/internal/testutil.parkUntil(") {
			parked++
		}
	}
	if parked != 1 || len(stacks) != 1 {
		t.Fatalf("leaked = %d stacks, %d of them the parked goroutine; want just it:\n%s", len(stacks), parked, strings.Join(stacks, "\n\n"))
	}
	if got := leaked([]string{"github.com/srir84/ratelimiter/internal/testut"}); len(got) != 0 {
		t.Errorf("a prefix that only starts the package path matched %d goroutines", len(got))
	}

	close(release)
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := leaked(prefixes)
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the release, leaked still reports:\n%s", strings.Join(left, "\n\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
