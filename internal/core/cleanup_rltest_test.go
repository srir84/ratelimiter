//go:build rltest

package core

import (
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/store"
)

// TestCleanupRunnerKill (plan S7 step 8, L1; DESIGN §A5.12): Kill stops the cleanup runner at
// once, with no drain: with a 20 ms cleanup interval and timed passes running, Kill returns
// within the bound (the test waits at most lifecycleWait for it), no pass follows, and no call
// reaches the adapter after it closes.
func TestCleanupRunnerKill(t *testing.T) {
	p, in := cleanSettings()
	p.CleanupIntervalNs = 20_000_000
	f := joinedFake(p)
	f.due = func(store.Request) store.Response { return clPage(clT0, 0) }
	l := cleanLimiter(t, f, p, in, reasonReady)
	pollUntil(t, "2 timed passes", func() bool { return len(f.requests("due_entries")) >= 2 })
	start := time.Now()
	killed := make(chan struct{})
	go func() {
		defer close(killed)
		Kill(l)
	}()
	select {
	case <-killed:
	case <-time.After(lifecycleWait):
		t.Fatalf("Kill did not return within %v", lifecycleWait)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("Kill took %v, want it at once", el)
	}
	after := len(f.requests("due_entries"))
	time.Sleep(100 * time.Millisecond) // five intervals
	if n := len(f.requests("due_entries")); n != after {
		t.Errorf("%d due_entries when Kill returned, then %d: the runner did not stop", after, n)
	}
	if got := f.callsAfterClose(); len(got) != 0 {
		t.Errorf("calls after the adapter closed: %q, want none", got)
	}
	if rep := l.RunCleanupPass(t.Context()); rep.Status != "unavailable" || rep.Diag.Reason != reasonClosing {
		t.Errorf("a pass after Kill: %+v, want unavailable (closing)", rep)
	}
}
