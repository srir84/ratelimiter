//go:build rltest

package fixtures_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"ratelimiter/harness/dbclient"
	"ratelimiter/harness/fixtures"
)

// fakeHitDB is the database behind the wrapper: it records each call's stop channel and
// answers with res.
type fakeHitDB struct {
	res dbclient.Result

	mu    sync.Mutex
	stops []<-chan struct{}
}

func (f *fakeHitDB) Hit(_ context.Context, _, _ string, _ int64, stop <-chan struct{}) dbclient.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, stop)
	return f.res
}

func (f *fakeHitDB) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stops)
}

// TestDBWrap_Modes: each database fault changes exactly the outcomes DESIGN §B6 gives it
// (FLT-5, FLT-9), for the next count operations in the order the faults were armed;
// fail_before never reaches the database, the other two run the operation and change only a
// committed outcome; every operation that reaches the database gets the caller's stop
// channel; a bad mode or count arms nothing.
func TestDBWrap_Modes(t *testing.T) {
	committed := dbclient.Result{ArrivalNs: 7_000, Class: dbclient.Committed}
	stop := make(chan struct{})
	hit := func(w *fixtures.DBWrap) dbclient.Result {
		return w.Hit(t.Context(), "u1", "r1", 7_000, stop)
	}

	db := &fakeHitDB{res: committed}
	w := fixtures.NewDBWrap(db)
	if got := hit(w); got != committed {
		t.Errorf("Hit with no fault armed = %+v, want the database's %+v", got, committed)
	}
	for _, arm := range []struct {
		mode  fixtures.DBMode
		count int
	}{{fixtures.FailBefore, 2}, {fixtures.FailAfterCommit, 1}, {fixtures.LoseCommitReply, 1}} {
		if err := w.Arm(arm.mode, arm.count); err != nil {
			t.Fatalf("Arm(%s, %d): %v", arm.mode, arm.count, err)
		}
	}
	if n := w.Armed(); n != 4 {
		t.Errorf("Armed() = %d, want 4", n)
	}
	want := []dbclient.Class{
		dbclient.FailedNoRow, dbclient.FailedNoRow, // fail_before × 2
		dbclient.FailedRowMayRemain, // fail_after_commit
		dbclient.Unknown,            // lose_commit_reply
		dbclient.Committed,          // nothing armed any more
	}
	calls := []int{1, 1, 2, 3, 4} // database calls after each Hit: fail_before sends nothing
	for i, class := range want {
		got := hit(w)
		if got.Class != class || (class != dbclient.Committed && got.Err == nil) {
			t.Errorf("Hit %d = %+v, want class %s with an error", i+1, got, class)
		}
		if class != dbclient.Committed && (got.ArrivalNs != 0 || got.Existed) {
			t.Errorf("Hit %d = %+v: a failed or unknown outcome carries no row", i+1, got)
		}
		if n := db.calls(); n != calls[i] {
			t.Errorf("after Hit %d the database saw %d calls, want %d", i+1, n, calls[i])
		}
	}
	if n := w.Armed(); n != 0 {
		t.Errorf("Armed() = %d after every fault was used, want 0", n)
	}
	for i, s := range db.stops {
		if s != stop {
			t.Errorf("database call %d got another stop channel, want the caller's", i+1)
		}
	}

	// fail_after_commit and lose_commit_reply leave an outcome that did not commit as it is.
	for _, mode := range []fixtures.DBMode{fixtures.FailAfterCommit, fixtures.LoseCommitReply} {
		for _, res := range []dbclient.Result{
			{Class: dbclient.FailedNoRow, Err: errors.New("refused")},
			{Class: dbclient.Cancelled, Err: errors.New("stopped")},
			{Class: dbclient.Unknown, Err: errors.New("lost")},
		} {
			w := fixtures.NewDBWrap(&fakeHitDB{res: res})
			if err := w.Arm(mode, 1); err != nil {
				t.Fatal(err)
			}
			if got := hit(w); got != res {
				t.Errorf("%s over %+v = %+v, want the database's outcome unchanged", mode, res, got)
			}
		}
	}

	for _, bad := range []struct {
		mode  fixtures.DBMode
		count int
	}{{"fail_sometimes", 1}, {"", 1}, {"refuse", 1}, {fixtures.FailBefore, 0}, {fixtures.LoseCommitReply, -1}} {
		if err := w.Arm(bad.mode, bad.count); err == nil {
			t.Errorf("Arm(%q, %d) succeeded, want an error", bad.mode, bad.count)
		}
	}
	if n := w.Armed(); n != 0 {
		t.Errorf("Armed() = %d after refused arms, want 0", n)
	}
}

// TestDBWrap_Concurrent: concurrent operations use up exactly the armed count.
func TestDBWrap_Concurrent(t *testing.T) {
	db := &fakeHitDB{res: dbclient.Result{ArrivalNs: 1_000, Class: dbclient.Committed}}
	w := fixtures.NewDBWrap(db)
	if err := w.Arm(fixtures.FailBefore, 30); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w.Hit(t.Context(), "u", "r", 1_000, nil).Class == dbclient.FailedNoRow {
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed != 30 || db.calls() != 70 {
		t.Errorf("%d operations failed before the insert and %d reached the database, want 30 and 70", failed, db.calls())
	}
}

// TestDBWrap_FaultCap: at most 64 armed database faults wait at once (plan choice; AGENTS.md
// §1 Queues): the 64th is armed, the 65th is refused and arms nothing, and once an operation
// has used up the oldest fault one more is armed, the one after it refused again.
func TestDBWrap_FaultCap(t *testing.T) {
	db := &fakeHitDB{res: dbclient.Result{ArrivalNs: 1_000, Class: dbclient.Committed}}
	w := fixtures.NewDBWrap(db)
	for i := range 64 {
		if err := w.Arm(fixtures.FailBefore, 1); err != nil {
			t.Fatalf("Arm %d of 64: %v", i+1, err)
		}
	}
	if err := w.Arm(fixtures.FailBefore, 1); err == nil {
		t.Error("the 65th Arm succeeded, want it refused while 64 faults wait")
	}
	if n := w.Armed(); n != 64 {
		t.Errorf("Armed() = %d after the refused Arm, want 64", n)
	}
	if got := w.Hit(t.Context(), "u", "r", 1_000, nil); got.Class != dbclient.FailedNoRow {
		t.Errorf("Hit = %+v, want the oldest fault's fail_before", got)
	}
	if err := w.Arm(fixtures.LoseCommitReply, 1); err != nil {
		t.Errorf("Arm once an operation used up the oldest fault: %v, want it armed", err)
	}
	if err := w.Arm(fixtures.FailBefore, 1); err == nil {
		t.Error("an Arm with 64 faults waiting again succeeded, want it refused")
	}
	if n := w.Armed(); n != 64 {
		t.Errorf("Armed() = %d, want 64", n)
	}
	// The faults run oldest first: 63 fail_before, then the lose_commit_reply armed last.
	for i := range 63 {
		if got := w.Hit(t.Context(), "u", "r", 1_000, nil); got.Class != dbclient.FailedNoRow {
			t.Fatalf("Hit %d = %+v, want fail_before", i+2, got)
		}
	}
	if got := w.Hit(t.Context(), "u", "r", 1_000, nil); got.Class != dbclient.Unknown {
		t.Errorf("the 65th Hit = %+v, want the lose_commit_reply armed after the cap freed", got)
	}
	if n := w.Armed(); n != 0 {
		t.Errorf("Armed() = %d after every fault was used, want 0", n)
	}
}
