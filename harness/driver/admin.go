//go:build rltest

package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"ratelimiter/harness/fixtures"
	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// The driver's admin support (IMPLEMENTATION_PLAN S8 steps 5 and 10; DESIGN §A6, §C4): rladmin
// runs as a child process (bin/test/rladmin, testutil.RLAdminBin) with an explicit environment
// of RL_STORE_ADDR and the store's RL_*_PASSWORD variables, never the test's own environment,
// and the policy written as TOML into the test's temporary directory. A start-epoch, restore or
// clear that exits 0 writes its op-log records; Restore and Clear add their key to the audit's
// Restored list; the tamper tool writes through the Fixture connection, so its keys are
// declared Damaged. StartEpoch, Status and Inspect still call the scripts directly as rl_admin.

// rladmin's own deadlines (cmd/rladmin/main.go, plan choices): commandTimeout for every
// command, rewriteTimeout for store-rewrite's BGREWRITEAOF and its wait for the rewrite. The
// driver kills a run adminSlack after its subcommand's deadline (adminWait), never while
// rladmin is still inside its own limit (AGENTS.md §6: no inner timeout longer than the outer
// one).
const (
	rladminCommandTimeout = 30 * time.Second
	rladminRewriteTimeout = 10 * time.Minute
	adminSlack            = 15 * time.Second // process start, policy load and exit
)

// adminWait bounds one rladmin run of subcommand sub (plan choice): rladmin's own deadline for
// it plus adminSlack.
func adminWait(sub string) time.Duration {
	if sub == "store-rewrite" {
		return rladminRewriteTimeout + adminSlack
	}
	return rladminCommandTimeout + adminSlack
}

// AdminResult is one rladmin run through the driver.
type AdminResult struct {
	Args []string // the arguments after the binary, as run (--json and --policy added)
	Code int      // the exit code: 0 ok, 2 refused, 3 store or integrity problem, 4 clock_error
	// Stdout and Stderr are the output as rladmin printed it, for the test's own checks (T-ADM-1
	// scans them for passwords, a backstop). A caller never gets an output that holds a password,
	// raw or, once decoded, through JSON escapes: the driver ends the test first
	// (checkNoPassword, on the raw output and on decodedOutput). Its own failure messages show
	// the output redacted (adminFatalf).
	Stdout string // with --json, one JSON object on success
	Stderr string // "rladmin: <message>" on failure
	// JSON is Stdout's one object, numbers kept exact (json.Number); nil when a run that did
	// not exit 0 printed nothing. A value the reply showed as the empty string (missing or
	// unreadable) is nil.
	JSON map[string]any
}

// Int returns the integer field name of the JSON output; ok is false when it is absent, null
// or not an integer.
func (r AdminResult) Int(name string) (int64, bool) {
	n, ok := r.JSON[name].(json.Number)
	if !ok {
		return 0, false
	}
	v, err := n.Int64()
	return v, err == nil
}

// Str returns the string field name of the JSON output, "" when it is absent or no string.
func (r AdminResult) Str(name string) string {
	s, _ := r.JSON[name].(string)
	return s
}

// RLAdmin runs rladmin with args (a subcommand and its flags) against the driver's store, with
// the driver's policy where the subcommand takes one (RLAdminPolicy).
func (d *Driver) RLAdmin(args ...string) AdminResult {
	d.t.Helper()
	return d.rladmin(d.policy, args)
}

// RLAdminPolicy runs rladmin with args against the driver's store (IMPLEMENTATION_PLAN S8 step
// 5): bin/test/rladmin, or $RL_RLADMIN_BIN (testutil.RLAdminBin), with --json added when args
// lack it and, for start-epoch, restore and clear, --policy naming p written as TOML into the
// test's temporary directory (args never name --policy themselves). Its environment is
// RL_STORE_ADDR and the store's RL_*_PASSWORD variables alone (storeproc Env), never
// os.Environ, and it is killed after adminWait. It returns the exit code and the output; an
// output that holds one of the store's passwords ends the test at once however the run ended,
// before any other check and before the caller sees the output (§A6: never print a password;
// checkNoPassword), and so does an output that spells one with JSON escapes such as \u00XX,
// which the raw scan cannot see: right after the raw scan, before anything else reads the
// output, the decoded object included, the driver scans every string a JSON decoder reads
// from it (a duplicate key's first value, a second object, an object before other text) and
// the whole text with its escapes decoded, which covers text that is not JSON
// (decodedOutput); a run that exits 0 without printing
// exactly one JSON object, nothing after it, is a test failure (AGENTS.md §1: every input is
// validated); a failure message that shows the output shows it with each password replaced
// (adminFatalf). A run that exits 0 writes its op-log records: a start-epoch the policy record
// of p and the epoch record {epoch_id, origin_ns = t_us × 1000} (adding p to the epoch
// policies the audit chooses from), a restore its restore record and a clear its clear
// record, each adding the key's k:<enc> in the expected epoch to the audit's Restored list. A
// refused or failed run changed nothing and writes nothing (orchestrator decision, S8 task 3).
// Call it from the test's goroutine: a run that cannot start, or whose --json output does not
// parse, fails the test at once.
func (d *Driver) RLAdminPolicy(p limiter.Policy, args ...string) AdminResult {
	d.t.Helper()
	return d.rladmin(p, args)
}

func (d *Driver) rladmin(p limiter.Policy, args []string) AdminResult {
	d.t.Helper()
	bin, err := testutil.RLAdminBin()
	if err != nil {
		d.t.Fatalf("driver: %v", err)
		return AdminResult{}
	}
	argv := slices.Clone(args)
	sub := ""
	if len(argv) > 0 {
		sub = argv[0]
	}
	if _, given := flagValue(argv, "policy"); given {
		d.t.Fatalf("driver: rladmin %s: pass the policy with RLAdminPolicy, not --policy, so the op log records the one rladmin ran with", sub)
		return AdminResult{}
	}
	if takesPolicy(sub) {
		path, err := d.policyFile(p)
		if err != nil {
			d.t.Fatalf("driver: %v", err)
			return AdminResult{}
		}
		argv = append(argv, "--policy", path)
	}
	if !slices.Contains(argv, "--json") {
		argv = append(argv, "--json")
	}
	wait := adminWait(sub)
	ctx, cancel := context.WithTimeout(d.t.Context(), wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = append([]string{"RL_STORE_ADDR=" + d.st.Addr}, d.st.Env()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r := AdminResult{Args: argv}
	err = cmd.Run()
	r.Stdout, r.Stderr = stdout.String(), stderr.String()
	// On every path, an abnormal exit's too, and before anything else reads the output: the raw
	// output, then the output with its JSON escapes decoded.
	if !d.checkNoPassword("the output of rladmin "+sub, r.Stdout+r.Stderr) {
		return r
	}
	if !d.checkNoPassword("the decoded JSON output of rladmin "+sub, decodedOutput(r.Stdout, r.Stderr)) {
		return r
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		r.Code = exit.ExitCode()
	default:
		d.adminFatalf("driver: rladmin %s did not run to its exit within %v: %v (stderr %q)", sub, wait, err, r.Stderr)
		return r
	}
	switch {
	case strings.TrimSpace(r.Stdout) != "":
		obj, err := oneJSONObject(r.Stdout)
		if err != nil {
			d.adminFatalf("driver: rladmin %s --json printed %q (stderr %q): %v", sub, r.Stdout, r.Stderr, err)
			return r
		}
		r.JSON = obj
	case r.Code == 0:
		d.adminFatalf("driver: rladmin %s exited 0 with no output (stdout %q, stderr %q), want one JSON object", sub, r.Stdout, r.Stderr)
		return r
	}
	if r.Code == 0 {
		d.logAdmin(p, sub, argv, r)
	}
	return r
}

// oneJSONObject decodes s, rladmin's --json output, as exactly one JSON object, its numbers
// kept exact (json.Number), with nothing after it but white space.
func oneJSONObject(s string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if obj == nil {
		return nil, errors.New("not a JSON object: null")
	}
	var more json.RawMessage
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, errors.New("more output after the JSON object")
	}
	return obj, nil
}

// takesPolicy reports whether rladmin's subcommand sub takes --policy (DESIGN §A6).
func takesPolicy(sub string) bool {
	return sub == "start-epoch" || sub == "restore" || sub == "clear"
}

// checkNoPassword reports whether text is free of the store's four passwords, in every form
// redactPasswords replaces. When it is not, it reports "<what> holds the <user> password" for
// each user whose password text holds, never the password, and ends the test at once
// (FailNow), so no caller's diagnostic can show the output (review R2); the caller returns
// right after (recTB's FailNow returns).
func (d *Driver) checkNoPassword(what, text string) bool {
	d.t.Helper()
	clean := true
	for _, u := range passwordUsers(d.st.PW) {
		for _, f := range passwordForms(u.pw) {
			if strings.Contains(text, f) {
				d.t.Errorf("driver: %s holds the %s password", what, u.user)
				clean = false
				break
			}
		}
	}
	if !clean {
		d.t.FailNow()
	}
	return clean
}

// decodedOutput returns rladmin's outputs as a JSON decoder reads them, for checkNoPassword (S8
// review round 2): JSON escapes such as \u00XX can spell a password that the raw output never
// shows, and decoding turns them back into it. For each output it holds every string and
// number token a json.Decoder reads (jsonTokens: every object key and value, a duplicate key's
// too, through every value, up to the first syntax error), and the whole output with every
// JSON escape decoded wherever it stands (unescapeJSON), which also covers text that is not
// JSON, such as a line after the object or an unterminated string. Each is its own line, so
// no match spans two of them.
func decodedOutput(outputs ...string) string {
	var texts []string
	for _, s := range outputs {
		texts = append(texts, jsonTokens(s)...)
		texts = append(texts, unescapeJSON(s))
	}
	return strings.Join(texts, "\n")
}

// jsonTokens returns the string tokens, object keys among them, and the number tokens that a
// json.Decoder reads from s, value after value, until the end of s or the first syntax error.
// Token reports each key as it reads it, so a key given twice shows both of its values, which
// decoding into a map would not.
func jsonTokens(s string) []string {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var out []string
	for {
		tok, err := dec.Token()
		if err != nil { // io.EOF, or text that is not JSON: what was read before it is kept
			return out
		}
		switch v := tok.(type) {
		case string:
			out = append(out, v)
		case json.Number:
			out = append(out, v.String())
		}
	}
}

// unescapeJSON returns s with every JSON string escape decoded wherever it stands, read left to
// right as encoding/json reads a string (RFC 8259 §7): \" \\ \/ \b \f \n \r \t, and \uXXXX,
// where a high surrogate followed by an escaped low surrogate is one character and any other
// surrogate is U+FFFD. So \\u0061 is a backslash and "u0061". A backslash that starts no escape
// stays as it is.
func unescapeJSON(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		if c, ok := shortEscape(s[i+1]); ok {
			b.WriteByte(c)
			i += 2
			continue
		}
		r, ok := escapedUnit(s, i)
		if !ok {
			b.WriteByte(s[i])
			i++
			continue
		}
		i += 6
		if utf16.IsSurrogate(r) {
			if low, ok := escapedUnit(s, i); ok {
				if pair := utf16.DecodeRune(r, low); pair != unicode.ReplacementChar {
					b.WriteRune(pair)
					i += 6
					continue
				}
			}
			r = unicode.ReplacementChar
		}
		b.WriteRune(r)
	}
	return b.String()
}

// shortEscape returns the character that the two-character JSON escape \c stands for.
func shortEscape(c byte) (byte, bool) {
	switch c {
	case '"', '\\', '/':
		return c, true
	case 'b':
		return '\b', true
	case 'f':
		return '\f', true
	case 'n':
		return '\n', true
	case 'r':
		return '\r', true
	case 't':
		return '\t', true
	}
	return 0, false
}

// escapedUnit reads the escape \uXXXX at s[i:], four hex digits of either case, as one UTF-16
// code unit.
func escapedUnit(s string, i int) (rune, bool) {
	if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
		return 0, false
	}
	var r rune
	for _, c := range []byte(s[i+2 : i+6]) {
		var v byte
		switch {
		case '0' <= c && c <= '9':
			v = c - '0'
		case 'a' <= c && c <= 'f':
			v = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			v = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(v)
	}
	return r, true
}

// adminFatalf fails the test with a message that may show rladmin's output (AGENTS.md §1
// Secrets): every form of the store's passwords is replaced in each string argument and again
// in the message, so neither the output nor its quoted rendering shows one, and an argument
// whose JSON escapes spell one is withheld (redactPasswords). checkNoPassword has already
// ended the test on any password the output held, raw or once decoded (decodedOutput).
func (d *Driver) adminFatalf(format string, args ...any) {
	d.t.Helper()
	for i, a := range args {
		if s, ok := a.(string); ok {
			args[i] = redactPasswords(s, d.st.PW)
		}
	}
	d.t.Fatal(redactPasswords(fmt.Sprintf(format, args...), d.st.PW))
}

// passwordUsers pairs the four ACL users with their passwords (DESIGN §A2).
func passwordUsers(pw storeproc.Passwords) []struct{ user, pw string } {
	return []struct{ user, pw string }{{"rl_lib", pw.Lib}, {"rl_admin", pw.Admin}, {"rl_fixture", pw.Fixture}, {"rl_ops", pw.Ops}}
}

// passwordForms returns the forms of pw that output may hold: the raw text, its JSON-escaped
// form and its Go-quoted form (none for an empty password).
func passwordForms(pw string) []string {
	if pw == "" {
		return nil
	}
	forms := []string{pw}
	if b, err := json.Marshal(pw); err == nil && len(b) >= 2 {
		forms = append(forms, string(b[1:len(b)-1]))
	}
	if q := strconv.Quote(pw); len(q) >= 2 {
		forms = append(forms, q[1:len(q)-1])
	}
	return forms
}

// redactPasswords returns text with every form of each password (passwordForms) replaced by
// "<user password>", naming its user; a text in which a replacement re-forms one, or whose
// JSON escapes, once decoded (unescapeJSON), still spell one, is withheld whole.
func redactPasswords(text string, pw storeproc.Passwords) string {
	users := passwordUsers(pw)
	for _, u := range users {
		for _, f := range passwordForms(u.pw) {
			text = strings.ReplaceAll(text, f, "<"+u.user+" password>")
		}
	}
	decoded := unescapeJSON(text)
	for _, u := range users {
		for _, f := range passwordForms(u.pw) {
			if strings.Contains(text, f) || strings.Contains(decoded, f) {
				return "<withheld: it holds a store password>"
			}
		}
	}
	return text
}

// policyFile returns the path of p written as the [policy] table of a TOML file (DESIGN §B2)
// in the test's temporary directory, writing it on first use: one file per fingerprint.
func (d *Driver) policyFile(p limiter.Policy) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.adminDir == "" {
		d.adminDir = d.t.TempDir()
	}
	path := filepath.Join(d.adminDir, "policy-"+fingerprint.Of(p)+".toml")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.WriteFile(path, policyTOML(p), 0o600); err != nil {
		return "", fmt.Errorf("writing the policy file: %w", err)
	}
	return path, nil
}

// policyTOML is p as the [policy] table, every §B2 key given, so rladmin derives exactly p
// and its fingerprint.
func policyTOML(p limiter.Policy) []byte {
	var b bytes.Buffer
	b.WriteString("[policy]\n")
	for _, kv := range []struct {
		k string
		v int64
	}{
		{"rate_tokens", p.RateTokens}, {"rate_period_ns", p.RatePeriodNs}, {"capacity", p.Capacity},
		{"max_tracked_buckets", p.MaxTrackedBuckets}, {"max_live_markers", p.MaxLiveMarkers},
		{"max_markers_per_key", p.MaxMarkersPerKey}, {"receipt_lifetime_ns", p.ReceiptLifetimeNs},
		{"refund_deadline_ns", p.RefundDeadlineNs}, {"outage_tolerance_ns", p.OutageToleranceNs},
		{"cleanup_interval_ns", p.CleanupIntervalNs},
	} {
		b.WriteString(kv.k + " = " + strconv.FormatInt(kv.v, 10) + "\n")
	}
	b.WriteString("failure_mode = " + strconv.Quote(p.FailureMode.String()) + "\n")
	b.WriteString("clock_source = " + strconv.Quote(p.ClockSource.String()) + "\n")
	return b.Bytes()
}

// flagValue returns the value of --name in args ("--name value" or "--name=value") and whether
// it was given.
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == "--"+name && i+1 < len(args) {
			return args[i+1], true
		}
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// logAdmin writes the op-log records of an rladmin run that exited 0 (IMPLEMENTATION_PLAN S8
// step 5): start-epoch, restore and clear; status, inspect and the store commands write none.
func (d *Driver) logAdmin(p limiter.Policy, sub string, argv []string, r AdminResult) {
	d.t.Helper()
	tUs, okT := r.Int("t_us")
	switch sub {
	case "start-epoch":
		id := r.Str("epoch_id")
		if !okT || id == "" {
			d.adminFatalf("driver: rladmin start-epoch exited 0 with output %q, want epoch_id and t_us", r.Stdout)
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.policies = append(d.policies, p)
		d.appendLocked(policyRecordOf(p), epochRecord{Type: "epoch", EpochID: id, OriginNs: tUs * 1000})
	case "restore", "clear":
		key, _ := flagValue(argv, "key")
		expect, _ := flagValue(argv, "expect-epoch")
		enc, err := keys.Encode(key)
		if err != nil || !okT {
			d.adminFatalf("driver: rladmin %s of %q exited 0 with output %q: %v", sub, key, r.Stdout, err)
			return
		}
		var rec any = clearRecord{Type: "clear", KeyB64URL: keyB64URL(key), TUs: tUs}
		if sub == "restore" {
			g, okG := r.Int("g")
			units, err := balanceUnits(argv, p)
			if !okG || err != nil {
				d.adminFatalf("driver: rladmin restore of %q exited 0 with output %q: %v", key, r.Stdout, err)
				return
			}
			rec = restoreRecord{Type: "restore", KeyB64URL: keyB64URL(key), BalanceUnits: units, TUs: tUs, G: g, EpochID: expect}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if k := keys.BucketKey(expect, enc); !slices.Contains(d.restored, k) {
			d.restored = append(d.restored, k)
		}
		d.logged++
		d.appendLocked(rec)
	}
}

// balanceUnits is the balance a restore sent (DESIGN §A6): --balance-units U, --balance-tokens
// T as T × rate_period_ns, or 0 when neither is given.
func balanceUnits(argv []string, p limiter.Policy) (int64, error) {
	if v, ok := flagValue(argv, "balance-units"); ok {
		return strconv.ParseInt(v, 10, 64)
	}
	if v, ok := flagValue(argv, "balance-tokens"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, err
		}
		if n != 0 && p.RatePeriodNs > (1<<63-1)/n {
			return 0, fmt.Errorf("--balance-tokens %d × rate_period_ns overflows", n)
		}
		return n * p.RatePeriodNs, nil
	}
	return 0, nil
}

// currentEpoch returns the epoch ID the store's record names now (read_epoch's field 4,
// returned whenever the record is well-formed, also under a hold, DESIGN §A4.6) and that
// epoch's policy: among the policies the driver started or replaced an epoch with, the one
// whose fingerprint equals the record's fp, else the driver's policy, chosen as the teardown
// audit chooses it (policyFor; IMPLEMENTATION_PLAN S8 step 10). It fails the test, and
// returns "", when the record names no epoch.
func (d *Driver) currentEpoch(what string) (string, limiter.Policy) {
	d.t.Helper()
	e := d.ReadEpoch()
	if e.ID == "" {
		d.t.Fatalf("driver: %s: no readable epoch record (read_epoch %d %q)", what, e.State, e.Reason)
		return "", d.policy
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return e.ID, d.policyFor(e.FP)
}

// ReplaceEpoch replaces the current epoch with a new one of the driver's policy
// (ReplaceEpochWith).
func (d *Driver) ReplaceEpoch() string {
	d.t.Helper()
	return d.ReplaceEpochWith(d.policy)
}

// ReplaceEpochWith runs rladmin start-epoch --replace --expect-epoch <the current ID> with
// policy p (ADM-1; DESIGN §A6) and returns the new epoch's ID; the run writes the policy and
// epoch records (RLAdminPolicy). Anything but exit 0 fails the test.
func (d *Driver) ReplaceEpochWith(p limiter.Policy) string {
	d.t.Helper()
	id, _ := d.currentEpoch("ReplaceEpoch")
	if id == "" {
		return ""
	}
	r := d.RLAdminPolicy(p, "start-epoch", "--replace", "--expect-epoch", id)
	if r.Code != 0 {
		d.adminFatalf("driver: ReplaceEpoch of %s: rladmin exited %d: %s", id, r.Code, r.Stderr)
		return ""
	}
	return r.Str("epoch_id")
}

// Restore runs rladmin restore of key with balanceUnits units, the current epoch's ID as
// --expect-epoch and that epoch's policy (currentEpoch: after ReplaceEpochWith, the policy of
// the replace) (ADM-2), and returns the run: exit 0 wrote the restore record and added the key
// to Restored (RLAdminPolicy).
func (d *Driver) Restore(key string, balanceUnits int64) AdminResult {
	d.t.Helper()
	id, p := d.currentEpoch("Restore")
	if id == "" {
		return AdminResult{}
	}
	return d.RLAdminPolicy(p, "restore", "--key", key, "--balance-units", strconv.FormatInt(balanceUnits, 10), "--expect-epoch", id)
}

// Clear runs rladmin clear of key with the current epoch's ID as --expect-epoch and that
// epoch's policy (currentEpoch) (ADM-3), and returns the run: exit 0 wrote the clear record
// and added the key to Restored.
func (d *Driver) Clear(key string) AdminResult {
	d.t.Helper()
	id, p := d.currentEpoch("Clear")
	if id == "" {
		return AdminResult{}
	}
	return d.RLAdminPolicy(p, "clear", "--key", key, "--expect-epoch", id)
}

// LogInspect runs rladmin inspect of key (ADM-4) and writes the op log's inspect snapshot
// record {key_b64url, present, u, t_us, g, s, x_us, mc} from its output, a value shown empty
// (null) as "" (IMPLEMENTATION_PLAN S8 step 5). Anything but exit 0 fails the test. Only this
// method writes inspect records, so a test controls where its snapshots fall in the op log.
func (d *Driver) LogInspect(key string) AdminResult {
	d.t.Helper()
	r := d.RLAdmin("inspect", "--key", key)
	present, ok := r.Int("present")
	if r.Code != 0 || !ok {
		d.adminFatalf("driver: LogInspect of %q: rladmin exited %d: %s%s", key, r.Code, r.Stdout, r.Stderr)
		return r
	}
	opt := func(name string) any {
		if v, ok := r.Int(name); ok {
			return v
		}
		return ""
	}
	rec := inspectRecord{
		Type: "inspect", KeyB64URL: keyB64URL(key), Present: present,
		U: opt("u"), TUs: opt("t_us"), G: opt("g"), S: opt("s"), XUs: opt("x_us"), MC: opt("mc"),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.appendLocked(rec)
	return r
}

// Tamper returns the state tamper tool (DESIGN §B6, §C4; HARNESS FLT-7, FLT-11) for the epoch
// the store's record names now (read_epoch's ID; "" when the record is unreadable, which the
// epoch-wide operations do not need). It writes through the driver's Fixture connection, so
// every key a tamper write names is declared Damaged for the teardown audit, and a wipe is
// noted, which exempts the test from the BUD-11 check (IMPLEMENTATION_PLAN S8 step 10).
func (d *Driver) Tamper() *fixtures.Tamper {
	d.t.Helper()
	return fixtures.NewTamper(d.Fixture(), d.ReadEpoch().ID)
}
