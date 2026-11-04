//go:build scripttest

package scripts_test

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/testutil/scriptrun"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// preludeLua is prelude.lua as the package embeds it, so a go test -overlay mutant of the
// prelude reaches these tests too (plan S4 Tests (a)).
//
//go:embed prelude.lua
var preludeLua string

// preludeClient starts a plain store (these ad-hoc scripts have no reply layout, so they
// skip the adapter and the teardown audit) and returns an rl_lib client.
func preludeClient(t *testing.T) *storeproc.Client {
	t.Helper()
	s := storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast})
	c, err := s.Conn("rl_lib")
	if err != nil {
		t.Fatalf("connecting as rl_lib: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// evalPrelude loads header + prelude.lua + body with SCRIPT LOAD and runs it with EVALSHA
// as rl_lib, with no keys and args as ARGV; it returns the reply's elements.
func evalPrelude(t *testing.T, c *storeproc.Client, body string, args ...string) []storeproc.Reply {
	t.Helper()
	return evalShadowed(t, c, "", body, args...)
}

// evalShadowed is evalPrelude with the Lua text before placed ahead of the prelude, so the
// locals it declares shadow, for the prelude's functions, the globals of the same name.
func evalShadowed(t *testing.T, c *storeproc.Client, before, body string, args ...string) []storeproc.Reply {
	t.Helper()
	sha := mustDo(t, c, "SCRIPT", "LOAD", "#!lua flags=no-writes\n"+before+preludeLua+body)
	r := mustDo(t, c, append([]string{"EVALSHA", sha.Str, "0"}, args...)...)
	if r.Kind != '*' {
		t.Fatalf("reply %s, want an array", r)
	}
	return r.Arr
}

// validatorBody applies the prelude validator named ARGV[1] to each later ARGV: 1 or 0.
const validatorBody = `
local fns = { is_uint53 = is_uint53, is_hex32 = is_hex32, is_score = is_score, is_enc = is_enc }
local f, out = fns[ARGV[1]], {}
for i = 2, #ARGV do
  out[#out + 1] = f(ARGV[i]) and 1 or 0
end
return out
`

// checkValidator runs validator fn on each input and compares with want.
func checkValidator(t *testing.T, c *storeproc.Client, fn string, inputs []string, want func(i int) bool) {
	t.Helper()
	got := evalPrelude(t, c, validatorBody, append([]string{fn}, inputs...)...)
	if len(got) != len(inputs) {
		t.Fatalf("%s: %d results for %d inputs", fn, len(got), len(inputs))
	}
	for i, r := range got {
		if w := want(i); r.Kind != ':' || (r.Int == 1) != w {
			t.Errorf("%s(%q) = %s, want %v", fn, inputs[i], r, w)
		}
	}
}

// encVector is one entry of internal/keys/testdata/is_enc_vectors.json.
type encVector struct {
	S     string `json:"s"`
	Valid bool   `json:"valid"`
}

// TestPreludeValidators (plan S4, L2; kills mutant PB4): the prelude's is_uint53, is_hex32
// and is_score on the plan's cases, and is_enc agreeing with Go keys.IsEnc on the shared
// vectors plus 1,000 seeded random strings, through an ad-hoc script (header + prelude.lua
// + a body) run with EVALSHA as rl_lib.
func TestPreludeValidators(t *testing.T) {
	c := preludeClient(t)
	type tc struct {
		in   string
		want bool
	}
	check := func(fn string, cases []tc) {
		t.Helper()
		in := make([]string, len(cases))
		for i, x := range cases {
			in[i] = x.in
		}
		checkValidator(t, c, fn, in, func(i int) bool { return cases[i].want })
	}

	check("is_uint53", []tc{
		{"0", true}, {"1", true}, {"10", true}, {"1760000000000123", true}, {"9007199254740991", true},
		{"01", false}, {"00", false}, {"", false}, {"-1", false}, {"+1", false}, {"1e3", false}, {"1.0", false},
		{" 1", false}, {"1 ", false}, {"0x1", false},
		{"9007199254740992", false}, {"9999999999999999", false}, {"12345678901234567", false},
	})
	check("is_hex32", []tc{
		{"0123456789abcdef0123456789abcdef", true}, {epochID, true},
		{"0123456789ABCDEF0123456789abcdef", false}, {"0123456789abcdef0123456789abcde", false},
		{"0123456789abcdef0123456789abcdef0", false}, {"0123456789abcdeg0123456789abcdef", false},
		{"", false}, {" 123456789abcdef0123456789abcdef", false},
	})
	check("is_score", []tc{
		{"0", true}, {"1760000060000000", true}, {"8998403161718784", true}, {"9007199254740991", true},
		{"inf", false}, {"+inf", false}, {"-inf", false}, {"nan", false}, {"1.5", false}, {"-1", false},
		{"9007199254740992", false}, {"", false}, {"x", false},
	})

	data, err := os.ReadFile("../keys/testdata/is_enc_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []encVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 10 {
		t.Fatalf("%d is_enc vectors, want the shared file's", len(vectors))
	}
	in := make([]string, len(vectors))
	for i, v := range vectors {
		in[i] = v.S
		if keys.IsEnc(v.S) != v.Valid {
			t.Fatalf("vector %q: keys.IsEnc = %v, file says %v", v.S, !v.Valid, v.Valid)
		}
	}
	checkValidator(t, c, "is_enc", in, func(i int) bool { return vectors[i].Valid })

	r := rand.New(rand.NewPCG(4, 1))
	random := make([]string, 1000)
	valid := 0
	for i := range random {
		random[i] = randomEnc(r)
		if keys.IsEnc(random[i]) {
			valid++
		}
	}
	if valid < 200 || valid > 800 {
		t.Fatalf("%d of 1000 random strings pass keys.IsEnc: the mix should hold both kinds", valid)
	}
	checkValidator(t, c, "is_enc", random, func(i int) bool { return keys.IsEnc(random[i]) })
}

// randomEnc draws a string near the is_enc grammar: a valid encoding of 1–256 random bytes,
// that encoding with its last or a random character replaced, or random alphabet text of
// 0–345 characters (the length rule's edges included).
func randomEnc(r *rand.Rand) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	b := make([]byte, 1+r.IntN(256))
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	enc := []byte(base64.RawURLEncoding.EncodeToString(b))
	switch r.IntN(5) {
	case 0, 1:
	case 2:
		enc[len(enc)-1] = alphabet[r.IntN(len(alphabet))]
	case 3:
		enc[r.IntN(len(enc))] = byte(r.IntN(256))
	default:
		n := r.IntN(346)
		if r.IntN(2) == 0 {
			n = 338 + r.IntN(8)
		}
		enc = make([]byte, n)
		for i := range enc {
			enc[i] = alphabet[r.IntN(len(alphabet))]
		}
	}
	return string(enc)
}

// refillBody computes refill (ARGV[1] = "refill", then u, cap, elapsed_us, rate per case)
// or ceil_div (ARGV[1] = "ceil_div", then a, b per case), each result as a %d string.
const refillBody = `
local out = {}
if ARGV[1] == 'refill' then
  for i = 2, #ARGV, 4 do
    local r = refill(tonumber(ARGV[i]), tonumber(ARGV[i + 1]), tonumber(ARGV[i + 2]), tonumber(ARGV[i + 3]))
    out[#out + 1] = string.format('%d', r)
  end
else
  for i = 2, #ARGV, 2 do
    out[#out + 1] = string.format('%d', ceil_div(tonumber(ARGV[i]), tonumber(ARGV[i + 1])))
  end
end
return out
`

// refillWant is refill per §A3.1, in math/big: u when elapsed_us ≤ 0, else
// min(cap, u + elapsed_us × 1000 × rate).
func refillWant(u, capU, elapsed, rate int64) *big.Int {
	if elapsed <= 0 {
		return big.NewInt(u)
	}
	v := new(big.Int).Mul(big.NewInt(elapsed), big.NewInt(1000))
	v.Mul(v, big.NewInt(rate))
	v.Add(v, big.NewInt(u))
	if c := big.NewInt(capU); v.Cmp(c) > 0 {
		return c
	}
	return v
}

// ceilDivWant is ⌈a / b⌉ in math/big.
func ceilDivWant(a, b int64) *big.Int {
	q, m := new(big.Int).DivMod(big.NewInt(a), big.NewInt(b), new(big.Int))
	if m.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// checkBatches runs fn's cases through the prelude, 100 per EVALSHA, and compares each
// result with want; a failure prints its inputs.
func checkBatches(t *testing.T, c *storeproc.Client, fn string, cases [][]int64, want func([]int64) *big.Int) {
	t.Helper()
	for start := 0; start < len(cases); start += 100 {
		batch := cases[start:min(start+100, len(cases))]
		args := []string{fn}
		for _, in := range batch {
			for _, v := range in {
				args = append(args, num(v))
			}
		}
		got := evalPrelude(t, c, refillBody, args...)
		if len(got) != len(batch) {
			t.Fatalf("%s: %d results for %d cases", fn, len(got), len(batch))
		}
		for i, in := range batch {
			if w := want(in).String(); got[i].Kind != '$' || got[i].Str != w {
				t.Errorf("%s%v = %s, want %s", fn, in, got[i], w)
			}
		}
	}
}

// draw returns a uniform bit length in [0, maxBits], then a uniform value of that many bits.
func draw(r *rand.Rand, maxBits int) int64 {
	bits := r.IntN(maxBits + 1)
	if bits == 0 {
		return 0
	}
	return int64(r.Uint64() >> (64 - bits))
}

// TestPreludeRefillCeilDiv (plan S4, L2; DESIGN §C3): refill and ceil_div equal math/big at
// the guard edges, then on a seeded sweep of 10,000 cases each across their §A3.1 domains.
func TestPreludeRefillCeilDiv(t *testing.T) {
	c := preludeClient(t)
	const (
		capU    = 20_000_000_000
		big52   = 1<<52 - 1
		missing = 1_000_000_000 // cap − u in the ns cases
	)
	refillEdges := [][]int64{
		// elapsed_us ≤ 0: u unchanged.
		{5, capU, 0, 10}, {5, capU, -1, 10}, {5, capU, -(tBound - 1), 10},
		// u = cap and u = cap − 1.
		{capU, capU, 1000, 10}, {capU, capU, 1, 1}, {capU - 1, capU, 1, 1}, {capU - 1, capU, 1, 10},
		// ns = elapsed × 1000 at missing − 1000, missing and ≥ 2^53 (rate 1, so add = ns).
		{capU - missing, capU, missing/1000 - 1, 1}, {capU - missing, capU, missing / 1000, 1},
		{0, big52, two53/1000 + 1, 1}, {0, big52, two53 / 1000, 1},
		// add = ns × rate at missing − 1, missing and ≥ 2^53, with ns below missing.
		{capU - 21_001, capU, 7, 3}, {capU - 21_000, capU, 7, 3}, {capU - 20_999, capU, 7, 3},
		{0, big52, 1_000_000_000, 1<<31 - 1}, {0, big52, 4194, 1<<31 - 1}, {1, big52, 4195, 1<<31 - 1},
	}
	checkBatches(t, c, "refill", refillEdges, func(in []int64) *big.Int { return refillWant(in[0], in[1], in[2], in[3]) })
	ceilEdges := [][]int64{
		{0, 7}, {0, 1}, {7, 7}, {6, 7}, {8, 7}, {1, 1}, {1, two53 - 2},
		{two53 - 4, 3}, {two53 - 2, 1}, {1<<52 - 1, 1 << 52}, {1 << 52, 1<<52 - 1}, {two53 - 1 - 999_999_937, 999_999_937},
	}
	checkBatches(t, c, "ceil_div", ceilEdges, func(in []int64) *big.Int { return ceilDivWant(in[0], in[1]) })

	r := rand.New(rand.NewPCG(1, 1))
	refills := make([][]int64, 10_000)
	for i := range refills {
		capV := draw(r, 52)
		for capV < 1 {
			capV = draw(r, 52)
		}
		u := draw(r, 52)
		for u > capV {
			u = draw(r, 52)
		}
		elapsed := draw(r, 53)
		for elapsed >= tBound {
			elapsed = draw(r, 53)
		}
		if r.IntN(2) == 0 {
			elapsed = -elapsed
		}
		rate := draw(r, 31)
		for rate < 1 {
			rate = draw(r, 31)
		}
		refills[i] = []int64{u, capV, elapsed, rate}
	}
	checkBatches(t, c, "refill", refills, func(in []int64) *big.Int { return refillWant(in[0], in[1], in[2], in[3]) })
	divs := make([][]int64, 10_000)
	for i := range divs {
		a, b := draw(r, 53), draw(r, 53)
		for b < 1 || a+b >= two53 {
			a, b = draw(r, 53), draw(r, 53)
		}
		divs[i] = []int64{a, b}
	}
	checkBatches(t, c, "ceil_div", divs, func(in []int64) *big.Int { return ceilDivWant(in[0], in[1]) })
	if t.Failed() {
		t.Logf("seeded sweep: rand.NewPCG(1, 1), %d refill and %d ceil_div cases", len(refills), len(divs))
	}
}

// evalWritingPrelude is evalPrelude for a body that writes: its header allows writes, and
// keys go in KEYS.
func evalWritingPrelude(t *testing.T, c *storeproc.Client, body string, keys []string, args ...string) []storeproc.Reply {
	t.Helper()
	sha := mustDo(t, c, "SCRIPT", "LOAD", "#!lua\n"+preludeLua+body)
	cmd := append([]string{"EVALSHA", sha.Str, strconv.Itoa(len(keys))}, keys...)
	r := mustDo(t, c, append(cmd, args...)...)
	if r.Kind != '*' {
		t.Fatalf("reply %s, want an array", r)
	}
	return r.Arr
}

// writeHelpersBody writes an epoch record KEYS[1] with hwm ARGV[1] and nb ARGV[2], loads it
// into E, sets T to ARGV[3], then calls finish with code OK, an empty reason and 0, and
// dec_counter('nb', ARGV[4]). It returns finish's reply, the record's hwm and nb as stored
// afterwards, and E.nb, and deletes the record.
const writeHelpersBody = `
K.epoch = KEYS[1]
redis.call('DEL', K.epoch)
redis.call('HSET', K.epoch, 'hwm', ARGV[1], 'nb', ARGV[2])
E = to_map(redis.call('HGETALL', K.epoch))
T = tonumber(ARGV[3])
local r = finish(OK, '', 0)
dec_counter('nb', tonumber(ARGV[4]))
local after = to_map(redis.call('HGETALL', K.epoch))
redis.call('DEL', K.epoch)
return { r[1], string.format('%d', r[2]), r[3], r[4], after.hwm, after.nb, E.nb }
`

// TestPreludeWriteHelpers (L2): finish stores the budget time as the high-water mark when it
// is later than the mark, and only then, and replies {code, T, reason, …} (§A3.3, CLK-4);
// dec_counter takes n off the record's counter in the store and in E, never below 0, also for
// n = 0 (§A3.3). No S4 script calls either helper. It runs header + prelude.lua + a body with
// EVALSHA as rl_lib, like TestPreludeValidators, but on a scriptrun store, whose teardown
// audit must pass (plan S4 Tests (a)): the body deletes the record it wrote.
func TestPreludeWriteHelpers(t *testing.T) {
	c := dial(t, scriptrun.Store(t), "rl_lib")
	cases := []struct {
		name            string
		hwm, nb, tUs, n int64
		wantHwm         int64  // the stored high-water mark after finish
		wantNb          string // nb after dec_counter, stored and in E
	}{
		{"T after the mark, n below nb", hwm0, 5, now0, 2, now0, "3"},
		{"T equal to the mark, n above nb", hwm0, 2, hwm0, 5, hwm0, "0"},
		{"T before the mark, n = 0", now0, 7, hwm0, 0, now0, "7"},
	}
	for _, tc := range cases {
		got := evalWritingPrelude(t, c, writeHelpersBody, []string{"rl:epoch"}, num(tc.hwm), num(tc.nb), num(tc.tUs), num(tc.n))
		want := []string{":40", strconv.Quote(num(tc.tUs)), `""`, ":0", strconv.Quote(num(tc.wantHwm)), strconv.Quote(tc.wantNb), strconv.Quote(tc.wantNb)}
		if len(got) != len(want) {
			t.Errorf("%s: %d results, want %d: %v", tc.name, len(got), len(want), got)
			continue
		}
		for i, r := range got {
			if r.String() != want[i] {
				t.Errorf("%s: result %d = %s, want %s (finish's reply 0–3, then the stored hwm, the stored nb, E.nb)", tc.name, i, r, want[i])
			}
		}
	}
}

// chunkRecorder goes ahead of the prelude: a local redis whose call records each command, its
// arguments in order, instead of running it, so the prelude's chunked helpers call it.
const chunkRecorder = `
local calls = {}
local redis = { call = function(...) calls[#calls + 1] = { ... } end }
`

// chunkBody runs hdel_chunked on the hash h and zrem_chunked on the sorted set z, each with
// the list e1 … en for n = ARGV[1], and returns the recorded commands.
const chunkBody = `
local list = {}
for i = 1, tonumber(ARGV[1]) do
  list[i] = 'e' .. string.format('%d', i)
end
hdel_chunked('h', list)
zrem_chunked('z', list)
return calls
`

// TestPreludeChunkedHelpers (plan S7 step 2 and Watch out; DESIGN §A4.8 rule 3, §A3.0 row 11;
// AGENTS.md §3 bounded work): hdel_chunked and zrem_chunked send at most 500 list elements per
// command. With redis.call shadowed by a recorder (no store state is touched), for lists of 0,
// 500, 501, 1,000 and 1,001 elements: HDEL h and ZREM z each come ⌈n / 500⌉ times and nothing
// else is called, every command carries 1 to 500 elements, and together each helper's commands
// carry every element exactly once, so no chunk overlaps another.
func TestPreludeChunkedHelpers(t *testing.T) {
	c := preludeClient(t)
	for _, n := range []int{0, 500, 501, 1000, 1001} {
		got := evalShadowed(t, c, chunkRecorder, chunkBody, strconv.Itoa(n))
		want := make([]string, n)
		for i := range want {
			want[i] = "e" + strconv.Itoa(i+1)
		}
		slices.Sort(want)
		sent := map[string][]string{} // command → the elements its calls carried
		calls := map[string]int{}
		for i, call := range got {
			if call.Kind != '*' || len(call.Arr) < 2 {
				t.Fatalf("n = %d: call %d is %s, want [command, key, element…]", n, i+1, call)
			}
			name, key, elems := call.Arr[0].Str, call.Arr[1].Str, call.Arr[2:]
			if (name != "HDEL" || key != "h") && (name != "ZREM" || key != "z") {
				t.Errorf("n = %d: call %d is %s %s, want only HDEL h and ZREM z", n, i+1, name, key)
				continue
			}
			calls[name]++
			if len(elems) < 1 || len(elems) > 500 {
				t.Errorf("n = %d: %s call %d carries %d elements, want 1 to 500", n, name, calls[name], len(elems))
			}
			for _, e := range elems {
				sent[name] = append(sent[name], e.Str)
			}
		}
		for _, name := range []string{"HDEL", "ZREM"} {
			if wantCalls := (n + 499) / 500; calls[name] != wantCalls {
				t.Errorf("n = %d: %d %s calls, want %d", n, calls[name], name, wantCalls)
			}
			slices.Sort(sent[name])
			if !slices.Equal(sent[name], want) {
				t.Errorf("n = %d: %s carried %d elements (sorted, first %q), want each of the %d exactly once", n, name, len(sent[name]), sent[name][:min(3, len(sent[name]))], n)
			}
		}
	}
}
