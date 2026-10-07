//go:build rltest

package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// records returns the op-log records from line from on, decoded with exact numbers.
func records(t *testing.T, d *Driver, from int) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range d.OpLog()[from:] {
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// policyRecordWant is the op log's policy record of p (DESIGN §C4; plan S8 step 5), field by
// field: the settings the reference model judges the epoch by.
func policyRecordWant(p limiter.Policy) map[string]any {
	return map[string]any{
		"type": "policy", "rate_tokens": json.Number(itoa(p.RateTokens)), "rate_period_ns": json.Number(itoa(p.RatePeriodNs)),
		"capacity": json.Number(itoa(p.Capacity)), "max_tracked_buckets": json.Number(itoa(p.MaxTrackedBuckets)),
		"receipt_lifetime_ns": json.Number(itoa(p.ReceiptLifetimeNs)),
	}
}

// epochPairWant is the policy and epoch records an epoch start or replace with p writes: the
// epoch's origin_ns is the controlled time now.
func epochPairWant(d *Driver, p limiter.Policy, id string) []map[string]any {
	return []map[string]any{policyRecordWant(p), {"type": "epoch", "epoch_id": id, "origin_ns": json.Number(itoa(d.Clock().NowNs()))}}
}

// TestDriverAdmin (IMPLEMENTATION_PLAN S8 steps 5 and 10): rladmin runs with the driver's
// explicit environment alone (the test's own RL_STORE_ADDR and RL_ADMIN_USER, pointing
// elsewhere, change nothing) and its --json output parses. ReplaceEpoch writes a policy and
// epoch pair (the driver's settings, field by field) and the instance re-joins; a refused
// restore writes nothing; after a quarantine, a restore of 3 tokens and a clear write their
// records (balance as sent, g and t_us from the output, the expected epoch) and add the key's
// k:<enc> to Restored; LogInspect writes the snapshot of the restored bucket; after a wipe,
// rladmin start-epoch with no flag writes the pair too. ReplaceEpochWith other settings
// (capacity 30) logs those settings, and in that epoch Restore and Clear of a key an instance
// of those settings quarantined both exit 0: they send the policy whose fingerprint the
// record names, not the driver's.
func TestDriverAdmin(t *testing.T) {
	t.Setenv("RL_STORE_ADDR", "127.0.0.1:1")
	t.Setenv("RL_ADMIN_USER", "rl_lib")
	d := New(t, Options{Instances: 1})
	p := d.Policy()
	idA := d.ReadEpoch().ID
	st := d.RLAdmin("status")
	if st.Code != 0 || st.Str("epoch_id") != idA || st.Str("fingerprint") != fingerprint.Of(p) || st.Str("hold") != "" {
		t.Fatalf("rladmin status: exit %d %q %q, want 0 with epoch %s", st.Code, st.Stdout, st.Stderr, idA)
	}
	if !slices.Contains(st.Args, "--json") || slices.Contains(st.Args, "--policy") {
		t.Errorf("status ran with %q, want --json and no --policy", st.Args)
	}

	from := len(d.OpLog())
	idB := d.ReplaceEpoch()
	if idB == "" || idB == idA || d.ReadEpoch().ID != idB {
		t.Fatalf("ReplaceEpoch: %q, store %q; want a new epoch over %s", idB, d.ReadEpoch().ID, idA)
	}
	recs := records(t, d, from)
	if want := epochPairWant(d, p, idB); !jsonEqual(recs, want) {
		t.Errorf("records of ReplaceEpoch:\n%v\nwant\n%v", recs, want)
	}
	if err := d.WaitJoined(0, idB, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	const key = "adm"
	op := d.Acquire(0, key)
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire: %v", op.Kind)
	}
	d.Done(op)
	from = len(d.OpLog())
	if r := d.Restore(key, 0); r.Code != 2 || !strings.Contains(r.Stderr, "not_quarantined") {
		t.Errorf("restore of a key not quarantined: exit %d %q, want 2 not_quarantined", r.Code, r.Stderr)
	}
	if n := len(d.OpLog()); n != from || len(d.restored) != 0 {
		t.Errorf("a refused restore wrote %d records and Restored %v, want none", n-from, d.restored)
	}
	if err := d.Tamper().DeleteIndexMember(key); err != nil {
		t.Fatal(err)
	}
	if op := d.Acquire(0, key); op.Kind != limiter.StateIntegrityError || op.Diag.Reason != "bucket_without_index" {
		t.Fatalf("acquire after the tamper: %v %+v, want state_integrity_error bucket_without_index", op.Kind, op.Diag)
	}
	from = len(d.OpLog())
	units := 3 * p.RatePeriodNs
	r := d.Restore(key, units)
	g, okG := r.Int("g")
	tUs, okT := r.Int("t_us")
	if r.Code != 0 || !okG || !okT {
		t.Fatalf("restore: exit %d %q %q", r.Code, r.Stdout, r.Stderr)
	}
	if in := d.Inspect(key); in.U != units || in.G != g || in.S != 0 || in.T != tUs || in.Quarantine == "" {
		t.Errorf("after the restore: %+v, want u %d, g %d, s 0, t %d, still quarantined", in, units, g, tUs)
	}
	c := d.Clear(key)
	cUs, okC := c.Int("t_us")
	if c.Code != 0 || !okC {
		t.Fatalf("clear: exit %d %q %q", c.Code, c.Stdout, c.Stderr)
	}
	snap := d.LogInspect(key)
	if v, _ := snap.Int("u"); v != units || snap.Str("quarantine") != "" {
		t.Errorf("LogInspect after the clear: %q, want u %d and no quarantine", snap.Stdout, units)
	}
	b64 := keyB64URL(key)
	recs = records(t, d, from)
	want := []map[string]any{
		{"type": "restore", "key_b64url": b64, "balance_units": json.Number(itoa(units)), "t_us": json.Number(itoa(tUs)), "g": json.Number(itoa(g)), "epoch_id": idB},
		{"type": "clear", "key_b64url": b64, "t_us": json.Number(itoa(cUs))},
		{"type": "inspect", "key_b64url": b64, "present": json.Number("1"), "u": json.Number(itoa(units)), "t_us": json.Number(itoa(tUs)),
			"g": json.Number(itoa(g)), "s": json.Number("0"), "x_us": json.Number(itoa(tUs + p.ReceiptLifetimeNs/1000)), "mc": json.Number("0")},
	}
	if !jsonEqual(recs, want) {
		t.Errorf("records of restore, clear and LogInspect:\n%v\nwant\n%v", recs, want)
	}
	enc, err := keys.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.restored, []string{keys.BucketKey(idB, enc)}) {
		t.Errorf("Restored %v, want the key's k:<enc> in epoch %s, once", d.restored, idB)
	}

	if err := d.Tamper().Wipe(); err != nil {
		t.Fatal(err)
	}
	if err := d.Clock().Set(d.Clock().NowNs()); err != nil { // a wipe deletes rl:ctl:time too
		t.Fatal(err)
	}
	from = len(d.OpLog())
	s := d.RLAdmin("start-epoch")
	idC := s.Str("epoch_id")
	if s.Code != 0 || idC == "" || !slices.Contains(s.Args, "--policy") {
		t.Fatalf("start-epoch after the wipe: exit %d %q %q (args %q)", s.Code, s.Stdout, s.Stderr, s.Args)
	}
	recs = records(t, d, from)
	if want := epochPairWant(d, p, idC); !jsonEqual(recs, want) {
		t.Errorf("records of start-epoch after the wipe:\n%v\nwant\n%v", recs, want)
	}

	p2 := p
	p2.Capacity = 30
	from = len(d.OpLog())
	idD := d.ReplaceEpochWith(p2)
	recs = records(t, d, from)
	if want := epochPairWant(d, p2, idD); !jsonEqual(recs, want) || recs[0]["capacity"] != json.Number("30") {
		t.Errorf("records of ReplaceEpochWith(capacity 30):\n%v\nwant\n%v", recs, want)
	}
	ni, err := d.AddInstance(InstanceSpec{Policy: &p2})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.WaitJoined(ni, idD, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	const key2 = "adm-capacity-30"
	op = d.Acquire(ni, key2)
	if op.Kind != limiter.Allowed {
		t.Fatalf("acquire in the capacity-30 epoch: %v", op.Kind)
	}
	d.Done(op)
	if err := d.Tamper().DeleteIndexMember(key2); err != nil {
		t.Fatal(err)
	}
	if op := d.Acquire(ni, key2); op.Kind != limiter.StateIntegrityError || op.Diag.Reason != "bucket_without_index" {
		t.Fatalf("acquire after the tamper in the capacity-30 epoch: %v %+v, want state_integrity_error bucket_without_index", op.Kind, op.Diag)
	}
	if r := d.Restore(key2, 0); r.Code != 0 {
		t.Errorf("restore in the capacity-30 epoch: exit %d %q %q, want 0", r.Code, r.Stdout, r.Stderr)
	}
	if c := d.Clear(key2); c.Code != 0 {
		t.Errorf("clear in the capacity-30 epoch: exit %d %q %q, want 0", c.Code, c.Stdout, c.Stderr)
	}
}

// TestDriverAdminChecks (IMPLEMENTATION_PLAN S8 step 5; AGENTS.md §1, §5 and §6: the driver's
// checks of an rladmin run have their must-fail fixtures). Each run is bounded by rladmin's own
// deadline for its subcommand plus slack: more than 30 s for every command and more than
// 10 min for store-rewrite (cmd/rladmin's commandTimeout and rewriteTimeout). Through fake
// rladmin binaries (RL_RLADMIN_BIN: #!/bin/sh scripts in the test's temporary directory) run
// by a driver on a recTB: one JSON object alone passes and parses; a run that exits 0 with text
// after its object, a second object, null or no output at all fails the test with the raw
// output, and a run killed by a signal fails it with its stderr; an output that holds the
// rl_admin password ($RL_ADMIN_PASSWORD, from the child's explicit environment) on stdout, or
// the rl_ops password on stderr of a refused run, is reported naming the user, never the
// password, and ends the test at once (FailNow) with no other report, also when that output is
// malformed or the run was killed by a signal (review R2). While the recTB's Fatal methods end
// the calling goroutine, as testing.T's do, such a run stops at the driver's report: the
// caller's code after it, a diagnostic that would show the raw output, never runs, and no
// report holds the password. No run of status writes an op-log record, and the driver's
// teardown reports nothing. redactPasswords replaces each user's password in its raw,
// JSON-escaped and Go-quoted forms, and adminFatalf shows output with each password replaced,
// as is and Go-quoted.
func TestDriverAdminChecks(t *testing.T) {
	pws := storeproc.Passwords{Lib: `l"ib`, Admin: `ad\min`, Fixture: "fix<ture", Ops: "9f9f9f"}
	text := `l"ib l\"ib ad\min ad\\min fix<ture fix\u003cture 9f9f9f`
	if got, want := redactPasswords(text, pws), "<rl_lib password> <rl_lib password> <rl_admin password> <rl_admin password> "+
		"<rl_fixture password> <rl_fixture password> <rl_ops password>"; got != want {
		t.Errorf("redactPasswords(%q) = %q, want %q", text, got, want)
	}

	for _, c := range []struct {
		sub  string
		over time.Duration // rladmin's own deadline for sub
	}{{"status", 30 * time.Second}, {"restore", 30 * time.Second}, {"store-info", 30 * time.Second}, {"store-rewrite", 10 * time.Minute}} {
		if w := adminWait(c.sub); w <= c.over {
			t.Errorf("adminWait(%q) = %v, want more than rladmin's own deadline %v", c.sub, w, c.over)
		}
	}

	f := &recTB{TB: t}
	seenErrs, seenFatals := 0, 0
	t.Cleanup(func() { // registered before New, so it runs after the driver's teardown
		errs, fatals := f.reports()
		if len(errs) != seenErrs || len(fatals) != seenFatals {
			t.Errorf("the driver's teardown reported %q %q, want nothing", errs[seenErrs:], fatals[seenFatals:])
		}
	})
	d := New(f, Options{NoEpoch: true})
	pw := d.Store().PW
	holdsPassword := func(reps []string) bool {
		for _, rep := range reps {
			for _, secret := range []string{pw.Lib, pw.Admin, pw.Fixture, pw.Ops} {
				if strings.Contains(rep, secret) {
					return true
				}
			}
		}
		return false
	}
	dir := t.TempDir()
	for i, c := range []struct {
		name, script string
		errs, fatals []string // the substrings of the run's one error or fatal report; nil: none
	}{
		{"one object", `printf '%s\n' '{"epoch_id":"e1"}'`, nil, nil},
		{"text after the object", `printf '%s\n' '{"epoch_id":"e1"}' 'rladmin: a line that is not JSON'`, nil,
			[]string{"rladmin status --json printed", "a line that is not JSON", "more output after the JSON object"}},
		{"a second object", `printf '%s\n' '{"epoch_id":"e1"}' '{"epoch_id":"e2"}'`, nil,
			[]string{"rladmin status --json printed", `\"e2\"`, "more output after the JSON object"}},
		{"null", `printf 'null\n'`, nil, []string{"rladmin status --json printed", "not a JSON object: null"}},
		{"no output", `exit 0`, nil, []string{"rladmin status exited 0 with no output", "want one JSON object"}},
		{"the rl_admin password on stdout", `printf '{"echo":"%s"}\n' "$RL_ADMIN_PASSWORD"`,
			[]string{"the output of rladmin status holds the rl_admin password"}, []string{"FailNow"}},
		{"the rl_ops password on stderr", `printf 'rladmin: %s\n' "$RL_OPS_PASSWORD" >&2; exit 2`,
			[]string{"the output of rladmin status holds the rl_ops password"}, []string{"FailNow"}},
		{"malformed output holding the rl_admin password", `printf 'not json %s\n' "$RL_ADMIN_PASSWORD"`,
			[]string{"the output of rladmin status holds the rl_admin password"}, []string{"FailNow"}},
		{"an abnormal exit holding the rl_admin password", `printf 'rladmin: %s\n' "$RL_ADMIN_PASSWORD" >&2; kill -KILL $$`,
			[]string{"the output of rladmin status holds the rl_admin password"}, []string{"FailNow"}},
		{"an abnormal exit", `printf 'rladmin: dying\n' >&2; kill -KILL $$`, nil,
			[]string{"rladmin status did not run to its exit", "signal: killed", "rladmin: dying"}},
	} {
		bin := filepath.Join(dir, "rladmin-"+strconv.Itoa(i))
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+c.script+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("RL_RLADMIN_BIN", bin)
		from := len(d.OpLog())
		r := d.RLAdmin("status")
		errs, fatals := f.reports()
		newErrs, newFatals := errs[seenErrs:], fatals[seenFatals:]
		seenErrs, seenFatals = len(errs), len(fatals)
		for _, rc := range []struct {
			kind      string
			got, want []string
		}{{"errors", newErrs, c.errs}, {"fatals", newFatals, c.fatals}} {
			switch {
			case rc.want == nil && len(rc.got) != 0:
				t.Errorf("%s: %s %q, want none", c.name, rc.kind, rc.got)
			case rc.want != nil && len(rc.got) != 1:
				t.Errorf("%s: %s %q, want one holding %q", c.name, rc.kind, rc.got, rc.want)
			case rc.want != nil:
				for _, w := range rc.want {
					if !strings.Contains(rc.got[0], w) {
						t.Errorf("%s: %s report %q lacks %q", c.name, rc.kind, rc.got[0], w)
					}
				}
			}
		}
		if holdsPassword(append(newErrs, newFatals...)) {
			t.Errorf("%s: a report holds a password", c.name)
		}
		if n := len(d.OpLog()); n != from {
			t.Errorf("%s: %d op-log records, want none (status writes none)", c.name, n-from)
		}
		if c.errs == nil && c.fatals == nil && (r.Code != 0 || r.Str("epoch_id") != "e1") {
			t.Errorf("%s: exit %d, JSON %v; want 0 and epoch_id e1", c.name, r.Code, r.JSON)
		}
	}

	// No run above reaches adminFatalf with a password in its output, so its replacement is
	// checked directly.
	d.adminFatalf("driver: rladmin status printed %s (stderr %q)", "not json "+pw.Admin, "rladmin: "+pw.Ops+"\n")
	_, fatals := f.reports()
	newFatals := fatals[seenFatals:]
	seenFatals = len(fatals)
	if holdsPassword(newFatals) {
		t.Error("adminFatalf: a report holds a password")
	} else if len(newFatals) != 1 || !strings.Contains(newFatals[0], "printed not json <rl_admin password> (stderr \"rladmin: <rl_ops password>\\n\")") {
		t.Errorf("adminFatalf: fatals %q, want one showing the output with each password replaced", newFatals)
	}

	// The run goes on its own goroutine so f's FailNow can end it, as testing.T's ends a test.
	bin := filepath.Join(dir, "rladmin-exit")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '{\"echo\":\"%s\"}\\n' \"$RL_ADMIN_PASSWORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RL_RLADMIN_BIN", bin)
	callerRan := false
	stopped := make(chan struct{})
	f.setExit(true)
	go func() {
		defer close(stopped)
		r := d.RLAdmin("status")
		callerRan = true
		f.Errorf("caller: rladmin status exited %d: %s%s", r.Code, r.Stdout, r.Stderr) // as a calling test's diagnostic
	}()
	select {
	case <-stopped:
		f.setExit(false)
	case <-time.After(adminWait("status") + adminSlack):
		f.setExit(false)
		t.Fatalf("a run whose output holds the rl_admin password did not end within %v", adminWait("status")+adminSlack)
	}
	errs, fatals := f.reports()
	newErrs, newFatals := errs[seenErrs:], fatals[seenFatals:]
	seenErrs, seenFatals = len(errs), len(fatals)
	if callerRan {
		t.Error("the caller's code ran after a run whose output holds the rl_admin password, want the run to end at the driver's report")
	}
	if holdsPassword(append(newErrs, newFatals...)) {
		t.Error("a report of the run whose output holds the rl_admin password holds a password")
	} else if len(newErrs) != 1 || !strings.Contains(newErrs[0], "the output of rladmin status holds the rl_admin password") || !slices.Equal(newFatals, []string{"FailNow"}) {
		t.Errorf("the run whose output holds the rl_admin password reported %q %q, want the rl_admin password report, then FailNow", newErrs, newFatals)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// jsonEqual compares decoded records by their JSON encoding.
func jsonEqual(a, b []map[string]any) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && string(x) == string(y)
}
