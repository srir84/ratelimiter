//go:build rltest

package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
)

// Fixed values of the L1 tests.
const (
	epochA  = "0123456789abcdef0123456789abcdef" // the epoch the fake store reports
	epochB  = "fedcba9876543210fedcba9876543210" // a replacement epoch
	epochC  = "00112233445566778899aabbccddeeff" // an epoch with another fingerprint
	otherFP = "ffffffffffffffffffffffffffffffff" // a fingerprint no test policy has
	tNow    = 1_760_000_000_000_000              // a budget time, µs since the Unix epoch
	lifeUs  = 60_000_000                         // the default receipt lifetime, µs
	hourNs  = 3_600_000_000_000                  // a re-read interval that keeps the watch to its startup read
	waitMax = 5 * time.Second                    // the bound of every wait for the watch
)

// testPolicy is the default policy with the harness limits (§A5.10).
func testPolicy(mode FailureMode) Policy {
	p := settings.DefaultPolicy()
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 100_000, 50_000
	p.FailureMode = mode
	return p
}

// testInstance is the default instance with the harness values (§A5.10), 4 slots of each
// kind and a 1 h re-read interval, so the watch reads once at startup and tests drive the
// rest through read.
func testInstance(p Policy) Instance {
	in := settings.DefaultInstance(p)
	in.MaxReleaseDelayNs, in.MarginNs = 5_000_000_000, 1_000_000_000
	in.MaxHeldReceipts, in.MaxPendingRefunds = 4, 4
	in.EpochRereadIntervalNs = hourNs
	return in
}

func intVal(n int64) resp.Value  { return resp.Value{Kind: resp.Int, Int: n} }
func strVal(s string) resp.Value { return resp.Value{Kind: resp.Bulk, Str: s} }

// reply builds a decoded reply {code, t, reason, v…}: an int or int64 becomes an integer, a
// string a bulk string.
func reply(code, t int64, reason string, vals ...any) store.Reply {
	elems := []resp.Value{intVal(code), intVal(t), strVal(reason)}
	for _, v := range vals {
		switch v := v.(type) {
		case int:
			elems = append(elems, intVal(int64(v)))
		case int64:
			elems = append(elems, intVal(v))
		case string:
			elems = append(elems, strVal(v))
		}
	}
	return store.Reply{Code: code, TUs: t, Reason: reason, Elems: elems}
}

// okResp is an OK response carrying reply(code, t, reason, vals…).
func okResp(code, t int64, reason string, vals ...any) store.Response {
	return store.Response{Outcome: store.OK, Reply: reply(code, t, reason, vals...), Sent: true, SentCommands: 1}
}

// epochOK is a state-0 read_epoch response: {0, 0, ”, id, fp, hwm, gen, clock_us, nb, nm, q}.
func epochOK(id, fp string, hwm, gen int64) store.Response {
	return okResp(0, 0, "", id, fp, hwm, gen, int64(tNow), 0, 0, 0)
}

// epochErr is a state-22 read_epoch response with reason and no readable field.
func epochErr(reason string) store.Response {
	return okResp(22, 0, reason, "", "", 0, 0, 0, 0, 0, "")
}

// allowedResp is an E_ALLOWED acquire response at tNow with g and s.
func allowedResp(g, s int64) store.Response {
	return okResp(1, tNow, "", int64(19_000_000_000), g, s, int64(tNow+lifeUs))
}

// fakeStore is the fake adapter of the L1 tests (NewWithHooks with a store wrapper): it
// answers read_epoch with the response setEpoch set, acquire through the acquire function
// (unavailable when none), and hold with the response setHold set ({40, 0, ”, 0} at first),
// and records every request. It replaces the real adapter, which wrap closes before it
// dials, so an L1 limiter on the fake opens no connection.
type fakeStore struct {
	mu      sync.Mutex
	epoch   store.Response
	hold    store.Response
	acquire func(store.Request) store.Response
	reqs    []store.Request
}

func newFake(epoch store.Response, acquire func(store.Request) store.Response) *fakeStore {
	return &fakeStore{epoch: epoch, hold: okResp(40, 0, "", 0), acquire: acquire}
}

// wrap is the store wrapper: it closes the real adapter at once (newLimiter holds the
// adapter's dials until the wrapper returns, so none has begun) and returns the fake.
func (f *fakeStore) wrap(inner store.Adapter) store.Adapter {
	inner.Close()
	return f
}

func (f *fakeStore) setEpoch(r store.Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epoch = r
}

func (f *fakeStore) setHold(r store.Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = r
}

func (f *fakeStore) setAcquire(fn func(store.Request) store.Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquire = fn
}

func (f *fakeStore) Call(_ context.Context, req store.Request) store.Response {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	ep, hd, acq := f.epoch, f.hold, f.acquire
	f.mu.Unlock()
	switch req.Script.Name {
	case "read_epoch":
		return ep
	case "acquire":
		if acq == nil {
			return store.Response{Outcome: store.Unavailable}
		}
		return acq(req)
	case "hold":
		return hd
	}
	return store.Response{Outcome: store.OutcomeUnknown}
}

func (f *fakeStore) CallBatch(_ context.Context, reqs []store.Request) []store.Response {
	out := make([]store.Response, len(reqs))
	for i := range out {
		out[i] = store.Response{Outcome: store.Unavailable}
	}
	return out
}

func (f *fakeStore) Do(context.Context, store.Kind, ...string) (resp.Value, store.Response) {
	return resp.Value{}, store.Response{Outcome: store.Unavailable}
}

func (f *fakeStore) ReadyConns(store.Kind) int { return 0 }

// Close has nothing to stop: wrap already closed the real adapter.
func (f *fakeStore) Close() {}

// requests returns the recorded requests of one script, in order.
func (f *fakeStore) requests(script string) []store.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Request
	for _, r := range f.reqs {
		if r.Script.Name == script {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeStore) count(script string) int { return len(f.requests(script)) }

// testStore is one test's fake RESP server, every limiter's store address; only the limiters
// built without a fake (the wire tests) connect to it.
type testStore struct {
	srv *fakeresp.Server
	pw  string
}

func newTestStore(t *testing.T) *testStore {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	pw := hex.EncodeToString(b)
	return &testStore{srv: fakeresp.Start(t, "rl_lib", pw), pw: pw}
}

func (s *testStore) options() StoreOptions {
	return StoreOptions{Addr: s.srv.Addr(), User: "rl_lib", Password: s.pw}
}

// limiter builds a limiter through NewWithHooks with fake as its store (when not nil) and
// the hooks h; t's cleanup closes it.
func (s *testStore) limiter(t *testing.T, p Policy, in Instance, fake *fakeStore, h Hooks) (*Limiter, error) {
	t.Helper()
	if fake != nil {
		h.StoreWrapper = fake.wrap
	}
	l, err := NewWithHooks(p, in, s.options(), h)
	if l == nil {
		t.Fatal("NewWithHooks returned a nil limiter")
	}
	t.Cleanup(l.Close)
	return l, err
}

// ready builds a valid limiter on fake and waits until its health reason is reason.
func (s *testStore) ready(t *testing.T, p Policy, in Instance, fake *fakeStore, h Hooks, reason string) *Limiter {
	t.Helper()
	l, err := s.limiter(t, p, in, fake, h)
	if err != nil {
		t.Fatalf("NewWithHooks: %v", err)
	}
	waitReason(t, l, reason)
	return l
}

// waitReason waits, at most waitMax, until l's health reason is reason.
func waitReason(t *testing.T, l *Limiter, reason string) {
	t.Helper()
	waitFor(t, "health reason "+reason, func() bool { return l.Health().Reason == reason })
}

// waitFor polls cond every millisecond for at most waitMax.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitMax)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within %v", what, waitMax)
		}
		time.Sleep(time.Millisecond)
	}
}

// joined is the read_epoch response under which a limiter with policy p joins epochA.
func joined(p Policy) store.Response { return epochOK(epochA, fingerprint.Of(p), tNow, 1) }

// fill reserves every free slot of l and returns how many it took.
func fill(l *Limiter) int {
	n := 0
	for l.slots.tryAcquire() {
		n++
	}
	return n
}

// slotCounts is the number of slots of each kind in use.
type slotCounts struct{ receipts, refunds int }

func slotsOf(l *Limiter) slotCounts {
	r, f := l.slots.inUse()
	return slotCounts{r, f}
}

// risenLabel returns the one label of rl_acquire_results_total that rose by exactly 1
// between two snapshots, every other label unchanged; it fails the test otherwise.
func risenLabel(t *testing.T, before, after metrics.MetricsSnapshot) string {
	t.Helper()
	counts := func(s metrics.MetricsSnapshot) map[string]float64 {
		out := map[string]float64{}
		for _, f := range s.Families {
			if f.Name == "rl_acquire_results_total" {
				for _, sr := range f.Series {
					out[sr.LabelValue] = sr.Value
				}
			}
		}
		return out
	}
	b, a := counts(before), counts(after)
	risen := ""
	for label, v := range a {
		switch d := v - b[label]; d {
		case 0:
		case 1:
			if risen != "" {
				t.Fatalf("rl_acquire_results_total rose for %q and %q", risen, label)
			}
			risen = label
		default:
			t.Fatalf("rl_acquire_results_total{result=%q} changed by %v", label, d)
		}
	}
	if risen == "" {
		t.Fatal("rl_acquire_results_total did not change")
	}
	return risen
}

// unusableSigner is a signer whose Usable reports false (FLT-10 unusable).
type unusableSigner struct{ receipt.Signer }

func (unusableSigner) Usable() bool { return false }

// failingSigner is a signer whose Sign fails (FLT-10 fail).
type failingSigner struct{ receipt.Signer }

func (failingSigner) Sign(receipt.Receipt) (receipt.Receipt, error) {
	return receipt.Receipt{}, errors.New("test: signing fails")
}

// failingReader is a key reader that fails (T-CFG-3 (d)).
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("test: no key bytes") }
