//go:build rltest

package spectest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/driver"
	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/limiter"
)

// leaks returns the places among texts that hold secret.
func leaks(secret string, texts map[string]string) []string {
	var out []string
	for where, s := range texts {
		if strings.Contains(s, secret) {
			out = append(out, where)
		}
	}
	return out
}

// diagnostics gathers what a host may log or serve about l and one result: the New error
// text, Health (as formatted and as JSON), Metrics and the result's Diag.
func diagnostics(t *testing.T, newErr error, l *limiter.Limiter, r limiter.Result) map[string]string {
	t.Helper()
	h := l.Health()
	js, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{
		"Health":      fmt.Sprintf("%+v", h),
		"Health JSON": string(js),
		"Metrics":     fmt.Sprintf("%+v", l.Metrics()),
		"Diag":        fmt.Sprintf("%+v", r.Diag),
	}
	if newErr != nil {
		texts["New error"] = newErr.Error()
	}
	return texts
}

// TestNoPasswordInDiagnostics (AGENTS.md §1 Secrets; SPEC CFG-7): a limiter with a wrong store
// password (the store answers WRONGPASS) and limiters with the right one never show the
// password in New's error, Health, Metrics or a failed acquire's Diag: the wrong-password
// instance once its failed reads set Health's store_error, the right-password instance on a
// refused and a lost-reply acquire, and both with invalid settings (New's error).
func TestNoPasswordInDiagnostics(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 1})
	right := d.Store().PW.Lib
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	wrong := hex.EncodeToString(raw[:])
	wrongStore := limiter.StoreOptions{Addr: d.Store().Addr, User: "rl_lib", Password: wrong}

	// A wrong password: AUTH fails, the instance never joins, Health reports the store error.
	i, err := d.AddInstance(driver.InstanceSpec{Store: &wrongStore})
	if err != nil {
		t.Fatal(err)
	}
	l := d.Limiter(i)
	deadline := time.Now().Add(5 * time.Second)
	for l.Health().StoreError == "" {
		if time.Now().After(deadline) {
			t.Fatal("the wrong-password instance reported no store error within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	op := d.Acquire(i, "pw")
	if op.Kind != limiter.NotReady {
		t.Errorf("wrong password: %v, want not_ready", op.Kind)
	}
	t.Logf("wrong password: health store_error %q", l.Health().StoreError)
	for _, secret := range []string{wrong, right} {
		if w := leaks(secret, diagnostics(t, err, l, op.Result)); len(w) != 0 {
			t.Errorf("wrong-password instance: a password appears in %v", w)
		}
	}

	// The right password: a refused acquire (unavailable) and a lost reply (outcome_unknown).
	sw := d.StoreWrapper(0)
	for _, f := range []fixtures.Fault{
		{Mode: fixtures.Refuse, Match: "acquire", Count: 1},
		{Mode: fixtures.DropReply, Match: "acquire", Count: 1},
	} {
		if err := sw.Arm(f); err != nil {
			t.Fatal(err)
		}
		op := d.Acquire(0, "pw")
		want := map[fixtures.Mode]limiter.ResultKind{fixtures.Refuse: limiter.Unavailable, fixtures.DropReply: limiter.OutcomeUnknown}[f.Mode]
		if op.Kind != want {
			t.Errorf("%s: %v, want %v", f.Mode, op.Kind, want)
		}
		if w := leaks(right, diagnostics(t, nil, d.Limiter(0), op.Result)); len(w) != 0 {
			t.Errorf("%s: the password appears in %v", f.Mode, w)
		}
	}

	// Invalid settings with either password: New's error never shows it.
	in := d.Instance()
	in.MarginNs = 1000
	for _, c := range []struct {
		name   string
		store  *limiter.StoreOptions
		secret string
	}{{"right password", nil, right}, {"wrong password", &wrongStore, wrong}} {
		i, err := d.AddInstance(driver.InstanceSpec{Instance: &in, Store: c.store})
		if err == nil {
			t.Fatalf("%s: invalid settings gave no error", c.name)
		}
		op := d.Acquire(i, "pw")
		if w := leaks(c.secret, diagnostics(t, err, d.Limiter(i), op.Result)); len(w) != 0 {
			t.Errorf("%s, invalid settings: the password appears in %v", c.name, w)
		}
	}
}
