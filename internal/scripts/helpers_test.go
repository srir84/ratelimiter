//go:build scripttest

package scripts_test

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/srir84/ratelimiter/internal/fingerprint"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/settings"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/store/resp"
	"github.com/srir84/ratelimiter/internal/testutil/scriptrun"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
	"github.com/srir84/ratelimiter/internal/testutil/storestate"
)

// The L2 tests' fixed values (IMPLEMENTATION_PLAN S4 Tests (a)). Times are µs since the
// Unix epoch, at least 10^15 and not round, so a number stored through tostring or .. would
// differ from the exact strings the tests compare (AGENTS.md §3).
const (
	epochID = "9c56cc51b374c3ba189210d5b6d4bf57"
	otherID = "0123456789abcdef0123456789abcdef"
	newID   = "f00dfacef00dfacef00dfacef00dface"

	hwm0    = 1_760_000_000_123_456 // the record's high-water mark
	now0    = 1_760_000_000_223_456 // the controlled time: 100 ms after hwm0
	origin0 = 1_759_999_000_000_001 // the record's origin and created
	gen0    = 7                     // the record's gen

	tBound  = 8_998_403_161_718_784 // TBOUND = 2^53 − 2^43 (§A3.1)
	two53   = 9_007_199_254_740_992 // 2^53
	maxUint = 9_007_199_254_740_991 // 2^53 − 1

	// The default test policy's script arguments, written out (§A5.10 defaults): capacity
	// 20 × rate_period_ns 10^9, 10 tokens per period, a 60 s receipt lifetime.
	capUnits   = 20_000_000_000
	ratePeriod = 1_000_000_000
	rateTokens = 10
	lifetimeUs = 60_000_000
	maxTracked = 1000

	// Budget key encodings (base64url, §A3.2) of the keys "a", "b", "c" and "d".
	encA = "YQ"
	encB = "Yg"
	encC = "Yw"
	encD = "ZA"
)

// testPolicy is the default policy with the required limits set and the controlled clock.
func testPolicy() settings.Policy {
	p := settings.DefaultPolicy()
	p.MaxTrackedBuckets, p.MaxLiveMarkers = maxTracked, 100_000
	p.ClockSource = settings.Controlled
	return p
}

// storeClockPolicy is testPolicy with the store's clock.
func storeClockPolicy() settings.Policy {
	p := testPolicy()
	p.ClockSource = settings.Store
	return p
}

// num formats an integer as the store holds it.
func num(n int64) string { return strconv.FormatInt(n, 10) }

// env is one test's store: the handle, the policy its record carries, an rl_fixture
// connection that builds states, an rl_lib connection that snapshots them, and adapters
// as rl_lib and rl_admin.
type env struct {
	t   *testing.T
	h   *scriptrun.Handle
	p   settings.Policy
	fp  string
	fix *storeproc.Client
	lib *storeproc.Client
	la  *store.StoreAdapter // rl_lib: acquire, hold, read_epoch
	aa  *store.StoreAdapter // rl_admin: admin_*, read_epoch
}

// newEnv starts a store for t whose epoch records carry p's fingerprint; more policies may
// be registered for records that carry theirs.
func newEnv(t *testing.T, p settings.Policy, more ...settings.Policy) *env {
	t.Helper()
	h := scriptrun.Store(t, append([]settings.Policy{p}, more...)...)
	return &env{
		t: t, h: h, p: p, fp: fingerprint.Of(p),
		fix: dial(t, h, "rl_fixture"),
		lib: dial(t, h, "rl_lib"),
		la:  h.Adapter(t, "rl_lib", p),
		aa:  h.Adapter(t, "rl_admin", p),
	}
}

// dial opens a client as user, closed at cleanup.
func dial(t *testing.T, h *scriptrun.Handle, user string) *storeproc.Client {
	t.Helper()
	c, err := h.Conn(user)
	if err != nil {
		t.Fatalf("connecting as %s: %v", user, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// do runs one fixture command as rl_fixture and fails the test on an error.
func (e *env) do(args ...string) storeproc.Reply {
	e.t.Helper()
	return mustDo(e.t, e.fix, args...)
}

// mustDo runs one command on c and fails the test on an error or an error reply.
func mustDo(t *testing.T, c *storeproc.Client, args ...string) storeproc.Reply {
	t.Helper()
	r, err := c.Do(args...)
	if err != nil {
		t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
	if r.Kind == '-' {
		t.Fatalf("%s: %s", strings.Join(args, " "), r.Str)
	}
	return r
}

// rec is an epoch record, field → value.
type rec map[string]string

// healthy is a well-formed record of epochID with fingerprint fp, no hold, and one tracked
// key (nb 1), the bucket that bucketA writes.
func healthy(fp string) rec {
	return rec{
		"id": epochID, "fp": fp, "schema": "1", "clock": "controlled", "hold": "",
		"hwm": num(hwm0), "origin": num(origin0), "created": num(origin0),
		"gen": num(gen0), "nb": "1", "nm": "0",
	}
}

// with returns a copy of r with the given field/value pairs set; a value "<del>" removes
// the field.
func (r rec) with(fv ...string) rec {
	out := maps.Clone(r)
	for i := 0; i+1 < len(fv); i += 2 {
		if fv[i+1] == "<del>" {
			delete(out, fv[i])
		} else {
			out[fv[i]] = fv[i+1]
		}
	}
	return out
}

// hset writes the hash key with fields fv (pairs), first deleting the key.
func (e *env) hset(key string, fv ...string) {
	e.t.Helper()
	e.do("DEL", key)
	if len(fv) > 0 {
		e.do(append([]string{"HSET", key}, fv...)...)
	}
}

// record writes r as rl:epoch, in field order.
func (e *env) record(r rec) {
	e.t.Helper()
	var fv []string
	for _, f := range slices.Sorted(maps.Keys(r)) {
		fv = append(fv, f, r[f])
	}
	e.hset(keys.EpochKey, fv...)
}

// clock sets the controlled time.
func (e *env) clock(us int64) {
	e.t.Helper()
	e.do("SET", keys.CtlTimeKey, num(us))
}

// Store key names in the epochID namespace.
func kKey(enc string) string  { return keys.BucketKey(epochID, enc) }
func miKey(enc string) string { return keys.MarkerIndexKey(epochID, enc) }

var (
	ixKey = keys.IndexKey(epochID)
	qKey  = keys.QuarantineKey(epochID)
)

// The bucket bucketA writes for a key: 5 tokens left at hwm0 − 1 s, generation gen0,
// sequence 3, latest receipt expiry hwm0 + 59 s, no markers, and its ix score at that
// expiry, its eligibility (§A3.2: refilling 15 tokens takes 1.5 s, before the expiry).
const (
	bucketU = 5_000_000_000
	bucketT = hwm0 - 1_000_000
	bucketS = 3
	bucketX = hwm0 + 59_000_000
)

// bucketA writes that bucket for enc, with its ix member.
func (e *env) bucketA(enc string) {
	e.t.Helper()
	e.hset(kKey(enc), "u", num(bucketU), "t", num(bucketT), "g", num(gen0), "s", num(bucketS), "x", num(bucketX), "mc", "0")
	e.do("ZADD", ixKey, num(bucketX), enc)
}

// base writes the healthy record, the controlled time now0 and enc's bucket.
func (e *env) base(enc string) {
	e.t.Helper()
	e.record(healthy(e.fp))
	e.clock(now0)
	e.bucketA(enc)
}

// snap takes a snapshot as rl_lib.
func (e *env) snap() storestate.State {
	e.t.Helper()
	s, err := storestate.Snapshot(e.lib)
	if err != nil {
		e.t.Fatalf("snapshot: %v", err)
	}
	return s
}

// acquireReq is an acquire of enc in epochID with fingerprint fp and policy p's limits,
// floors 0 (§A4.1 KEYS and ARGV order).
func acquireReq(p settings.Policy, fp, enc string) store.Request {
	return store.Request{
		Kind:   store.Call,
		Script: scripts.Acquire,
		Keys:   []string{keys.EpochKey, keys.CtlTimeKey, ixKey, qKey, kKey(enc), miKey(enc)},
		Args: []string{
			epochID, fp, enc,
			num(p.Capacity * p.RatePeriodNs), num(p.RatePeriodNs), num(p.RateTokens),
			num(p.ReceiptLifetimeNs / 1000), num(p.MaxTrackedBuckets), "0", "0",
		},
	}
}

// Positions in acquire's ARGV (§A4.1).
const (
	argEpochID    = 0
	argFP         = 1
	argMaxTracked = 7
	argFloorHwm   = 8
	argFloorGen   = 9
)

// acquire runs an acquire of enc as rl_lib and returns its decoded reply.
func (e *env) acquire(enc string) []any {
	e.t.Helper()
	return e.run(e.la, acquireReq(e.p, e.fp, enc))
}

// run calls req through a with scriptrun.Run and returns the reply's elements; anything
// but a decoded reply (a layout or range error, NOPERM, an ACL failure, an AUDIT error)
// fails the test.
func (e *env) run(a scriptrun.Caller, req store.Request) []any {
	e.t.Helper()
	return runOK(e.t, a, req)
}

func runOK(t *testing.T, a scriptrun.Caller, req store.Request) []any {
	t.Helper()
	r := scriptrun.Run(t, a, req)
	if r.Outcome != store.OK {
		t.Fatalf("%s: outcome %v (store error %q, raw reply %s), want a decoded reply", req.Script.Name, r.Outcome, r.StoreError, r.Raw)
	}
	return values(r.Reply.Elems)
}

// values turns reply elements into int64 and string values.
func values(vs []resp.Value) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		switch v.Kind {
		case resp.Int:
			out[i] = v.Int
		case resp.Bulk:
			out[i] = v.Str
		case resp.Simple, resp.Error, resp.Array: // no §A4.9 reply holds one; Decode refuses them
			out[i] = v.String()
		}
	}
	return out
}

// rep builds an expected reply: ints become int64, strings stay strings.
func rep(vals ...any) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		switch v := v.(type) {
		case int:
			out[i] = int64(v)
		default:
			out[i] = v
		}
	}
	return out
}

// checkReply fails the test unless got equals want.
func checkReply(t *testing.T, what string, got, want []any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: reply %v, want %v", what, got, want)
	}
}

// cloneState deep-copies a snapshot.
func cloneState(s storestate.State) storestate.State {
	out := make(storestate.State, len(s))
	for k, v := range s {
		v.Fields = maps.Clone(v.Fields)
		out[k] = v
	}
	return out
}

// setField sets a hash field in st, creating the hash when the key is missing.
func setField(st storestate.State, key, field, value string) {
	v, ok := st[key]
	if !ok {
		v = storestate.Value{Type: storestate.Hash, Fields: map[string]string{}}
	}
	v.Fields[field] = value
	st[key] = v
}

// checkState fails the test unless got equals want, listing the differences.
func checkState(t *testing.T, what string, got, want storestate.State) {
	t.Helper()
	if d := diff(want, got); len(d) > 0 {
		t.Errorf("%s: the store differs from the expected state (- expected, + got):\n  %s", what, strings.Join(d, "\n  "))
	}
}

// checkUnchanged fails the test unless the store still equals before.
func (e *env) checkUnchanged(what string, before storestate.State) {
	e.t.Helper()
	checkState(e.t, what, e.snap(), before)
}

// diff lists the differences between two snapshots, one line per key or field.
func diff(a, b storestate.State) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(mergeKeys(a, b))) {
		va, aok := a[k]
		vb, bok := b[k]
		switch {
		case !aok:
			out = append(out, fmt.Sprintf("+ %s %s", k, show(vb)))
		case !bok:
			out = append(out, fmt.Sprintf("- %s %s", k, show(va)))
		case va.Type != vb.Type || va.Str != vb.Str:
			out = append(out, fmt.Sprintf("- %s %s", k, show(va)), fmt.Sprintf("+ %s %s", k, show(vb)))
		default:
			for _, f := range slices.Sorted(maps.Keys(mergeKeys(va.Fields, vb.Fields))) {
				fa, faok := va.Fields[f]
				fb, fbok := vb.Fields[f]
				switch {
				case !faok:
					out = append(out, fmt.Sprintf("+ %s %q = %q", k, f, fb))
				case !fbok:
					out = append(out, fmt.Sprintf("- %s %q = %q", k, f, fa))
				case fa != fb:
					out = append(out, fmt.Sprintf("- %s %q = %q", k, f, fa), fmt.Sprintf("+ %s %q = %q", k, f, fb))
				}
			}
		}
	}
	return out
}

func mergeKeys[V any](a, b map[string]V) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func show(v storestate.Value) string {
	if v.Type == storestate.String {
		return fmt.Sprintf("string %q", v.Str)
	}
	return fmt.Sprintf("%s %v", v.Type, v.Fields)
}
