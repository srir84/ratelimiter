package limiter_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/settings"
	"ratelimiter/internal/testutil/fakeresp"
	"ratelimiter/limiter"
)

// TestNewRejectsControlled: production construction refuses the controlled clock (DESIGN
// §A5.12, SPEC CMP-9, CFG-4): New returns a non-nil limiter that is permanently not ready
// (settings_invalid), the error names ClockSource and never the password, and nothing
// connects to the store. The same settings with the store clock pass validation, and New
// builds a valid limiter from them (§A5.2): it connects to the given store address and
// authenticates with the given user and password (a command arrives after AUTH), and its
// Health reports fixtures false.
func TestNewRejectsControlled(t *testing.T) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	pw := hex.EncodeToString(b)
	srv := fakeresp.Start(t, "rl_lib", pw)
	st := limiter.StoreOptions{Addr: srv.Addr(), User: "rl_lib", Password: pw}

	p := limiter.DefaultPolicy()
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 100_000, 50_000
	in := limiter.DefaultInstance(p)
	in.MaxReleaseDelayNs, in.MarginNs = 5_000_000_000, 1_000_000_000
	in.MaxHeldReceipts, in.MaxPendingRefunds = 1_152, 1_152
	in.EpochRereadIntervalNs = 50_000_000 // 50 ms: the wait below covers 3 re-read intervals
	if err := settings.Validate(p, in, false); err != nil {
		t.Fatalf("the store-clock settings must be valid: %v", err)
	}

	p.ClockSource = limiter.Controlled
	l, err := limiter.New(p, in, st)
	if l == nil {
		t.Fatal("New returned a nil limiter")
	}
	t.Cleanup(func() { l.Close() })
	var se *settings.Error
	if !errors.As(err, &se) || se.Field != "ClockSource" {
		t.Fatalf("New with ClockSource = Controlled: err = %v, want a settings error on ClockSource", err)
	}
	if strings.Contains(err.Error(), pw) {
		t.Fatalf("the error contains the store password: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // 3 re-read intervals: an invalid instance never joins
	if r := l.Acquire(t.Context(), "k"); r.Kind != limiter.NotReady || r.Diag.Reason != "settings_invalid" || r.Permit != nil {
		t.Fatalf("Acquire = %v %+v, want not_ready (settings_invalid) without a permit", r.Kind, r.Diag)
	}
	h := l.Health()
	if h.Status != "not_ready" || h.Reason != "settings_invalid" || h.ClockSource != "controlled" || h.Fixtures {
		t.Fatalf("Health = %+v, want not_ready, settings_invalid, clock_source controlled, fixtures false", h)
	}
	if n := srv.Accepted(); n != 0 {
		t.Fatalf("the store accepted %d connections, want 0", n)
	}

	p.ClockSource = limiter.Store
	valid, err := limiter.New(p, in, st)
	if valid == nil || err != nil {
		t.Fatalf("New with the store clock: limiter %v, err = %v; want a limiter, no error", valid != nil, err)
	}
	t.Cleanup(func() { valid.Close() })
	if h := valid.Health(); h.Fixtures || h.ClockSource != "store" {
		t.Fatalf("Health = %+v, want fixtures false, clock_source store", h)
	}
	deadline := time.Now().Add(5 * time.Second)
	for srv.Accepted() == 0 || srv.Count("EVALSHA") == 0 { // the watch's read_epoch, sent once AUTH passed
		if time.Now().After(deadline) {
			t.Fatalf("within 5 s: %d connections accepted, %d EVALSHA received; want both > 0", srv.Accepted(), srv.Count("EVALSHA"))
		}
		time.Sleep(time.Millisecond)
	}
}
