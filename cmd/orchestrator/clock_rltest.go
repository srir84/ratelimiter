//go:build rltest

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/internal/store/resp"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// clockTimeout bounds one rl_fixture exchange on the local store: a dial, AUTH and the few
// commands of setting the controlled clock, a clock step or a tamper operation (plan choice).
const clockTimeout = 10 * time.Second

// buildRefusals is this build's part of preflight: an rltest orchestrator drives the
// controlled clock in-process (setControlledClock) and runs the faultctl cmds in-process too
// (runFaultctl), refusing only those it could not run (faultctlRefusals).
func buildRefusals(sc config.Scenario, controlled bool) []refusal {
	return faultctlRefusals(sc, controlled)
}

// setControlledClock sets a controlled-clock run's time to now, truncated to µs, in ns, through
// the S5 clock driver (fixtures.Clock.Set) on an rl_fixture connection to the store at addr,
// in-process (IMPLEMENTATION_PLAN H3 step 3; DESIGN §B6 "Controlled time"; HARNESS §4 step 2):
// the run sequence calls it before rladmin start-epoch, so H3 needs no faultctl. It returns the
// value written. The whole call ends within clockTimeout or when ctx ends.
func setControlledClock(ctx context.Context, addr, fixturePassword string, now time.Time) (int64, error) {
	ns := now.UnixMicro() * 1_000
	err := withFixtureConn(ctx, addr, fixturePassword, func(c fixtureConn) error {
		return fixtures.NewClock(c, nil).Set(ns)
	})
	if err != nil {
		return 0, fmt.Errorf("setting the controlled clock: %w", err)
	}
	return ns, nil
}

// withFixtureConn runs fn on an rl_fixture connection to the store at addr (DESIGN §A2): the
// clock driver's and the tamper tool's. The dial, AUTH and fn's commands end within
// clockTimeout or when ctx ends.
func withFixtureConn(ctx context.Context, addr, fixturePassword string, fn func(fixtureConn) error) error {
	ctx, cancel := context.WithTimeout(ctx, clockTimeout)
	defer cancel()
	c, err := dialStore(ctx, addr, userFixture, fixturePassword)
	if err != nil {
		return err
	}
	defer c.close()
	return fn(fixtureConn{c})
}

// fixtureConn is the clock driver's connection, a fixtures.ClockConn: the store connection as
// rl_fixture (DESIGN §A2), its replies in the storeproc.Reply form the fixtures take.
type fixtureConn struct{ c *storeConn }

// Do sends one command and returns its reply, an error reply included.
func (f fixtureConn) Do(args ...string) (storeproc.Reply, error) {
	v, err := f.c.do(args...)
	if err != nil {
		return storeproc.Reply{}, err
	}
	return toReply(v), nil
}

// toReply converts a RESP2 value to the storeproc.Reply form the fixtures take.
func toReply(v resp.Value) storeproc.Reply {
	r := storeproc.Reply{Str: v.Str, Int: v.Int, Nil: v.Null}
	switch v.Kind {
	case resp.Simple:
		r.Kind = '+'
	case resp.Error:
		r.Kind = '-'
	case resp.Int:
		r.Kind = ':'
	case resp.Bulk:
		r.Kind = '$'
	case resp.Array:
		r.Kind = '*'
		for _, e := range v.Elems {
			r.Arr = append(r.Arr, toReply(e))
		}
	}
	return r
}
