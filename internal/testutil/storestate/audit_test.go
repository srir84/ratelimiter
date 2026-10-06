package storestate_test

import (
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/testutil/storestate"
)

// The fixture's key names, written out (DESIGN §A3.2): the current namespace nsID with the
// budget keys "a" (enc YQ, one marker) and "b" (enc Yg, no marker), and an old namespace.
const (
	nsID  = "0123456789abcdef0123456789abcdef"
	oldID = "fedcba9876543210fedcba9876543210"
	epK   = "rl:epoch"
	ctlK  = "rl:ctl:time"
	ixK   = "rl:e:" + nsID + ":ix"
	qK    = "rl:e:" + nsID + ":q"
	kA    = "rl:e:" + nsID + ":k:YQ"
	miA   = "rl:e:" + nsID + ":mi:YQ"
	kB    = "rl:e:" + nsID + ":k:Yg"
	oldK  = "rl:e:" + oldID + ":k:YQ"
	// tbound is TBOUND = 2^53 − 2^43 = 9007199254740992 − 8796093022208 (§A3.1).
	tbound = "8998403161718784"
)

// auditPolicy has 10 tokens per 1 s and capacity 20: cap_units = 20 × 10^9 = 2×10^10, and
// refill adds rate_tokens × 1000 = 10^4 units per µs.
func auditPolicy() settings.Policy {
	p := settings.DefaultPolicy()
	p.RateTokens, p.RatePeriodNs, p.Capacity = 10, 1_000_000_000, 20
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 100, 1000
	return p
}

func hash(kv ...string) storestate.Value {
	return storestate.Value{Type: storestate.Hash, Fields: fields(kv)}
}

func zset(kv ...string) storestate.Value {
	return storestate.Value{Type: storestate.ZSet, Fields: fields(kv)}
}

func str(s string) storestate.Value { return storestate.Value{Type: storestate.String, Str: s} }

func fields(kv []string) map[string]string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// cleanState is a state every rule accepts, its numbers worked out by hand:
//   - hwm 1760000000500000, gen 5, nb 2 (two k: keys), nm 1 (one marker, mc 1 + 0).
//   - "a": u 1.5×10^10, t 1760000000450000, x 1760000060400000, marker m:2:2 expiring at
//     1760000060300000, credited at 1760000000450000. Eligibility = max(t + ⌈5×10^9 /
//     10^4⌉, x) = max(1760000000950000, x) = x; ix score = min(x, 1760000060300000) =
//     1760000060300000, the bound.
//   - "b": u 1.9×10^10, t = hwm, x = t + 60 s; eligibility = max(t + 10^5, x) = x =
//     1760000060500000 = its ix score, the bound.
func cleanState(fp string) storestate.State {
	return storestate.State{
		epK: hash("id", nsID, "fp", fp, "schema", "1", "clock", "controlled", "hold", "",
			"hwm", "1760000000500000", "origin", "1760000000000000", "gen", "5", "nb", "2", "nm", "1",
			"created", "1760000000000000"),
		ctlK: str("1760000000500000"),
		kA: hash("u", "15000000000", "t", "1760000000450000", "g", "2", "s", "3", "x", "1760000060400000",
			"mc", "1", "m:2:2", "1760000060300000:1760000000450000"),
		miA: zset("2:2", "1760000060300000"),
		kB: hash("u", "19000000000", "t", "1760000000500000", "g", "5", "s", "1", "x", "1760000060500000",
			"mc", "0"),
		ixK: zset("YQ", "1760000060300000", "Yg", "1760000060500000"),
	}
}

// set sets fields of st[key], which must be a hash or a sorted set.
func set(st storestate.State, key string, kv ...string) {
	for i := 0; i+1 < len(kv); i += 2 {
		st[key].Fields[kv[i]] = kv[i+1]
	}
}

// unset deletes fields of st[key].
func unset(st storestate.State, key string, names ...string) {
	for _, n := range names {
		delete(st[key].Fields, n)
	}
}

// line is a report line: the quoted key name, then what was found.
func line(key, rest string) string { return strconv.Quote(key) + ": " + rest }

// TestAuditDetectsEachViolation (plan S4, L1): each fixture breaks one rule of
// storestate.Audit and gets exactly the lines that rule (and the rules it necessarily
// breaks too) report; the passing fixtures get none. Expected lines are written out from
// DESIGN §A3.1–§A3.3, §C3 and the numbers in cleanState.
func TestAuditDetectsEachViolation(t *testing.T) {
	type edit = func(storestate.State, *storestate.AuditConfig)
	type fixture struct {
		name string
		edit edit
		want []string
	}
	p := auditPolicy()
	fp := fingerprint.Of(p)
	p21 := auditPolicy()
	p21.Capacity = 21
	noRate := auditPolicy()
	noRate.RateTokens = 0
	bigCap := auditPolicy()
	bigCap.Capacity = 4_503_600 // × 10^9 = 4503600000000000 ≥ 2^52 = 4503599627370496
	maxCap := auditPolicy()
	maxCap.Capacity = 4_503_599 // × 10^9 = 4503599000000000 < 2^52 (plan S4 TestAcquireDecisions)
	// The limits Audit checks: rate_tokens 1 … 2^31 − 1 and cap_units < 2^52 (§A3.1), and
	// capacity and rate_period_ns at least 1.
	minRate := auditPolicy()
	minRate.RateTokens = 1
	maxRate := auditPolicy()
	maxRate.RateTokens = 2_147_483_647 // 2^31 − 1
	overRate := auditPolicy()
	overRate.RateTokens = 2_147_483_648 // 2^31
	noCap := auditPolicy()
	noCap.Capacity = 0
	noPeriod := auditPolicy()
	noPeriod.RatePeriodNs = 0
	wideCap := auditPolicy()
	wideCap.Capacity, wideCap.RatePeriodNs = 1<<40, 1<<30 // cap_units 2^70 does not fit in 64 bits

	// The canary's encodings differ from one another, so each fixture below exercises one.
	canary := append([]byte{0xfb, 0xff, 0xbf}, "audit-canary-0123456789abcdef"...)
	canary1 := []byte("audit-canary-1-fedcba9876543210!") // a second 32-byte canary
	hexLow := hex.EncodeToString(canary)
	hexUp := strings.ToUpper(hexLow)
	b64 := base64.StdEncoding.EncodeToString(canary)
	b64raw := base64.RawStdEncoding.EncodeToString(canary)
	b64url := base64.URLEncoding.EncodeToString(canary)
	b64urlRaw := base64.RawURLEncoding.EncodeToString(canary)
	if len(canary) != 32 || hexLow == hexUp || b64 == b64raw || b64 == b64url || b64url == b64urlRaw ||
		!strings.ContainsAny(b64, "+/") || !strings.ContainsAny(b64url, "-_") {
		t.Fatalf("canary %x does not separate its encodings", canary)
	}
	withCanary := func(c *storestate.AuditConfig) { c.Canaries = [][]byte{canary} }
	inValue := func(form string) func(storestate.State, *storestate.AuditConfig) {
		return func(st storestate.State, c *storestate.AuditConfig) {
			withCanary(c)
			st["rl:fill:c"] = str("<" + form + ">")
		}
	}
	// embedded puts the canary inside a longer base64 value, after pre and before post:
	// with 1 or 2 bytes before it, or bytes after it, the value holds none of the 7 forms
	// above, only the part of the canary's encoding that does not depend on its
	// neighbours. The canary's first bytes 0xfb 0xff 0xbf put '/' in that part in the
	// standard alphabet and '_' in the URL one, at every offset, so the two never match
	// each other's values.
	embedded := func(enc *base64.Encoding, pre, post []byte) edit {
		return inValue(enc.EncodeToString(slices.Concat(pre, canary, post)))
	}
	pre1, pre2, post := []byte{0x01}, []byte{0x01, 0x02}, []byte{0xff, 0xee}
	setFP := func(q settings.Policy) func(storestate.State, *storestate.AuditConfig) {
		return func(st storestate.State, c *storestate.AuditConfig) {
			c.Policy = q
			set(st, epK, "fp", fingerprint.Of(q))
		}
	}
	malformedRecord := func(field, value string) func(storestate.State, *storestate.AuditConfig) {
		return func(st storestate.State, _ *storestate.AuditConfig) { set(st, epK, field, value) }
	}
	missingRecordField := func(field string) func(storestate.State, *storestate.AuditConfig) {
		return func(st storestate.State, _ *storestate.AuditConfig) { unset(st, epK, field) }
	}
	ixScore := func(score string) func(storestate.State, *storestate.AuditConfig) {
		return func(st storestate.State, _ *storestate.AuditConfig) { set(st, ixK, "Yg", score) }
	}
	// refillBound makes "b" empty but for 1 unit, with every receipt expired (x = t): its
	// eligibility is t + ⌈19999999999 / 10^4⌉ = 1760000000500000 + 2000000 = 1760000002500000.
	refillBound := func(score string) func(storestate.State, *storestate.AuditConfig) {
		return func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, kB, "u", "1", "x", "1760000000500000")
			set(st, ixK, "Yg", score)
		}
	}
	// halfFull leaves "b" half full (u = 10^10) with every receipt expired (x = t): its
	// eligibility is t + ⌈(2×10^10 − 10^10) / 10^4⌉ = 1760000000500000 + 1000000 =
	// 1760000001500000, where cap_units alone would give t + 2000000.
	halfFull := func(score string) edit {
		return func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, kB, "u", "10000000000", "x", "1760000000500000")
			set(st, ixK, "Yg", score)
		}
	}
	// threeMarkers gives "a" three markers (mc 3, nm 3), each in mi:YQ with its expiry:
	// m:2:1 expiring at 1760000060350000, m:2:2 at 1760000060300000 (the earliest, and
	// still ix YQ's score) and m:2:3 at x = 1760000060400000 with credit time hwm =
	// 1760000000500000. Eligibility stays x, so the ix bound is the earliest expiry,
	// 1760000060300000, which is neither the first marker's nor the last's.
	threeMarkers := func(st storestate.State) {
		set(st, kA, "mc", "3",
			"m:2:1", "1760000060350000:1760000000450000",
			"m:2:2", "1760000060300000:1760000000450000",
			"m:2:3", "1760000060400000:1760000000500000")
		st[miA] = zset("2:1", "1760000060350000", "2:2", "1760000060300000", "2:3", "1760000060400000")
		set(st, epK, "nm", "3")
	}
	nmLine := func(nm, mc, mi string) string {
		return line(epK, "nm "+nm+", but the sum of mc is "+mc+" and the sum of ZCARD mi:<enc> is "+mi)
	}
	nbLine := func(nb, lo, hi string) string {
		return line(epK, "nb "+nb+", want "+lo+" … "+hi+" (the k:<enc> keys, plus Restored)")
	}

	cases := []fixture{
		// Passing fixtures.
		{"clean", nil, nil},
		{"declared-damaged bucket with its q entry", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{kA}
			st[kA] = str("broken")
			st[qK] = hash("YQ", "malformed_bucket|1760000000500000")
		}, nil},
		{"declared-damaged marker index", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{miA}
			st[miA] = str("broken")
		}, nil},
		{"declared-damaged ix leaves the namespace at grammar", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{ixK}
			st[ixK] = str("broken")
			set(st, kA, "u", "20000000001")
		}, nil},
		{"declared-damaged q", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{qK}
			st[qK] = str("broken")
		}, nil},
		{"declared-damaged epoch record", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{epK}
			st[epK] = str("broken")
			set(st, kB, "u", "x")
		}, nil},
		{"no epoch record", func(st storestate.State, _ *storestate.AuditConfig) {
			delete(st, epK)
			set(st, kB, "u", "x")
		}, nil},
		{"malformed keys in an old namespace", func(st storestate.State, _ *storestate.AuditConfig) {
			st[oldK] = hash("u", "x")
			st["rl:e:"+oldID+":mi:YQ"] = zset("junk", "1.5")
			st["rl:e:"+oldID+":ix"] = zset("!!", "inf")
			st["rl:e:"+oldID+":q"] = hash("YQ", "x")
		}, nil},
		{"nb = k: keys + 1 with one Restored key", func(st storestate.State, c *storestate.AuditConfig) {
			c.Restored = []string{kB}
			set(st, epK, "nb", "3")
		}, nil},
		{"Restored key's ix score above its bound", func(st storestate.State, c *storestate.AuditConfig) {
			c.Restored = []string{kB}
			set(st, ixK, "Yg", "1760000060500001")
		}, nil},
		{"filler keys", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:fill:0001"] = str("x")
			st["rl:pad:0001"] = hash("a", "b")
		}, nil},
		{"u = cap_units", func(st storestate.State, _ *storestate.AuditConfig) { set(st, kB, "u", "20000000000") }, nil},
		// g = 1, the lowest generation acquire issues, is in range: the upper edge of row 4's g < 1.
		{"g = 1", func(st storestate.State, _ *storestate.AuditConfig) { set(st, kB, "g", "1") }, nil},
		{"hwm = TBOUND - 1", malformedRecord("hwm", "8998403161718783"), nil},
		{"hold set", malformedRecord("hold", "store_rollback"), nil},
		{"ix score at the refill eligibility", refillBound("1760000002500000"), nil},
		{"scores compared as numbers", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, ixK, "Yg", "1.7600000605e15", "YQ", "1760000060300000.0")
			set(st, miA, "2:2", "1.7600000603e+15")
		}, nil},
		{"policy at the cap_units bound", setFP(maxCap), nil},
		{"canary absent", func(_ storestate.State, c *storestate.AuditConfig) { withCanary(c) }, nil},
		{"unattributable ix member declared by its placeholder", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{"rl:e:" + nsID + ":k:!"}
			set(st, ixK, "!", "1760000000000000")
		}, nil},
		{"controlled time is not validated", func(st storestate.State, _ *storestate.AuditConfig) { st[ctlK] = str("-5") }, nil},
		{"marker expiry = x", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, kA, "m:2:2", "1760000060400000:1760000000450000")
			set(st, miA, "2:2", "1760000060400000")
		}, nil},
		{"marker credit time = hwm", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, kA, "m:2:2", "1760000060300000:1760000000500000")
		}, nil},
		{"three markers, one expiring at x and one credited at hwm", func(st storestate.State, _ *storestate.AuditConfig) {
			threeMarkers(st)
		}, nil},
		{"ix score at a half-full bucket's eligibility", halfFull("1760000001500000"), nil},
		{"declared-damaged epoch record that is still a hash", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{epK}
			set(st, epK, "schema", "2")
			set(st, kB, "u", "x")
		}, nil},
		{"declared-damaged ix that is still a sorted set", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{ixK}
			set(st, ixK, "!!", "inf", "Yg", "inf")
		}, nil},
		{"declared-damaged q that is still a hash", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{qK}
			st[qK] = hash("YQ", "x|1")
		}, nil},
		{"store clock", func(st storestate.State, _ *storestate.AuditConfig) { set(st, epK, "clock", "store") }, nil},
		{"policy: rate_tokens 1", setFP(minRate), nil},
		{"policy: rate_tokens 2^31 - 1", setFP(maxRate), nil},

		// Grammar.
		{"stray key", func(st storestate.State, _ *storestate.AuditConfig) { st["rl:junk"] = str("x") },
			[]string{line("rl:junk", "not a key of the §A3.2 layout")}},
		{"stray: bucket key with a bad enc", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:e:"+nsID+":k:!"] = hash("u", "1")
		}, []string{line("rl:e:"+nsID+":k:!", "not a key of the §A3.2 layout")}},
		{"stray: upper-case namespace", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:e:0123456789ABCDEF0123456789ABCDEF:ix"] = zset("YQ", "1")
		}, []string{line("rl:e:0123456789ABCDEF0123456789ABCDEF:ix", "not a key of the §A3.2 layout")}},
		{"stray: unknown suffix", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:e:"+nsID+":junk"] = str("x")
		}, []string{line("rl:e:"+nsID+":junk", "not a key of the §A3.2 layout")}},
		{"stray: marker index with a bad enc", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:e:"+nsID+":mi:!"] = zset("2:2", "1760000060300000")
		}, []string{line("rl:e:"+nsID+":mi:!", "not a key of the §A3.2 layout")}},
		{"stray: marker index with a bad enc in an old namespace", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:e:"+oldID+":mi:!"] = zset("2:2", "1760000060300000")
		}, []string{line("rl:e:"+oldID+":mi:!", "not a key of the §A3.2 layout")}},
		{"stray: suffix ixx", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:e:"+nsID+":ixx"] = zset("YQ", "1760000060300000")
		}, []string{line("rl:e:"+nsID+":ixx", "not a key of the §A3.2 layout")}},
		{"stray: suffix qq", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:e:"+nsID+":qq"] = hash("YQ", "x|1")
		}, []string{line("rl:e:"+nsID+":qq", "not a key of the §A3.2 layout")}},
		{"stray: no colon after the namespace id", func(st storestate.State, _ *storestate.AuditConfig) {
			st["rl:e:"+nsID+"-ix"] = zset("YQ", "1")
		}, []string{line("rl:e:"+nsID+"-ix", "not a key of the §A3.2 layout")}},
		{"stray: Damaged does not excuse a bad name", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{"rl:junk"}
			st["rl:junk"] = str("x")
		}, []string{line("rl:junk", "not a key of the §A3.2 layout")}},
		{"wrong type: ix", func(st storestate.State, _ *storestate.AuditConfig) { st[ixK] = str("x") },
			[]string{line(ixK, "type string, want zset")}},
		{"wrong type: q", func(st storestate.State, _ *storestate.AuditConfig) { st[qK] = str("x") },
			[]string{line(qK, "type string, want hash")}},
		{"wrong type: controlled time", func(st storestate.State, _ *storestate.AuditConfig) { st[ctlK] = hash("v", "1") },
			[]string{line(ctlK, "type hash, want string")}},
		{"wrong type: epoch record", func(st storestate.State, _ *storestate.AuditConfig) { st[epK] = str("x") },
			[]string{line(epK, "type string, want hash")}},
		{"wrong type: bucket", func(st storestate.State, _ *storestate.AuditConfig) { st[kB] = str("x") },
			[]string{line(kB, "type string, want hash"), line(kB, "check_key malformed_bucket: k:<enc> is a string")}},
		{"wrong type: marker index", func(st storestate.State, _ *storestate.AuditConfig) { st[miA] = str("x") },
			[]string{
				line(miA, "type string, want zset"),
				line(kA, "check_key marker_index_mismatch: mi:<enc> is a string"),
				nmLine("1", "1", "0"),
			}},
		{"wrong type in an old namespace", func(st storestate.State, _ *storestate.AuditConfig) { st[oldK] = str("x") },
			[]string{line(oldK, "type string, want hash")}},

		// The epoch record (record_wellformed, hwm) and the audit policy.
		{"record: id upper case", malformedRecord("id", "0123456789ABCDEF0123456789ABCDEF"),
			[]string{line(epK, `epoch_record_malformed: id = "0123456789ABCDEF0123456789ABCDEF"`)}},
		{"record: id with a non-hex letter", malformedRecord("id", "0123456789abcdeg0123456789abcdef"),
			[]string{line(epK, `epoch_record_malformed: id = "0123456789abcdeg0123456789abcdef"`)}},
		{"record: fp", malformedRecord("fp", "xyz"), []string{line(epK, `epoch_record_malformed: fp = "xyz"`)}},
		// is_hex32 takes exactly 32 lowercase hex characters (§A4.2); fp+"0" has 33.
		{"record: fp of 33 hex characters", malformedRecord("fp", fp+"0"),
			[]string{line(epK, `epoch_record_malformed: fp = "`+fp+`0"`)}},
		{"record: fp of 31 hex characters", malformedRecord("fp", fp[:31]),
			[]string{line(epK, `epoch_record_malformed: fp = "`+fp[:31]+`"`)}},
		{"record: schema", malformedRecord("schema", "2"), []string{line(epK, `epoch_record_malformed: schema = "2"`)}},
		{"record: clock", malformedRecord("clock", "wall"), []string{line(epK, `epoch_record_malformed: clock = "wall"`)}},
		{"record: gen", malformedRecord("gen", "01"), []string{line(epK, `epoch_record_malformed: gen = "01"`)}},
		{"record: nb", malformedRecord("nb", "-1"), []string{line(epK, `epoch_record_malformed: nb = "-1"`)}},
		{"record: nm", malformedRecord("nm", "1.5"), []string{line(epK, `epoch_record_malformed: nm = "1.5"`)}},
		{"record: origin", malformedRecord("origin", ""), []string{line(epK, `epoch_record_malformed: origin = ""`)}},
		{"record: created missing", missingRecordField("created"), []string{line(epK, "epoch_record_malformed: created missing")}},
		{"record: hold missing", missingRecordField("hold"), []string{line(epK, "epoch_record_malformed: hold missing")}},
		{"record: hwm missing", missingRecordField("hwm"), []string{line(epK, "hwm_missing")}},
		{"record: hwm not a number", malformedRecord("hwm", "x"), []string{line(epK, `hwm_malformed: hwm = "x"`)}},
		{"record: hwm = TBOUND", malformedRecord("hwm", tbound),
			[]string{line(epK, "hwm_malformed: hwm "+tbound+" ≥ TBOUND "+tbound)}},
		{"record: fp of another policy", func(_ storestate.State, c *storestate.AuditConfig) { c.Policy = p21 },
			[]string{line(epK, "fp "+fp+" is not the audit policy's fingerprint "+fingerprint.Of(p21))}},
		{"policy: rate_tokens 0", setFP(noRate),
			[]string{"audit policy: limits outside §A3.1: rate_tokens 0, capacity 20, rate_period_ns 1000000000"}},
		{"policy: cap_units = 2^52 or more", setFP(bigCap),
			[]string{"audit policy: limits outside §A3.1: rate_tokens 10, capacity 4503600, rate_period_ns 1000000000"}},
		{"policy: rate_tokens 2^31", setFP(overRate),
			[]string{"audit policy: limits outside §A3.1: rate_tokens 2147483648, capacity 20, rate_period_ns 1000000000"}},
		{"policy: capacity 0", setFP(noCap),
			[]string{"audit policy: limits outside §A3.1: rate_tokens 10, capacity 0, rate_period_ns 1000000000"}},
		{"policy: rate_period_ns 0", setFP(noPeriod),
			[]string{"audit policy: limits outside §A3.1: rate_tokens 10, capacity 20, rate_period_ns 0"}},
		{"policy: cap_units past 64 bits", setFP(wideCap),
			[]string{"audit policy: limits outside §A3.1: rate_tokens 10, capacity 1099511627776, rate_period_ns 1073741824"}},

		// check_key rows (§A3.3), top to bottom.
		{"row 2: marker index without bucket", func(st storestate.State, _ *storestate.AuditConfig) { delete(st, kA) },
			[]string{
				line(kA, "check_key marker_index_without_bucket: mi:<enc> exists without k:<enc>"),
				nmLine("1", "0", "1"),
				line(ixK, "ZCARD 2, but 1 k:<enc> keys"),
				nbLine("2", "1", "1"),
			}},
		{"row 3: index without bucket", func(st storestate.State, _ *storestate.AuditConfig) {
			delete(st, kA)
			delete(st, miA)
		}, []string{
			line(kA, "check_key index_without_bucket: an ix member exists without k:<enc>"),
			nmLine("1", "0", "0"),
			line(ixK, "ZCARD 2, but 1 k:<enc> keys"),
			nbLine("2", "1", "1"),
		}},
		{"row 4: a bucket field missing", func(st storestate.State, _ *storestate.AuditConfig) { unset(st, kB, "s") },
			[]string{line(kB, "check_key malformed_bucket: 5 of the 6 bucket fields")}},
		{"row 4: an unknown field", func(st storestate.State, _ *storestate.AuditConfig) { set(st, kB, "zz", "1") },
			[]string{line(kB, `check_key malformed_bucket: field "zz" is neither a bucket field nor a marker`)}},
		{"row 4: u above cap_units", func(st storestate.State, _ *storestate.AuditConfig) { set(st, kB, "u", "20000000001") },
			[]string{line(kB, "check_key malformed_bucket: u 20000000001 > cap_units 20000000000")}},
		// g < 1 is out of range too (owner decision at S4's §5 stop, added to §A3.3 row 4).
		{"row 4: g below 1", func(st storestate.State, _ *storestate.AuditConfig) { set(st, kB, "g", "0") },
			[]string{line(kB, "check_key malformed_bucket: g 0 < 1")}},
		{"row 5: markers but no bucket fields", func(st storestate.State, _ *storestate.AuditConfig) {
			st[kB] = hash("m:5:1", "1760000060500000:1760000000500000")
		}, []string{line(kB, "check_key marker_without_bucket: 1 marker fields, no bucket field")}},
		{"row 6: marker field grammar", func(st storestate.State, _ *storestate.AuditConfig) {
			unset(st, kA, "m:2:2")
			set(st, kA, "m:2:x", "1760000060300000:1760000000450000")
		}, []string{line(kA, `check_key malformed_marker: "m:2:x" = "1760000060300000:1760000000450000"`)}},
		{"row 6: marker value grammar", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, kA, "m:2:2", "1760000060300000")
		}, []string{line(kA, `check_key malformed_marker: "m:2:2" = "1760000060300000"`)}},
		{"row 6: marker generation is not g", func(st storestate.State, _ *storestate.AuditConfig) {
			unset(st, kA, "m:2:2")
			set(st, kA, "m:1:2", "1760000060300000:1760000000450000")
			st[miA] = zset("1:2", "1760000060300000")
		}, []string{line(kA, `check_key malformed_marker: "m:1:2": generation 1, g 2`)}},
		{"row 7: mc mismatch", func(st storestate.State, _ *storestate.AuditConfig) { set(st, kA, "mc", "2") },
			[]string{line(kA, "check_key marker_count: 1 marker fields, mc 2"), nmLine("1", "2", "1")}},
		{"row 8: mi member not <g>:<s>", func(st storestate.State, _ *storestate.AuditConfig) {
			st[miA] = zset("2-2", "1760000060300000")
		}, []string{line(kA, `check_key marker_index_mismatch: member "2-2" is not <g>:<s>`)}},
		{"row 8: marker without its mi member", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, kA, "m:2:3", "1760000060350000:1760000000450000", "mc", "2")
			set(st, epK, "nm", "2")
		}, []string{line(kA, "check_key marker_index_mismatch: 1 members, 2 marker fields"), nmLine("2", "2", "1")}},
		{"row 8: mi member without its marker", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, miA, "2:9", "1760000060300000")
		}, []string{line(kA, `check_key marker_index_mismatch: member "2:9" has no marker field`), nmLine("1", "1", "2")}},
		{"row 8: mi score is not the marker expiry", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, miA, "2:2", "1760000060300001")
		}, []string{line(kA, `check_key marker_index_mismatch: member "2:2" score "1760000060300001", marker expiry 1760000060300000`)}},
		{"row 8: markers without a marker index", func(st storestate.State, _ *storestate.AuditConfig) { delete(st, miA) },
			[]string{line(kA, "check_key marker_index_mismatch: 1 marker fields, no mi:<enc>"), nmLine("1", "1", "0")}},
		{"row 9: bucket without index", func(st storestate.State, _ *storestate.AuditConfig) { unset(st, ixK, "Yg") },
			[]string{line(kB, "check_key bucket_without_index: no ix member"), line(ixK, "ZCARD 1, but 2 k:<enc> keys")}},
		{"row 11: ix score inf", ixScore("inf"), []string{line(kB, `check_key malformed_index: ix score "inf"`)}},
		{"row 11: ix score 1.5", ixScore("1.5"), []string{line(kB, `check_key malformed_index: ix score "1.5"`)}},
		{"row 11: ix score -1", ixScore("-1"), []string{line(kB, `check_key malformed_index: ix score "-1"`)}},
		{"row 11: ix score 2^53", ixScore("9007199254740992"),
			[]string{line(kB, `check_key malformed_index: ix score "9007199254740992"`)}},

		// Bounds on ok budget keys.
		{"g above gen", func(st storestate.State, _ *storestate.AuditConfig) { set(st, kB, "g", "6") },
			[]string{line(kB, "g 6 > gen 5")}},
		{"Restored does not exempt g", func(st storestate.State, c *storestate.AuditConfig) {
			c.Restored = []string{kB}
			set(st, kB, "g", "6")
		}, []string{line(kB, "g 6 > gen 5")}},
		{"marker expiry above x", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, kA, "m:2:2", "1760000060400001:1760000000450000")
			set(st, miA, "2:2", "1760000060400001")
		}, []string{line(kA, `"m:2:2" expiry 1760000060400001 > x 1760000060400000`)}},
		{"t above hwm", func(st storestate.State, _ *storestate.AuditConfig) { set(st, kB, "t", "1760000000500001") },
			[]string{line(kB, "t 1760000000500001 > hwm 1760000000500000")}},
		{"marker credit time above hwm", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, kA, "m:2:2", "1760000060300000:1760000000500001")
		}, []string{line(kA, `"m:2:2" credit time 1760000000500001 > hwm 1760000000500000`)}},
		{"ix score above eligibility", ixScore("1760000060500001"),
			[]string{line(kB, "ix score 1760000060500001 > 1760000060500000 = min(eligibility 1760000060500000, earliest marker expiry none)")}},
		{"ix score above the earliest marker expiry", func(st storestate.State, _ *storestate.AuditConfig) {
			set(st, ixK, "YQ", "1760000060300001")
		}, []string{line(kA, "ix score 1760000060300001 > 1760000060300000 = min(eligibility 1760000060400000, earliest marker expiry 1760000060300000)")}},
		{"ix score above the refill eligibility", refillBound("1760000002500001"),
			[]string{line(kB, "ix score 1760000002500001 > 1760000002500000 = min(eligibility 1760000002500000, earliest marker expiry none)")}},
		{"ix score above a half-full bucket's eligibility", halfFull("1760000001500001"),
			[]string{line(kB, "ix score 1760000001500001 > 1760000001500000 = min(eligibility 1760000001500000, earliest marker expiry none)")}},
		{"ix score above the earliest of three markers", func(st storestate.State, _ *storestate.AuditConfig) {
			threeMarkers(st)
			set(st, ixK, "YQ", "1760000060300001")
		}, []string{line(kA, "ix score 1760000060300001 > 1760000060300000 = min(eligibility 1760000060400000, earliest marker expiry 1760000060300000)")}},
		{"the middle one of three markers expiring after x", func(st storestate.State, _ *storestate.AuditConfig) {
			threeMarkers(st)
			set(st, kA, "m:2:2", "1760000060400001:1760000000450000")
			set(st, miA, "2:2", "1760000060400001")
		}, []string{line(kA, `"m:2:2" expiry 1760000060400001 > x 1760000060400000`)}},
		{"the middle one of three markers credited after hwm", func(st storestate.State, _ *storestate.AuditConfig) {
			threeMarkers(st)
			set(st, kA, "m:2:2", "1760000060300000:1760000000500001")
		}, []string{line(kA, `"m:2:2" credit time 1760000000500001 > hwm 1760000000500000`)}},
		{"Restored does not exempt a marker expiry above x", func(st storestate.State, c *storestate.AuditConfig) {
			c.Restored = []string{kA}
			threeMarkers(st)
			set(st, kA, "m:2:2", "1760000060400001:1760000000450000")
			set(st, miA, "2:2", "1760000060400001")
		}, []string{line(kA, `"m:2:2" expiry 1760000060400001 > x 1760000060400000`)}},

		// Counters, q and ix members.
		{"nm mismatch", malformedRecord("nm", "2"), []string{nmLine("2", "1", "1")}},
		{"nb below the k: keys", malformedRecord("nb", "1"), []string{nbLine("1", "2", "2")}},
		{"nb above the k: keys", malformedRecord("nb", "3"), []string{nbLine("3", "2", "2")}},
		{"nb two above with one Restored key", func(st storestate.State, c *storestate.AuditConfig) {
			c.Restored = []string{kB}
			set(st, epK, "nb", "4")
		}, []string{nbLine("4", "2", "3")}},
		{"undeclared q entry", func(st storestate.State, _ *storestate.AuditConfig) {
			st[qK] = hash("YQ", "malformed_bucket|1760000000500000")
		}, []string{line(qK, `entry "YQ", but neither its k: nor its mi: key is Damaged`)}},
		{"q entry of a key other than the Damaged one", func(st storestate.State, c *storestate.AuditConfig) {
			c.Damaged = []string{kB}
			st[qK] = hash("YQ", "malformed_bucket|1760000000500000")
		}, []string{line(qK, `entry "YQ", but neither its k: nor its mi: key is Damaged`)}},
		{"ix member fails is_enc", func(st storestate.State, _ *storestate.AuditConfig) { set(st, ixK, "!!", "1") },
			[]string{line(ixK, `member "!!" fails is_enc`), line(ixK, "ZCARD 3, but 2 k:<enc> keys")}},

		// Canaries.
		{"canary raw in a value", inValue(string(canary)), []string{line("rl:fill:c", "canary 0 (raw) in the value")}},
		{"canary in hex in a value", inValue(hexLow), []string{line("rl:fill:c", "canary 0 (hex) in the value")}},
		{"canary in upper-case hex in a value", inValue(hexUp),
			[]string{line("rl:fill:c", "canary 0 (upper-case hex) in the value")}},
		{"canary in base64 in a value", inValue(b64), []string{line("rl:fill:c", "canary 0 (base64) in the value")}},
		{"canary in unpadded base64 in a value", inValue(b64raw),
			[]string{line("rl:fill:c", "canary 0 (unpadded base64) in the value")}},
		{"canary in base64url in a value", inValue(b64url), []string{line("rl:fill:c", "canary 0 (base64url) in the value")}},
		{"canary in unpadded base64url in a value", inValue(b64urlRaw),
			[]string{line("rl:fill:c", "canary 0 (unpadded base64url) in the value")}},
		{"canary 1 byte into a base64 value", embedded(base64.StdEncoding, pre1, nil),
			[]string{line("rl:fill:c", "canary 0 (base64 at byte offset 1 mod 3) in the value")}},
		{"canary 2 bytes into a base64 value", embedded(base64.StdEncoding, pre2, nil),
			[]string{line("rl:fill:c", "canary 0 (base64 at byte offset 2 mod 3) in the value")}},
		{"canary at the start of a base64 value with bytes after it", embedded(base64.StdEncoding, nil, post),
			[]string{line("rl:fill:c", "canary 0 (base64 at byte offset 0 mod 3) in the value")}},
		{"canary 1 byte into a base64 value with bytes after it", embedded(base64.StdEncoding, pre1, post),
			[]string{line("rl:fill:c", "canary 0 (base64 at byte offset 1 mod 3) in the value")}},
		{"canary 2 bytes into a base64 value with bytes after it", embedded(base64.StdEncoding, pre2, post),
			[]string{line("rl:fill:c", "canary 0 (base64 at byte offset 2 mod 3) in the value")}},
		{"canary 1 byte into a base64url value", embedded(base64.RawURLEncoding, pre1, nil),
			[]string{line("rl:fill:c", "canary 0 (base64url at byte offset 1 mod 3) in the value")}},
		{"canary 2 bytes into a base64url value", embedded(base64.RawURLEncoding, pre2, nil),
			[]string{line("rl:fill:c", "canary 0 (base64url at byte offset 2 mod 3) in the value")}},
		{"canary at the start of a base64url value with bytes after it", embedded(base64.RawURLEncoding, nil, post),
			[]string{line("rl:fill:c", "canary 0 (base64url at byte offset 0 mod 3) in the value")}},
		{"canary 1 byte into a base64url value with bytes after it", embedded(base64.RawURLEncoding, pre1, post),
			[]string{line("rl:fill:c", "canary 0 (base64url at byte offset 1 mod 3) in the value")}},
		{"canary 2 bytes into a base64url value with bytes after it", embedded(base64.RawURLEncoding, pre2, post),
			[]string{line("rl:fill:c", "canary 0 (base64url at byte offset 2 mod 3) in the value")}},
		{"two canaries in two keys", func(st storestate.State, c *storestate.AuditConfig) {
			c.Canaries = [][]byte{canary, canary1}
			st["rl:fill:a"] = str("<" + string(canary) + ">")
			st["rl:fill:b"] = str("<" + string(canary1) + ">")
		}, []string{line("rl:fill:a", "canary 0 (raw) in the value"), line("rl:fill:b", "canary 1 (raw) in the value")}},
		{"canary in a key name", func(st storestate.State, c *storestate.AuditConfig) {
			withCanary(c)
			st["rl:fill:"+hexUp] = str("x")
		}, []string{line("rl:fill:"+hexUp, "canary 0 (upper-case hex) in the key name")}},
		{"canary in a field name", func(st storestate.State, c *storestate.AuditConfig) {
			withCanary(c)
			st["rl:fill:h"] = hash(hexLow, "1")
		}, []string{line("rl:fill:h", "canary 0 (hex) in a field name")}},
		{"canary in a member", func(st storestate.State, c *storestate.AuditConfig) {
			withCanary(c)
			st["rl:fill:z"] = zset(b64, "1")
		}, []string{line("rl:fill:z", "canary 0 (base64) in a member")}},
		{"canary in the epoch record", func(st storestate.State, c *storestate.AuditConfig) {
			withCanary(c)
			set(st, epK, "hold", b64urlRaw)
		}, []string{line(epK, `canary 0 (unpadded base64url) in the value of field "hold"`)}},
		{"empty canary", func(_ storestate.State, c *storestate.AuditConfig) { c.Canaries = [][]byte{{}} },
			[]string{"audit config: canary 0 is empty"}},
		{"empty filler prefix is not used", func(st storestate.State, c *storestate.AuditConfig) {
			c.FillerPrefixes = []string{"", "rl:fill:"}
			st["rl:other"] = str("x")
		}, []string{"audit config: filler prefix 0 is empty", line("rl:other", "not a key of the §A3.2 layout")}},
	}
	// Row 4 for each bucket field: a number printed as a float (as tostring or .. on a
	// number prints it), then is_uint53's bounds: 2^53, and 19 digits (2^63, past int64).
	for _, f := range []struct{ field, value string }{
		{"u", "1.9e+10"}, {"t", "1.5"}, {"g", "5.0"}, {"s", "1.0"}, {"x", "1.7600000605e+15"}, {"mc", "0.0"},
		{"s", "9007199254740992"}, {"u", "9223372036854775808"},
	} {
		cases = append(cases, fixture{"row 4: " + f.field + " = " + f.value,
			func(st storestate.State, _ *storestate.AuditConfig) { set(st, kB, f.field, f.value) },
			[]string{line(kB, "check_key malformed_bucket: "+f.field+" = "+strconv.Quote(f.value))}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := cleanState(fp)
			c := storestate.AuditConfig{Policy: p, FillerPrefixes: []string{"rl:fill:", "rl:pad:"}}
			if tc.edit != nil {
				tc.edit(st, &c)
			}
			got := slices.Sorted(slices.Values(storestate.Audit(st, c)))
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(got, want) {
				t.Errorf("Audit reported\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
			}
		})
	}
}
