package store

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/settings"
	"github.com/srir84/ratelimiter/internal/store/resp"
)

// field is one integer position of a reply and its inclusive bounds.
type field struct {
	pos    int
	lo, hi int64
}

// row is one (script, code) of DESIGN §A4.9, written out from IMPLEMENTATION_PLAN S3 step
// 7: a valid reply, its element kinds (I integer, S bulk string, E integer or ”), whether
// t_us must be 0, the bounds of every integer past t_us, the limits to decode with, and fix,
// which keeps fields that depend on t_us valid when a test changes t_us.
type row struct {
	script string
	kinds  string
	reply  resp.Value
	zeroT  bool
	minLen bool // due_entries code 0: a minimum length, not an exact one
	fields []field
	expiry int64 // ReceiptExpiryUs, for refund rows
	fix    func(e []resp.Value)
}

func (r row) name() string {
	return r.script + " code " + strconv.FormatInt(r.reply.Elems[0].Int, 10)
}

func (r row) limits() Limits {
	lim := testLimits()
	lim.ReceiptExpiryUs = r.expiry
	return lim
}

// refusals are rows {c, 0, reason}; keyResults are rows {c, t, reason}.
func refusals(script string, codes ...int) []row {
	out := make([]row, len(codes))
	for i, c := range codes {
		out[i] = row{script: script, kinds: "IIS", reply: rep(c, 0, "reason"), zeroT: true}
	}
	return out
}

func keyResults(script string, codes ...int) []row {
	out := make([]row, len(codes))
	for i, c := range codes {
		out[i] = row{script: script, kinds: "IIS", reply: rep(c, tNow, "reason")}
	}
	return out
}

func wide(positions ...int) []field {
	out := make([]field, len(positions))
	for i, p := range positions {
		out[i] = field{p, 0, max53}
	}
	return out
}

// layoutRows lists every (script, code) of §A4.9 with a valid reply.
func layoutRows() []row {
	const id = "0123456789abcdef0123456789abcdef"
	const fp = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	setT := func(pos int, delta int64) func(e []resp.Value) {
		return func(e []resp.Value) { e[pos] = vInt(e[1].Int + delta) }
	}
	zero := func(pos int) func(e []resp.Value) {
		return func(e []resp.Value) { e[pos] = vInt(0) }
	}
	rows := []row{
		{script: "acquire", kinds: "IISIIII", reply: allowed(5),
			fields: []field{{3, 0, 19_000_000_000}, {4, 1, max53}, {5, 1, max53}, {6, tNow + lifeUs, tNow + lifeUs}},
			fix:    setT(6, lifeUs)},
		{script: "acquire", kinds: "IISI", reply: rep(2, tNow, "", 500_000_000), fields: []field{{3, 0, 999_999_999}}},
		{script: "refund", kinds: "IISI", reply: rep(3, tNow, "", 1_000_000_000), fields: []field{{3, 0, 1_000_000_000}}},
		// credit_us ≤ t_us binds when the receipt expires later …
		{script: "refund", kinds: "IISI", reply: rep(4, tNow, "", tNow-10), expiry: tNow + lifeUs,
			fields: []field{{3, 0, tNow}}, fix: zero(3)},
		// … and credit_us < ReceiptExpiryUs when it expired before t_us.
		{script: "refund", kinds: "IISI", reply: rep(4, tNow, "", tNow-200), expiry: tNow - 100,
			fields: []field{{3, 0, tNow - 101}}, fix: zero(3)},
		{script: "due_entries", kinds: "IISISS", reply: rep(0, tNow, "", tNow-5, "m1", "m2"), minLen: true,
			fields: []field{{3, 0, tNow}}, fix: zero(3)},
		{script: "reclaim", kinds: "IISI", reply: rep(40, 0, "", 17), zeroT: true, fields: wide(3)},
		{script: "read_epoch", kinds: "IISSSIIIIIE", reply: rep(0, 0, "", id, fp, tNow, 3, tNow+7, 10, 2, 0), zeroT: true,
			fields: wide(5, 6, 7, 8, 9, 10)},
		{script: "read_epoch", kinds: "IISSSIIIIIE", reply: rep(22, 0, "integrity_hold", id, fp, tNow, 3, tNow, 10, 2, ""), zeroT: true,
			fields: wide(5, 6, 7, 8, 9)},
		{script: "hold", kinds: "IISI", reply: rep(40, 0, "", 0), zeroT: true, fields: wide(3)},
		{script: "admin_start_epoch", kinds: "IISS", reply: rep(40, tNow, "", id)},
		{script: "admin_restore", kinds: "IISI", reply: rep(40, tNow, "", 4), fields: wide(3)},
		{script: "admin_clear", kinds: "IISI", reply: rep(40, tNow, "", 0), fields: wide(3)},
		{script: "admin_status", kinds: "IISSSSEIII", reply: rep(40, 0, "", id, fp, "", tNow, 10, 2, 1), zeroT: true,
			fields: wide(6, 7, 8, 9)},
		{script: "admin_status", kinds: "IISSSSEIII", reply: rep(40, 0, "", id, fp, "integrity_hold", "", 10, 2, 1), zeroT: true,
			fields: wide(7, 8, 9)},
		{script: "admin_inspect", kinds: "IISIEEEEEEESIEE",
			reply: rep(40, 0, "", 1, 5, tNow, 3, 2, tNow+lifeUs, 2, tNow+lifeUs, "", 2, tNow+100, tNow+200), zeroT: true,
			fields: wide(3, 4, 5, 6, 7, 8, 9, 10, 13, 14)},
		{script: "admin_inspect", kinds: "IISIEEEEEEESI",
			reply: rep(40, 0, "", 0, "", "", "", "", "", "", "", "quarantined", 0), zeroT: true, fields: wide(3)},
	}
	for c := int64(30); c <= 33; c++ {
		rows = append(rows, row{script: "cleanup_entry", kinds: "IISI", reply: rep(c, tNow, "", 2), fields: []field{{3, 0, 1000}}})
	}
	for _, rs := range [][]row{
		refusals("acquire", 20, 21, 22, 25), keyResults("acquire", 23, 24),
		keyResults("refund", 10, 11, 23, 24), refusals("refund", 20, 21, 22, 25),
		refusals("due_entries", 20, 21, 22),
		refusals("cleanup_entry", 20, 21, 22, 25),
		refusals("hold", 41),
		refusals("admin_start_epoch", 41, 21),
		refusals("admin_restore", 41, 21, 22, 25),
		refusals("admin_clear", 41, 21, 22, 25),
		refusals("admin_status", 41),
		refusals("admin_inspect", 41, 25),
	} {
		rows = append(rows, rs...)
	}
	return rows
}

// withElems returns r's reply with its elements replaced.
func withElems(e []resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: e} }

func cloneElems(v resp.Value) []resp.Value { return append([]resp.Value(nil), v.Elems...) }

// wrongKind returns a value of a kind the layout letter k does not allow.
func wrongKind(k byte) []resp.Value {
	switch k {
	case 'I':
		return []resp.Value{{Kind: resp.Bulk, Str: "1"}, {Kind: resp.Simple, Str: "1"}, {Kind: resp.Bulk, Null: true}}
	case 'S':
		return []resp.Value{vInt(1), {Kind: resp.Bulk, Null: true}, {Kind: resp.Simple, Str: "x"}}
	}
	return []resp.Value{{Kind: resp.Bulk, Str: "x"}, {Kind: resp.Bulk, Null: true}, {Kind: resp.Simple, Str: ""}}
}

func TestDecodeLayouts(t *testing.T) {
	scriptsSeen := map[string]bool{}
	for _, r := range layoutRows() {
		scriptsSeen[r.script] = true
		lim := r.limits()
		t.Run(r.name(), func(t *testing.T) {
			if len(r.kinds) != len(r.reply.Elems) {
				t.Fatalf("test row: %d kinds for %d elements", len(r.kinds), len(r.reply.Elems))
			}
			got, err := Decode(r.script, r.reply, lim)
			if err != nil {
				t.Fatalf("Decode(%s) = %v, want ok", r.reply, err)
			}
			e := r.reply.Elems
			if got.Code != e[0].Int || got.TUs != e[1].Int || got.Reason != e[2].Str || len(got.Elems) != len(e) {
				t.Errorf("Decode = {%d %d %q, %d elements}, want {%d %d %q, %d elements}",
					got.Code, got.TUs, got.Reason, len(got.Elems), e[0].Int, e[1].Int, e[2].Str, len(e))
			}
			if !r.minLen { // due_entries code 0 has a minimum length: its own subtest below
				long := append(cloneElems(r.reply), vInt(0))
				if _, err := Decode(r.script, withElems(long), lim); err == nil {
					t.Errorf("length %d (one more) decoded", len(long))
				}
				short := cloneElems(r.reply)[:len(e)-1]
				if _, err := Decode(r.script, withElems(short), lim); err == nil {
					t.Errorf("length %d (one less) decoded", len(short))
				}
			}
			for i := range e {
				for _, bad := range wrongKind(r.kinds[i]) {
					m := cloneElems(r.reply)
					m[i] = bad
					if _, err := Decode(r.script, withElems(m), lim); err == nil {
						t.Errorf("element %d (%c) as %s decoded", i, r.kinds[i], bad)
					}
				}
			}
		})
	}
	if len(scriptsSeen) != 12 {
		t.Errorf("rows cover %d scripts, want the 12 of §A4.1", len(scriptsSeen))
	}

	t.Run("unknown code", func(t *testing.T) {
		for s := range scriptsSeen {
			for _, code := range []int{-1, 5, 99, 1 << 40} {
				if _, err := Decode(s, rep(code, 0, ""), testLimits()); err == nil {
					t.Errorf("%s code %d decoded", s, code)
				}
			}
		}
		for _, c := range []struct {
			script string
			code   int
		}{{"acquire", 3}, {"refund", 1}, {"due_entries", 30}, {"read_epoch", 40}, {"hold", 0}, {"admin_inspect", 21}} {
			if _, err := Decode(c.script, rep(c.code, 0, "", 0), testLimits()); err == nil {
				t.Errorf("%s with code %d (another script's) decoded", c.script, c.code)
			}
		}
		if _, err := Decode("no_such_script", allowed(1), testLimits()); err == nil {
			t.Error("an unknown script's reply decoded")
		}
	})
	t.Run("null element", func(t *testing.T) {
		_, err := Decode("acquire", rep(2, tNow, resp.Value{Kind: resp.Bulk, Null: true}, 0), testLimits())
		if err == nil || !strings.Contains(err.Error(), "element 2 is null bulk") {
			t.Errorf("Decode with a null reason = %v, want an error naming element 2 as null bulk", err)
		}
	})
	t.Run("non-array reply", func(t *testing.T) {
		for _, v := range []resp.Value{
			vInt(1), {Kind: resp.Bulk, Str: "1"}, {Kind: resp.Simple, Str: "OK"}, {Kind: resp.Error, Str: "ERR x"},
			{Kind: resp.Array, Null: true}, {Kind: resp.Bulk, Null: true}, {Kind: resp.Array}, {},
		} {
			if _, err := Decode("acquire", v, testLimits()); err == nil {
				t.Errorf("reply %s decoded", v)
			}
		}
	})
	t.Run("due_entries lengths", func(t *testing.T) {
		if _, err := Decode("due_entries", rep(0, tNow, ""), testLimits()); err == nil {
			t.Error("due_entries length 3 decoded")
		}
		if _, err := Decode("due_entries", rep(0, tNow, "", 0), testLimits()); err != nil {
			t.Errorf("due_entries length 4: %v", err)
		}
		long := []any{0, tNow, "", tNow - 1}
		for i := range 1000 {
			long = append(long, "member"+strconv.Itoa(i))
		}
		if got, err := Decode("due_entries", rep(long...), testLimits()); err != nil || len(got.Elems) != 1004 {
			t.Errorf("due_entries length 1,004: %d elements, %v", len(got.Elems), err)
		}
	})
	t.Run("admin_inspect n=2 with 14 elements", func(t *testing.T) {
		v := rep(40, 0, "", 1, 5, tNow, 3, 2, tNow+lifeUs, 2, tNow+lifeUs, "", 2, tNow+100)
		if _, err := Decode("admin_inspect", v, testLimits()); err == nil {
			t.Error("admin_inspect n=2 with 14 elements decoded")
		}
		if _, err := Decode("admin_inspect", rep(40, 0, "", 0, "", "", "", "", "", "", "", "", -1), testLimits()); err == nil {
			t.Error("admin_inspect n=-1 decoded")
		}
	})
}

// rangeCase is one bound of TestDecodeRanges: a reply and whether it must decode.
type rangeCase struct {
	name   string
	script string
	reply  resp.Value
	lim    Limits
	ok     bool
}

// rangeCases puts every integer of every row at each bound and one past it: t_us (0 where
// the layout shows 0, else 0 … TBOUND − 1) and each listed field.
func rangeCases() []rangeCase {
	var out []rangeCase
	for _, r := range layoutRows() {
		tField := field{1, 0, tBnd - 1}
		if r.zeroT {
			tField = field{1, 0, 0}
		}
		for _, f := range append([]field{tField}, r.fields...) {
			for _, b := range []struct {
				v  int64
				ok bool
			}{{f.lo, true}, {f.lo - 1, false}, {f.hi, true}, {f.hi + 1, false}} {
				e := cloneElems(r.reply)
				e[f.pos] = vInt(b.v)
				if f.pos == 1 && r.fix != nil {
					r.fix(e)
				}
				out = append(out, rangeCase{
					name:   fmt.Sprintf("%s: element %d = %d", r.name(), f.pos, b.v),
					script: r.script, reply: withElems(e), lim: r.limits(), ok: b.ok,
				})
			}
		}
	}
	return out
}

func TestDecodeRanges(t *testing.T) {
	cases := rangeCases()
	for _, c := range cases {
		_, err := Decode(c.script, c.reply, c.lim)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: %v, want ok", c.name, err)
		case !c.ok && err == nil:
			t.Errorf("%s decoded, want a range error", c.name)
		}
	}
	// The examples the plan names, spelled out.
	for _, c := range []struct {
		name   string
		script string
		reply  resp.Value
		expiry int64
	}{
		{"expiry_us = t_us + LifetimeUs + 1", "acquire", rep(1, tNow, "", 0, 1, 1, tNow+lifeUs+1), 0},
		{"expiry_us = t_us + LifetimeUs - 1", "acquire", rep(1, tNow, "", 0, 1, 1, tNow+lifeUs-1), 0},
		{"credit_us = ReceiptExpiryUs", "refund", rep(4, tNow, "", tNow-100), tNow - 100},
		{"t_us = TBOUND", "acquire", rep(2, tBnd, "", 0), 0},
		{"limited u = rate_period_ns", "acquire", rep(2, tNow, "", 1_000_000_000), 0},
		{"allowed u above cap_units - rate_period_ns", "acquire", rep(1, tNow, "", 19_000_000_001, 1, 1, tNow+lifeUs), 0},
		{"g = 0", "acquire", rep(1, tNow, "", 0, 0, 1, tNow+lifeUs), 0},
		{"markers_removed above max_markers_per_key", "cleanup_entry", rep(31, tNow, "", 1001), 0},
		{"oldest_due above T0", "due_entries", rep(0, tNow, "", tNow+1), 0},
		{"read_epoch gen = 2^53", "read_epoch", rep(0, 0, "", "id", "fp", tNow, max53+1, tNow, 1, 1, ""), 0},
	} {
		lim := testLimits()
		lim.ReceiptExpiryUs = c.expiry
		if _, err := Decode(c.script, c.reply, lim); err == nil {
			t.Errorf("%s decoded, want a range error", c.name)
		}
	}
	// admin_status shows a stored hwm as it is, even at or above TBOUND.
	if _, err := Decode("admin_status", rep(40, 0, "", "id", "fp", "", tBnd, 1, 1, 0), testLimits()); err != nil {
		t.Errorf("admin_status with hwm = TBOUND: %v, want ok", err)
	}
	if len(cases) < 400 {
		t.Errorf("only %d range cases", len(cases))
	}

	t.Run("through the adapter", func(t *testing.T) {
		e := newEnv(t)
		bad := rep(1, tNow, "", 0, 1, 1, tNow+lifeUs+1) // expiry_us one past t_us + LifetimeUs
		sc := e.script("acquire", func(_, _ []string) resp.Value { return bad })
		e.start()
		e.warm(8, 2)
		r := e.a.Call(t.Context(), acquireReq(sc, 1))
		if r.Outcome != OutcomeUnknown || !r.Sent || r.SentCommands != 1 {
			t.Errorf("Call = %v, Sent %v, SentCommands %d; want outcome_unknown, true, 1", r.Outcome, r.Sent, r.SentCommands)
		}
		if !strings.Contains(r.Raw, strconv.FormatInt(tNow+lifeUs+1, 10)) {
			t.Errorf("Raw = %q, want the raw reply", r.Raw)
		}
	})

	// Call and CallBatch check E_ALREADY_APPLIED's credit_us against the request's own
	// ReceiptExpiryUs (plan S3 step 8): before it → ok; equal to it → outcome_unknown.
	t.Run("refund through the adapter", func(t *testing.T) {
		e := newEnv(t)
		sc := e.script("refund", func(_, _ []string) resp.Value { return rep(4, tNow, "", tNow-10) })
		e.start()
		e.warm(8, 2)
		for _, c := range []struct {
			name   string
			expiry int64 // the request's ReceiptExpiryUs
			want   Outcome
		}{
			{"credit_us 10 µs before t_us, the receipt expiring 60 s after t_us", tNow + lifeUs, OK},
			{"credit_us = ReceiptExpiryUs", tNow - 10, OutcomeUnknown},
		} {
			req := Request{Kind: Refund, Script: sc, Args: []string{"1"}, ReceiptExpiryUs: c.expiry}
			rs := append([]Response{e.a.Call(t.Context(), req)}, e.a.CallBatch(t.Context(), []Request{req})...)
			for i, r := range rs {
				entry := []string{"Call", "CallBatch"}[i]
				switch {
				case r.Outcome != c.want || !r.Sent || r.SentCommands != 1:
					t.Errorf("%s, %s: %v, Sent %v, SentCommands %d; want %v, true, 1",
						c.name, entry, r.Outcome, r.Sent, r.SentCommands, c.want)
				case c.want == OK && r.Reply.Code != 4:
					t.Errorf("%s, %s: reply code %d, want 4 (E_ALREADY_APPLIED)", c.name, entry, r.Reply.Code)
				case c.want == OutcomeUnknown && !strings.Contains(r.Raw, strconv.FormatInt(tNow-10, 10)):
					t.Errorf("%s, %s: Raw = %q, want the raw reply", c.name, entry, r.Raw)
				}
			}
		}
	})
}

func TestLimitsFor(t *testing.T) {
	if got, want := LimitsFor(settings.DefaultPolicy()), testLimits(); got != want {
		t.Errorf("LimitsFor(DefaultPolicy()) = %+v, want %+v", got, want)
	}
}

func FuzzDecode(f *testing.F) {
	for _, r := range layoutRows() {
		f.Add(r.script, resp.AppendValue(nil, r.reply))
		long := append(cloneElems(r.reply), vInt(0))
		f.Add(r.script, resp.AppendValue(nil, withElems(long)))
		f.Add(r.script, resp.AppendValue(nil, withElems(cloneElems(r.reply)[:len(r.reply.Elems)-1])))
		kind := cloneElems(r.reply)
		kind[1] = resp.Value{Kind: resp.Bulk, Str: "0"}
		f.Add(r.script, resp.AppendValue(nil, withElems(kind)))
	}
	for _, c := range rangeCases() {
		if !c.ok {
			f.Add(c.script, resp.AppendValue(nil, c.reply))
		}
	}
	f.Add("acquire", []byte("-OOM command not allowed\r\n"))
	f.Add("acquire", []byte(":1\r\n"))
	lim := testLimits()
	lim.ReceiptExpiryUs = tNow + lifeUs
	f.Fuzz(func(t *testing.T, script string, raw []byte) {
		v, err := resp.NewReader(bytes.NewReader(raw)).ReadValue()
		if err != nil {
			return
		}
		got, err := Decode(script, v, lim)
		var de *DecodeError
		switch {
		case err != nil && !errors.As(err, &de):
			t.Fatalf("Decode error %v is not a *DecodeError", err)
		case err == nil:
			if v.Kind != resp.Array || len(v.Elems) < 3 || got.Code != v.Elems[0].Int || got.TUs != v.Elems[1].Int || got.Reason != v.Elems[2].Str {
				t.Fatalf("Decode(%s) = {%d %d %q}, not the reply's first three elements", v, got.Code, got.TUs, got.Reason)
			}
			if got.TUs >= tBnd {
				t.Fatalf("Decode(%s) passed t_us %d ≥ TBOUND", v, got.TUs)
			}
			for _, e := range v.Elems {
				if e.Kind == resp.Int && (e.Int < 0 || e.Int > max53) {
					t.Fatalf("Decode(%s) passed integer %d", v, e.Int)
				}
			}
		}
		// Through the adapter's settling: a typed reply, or outcome_unknown with the raw
		// reply; an error reply is classified instead.
		r := (&StoreAdapter{}).settle(t.Context(), time.Time{}, script, exchanged{sent: true, v: v}, lim)
		switch {
		case v.Kind == resp.Error:
		case err == nil && r.Outcome != OK:
			t.Fatalf("settle(%s) = %v after a clean Decode", v, r.Outcome)
		case err != nil && (r.Outcome != OutcomeUnknown || r.Raw == ""):
			t.Fatalf("settle(%s) = %v with Raw %q, want outcome_unknown with the raw reply", v, r.Outcome, r.Raw)
		}
	})
}
