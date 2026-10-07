//go:build rltest

// Package fixtures holds the test-build fixtures of DESIGN §B6 (HARNESS §9.4): the
// controlled clock driver, the store-adapter wrapper (refuse, drop reply, delay, barriers),
// the signer wrapper (fail, unusable, corrupt) and the state tamper tool (FLT-7, FLT-11).
// Every file carries //go:build rltest, so no benchmark or production build can reach them.
// Shapes are plan choices (IMPLEMENTATION_PLAN S5 step 9, S6 step 10, S8 step 4).
package fixtures

import (
	"fmt"
	"strconv"
	"sync"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storeproc"
)

// Error is a fixture error with a fixed text (plan choice): its values are constants,
// compared with errors.Is.
type Error string

func (e Error) Error() string { return string(e) }

// The clock's refusals: it writes nothing for them.
const (
	// ErrNegative: a time or a step below 0, or a step back past 0.
	ErrNegative Error = "fixtures: controlled time must not be negative"
	// ErrNotMicro: a value that is not a whole number of µs (the store's granularity, §B6).
	ErrNotMicro Error = "fixtures: controlled time must be a multiple of 1,000 ns"
	// ErrUnset: Advance or Back before the first Set.
	ErrUnset Error = "fixtures: controlled time not set yet"
	// ErrOverflow: a step that would leave int64.
	ErrOverflow Error = "fixtures: controlled time step overflows"
)

// ClockConn is the one method Clock needs from a *storeproc.Client connected as rl_fixture
// (plan choice), so a test can stand in for the store.
type ClockConn interface {
	Do(args ...string) (storeproc.Reply, error)
}

// Clock is the controlled clock driver (DESIGN §B6): it takes ns and writes rl:ctl:time in µs
// through an rl_fixture connection. It rejects negative values and values that are not a
// multiple of 1,000 ns, writing nothing for them. It is safe for concurrent use.
type Clock struct {
	mu    sync.Mutex
	conn  ClockConn
	ns    int64
	set   bool
	onSet func(ns int64) // runs after each successful write, under the clock's lock
}

// NewClock returns a clock writing through conn. onSet, when not nil, hears every value
// written, in order (the driver's op log); it must not call the clock.
func NewClock(conn ClockConn, onSet func(ns int64)) *Clock {
	return &Clock{conn: conn, onSet: onSet}
}

// Set writes the controlled time ns.
func (c *Clock) Set(ns int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.write(ns)
}

// Advance moves the controlled time forward by ns (0 rewrites the same value).
func (c *Clock) Advance(ns int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkNs(ns); err != nil {
		return err
	}
	if !c.set {
		return ErrUnset
	}
	if c.ns > maxInt64-ns {
		return ErrOverflow
	}
	return c.write(c.ns + ns)
}

// Back moves the controlled time back by ns; the result must stay at or above 0.
func (c *Clock) Back(ns int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkNs(ns); err != nil {
		return err
	}
	if !c.set {
		return ErrUnset
	}
	return c.write(c.ns - ns)
}

// NowNs returns the last value written (0 before the first Set).
func (c *Clock) NowNs() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ns
}

const maxInt64 = 1<<63 - 1

// checkNs is the value rule of DESIGN §B6: not negative, a whole number of µs.
func checkNs(ns int64) error {
	if ns < 0 {
		return ErrNegative
	}
	if ns%1000 != 0 {
		return ErrNotMicro
	}
	return nil
}

// write stores ns as rl:ctl:time (µs) and records it; c.mu is held.
func (c *Clock) write(ns int64) error {
	if err := checkNs(ns); err != nil {
		return err
	}
	r, err := c.conn.Do("SET", keys.CtlTimeKey, strconv.FormatInt(ns/1000, 10))
	if err != nil {
		return fmt.Errorf("fixtures: writing the controlled time: %w", err)
	}
	if r.Kind != '+' || r.Str != "OK" {
		return fmt.Errorf("fixtures: writing the controlled time: reply %s", r)
	}
	c.ns, c.set = ns, true
	if c.onSet != nil {
		c.onSet(ns)
	}
	return nil
}
