//go:build rltest

package core

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// scenario sets up one acquire of the local-check tests (DESIGN §A5.3).
type scenario struct {
	key       string                      // the budget key
	cancelled bool                        // ctx is already done
	epoch     func(Policy) store.Response // the read_epoch response; nil joins epochA
	health    string                      // the health reason to wait for before acquiring
	invalid   bool                        // settings that fail RCP-8 (margin 59 ms < 60 ms)
	closing   bool                        // Close before acquiring
	unusable  bool                        // the signer reports Usable() false
	full      bool                        // every slot is reserved
}

func baseScenario() scenario { return scenario{key: "k", health: reasonReady} }

// checkResult is what one scenario's acquire did.
type checkResult struct {
	r             Result
	calls         int // acquire requests the adapter received
	before, after slotCounts
	health        Health // right after the acquire
}

// run builds a fresh Closed limiter for sc on a fake store whose acquire allows, applies
// sc, and acquires once.
func (s *testStore) run(t *testing.T, sc scenario) checkResult {
	t.Helper()
	p := testPolicy(Closed)
	in := testInstance(p)
	if sc.invalid {
		in.MarginNs = 59_000_000
	}
	ep := joined(p)
	if sc.epoch != nil {
		ep = sc.epoch(p)
	}
	fake := newFake(ep, func(store.Request) store.Response { return allowedResp(1, 1) })
	var h Hooks
	if sc.unusable {
		h.SignerWrapper = func(sg receipt.Signer) receipt.Signer { return unusableSigner{sg} }
	}
	l, err := s.limiter(t, p, in, fake, h)
	switch {
	case sc.invalid && err == nil:
		t.Fatal("settings that fail RCP-8 were accepted")
	case !sc.invalid && err != nil:
		t.Fatalf("NewWithHooks: %v", err)
	case !sc.invalid:
		waitReason(t, l, sc.health)
	}
	if sc.closing {
		l.Close()
	}
	if sc.full {
		if n := fill(l); n != 4 {
			t.Fatalf("reserved %d slots, want all 4", n)
		}
	}
	ctx := t.Context()
	if sc.cancelled {
		c, cancel := context.WithCancel(ctx)
		cancel()
		ctx = c
	}
	before := slotsOf(l)
	r := l.Acquire(ctx, sc.key)
	return checkResult{r: r, calls: fake.count("acquire"), before: before, after: slotsOf(l), health: l.Health()}
}

// breaker makes one local check of §A5.3 fail.
type breaker struct {
	name  string
	step  int // §A5.3 order: 1 key, 2 ctx, 3 epoch-watch state, 4 signer, 5 slots
	apply func(*scenario)
	kind  ResultKind
	diag  Diag
}

// breakers lists one breaker per local check, in §A5.3 order, each watch state on its own.
func breakers() []breaker {
	watch := func(name string, ep func(Policy) store.Response, kind ResultKind, diag Diag) breaker {
		return breaker{name: name, step: 3, kind: kind, diag: diag, apply: func(s *scenario) {
			s.epoch, s.health = ep, name
		}}
	}
	fixed := func(r store.Response) func(Policy) store.Response { return func(Policy) store.Response { return r } }
	return []breaker{
		{name: "invalid_key", step: 1, kind: ValidationError, apply: func(s *scenario) { s.key = "" }},
		{name: "ctx_done", step: 2, kind: Cancelled, apply: func(s *scenario) { s.cancelled = true }},
		watch("no_epoch", fixed(epochErr("epoch_record_missing")), NotReady, Diag{Reason: "no_epoch"}),
		{name: "settings_invalid", step: 3, kind: NotReady, diag: Diag{Reason: "settings_invalid"}, apply: func(s *scenario) { s.invalid = true }},
		{name: "closing", step: 3, kind: NotReady, diag: Diag{Reason: "closing"}, apply: func(s *scenario) { s.closing = true }},
		watch("fingerprint_mismatch", fixed(epochOK(epochA, otherFP, tNow, 1)), ConfigError, Diag{Reason: "fingerprint_mismatch"}),
		watch("state_lost", fixed(epochErr("hwm_missing")), StateIntegrityError, Diag{Scope: "epoch", Reason: "hwm_missing"}),
		watch("integrity_hold", fixed(epochErr("integrity_hold")), StateIntegrityError, Diag{Scope: "epoch", Reason: "integrity_hold"}),
		{name: "signer_unusable", step: 4, kind: ConfigError, diag: Diag{Reason: "signer_unusable"}, apply: func(s *scenario) { s.unusable = true }},
		{name: "slots_full", step: 5, kind: Busy, apply: func(s *scenario) { s.full = true }},
	}
}

// refusedLocally fails the test unless c is the refusal kind/diag with no permit, no
// acquire sent and the slots unchanged.
func refusedLocally(t *testing.T, what string, c checkResult, kind ResultKind, diag Diag) {
	t.Helper()
	if c.r.Kind != kind || c.r.Diag != diag || c.r.Permit != nil || c.r.DecisionNs != 0 || c.r.RetryAfterNs != 0 {
		t.Errorf("%s: Acquire = %v %+v (permit %v); want %v %+v, no permit", what, c.r.Kind, c.r.Diag, c.r.Permit != nil, kind, diag)
	}
	if c.calls != 0 || c.before != c.after {
		t.Errorf("%s: %d acquire calls, slots %+v → %+v; want 0 calls, slots unchanged", what, c.calls, c.before, c.after)
	}
}

// TestAcquireLocalCheckOrder: each local check of §A5.3 alone gives its result before
// anything is sent: 0 adapter calls and the slots unchanged (API-1a, API-7, API-7b, API-7c,
// API-8, API-8a, API-9). With no check failing the same setup sends one acquire.
func TestAcquireLocalCheckOrder(t *testing.T) {
	s := newTestStore(t)
	c := s.run(t, baseScenario())
	if c.r.Kind != Allowed || c.r.Permit == nil || c.calls != 1 || c.after != (slotCounts{1, 1}) {
		t.Fatalf("no failing check: %v, %d calls, slots %+v; want allowed, 1 call, 1 and 1 slots", c.r.Kind, c.calls, c.after)
	}
	for _, b := range breakers() {
		t.Run(b.name, func(t *testing.T) {
			sc := baseScenario()
			b.apply(&sc)
			refusedLocally(t, b.name, s.run(t, sc), b.kind, b.diag)
		})
	}
	for _, key := range []string{"", strings.Repeat("a", 257), "\xff\xfe"} { // API-1
		sc := baseScenario()
		sc.key = key
		refusedLocally(t, "key "+strconv.Quote(key), s.run(t, sc), ValidationError, Diag{})
	}
}

// cannotCoexist names the ordered breaker pairs of one check step, which one acquire cannot
// meet together, and why. DESIGN §A5.3 ("Local checks, in order") lists no_epoch,
// settings_invalid, closing, fingerprint_mismatch, state_lost and integrity_hold as values of
// one epoch-watch state, so an instance is in exactly one of them; the reachable sequences
// (Close on an instance in each other state) are asserted in TestAcquireLocalCheckOrderPairs.
func cannotCoexist() map[[2]string]string {
	const (
		invalid      = "invalid settings never start the epoch watch: settings_invalid is the instance's only state"
		invalidClose = "one epoch-watch state (DESIGN §A5.3): settings_invalid is permanent (plan S5 step 2), so Close leaves it settings_invalid"
		closing      = "one epoch-watch state (DESIGN §A5.3): Close replaces whatever the watch read with closing and stops the watch (plan S5 step 8)"
		single       = "one epoch-watch check: each read_epoch reply maps to exactly one state (§A5.7)"
	)
	return map[[2]string]string{
		{"no_epoch", "settings_invalid"}:             invalid,
		{"no_epoch", "closing"}:                      closing,
		{"no_epoch", "fingerprint_mismatch"}:         single,
		{"no_epoch", "state_lost"}:                   single,
		{"no_epoch", "integrity_hold"}:               single,
		{"settings_invalid", "closing"}:              invalidClose,
		{"settings_invalid", "fingerprint_mismatch"}: invalid,
		{"settings_invalid", "state_lost"}:           invalid,
		{"settings_invalid", "integrity_hold"}:       invalid,
		{"closing", "fingerprint_mismatch"}:          closing,
		{"closing", "state_lost"}:                    closing,
		{"closing", "integrity_hold"}:                closing,
		{"fingerprint_mismatch", "state_lost"}:       single,
		{"fingerprint_mismatch", "integrity_hold"}:   single,
		{"state_lost", "integrity_hold"}:             single,
	}
}

// TestAcquireLocalCheckOrderPairs: every breaker is first verified alone; then every ordered
// pair of breakers from two different §A5.3 steps, applied together, returns the earlier
// check's kind and Diag reason with 0 adapter calls and the slots unchanged (extends
// T-ORD-1's 6 pairs). Pairs of one step cannot coexist and are listed with the reason. What
// can happen instead is Close on an instance in each other epoch-watch state: Acquire then
// returns not_ready (closing) and health reports closing, except on a settings_invalid
// instance, which stays settings_invalid.
func TestAcquireLocalCheckOrderPairs(t *testing.T) {
	s := newTestStore(t)
	bs := breakers()
	for _, b := range bs {
		sc := baseScenario()
		b.apply(&sc)
		refusedLocally(t, b.name+" alone", s.run(t, sc), b.kind, b.diag)
	}
	if t.Failed() {
		t.FailNow()
	}
	cannot := cannotCoexist()
	pairs, listed := 0, 0
	for i, a := range bs {
		for _, b := range bs[i+1:] {
			name := a.name + "+" + b.name
			if a.step == b.step {
				if cannot[[2]string{a.name, b.name}] == "" {
					t.Errorf("%s: two checks of step %d, not in the cannot-coexist table", name, a.step)
				}
				listed++
				continue
			}
			pairs++
			t.Run(name, func(t *testing.T) {
				sc := baseScenario()
				a.apply(&sc)
				b.apply(&sc)
				refusedLocally(t, name, s.run(t, sc), a.kind, a.diag)
			})
		}
	}
	if pairs != 30 || listed != 15 || len(cannot) != 15 {
		t.Errorf("%d pairs applied, %d listed, %d table rows; want 30, 15, 15", pairs, listed, len(cannot))
	}

	// Close on an instance in each other epoch-watch state (DESIGN §A5.3, plan S5 steps 2
	// and 8): reached after the state, so the scenario waits for that state's health reason
	// and then closes.
	closed := 0
	for _, b := range bs {
		if b.step != 3 || b.name == "closing" {
			continue
		}
		closed++
		t.Run("Close on "+b.name, func(t *testing.T) {
			sc := baseScenario()
			b.apply(&sc)
			sc.closing = true
			reason := "closing"
			if b.name == "settings_invalid" {
				reason = "settings_invalid"
			}
			c := s.run(t, sc)
			refusedLocally(t, "Close on "+b.name, c, NotReady, Diag{Reason: reason})
			if c.health.Status != "not_ready" || c.health.Reason != reason {
				t.Errorf("Close on %s: health %s/%s, want not_ready/%s", b.name, c.health.Status, c.health.Reason, reason)
			}
		})
	}
	if closed != 5 {
		t.Errorf("Close applied on %d epoch-watch states, want 5", closed)
	}
}

// deductionID is §A5.9's deduction_id, computed here by hand: "d1_" +
// base64url(SHA-256(epoch_id ‖ enc ‖ g (8) ‖ s (8)))[:22].
func deductionID(t *testing.T, epochHex, enc string, g, s uint64) string {
	t.Helper()
	id, err := hex.DecodeString(epochHex)
	if err != nil {
		t.Fatal(err)
	}
	b := append(id, enc...)
	b = binary.BigEndian.AppendUint64(b, g)
	b = binary.BigEndian.AppendUint64(b, s)
	sum := sha256.Sum256(b)
	return "d1_" + base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

// ceilDiv is ⌈a / b⌉ in math/big.
func ceilDiv(a, b int64) int64 {
	q, r := new(big.Int).QuoRem(big.NewInt(a), big.NewInt(b), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// TestAcquireCodeMapping: the acquire request carries §A4.1's KEYS and ARGV; every §A4.9
// acquire code and every adapter outcome maps to its result (§A5.3), an allowed permit's
// receipt holding the §A5.9 fields and a MAC its signer verifies; a signing error after code
// 1 is receipt_error with the slots freed (API-12a); an error reply sets the health store
// error; and a limited result's RetryAfterNs equals ⌈(rp − u) / rt⌉ from math/big for 1,000
// seeded (u, rp, rt).
func TestAcquireCodeMapping(t *testing.T) {
	s := newTestStore(t)
	p := testPolicy(Closed)
	fake := newFake(joined(p), func(store.Request) store.Response { return allowedResp(7, 3) })
	l := s.ready(t, p, testInstance(p), fake, Hooks{}, reasonReady)

	r := l.Acquire(t.Context(), "k")
	reqs := fake.requests("acquire")
	if len(reqs) != 1 {
		t.Fatalf("%d acquire requests, want 1", len(reqs))
	}
	req := reqs[0]
	wantKeys := []string{"rl:epoch", "rl:ctl:time", "rl:e:" + epochA + ":ix", "rl:e:" + epochA + ":q", "rl:e:" + epochA + ":k:aw", "rl:e:" + epochA + ":mi:aw"}
	wantArgs := []string{epochA, fingerprint.Of(p), "aw", "20000000000", "1000000000", "10", "60000000", "100000", "1760000000000000", "1"}
	if req.Kind != store.Call || req.Script.SHA != scripts.Acquire.SHA || !slices.Equal(req.Keys, wantKeys) || !slices.Equal(req.Args, wantArgs) {
		t.Fatalf("acquire request: kind %v, script %s, KEYS %q, ARGV %q\nwant call, acquire, KEYS %q, ARGV %q", req.Kind, req.Script.Name, req.Keys, req.Args, wantKeys, wantArgs)
	}
	if r.Kind != Allowed || r.DecisionNs != 1_760_000_000_000_000_000 || r.RetryAfterNs != 0 || r.Permit == nil || r.Diag != (Diag{}) {
		t.Fatalf("code 1: %v, decision %d, retry %d, permit %v, %+v; want allowed at 1760000000000000000 ns with a permit", r.Kind, r.DecisionNs, r.RetryAfterNs, r.Permit != nil, r.Diag)
	}
	if got, want := r.Permit.DeductionID(), deductionID(t, epochA, "aw", 7, 3); got != want || PermitEpoch(r.Permit) != epochA {
		t.Fatalf("permit: deduction_id %q, epoch %q; want %q, %q", got, PermitEpoch(r.Permit), want, epochA)
	}
	// The receipt (§A5.9, RCP-1, RCP-3): the held epoch, enc "aw", the reply's g 7 and s 3,
	// cost 1, issued at the reply's t and expiring one receipt lifetime later
	// (1,760,000,000,000,000 + 60,000,000 µs), and signed: this instance's signer verifies it.
	rc := r.Permit.receipt
	wantEpoch, err := hex.DecodeString(epochA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rc.EpochID[:], wantEpoch) || rc.Enc != "aw" || rc.G != 7 || rc.S != 3 || rc.Cost != 1 ||
		rc.IssueUs != 1_760_000_000_000_000 || rc.ExpiryUs != 1_760_000_060_000_000 {
		t.Fatalf("receipt: epoch %x, enc %q, g %d, s %d, cost %d, issue %d, expiry %d; want %s, aw, 7, 3, 1, 1760000000000000, 1760000060000000",
			rc.EpochID, rc.Enc, rc.G, rc.S, rc.Cost, rc.IssueUs, rc.ExpiryUs, epochA)
	}
	if !l.signer.Verify(rc) {
		t.Fatalf("the instance's signer does not verify the permit's receipt (MAC %x)", rc.MAC)
	}
	if got := slotsOf(l); got != (slotCounts{1, 1}) {
		t.Fatalf("an allowed permit holds %+v slots, want 1 and 1", got)
	}
	r.Permit.Done()
	if got := slotsOf(l); got != (slotCounts{0, 0}) {
		t.Fatalf("after Done: %+v slots in use, want 0 and 0", got)
	}

	cases := []struct {
		name     string
		resp     store.Response
		kind     ResultKind
		diag     Diag
		decision int64
		retry    int64
	}{
		// ⌈(10^9 − 123,456,789) / 10⌉ = ⌈87,654,321.1⌉ = 87,654,322
		{"code 2", okResp(2, tNow, "", int64(123_456_789)), Limited, Diag{}, 1_760_000_000_000_000_000, 87_654_322},
		{"code 20", okResp(20, 0, "epoch_or_fingerprint"), ConfigError, Diag{Reason: "epoch_or_fingerprint"}, 0, 0},
		{"code 21", okResp(21, 0, "clock"), ClockError, Diag{Reason: "clock"}, 0, 0},
		{"code 22", okResp(22, 0, "integrity_hold"), StateIntegrityError, Diag{Scope: "epoch", Reason: "integrity_hold"}, 0, 0},
		{"code 23", okResp(23, tNow, "malformed_bucket"), StateIntegrityError, Diag{Scope: "key", Reason: "malformed_bucket"}, 0, 0},
		{"code 24", okResp(24, tNow, "tracked_buckets"), CapacityError, Diag{Reason: "tracked_buckets"}, 0, 0},
		{"code 25", okResp(25, 0, "keys_mismatch"), OutcomeUnknown, Diag{Reason: "keys_mismatch"}, 0, 0},
		{"unavailable", store.Response{Outcome: store.Unavailable}, Unavailable, Diag{}, 0, 0},
		{"cancelled", store.Response{Outcome: store.Cancelled}, Cancelled, Diag{}, 0, 0},
		{"outcome_unknown", store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR test"}, OutcomeUnknown, Diag{StoreError: "ERR test"}, 0, 0},
		{"capacity_error", store.Response{Outcome: store.CapacityError, Sent: true, SentCommands: 1, StoreError: "OOM command not allowed"}, CapacityError, Diag{StoreError: "OOM command not allowed"}, 0, 0},
		// Replies the decoder never passes (§A4.9) are refused all the same, both sides of
		// each bound: outcome_unknown, never a result built from them.
		{"u = 0", okResp(2, tNow, "", int64(0)), Limited, Diag{}, 1_760_000_000_000_000_000, 100_000_000},
		{"u = -1", okResp(2, tNow, "", int64(-1)), OutcomeUnknown, Diag{}, 0, 0},
		{"u = rp - 1", okResp(2, tNow, "", int64(999_999_999)), Limited, Diag{}, 1_760_000_000_000_000_000, 1},
		{"u = rp", okResp(2, tNow, "", int64(1_000_000_000)), OutcomeUnknown, Diag{}, 0, 0},
		{"t = 0", okResp(2, 0, "", int64(0)), Limited, Diag{}, 0, 100_000_000},
		{"t = -1", okResp(2, -1, "", int64(0)), OutcomeUnknown, Diag{}, 0, 0},
		// TBOUND − 1 = 2^53 − 2^43 − 1 = 8,998,403,161,718,783 µs
		{"t = TBOUND - 1", okResp(2, 8_998_403_161_718_783, "", int64(0)), Limited, Diag{}, 8_998_403_161_718_783_000, 100_000_000},
		{"t = TBOUND", okResp(2, 8_998_403_161_718_784, "", int64(0)), OutcomeUnknown, Diag{}, 0, 0},
		{"g = 0", okResp(1, tNow, "", int64(0), int64(0), int64(1), int64(tNow+lifeUs)), OutcomeUnknown, Diag{}, 0, 0},
		{"s = 0", okResp(1, tNow, "", int64(0), int64(1), int64(0), int64(tNow+lifeUs)), OutcomeUnknown, Diag{}, 0, 0},
		{"code 1 too short", okResp(1, tNow, "", int64(0)), OutcomeUnknown, Diag{}, 0, 0},
		{"code 2 too short", okResp(2, tNow, ""), OutcomeUnknown, Diag{}, 0, 0},
		{"code 3", okResp(3, tNow, "", int64(0)), OutcomeUnknown, Diag{}, 0, 0},
	}
	for _, c := range cases {
		fake.setAcquire(func(store.Request) store.Response { return c.resp })
		calls := fake.count("acquire")
		r := l.Acquire(t.Context(), "k")
		if r.Kind != c.kind || r.Diag != c.diag || r.DecisionNs != c.decision || r.RetryAfterNs != c.retry || r.Permit != nil {
			t.Errorf("%s: %v %+v, decision %d, retry %d, permit %v; want %v %+v, decision %d, retry %d, no permit",
				c.name, r.Kind, r.Diag, r.DecisionNs, r.RetryAfterNs, r.Permit != nil, c.kind, c.diag, c.decision, c.retry)
		}
		if n := fake.count("acquire") - calls; n != 1 {
			t.Errorf("%s: %d acquire calls, want 1", c.name, n)
		}
		if got := slotsOf(l); got != (slotCounts{0, 0}) {
			t.Errorf("%s: %+v slots in use after return, want 0 and 0", c.name, got)
		}
	}

	// Signing fails after code 1: receipt_error, no permit, slots freed (API-12a).
	fake2 := newFake(joined(p), func(store.Request) store.Response { return allowedResp(1, 1) })
	l2 := s.ready(t, p, testInstance(p), fake2, Hooks{SignerWrapper: func(sg receipt.Signer) receipt.Signer { return failingSigner{sg} }}, reasonReady)
	if r := l2.Acquire(t.Context(), "k"); r.Kind != ReceiptError || r.Permit != nil || r.Diag != (Diag{Reason: "signing_failed"}) || slotsOf(l2) != (slotCounts{0, 0}) || fake2.count("acquire") != 1 {
		t.Errorf("signing fails: %v %+v, permit %v, slots %+v, %d calls; want receipt_error (signing_failed), no permit, 0 slots, 1 call",
			r.Kind, r.Diag, r.Permit != nil, slotsOf(l2), fake2.count("acquire"))
	}

	// An acquire's error reply is also the health store error (plan choice): outcome_unknown
	// with "ERR x" sets Health().StoreError to "ERR x" (a fresh limiter, whose watch does not
	// read again within the test, so nothing clears it).
	fake3 := newFake(joined(p), func(store.Request) store.Response {
		return store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR x"}
	})
	l3 := s.ready(t, p, testInstance(p), fake3, Hooks{}, reasonReady)
	if se := l3.Health().StoreError; se != "" {
		t.Fatalf("store_error %q after a decoded read, want empty", se)
	}
	if r := l3.Acquire(t.Context(), "k"); r.Kind != OutcomeUnknown || r.Diag.StoreError != "ERR x" {
		t.Fatalf("error reply: %v %+v, want outcome_unknown with store error ERR x", r.Kind, r.Diag)
	}
	if se := l3.Health().StoreError; se != "ERR x" {
		t.Errorf("store_error %q after an acquire's error reply, want ERR x", se)
	}

	// RetryAfterNs for 1,000 seeded (u, rp, rt): 20 policies × 50 units values, against math/big.
	rng := rand.New(rand.NewPCG(20261006, 5))
	const policies, perPolicy = 20, 50
	checked := 0
	for i := range policies {
		rp := 1000 * (1 + rng.Int64N(1<<40)) // a multiple of 1,000 below 2^50 + 1,000
		rt := 1 + rng.Int64N(math.MaxInt32)  // 1 … 2^31 − 1
		switch i {
		case 0:
			rp, rt = 1000, 1
		case 1:
			rt = math.MaxInt32
		case 2:
			rp, rt = 1_000_000_000, 10 // the default policy
		}
		pi := testPolicy(Closed)
		pi.RatePeriodNs, pi.RateTokens, pi.Capacity = rp, rt, 3 // cap_units = 3 × rp < 2^52
		fi := newFake(joined(pi), nil)
		li := s.ready(t, pi, testInstance(pi), fi, Hooks{}, reasonReady)
		for j := range perPolicy {
			u := rng.Int64N(rp)
			switch j {
			case 0:
				u = 0
			case 1:
				u = rp - 1
			}
			fi.setAcquire(func(store.Request) store.Response { return okResp(2, tNow, "", u) })
			r := li.Acquire(t.Context(), "k")
			if want := ceilDiv(rp-u, rt); r.Kind != Limited || r.RetryAfterNs != want || r.DecisionNs != 1_760_000_000_000_000_000 {
				t.Fatalf("u %d, rp %d, rt %d: %v, retry %d, decision %d; want limited, retry %d, decision 1760000000000000000",
					u, rp, rt, r.Kind, r.RetryAfterNs, r.DecisionNs, want)
			}
			checked++
		}
	}
	if checked != 1000 {
		t.Fatalf("checked %d triples, want 1000", checked)
	}
}

// TestAcquireDegradedOpen: an unavailable acquire in an Open limiter is degraded with a
// DEGRADED permit (no receipt, DeductionID "", slots 0, Done a no-op, API-5); in a Closed
// one it is unavailable with no permit (API-20a, API-20b); in Open, outcome_unknown and
// not_ready stay as they are (API-21).
func TestAcquireDegradedOpen(t *testing.T) {
	s := newTestStore(t)
	unavailable := func(store.Request) store.Response { return store.Response{Outcome: store.Unavailable} }

	po := testPolicy(Open)
	fo := newFake(joined(po), unavailable)
	lo := s.ready(t, po, testInstance(po), fo, Hooks{}, reasonReady)
	before := lo.Metrics()
	r := lo.Acquire(t.Context(), "k")
	if label := risenLabel(t, before, lo.Metrics()); label != "degraded" {
		t.Errorf("Open, unavailable: counted as %q, want degraded", label)
	}
	if r.Kind != Degraded || r.Permit == nil || r.Permit.DeductionID() != "" || PermitEpoch(r.Permit) != "" || r.DecisionNs != 0 || r.RetryAfterNs != 0 {
		t.Fatalf("Open, unavailable: %v, permit %v; want degraded with a permit, no deduction_id", r.Kind, r.Permit != nil)
	}
	if got := slotsOf(lo); got != (slotCounts{0, 0}) || fo.count("acquire") != 1 {
		t.Fatalf("Open, unavailable: %+v slots, %d calls; want 0 and 0 slots, 1 call", got, fo.count("acquire"))
	}
	if st := r.Permit.life.load(); st != stateDegraded || r.Permit.slots != nil {
		t.Fatalf("Open, unavailable: permit state %v, slots held %v; want DEGRADED, no slots", st, r.Permit.slots != nil)
	}
	r.Permit.Done()
	if got := slotsOf(lo); got != (slotCounts{0, 0}) || r.Permit.life.load() != stateDegraded {
		t.Fatalf("Done on a degraded permit: %+v slots, state %v; want 0 and 0, DEGRADED", got, r.Permit.life.load())
	}

	pc := testPolicy(Closed)
	fc := newFake(joined(pc), unavailable)
	lc := s.ready(t, pc, testInstance(pc), fc, Hooks{}, reasonReady)
	if r := lc.Acquire(t.Context(), "k"); r.Kind != Unavailable || r.Permit != nil || slotsOf(lc) != (slotCounts{0, 0}) {
		t.Fatalf("Closed, unavailable: %v, permit %v, slots %+v; want unavailable, no permit, 0 slots", r.Kind, r.Permit != nil, slotsOf(lc))
	}

	fo.setAcquire(func(store.Request) store.Response {
		return store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1}
	})
	if r := lo.Acquire(t.Context(), "k"); r.Kind != OutcomeUnknown || r.Permit != nil || slotsOf(lo) != (slotCounts{0, 0}) {
		t.Fatalf("Open, outcome_unknown: %v, permit %v, slots %+v; want outcome_unknown, no permit, 0 slots", r.Kind, r.Permit != nil, slotsOf(lo))
	}
	fn := newFake(epochErr("epoch_record_missing"), unavailable)
	ln := s.ready(t, po, testInstance(po), fn, Hooks{}, reasonNoEpoch)
	if r := ln.Acquire(t.Context(), "k"); r.Kind != NotReady || r.Permit != nil || fn.count("acquire") != 0 {
		t.Fatalf("Open, no epoch: %v, permit %v, %d calls; want not_ready, no permit, 0 calls", r.Kind, r.Permit != nil, fn.count("acquire"))
	}
}

// norm is a result normalized for TestFailureModeTable.
type norm struct {
	kind          ResultKind
	scope, reason string
	retry         int64
	decision      int64
	permit        bool
	slots         slotCounts // in use after Acquire returned
	label         string     // the rl_acquire_results_total label that rose
}

// fmRow is one row of the failure-mode table: an adapter outcome, an E_ALLOWED reply
// through the real adapter (wire), or a local refusal.
type fmRow struct {
	name  string
	resp  store.Response // the fake adapter's acquire response
	stop  func(*Limiter) // when set, the fake runs it (Close or Kill) on the limiter before it answers resp
	wire  bool           // the real adapter against fakeresp, which answers E_ALLOWED with u
	u     int64          // the wire row's u
	local string         // validation, busy, cancelled or not_ready
	want  norm           // in a Closed limiter
	raw   string         // the Diag.StoreError the row must carry
}

// outOfRange is the out-of-range wire row's acquire reply, E_ALLOWED with u = cap_units −
// rate_period_ns + 1 under the default policy, as resp.Value.String prints it.
const outOfRange = `[1 1760000000000000 "" 19000000001 1 1 1760000060000000]`

// wireInterval is the wire limiters' re-read interval: short, so a startup read that finds
// no ready connection yet (a slow dial) is retried well within waitMax.
const wireInterval = 50_000_000 // 50 ms

// wireLimiter builds a limiter of mode whose real adapter talks to a fakeresp server that
// answers read_epoch with this policy's fingerprint and acquire with E_ALLOWED {1, t, ”, u,
// 1, 1, t + lifetime_us}; calls counts the acquires the server ran. Its re-read interval is
// wireInterval; D stays the default 2 s, within §A5.6's inequality.
func wireLimiter(t *testing.T, mode FailureMode, u int64) (*Limiter, *atomic.Int32) {
	t.Helper()
	return wireLimiterWith(t, mode, u, Hooks{})
}

// wireLimiterWith is wireLimiter built with the hooks h (a store wrapper around the real
// adapter, for example).
func wireLimiterWith(t *testing.T, mode FailureMode, u int64, h Hooks) (*Limiter, *atomic.Int32) {
	t.Helper()
	ts := newTestStore(t)
	p := testPolicy(mode)
	fp := fingerprint.Of(p)
	arr := func(vs ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: vs} }
	ts.srv.Load(scripts.ReadEpoch.Text)
	ts.srv.Handle(scripts.ReadEpoch.SHA, func(_, _ []string) resp.Value {
		return arr(intVal(0), intVal(0), strVal(""), strVal(epochA), strVal(fp), intVal(tNow), intVal(1),
			intVal(tNow), intVal(0), intVal(0), intVal(0))
	})
	calls := &atomic.Int32{}
	ts.srv.Load(scripts.Acquire.Text)
	ts.srv.Handle(scripts.Acquire.SHA, func(_, _ []string) resp.Value {
		calls.Add(1)
		return arr(intVal(1), intVal(tNow), strVal(""), intVal(u), intVal(1), intVal(1), intVal(tNow+lifeUs))
	})
	in := testInstance(p)
	in.EpochRereadIntervalNs = wireInterval
	return ts.ready(t, p, in, nil, h, reasonReady), calls
}

// runRow runs one row in a fresh limiter of mode; it returns the normalized result, the
// acquire calls the adapter received and the result itself.
func (s *testStore) runRow(t *testing.T, row fmRow, mode FailureMode) (norm, int, Result) {
	t.Helper()
	var l *Limiter
	var calls func() int
	key, ctx := "k", t.Context()
	if row.wire {
		wl, n := wireLimiter(t, mode, row.u)
		l, calls = wl, func() int { return int(n.Load()) }
	} else {
		p := testPolicy(mode)
		ep, reason := joined(p), reasonReady
		if row.local == "not_ready" {
			ep, reason = epochErr("epoch_record_missing"), reasonNoEpoch
		}
		fake := newFake(ep, func(store.Request) store.Response {
			if row.stop != nil {
				row.stop(l) // a shutdown that begins while the acquire is in the adapter
			}
			return row.resp
		})
		l = s.ready(t, p, testInstance(p), fake, Hooks{}, reason)
		calls = func() int { return fake.count("acquire") }
	}
	switch row.local {
	case "validation":
		key = strings.Repeat("x", 257)
	case "busy":
		if n := fill(l); n != 4 {
			t.Fatalf("%s: reserved %d slots, want 4", row.name, n)
		}
	case "cancelled":
		c, cancel := context.WithCancel(ctx)
		cancel()
		ctx = c
	}
	before := l.Metrics()
	r := l.Acquire(ctx, key)
	n := norm{
		kind: r.Kind, scope: r.Diag.Scope, reason: r.Diag.Reason, retry: r.RetryAfterNs, decision: r.DecisionNs,
		permit: r.Permit != nil, slots: slotsOf(l), label: risenLabel(t, before, l.Metrics()),
	}
	if r.Diag.StoreError != row.raw {
		t.Errorf("%s (%v): Diag.StoreError %q, want %q", row.name, mode, r.Diag.StoreError, row.raw)
	}
	return n, calls(), r
}

// TestFailureModeTable (DESIGN §C4, plan S5; S6 adds the refund rows, refundRows): every adapter outcome (OK with each acquire
// code 1, 2, 20–25 and an out-of-range and an in-range allowed reply; unavailable;
// cancelled; outcome_unknown; capacity_error) and every local refusal, each in a Closed and an Open limiter (the fake
// answers read_epoch with each limiter's own fingerprint). The normalized results are the
// row's literal expectation in both modes, except unavailable (Closed) ↔ degraded (Open:
// permit, DeductionID "", 0 slots, Done a no-op; API-5). An unavailable that comes back after
// Close or Kill began is not_ready (closing) in both modes (API-21). The adapter receives exactly one
// acquire per row that reaches send (no retry, API-12) and none for a local refusal. The
// wire rows go through the real adapter, which decodes with this instance's limits: an
// allowed reply one unit past the u bound → outcome_unknown, no permit, the raw reply in
// Diag; at the bound (expiry_us = t + lifetime_us) → allowed with a permit. Together they pin
// the exact limits core gives the decoder.
func TestFailureModeTable(t *testing.T) {
	s := newTestStore(t)
	const dec = 1_760_000_000_000_000_000
	rows := []fmRow{
		{name: "code 1", resp: allowedResp(1, 1), want: norm{kind: Allowed, decision: dec, permit: true, slots: slotCounts{1, 1}, label: "allowed"}},
		// ⌈(10^9 − 999,999,999) / 10⌉ = ⌈0.1⌉ = 1
		{name: "code 2", resp: okResp(2, tNow, "", int64(999_999_999)), want: norm{kind: Limited, retry: 1, decision: dec, label: "limited"}},
		{name: "code 20", resp: okResp(20, 0, "epoch_or_fingerprint"), want: norm{kind: ConfigError, reason: "epoch_or_fingerprint", label: "config_error"}},
		{name: "code 21", resp: okResp(21, 0, "clock"), want: norm{kind: ClockError, reason: "clock", label: "clock_error"}},
		{name: "code 22", resp: okResp(22, 0, "hwm_malformed"), want: norm{kind: StateIntegrityError, scope: "epoch", reason: "hwm_malformed", label: "state_integrity_error"}},
		{name: "code 23", resp: okResp(23, tNow, "quarantined"), want: norm{kind: StateIntegrityError, scope: "key", reason: "quarantined", label: "state_integrity_error"}},
		{name: "code 24", resp: okResp(24, tNow, "live_markers"), want: norm{kind: CapacityError, reason: "live_markers", label: "capacity_error"}},
		{name: "code 25", resp: okResp(25, 0, "keys_mismatch"), want: norm{kind: OutcomeUnknown, reason: "keys_mismatch", label: "outcome_unknown"}},
		// The decoder's bound on an allowed reply's u is cap_units − rate_period_ns = 2 × 10^10
		// − 10^9 = 19,000,000,000 (§A4.9), and expiry_us must be t + lifetime_us: one above
		// the bound is refused, the bound itself is allowed.
		{name: "out of range", wire: true, u: 19_000_000_001, raw: outOfRange, want: norm{kind: OutcomeUnknown, label: "outcome_unknown"}},
		{name: "in range", wire: true, u: 19_000_000_000, want: norm{kind: Allowed, decision: dec, permit: true, slots: slotCounts{1, 1}, label: "allowed"}},
		{name: "unavailable", resp: store.Response{Outcome: store.Unavailable}, want: norm{kind: Unavailable, label: "unavailable"}},
		// Close or Kill begins while the acquire is in the adapter, which then answers
		// unavailable (closed, nothing sent): the instance is closing, so not_ready in both
		// modes (API-21, API-1e, API-8a).
		{name: "unavailable during Close", resp: store.Response{Outcome: store.Unavailable}, stop: func(l *Limiter) { l.Close() }, want: norm{kind: NotReady, reason: "closing", label: "not_ready"}},
		{name: "unavailable during Kill", resp: store.Response{Outcome: store.Unavailable}, stop: Kill, want: norm{kind: NotReady, reason: "closing", label: "not_ready"}},
		{name: "cancelled", resp: store.Response{Outcome: store.Cancelled}, want: norm{kind: Cancelled, label: "cancelled"}},
		{name: "outcome_unknown", resp: store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR lost"}, raw: "ERR lost", want: norm{kind: OutcomeUnknown, label: "outcome_unknown"}},
		{name: "capacity_error", resp: store.Response{Outcome: store.CapacityError, Sent: true, SentCommands: 1, StoreError: "OOM command not allowed"}, raw: "OOM command not allowed", want: norm{kind: CapacityError, label: "capacity_error"}},
		{name: "local validation", local: "validation", want: norm{kind: ValidationError, label: "validation_error"}},
		{name: "local busy", local: "busy", want: norm{kind: Busy, slots: slotCounts{4, 4}, label: "busy"}},
		{name: "local cancelled", local: "cancelled", want: norm{kind: Cancelled, label: "cancelled"}},
		{name: "local not ready", local: "not_ready", want: norm{kind: NotReady, reason: "no_epoch", label: "not_ready"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			wantCalls := 1
			if row.local != "" {
				wantCalls = 0
			}
			closed, closedCalls, rc := s.runRow(t, row, Closed)
			open, openCalls, ro := s.runRow(t, row, Open)
			if closedCalls != wantCalls || openCalls != wantCalls {
				t.Errorf("acquire calls: Closed %d, Open %d; want %d each (no retry, API-12)", closedCalls, openCalls, wantCalls)
			}
			if closed != row.want {
				t.Errorf("Closed: %+v\nwant    %+v", closed, row.want)
			}
			wantOpen := row.want
			if row.want.kind == Unavailable {
				wantOpen = norm{kind: Degraded, permit: true, label: "degraded"}
			}
			if open != wantOpen {
				t.Errorf("Open: %+v\nwant  %+v", open, wantOpen)
			}
			if row.want.kind != Unavailable && open != closed {
				t.Errorf("the modes differ: Closed %+v, Open %+v", closed, open)
			}
			for _, r := range []Result{rc, ro} {
				switch r.Kind {
				case Allowed:
					r.Permit.Done()
				case Degraded:
					if r.Permit.DeductionID() != "" {
						t.Errorf("degraded permit has deduction_id %q", r.Permit.DeductionID())
					}
					if st := r.Permit.life.load(); st != stateDegraded || r.Permit.slots != nil {
						t.Fatalf("degraded permit: state %v, slots held %v; want DEGRADED, no slots", st, r.Permit.slots != nil)
					}
					r.Permit.Done()
					if r.Permit.life.load() != stateDegraded {
						t.Errorf("Done changed a degraded permit to %v", r.Permit.life.load())
					}
				case Limited, ValidationError, ClockError, Busy, Cancelled, ConfigError, NotReady,
					StateIntegrityError, CapacityError, Unavailable, OutcomeUnknown, ReceiptError:
				}
			}
		})
	}
	t.Run("refund", func(t *testing.T) { s.refundRows(t) })
}

// refundRow is one refund row of TestFailureModeTable: the adapter's response to the first
// refund attempt (the wire row: an out-of-range reply through the real adapter instead), the
// one final event the listener gets, and the refund calls, 1 when the response is final and 2
// when it is retried (the next attempt answers code 3).
type refundRow struct {
	name  string
	resp  store.Response
	wire  bool
	want  RefundEvent
	calls int
	early bool // an epoch-wide reply (20, 22): the epoch watch reads once more at once (§A5.7)
}

// The budget times of a refund row's first reply and of the code 3 answering a retry.
const (
	refundFirstUs = tNow + 1_000
	refundNextUs  = tNow + 2_000
)

// refundRows (plan S6 Tests (a)): each refund adapter outcome (OK with each §A4.9 refund code
// 3, 4, 10, 11, 20–25 and an out-of-range reply; unavailable; cancelled; outcome_unknown;
// capacity_error), in a Closed and an Open limiter, gives the same event (Result, Reason,
// Attempts, CreditDecisionNs, DeductionID) and the same final-or-retry choice, each the row's
// literal expectation, with both slots freed (API-21, API-23: the failure mode never applies to
// refunds). The out-of-range row, E_APPLIED with credited_units = rate_period_ns + 1 through
// the real adapter against fakeresp, is retried as outcome_unknown, never final. An error
// reply's text becomes Health.StoreError, and only the epoch-wide codes 20 and 22 make the
// epoch watch read again at once (§A5.7).
func (s *testStore) refundRows(t *testing.T) {
	retried := func(attempts int) RefundEvent {
		return RefundEvent{Result: Applied, Attempts: attempts, CreditDecisionNs: refundNextUs * 1000}
	}
	rows := []refundRow{
		{name: "code 3", resp: okResp(3, refundFirstUs, "", int64(1_000_000_000)),
			want: RefundEvent{Result: Applied, Attempts: 1, CreditDecisionNs: refundFirstUs * 1000}, calls: 1},
		{name: "code 4", resp: okResp(4, refundFirstUs, "", int64(tNow+500)),
			want: RefundEvent{Result: AlreadyApplied, Attempts: 1, CreditDecisionNs: (tNow + 500) * 1000}, calls: 1},
		{name: "code 10", resp: okResp(10, refundFirstUs, "generation_or_sequence"), want: RefundEvent{Result: ReceiptInvalid, Attempts: 1}, calls: 1},
		{name: "code 11", resp: okResp(11, refundFirstUs, "expired"), want: RefundEvent{Result: ReceiptExpired, Attempts: 1}, calls: 1},
		{name: "code 20", resp: okResp(20, 0, "epoch_or_fingerprint"), want: retried(2), calls: 2, early: true},
		{name: "code 21", resp: okResp(21, 0, "clock"), want: retried(2), calls: 2},
		{name: "code 22", resp: okResp(22, 0, "integrity_hold"), want: retried(2), calls: 2, early: true},
		{name: "code 23", resp: okResp(23, refundFirstUs, "refund_bucket_missing"), want: retried(2), calls: 2},
		{name: "code 24", resp: okResp(24, refundFirstUs, "key_markers"), want: retried(2), calls: 2},
		{name: "code 25", resp: okResp(25, 0, "keys_mismatch"), want: retried(2), calls: 2},
		{name: "out of range", wire: true, want: retried(2), calls: 2},
		{name: "unavailable", resp: store.Response{Outcome: store.Unavailable}, want: retried(1), calls: 2},
		{name: "cancelled", resp: store.Response{Outcome: store.Cancelled}, want: retried(1), calls: 2},
		{name: "outcome_unknown", resp: store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR lost"},
			want: retried(2), calls: 2},
		{name: "capacity_error", resp: store.Response{Outcome: store.CapacityError, Sent: true, SentCommands: 1, StoreError: "OOM command not allowed"},
			want: retried(2), calls: 2},
	}
	// The receipt is the same in both modes (epochA, "aw", g 1, s 1), so is its deduction_id.
	id := deductionID(t, epochA, "aw", 1, 1)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			want := row.want
			want.DeductionID = id
			for _, mode := range []FailureMode{Closed, Open} {
				ev, calls, storeErr := s.runRefundRow(t, row, mode)
				ev.ReleaseMonoNs, ev.FinalMonoNs = 0, 0
				if ev != want || calls != row.calls {
					t.Errorf("%v: event %+v after %d refund call(s)\nwant  %+v after %d", mode, ev, calls, want, row.calls)
				}
				if storeErr != row.resp.StoreError {
					t.Errorf("%v: Health.StoreError %q, want %q", mode, storeErr, row.resp.StoreError)
				}
			}
		})
	}
}

// runRefundRow runs one refund row in a fresh limiter of mode: one allowed acquire, released;
// it returns the one final event, the refund calls the store got and Health.StoreError, and
// checks that both slots were freed and that the epoch watch read again only after an
// epoch-wide reply. Every row's refund is final before its cleanup Close, so the limiter
// drains for only rowDrainNs.
func (s *testStore) runRefundRow(t *testing.T, row refundRow, mode FailureMode) (RefundEvent, int, string) {
	t.Helper()
	const rowDrainNs = 100_000_000 // 100 ms
	p := testPolicy(mode)
	log := &eventLog{}
	var l *Limiter
	var calls func() int
	var fake *fakeStore
	if row.wire {
		ts := newTestStore(t)
		fp := fingerprint.Of(p)
		arr := func(vs ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: vs} }
		ts.srv.Load(scripts.ReadEpoch.Text)
		ts.srv.Handle(scripts.ReadEpoch.SHA, func(_, _ []string) resp.Value {
			return arr(intVal(0), intVal(0), strVal(""), strVal(epochA), strVal(fp), intVal(tNow), intVal(1),
				intVal(tNow), intVal(0), intVal(0), intVal(0))
		})
		ts.srv.Load(scripts.Acquire.Text)
		ts.srv.Handle(scripts.Acquire.SHA, func(_, _ []string) resp.Value {
			return arr(intVal(1), intVal(tNow), strVal(""), intVal(19_000_000_000), intVal(1), intVal(1), intVal(tNow+lifeUs))
		})
		n := &atomic.Int32{}
		ts.srv.Load(scripts.Refund.Text)
		ts.srv.Handle(scripts.Refund.SHA, func(_, _ []string) resp.Value {
			if n.Add(1) == 1 { // credited_units = rate_period_ns + 1: outside §A4.9's 0 … rate_period_ns
				return arr(intVal(3), intVal(refundFirstUs), strVal(""), intVal(1_000_000_001))
			}
			return arr(intVal(3), intVal(refundNextUs), strVal(""), intVal(1_000_000_000))
		})
		in := refundInstance(p)
		in.EpochRereadIntervalNs = wireInterval
		in.DrainTimeoutNs = rowDrainNs
		l = ts.ready(t, p, in, nil, Hooks{}, reasonReady, WithRefundListener(log.listen))
		calls = func() int { return int(n.Load()) }
	} else {
		fake = newFake(joined(p), func(store.Request) store.Response { return allowedResp(1, 1) })
		fake.setRefund(replies(row.resp, appliedAt(refundNextUs)))
		in := refundInstance(p)
		in.DrainTimeoutNs = rowDrainNs
		l = s.ready(t, p, in, fake, Hooks{}, reasonReady, WithRefundListener(log.listen))
		calls = func() int { return fake.count("refund") }
	}
	acquireOK(t, l).Release()
	ev := waitEvents(t, log, 1)[0]
	if got := slotsOf(l); got != (slotCounts{0, 0}) {
		t.Errorf("%s (%v): slots in use %+v after the final event, want 0 and 0", row.name, mode, got)
	}
	if fake != nil { // the startup read, plus one early read after an epoch-wide reply
		if row.early {
			waitFor(t, "the early epoch read", func() bool { return fake.count("read_epoch") == 2 })
		} else if n := fake.count("read_epoch"); n != 1 {
			t.Errorf("%s (%v): %d epoch reads, want 1 (only the startup read)", row.name, mode, n)
		}
	}
	return ev, calls(), l.Health().StoreError
}

// expectHealth fails the test unless l's health has reason (status ready only for "ready")
// and epoch_id id.
func expectHealth(t *testing.T, step string, l *Limiter, reason, id string) {
	t.Helper()
	h := l.Health()
	status := "not_ready"
	if reason == reasonReady {
		status = "ready"
	}
	if h.Status != status || h.Reason != reason || h.EpochID != id {
		t.Fatalf("%s: health %s/%s, epoch %q; want %s/%s, epoch %q", step, h.Status, h.Reason, h.EpochID, status, reason, id)
	}
}

// expectRefused fails the test unless an acquire is refused with kind and diag before
// anything is sent.
func expectRefused(t *testing.T, step string, l *Limiter, fake *fakeStore, kind ResultKind, diag Diag) {
	t.Helper()
	calls := fake.count("acquire")
	if r := l.Acquire(t.Context(), "k"); r.Kind != kind || r.Diag != diag || r.Permit != nil || fake.count("acquire") != calls {
		t.Fatalf("%s: Acquire = %v %+v (%d sent); want %v %+v, nothing sent", step, r.Kind, r.Diag, fake.count("acquire")-calls, kind, diag)
	}
}

// expectFloors acquires once and fails the test unless the request carried epoch id and the
// floors hwm and gen in ARGV (§A4.1: ARGV[1], ARGV[9], ARGV[10]).
func expectFloors(t *testing.T, step string, l *Limiter, fake *fakeStore, id, hwm, gen string) {
	t.Helper()
	l.Acquire(t.Context(), "k")
	reqs := fake.requests("acquire")
	if len(reqs) == 0 {
		t.Fatalf("%s: no acquire was sent", step)
	}
	a, k := reqs[len(reqs)-1].Args, reqs[len(reqs)-1].Keys
	if len(a) != 10 || a[0] != id || a[8] != hwm || a[9] != gen || len(k) != 6 || k[2] != "rl:e:"+id+":ix" {
		t.Fatalf("%s: KEYS %q, ARGV %q; want epoch %s and floors %s, %s", step, k, a, id, hwm, gen)
	}
}

// expectQ fails the test unless the last read_epoch request passed q as KEYS[3].
func expectQ(t *testing.T, step string, fake *fakeStore, q string) {
	t.Helper()
	reqs := fake.requests("read_epoch")
	if len(reqs) == 0 {
		t.Fatalf("%s: no read_epoch request", step)
	}
	if k := reqs[len(reqs)-1].Keys; !slices.Equal(k, []string{"rl:epoch", "rl:ctl:time", q}) {
		t.Fatalf("%s: read_epoch KEYS %q, want rl:epoch, rl:ctl:time, %s", step, k, q)
	}
}

// expectHolds fails the test unless the hold calls so far are n, the last one hold(id,
// 'store_rollback') on rl:epoch.
func expectHolds(t *testing.T, step string, fake *fakeStore, n int, id string) {
	t.Helper()
	holds := fake.requests("hold")
	if len(holds) != n {
		t.Fatalf("%s: %d hold calls, want %d", step, len(holds), n)
	}
	if n == 0 {
		return
	}
	h := holds[n-1]
	if h.Script.SHA != scripts.Hold.SHA || !slices.Equal(h.Keys, []string{"rl:epoch"}) || !slices.Equal(h.Args, []string{id, "store_rollback"}) {
		t.Fatalf("%s: hold KEYS %q ARGV %q, want rl:epoch and %s, store_rollback", step, h.Keys, h.Args, id)
	}
}

// TestEpochWatchRows: each §A5.7 row gives its state and health reason; the floors in the
// acquire ARGV are the highest hwm and gen read from the record in the joined epoch (never
// a reply's t) and reset on a replacement epoch; a rollback calls hold with the expected id
// and does not join; integrity rows come before the fingerprint row (CFG-6); KEYS[3] is the
// joined epoch's q (rl:e:!:q before joining); each read sets the gauges; a failed hold
// sets the store error; and an acquire's code 20 or 22, each on its own, triggers one early
// read, at most once per interval.
func TestEpochWatchRows(t *testing.T) {
	s := newTestStore(t)
	p := testPolicy(Closed)
	fp := fingerprint.Of(p)
	const tLater = tNow + 9_000_000 // a limited reply's t, past every hwm read below
	fake := newFake(store.Response{Outcome: store.Unavailable}, func(store.Request) store.Response {
		return okResp(2, tLater, "", int64(0))
	})
	l, err := s.limiter(t, p, testInstance(p), fake, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	read := func(r store.Response) {
		fake.setEpoch(r)
		l.watch.read(ctx)
	}
	gauge := func(name string) float64 { return gaugeValue(t, l.Metrics(), name, "") }

	// The startup read finds the store unreachable: state unchanged (no_epoch), store error set.
	waitFor(t, "the startup read", func() bool { return l.Health().StoreError == "unavailable" })
	expectQ(t, "startup", fake, "rl:e:!:q")
	expectHealth(t, "store unreachable before joining", l, reasonNoEpoch, "")
	expectRefused(t, "store unreachable before joining", l, fake, NotReady, Diag{Reason: "no_epoch"})

	read(epochErr("epoch_record_missing"))
	expectHealth(t, "missing, never joined", l, reasonNoEpoch, "")
	if se := l.Health().StoreError; se != "" {
		t.Fatalf("a decoded read left store_error %q", se)
	}

	// state 0, fingerprint matches: join; every read sets the gauges.
	read(okResp(0, 0, "", epochA, fp, int64(tNow+500), int64(5), int64(tNow), int64(7), int64(3), int64(2)))
	expectHealth(t, "join", l, reasonReady, epochA)
	if lag, nb, nm, q, hold := gauge("rl_clock_lag_microseconds"), gauge("rl_tracked_buckets"), gauge("rl_live_markers"),
		gauge("rl_quarantined_keys"), gauge("rl_integrity_hold"); lag != 500 || nb != 7 || nm != 3 || q != 2 || hold != 0 {
		t.Fatalf("gauges: lag %v, tracked %v, live %v, quarantined %v, hold %v; want 500, 7, 3, 2, 0", lag, nb, nm, q, hold)
	}
	expectFloors(t, "join", l, fake, epochA, "1760000000000500", "5")
	read(epochOK(epochA, fp, tNow+1000, 6)) // clock_us = tNow < hwm
	expectQ(t, "joined", fake, "rl:e:"+epochA+":q")
	expectFloors(t, "higher hwm and gen", l, fake, epochA, "1760000000001000", "6")
	// The limited replies above carried t = tLater: the floors stay the record's values.
	expectFloors(t, "after a reply's t", l, fake, epochA, "1760000000001000", "6")
	// So does an allowed reply's t (Watch out: floors come from the record only).
	fake.setAcquire(func(store.Request) store.Response {
		return okResp(1, tLater, "", int64(19_000_000_000), int64(1), int64(1), int64(tLater+lifeUs))
	})
	allowed := l.Acquire(ctx, "k")
	if allowed.Kind != Allowed || allowed.DecisionNs != 1_760_000_009_000_000_000 {
		t.Fatalf("allowed at tLater: %v, decision %d; want allowed at 1760000009000000000", allowed.Kind, allowed.DecisionNs)
	}
	allowed.Permit.Done()
	fake.setAcquire(func(store.Request) store.Response { return okResp(2, tLater, "", int64(0)) })
	expectFloors(t, "after an allowed reply's t", l, fake, epochA, "1760000000001000", "6")

	// state 0 with a lower hwm, then a lower gen, in the same epoch: rollback → hold.
	read(epochOK(epochA, fp, tNow+999, 6))
	expectHealth(t, "lower hwm", l, reasonIntegrityHold, epochA)
	expectHolds(t, "lower hwm", fake, 1, epochA)
	expectRefused(t, "lower hwm", l, fake, StateIntegrityError, Diag{Scope: "epoch", Reason: "integrity_hold"})
	if hold := gauge("rl_integrity_hold"); hold != 1 {
		t.Fatalf("rl_integrity_hold = %v under a hold, want 1", hold)
	}
	read(epochOK(epochA, fp, tNow+1000, 6))
	expectHealth(t, "back at the floors", l, reasonReady, epochA)
	read(epochOK(epochA, fp, tNow+1000, 5))
	expectHealth(t, "lower gen", l, reasonIntegrityHold, epochA)
	expectHolds(t, "lower gen", fake, 2, epochA)
	// Integrity before fingerprint: a lower gen under another fingerprint is still a rollback.
	read(epochOK(epochA, fp, tNow+1000, 6))
	read(epochOK(epochA, otherFP, tNow+1000, 5))
	expectHealth(t, "lower gen, other fingerprint", l, reasonIntegrityHold, epochA)
	expectHolds(t, "lower gen, other fingerprint", fake, 3, epochA)

	// state 22 integrity_hold: held, and nothing more is called.
	read(epochErr("integrity_hold"))
	expectHealth(t, "integrity_hold", l, reasonIntegrityHold, epochA)
	expectHolds(t, "integrity_hold", fake, 3, epochA)
	expectRefused(t, "integrity_hold", l, fake, StateIntegrityError, Diag{Scope: "epoch", Reason: "integrity_hold"})

	// A replacement epoch with a lower hwm and gen: join it with fresh floors.
	read(epochOK(epochB, fp, 100, 1))
	expectHealth(t, "replacement epoch", l, reasonReady, epochB)
	expectFloors(t, "replacement epoch", l, fake, epochB, "100", "1")
	read(epochOK(epochB, fp, 100, 1))
	expectQ(t, "replacement epoch", fake, "rl:e:"+epochB+":q")

	// An epoch this instance already left: rollback → hold for that id, no join; also under
	// another fingerprint (integrity before fingerprint).
	read(epochOK(epochA, fp, tNow+5000, 9))
	expectHealth(t, "left epoch", l, reasonIntegrityHold, epochB)
	expectHolds(t, "left epoch", fake, 4, epochA)
	read(epochOK(epochA, otherFP, tNow+5000, 9))
	expectHealth(t, "left epoch, other fingerprint", l, reasonIntegrityHold, epochB)
	expectHolds(t, "left epoch, other fingerprint", fake, 5, epochA)

	// state 22 after joining: missing, malformed, hwm missing or malformed → state_lost.
	for _, reason := range []string{"epoch_record_missing", "epoch_record_malformed", "hwm_missing", "hwm_malformed"} {
		read(epochOK(epochB, fp, 100, 1))
		expectHealth(t, "rejoin", l, reasonReady, epochB)
		read(epochErr(reason))
		expectHealth(t, reason, l, reasonStateLost, epochB)
		expectRefused(t, reason, l, fake, StateIntegrityError, Diag{Scope: "epoch", Reason: reason})
	}

	// state 0, fingerprint differs: mismatch.
	read(epochOK(epochC, otherFP, 100, 1))
	expectHealth(t, "fingerprint differs", l, reasonMismatch, epochB)
	expectRefused(t, "fingerprint differs", l, fake, ConfigError, Diag{Reason: "fingerprint_mismatch"})

	// Store unreachable or an error reply: the state stays, store_error is set.
	read(epochOK(epochB, fp, 100, 1))
	read(store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR test"})
	expectHealth(t, "error reply", l, reasonReady, epochB)
	if se := l.Health().StoreError; se != "ERR test" {
		t.Fatalf("store_error %q after an error reply, want ERR test", se)
	}
	read(store.Response{Outcome: store.Unavailable})
	expectHealth(t, "unreachable", l, reasonReady, epochB)
	if se := l.Health().StoreError; se != "unavailable" {
		t.Fatalf("store_error %q while unreachable, want unavailable", se)
	}
	// A reply out of shape (which the decoder never passes) is a failed read too: the
	// state stays, store_error says so; both sides of each bound.
	const twoTo53 = int64(1) << 53
	for _, c := range []struct {
		name string
		r    store.Response
		ok   bool
	}{
		{"gen 2^53 - 1", okResp(0, 0, "", epochB, fp, int64(100), twoTo53-1, int64(tNow), 0, 0, 0), true},
		{"gen 2^53", okResp(0, 0, "", epochB, fp, int64(100), twoTo53, int64(tNow), 0, 0, 0), false},
		{"nb -1", okResp(0, 0, "", epochB, fp, int64(100), int64(1), int64(tNow), -1, 0, 0), false},
		{"state 0, id not hex32", okResp(0, 0, "", "0123456789ABCDEF0123456789ABCDEF", fp, int64(100), int64(1), int64(tNow), 0, 0, 0), false},
		{"10 elements", okResp(0, 0, "", epochB, fp, int64(100), int64(1), int64(tNow), 0, 0), false},
		{"state 21", okResp(21, 0, "clock", "", "", 0, 0, 0, 0, 0, ""), false},
		{"quarantined not '' nor a count", okResp(0, 0, "", epochB, fp, int64(100), int64(1), int64(tNow), 0, 0, "x"), false},
	} {
		read(store.Response{Outcome: store.Unavailable}) // store_error "unavailable"
		read(c.r)
		want := "read_epoch reply out of shape"
		if c.ok {
			want = ""
		}
		if se := l.Health().StoreError; se != want {
			t.Fatalf("%s: store_error %q, want %q", c.name, se, want)
		}
		expectHealth(t, c.name, l, reasonReady, epochB)
	}
	// The clock lag is hwm − clock_us, and 0 when negative or when either is unreadable (0).
	// The hwm never falls here, so no read is a rollback.
	for _, c := range []struct {
		hwm, clock int64
		lag        float64
	}{{tNow, tNow + 1, 0}, {tNow, tNow, 0}, {tNow + 1, tNow, 1}, {tNow + 1, 0, 0}} {
		read(okResp(0, 0, "", epochB, fp, c.hwm, twoTo53-1, c.clock, 0, 0, 0))
		if lag := gauge("rl_clock_lag_microseconds"); lag != c.lag {
			t.Fatalf("hwm %d, clock_us %d: lag %v, want %v", c.hwm, c.clock, lag, c.lag)
		}
	}
	if n := len(fake.requests("hold")); n != 5 {
		t.Fatalf("%d hold calls, want 5", n)
	}

	// Code 20 triggers one early read; code 22 right after gets none (at most once per
	// interval, and the interval is 1 h).
	read(epochOK(epochB, fp, tNow+1, twoTo53-1)) // at the floors: joined
	expectHealth(t, "before the early reads", l, reasonReady, epochB)
	reads := fake.count("read_epoch")
	fake.setAcquire(func(store.Request) store.Response { return okResp(20, 0, "epoch_or_fingerprint") })
	if r := l.Acquire(ctx, "k"); r.Kind != ConfigError {
		t.Fatalf("code 20: %v, want config_error", r.Kind)
	}
	waitFor(t, "an early read after code 20", func() bool { return fake.count("read_epoch") == reads+1 })
	fake.setAcquire(func(store.Request) store.Response { return okResp(22, 0, "integrity_hold") })
	if r := l.Acquire(ctx, "k"); r.Kind != StateIntegrityError {
		t.Fatalf("code 22: %v, want state_integrity_error", r.Kind)
	}
	time.Sleep(200 * time.Millisecond)
	if n := fake.count("read_epoch"); n != reads+1 {
		t.Fatalf("%d reads after a second trigger within the interval, want %d", n, reads+1)
	}

	// hold's outcome is reported: a failed hold sets store_error ("unavailable"); a later
	// rollback read decodes (clearing it) and its hold succeeds, so it stays clear.
	fake.setHold(store.Response{Outcome: store.Unavailable})
	read(epochOK(epochB, fp, tNow, twoTo53-1)) // hwm below the floor tNow + 1: rollback
	expectHealth(t, "failed hold", l, reasonIntegrityHold, epochB)
	expectHolds(t, "failed hold", fake, 6, epochB)
	if se := l.Health().StoreError; se != "unavailable" {
		t.Fatalf("store_error %q after a failed hold, want unavailable", se)
	}
	fake.setHold(okResp(40, 0, "", 0))
	read(epochOK(epochB, fp, tNow, twoTo53-1))
	expectHealth(t, "hold succeeds", l, reasonIntegrityHold, epochB)
	expectHolds(t, "hold succeeds", fake, 7, epochB)
	if se := l.Health().StoreError; se != "" {
		t.Fatalf("store_error %q after a decoded read and a successful hold, want empty", se)
	}

	// Code 22 on its own triggers the early read too: a fresh limiter, whose first trigger
	// it is; a second trigger (code 20) within the interval adds none.
	fake2 := newFake(joined(p), func(store.Request) store.Response { return okResp(22, 0, "integrity_hold") })
	l2 := s.ready(t, p, testInstance(p), fake2, Hooks{}, reasonReady)
	reads2 := fake2.count("read_epoch")
	if r := l2.Acquire(ctx, "k"); r.Kind != StateIntegrityError {
		t.Fatalf("code 22 first: %v, want state_integrity_error", r.Kind)
	}
	waitFor(t, "an early read after code 22", func() bool { return fake2.count("read_epoch") == reads2+1 })
	fake2.setAcquire(func(store.Request) store.Response { return okResp(20, 0, "epoch_or_fingerprint") })
	if r := l2.Acquire(ctx, "k"); r.Kind != ConfigError {
		t.Fatalf("code 20 after code 22: %v, want config_error", r.Kind)
	}
	time.Sleep(200 * time.Millisecond)
	if n := fake2.count("read_epoch"); n != reads2+1 {
		t.Fatalf("%d reads after code 22 then code 20 within the interval, want %d", n, reads2+1)
	}
}

// TestNewNeverNil: settings that fail validation (RCP-8, §A3.1 bounds and overflow, the
// controlled clock in production construction), a failing key reader, and a store or signer
// wrapper that returns nil (the store wrapper after a pause long enough for dials begun too
// early to connect) each give a non-nil limiter and an error, health settings_invalid and
// not_ready, and no connection to the store over 3 re-read intervals (§A5.2, T-CFG-3, plan
// S5 step 2). The same valid settings connect, also through a store wrapper that returns the
// adapter it was given.
func TestNewNeverNil(t *testing.T) {
	s := newTestStore(t)
	const interval = 100_000_000 // 100 ms
	base := func() (Policy, Instance) {
		p := testPolicy(Closed)
		in := testInstance(p)
		in.EpochRereadIntervalNs = interval
		return p, in
	}
	cases := []struct {
		name       string
		edit       func(*Policy, *Instance)
		hooks      Hooks
		production bool
	}{
		{name: "RCP-8 sum 61 s", edit: func(_ *Policy, in *Instance) { in.MaxReleaseDelayNs = 25_000_000_000 }},
		{name: "RCP-8 margin 59 ms", edit: func(_ *Policy, in *Instance) { in.MarginNs = 59_000_000 }},
		{name: "cap_units 2^52", edit: func(p *Policy, _ *Instance) { p.Capacity = 4_503_600 }}, // × 10^9 ≥ 2^52
		{name: "int64 overflow", edit: func(p *Policy, _ *Instance) { p.Capacity = math.MaxInt64/1_000_000_000 + 1 }},
		{name: "rate_tokens 2^31", edit: func(p *Policy, _ *Instance) { p.RateTokens = 1 << 31 }},
		{name: "controlled clock in New", edit: func(p *Policy, _ *Instance) { p.ClockSource = Controlled }, production: true},
		{name: "key reader fails", edit: func(*Policy, *Instance) {}, hooks: Hooks{KeyReader: failingReader{}}},
		{name: "store wrapper returns nil", edit: func(*Policy, *Instance) {}, hooks: Hooks{StoreWrapper: func(store.Adapter) store.Adapter {
			time.Sleep(50 * time.Millisecond) // far longer than dials to 127.0.0.1 take
			return nil
		}}},
		{name: "signer wrapper returns nil", edit: func(*Policy, *Instance) {}, hooks: Hooks{SignerWrapper: func(receipt.Signer) receipt.Signer { return nil }}},
	}
	ls := make([]*Limiter, len(cases))
	for i, c := range cases {
		p, in := base()
		c.edit(&p, &in)
		var err error
		if c.production {
			ls[i], err = New(p, in, s.options())
		} else {
			ls[i], err = NewWithHooks(p, in, s.options(), c.hooks)
		}
		if ls[i] == nil {
			t.Fatalf("%s: a nil limiter", c.name)
		}
		t.Cleanup(func() { ls[i].Close() })
		if err == nil || strings.Contains(err.Error(), s.pw) {
			t.Fatalf("%s: err = %v, want an error without the password", c.name, err)
		}
	}
	time.Sleep(3 * time.Duration(interval))
	for i, c := range cases {
		l := ls[i]
		if r := l.Acquire(t.Context(), "k"); r.Kind != NotReady || r.Diag.Reason != "settings_invalid" || r.Permit != nil {
			t.Errorf("%s: Acquire = %v %+v, want not_ready (settings_invalid)", c.name, r.Kind, r.Diag)
		}
		if h := l.Health(); h.Status != "not_ready" || h.Reason != "settings_invalid" {
			t.Errorf("%s: health %s/%s, want not_ready/settings_invalid", c.name, h.Status, h.Reason)
		}
	}
	if n := s.srv.Accepted(); n != 0 {
		t.Fatalf("the store accepted %d connections from invalid limiters, want 0", n)
	}
	p, in := base()
	if _, err := s.limiter(t, p, in, nil, Hooks{}); err != nil {
		t.Fatalf("the valid settings: %v", err)
	}
	waitFor(t, "a connection from a valid limiter", func() bool { return s.srv.Accepted() > 0 })
	s2 := newTestStore(t)
	if _, err := s2.limiter(t, p, in, nil, Hooks{StoreWrapper: func(a store.Adapter) store.Adapter { return a }}); err != nil {
		t.Fatalf("the valid settings, store wrapper returning the adapter: %v", err)
	}
	waitFor(t, "a connection from a valid limiter whose store wrapper returned the adapter", func() bool { return s2.srv.Accepted() > 0 })
}
