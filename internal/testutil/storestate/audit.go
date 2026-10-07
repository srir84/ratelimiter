package storestate

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/settings"
)

const (
	// two53 is 2^53: every integer the state holds is below it (§A3.1).
	two53 = 1 << 53
	// tBound is TBOUND = 2^53 − 2^43, the bound on every budget time and on hwm (§A3.1).
	tBound = two53 - 1<<43
	// capUnitsLimit bounds cap_units = capacity × rate_period_ns (§A3.1).
	capUnitsLimit = 1 << 52
)

// AuditConfig is what Audit needs besides the snapshot (DESIGN §C3; plan S4 step 13).
type AuditConfig struct {
	// Policy is the policy whose fingerprint equals rl:epoch.fp: the limits the current
	// epoch's buckets follow (T-CFG-5 replaces it).
	Policy settings.Policy
	// Damaged names the store keys a test breaks on purpose. Restored names the keys
	// admin_restore wrote: a budget key counts as restored when its k:<enc> or mi:<enc> key
	// is listed. Both hold store key names.
	Damaged, Restored []string
	// FillerPrefixes are the key-name prefixes outside the §A3.2 layout that tests fill:
	// rl:fill: (scriptrun) and rl:pad: (the driver).
	FillerPrefixes []string
	// Canaries are byte strings that no key name, field, member or value may hold.
	Canaries [][]byte
}

// Audit checks snap and returns one line per violation, nil when there is none (DESIGN
// §C3, §C4; plan S4 step 13). The rules:
//
//   - Grammar, every key: rl:epoch (a hash), rl:ctl:time (a string), or rl:e:<32 hex>:
//     followed by ix (a sorted set), q (a hash), k:<enc> (a hash) or mi:<enc> (a sorted
//     set) with enc per keys.IsEnc; or any key, of any type, under a FillerPrefixes entry.
//     A Damaged key is checked by name only. Any other key must also have its type, and
//     rl:epoch must be well-formed: record_wellformed, and hwm present, is_uint53 and
//     below TBOUND (§A4.2).
//   - Current epoch only: the namespace that rl:epoch.id names, when rl:epoch exists, is
//     not Damaged and passes the grammar rule; other namespaces get the grammar rule only.
//     Policy's fingerprint must equal rl:epoch.fp, and its limits must lie inside §A3.1.
//     A Damaged ix or q, or one of the wrong type (plan choice), leaves the namespace at
//     the grammar rule. A budget key whose k:<enc> or mi:<enc> is Damaged is exempt, with
//     its ix member and q field, even when the key does not exist. Every other budget key
//     (an enc with a k:<enc> or mi:<enc> key or an ix member) must pass check_key (§A3.3,
//     restated here) with ok, and then g ≤ gen, each marker expiry ≤ x, t and each marker
//     credit time ≤ hwm, and, unless it is Restored, ix score ≤ min(eligibility, earliest
//     marker expiry), with eligibility = max(t + ⌈(cap_units − u) / (rate_tokens × 1000)⌉,
//     x) (§A3.2). Every ix member that is not exempt must pass is_enc, and q may hold only
//     exempt budget keys, so it is empty when nothing is Damaged.
//   - Counters, only when Damaged is empty: nm = the sum of mc = the sum of ZCARD
//     mi:<enc>; ZCARD ix = the number of k:<enc> keys ≤ nb ≤ that number + the number of
//     distinct budget keys of the namespace that Restored names (§A3.3: a restore may count
//     a key once too many). A key listed twice, or by its k: and its mi: name, counts once;
//     a key of another namespace counts none, since nb starts again in each epoch.
//   - Canaries: no key name, field, member, score or value holds a canary raw, in hex
//     (lower or upper case) or in base64 (standard or URL alphabet, padded or not), also
//     inside a longer base64 value, at any byte offset and with bytes after it.
//
// An empty canary or filler prefix is reported as a configuration error and never used.
func Audit(snap State, c AuditConfig) []string {
	a := &auditor{snap: snap, c: c, damaged: make(map[string]bool, len(c.Damaged))}
	for _, d := range c.Damaged {
		a.damaged[d] = true
	}
	a.config()
	names := slices.Sorted(maps.Keys(snap))
	parsed := make(map[string]keyName, len(names))
	for _, n := range names {
		kn := a.parse(n)
		parsed[n] = kn
		a.grammar(n, kn)
	}
	if e, ok := a.record(); ok {
		a.namespace(e, names, parsed)
	}
	a.canaries(names)
	return a.out
}

// auditor collects the violations of one Audit call.
type auditor struct {
	snap    State
	c       AuditConfig
	damaged map[string]bool
	out     []string
}

func (a *auditor) addf(format string, args ...any) {
	a.out = append(a.out, fmt.Sprintf(format, args...))
}

// config reports the configuration entries Audit cannot use.
func (a *auditor) config() {
	for i, c := range a.c.Canaries {
		if len(c) == 0 {
			a.addf("audit config: canary %d is empty", i)
		}
	}
	for i, p := range a.c.FillerPrefixes {
		if p == "" {
			a.addf("audit config: filler prefix %d is empty", i)
		}
	}
}

// keyKind is a key name's place in the §A3.2 layout.
type keyKind int

const (
	kindStray  keyKind = iota // no place: a violation
	kindFiller                // under a filler prefix
	kindEpoch                 // rl:epoch
	kindCtl                   // rl:ctl:time
	kindIx                    // rl:e:<id>:ix
	kindQ                     // rl:e:<id>:q
	kindK                     // rl:e:<id>:k:<enc>
	kindMi                    // rl:e:<id>:mi:<enc>
)

// want returns the type §A3.2 gives a key of kind k, or 0 when there is none to check.
func (k keyKind) want() Type {
	switch k {
	case kindEpoch, kindQ, kindK:
		return Hash
	case kindIx, kindMi:
		return ZSet
	case kindCtl:
		return String
	case kindStray, kindFiller:
	}
	return 0
}

// keyName is a parsed key name: its kind, and the namespace ID and budget key encoding
// where the kind has them.
type keyName struct {
	kind    keyKind
	id, enc string
}

// parse places a key name in the layout; filler prefixes are tried after the layout.
func (a *auditor) parse(name string) keyName {
	switch name {
	case keys.EpochKey:
		return keyName{kind: kindEpoch}
	case keys.CtlTimeKey:
		return keyName{kind: kindCtl}
	}
	if rest, ok := strings.CutPrefix(name, "rl:e:"); ok && len(rest) > 33 && rest[32] == ':' && isHex32(rest[:32]) {
		id, suffix := rest[:32], rest[33:]
		if suffix == "ix" {
			return keyName{kind: kindIx, id: id}
		}
		if suffix == "q" {
			return keyName{kind: kindQ, id: id}
		}
		if enc, ok := strings.CutPrefix(suffix, "k:"); ok && keys.IsEnc(enc) {
			return keyName{kind: kindK, id: id, enc: enc}
		}
		if enc, ok := strings.CutPrefix(suffix, "mi:"); ok && keys.IsEnc(enc) {
			return keyName{kind: kindMi, id: id, enc: enc}
		}
	}
	for _, p := range a.c.FillerPrefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return keyName{kind: kindFiller}
		}
	}
	return keyName{kind: kindStray}
}

// grammar applies the grammar rule's name and type checks to one key.
func (a *auditor) grammar(name string, kn keyName) {
	if kn.kind == kindStray {
		a.addf("%q: not a key of the §A3.2 layout", name)
		return
	}
	want := kn.kind.want()
	if want == 0 || a.damaged[name] {
		return
	}
	if got := a.snap[name].Type; got != want {
		a.addf("%q: type %s, want %s", name, got, want)
	}
}

// epoch holds the values of the current epoch record that the rules use.
type epoch struct {
	id, fp           string
	hwm, gen, nb, nm int64
}

// record checks rl:epoch and returns it when it names the current namespace: it exists,
// is not Damaged, is a hash (grammar reports another type) and is well-formed. Each field
// that fails is reported with load_epoch's reason (§A4.2). A record whose fp is not the
// audit policy's fingerprint is reported but still names the namespace.
func (a *auditor) record() (epoch, bool) {
	v, ok := a.snap[keys.EpochKey]
	if !ok || a.damaged[keys.EpochKey] || v.Type != Hash {
		return epoch{}, false
	}
	good := true
	field := func(name string, valid func(string) bool) string {
		s, ok := v.Fields[name]
		switch {
		case !ok:
			a.addf("%q: epoch_record_malformed: %s missing", keys.EpochKey, name)
			good = false
		case !valid(s):
			a.addf("%q: epoch_record_malformed: %s = %s", keys.EpochKey, name, quote(s))
			good = false
		}
		return s
	}
	count := func(name string) int64 {
		n, _ := uint53(field(name, isUint53))
		return n
	}
	e := epoch{id: field("id", isHex32), fp: field("fp", isHex32)}
	field("schema", func(s string) bool { return s == "1" })
	field("clock", func(s string) bool { return s == "store" || s == "controlled" })
	e.gen, e.nb, e.nm = count("gen"), count("nb"), count("nm")
	count("origin")
	count("created")
	field("hold", func(string) bool { return true })
	hwm, ok := v.Fields["hwm"]
	n, valid := uint53(hwm)
	switch {
	case !ok:
		a.addf("%q: hwm_missing", keys.EpochKey)
		good = false
	case !valid:
		a.addf("%q: hwm_malformed: hwm = %s", keys.EpochKey, quote(hwm))
		good = false
	case n >= tBound:
		a.addf("%q: hwm_malformed: hwm %d ≥ TBOUND %d", keys.EpochKey, n, int64(tBound))
		good = false
	}
	e.hwm = n
	if !good {
		return epoch{}, false
	}
	if want := fingerprint.Of(a.c.Policy); e.fp != want {
		a.addf("%q: fp %s is not the audit policy's fingerprint %s", keys.EpochKey, e.fp, want)
	}
	return e, true
}

// namespace applies the current-epoch rule and, when nothing is Damaged, the counter rule
// to the namespace e names.
func (a *auditor) namespace(e epoch, names []string, parsed map[string]keyName) {
	ixName, qName := keys.IndexKey(e.id), keys.QuarantineKey(e.id)
	ix, ixOK := a.shared(ixName, ZSet)
	q, qOK := a.shared(qName, Hash)
	if !ixOK || !qOK {
		return
	}
	lim, limOK := a.limits()
	exempt := namedBudgetKeys(a.c.Damaged, e.id)
	restored := namedBudgetKeys(a.c.Restored, e.id)
	for _, enc := range slices.Sorted(maps.Keys(q)) {
		if !exempt[enc] {
			a.addf("%q: entry %s, but neither its k: nor its mi: key is Damaged", qName, quote(enc))
		}
	}
	encs := make(map[string]bool)
	for _, m := range slices.Sorted(maps.Keys(ix)) {
		switch {
		case exempt[m]:
		case !keys.IsEnc(m):
			a.addf("%q: member %s fails is_enc", ixName, quote(m))
		default:
			encs[m] = true
		}
	}
	buckets := 0
	for _, n := range names {
		kn := parsed[n]
		if kn.id != e.id || (kn.kind != kindK && kn.kind != kindMi) {
			continue
		}
		if kn.kind == kindK {
			buckets++
		}
		if !exempt[kn.enc] {
			encs[kn.enc] = true
		}
	}
	if limOK {
		for _, enc := range slices.Sorted(maps.Keys(encs)) {
			a.budgetKey(e, enc, ix, restored[enc], lim)
		}
	}
	if len(a.c.Damaged) == 0 {
		a.counters(e, len(ix), buckets, len(restored), names, parsed)
	}
}

// shared returns the fields of the namespace's ix or q (nil when the key is absent), or
// false when the key is Damaged or of the wrong type: then the namespace gets the grammar
// rule only.
func (a *auditor) shared(name string, want Type) (map[string]string, bool) {
	v, ok := a.snap[name]
	if a.damaged[name] || (ok && v.Type != want) {
		return nil, false
	}
	return v.Fields, true
}

// limits are the audit policy's values that the bucket rules use.
type limits struct {
	capUnits   int64 // capacity × rate_period_ns
	unitsPerUs int64 // rate_tokens × 1000: what refill adds per µs (§A3.1)
}

// limits returns the audit policy's limits, or reports a policy outside §A3.1, for which
// the bucket arithmetic is not exact; the bucket checks are then skipped.
func (a *auditor) limits() (limits, bool) {
	p := a.c.Policy
	hi, capUnits := bits.Mul64(uint64(p.Capacity), uint64(p.RatePeriodNs))
	if p.RateTokens < 1 || p.RateTokens > math.MaxInt32 || p.Capacity < 1 || p.RatePeriodNs < 1 ||
		hi != 0 || capUnits >= capUnitsLimit {
		a.addf("audit policy: limits outside §A3.1: rate_tokens %d, capacity %d, rate_period_ns %d",
			p.RateTokens, p.Capacity, p.RatePeriodNs)
		return limits{}, false
	}
	return limits{capUnits: int64(capUnits), unitsPerUs: p.RateTokens * 1000}, true
}

// budgetKey applies check_key and the bucket rules to one budget key of the namespace.
func (a *auditor) budgetKey(e epoch, enc string, ix map[string]string, restored bool, lim limits) {
	kName := keys.BucketKey(e.id, enc)
	k, kOK := a.snap[kName]
	mi, miOK := a.snap[keys.MarkerIndexKey(e.id, enc)]
	score, ixOK := ix[enc]
	b, outcome, detail := checkKey(k, kOK, mi, miOK, score, ixOK, lim.capUnits)
	switch outcome {
	case "ok":
	case "absent":
		return
	default:
		a.addf("%q: check_key %s: %s", kName, outcome, detail)
		return
	}
	if b.g > e.gen {
		a.addf("%q: g %d > gen %d", kName, b.g, e.gen)
	}
	if b.t > e.hwm {
		a.addf("%q: t %d > hwm %d", kName, b.t, e.hwm)
	}
	earliest, earliestText := int64(math.MaxInt64), "none"
	for _, m := range b.markers {
		if m.expiry > b.x {
			a.addf("%q: %s expiry %d > x %d", kName, quote(m.field), m.expiry, b.x)
		}
		if m.credit > e.hwm {
			a.addf("%q: %s credit time %d > hwm %d", kName, quote(m.field), m.credit, e.hwm)
		}
		if m.expiry < earliest {
			earliest, earliestText = m.expiry, strconv.FormatInt(m.expiry, 10)
		}
	}
	if restored {
		return
	}
	elig := max(b.t+ceilDiv(lim.capUnits-b.u, lim.unitsPerUs), b.x)
	if bound := min(elig, earliest); b.score > bound {
		a.addf("%q: ix score %d > %d = min(eligibility %d, earliest marker expiry %s)",
			kName, b.score, bound, elig, earliestText)
	}
}

// counters applies the counter rule; restored is the number of distinct budget keys of the
// namespace that Restored names, each of which a restore may have counted once too many.
func (a *auditor) counters(e epoch, ixCard, buckets, restored int, names []string, parsed map[string]keyName) {
	var sumMC, sumMI int64
	for _, n := range names {
		kn, v := parsed[n], a.snap[n]
		if kn.id != e.id {
			continue
		}
		if kn.kind == kindK && v.Type == Hash {
			if mc, ok := uint53(v.Fields["mc"]); ok {
				sumMC = addSat(sumMC, mc)
			}
		}
		if kn.kind == kindMi && v.Type == ZSet {
			sumMI = addSat(sumMI, int64(len(v.Fields)))
		}
	}
	if e.nm != sumMC || e.nm != sumMI {
		a.addf("%q: nm %d, but the sum of mc is %d and the sum of ZCARD mi:<enc> is %d",
			keys.EpochKey, e.nm, sumMC, sumMI)
	}
	if ixCard != buckets {
		a.addf("%q: ZCARD %d, but %d k:<enc> keys", keys.IndexKey(e.id), ixCard, buckets)
	}
	if lo, hi := int64(buckets), int64(buckets)+int64(restored); e.nb < lo || e.nb > hi {
		a.addf("%q: nb %d, want %d … %d (the k:<enc> keys, plus Restored)", keys.EpochKey, e.nb, lo, hi)
	}
}

// canaryForm is one text form of a canary: its name and the text searched for.
type canaryForm struct{ name, text string }

// canaryForms returns the forms of the canary rule for c (DESIGN §C4): raw, hex in lower and
// upper case, base64 in the standard and URL alphabets, padded and not, and, since inside a
// longer base64 value the canary may start at any byte offset and have more bytes after it,
// for each offset mod 3 the part of its encoding that depends on neither. Nil for an empty c.
func canaryForms(c []byte) []canaryForm {
	if len(c) == 0 {
		return nil
	}
	h := hex.EncodeToString(c)
	forms := []canaryForm{
		{"raw", string(c)},
		{"hex", h},
		{"upper-case hex", strings.ToUpper(h)},
		{"base64", base64.StdEncoding.EncodeToString(c)},
		{"unpadded base64", base64.RawStdEncoding.EncodeToString(c)},
		{"base64url", base64.URLEncoding.EncodeToString(c)},
		{"unpadded base64url", base64.RawURLEncoding.EncodeToString(c)},
	}
	for _, alphabet := range []struct {
		name string
		enc  *base64.Encoding
	}{{"base64", base64.RawStdEncoding}, {"base64url", base64.RawURLEncoding}} {
		for k := range 3 {
			if core := base64Core(alphabet.enc, c, k); core != "" {
				forms = append(forms, canaryForm{fmt.Sprintf("%s at byte offset %d mod 3", alphabet.name, k), core})
			}
		}
	}
	return forms
}

// FindCanaries applies the canary rule of Audit to data, the bytes of a file (the driver
// searches the store's AOF files with it, DESIGN §C4; plan S6 step 13): for each canary
// found, in any form, one message naming the canary's index and the first form found. It
// never includes the canary itself. An empty canary is skipped.
func FindCanaries(data []byte, canaries [][]byte) []string {
	var out []string
	for i, c := range canaries {
		for _, f := range canaryForms(c) {
			if bytes.Contains(data, []byte(f.text)) {
				out = append(out, fmt.Sprintf("canary %d (%s)", i, f.name))
				break
			}
		}
	}
	return out
}

// canaries applies the canary rule to every key name, field, member, score and value.
func (a *auditor) canaries(names []string) {
	type canary struct {
		index int
		forms []canaryForm
	}
	var cs []canary
	for i, c := range a.c.Canaries {
		if forms := canaryForms(c); len(forms) > 0 {
			cs = append(cs, canary{index: i, forms: forms})
		}
	}
	if len(cs) == 0 {
		return
	}
	check := func(key, where, s string) {
		for _, c := range cs {
			for _, f := range c.forms {
				if strings.Contains(s, f.text) {
					a.addf("%q: canary %d (%s) in %s", key, c.index, f.name, where)
					break
				}
			}
		}
	}
	for _, n := range names {
		v := a.snap[n]
		check(n, "the key name", n)
		for _, f := range slices.Sorted(maps.Keys(v.Fields)) {
			if v.Type == ZSet {
				check(n, "a member", f)
				check(n, "the score of member "+quote(f), v.Fields[f])
			} else {
				check(n, "a field name", f)
				check(n, "the value of field "+quote(f), v.Fields[f])
			}
		}
		check(n, "the value", v.Str)
	}
}

// base64Core returns the characters of c's encoding by enc, an unpadded encoding, that
// hold only c's bits when c starts k bytes into a 3-byte group (k = 0, 1 or 2), whatever
// bytes come before or after it: characters [⌈8k/6⌉, ⌊8(k+n)/6⌋) of the encoding of k
// zero bytes followed by c, n = len(c). It is "" when c is too short to fill one.
func base64Core(enc *base64.Encoding, c []byte, k int) string {
	s := enc.EncodeToString(append(make([]byte, k, k+len(c)), c...))
	lo, hi := (8*k+5)/6, 8*(k+len(c))/6
	if hi <= lo {
		return ""
	}
	return s[lo:hi]
}

// bucket is a budget key that check_key found ok.
type bucket struct {
	u, t, g, x int64
	markers    []marker // in field-name order
	score      int64    // the key's ix score
}

// marker is one m:<g>:<s> field of a bucket.
type marker struct {
	field          string
	expiry, credit int64
}

// checkKey restates check_key (§A3.3; plan S4 step 2) on a snapshot whose ix is a sorted
// set or absent, so the shared_key_malformed row never matches: the rows top to bottom,
// the first match deciding. It returns the outcome ("ok" with the bucket, "absent", or a
// quarantine reason) and, for a reason, what matched.
func checkKey(k Value, kOK bool, mi Value, miOK bool, score string, ixOK bool, capUnits int64) (bucket, string, string) {
	if !kOK {
		switch {
		case !miOK && !ixOK:
			return bucket{}, "absent", ""
		case miOK:
			return bucket{}, "marker_index_without_bucket", "mi:<enc> exists without k:<enc>"
		default:
			return bucket{}, "index_without_bucket", "an ix member exists without k:<enc>"
		}
	}
	b, byGS, reason, detail := bucketRows(k, capUnits)
	if reason == "" {
		reason, detail = markerIndexRow(mi, miOK, byGS)
	}
	if reason != "" {
		return bucket{}, reason, detail
	}
	if !ixOK {
		return bucket{}, "bucket_without_index", "no ix member"
	}
	s, ok := isScore(score)
	if !ok {
		return bucket{}, "malformed_index", "ix score " + quote(score)
	}
	b.score = s
	return b, "ok", ""
}

// bucketFieldNames are the six bucket fields of k:<enc>, in §A3.2 order.
func bucketFieldNames() []string { return []string{"u", "t", "g", "s", "x", "mc"} }

// bucketRows applies the rows about k:<enc> alone (4 to 7): it returns the bucket and its
// markers by "<g>:<s>", or the first matching row's reason and what matched. A field that
// is neither a bucket field nor m:-prefixed counts as malformed_bucket (plan S4 step 2), and
// so does g < 1 (owner decision at S4's §5 stop: g < 1 joins row 4's out-of-range rule).
func bucketRows(k Value, capUnits int64) (bucket, map[string]marker, string, string) {
	const malformed = "malformed_bucket"
	if k.Type != Hash {
		return bucket{}, nil, malformed, "k:<enc> is a " + k.Type.String()
	}
	present := 0
	var markerFields []string
	for _, f := range slices.Sorted(maps.Keys(k.Fields)) {
		switch {
		case slices.Contains(bucketFieldNames(), f):
			present++
		case strings.HasPrefix(f, "m:"):
			markerFields = append(markerFields, f)
		default:
			return bucket{}, nil, malformed, "field " + quote(f) + " is neither a bucket field nor a marker"
		}
	}
	switch {
	case present == 0 && len(markerFields) > 0:
		return bucket{}, nil, "marker_without_bucket", fmt.Sprintf("%d marker fields, no bucket field", len(markerFields))
	case present < len(bucketFieldNames()):
		return bucket{}, nil, malformed, fmt.Sprintf("%d of the 6 bucket fields", present)
	}
	var v [6]int64
	for i, f := range bucketFieldNames() {
		n, ok := uint53(k.Fields[f])
		if !ok {
			return bucket{}, nil, malformed, f + " = " + quote(k.Fields[f])
		}
		v[i] = n
	}
	b, mc := bucket{u: v[0], t: v[1], g: v[2], x: v[4]}, v[5]
	if b.u > capUnits {
		return bucket{}, nil, malformed, fmt.Sprintf("u %d > cap_units %d", b.u, capUnits)
	}
	if b.g < 1 { // out of range too, as in check_key (owner decision at S4's §5 stop)
		return bucket{}, nil, malformed, fmt.Sprintf("g %d < 1", b.g)
	}
	byGS := make(map[string]marker, len(markerFields))
	for _, f := range markerFields {
		gs := strings.TrimPrefix(f, "m:")
		g, _, nameOK := pair(gs)
		expiry, credit, valueOK := pair(k.Fields[f])
		switch {
		case !nameOK || !valueOK:
			return bucket{}, nil, "malformed_marker", quote(f) + " = " + quote(k.Fields[f])
		case g != b.g:
			return bucket{}, nil, "malformed_marker", fmt.Sprintf("%s: generation %d, g %d", quote(f), g, b.g)
		}
		m := marker{field: f, expiry: expiry, credit: credit}
		b.markers = append(b.markers, m)
		byGS[gs] = m
	}
	if n := int64(len(markerFields)); n != mc {
		return bucket{}, nil, "marker_count", fmt.Sprintf("%d marker fields, mc %d", n, mc)
	}
	return b, byGS, "", ""
}

// markerIndexRow applies row 8: mi:<enc>, when it exists, is a sorted set whose members
// are exactly the markers' "<g>:<s>", each scored with its marker's expiry, compared as
// numbers.
func markerIndexRow(mi Value, miOK bool, byGS map[string]marker) (string, string) {
	const mismatch = "marker_index_mismatch"
	if !miOK {
		if len(byGS) > 0 {
			return mismatch, fmt.Sprintf("%d marker fields, no mi:<enc>", len(byGS))
		}
		return "", ""
	}
	if mi.Type != ZSet {
		return mismatch, "mi:<enc> is a " + mi.Type.String()
	}
	for _, member := range slices.Sorted(maps.Keys(mi.Fields)) {
		sc := mi.Fields[member]
		if _, _, ok := pair(member); !ok {
			return mismatch, "member " + quote(member) + " is not <g>:<s>"
		}
		m, ok := byGS[member]
		if !ok {
			return mismatch, "member " + quote(member) + " has no marker field"
		}
		if f, err := strconv.ParseFloat(sc, 64); err != nil || f != float64(m.expiry) {
			return mismatch, fmt.Sprintf("member %s score %s, marker expiry %d", quote(member), quote(sc), m.expiry)
		}
	}
	if len(mi.Fields) != len(byGS) {
		return mismatch, fmt.Sprintf("%d members, %d marker fields", len(mi.Fields), len(byGS))
	}
	return "", ""
}

// namedBudgetKeys returns the encodings whose k:<enc> or mi:<enc> key in namespace id is
// in list. They are not checked against is_enc, so naming rl:e:<id>:k:<m> exempts the ix
// member m even when m fails is_enc. keys.BucketPlaceholder (rl:e:<id>:k:!) thus exempts
// only the ix member literally named "!": a test that adds any other member failing
// is_enc declares the ix key it broke, or rl:e:<id>:k:<member>.
func namedBudgetKeys(list []string, id string) map[string]bool {
	kPrefix, miPrefix := keys.BucketKey(id, ""), keys.MarkerIndexKey(id, "")
	out := make(map[string]bool)
	for _, n := range list {
		if enc, ok := strings.CutPrefix(n, kPrefix); ok {
			out[enc] = true
		} else if enc, ok := strings.CutPrefix(n, miPrefix); ok {
			out[enc] = true
		}
	}
	return out
}

// uint53 parses s by is_uint53 (§A4.2): 1 to 16 decimal digits, no leading zero unless s
// is "0", value below 2^53.
func uint53(s string) (int64, bool) {
	if len(s) < 1 || len(s) > 16 || (s[0] == '0' && len(s) > 1) {
		return 0, false
	}
	var n int64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if n >= two53 {
		return 0, false
	}
	return n, true
}

// isUint53 reports is_uint53(s).
func isUint53(s string) bool {
	_, ok := uint53(s)
	return ok
}

// isHex32 reports is_hex32(s) (§A4.2): 32 lowercase hex characters.
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isScore parses s by is_score (§A4.2): read as a number, an integer in [0, 2^53).
func isScore(s string) (int64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != math.Trunc(f) || f < 0 || f >= two53 {
		return 0, false
	}
	return int64(f), true
}

// pair parses "<a>:<b>" with both parts is_uint53: a marker field's "<g>:<s>", a marker
// value's "<expiry_us>:<credit_us>" and an mi member.
func pair(s string) (int64, int64, bool) {
	l, r, ok := strings.Cut(s, ":")
	x, xOK := uint53(l)
	y, yOK := uint53(r)
	return x, y, ok && xOK && yOK
}

// ceilDiv is ⌈a / b⌉ for a ≥ 0 and b > 0. For b ≤ 0, which limits never lets through, it
// returns 0, so a broken policy check shows up as a wrong report, never as a panic.
func ceilDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}

// addSat returns a + b for b ≥ 0, saturating at math.MaxInt64, which no counter equals.
func addSat(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// quote quotes s for a report line, cut to its first 64 bytes.
func quote(s string) string {
	if len(s) > 64 {
		return strconv.Quote(s[:64]) + "…"
	}
	return strconv.Quote(s)
}
