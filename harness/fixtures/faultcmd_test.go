//go:build rltest

package fixtures_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storeproc"
)

// TestParseFaultCommand (IMPLEMENTATION_PLAN H4 step 3): the store and api commands parse to
// their fault and arguments, the command line kept as given; a missing, extra or unknown word
// and a value out of range are ErrUsage.
func TestParseFaultCommand(t *testing.T) {
	for _, c := range []struct {
		line string
		want fixtures.FaultCommand
	}{
		{"store down 4s", fixtures.FaultCommand{Op: fixtures.OpStoreDown, Duration: 4 * time.Second}},
		{"store pause 4.5s", fixtures.FaultCommand{Op: fixtures.OpStorePause, Duration: 4500 * time.Millisecond}},
		{"store down 5m", fixtures.FaultCommand{Op: fixtures.OpStoreDown, Duration: 5 * time.Minute}},
		{"api kill 2", fixtures.FaultCommand{Op: fixtures.OpAPIKill, Instance: 2}},
		{"api kill 3 --restart", fixtures.FaultCommand{Op: fixtures.OpAPIKill, Instance: 3, Restart: true}},
		{"clock set 0", fixtures.FaultCommand{Op: fixtures.OpClockSet}},
		{"clock back 1000", fixtures.FaultCommand{Op: fixtures.OpClockBack, Ns: 1000}},
	} {
		c.want.Line = c.line
		got, err := fixtures.ParseFaultCommand(strings.Fields(c.line))
		if err != nil || got != c.want {
			t.Errorf("ParseFaultCommand(%q) = %+v, %v; want %+v", c.line, got, err, c.want)
		}
	}
	for _, line := range []string{
		"", "store", "store down", "store down 4s 5s", "store up 4s", "store down 0s", "store down -1s",
		"store down 5m1s", "store down 4", "store pause soon",
		"api", "api kill", "api kill 0", "api kill 4", "api kill two", "api kill 2 restart",
		"api kill 2 --restart now", "api stop 1",
		"tamper", "clock advance", "clock set 1000 2000", "faultctl store down 4s", "sleep 4s",
	} {
		if got, err := fixtures.ParseFaultCommand(strings.Fields(line)); !errors.Is(err, fixtures.ErrUsage) {
			t.Errorf("ParseFaultCommand(%q) = %+v, %v; want ErrUsage", line, got, err)
		}
	}
}

// TestClockLoad (IMPLEMENTATION_PLAN H4 step 3): a clock driver in a new process loads the
// controlled time an earlier driver wrote (GET as rl_fixture) without writing it, then steps
// from it; a missing key is ErrUnset, and a stored value that is not a µs count from 0 whose
// ns fit an int64 is refused; either way the clock stays as it was.
func TestClockLoad(t *testing.T) {
	st := storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
	conn := func() *storeproc.Client {
		t.Helper()
		c, err := st.Conn("rl_fixture")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	raw := conn()
	var heard []int64
	loaded := fixtures.NewClock(conn(), func(ns int64) { heard = append(heard, ns) })
	if err := loaded.Load(); !errors.Is(err, fixtures.ErrUnset) {
		t.Errorf("Load with no rl:ctl:time: %v, want ErrUnset", err)
	}
	if err := loaded.Advance(1000); !errors.Is(err, fixtures.ErrUnset) {
		t.Errorf("Advance after a failed Load: %v, want ErrUnset", err)
	}

	const T = int64(1_791_374_400_000_000_000)
	if err := fixtures.NewClock(conn(), nil).Set(T); err != nil {
		t.Fatal(err)
	}
	if err := loaded.Load(); err != nil || loaded.NowNs() != T {
		t.Fatalf("Load after another driver's Set(%d): NowNs %d, %v; want %d", T, loaded.NowNs(), err, T)
	}
	if len(heard) != 0 {
		t.Errorf("onSet heard %v on a Load, want nothing: Load writes nothing", heard)
	}
	if err := loaded.Advance(2000); err != nil {
		t.Fatal(err)
	}
	if r, err := raw.Do("GET", keys.CtlTimeKey); err != nil || r.Str != "1791374400000002" {
		t.Errorf("rl:ctl:time after Load and Advance(2000) = %s, %v; want 1791374400000002", r, err)
	}

	for _, v := range []string{"abc", "-5", "1.5", "", "9223372036854776"} {
		if r, err := raw.Do("SET", keys.CtlTimeKey, v); err != nil || r.Str != "OK" {
			t.Fatalf("SET rl:ctl:time %q: %s, %v", v, r, err)
		}
		if err := loaded.Load(); err == nil || loaded.NowNs() != T+2000 {
			t.Errorf("Load of rl:ctl:time %q: NowNs %d, %v; want an error and NowNs %d kept", v, loaded.NowNs(), err, T+2000)
		}
	}
}
