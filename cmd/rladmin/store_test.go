package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/testutil/fakeresp"
)

// TestDialCancelEndsWait's cancel points and its bound.
const (
	sweepDials = 300                  // the dials cancelled a set time after they start
	sweepStep  = 1 * time.Microsecond // how much later each of those is cancelled than the one before
	promptWait = 2 * time.Second      // how soon after its cancel a dial must have returned
	dialWait   = 30 * time.Second     // how long the last dial may take to get its AUTH to the store
	ctxWait    = time.Minute          // each dial's deadline, which only a failed dial waits for
)

// TestDialCancelEndsWait (F1; DESIGN §A6 exit codes): cancelling dial's ctx, as a signal does,
// ends its wait at once with ctx's error, wherever in dial the cancel lands. The fake store
// accepts, reads AUTH and never answers it (HangAuth), and each ctx's deadline is a minute
// away, so only the cancel can end a dial. The i-th of 300 dials is cancelled i µs after it
// starts, so the cancels sweep its first 300 µs: the TCP connect (about 0.1 ms here), the AUTH
// it sends and the wait for the reply. One more dial is cancelled once its AUTH has reached
// the store. Each must return within 2 s of its cancel, with no connection and an error that
// is context.Canceled (run makes any dial error exit 3). A dial still waiting then fails the
// test, and closing the store's side of its connection releases it.
//
// A dial whose cancel only sets a past deadline fails here every run, by its error: the AUTH
// read's timeout, not ctx's error. Its window, a cancel landing just before resp.Handshake
// sets its own deadline, which replaces the past one so that the dial waits out its deadline,
// is narrow: the sweep lands in it in some runs only, most often under -race.
func TestDialCancelEndsWait(t *testing.T) {
	srv := fakeresp.Start(t, "rl_admin", testPW)
	srv.HangAuth()
	var slowest time.Duration
	for i := range sweepDials {
		at := time.Duration(i) * sweepStep
		took := dialCancelled(t, srv, "with its cancel planned "+at.String()+" after it started", func(began time.Time, _ <-chan struct{}) {
			for time.Since(began) < at {
				yieldIfAlone()
			}
		})
		slowest = max(slowest, took)
	}
	last := fakeresp.Start(t, "rl_admin", testPW) // counts only the last dial's AUTH
	last.HangAuth()
	took := dialCancelled(t, last, "with its cancel planned once its AUTH reached the store", func(began time.Time, done <-chan struct{}) {
		for last.Count("AUTH") == 0 && time.Since(began) < dialWait {
			select {
			case <-done: // it ended before its AUTH arrived: the checks report how
				return
			default:
				runtime.Gosched()
			}
		}
	})
	t.Logf("the slowest of %d cancelled dials returned %v after its cancel", sweepDials+1, max(slowest, took))
}

// dialCancelled runs dial against srv, which never answers AUTH, cancels its ctx once
// untilCancel(began, done) returns (began: when dial was called; done closes once dial has
// returned), and ends the test unless dial returned within promptWait of that cancel, with no
// connection and ctx's error. It returns how long dial took to return after the cancel. Each
// failure names when the cancel actually came after the dial's start, which can be later than
// planned under load, and how many AUTH commands srv received during the dial: srv may be
// shared by many dials, so this counts from the dial's start and may include a late AUTH of
// the dial before.
func dialCancelled(t *testing.T, srv *fakeresp.Server, what string, untilCancel func(began time.Time, done <-chan struct{})) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), ctxWait)
	defer cancel()
	type outcome struct {
		cancelled time.Time // when cancel returned
		released  bool      // promptWait passed first; closing the store's side ended the dial
	}
	var began atomic.Pointer[time.Time]
	running := make(chan struct{}) // closed once the canceller runs
	done := make(chan struct{})    // closed once dial has returned
	out := make(chan outcome, 1)
	go func() { // the canceller; the test waits for its outcome below
		close(running)
		for began.Load() == nil {
			yieldIfAlone()
		}
		untilCancel(*began.Load(), done)
		cancel()
		o := outcome{cancelled: time.Now()}
		timer := time.NewTimer(promptWait)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			o.released = true
			srv.DropConns() // the dial's read then sees the connection closed
		}
		out <- o
	}()
	<-running // so the canceller already spins when dial starts
	auths := srv.Count("AUTH")
	start := time.Now()
	began.Store(&start)
	c, err := dial(ctx, srv.Addr(), "rl_admin", testPW)
	ended := time.Now()
	close(done)
	o := <-out
	if c != nil {
		c.close()
	}
	// The actual cancel: when cancel returned, after the dial's start; and the AUTH commands
	// srv received since the dial started.
	actual := fmt.Sprintf("cancelled %v after it started, %d AUTH received meanwhile", o.cancelled.Sub(start), srv.Count("AUTH")-auths)
	switch {
	case o.released:
		t.Fatalf("dial %s (%s) still waited %v after its cancel, want it ended at once; closing the store's side of the connection then ended it with %v",
			what, actual, promptWait, err)
	case c != nil:
		t.Fatalf("dial %s (%s) connected, want an error: the store never answers AUTH", what, actual)
	case !errors.Is(err, context.Canceled):
		t.Fatalf("dial %s (%s) failed with %v, want ctx's error, context.Canceled", what, actual, err)
	}
	return ended.Sub(o.cancelled)
}

// yieldIfAlone lets other goroutines run when this one is the only one that can: a spin loop
// then still ends at its time without holding the only P (GOMAXPROCS 1) until it is preempted.
func yieldIfAlone() {
	if runtime.GOMAXPROCS(0) == 1 {
		runtime.Gosched()
	}
}
