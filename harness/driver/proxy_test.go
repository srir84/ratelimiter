//go:build rltest

package driver

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/scripts"
	"ratelimiter/limiter"
)

// proxyGoneWait bounds the wait for a driver's proxies to end after its teardown (plan choice).
const proxyGoneWait = 5 * time.Second

// proxyStacks returns the stack of every running goroutine with a frame in package respproxy.
func proxyStacks() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "ratelimiter/harness/fixtures/respproxy.") {
			out = append(out, g)
		}
	}
	return out
}

// TestDriverClosesProxies (plan S9 step 6; AGENTS.md §2 Goroutines): the driver closes every
// proxy it started once its instances are closed. A subtest builds a driver whose instance is
// ViaProxy and allows one acquire through the proxy; once the subtest, its teardown included,
// has ended, no goroutine of a respproxy is left (polled for up to 5 s).
func TestDriverClosesProxies(t *testing.T) {
	t.Run("ViaProxy", func(t *testing.T) {
		d := New(t, Options{Instances: 1, PerInstance: []InstanceSpec{{ViaProxy: true}}})
		if d.Proxy(0) == nil {
			t.Fatal("Proxy(0) is nil for a ViaProxy instance")
		}
		d.Done(allowedOp(t, d, 0, "closes"))
	})
	deadline := time.Now().Add(proxyGoneWait)
	for {
		left := proxyStacks()
		if len(left) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%d goroutines of a respproxy still running %v after the driver's teardown:\n\n%s",
				len(left), proxyGoneWait, strings.Join(left, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDriverViaProxy (plan S9 step 6), with the epoch read once at startup and timed cleanup
// off (both every hour), so only the test's acquires are written: instance 1 is built ViaProxy,
// instance 0 is not.
// Proxy(0) is nil; Proxy(1) listens on 127.0.0.1 at another address than the store, and
// instance 1 joins and allows through it. An error reply injected at the proxy reaches
// instance 1 only: its acquire is outcome_unknown with the reply in Diag.StoreError, its one
// acquire command written (Sent: cmd "acquire"), and the key's bucket unchanged, since the
// store never ran it; instance 0's acquire of another key is allowed meanwhile. Sent names the
// SCRIPT LOAD and AUTH commands too: after a restart of the store (empty script cache), the
// first allowed acquire on instance 1 is acquire, SCRIPT LOAD, acquire, and its connections
// were dialed again (AUTH).
func TestDriverViaProxy(t *testing.T) {
	p := DefaultPolicy()
	p.CleanupIntervalNs = 3_600_000_000_000 // only the test's acquires are written (plan choice)
	in := DefaultInstance(p)
	in.EpochRereadIntervalNs = 3_600_000_000_000
	d := New(t, Options{Instances: 2, Policy: p, Instance: in, PerInstance: []InstanceSpec{{}, {ViaProxy: true}}})
	if d.Proxy(0) != nil {
		t.Error("Proxy(0) is set for an instance built without ViaProxy")
	}
	px := d.Proxy(1)
	if px == nil {
		t.Fatal("Proxy(1) is nil for a ViaProxy instance")
	}
	if px.Addr() == d.Store().Addr {
		t.Fatalf("the proxy's address %s is the store's", px.Addr())
	}
	if h := d.Limiter(1).Health(); h.Status != "ready" {
		t.Fatalf("instance 1 through the proxy: %s/%s (%s), want ready", h.Status, h.Reason, h.StoreError)
	}
	d.Done(allowedOp(t, d, 1, "via"))
	before := d.Inspect("via")

	const line = "READONLY You can't write against a read only replica."
	if err := px.InjectError(scripts.Acquire.Name, line, 1); err != nil {
		t.Fatal(err)
	}
	d.Done(allowedOp(t, d, 0, "direct")) // instance 0 does not go through the proxy
	mark := len(d.Sent(1))
	op := d.Acquire(1, "via")
	if op.Kind != limiter.OutcomeUnknown || op.Permit != nil || op.Diag.StoreError != line || op.SentCommands != 1 {
		t.Errorf("acquire with an injected error: %v %+v (permit %v, SentCommands %d), want outcome_unknown with StoreError %q, sent once",
			op.Kind, op.Diag, op.Permit != nil, op.SentCommands, line)
	}
	if n := Count(d.Sent(1)[mark:], "acquire"); n != 1 {
		t.Errorf("%d acquire sends for the injected acquire, want 1", n)
	}
	if after := d.Inspect("via"); !slices.Equal([]int64{after.U, after.G, after.S}, []int64{before.U, before.G, before.S}) {
		t.Errorf("bucket after the injected acquire: %+v, want unchanged %+v (the store never ran it)", after, before)
	}
	d.Done(allowedOp(t, d, 1, "via")) // the count is used up

	d.Store().Stop()
	d.RestartStore()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mark = len(d.Sent(1))
		op = d.Acquire(1, "via")
		if op.Kind == limiter.Allowed {
			break
		}
		if op.Kind != limiter.Unavailable || op.SentCommands != 0 || !time.Now().Before(deadline) {
			t.Fatalf("acquire after the store restart: %v (SentCommands %d, %+v), want unavailable with nothing sent until one is allowed (5 s)",
				op.Kind, op.SentCommands, op.Diag)
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.Done(op)
	var cmds []string
	for _, s := range d.Sent(1)[mark:] {
		if s.Bytes > 0 && s.Cmd != "AUTH" && s.Cmd != "PING" {
			cmds = append(cmds, s.Cmd)
		}
	}
	if want := []string{"acquire", "SCRIPT LOAD", "acquire"}; !slices.Equal(cmds, want) {
		t.Errorf("the first allowed acquire after the restart sent %q (AUTH and PING left out), want %q", cmds, want)
	}
	if Count(d.Sent(1), "AUTH") < 2 {
		t.Errorf("instance 1 sent %d AUTH, want its first dial's and a redial's at least", Count(d.Sent(1), "AUTH"))
	}
}
