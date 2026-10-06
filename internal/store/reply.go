package store

import (
	"fmt"

	"ratelimiter/internal/settings"
	"ratelimiter/internal/store/resp"
)

const (
	// maxUint53 bounds every integer in a script reply: 0 … 2^53 − 1 (§A4.9).
	maxUint53 = 1<<53 - 1
	// tBound is TBOUND = 2^53 − 2^43 (§A3.1): every budget time is below it.
	tBound = 1<<53 - 1<<43
)

// Reply is a decoded script reply (DESIGN §A4.9). Code, TUs (t_us) and Reason are the
// first three elements; Elems holds every element, Elems[0] being the code, so a value is
// read by its 0-based position in the §A4.9 layout: Elems[3] is an allowed acquire's u.
type Reply struct {
	Code, TUs int64
	Reason    string
	Elems     []resp.Value
}

// Limits are the values the §A4.9 range checks need (plan choice): the instance's policy
// values, and ReceiptExpiryUs, which a refund request supplies for its receipt.
type Limits struct {
	CapUnits, RatePeriodNs, LifetimeUs, MaxMarkersPerKey, ReceiptExpiryUs int64
}

// LimitsFor returns the limits of a validated policy p (plan choice): cap_units =
// capacity × rate_period_ns (below 2^52 once validated, §A3.1) and lifetime_us =
// receipt_lifetime_ns / 1000. ReceiptExpiryUs stays 0: each refund request sets its own.
func LimitsFor(p settings.Policy) Limits {
	return Limits{
		CapUnits:         p.Capacity * p.RatePeriodNs,
		RatePeriodNs:     p.RatePeriodNs,
		LifetimeUs:       p.ReceiptLifetimeNs / 1000,
		MaxMarkersPerKey: p.MaxMarkersPerKey,
	}
}

// DecodeError says why a reply failed its layout or range check (§A4.9). The adapter turns
// it into outcome_unknown with the raw reply in Response.Raw.
type DecodeError struct {
	Script string
	Reason string
}

func (e *DecodeError) Error() string { return "store: " + e.Script + " reply: " + e.Reason }

// layout is one (script, code) row of §A4.9. kinds has one letter per position: I an
// integer, S a bulk string, E an integer or the empty bulk string. zeroT says the layout
// shows 0 for t_us. rest is the kind of each position past kinds when the length is not
// exact ('S' for due_entries members, 'E' for admin_inspect expiries); countPos, when not 0,
// is the position of the integer that counts them (admin_inspect's n).
type layout struct {
	kinds    string
	zeroT    bool
	rest     byte
	countPos int
}

// layoutOf returns the layout of script's reply code (IMPLEMENTATION_PLAN S3 step 7).
func layoutOf(script string, code int64) (layout, bool) {
	refusal := layout{kinds: "IIS", zeroT: true}    // {c, 0, reason}
	keyResult := layout{kinds: "IIS"}               // {c, t, reason}
	oneValue := layout{kinds: "IISI"}               // {c, t, '' or reason, v}
	zeroValue := layout{kinds: "IISI", zeroT: true} // {c, 0, '', v}
	switch script {
	case "acquire":
		switch code {
		case 1:
			return layout{kinds: "IISIIII"}, true // {1, t, '', u, g, s, expiry_us}
		case 2:
			return oneValue, true // {2, t, '', u}
		case 20, 21, 22, 25:
			return refusal, true
		case 23, 24:
			return keyResult, true
		}
	case "refund":
		switch code {
		case 3, 4:
			return oneValue, true // {3, t, '', credited_units}, {4, t, '', credit_us}
		case 10, 11, 23, 24:
			return keyResult, true
		case 20, 21, 22, 25:
			return refusal, true
		}
	case "due_entries":
		switch code {
		case 0:
			return layout{kinds: "IISI", rest: 'S'}, true // {0, T0, '', oldest_due_or_0, member…}
		case 20, 21, 22:
			return refusal, true
		}
	case "cleanup_entry":
		switch code {
		case 30, 31, 32, 33:
			return oneValue, true // {c, t, reason, markers_removed}
		case 20, 21, 22, 25:
			return refusal, true
		}
	case "reclaim":
		if code == 40 {
			return zeroValue, true // {40, 0, '', unlinked_count}
		}
	case "read_epoch":
		if code == 0 || code == 22 {
			// {state, 0, reason, id, fp, hwm, gen, clock_us, nb, nm, quarantined_or_''}
			return layout{kinds: "IISSSIIIIIE", zeroT: true}, true
		}
	case "hold":
		switch code {
		case 40:
			return zeroValue, true // {40, 0, '', 0}
		case 41:
			return refusal, true
		}
	case "admin_start_epoch":
		switch code {
		case 40:
			return layout{kinds: "IISS"}, true // {40, t, '', new_id}
		case 41, 21:
			return refusal, true
		}
	case "admin_restore", "admin_clear":
		switch code {
		case 40:
			return oneValue, true // {40, t, '', g} / {40, t, '', 0}
		case 41, 21, 22, 25:
			return refusal, true
		}
	case "admin_status":
		switch code {
		case 40:
			// {40, 0, '', id, fp, hold, hwm_or_'', nb, nm, quarantined_count}
			return layout{kinds: "IISSSSEIII", zeroT: true}, true
		case 41:
			return refusal, true
		}
	case "admin_inspect":
		switch code {
		case 40:
			// {40, 0, '', present, u, t, g, s, x, mc, ix_score (each or ''), quarantine_or_'',
			// n, expiry_1 … expiry_n}
			return layout{kinds: "IISIEEEEEEESI", zeroT: true, rest: 'E', countPos: 12}, true
		case 41, 25:
			return refusal, true
		}
	}
	return layout{}, false
}

// bound is one range check: the integer at pos lies in lo … hi.
type bound struct {
	pos    int
	name   string
	lo, hi int64
}

// bounds returns the §A4.9 range checks of script's reply code beyond the rules every
// integer and t_us obey; t is the reply's t_us.
func bounds(script string, code, t int64, lim Limits) []bound {
	switch {
	case script == "acquire" && code == 1: // E_ALLOWED
		return []bound{
			{3, "u", 0, lim.CapUnits - lim.RatePeriodNs},
			{4, "g", 1, maxUint53},
			{5, "s", 1, maxUint53},
			{6, "expiry_us", t + lim.LifetimeUs, t + lim.LifetimeUs},
		}
	case script == "acquire" && code == 2: // E_LIMITED
		return []bound{{3, "u", 0, lim.RatePeriodNs - 1}}
	case script == "refund" && code == 3: // E_APPLIED
		return []bound{{3, "credited_units", 0, lim.RatePeriodNs}}
	case script == "refund" && code == 4: // E_ALREADY_APPLIED
		return []bound{
			{3, "credit_us", 0, t},
			{3, "credit_us", 0, lim.ReceiptExpiryUs - 1},
		}
	case script == "cleanup_entry" && code >= 30 && code <= 33:
		return []bound{{3, "markers_removed", 0, lim.MaxMarkersPerKey}}
	case script == "due_entries" && code == 0:
		return []bound{{3, "oldest_due", 0, t}}
	}
	return nil
}

// Decode checks reply against script's §A4.9 layout for its code (exact length, or the
// due_entries minimum, or admin_inspect's 13 + n; each element's kind) and then every range
// check, computed from lim: every integer in 0 … 2^53 − 1; t_us 0 where the layout shows 0,
// else below TBOUND; and the per-code bounds. Any failure is a *DecodeError, which callers
// treat as outcome_unknown. It never panics, whatever the reply.
func Decode(script string, reply resp.Value, lim Limits) (Reply, error) {
	fail := func(format string, args ...any) (Reply, error) {
		return Reply{}, &DecodeError{Script: script, Reason: fmt.Sprintf(format, args...)}
	}
	if reply.Kind != resp.Array || reply.Null {
		return fail("reply is %s, want an array", describe(reply))
	}
	e := reply.Elems
	if len(e) == 0 || e[0].Kind != resp.Int {
		return fail("no integer code first")
	}
	code := e[0].Int
	l, ok := layoutOf(script, code)
	if !ok {
		return fail("unknown code %d", code)
	}
	if msg := l.check(e); msg != "" {
		return fail("code %d: %s", code, msg)
	}
	for i, v := range e {
		if v.Kind == resp.Int && (v.Int < 0 || v.Int > maxUint53) {
			return fail("code %d: element %d = %d, outside 0 … 2^53 − 1", code, i, v.Int)
		}
	}
	t := e[1].Int
	if l.zeroT && t != 0 {
		return fail("code %d: t_us = %d, want 0", code, t)
	}
	if !l.zeroT && t >= tBound {
		return fail("code %d: t_us = %d, want below TBOUND", code, t)
	}
	for _, b := range bounds(script, code, t, lim) {
		if b.pos >= len(e) || e[b.pos].Kind != resp.Int { // the layout guarantees it; fail closed
			return fail("code %d: no integer %s at position %d", code, b.name, b.pos)
		}
		if v := e[b.pos].Int; v < b.lo || v > b.hi {
			return fail("code %d: %s = %d, want %d … %d", code, b.name, v, b.lo, b.hi)
		}
	}
	return Reply{Code: code, TUs: t, Reason: e[2].Str, Elems: e}, nil
}

// check tests e's length and element kinds against l; it returns "" when they match.
func (l layout) check(e []resp.Value) string {
	n := len(l.kinds)
	switch {
	case len(e) < n:
		return fmt.Sprintf("%d elements, want %d", len(e), n)
	case l.countPos > 0:
		c := e[l.countPos]
		if c.Kind != resp.Int || c.Int < 0 || int64(len(e)-n) != c.Int {
			return fmt.Sprintf("%d elements, want %d + n with n at position %d", len(e), n, l.countPos)
		}
	case l.rest == 0 && len(e) != n:
		return fmt.Sprintf("%d elements, want %d", len(e), n)
	}
	for i := range l.kinds {
		if !kindMatches(l.kinds[i], e[i]) {
			return fmt.Sprintf("element %d is %s, want %c", i, describe(e[i]), l.kinds[i])
		}
	}
	for i, v := range e[n:] {
		if !kindMatches(l.rest, v) {
			return fmt.Sprintf("element %d is %s, want %c", n+i, describe(v), l.rest)
		}
	}
	return ""
}

// kindMatches reports whether v has the layout kind k (I, S or E).
func kindMatches(k byte, v resp.Value) bool {
	isInt := v.Kind == resp.Int
	isStr := v.Kind == resp.Bulk && !v.Null
	switch k {
	case 'I':
		return isInt
	case 'S':
		return isStr
	case 'E':
		return isInt || (isStr && v.Str == "")
	}
	return false
}

// describe names a value's kind for error messages, without its content.
func describe(v resp.Value) string {
	if v.Null {
		return "null " + v.Kind.String()
	}
	return v.Kind.String()
}
