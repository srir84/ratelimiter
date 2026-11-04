//go:build rltest

package fixtures

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/testutil/fakeresp"
)

// TestCheckPID: checkPID refuses every PID a fault must never signal — 0 and -1 (kill(2) would
// reach the caller's process group, or every process the user owns), 1 (init or launchd), this
// process, and a value a pid_t cannot hold (MaxInt32+1 would wrap) — and accepts 2 and
// MaxInt32 (plan choice, IMPLEMENTATION_PLAN H4 steps 1–2).
func TestCheckPID(t *testing.T) {
	cases := []struct {
		name string
		pid  int
		ok   bool
	}{
		{"zero", 0, false},
		{"one", 1, false},
		{"minus_one", -1, false},
		{"this_process", os.Getpid(), false},
		{"above_max_int32", math.MaxInt32 + 1, false},
		{"two", 2, true},
		{"max_int32", math.MaxInt32, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkPID(c.pid); (err == nil) != c.ok {
				t.Errorf("checkPID(%d) = %v, want accepted %v", c.pid, err, c.ok)
			}
		})
	}
}

// TestDialOpsCancelEndsWait's times: the dial's deadline, which only a wait that the cancel did
// not end reaches; how soon after its call authenticate must have returned; and how long the
// cancel may take to act on the connection.
const (
	cancelDeadline = 3 * time.Second
	cancelPrompt   = time.Second
	cancelActWait  = 10 * time.Second
)

// cancelPassword is the fake store's password: no error may hold it.
const cancelPassword = "dial-cancel-test-password"

// TestDialOpsCancelEndsWait (AGENTS.md §2 Context): cancelling the ctx of dialOps's handshake
// step, authenticate, ends its wait at once with ctx's error, even when the cancel lands after
// authenticate has registered its reaction to it and before resp.Handshake has set its own
// deadline. The fake store reads AUTH and never answers it (HangAuth), and the deadline is 3 s
// away. The connection is a cancelConn, which puts the cancel in that window every run:
// Handshake's SetDeadline cancels ctx and waits until the cancel has acted on the connection
// before it sets the deadline. authenticate must return within 1 s with context.Canceled, an
// error without the password, and the connection closed. A cancel that only sets a past
// deadline is replaced by Handshake's own, so authenticate then waits out the 3 s.
func TestDialOpsCancelEndsWait(t *testing.T) {
	const user = "rl_ops"
	srv := fakeresp.Start(t, user, cancelPassword)
	srv.HangAuth()
	ctx, cancel := context.WithTimeout(t.Context(), cancelDeadline)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() }) // closed by authenticate already, unless the test failed
	c := newCancelConn(nc, cancel)
	began := time.Now()
	err = authenticate(ctx, c, user, cancelPassword)
	took := time.Since(began)
	late, closed := c.state()
	switch {
	case late:
		t.Fatalf("authenticate: the cancel did not act on the connection (a Close or a SetDeadline) within %v; authenticate returned %v after %v",
			cancelActWait, err, took)
	case took > cancelPrompt:
		t.Fatalf("authenticate returned %v after its call, want within %v: the cancel, landed before resp.Handshake set its deadline, "+
			"did not end the wait for the AUTH reply (it returned %v)", took, cancelPrompt, err)
	case !errors.Is(err, context.Canceled):
		t.Fatalf("authenticate after its ctx was cancelled: %v, want ctx's error, context.Canceled", err)
	case strings.Contains(err.Error(), cancelPassword):
		t.Fatal("authenticate's error holds the password")
	case !closed:
		t.Fatal("authenticate failed and left the connection open")
	}
}

// cancelConn is a connection whose first SetDeadline (resp.Handshake's, the first call
// authenticate makes on it) cancels the dial's ctx and then waits until that cancel has acted
// on the connection (a Close, or a later SetDeadline: what a context.AfterFunc on ctx does), at
// most cancelActWait, before it sets the deadline it was asked for.
type cancelConn struct {
	net.Conn
	cancel context.CancelFunc
	acted  chan struct{} // closed once a Close or a later SetDeadline has reached the connection
	once   sync.Once

	mu        sync.Mutex
	deadlines int  // SetDeadline calls so far
	late      bool // the first SetDeadline waited cancelActWait for the cancel to act
	closed    bool // Close was called
}

func newCancelConn(nc net.Conn, cancel context.CancelFunc) *cancelConn {
	return &cancelConn{Conn: nc, cancel: cancel, acted: make(chan struct{})}
}

func (c *cancelConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	c.deadlines++
	first := c.deadlines == 1
	c.mu.Unlock()
	if !first {
		err := c.Conn.SetDeadline(d)
		c.act()
		return err
	}
	c.cancel()
	wait := time.NewTimer(cancelActWait)
	defer wait.Stop()
	select {
	case <-c.acted:
	case <-wait.C:
		c.mu.Lock()
		c.late = true
		c.mu.Unlock()
	}
	return c.Conn.SetDeadline(d)
}

func (c *cancelConn) Close() error {
	err := c.Conn.Close()
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.act()
	return err
}

// act records that something acted on the connection, once it has reached it.
func (c *cancelConn) act() { c.once.Do(func() { close(c.acted) }) }

// state reports whether the cancel failed to act within cancelActWait and whether the
// connection was closed.
func (c *cancelConn) state() (late, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.late, c.closed
}
