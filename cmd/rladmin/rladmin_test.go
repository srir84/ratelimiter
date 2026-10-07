package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
	"ratelimiter/limiter"
)

// Test values, all literals (AGENTS.md §5).
const (
	testPW  = "pw-Zq9!kV2-secret-for-rladmin-tests-0F3e"
	epochA  = "0123456789abcdef0123456789abcdef"
	epochB  = "fedcba9876543210fedcba9876543210"
	fpA     = "00112233445566778899aabbccddeeff"
	keyEnc  = "YWxpY2U" // base64url("alice") without padding
	two53   = 9007199254740992
	tbound  = 8998403161718784 // 2^53 − 2^43 (§A3.1)
	tSample = 1760000000000000 // a budget time in µs
)

// policyTOML is a shared config file: the [policy] keys the tests need (the two required
// ones) and tables rladmin ignores.
const policyTOML = `[policy]
max_tracked_buckets = 100_000
max_live_markers = 50_000

[instance]
max_release_delay_ns = 5_000_000_000
`

// defaultLines are fingerprint lines of the default policy with max_tracked_buckets 100,000
// and max_live_markers 50,000, written by hand from DESIGN §A5.9 and §B2: sorted name=value
// lines plus store_schema_version=1 and time_origin=unix_us.
const defaultLines = "capacity=20\ncleanup_interval_ns=5000000000\nclock_source=store\nfailure_mode=closed\n" +
	"max_live_markers=50000\nmax_markers_per_key=1000\nmax_tracked_buckets=100000\n" +
	"outage_tolerance_ns=5000000000\nrate_period_ns=1000000000\nrate_tokens=10\n" +
	"receipt_lifetime_ns=60000000000\nrefund_deadline_ns=30000000000\nstore_schema_version=1\ntime_origin=unix_us\n"

// fpOf is the fingerprint of lines: the first 32 hex characters of their SHA-256 (§A5.9).
func fpOf(lines string) string {
	sum := sha256.Sum256([]byte(lines))
	return hex.EncodeToString(sum[:16])
}

func ival(n int64) resp.Value        { return resp.Value{Kind: resp.Int, Int: n} }
func bulk(s string) resp.Value       { return resp.Value{Kind: resp.Bulk, Str: s} }
func arr(e ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: e} }

// readEpochReply is a healthy read_epoch reply naming id, with gen as given.
func readEpochReply(id string, gen int64) resp.Value {
	return arr(ival(0), ival(0), bulk(""), bulk(id), bulk(fpA), ival(tSample), ival(gen), ival(tSample), ival(3), ival(1), bulk(""))
}

// statusReply is admin_status's OK reply for id, with nb as given.
func statusReply(id string, nb int64) resp.Value {
	return arr(ival(40), ival(0), bulk(""), bulk(id), bulk(fpA), bulk(""), ival(tSample), ival(nb), ival(1), ival(0))
}

// inspectReply is admin_inspect's OK reply for a present key with u as given and one marker.
func inspectReply(u int64) resp.Value {
	return arr(ival(40), ival(0), bulk(""), ival(1), ival(u), ival(tSample), ival(2), ival(1), ival(tSample+60_000_000),
		ival(1), ival(tSample+60_000_000), bulk(""), ival(1), ival(tSample+60_000_000))
}

func refusal(code int64, reason string) resp.Value { return arr(ival(code), ival(0), bulk(reason)) }

// fixed answers every call of a script with v.
func fixed(v resp.Value) fakeresp.Handler { return func(_, _ []string) resp.Value { return v } }

// rlEnv is a fake store as rl_admin with testPW, the environment that reaches it, and a
// policy file.
type rlEnv struct {
	srv    *fakeresp.Server
	env    map[string]string
	policy string
}

func newRLEnv(t *testing.T) *rlEnv {
	t.Helper()
	srv := fakeresp.Start(t, "rl_admin", testPW)
	path := filepath.Join(t.TempDir(), "shared.toml")
	if err := os.WriteFile(path, []byte(policyTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	return &rlEnv{srv: srv, env: map[string]string{"RL_STORE_ADDR": srv.Addr(), "RL_ADMIN_PASSWORD": testPW}, policy: path}
}

// handle makes s known to the fake store and answers it with h.
func (e *rlEnv) handle(s scripts.Script, h fakeresp.Handler) {
	e.srv.Load(s.Text)
	e.srv.Handle(s.SHA, h)
}

// run runs rladmin with args in e's environment.
func (e *rlEnv) run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runWith(t, e.env, args...)
}

func runWith(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	lookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	code = run(t.Context(), args, lookup, &out, &errOut)
	return code, out.String(), errOut.String()
}

// evalshaArgs returns the arguments of every EVALSHA of s the fake store received.
func (e *rlEnv) evalshaArgs(s scripts.Script) [][]string {
	var out [][]string
	for _, c := range e.srv.Commands() {
		if len(c.Args) > 1 && strings.EqualFold(c.Args[0], "EVALSHA") && c.Args[1] == s.SHA {
			out = append(out, c.Args)
		}
	}
	return out
}

// TestExitCodes (DESIGN §A6; plan S8 steps 3 and 11): every reply kind of the admin scripts
// maps to its exit code — 40 → 0, 41 → 2, 20/22/25, an error reply and "epoch changed during
// read" → 3, 21 → 4 — and every reply that fails a §A4.9 range check exits 3 with the raw reply
// printed, while the same reply at its bound exits 0. Both sides of each bound are pinned.
// status and inspect take the expected ID from read_epoch (also under a hold), re-read and
// retry exactly once on epoch_changed, and show each reply element in its own field; the
// address, user and required flags come as DESIGN §A6 says, and a lost connection exits 3.
func TestExitCodes(t *testing.T) {
	restoreArgs := func(e *rlEnv) []string {
		return []string{"restore", "--policy", e.policy, "--key", "alice", "--expect-epoch", epochA}
	}
	clearArgs := func(e *rlEnv) []string {
		return []string{"clear", "--policy", e.policy, "--key", "alice", "--expect-epoch", epochA}
	}
	startArgs := func(e *rlEnv) []string { return []string{"start-epoch", "--policy", e.policy} }
	statusArgs := func(*rlEnv) []string { return []string{"status", "--json"} }
	inspectArgs := func(*rlEnv) []string { return []string{"inspect", "--key", "alice", "--json"} }
	startOK := func(t int64) fakeresp.Handler {
		return func(_, argv []string) resp.Value { return arr(ival(40), ival(t), bulk(""), bulk(argv[0])) }
	}
	healthy := func(e *rlEnv) { e.handle(scripts.ReadEpoch, fixed(readEpochReply(epochA, 7))) }
	const decodeFail = "reply fails the §A4.9 checks" // a layout or range failure (plan S8 step 11)

	cases := []struct {
		name  string
		setup func(e *rlEnv)
		args  func(e *rlEnv) []string
		code  int
		// want are texts the output must hold: stdout for exit 0, stderr otherwise.
		want []string
	}{
		{"restore 40", func(e *rlEnv) { e.handle(scripts.AdminRestore, fixed(arr(ival(40), ival(tSample), bulk(""), ival(5)))) },
			restoreArgs, 0, []string{"g", "5", "t_us", "1760000000000000"}},
		{"restore 41", func(e *rlEnv) { e.handle(scripts.AdminRestore, fixed(refusal(41, "not_quarantined"))) },
			restoreArgs, 2, []string{"not_quarantined"}},
		{"restore 20", func(e *rlEnv) { e.handle(scripts.AdminRestore, fixed(refusal(20, "fingerprint_mismatch"))) },
			restoreArgs, 3, []string{decodeFail, "unknown code 20", "raw reply: [20 0 \"fingerprint_mismatch\"]"}},
		{"restore 22", func(e *rlEnv) { e.handle(scripts.AdminRestore, fixed(refusal(22, "integrity_hold"))) },
			restoreArgs, 3, []string{"integrity_hold"}},
		{"restore 25", func(e *rlEnv) { e.handle(scripts.AdminRestore, fixed(refusal(25, "keys_mismatch"))) },
			restoreArgs, 3, []string{"keys_mismatch"}},
		{"restore 21", func(e *rlEnv) { e.handle(scripts.AdminRestore, fixed(refusal(21, "clock"))) },
			restoreArgs, 4, []string{"clock_error"}},
		{"restore error reply", func(e *rlEnv) {
			e.handle(scripts.AdminRestore, fixed(refusal(40, "unused")))
			e.srv.ReplyError("EVALSHA", "ERR boom in the store", 1)
		}, restoreArgs, 3, []string{"store error reply: ERR boom in the store"}},
		{"clear 40", func(e *rlEnv) { e.handle(scripts.AdminClear, fixed(arr(ival(40), ival(tSample), bulk(""), ival(0)))) },
			clearArgs, 0, []string{"t_us", "1760000000000000"}},
		{"clear 41", func(e *rlEnv) { e.handle(scripts.AdminClear, fixed(refusal(41, "bucket_missing"))) },
			clearArgs, 2, []string{"bucket_missing"}},
		{"clear 22", func(e *rlEnv) { e.handle(scripts.AdminClear, fixed(refusal(22, "integrity_hold"))) },
			clearArgs, 3, []string{"integrity_hold"}},
		{"clear 21", func(e *rlEnv) { e.handle(scripts.AdminClear, fixed(refusal(21, "clock"))) },
			clearArgs, 4, []string{"clock_error"}},
		{"start-epoch 41", func(e *rlEnv) { e.handle(scripts.AdminStartEpoch, fixed(refusal(41, "epoch_exists"))) },
			startArgs, 2, []string{"epoch_exists"}},
		{"start-epoch 21", func(e *rlEnv) { e.handle(scripts.AdminStartEpoch, fixed(refusal(21, "clock"))) },
			startArgs, 4, []string{"clock_error"}},
		{"start-epoch error reply", func(e *rlEnv) {
			e.handle(scripts.AdminStartEpoch, startOK(tSample))
			e.srv.ReplyError("EVALSHA", "OOM command not allowed", 1)
		}, startArgs, 3, []string{"store error reply: OOM"}},
		{"status epoch_record_missing", func(e *rlEnv) {
			e.handle(scripts.ReadEpoch, fixed(arr(ival(22), ival(0), bulk("epoch_record_missing"), bulk(""), bulk(""),
				ival(0), ival(0), ival(0), ival(0), ival(0), bulk(""))))
			e.handle(scripts.AdminStatus, fixed(refusal(41, "epoch_record_missing")))
		}, statusArgs, 2, []string{"epoch_record_missing", "rladmin start-epoch --policy FILE", "without --replace"}},
		{"status epoch_record_malformed", func(e *rlEnv) {
			e.handle(scripts.ReadEpoch, fixed(arr(ival(22), ival(0), bulk("epoch_record_malformed"), bulk(""), bulk(""),
				ival(0), ival(0), ival(0), ival(0), ival(0), bulk(""))))
			e.handle(scripts.AdminStatus, fixed(refusal(41, "epoch_record_malformed")))
		}, statusArgs, 2, []string{"epoch_record_malformed", "rladmin start-epoch --policy FILE", "without --replace"}},
		{"inspect 25", func(e *rlEnv) { healthy(e); e.handle(scripts.AdminInspect, fixed(refusal(25, "keys_mismatch"))) },
			inspectArgs, 3, []string{"keys_mismatch"}},
		{"inspect 41", func(e *rlEnv) { healthy(e); e.handle(scripts.AdminInspect, fixed(refusal(41, "shared_key_malformed"))) },
			inspectArgs, 2, []string{"shared_key_malformed"}},

		// Range checks (§A4.9): one integer past its bound per reply kind, then at its bound.
		{"read_epoch gen 2^53", func(e *rlEnv) {
			e.handle(scripts.ReadEpoch, fixed(readEpochReply(epochA, two53)))
			e.handle(scripts.AdminStatus, fixed(statusReply(epochA, 3)))
		}, statusArgs, 3, []string{"read_epoch", decodeFail, "element 6 = 9007199254740992", "raw reply: [0 0 \"\" \"" + epochA + "\""}},
		{"read_epoch gen 2^53-1", func(e *rlEnv) {
			e.handle(scripts.ReadEpoch, fixed(readEpochReply(epochA, two53-1)))
			e.handle(scripts.AdminStatus, fixed(statusReply(epochA, 3)))
		}, statusArgs, 0, []string{`"epoch_id":"` + epochA + `"`}},
		{"admin_status nb 2^53", func(e *rlEnv) { healthy(e); e.handle(scripts.AdminStatus, fixed(statusReply(epochA, two53))) },
			statusArgs, 3, []string{"admin_status", decodeFail, "element 7 = 9007199254740992", "raw reply: [40 0 \"\" \"" + epochA + "\"", " 9007199254740992 "}},
		{"admin_status nb 2^53-1", func(e *rlEnv) { healthy(e); e.handle(scripts.AdminStatus, fixed(statusReply(epochA, two53-1))) },
			statusArgs, 0, []string{`"tracked_buckets":9007199254740991`}},
		// A stored hwm is shown as it is, also at or above TBOUND (§A4.9), up to 2^53 − 1.
		{"admin_status hwm TBOUND", func(e *rlEnv) {
			healthy(e)
			e.handle(scripts.AdminStatus, fixed(arr(ival(40), ival(0), bulk(""), bulk(epochA), bulk(fpA), bulk(""), ival(tbound), ival(1), ival(1), ival(0))))
		}, statusArgs, 0, []string{`"hwm_us":8998403161718784`}},
		{"admin_status hwm 2^53-1", func(e *rlEnv) {
			healthy(e)
			e.handle(scripts.AdminStatus, fixed(arr(ival(40), ival(0), bulk(""), bulk(epochA), bulk(fpA), bulk(""), ival(two53-1), ival(1), ival(1), ival(0))))
		}, statusArgs, 0, []string{`"hwm_us":9007199254740991`}},
		{"admin_status hwm missing", func(e *rlEnv) {
			healthy(e)
			e.handle(scripts.AdminStatus, fixed(arr(ival(40), ival(0), bulk(""), bulk(epochA), bulk(fpA), bulk("integrity_hold"), bulk(""), ival(1), ival(1), ival(0))))
		}, statusArgs, 0, []string{`"hold":"integrity_hold","hwm_us":null`}},
		{"admin_status hwm 2^53", func(e *rlEnv) {
			healthy(e)
			e.handle(scripts.AdminStatus, fixed(arr(ival(40), ival(0), bulk(""), bulk(epochA), bulk(fpA), bulk(""), ival(two53), ival(1), ival(1), ival(0))))
		}, statusArgs, 3, []string{decodeFail, "element 6 = 9007199254740992"}},
		{"admin_start_epoch t TBOUND", func(e *rlEnv) { e.handle(scripts.AdminStartEpoch, startOK(tbound)) },
			startArgs, 3, []string{"admin_start_epoch", decodeFail, "t_us = 8998403161718784", "raw reply: [40 8998403161718784 \"\" \""}},
		{"admin_start_epoch t TBOUND-1", func(e *rlEnv) { e.handle(scripts.AdminStartEpoch, startOK(tbound-1)) },
			startArgs, 0, []string{"t_us", "8998403161718783"}},
		{"admin_restore g -1", func(e *rlEnv) {
			e.handle(scripts.AdminRestore, fixed(arr(ival(40), ival(tSample), bulk(""), ival(-1))))
		},
			restoreArgs, 3, []string{"admin_restore", decodeFail, "element 3 = -1", "raw reply: [40 1760000000000000 \"\" -1]"}},
		{"admin_restore g 0", func(e *rlEnv) { e.handle(scripts.AdminRestore, fixed(arr(ival(40), ival(tSample), bulk(""), ival(0)))) },
			restoreArgs, 0, []string{"g", "t_us"}},
		{"admin_inspect u 2^53", func(e *rlEnv) { healthy(e); e.handle(scripts.AdminInspect, fixed(inspectReply(two53))) },
			inspectArgs, 3, []string{"admin_inspect", decodeFail, "element 4 = 9007199254740992", "raw reply: [40 0 \"\" 1 9007199254740992 "}},
		{"admin_inspect u 2^53-1", func(e *rlEnv) { healthy(e); e.handle(scripts.AdminInspect, fixed(inspectReply(two53-1))) },
			inspectArgs, 0, []string{`"u":9007199254740991`}},
		// An expiry the reply shows as '' (it fails is_score, §A4.9) is null in marker_expiries.
		{"admin_inspect marker_expiries", func(e *rlEnv) {
			healthy(e)
			e.handle(scripts.AdminInspect, fixed(arr(ival(40), ival(0), bulk(""), ival(1), ival(5), ival(tSample), ival(2), ival(1),
				ival(tSample+60_000_000), ival(1), ival(tSample+60_000_000), bulk(""), ival(2), ival(tSample+60_000_000), bulk(""))))
		}, inspectArgs, 0, []string{`"marker_expiries":[1760000060000000,null]`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRLEnv(t)
			c.setup(e)
			code, stdout, stderr := e.run(t, c.args(e)...)
			if code != c.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, c.code, stdout, stderr)
			}
			got := stderr
			if c.code == 0 {
				got = stdout
				if stderr != "" {
					t.Errorf("stderr on exit 0:\n%s", stderr)
				}
			} else if stdout != "" {
				t.Errorf("stdout on exit %d:\n%s", c.code, stdout)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("output lacks %q:\n%s", w, got)
				}
			}
		})
	}

	// Epoch changed once: status and inspect read the epoch again and retry once, with the new
	// ID, and succeed.
	t.Run("status epoch changed once", func(t *testing.T) {
		e := newRLEnv(t)
		reads := 0
		e.handle(scripts.ReadEpoch, func(_, _ []string) resp.Value {
			reads++
			if reads == 1 {
				return readEpochReply(epochA, 7)
			}
			return readEpochReply(epochB, 0)
		})
		e.handle(scripts.AdminStatus, func(_, argv []string) resp.Value {
			if argv[0] != epochB {
				return refusal(41, "epoch_changed")
			}
			return statusReply(epochB, 0)
		})
		code, stdout, stderr := e.run(t, "status", "--json")
		if code != 0 || !strings.Contains(stdout, `"epoch_id":"`+epochB+`"`) {
			t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and epoch %s", code, stdout, stderr, epochB)
		}
		st := e.evalshaArgs(scripts.AdminStatus)
		if len(st) != 2 || st[0][5] != epochA || st[1][5] != epochB {
			t.Errorf("admin_status calls %v, want two: with %s, then with %s", st, epochA, epochB)
		}
		if st[1][4] != "rl:e:"+epochB+":q" {
			t.Errorf("the retry's q is %s, want rl:e:%s:q", st[1][4], epochB)
		}
	})
	t.Run("inspect epoch changed once", func(t *testing.T) {
		e := newRLEnv(t)
		reads := 0
		e.handle(scripts.ReadEpoch, func(_, _ []string) resp.Value {
			reads++
			if reads == 1 {
				return readEpochReply(epochA, 7)
			}
			return readEpochReply(epochB, 0)
		})
		e.handle(scripts.AdminInspect, func(_, argv []string) resp.Value {
			if argv[0] != epochB {
				return refusal(41, "epoch_changed")
			}
			return inspectReply(5)
		})
		code, stdout, stderr := e.run(t, "inspect", "--key", "alice", "--json")
		if code != 0 || !strings.Contains(stdout, `"u":5,`) {
			t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and u 5", code, stdout, stderr)
		}
		inspectCall := func(id string) []string {
			ns := "rl:e:" + id + ":"
			return []string{"EVALSHA", scripts.AdminInspect.SHA, "5", "rl:epoch", ns + "ix", ns + "q", ns + "k:" + keyEnc, ns + "mi:" + keyEnc, id, keyEnc}
		}
		in := e.evalshaArgs(scripts.AdminInspect)
		if len(in) != 2 || !slices.Equal(in[0], inspectCall(epochA)) || !slices.Equal(in[1], inspectCall(epochB)) {
			t.Errorf("admin_inspect calls\n  %q\nwant two:\n  %q\n  %q", in, inspectCall(epochA), inspectCall(epochB))
		}
		if n := len(e.evalshaArgs(scripts.ReadEpoch)); n != 2 {
			t.Errorf("%d read_epoch calls, want 2", n)
		}
	})
	// Epoch changed twice: one re-read and one retry, then exit 3 (§A4.6), so exactly two
	// read_epoch and two admin_status or admin_inspect calls.
	for _, c := range []struct {
		name string
		s    scripts.Script
		args []string
	}{
		{"status epoch changed twice", scripts.AdminStatus, []string{"status", "--json"}},
		{"inspect epoch changed twice", scripts.AdminInspect, []string{"inspect", "--key", "alice", "--json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newRLEnv(t)
			healthy(e)
			e.handle(c.s, fixed(refusal(41, "epoch_changed")))
			code, stdout, stderr := e.run(t, c.args...)
			if code != 3 || stdout != "" || !strings.Contains(stderr, "epoch changed during read") {
				t.Fatalf("exit %d, stdout %q, want 3 and \"epoch changed during read\"; stderr:\n%s", code, stdout, stderr)
			}
			if r, n := len(e.evalshaArgs(scripts.ReadEpoch)), len(e.evalshaArgs(c.s)); r != 2 || n != 2 {
				t.Errorf("%d read_epoch and %d %s calls, want 2 and 2", r, n, c.s.Name)
			}
		})
	}
	// Under a hold read_epoch reports state 22 and still names the epoch (§A4.6, §A4.9 read_epoch
	// row): status sends that ID with that epoch's q, and shows the hold.
	t.Run("status under a hold names the epoch", func(t *testing.T) {
		e := newRLEnv(t)
		e.handle(scripts.ReadEpoch, fixed(arr(ival(22), ival(0), bulk("integrity_hold"), bulk(epochA), bulk(fpA),
			ival(tSample), ival(7), ival(tSample), ival(3), ival(1), bulk(""))))
		e.handle(scripts.AdminStatus, fixed(arr(ival(40), ival(0), bulk(""), bulk(epochA), bulk(fpA), bulk("store_rollback"),
			ival(tSample), ival(3), ival(1), ival(0))))
		code, stdout, stderr := e.run(t, "status", "--json")
		want := `{"epoch_id":"` + epochA + `","fingerprint":"` + fpA + `","hold":"store_rollback","hwm_us":1760000000000000,` +
			`"tracked_buckets":3,"live_markers":1,"quarantined_keys":0}` + "\n"
		if code != 0 || stdout != want {
			t.Fatalf("exit %d, stdout\n  %q\nwant 0 and\n  %q\nstderr:\n%s", code, stdout, want, stderr)
		}
		wantCall := []string{"EVALSHA", scripts.AdminStatus.SHA, "2", "rl:epoch", "rl:e:" + epochA + ":q", epochA}
		if st := e.evalshaArgs(scripts.AdminStatus); len(st) != 1 || !slices.Equal(st[0], wantCall) {
			t.Errorf("admin_status calls %q, want one: %q", st, wantCall)
		}
	})
	// Every --json field comes from its own reply element (§A4.9 layouts): a distinct value in
	// each, and the exact line.
	t.Run("status and inspect JSON, field by field", func(t *testing.T) {
		e := newRLEnv(t)
		healthy(e)
		e.handle(scripts.AdminStatus, fixed(arr(ival(40), ival(0), bulk(""), bulk(epochA), bulk(fpA), bulk(""),
			ival(101), ival(102), ival(103), ival(104))))
		e.handle(scripts.AdminInspect, fixed(arr(ival(40), ival(0), bulk(""), ival(1), ival(11), ival(12), ival(13), ival(14),
			ival(15), ival(16), ival(17), bulk("index_without_bucket|12"), ival(2), ival(18), ival(19))))
		for _, c := range []struct {
			args []string
			want string
		}{
			{[]string{"status", "--json"}, `{"epoch_id":"` + epochA + `","fingerprint":"` + fpA + `","hold":"","hwm_us":101,` +
				`"tracked_buckets":102,"live_markers":103,"quarantined_keys":104}` + "\n"},
			{[]string{"inspect", "--key", "alice", "--json"}, `{"present":1,"u":11,"t_us":12,"g":13,"s":14,"x_us":15,"mc":16,` +
				`"ix_score":17,"quarantine":"index_without_bucket|12","marker_expiries":[18,19]}` + "\n"},
		} {
			if code, stdout, stderr := e.run(t, c.args...); code != 0 || stdout != c.want {
				t.Errorf("rladmin %q: exit %d, stdout\n  %q\nwant 0 and\n  %q\nstderr:\n%s", c.args, code, stdout, c.want, stderr)
			}
		}
	})
	// The store address and user (DESIGN §A6): --store wins over RL_STORE_ADDR, and
	// RL_ADMIN_USER names the user AUTH sends.
	t.Run("--store wins over RL_STORE_ADDR", func(t *testing.T) {
		e := newRLEnv(t)
		healthy(e)
		e.handle(scripts.AdminStatus, fixed(statusReply(epochA, 3)))
		dead := fakeresp.Start(t, "rl_admin", testPW)
		deadAddr := dead.Addr()
		dead.Stop()
		env := map[string]string{"RL_STORE_ADDR": deadAddr, "RL_ADMIN_PASSWORD": testPW}
		code, stdout, stderr := runWith(t, env, "status", "--store", e.srv.Addr(), "--json")
		if code != 0 || !strings.Contains(stdout, `"epoch_id":"`+epochA+`"`) {
			t.Errorf("exit %d, stdout %q, want 0 from the store --store names; stderr:\n%s", code, stdout, stderr)
		}
	})
	t.Run("RL_ADMIN_USER names the user", func(t *testing.T) {
		srv := fakeresp.Start(t, "rl_other", testPW)
		e := &rlEnv{srv: srv, env: map[string]string{"RL_STORE_ADDR": srv.Addr(), "RL_ADMIN_PASSWORD": testPW, "RL_ADMIN_USER": "rl_other"}}
		healthy(e)
		e.handle(scripts.AdminStatus, fixed(statusReply(epochA, 3)))
		if code, stdout, stderr := e.run(t, "status", "--json"); code != 0 || !strings.Contains(stdout, `"epoch_id":"`+epochA+`"`) {
			t.Errorf("exit %d, stdout %q, want 0 as rl_other; stderr:\n%s", code, stdout, stderr)
		}
	})
	// A connection lost after AUTH, while the command waits for its reply, is a store problem
	// (exit 3), and the command is not sent again (AGENTS.md §6).
	t.Run("connection lost after AUTH exits 3", func(t *testing.T) {
		e := newRLEnv(t)
		e.handle(scripts.AdminClear, func(_, _ []string) resp.Value {
			e.srv.DropConns()
			return arr(ival(40), ival(tSample), bulk(""), ival(0))
		})
		code, stdout, stderr := e.run(t, clearArgs(e)...)
		if code != 3 || stdout != "" {
			t.Errorf("exit %d, stdout %q, want 3; stderr:\n%s", code, stdout, stderr)
		}
		if n, a := len(e.evalshaArgs(scripts.AdminClear)), e.srv.Accepted(); n != 1 || a != 1 {
			t.Errorf("%d admin_clear calls on %d connections, want 1 on 1", n, a)
		}
	})

	// NOSCRIPT proves the script never ran: one SCRIPT LOAD and exactly one resend
	// (AGENTS.md §6). A second NOSCRIPT is an error reply: exit 3, nothing more sent.
	t.Run("NOSCRIPT reloads once", func(t *testing.T) {
		e := newRLEnv(t)
		e.srv.Handle(scripts.AdminClear.SHA, fixed(arr(ival(40), ival(tSample), bulk(""), ival(0)))) // not loaded
		code, _, stderr := e.run(t, clearArgs(e)...)
		if code != 0 {
			t.Fatalf("exit %d, want 0; stderr:\n%s", code, stderr)
		}
		if n, l := len(e.evalshaArgs(scripts.AdminClear)), e.srv.Count("SCRIPT LOAD"); n != 2 || l != 1 {
			t.Errorf("%d EVALSHA and %d SCRIPT LOAD, want 2 and 1", n, l)
		}
	})
	t.Run("NOSCRIPT twice", func(t *testing.T) {
		e := newRLEnv(t)
		e.srv.Handle(scripts.AdminClear.SHA, fixed(arr(ival(40), ival(tSample), bulk(""), ival(0))))
		e.srv.Load(scripts.AdminClear.Text)
		e.srv.ReplyError("EVALSHA", "NOSCRIPT No matching script. Please use EVAL.", 2)
		code, _, stderr := e.run(t, clearArgs(e)...)
		if code != 3 || !strings.Contains(stderr, "NOSCRIPT") {
			t.Fatalf("exit %d, want 3 with the NOSCRIPT reply; stderr:\n%s", code, stderr)
		}
		if n, l := e.srv.Count("EVALSHA"), e.srv.Count("SCRIPT LOAD"); n != 2 || l != 1 {
			t.Errorf("%d EVALSHA and %d SCRIPT LOAD, want 2 and 1", n, l)
		}
	})

	// Usage errors and --help.
	t.Run("help", func(t *testing.T) {
		code, stdout, _ := runWith(t, map[string]string{}, "--help")
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		names := []string{"start-epoch", "status", "inspect", "restore", "clear", "store-info", "store-rewrite"}
		if code != 0 || len(lines) != len(names) {
			t.Fatalf("--help: exit %d, %d lines, want 0 and %d:\n%s", code, len(lines), len(names), stdout)
		}
		for i, n := range names {
			if !strings.HasPrefix(lines[i], "rladmin "+n+" ") {
				t.Errorf("--help line %d is %q, want the %s line", i+1, lines[i], n)
			}
		}
	})
	// Every required flag missing, and --policy empty (the policy is always loaded, so none is
	// sent with a zero fingerprint), is a usage error.
	t.Run("usage errors exit 2 and send nothing", func(t *testing.T) {
		e := newRLEnv(t)
		for _, args := range [][]string{
			{},
			{"frobnicate"},
			{"inspect"},
			{"start-epoch"},
			{"start-epoch", "--policy", ""},
			{"start-epoch", "--policy", "", "--replace", "--expect-epoch", epochA},
			{"restore", "--key", "alice", "--expect-epoch", epochA},
			{"restore", "--policy", "", "--key", "alice", "--expect-epoch", epochA},
			{"restore", "--policy", e.policy, "--expect-epoch", epochA},
			{"clear", "--key", "alice", "--expect-epoch", epochA},
			{"clear", "--policy", "", "--key", "alice", "--expect-epoch", epochA},
			{"clear", "--policy", e.policy, "--expect-epoch", epochA},
			{"clear", "--policy", e.policy, "--key", "alice"},
			{"restore", "--policy", e.policy, "--key", "alice"},
			{"restore", "--policy", e.policy, "--key", "alice", "--expect-epoch", "ABC"},
			{"start-epoch", "--policy", e.policy, "--replace"},
			{"start-epoch", "--policy", e.policy, "--expect-epoch", epochA},
			{"clear", "--policy", e.policy, "--key", "", "--expect-epoch", epochA},
			{"status", "extra"},
		} {
			if code, stdout, stderr := e.run(t, args...); code != 2 || stdout != "" || !strings.Contains(stderr, "usage:") {
				t.Errorf("rladmin %q: exit %d, stdout %q, want exit 2 with the usage on stderr:\n%s", args, code, stdout, stderr)
			}
		}
		noAddr := map[string]string{"RL_ADMIN_PASSWORD": testPW}
		if code, _, stderr := runWith(t, noAddr, "status"); code != 2 || !strings.Contains(stderr, "RL_STORE_ADDR") {
			t.Errorf("no store address: exit %d, want 2 naming RL_STORE_ADDR:\n%s", code, stderr)
		}
		noPW := map[string]string{"RL_STORE_ADDR": e.srv.Addr()}
		if code, _, stderr := runWith(t, noPW, "status"); code != 2 || !strings.Contains(stderr, "RL_ADMIN_PASSWORD") {
			t.Errorf("no password: exit %d, want 2 naming RL_ADMIN_PASSWORD:\n%s", code, stderr)
		}
		if n := e.srv.Accepted(); n != 0 {
			t.Errorf("usage errors opened %d connections, want 0", n)
		}
	})
	t.Run("store unreachable exits 3", func(t *testing.T) {
		e := newRLEnv(t)
		addr := e.srv.Addr()
		e.srv.Stop()
		if code, _, stderr := runWith(t, map[string]string{"RL_STORE_ADDR": addr, "RL_ADMIN_PASSWORD": testPW}, "status"); code != 3 {
			t.Errorf("stopped store: exit %d, want 3:\n%s", code, stderr)
		}
	})
	t.Run("AUTH refused exits 3", func(t *testing.T) {
		e := newRLEnv(t)
		env := map[string]string{"RL_STORE_ADDR": e.srv.Addr(), "RL_ADMIN_PASSWORD": "another-password"}
		if code, _, stderr := runWith(t, env, "status"); code != 3 || !strings.Contains(stderr, "WRONGPASS") {
			t.Errorf("wrong password: exit %d, want 3 with the refusal:\n%s", code, stderr)
		}
		if n := e.srv.Count("EVALSHA"); n != 0 {
			t.Errorf("%d EVALSHA after a refused AUTH, want 0", n)
		}
	})
}

// TestPolicyTOMLDefaults (DESIGN §B2, §A6; plan S8 step 3): rladmin decodes only the [policy]
// table onto limiter.DefaultPolicy(): absent keys keep their defaults, other tables and
// top-level keys are ignored, every §B2 key is read, an unknown [policy] key (any other
// casing of the table or a key included) or an invalid value is an invalid policy, and an
// invalid policy exits 2 before connecting. Expected values are the DESIGN literals.
func TestPolicyTOMLDefaults(t *testing.T) {
	t.Run("absent keys keep defaults, other tables ignored", func(t *testing.T) {
		pi, err := decodePolicy([]byte("title = \"anything\"\n" + policyTOML + "\n[store]\naddr = 5\n[api]\nrate_tokens = \"x\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		want := limiter.Policy{
			RateTokens: 10, RatePeriodNs: 1_000_000_000, Capacity: 20, FailureMode: limiter.Closed,
			MaxTrackedBuckets: 100_000, MaxLiveMarkers: 50_000, MaxMarkersPerKey: 1_000,
			ReceiptLifetimeNs: 60_000_000_000, RefundDeadlineNs: 30_000_000_000,
			OutageToleranceNs: 5_000_000_000, CleanupIntervalNs: 5_000_000_000, ClockSource: limiter.Store,
		}
		if pi.p != want {
			t.Errorf("policy %+v, want %+v", pi.p, want)
		}
		if pi.fp != fpOf(defaultLines) {
			t.Errorf("fingerprint %s, want %s", pi.fp, fpOf(defaultLines))
		}
		if pi.lim.CapUnits != 20_000_000_000 || pi.lim.LifetimeUs != 60_000_000 {
			t.Errorf("cap_units %d and lifetime_us %d, want 20,000,000,000 and 60,000,000", pi.lim.CapUnits, pi.lim.LifetimeUs)
		}
	})
	t.Run("every key is read", func(t *testing.T) {
		pi, err := decodePolicy([]byte(`[policy]
rate_tokens = 7
rate_period_ns = 2_000_000_000
capacity = 9
failure_mode = "open"
max_tracked_buckets = 11
max_live_markers = 13
max_markers_per_key = 12
receipt_lifetime_ns = 40_000_000_000
refund_deadline_ns = 20_000_000_000
outage_tolerance_ns = 3_000_000_000
cleanup_interval_ns = 4_000_000_000
clock_source = "controlled"
`))
		if err != nil {
			t.Fatal(err)
		}
		want := limiter.Policy{
			RateTokens: 7, RatePeriodNs: 2_000_000_000, Capacity: 9, FailureMode: limiter.Open,
			MaxTrackedBuckets: 11, MaxLiveMarkers: 13, MaxMarkersPerKey: 12,
			ReceiptLifetimeNs: 40_000_000_000, RefundDeadlineNs: 20_000_000_000,
			OutageToleranceNs: 3_000_000_000, CleanupIntervalNs: 4_000_000_000, ClockSource: limiter.Controlled,
		}
		if pi.p != want {
			t.Errorf("policy %+v, want %+v", pi.p, want)
		}
		if pi.lim.CapUnits != 18_000_000_000 || pi.lim.LifetimeUs != 40_000_000 {
			t.Errorf("cap_units %d and lifetime_us %d, want 18,000,000,000 and 40,000,000", pi.lim.CapUnits, pi.lim.LifetimeUs)
		}
	})
	// Each invalid policy fails for its own rule: the base holds the required keys, with
	// max_live_markers at max_markers_per_key's default (§A5.10).
	const base = "[policy]\nmax_tracked_buckets = 1\nmax_live_markers = 1000\n"
	if _, err := decodePolicy([]byte(base)); err != nil {
		t.Fatalf("the base policy is refused: %v", err)
	}
	for _, c := range []struct{ name, toml, why string }{
		{"no [policy] table: the required keys are missing", "[instance]\nmargin_ns = 1\n", "MaxTrackedBuckets"},
		{"unknown [policy] table", base + "\n[policy.extra]\nx = 1\n", "unknown key policy.extra"},
		{"misspelled [policy] key", base + "rate_token = 5\n", "unknown key policy.rate_token"},
		{"dotted key under [policy]", base + "extra.x = 1\n", "unknown key policy.extra.x"},
		{"dotted key ending in a §B2 name", base + "extra.capacity = 1\n", "unknown key policy.extra.capacity"},
		// TOML names are case-sensitive, though BurntSushi/toml matches a field in any casing:
		// only the exact [policy] table and §B2 names count.
		{"mis-cased [policy] key", base + "Capacity = 5\n", "unknown key policy.Capacity"},
		{"upper-case [policy] key", base + "CAPACITY = 5\n", "unknown key policy.CAPACITY"},
		{"a key in two casings", base + "capacity = 20\nCapacity = 5\n", "unknown key policy.Capacity"},
		{"[Policy] table", "[Policy]\nmax_tracked_buckets = 1\nmax_live_markers = 1000\n", "unknown key Policy"},
		{"[POLICY] table", "[POLICY]\nmax_tracked_buckets = 1\nmax_live_markers = 1000\n", "unknown key POLICY"},
		{"misspelled key under [Policy]", base + "\n[Policy]\nrate_token = 5\n", "unknown key Policy"},
		{"bad failure_mode", base + "failure_mode = \"sideways\"\n", "failure_mode"},
		{"bad clock_source", base + "clock_source = \"wall\"\n", "clock_source"},
		{"wrong type", base + "capacity = \"20\"\n", "capacity"},
		{"not a whole µs", base + "rate_period_ns = 1_000_000_001\n", "RatePeriodNs"},
		// cap_units = capacity × rate_period_ns must stay below 2^52 = 4,503,599,627,370,496
		// (§A3.1): 4,503,599,627,371,000 is the first whole µs past it.
		{"cap_units past 2^52", base + "rate_period_ns = 4_503_599_627_371_000\ncapacity = 1\nrate_tokens = 1\n", "Capacity × RatePeriodNs"},
		{"not TOML", "[policy\n", "decoding the policy file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodePolicy([]byte(c.toml))
			if err == nil || !strings.Contains(err.Error(), c.why) {
				t.Errorf("decodePolicy: %v, want an error naming %q, for:\n%s", err, c.why, c.toml)
			}
		})
	}
	// The 2^52 bound's other side: 4,503,599,627,370,000, the last whole µs below it, passes.
	if _, err := decodePolicy([]byte(base + "rate_period_ns = 4_503_599_627_370_000\ncapacity = 1\nrate_tokens = 1\n")); err != nil {
		t.Errorf("cap_units below 2^52 refused: %v", err)
	}
	t.Run("invalid policy exits 2 before connecting", func(t *testing.T) {
		e := newRLEnv(t)
		bad := filepath.Join(t.TempDir(), "bad.toml")
		if err := os.WriteFile(bad, []byte("[policy]\nmax_tracked_buckets = 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"start-epoch", "--policy", bad},
			{"restore", "--policy", bad, "--key", "alice", "--expect-epoch", epochA},
			{"clear", "--policy", bad, "--key", "alice", "--expect-epoch", epochA},
			{"start-epoch", "--policy", filepath.Join(t.TempDir(), "missing.toml")},
		} {
			if code, _, stderr := e.run(t, args...); code != 2 || !strings.Contains(stderr, "invalid policy") {
				t.Errorf("rladmin %q: exit %d, want 2 for an invalid policy:\n%s", args, code, stderr)
			}
		}
		if n := e.srv.Accepted(); n != 0 {
			t.Errorf("an invalid policy opened %d connections, want 0", n)
		}
	})
}

// TestPolicyFileBounded (AGENTS.md §2: a disk wait is bounded; review R1): rladmin reads
// --policy only from a regular file, and at most 1 MiB (1,048,576 bytes) of it. A FIFO with no
// writer is refused at once (well inside 5 s) with exit 2 and no connection; a valid policy
// padded to 1,048,577 bytes is an invalid policy (exit 2, no connection), while the same
// policy padded to exactly 1,048,576 bytes, and the plain policy file, load. Sizes are
// literals.
func TestPolicyFileBounded(t *testing.T) {
	t.Run("a FIFO is refused at once", func(t *testing.T) {
		e := newRLEnv(t)
		fifo := filepath.Join(t.TempDir(), "policy.fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		type result struct {
			code   int
			stderr string
		}
		done := make(chan result, 1)
		bound := time.NewTimer(5 * time.Second) // anchored before the run starts
		defer bound.Stop()
		go func() {
			code, _, stderr := e.run(t, "start-epoch", "--policy", fifo)
			done <- result{code, stderr}
		}()
		select {
		case r := <-done:
			if r.code != 2 || !strings.Contains(r.stderr, "invalid policy") || !strings.Contains(r.stderr, "not a regular file") {
				t.Errorf("exit %d, want 2 for a FIFO policy path (not a regular file):\n%s", r.code, r.stderr)
			}
		case <-bound.C:
			// The load waits on the FIFO: a writer that closes at once ends that wait (EOF),
			// so the run's goroutine ends before the test does.
			if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			t.Fatal("rladmin start-epoch --policy <a FIFO> still ran after 5 s, want exit 2 at once")
		}
		if n := e.srv.Accepted(); n != 0 {
			t.Errorf("a FIFO policy path opened %d connections, want 0", n)
		}
	})
	t.Run("at most 1 MiB is read", func(t *testing.T) {
		e := newRLEnv(t)
		dir := t.TempDir()
		// padded writes policyTOML and one comment line ('#', 'x's, '\n'): size bytes in all.
		padded := func(size int) string {
			t.Helper()
			path := filepath.Join(dir, "policy-"+strconv.Itoa(size)+".toml")
			text := policyTOML + "#" + strings.Repeat("x", size-len(policyTOML)-2) + "\n"
			if len(text) != size {
				t.Fatalf("padded file of %d bytes, want %d", len(text), size)
			}
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}
		for _, path := range []string{e.policy, padded(1_048_576)} {
			if pi, err := loadPolicy(path); err != nil || pi.fp != fpOf(defaultLines) {
				t.Errorf("loadPolicy(%s): fingerprint %q, %v; want %s", filepath.Base(path), pi.fp, err, fpOf(defaultLines))
			}
		}
		code, _, stderr := e.run(t, "start-epoch", "--policy", padded(1_048_577))
		if code != 2 || !strings.Contains(stderr, "invalid policy") || !strings.Contains(stderr, "larger than 1048576 bytes") {
			t.Errorf("a policy file of 1,048,577 bytes: exit %d, want 2 (larger than 1048576 bytes):\n%s", code, stderr)
		}
		if n := e.srv.Accepted(); n != 0 {
			t.Errorf("a policy file over the cap opened %d connections, want 0", n)
		}
	})
}

// TestBalanceTokensToUnits (DESIGN §A6): restore sends --balance-units U as it is and
// --balance-tokens T as T × rate_period_ns, computed exactly, 0 when neither is given, with
// the KEYS and ARGV of §A4.1; giving both, or a value not written in decimal, is a usage error
// that sends nothing. Expected values are hand computed.
func TestBalanceTokensToUnits(t *testing.T) {
	cases := []struct {
		name    string
		period  string // the policy's rate_period_ns
		balance []string
		want    string
	}{
		{"default 0", "1_000_000_000", nil, "0"},
		{"units", "1_000_000_000", []string{"--balance-units", "1234"}, "1234"},
		{"units 0", "1_000_000_000", []string{"--balance-units", "0"}, "0"},
		{"3 tokens", "1_000_000_000", []string{"--balance-tokens", "3"}, "3000000000"},
		{"3 tokens of 0.25 s", "250_000_000", []string{"--balance-tokens", "3"}, "750000000"},
		{"0 tokens", "1_000_000_000", []string{"--balance-tokens", "0"}, "0"},
		{"negative tokens", "1_000_000_000", []string{"--balance-tokens", "-1"}, "-1000000000"},
		{"tokens past int64 units", "1_000_000_000", []string{"--balance-tokens", "9223372036854775807"}, "9223372036854775807000000000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRLEnv(t)
			pol := filepath.Join(t.TempDir(), "p.toml")
			toml := "[policy]\nmax_tracked_buckets = 100_000\nmax_live_markers = 50_000\nrate_period_ns = " + c.period + "\n"
			if err := os.WriteFile(pol, []byte(toml), 0o600); err != nil {
				t.Fatal(err)
			}
			e.handle(scripts.AdminRestore, fixed(arr(ival(40), ival(tSample), bulk(""), ival(1))))
			args := append([]string{"restore", "--policy", pol, "--key", "alice", "--expect-epoch", epochA}, c.balance...)
			if code, _, stderr := e.run(t, args...); code != 0 {
				t.Fatalf("exit %d, want 0:\n%s", code, stderr)
			}
			calls := e.evalshaArgs(scripts.AdminRestore)
			if len(calls) != 1 {
				t.Fatalf("%d admin_restore calls, want 1", len(calls))
			}
			capUnits := map[string]string{"1_000_000_000": "20000000000", "250_000_000": "5000000000"}[c.period]
			ns := "rl:e:" + epochA + ":"
			want := []string{"EVALSHA", scripts.AdminRestore.SHA, "6",
				"rl:epoch", "rl:ctl:time", ns + "ix", ns + "q", ns + "k:" + keyEnc, ns + "mi:" + keyEnc,
				epochA, calls[0][10], keyEnc, c.want, capUnits, "60000000"}
			if !slices.Equal(calls[0], want) {
				t.Errorf("admin_restore sent\n  %q\nwant\n  %q", calls[0], want)
			}
		})
	}
	t.Run("the fingerprint is the policy's", func(t *testing.T) {
		e := newRLEnv(t)
		e.handle(scripts.AdminRestore, fixed(arr(ival(40), ival(tSample), bulk(""), ival(1))))
		if code, _, stderr := e.run(t, "restore", "--policy", e.policy, "--key", "alice", "--expect-epoch", epochA); code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		if got := e.evalshaArgs(scripts.AdminRestore)[0][10]; got != fpOf(defaultLines) {
			t.Errorf("fp %s, want %s", got, fpOf(defaultLines))
		}
	})
	t.Run("both flags", func(t *testing.T) {
		e := newRLEnv(t)
		code, _, stderr := e.run(t, "restore", "--policy", e.policy, "--key", "alice", "--expect-epoch", epochA,
			"--balance-units", "1", "--balance-tokens", "1")
		if code != 2 || e.srv.Accepted() != 0 {
			t.Errorf("both balance flags: exit %d, %d connections, want 2 and 0:\n%s", code, e.srv.Accepted(), stderr)
		}
	})
	// Decimal only: a leading zero (octal 010 = 8), a base prefix or an underscore is a usage
	// error that sends nothing, though a restore of it would succeed.
	t.Run("decimal only", func(t *testing.T) {
		e := newRLEnv(t)
		e.handle(scripts.AdminRestore, fixed(arr(ival(40), ival(tSample), bulk(""), ival(1))))
		for _, b := range [][]string{
			{"--balance-tokens", "010"}, {"--balance-tokens", "0x10"},
			{"--balance-units", "010"}, {"--balance-units", "0x10"}, {"--balance-units", "1_000"},
		} {
			args := append([]string{"restore", "--policy", e.policy, "--key", "alice", "--expect-epoch", epochA}, b...)
			if code, stdout, stderr := e.run(t, args...); code != 2 || stdout != "" || !strings.Contains(stderr, "usage:") {
				t.Errorf("rladmin %q: exit %d, stdout %q, want exit 2 with the usage on stderr:\n%s", b, code, stdout, stderr)
			}
		}
		if n := e.srv.Accepted(); n != 0 {
			t.Errorf("balances not in decimal opened %d connections, want 0", n)
		}
	})
}

// TestNoSecretsInOutput (AGENTS.md §1 Secrets; DESIGN §A6): the password never appears, whole
// or in part, in any output: --help, usage errors, a refused AUTH that repeats it, an error
// reply that repeats it, a raw reply that holds it (also past Value.String's 64-byte clip),
// result fields and store-info text that hold it, in table and JSON form, also in its
// JSON-escaped and Go-quoted forms.
func TestNoSecretsInOutput(t *testing.T) {
	check := func(t *testing.T, what, out string) {
		t.Helper()
		for i := 0; i+8 <= len(testPW); i++ {
			if strings.Contains(out, testPW[i:i+8]) {
				t.Errorf("%s holds part of the password (%q):\n%s", what, testPW[i:i+8], out)
				return
			}
		}
	}
	runAll := func(t *testing.T, e *rlEnv, args ...string) (int, string) {
		t.Helper()
		code, stdout, stderr := e.run(t, args...)
		check(t, "stdout of "+strings.Join(args, " "), stdout)
		check(t, "stderr of "+strings.Join(args, " "), stderr)
		return code, stdout + stderr
	}
	long := strings.Repeat("x", 40) + testPW // the clip at 64 bytes would keep part of it

	t.Run("help and usage", func(t *testing.T) {
		e := newRLEnv(t)
		runAll(t, e, "--help")
		runAll(t, e, "restore", "--balance-units", testPW)
		runAll(t, e, "no-such-command", testPW)
	})
	t.Run("AUTH refusal repeating the password", func(t *testing.T) {
		e := newRLEnv(t)
		e.srv.ReplyError("AUTH", "WRONGPASS "+testPW+" is wrong", 1)
		if code, out := runAll(t, e, "status"); code != 3 || !strings.Contains(out, "WRONGPASS") {
			t.Errorf("exit %d, want 3 with the refusal:\n%s", code, out)
		}
	})
	t.Run("error reply repeating the password", func(t *testing.T) {
		e := newRLEnv(t)
		e.handle(scripts.ReadEpoch, fixed(readEpochReply(epochA, 1)))
		e.srv.ReplyError("EVALSHA", "ERR "+long, 1)
		if code, out := runAll(t, e, "status"); code != 3 || !strings.Contains(out, "store error reply") {
			t.Errorf("exit %d, want 3:\n%s", code, out)
		}
	})
	t.Run("raw reply holding the password", func(t *testing.T) {
		e := newRLEnv(t)
		e.handle(scripts.AdminRestore, fixed(arr(ival(40), ival(tSample), bulk(long), ival(-1))))
		if code, out := runAll(t, e, "restore", "--policy", e.policy, "--key", "alice", "--expect-epoch", epochA); code != 3 || !strings.Contains(out, "raw reply") {
			t.Errorf("exit %d, want 3 with the raw reply:\n%s", code, out)
		}
	})
	t.Run("result fields holding the password", func(t *testing.T) {
		for _, asJSON := range []bool{false, true} {
			e := newRLEnv(t)
			e.handle(scripts.ReadEpoch, fixed(readEpochReply(epochA, 1)))
			e.handle(scripts.AdminStatus, fixed(arr(ival(40), ival(0), bulk(""), bulk(epochA), bulk(fpA), bulk(testPW), ival(1), ival(1), ival(1), ival(1))))
			args := []string{"status"}
			if asJSON {
				args = append(args, "--json")
			}
			if code, out := runAll(t, e, args...); code != 0 || !strings.Contains(out, redacted) {
				t.Errorf("status %v: exit %d, want 0 with the hold redacted:\n%s", args, code, out)
			}
		}
	})
	t.Run("store-info text holding the password", func(t *testing.T) {
		e := newRLEnv(t)
		e.srv.ReplyValue("INFO", bulk("# Persistence\r\nloading:0\r\nnote:"+testPW+"\r\n"), 2)
		e.srv.ReplyValue("SLOWLOG", arr(arr(ival(1), ival(2), ival(3), arr(bulk("AUTH"), bulk("rl_admin"), bulk(testPW)), bulk("127.0.0.1:1"), bulk(""))), 2)
		if code, out := runAll(t, e, "store-info"); code != 0 {
			t.Errorf("store-info: exit %d, want 0:\n%s", code, out)
		}
		code, out := runAll(t, e, "store-info", "--json")
		if code != 0 {
			t.Fatalf("store-info --json: exit %d, want 0:\n%s", code, out)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Errorf("store-info --json is not one JSON object: %v\n%s", err, out)
		}
	})
	// A password holding characters that JSON and Go quoting escape (" \ < > &): its raw form,
	// its JSON-escaped form (store-info --json) and its Go-quoted form (the table's slow-log
	// line) never reach the output. The escaped forms are written by hand.
	t.Run("password with escaped characters", func(t *testing.T) {
		const pw = `p"w\<x>&y-0123456789`
		forms := map[string]string{
			"raw":          pw,
			"JSON-escaped": `p\"w\\\u003cx\u003e\u0026y-0123456789`,
			"Go-quoted":    `p\"w\\<x>&y-0123456789`,
		}
		srv := fakeresp.Start(t, "rl_admin", pw)
		e := &rlEnv{srv: srv, env: map[string]string{"RL_STORE_ADDR": srv.Addr(), "RL_ADMIN_PASSWORD": pw}}
		srv.ReplyValue("INFO", bulk("# Persistence\r\nloading:0\r\nnote:"+pw+"\r\n"), 2)
		srv.ReplyValue("SLOWLOG", arr(arr(ival(1), ival(2), ival(3), arr(bulk("AUTH"), bulk("rl_admin"), bulk(pw)), bulk("127.0.0.1:1"), bulk(""))), 2)
		for _, c := range []struct {
			args []string
			want []string // the redacted texts stdout holds
		}{
			{[]string{"store-info", "--json"}, []string{`"note":"<password>"`, `"args":["AUTH","rl_admin","<password>"]`}},
			{[]string{"store-info"}, []string{"\nnote:<password>\n", `"AUTH rl_admin <password>"`}},
		} {
			code, stdout, stderr := e.run(t, c.args...)
			if code != 0 {
				t.Errorf("rladmin %q: exit %d, want 0:\n%s", c.args, code, stderr)
			}
			for name, f := range forms {
				if strings.Contains(stdout+stderr, f) {
					t.Errorf("rladmin %q: the output holds the password's %s form:\n%s%s", c.args, name, stdout, stderr)
				}
			}
			for _, w := range c.want {
				if !strings.Contains(stdout, w) {
					t.Errorf("rladmin %q: stdout lacks %q:\n%s", c.args, w, stdout)
				}
			}
		}
	})
}

// TestStoreRewritePolls (DESIGN §A6; plan S8 step 3): store-rewrite sends BGREWRITEAOF, then
// reads INFO persistence until no rewrite is in progress or scheduled, and exits 0 only when
// the rewrite ended ok; a failed rewrite, an error reply or a reply of another shape exits 3.
// When BGREWRITEAOF replies that a rewrite is already running (Valkey's "ERR Background append
// only file rewriting already in progress"; review A1), it reads INFO persistence a poll later,
// and every poll, until that rewrite is neither in progress nor scheduled, then sends
// BGREWRITEAOF again, as often as that reply comes back, and judges only its own rewrite's
// aof_last_bgrewrite_status; another error reply after a wait still exits 3.
func TestStoreRewritePolls(t *testing.T) {
	persistence := func(inProgress, scheduled, status string) resp.Value {
		return bulk("# Persistence\r\nloading:0\r\naof_rewrite_in_progress:" + inProgress +
			"\r\naof_rewrite_scheduled:" + scheduled + "\r\naof_last_bgrewrite_status:" + status + "\r\n")
	}
	started := resp.Value{Kind: resp.Simple, Str: "Background append only file rewriting started"}
	running := resp.Value{Kind: resp.Error, Str: "ERR Background append only file rewriting already in progress"}
	const cantRun = "ERR Can't execute an AOF background rewriting. Please check the server logs for more information."
	cases := []struct {
		name     string
		rewrites []resp.Value // the BGREWRITEAOF replies, in order
		infos    []resp.Value // the INFO replies, in order
		code     int
		polls    int
	}{
		{"in progress twice, then done", []resp.Value{started}, []resp.Value{persistence("1", "0", "ok"), persistence("1", "0", "ok"), persistence("0", "0", "ok")}, 0, 3},
		{"scheduled, in progress, done", []resp.Value{started}, []resp.Value{persistence("0", "1", "ok"), persistence("1", "0", "ok"), persistence("0", "0", "ok")}, 0, 3},
		{"done at once", []resp.Value{started}, []resp.Value{persistence("0", "0", "ok")}, 0, 1},
		{"failed", []resp.Value{started}, []resp.Value{persistence("1", "0", "ok"), persistence("0", "0", "err")}, 3, 2},
		{"no rewrite fields", []resp.Value{started}, []resp.Value{bulk("# Persistence\r\nloading:0\r\n")}, 3, 1},
		{"a rewrite already running, then ours", []resp.Value{running, started},
			[]resp.Value{persistence("1", "0", "ok"), persistence("0", "0", "ok"), persistence("1", "0", "ok"), persistence("0", "0", "ok")}, 0, 4},
		// The running rewrite ended err: not this command's rewrite, so not its outcome.
		{"still running at the resend", []resp.Value{running, running, started},
			[]resp.Value{persistence("0", "1", "ok"), persistence("0", "0", "err"), persistence("0", "0", "err"), persistence("0", "0", "ok")}, 0, 4},
		{"a rewrite already running, then ours fails", []resp.Value{running, started},
			[]resp.Value{persistence("0", "0", "ok"), persistence("1", "0", "ok"), persistence("0", "0", "err")}, 3, 3},
		{"a rewrite already running, then another error reply", []resp.Value{running, {Kind: resp.Error, Str: cantRun}},
			[]resp.Value{persistence("0", "0", "ok")}, 3, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRLEnv(t)
			for _, v := range c.rewrites {
				e.srv.ReplyValue("BGREWRITEAOF", v, 1)
			}
			for _, v := range c.infos {
				e.srv.ReplyValue("INFO", v, 1)
			}
			code, stdout, stderr := e.run(t, "store-rewrite", "--json")
			if code != c.code {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.code, stdout, stderr)
			}
			if n := e.srv.Count("INFO"); n != c.polls {
				t.Errorf("%d INFO polls, want %d", n, c.polls)
			}
			if n := e.srv.Count("BGREWRITEAOF"); n != len(c.rewrites) {
				t.Errorf("%d BGREWRITEAOF sent, want %d", n, len(c.rewrites))
			}
			if c.code == 0 && stdout != `{"aof_rewrite_in_progress":"0","aof_last_bgrewrite_status":"ok"}`+"\n" {
				t.Errorf("stdout %q", stdout)
			}
			if last := c.rewrites[len(c.rewrites)-1]; last.Kind == resp.Error && !strings.Contains(stderr, last.Str) {
				t.Errorf("stderr %q lacks the error reply %q", stderr, last.Str)
			}
		})
	}
	t.Run("error reply", func(t *testing.T) {
		e := newRLEnv(t)
		e.srv.ReplyError("BGREWRITEAOF", cantRun, 1)
		if code, _, stderr := e.run(t, "store-rewrite"); code != 3 || !strings.Contains(stderr, "Can't execute an AOF background rewriting") {
			t.Errorf("exit %d, want 3 with the reply:\n%s", code, stderr)
		}
		if n := e.srv.Count("INFO"); n != 0 {
			t.Errorf("%d INFO polls after the error reply, want 0", n)
		}
	})
}
