package scriptrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/fingerprint"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/settings"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
	"github.com/srir84/ratelimiter/internal/testutil/storestate"
)

const (
	// AuditEnv turns on the discipline run when it is "1"; it is read only here (AGENTS.md §1).
	AuditEnv = "RL_SCRIPT_AUDIT"
	// FillerPrefix is the key prefix outside the §A3.2 layout that L2 tests may fill (the OOM
	// test's filler, plan choice); the audit accepts any key under it.
	FillerPrefix = "rl:fill:"
	// RepliesFile is where Run appends one line per decoded reply, under the module root,
	// unless RepliesEnv names another file.
	RepliesFile = ".out/replies-l2.jsonl"
	// RepliesEnv, when set, names the file Run appends to instead of RepliesFile: an absolute
	// path, since each test binary runs in its own package directory. bin/mutate sets it for
	// every go test it starts, to a file in a temp directory, so the replies of a mutant never
	// count for the reply-code closure (plan S4 Watch out); its copy of the name, repliesEnv in
	// tools/mutate, must stay equal (TestMutate_RepliesEnv). It is read only here (AGENTS.md §1).
	RepliesEnv = "RL_REPLIES_FILE"
	// dependencyDeadline is D for the adapters Adapter builds: generous, since a test's store
	// is local and a slow machine must not turn a reply into outcome_unknown.
	dependencyDeadline = 10 * time.Second
)

// Handle is a test's store (plan choice): the fast-profile store itself, the policies its
// epoch records may carry, and the keys the test breaks on purpose. Damaged is safe for
// concurrent use.
type Handle struct {
	*storeproc.Store
	policies []settings.Policy

	mu      sync.Mutex
	damaged []string
}

// Store starts a fast-profile store for t and registers its audit in t.Cleanup after the
// store's own cleanup, so the audit runs first, while the store is up: one Snapshot as
// rl_lib, then Audit with the registered policy whose fingerprint equals rl:epoch.fp (the
// first one when none does, so Audit reports the mismatch), the filler prefix rl:fill: and
// the keys given to Damaged. Each violation is a t.Errorf.
func Store(t testing.TB, policies ...settings.Policy) *Handle {
	t.Helper()
	h := &Handle{Store: storeproc.Start(t, storeproc.Options{Profile: storeproc.Fast}), policies: policies}
	t.Cleanup(func() { h.audit(t) })
	return h
}

// Damaged declares store keys the test breaks on purpose: the audit checks them by name
// only (storestate.AuditConfig.Damaged). Name only keys broken on purpose.
func (h *Handle) Damaged(keys ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.damaged = append(h.damaged, keys...)
}

// Adapter returns a store adapter connected as user, a DESIGN §A2 ACL user, with the
// reply range limits of policy p (store.LimitsFor), one connection of each kind; Close is
// registered with t.Cleanup.
func (h *Handle) Adapter(t testing.TB, user string, p settings.Policy) *store.StoreAdapter {
	t.Helper()
	pw, ok := h.password(user)
	if !ok {
		t.Fatalf("scriptrun: no password for user %q", user)
	}
	a := store.New(t.Context(), store.Options{
		Addr: h.Addr, User: user, Password: pw,
		StoreConns: 1, RefundConns: 1,
		DependencyDeadline: dependencyDeadline,
		Limits:             store.LimitsFor(p),
	})
	t.Cleanup(a.Close)
	return a
}

// password returns the password of a DESIGN §A2 ACL user.
func (h *Handle) password(user string) (string, bool) {
	switch user {
	case "rl_lib":
		return h.PW.Lib, true
	case "rl_admin":
		return h.PW.Admin, true
	case "rl_fixture":
		return h.PW.Fixture, true
	case "rl_ops":
		return h.PW.Ops, true
	}
	return "", false
}

// audit is the teardown audit Store registers.
func (h *Handle) audit(t testing.TB) {
	c, err := h.Conn("rl_lib")
	if err != nil {
		t.Errorf("store audit: connecting as rl_lib: %v", err)
		return
	}
	defer func() { _ = c.Close() }()
	snap, err := storestate.Snapshot(c)
	if err != nil {
		t.Errorf("store audit: %v", err)
		return
	}
	h.mu.Lock()
	damaged := slices.Clone(h.damaged)
	h.mu.Unlock()
	cfg := storestate.AuditConfig{
		Policy:         h.policyFor(snap[keys.EpochKey].Fields["fp"]),
		Damaged:        damaged,
		FillerPrefixes: []string{FillerPrefix},
	}
	for _, v := range storestate.Audit(snap, cfg) {
		t.Errorf("store audit: %s", v)
	}
}

// policyFor returns the registered policy whose fingerprint is fp, else the first one (or
// the zero policy when none is registered).
func (h *Handle) policyFor(fp string) settings.Policy {
	for _, p := range h.policies {
		if fingerprint.Of(p) == fp {
			return p
		}
	}
	if len(h.policies) > 0 {
		return h.policies[0]
	}
	return settings.Policy{}
}

// Caller is the one method Run needs from S3's store adapter.
type Caller interface {
	Call(ctx context.Context, req store.Request) store.Response
}

// Audited reports which variant Run calls (plan S7 step 12): true in the discipline run
// (RL_SCRIPT_AUDIT=1), where each script is the instrumented variant Instrument builds, and
// false in the plain run. A test that records script timings records them only when it is
// false, since the instrumented scripts are slower (see EvidenceRun).
func Audited() bool { return os.Getenv(AuditEnv) == "1" }

// EvidenceRun reports whether a test may write run evidence such as script timings under
// .out/ (plan S7 step 12): only in the plain run (not Audited) whose replies go to
// .out/replies-l2.jsonl, that is with RL_REPLIES_FILE unset. bin/mutate sets RL_REPLIES_FILE
// for every go test it starts (its test output never reaches .out/*.json), so a run of mutated
// scripts or code never overwrites the plain run's evidence.
func EvidenceRun() bool { return !Audited() && os.Getenv(RepliesEnv) == "" }

// Run calls req's script through a, with t's context. With RL_SCRIPT_AUDIT=1 it calls
// Instrument(req.Script) instead, and an error reply holding AUDIT fails the test with
// t.Errorf, which is safe from any goroutine; so does a script Instrument cannot rewrite,
// which is never called (the response is then outcome_unknown, with the AUDIT error as its
// store error). After each decoded reply it appends one JSON line {script, code, reason} to
// <module root>/.out/replies-l2.jsonl, or to the file RL_REPLIES_FILE names (opened per call
// with O_APPEND|O_CREATE, its directory created when missing; one write per line).
func Run(t testing.TB, a Caller, req store.Request) store.Response {
	t.Helper()
	if Audited() {
		s, err := Instrument(req.Script)
		if err != nil {
			t.Errorf("%s: the script discipline run failed: %v", req.Script.Name, err)
			return store.Response{Outcome: store.OutcomeUnknown, StoreError: err.Error()}
		}
		req.Script = s
	}
	r := a.Call(t.Context(), req)
	if strings.Contains(r.StoreError, "AUDIT") {
		t.Errorf("%s: the script discipline run failed: %s", req.Script.Name, r.StoreError)
	}
	if r.Outcome == store.OK {
		if err := appendReply(req.Script.Name, r.Reply); err != nil {
			t.Errorf("%s: recording the reply: %v", req.Script.Name, err)
		}
	}
	return r
}

// replyLine is one line of .out/replies-l2.jsonl.
type replyLine struct {
	Script string `json:"script"`
	Code   int64  `json:"code"`
	Reason string `json:"reason"`
}

// repliesPath is the file Run appends to: the one RepliesEnv names when it is set, else
// RepliesFile under the module root. A relative RepliesEnv is an error.
func repliesPath() (string, error) {
	if p := os.Getenv(RepliesEnv); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s=%q is not an absolute path", RepliesEnv, p)
		}
		return p, nil
	}
	root, err := storeproc.ModuleRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(RepliesFile)), nil
}

// appendReply appends one decoded reply's line to the replies file with a single write.
func appendReply(script string, rep store.Reply) error {
	path, err := repliesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	line, err := json.Marshal(replyLine{Script: script, Code: rep.Code, Reason: rep.Reason})
	if err != nil {
		return fmt.Errorf("encoding the reply line: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	_, werr := f.Write(append(line, '\n'))
	return errors.Join(werr, f.Close())
}

// Instrument returns s for the discipline run (DESIGN §C3; plan S4 step 14): its text with
// pcall(redis.call, → audit_pcall_call(, redis.pcall( → audit_rpcall( and redis.call( →
// audit_call(, with any whitespace, line breaks included, between the tokens (stylua splits
// long calls), the wrappers inserted after the header line, and the rest of the script (the
// prelude and the script's own code) made the body of the function audit_body, which the
// trailer calls, under the same name. Instrument's own output, recognized only by its exact
// text (the header line, the wrappers and the audit_body opener first, the trailer last, and
// no redis.call or redis.pcall between), comes back unchanged. It fails closed, with an
// AUDIT error: a first line that is not a #!lua header (#!lua, alone or followed by a space
// or tab and its options), since that line is kept as it is; a text that starts as its
// output but is not exactly that; and a redis.call or redis.pcall still in the rewritten
// text (any other form, such as local f = redis.call, a comment between the tokens, or a
// mention in a comment), since the run could not see that command.
func Instrument(s scripts.Script) (scripts.Script, error) {
	header, body, _ := strings.Cut(s.Text, "\n")
	if !regexp.MustCompile(`^#!lua(?:[ \t].*)?$`).MatchString(header) {
		return scripts.Script{}, fmt.Errorf("AUDIT %s: the first line %q is not a #!lua header, so the discipline run cannot instrument the script", s.Name, header)
	}
	raw := regexp.MustCompile(`\bredis\s*\.\s*p?call\b`)
	if inner, ok := strings.CutPrefix(body, wrappers+bodyOpen); ok {
		if inner, ok := strings.CutSuffix(inner, bodyClose); ok && !raw.MatchString(header) && !raw.MatchString(inner) {
			return s, nil
		}
		return scripts.Script{}, fmt.Errorf("AUDIT %s: the script starts with the discipline-run wrappers, but is not as Instrument writes it", s.Name)
	}
	for _, r := range []struct{ pattern, to string }{
		{`\bpcall\s*\(\s*redis\s*\.\s*call\s*,`, "audit_pcall_call("},
		{`\bredis\s*\.\s*pcall\s*\(`, "audit_rpcall("},
		{`\bredis\s*\.\s*call\s*\(`, "audit_call("},
	} {
		body = regexp.MustCompile(r.pattern).ReplaceAllLiteralString(body, r.to)
	}
	text := header + "\n" + body // the header too: it is kept as it is
	if loc := raw.FindStringIndex(text); loc != nil {
		start := strings.LastIndexByte(text[:loc[0]], '\n') + 1
		end := len(text)
		if i := strings.IndexByte(text[loc[0]:], '\n'); i >= 0 {
			end = loc[0] + i
		}
		return scripts.Script{}, fmt.Errorf("AUDIT %s: %q is not called as redis.call(…), redis.pcall(…) or pcall(redis.call, …), so the discipline run cannot check it (in %q)",
			s.Name, text[loc[0]:loc[1]], text[start:end])
	}
	return scripts.New(s.Name, header+"\n"+wrappers+bodyOpen+body+bodyClose), nil
}

// wrapperMark starts the wrappers.
const wrapperMark = "-- scriptrun: discipline-run wrappers"

// bodyOpen and bodyClose make the script's own text, after its header line, the body of
// audit_body, and call it in protected mode: once it returns or fails, the first violation a
// wrapper raised is raised again, also when the script's own pcall caught it (a script's
// pcall(function … end) holding a call), and else the script's reply or error is passed on.
const (
	bodyOpen  = "local function audit_body(...)\n"
	bodyClose = `
end
local audit_ok, audit_reply = pcall(audit_body, ...)
if audit_violation then
  error(audit_violation, 0)
end
if not audit_ok then
  error(audit_reply, 0)
end
return audit_reply
`
)

// wrappers are the discipline run's Lua (DESIGN §C3), a Go constant so tools/trace never
// lints their dynamic redis.call(cmd, …). Each wrapper checks before it calls, and keeps the
// first violation in audit_violation as it raises error('AUDIT …'), so the script's own pcall
// may catch that error but never ends the run with it (bodyClose raises it again). A run
// fails on:
//   - a read (HGETALL, HGET, HEXISTS, HLEN, ZRANGE, ZSCORE, ZCARD, GET, TIME) after the first
//     write (HSET, HDEL, HINCRBY, ZADD, ZREM, DEL, UNLINK) (§A4.8 rule 2);
//   - a second clock read (TIME, or GET of rl:ctl:time), or one before the epoch record
//     (rl:epoch) is read (§A4.8 rule 7);
//   - a write to a key neither read earlier in the run without an error reply (so its type
//     was verified) nor deleted earlier in the run; DEL and UNLINK need no prior read;
//   - a key argument that is not in KEYS (AGENTS.md §3);
//   - any other command, which the run could not classify.
const wrappers = wrapperMark + `
local audit_in_keys, audit_read_ok, audit_deleted = {}, {}, {}
local audit_wrote, audit_epoch_read, audit_clock_reads = false, false, 0
local audit_violation = nil
for _, audit_key in ipairs(KEYS) do
  audit_in_keys[audit_key] = true
end
local function audit_fail(msg)
  if not audit_violation then audit_violation = msg end
  error(msg, 0)
end
local function audit_before(cmd, ...)
  local c, args = string.upper(cmd), { ... }
  local read = c == 'HGETALL' or c == 'HGET' or c == 'HEXISTS' or c == 'HLEN' or c == 'ZRANGE'
    or c == 'ZSCORE' or c == 'ZCARD' or c == 'GET' or c == 'TIME'
  local write = c == 'HSET' or c == 'HDEL' or c == 'HINCRBY' or c == 'ZADD' or c == 'ZREM'
  local del = c == 'DEL' or c == 'UNLINK'
  if not (read or write or del) then audit_fail('AUDIT ' .. c .. ' is not a command the discipline run knows') end
  local key_args = {}
  if del then
    key_args = args
  elseif c ~= 'TIME' then
    key_args = { args[1] }
  end
  for _, k in ipairs(key_args) do
    if not audit_in_keys[k] then audit_fail('AUDIT ' .. c .. ': key ' .. k .. ' is not in KEYS') end
  end
  local clock = c == 'TIME' or (c == 'GET' and args[1] == 'rl:ctl:time')
  if read and audit_wrote then audit_fail('AUDIT ' .. c .. ' reads after the first write') end
  if clock and audit_clock_reads > 0 then audit_fail('AUDIT ' .. c .. ' is a second clock read') end
  if clock and not audit_epoch_read then audit_fail('AUDIT ' .. c .. ' reads the clock before the epoch record') end
  if write and not (audit_read_ok[args[1]] or audit_deleted[args[1]]) then
    audit_fail('AUDIT ' .. c .. ' writes ' .. args[1] .. ', which was neither read nor deleted before')
  end
  return c, args, read, del, clock
end
local function audit_after(c, args, read, del, clock, ok)
  if clock then audit_clock_reads = audit_clock_reads + 1 end
  if read and args[1] == 'rl:epoch' then audit_epoch_read = true end
  if read and ok and c ~= 'TIME' then audit_read_ok[args[1]] = true end
  if not read then audit_wrote = true end
  if del then
    for _, k in ipairs(args) do
      audit_deleted[k] = true
    end
  end
end
local function audit_call(cmd, ...)
  local c, args, read, del, clock = audit_before(cmd, ...)
  local r = redis.call(cmd, ...)
  audit_after(c, args, read, del, clock, true)
  return r
end
local function audit_rpcall(cmd, ...)
  local c, args, read, del, clock = audit_before(cmd, ...)
  local r = redis.pcall(cmd, ...)
  audit_after(c, args, read, del, clock, not (type(r) == 'table' and r.err ~= nil))
  return r
end
local function audit_pcall_call(cmd, ...)
  local c, args, read, del, clock = audit_before(cmd, ...)
  local ok, r = pcall(redis.call, cmd, ...)
  audit_after(c, args, read, del, clock, ok)
  return ok, r
end
`
