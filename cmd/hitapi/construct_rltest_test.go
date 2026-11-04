//go:build rltest

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/internal/testutil"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
	"github.com/srir84/ratelimiter/limiter"
)

const (
	// rladminWait bounds rladmin start-epoch, its own 30 s deadline included.
	rladminWait = 45 * time.Second
	// readyWait bounds the wait for the limiter to join the epoch (Health status ready).
	readyWait = 10 * time.Second
)

// TestConstruct_FixturesWired (IMPLEMENTATION_PLAN H4 step 5; DESIGN §B5 "Construction",
// §A5.12, §A5.4 test hook): the test build's construct, against a real store (storeproc) with
// an epoch rladmin started, wires the fixtures the /_test routes drive into its limiter. The
// send observer GET /_test/sends reads (b.fixtures.Sends) hears the limiter's writes: after an
// allowed Acquire it holds AUTH and acquire records with bytes written. The signer wrapper the
// route arms (b.fixtures.Signer) is the limiter's: with one unusable fault armed on it, the next
// Acquire is config_error with no permit, and the one after it allowed again.
func TestConstruct_FixturesWired(t *testing.T) {
	ctx := t.Context()
	st := storeproc.Start(t, storeproc.Options{})
	data, err := os.ReadFile(harnessTOML)
	if err != nil {
		t.Fatal(err)
	}
	f, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	f.Store.Addr = st.Addr
	out, err := config.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	bin, err := testutil.RLAdminBin()
	if err != nil {
		t.Fatal(err)
	}
	actx, cancel := context.WithTimeout(ctx, rladminWait)
	defer cancel()
	cmd := exec.CommandContext(actx, bin, "start-epoch", "--policy", cfgPath, "--json")
	cmd.Env = []string{"RL_STORE_ADDR=" + st.Addr, "RL_ADMIN_PASSWORD=" + st.PW.Admin}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rladmin start-epoch: %v: %s", err, out)
	}

	f.Store.Password = st.PW.Lib
	b, err := construct(f, nil, func(limiter.RefundEvent) {})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	t.Cleanup(func() { b.lim.Close() })
	if b.fixtures.Sends == nil || b.fixtures.Signer == nil {
		t.Fatalf("construct's fixtures: send observer %v, signer wrapper %v; want both", b.fixtures.Sends, b.fixtures.Signer)
	}
	for limit := time.Now().Add(readyWait); b.lim.Health().Status != "ready"; time.Sleep(10 * time.Millisecond) {
		if !time.Now().Before(limit) {
			t.Fatalf("the limiter is not ready %v after construct: %+v", readyWait, b.lim.Health())
		}
	}

	acquire := func(key string, want limiter.ResultKind) {
		t.Helper()
		r := b.lim.Acquire(ctx, key)
		if r.Kind != want {
			t.Fatalf("Acquire(%s) = %v (%+v), want %v", key, r.Kind, r.Diag, want)
		}
		if (r.Permit != nil) != (want == limiter.Allowed) {
			t.Fatalf("Acquire(%s) = %v with permit %v", key, r.Kind, r.Permit)
		}
		if r.Permit != nil {
			r.Permit.Done()
		}
	}
	acquire("k1", limiter.Allowed)
	var auths, acquires int
	for _, s := range b.fixtures.Sends.Since(0) {
		switch {
		case s.Cmd == "AUTH" && s.BytesWritten > 0:
			auths++
		case s.Cmd == "acquire" && s.BytesWritten > 0:
			acquires++
		}
	}
	if auths == 0 || acquires == 0 {
		t.Errorf("the send observer holds %d AUTH and %d acquire records with bytes written, want at least 1 of each: %+v",
			auths, acquires, b.fixtures.Sends.Since(0))
	}

	if err := b.fixtures.Signer.Arm(fixtures.SignerFault{Mode: fixtures.SignUnusable, Count: 1}); err != nil {
		t.Fatal(err)
	}
	acquire("k2", limiter.ConfigError)
	acquire("k3", limiter.Allowed)
}
