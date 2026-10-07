//go:build rltest

package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
	"ratelimiter/limitertest"
)

// The op log (DESIGN §C4; IMPLEMENTATION_PLAN S5 step 11): JSONL, one object per line with a
// "type" field (plan choice), the records and fields exactly as step 11 lists them, in the
// grammar refmodel/replay.py reads:
//   - policy {rate_tokens, rate_period_ns, capacity, max_tracked_buckets, receipt_lifetime_ns},
//     logged right before the epoch record it applies to;
//   - epoch {epoch_id, origin_ns}: origin_ns is the start reply's budget time × 1000;
//   - clock {value_ns}: every controlled-time write of the clock driver;
//   - acquire {instance, key_b64url, result, decision_ns, retry_after_ns, u, g, s, expiry_us,
//     deduction_id, epoch_id}: u … expiry_us from the reply (0 where the reply has none);
//     decision_ns is the result's DecisionNs, or the reply's t × 1000 for a capacity_error or
//     a receipt_error, whose results carry none; for an allowed result deduction_id =
//     Permit.DeductionID() and epoch_id = limitertest.PermitEpoch, else '';
//   - release {instance, deduction_id} and done {deduction_id} (S6 step 10): a permit finish
//     through the driver (Release, Done), logged before the call; a permit released with its
//     own Release gets its release record when its final event arrives, before the refund
//     record; release's instance is the acquire record's; a degraded permit's deduction_id is
//     '';
//   - refund {deduction_id, key_b64url, g, s, expiry_us, attempts: [{code, t_us, credited}],
//     result} (S6 step 10): one per final event the driver's listener receives, written when
//     it arrives; key_b64url, g, s and expiry_us are the acquire record's; attempts are the
//     captured refund calls in order (RefundAttempt's Code, TUs and Credited: the §A4.9 code
//     or the outcome's name, the reply's t, and credited units or credit_us), and result the
//     event's result. A call still in flight when the limiter finalized the permit
//     (unresolved by the retention timer) is {"outcome_unknown", 0, 0} if it sent a command,
//     added to the written record when it returns later (orchestrator decision, S6 task 2);
//     so is every call with a final code (3, 4, 10, 11) in the record of an unresolved
//     event: the event counted it, but the retention timer took the permit before that reply
//     could end it (orchestrator decision, S6 review round 1);
//   - cleanup {key_b64url, t_us, code} (S7 step 11): one per cleanup_entry step result (codes
//     30–33) an instance's store adapter returns, before its store wrapper's faults, written
//     when it returns: a dropped reply keeps its record, while a refused step, which never
//     ran, a NOSCRIPT reply (its resend has the record) and a refusal (20–22, 25) have none.
//     key_b64url is the step's ix member (a key's enc is its unpadded base64url; a member
//     that fails is_enc, from damaged state alone, is not logged), code the reply's code (30
//     removed, 31 moved, 32 quarantined, 33 skipped) and t_us the reply's t, the step's
//     budget time (a gone or not_due skip carries it too) (orchestrator decision, S7 task 2);
//   - cleanup_scan {t_us} (orchestrator decision, S7 task 2): one per due_entries page (code
//     0) the adapter returns, the same way; t_us is the page's T0, which the page stored as
//     the high-water mark (CLK-4). A refusal stores nothing and has no record;
//   - restore {key_b64url, balance_units, t_us, g, epoch_id} and clear {key_b64url, t_us} (S8
//     step 5): one per rladmin restore or clear that exits 0 (a refused or failed one changed
//     nothing and has none; orchestrator decision, S8 task 3): balance_units as sent, t_us and
//     g from the output, epoch_id the --expect-epoch, which the fence matched;
//   - inspect {key_b64url, present, u, t_us, g, s, x_us, mc} (S8 step 5): a snapshot from
//     rladmin inspect's output, written only by LogInspect; a value the reply showed empty is
//     "";
//   - a policy and epoch pair for every epoch the driver starts or replaces, however: StartEpoch,
//     ReplaceEpoch, or an rladmin start-epoch that exits 0 (after a wipe, say), origin_ns being
//     its output's t_us × 1000 (S8 step 5).
//
// Ops are logged in completion order, so only tests that issue ops one at a time use the
// bridge; check_bud11 (teardown) sorts by budget time and takes concurrent logs too. Writes
// through Fixture, rl:ctl:time included, are not logged: a test that makes them does not use
// the bridge. A timed cleanup pass that runs while the test issues ops logs its records in
// completion order too, so a test that bridges keeps timed passes off (FastPolicy) or idle.

type policyRecord struct {
	Type              string `json:"type"`
	RateTokens        int64  `json:"rate_tokens"`
	RatePeriodNs      int64  `json:"rate_period_ns"`
	Capacity          int64  `json:"capacity"`
	MaxTrackedBuckets int64  `json:"max_tracked_buckets"`
	ReceiptLifetimeNs int64  `json:"receipt_lifetime_ns"`
}

type epochRecord struct {
	Type     string `json:"type"`
	EpochID  string `json:"epoch_id"`
	OriginNs int64  `json:"origin_ns"`
}

type clockRecord struct {
	Type    string `json:"type"`
	ValueNs int64  `json:"value_ns"`
}

type acquireRecord struct {
	Type         string `json:"type"`
	Instance     string `json:"instance"`
	KeyB64URL    string `json:"key_b64url"`
	Result       string `json:"result"`
	DecisionNs   int64  `json:"decision_ns"`
	RetryAfterNs int64  `json:"retry_after_ns"`
	U            int64  `json:"u"`
	G            int64  `json:"g"`
	S            int64  `json:"s"`
	ExpiryUs     int64  `json:"expiry_us"`
	DeductionID  string `json:"deduction_id"`
	EpochID      string `json:"epoch_id"`
}

type releaseRecord struct {
	Type        string `json:"type"`
	Instance    string `json:"instance"`
	DeductionID string `json:"deduction_id"`
}

type doneRecord struct {
	Type        string `json:"type"`
	DeductionID string `json:"deduction_id"`
}

type refundRecord struct {
	Type        string          `json:"type"`
	DeductionID string          `json:"deduction_id"`
	KeyB64URL   string          `json:"key_b64url"`
	G           int64           `json:"g"`
	S           int64           `json:"s"`
	ExpiryUs    int64           `json:"expiry_us"`
	Attempts    []attemptRecord `json:"attempts"` // never nil: an empty list is []
	Result      string          `json:"result"`
}

// attemptRecord is one refund attempt in a refund record: code is an int (a §A4.9 code) or a
// string (an outcome's name).
type attemptRecord struct {
	Code     any   `json:"code"`
	TUs      int64 `json:"t_us"`
	Credited int64 `json:"credited"`
}

type cleanupRecord struct {
	Type      string `json:"type"`
	KeyB64URL string `json:"key_b64url"`
	TUs       int64  `json:"t_us"`
	Code      int64  `json:"code"`
}

type cleanupScanRecord struct {
	Type string `json:"type"`
	TUs  int64  `json:"t_us"`
}

type restoreRecord struct {
	Type         string `json:"type"`
	KeyB64URL    string `json:"key_b64url"`
	BalanceUnits int64  `json:"balance_units"`
	TUs          int64  `json:"t_us"`
	G            int64  `json:"g"`
	EpochID      string `json:"epoch_id"`
}

type clearRecord struct {
	Type      string `json:"type"`
	KeyB64URL string `json:"key_b64url"`
	TUs       int64  `json:"t_us"`
}

// inspectRecord is an inspect snapshot: each of u … mc is an int64, or "" where the reply
// showed the empty string (missing or unreadable, §A4.9).
type inspectRecord struct {
	Type      string `json:"type"`
	KeyB64URL string `json:"key_b64url"`
	Present   int64  `json:"present"`
	U         any    `json:"u"`
	TUs       any    `json:"t_us"`
	G         any    `json:"g"`
	S         any    `json:"s"`
	XUs       any    `json:"x_us"`
	MC        any    `json:"mc"`
}

func policyRecordOf(p limiter.Policy) policyRecord {
	return policyRecord{
		Type: "policy", RateTokens: p.RateTokens, RatePeriodNs: p.RatePeriodNs, Capacity: p.Capacity,
		MaxTrackedBuckets: p.MaxTrackedBuckets, ReceiptLifetimeNs: p.ReceiptLifetimeNs,
	}
}

// acquireRecordOf is op's acquire record. The reply gives u, g, s and expiry_us: all four
// for E_ALLOWED (code 1), u for E_LIMITED (code 2). decision_ns is the result's DecisionNs,
// except for capacity_error and receipt_error, whose results carry none: there it is the
// reply's budget time × 1000 (E_CAPACITY {24, t, reason}; for receipt_error the E_ALLOWED
// reply whose signing failed, its charge kept, API-12a), the decision time the reference
// model gives it.
func acquireRecordOf(op Op, tag string) acquireRecord {
	instance := strconv.Itoa(op.Instance)
	if tag != "" {
		instance += " request_id=" + tag
	}
	rec := acquireRecord{
		Type: "acquire", Instance: instance, KeyB64URL: keyB64URL(op.Key),
		Result: op.Kind.String(), DecisionNs: op.DecisionNs, RetryAfterNs: op.RetryAfterNs,
	}
	if op.Replied {
		switch op.Reply.Code {
		case 1:
			rec.U, rec.G, rec.S, rec.ExpiryUs = elemInt(op.Reply, 3), elemInt(op.Reply, 4), elemInt(op.Reply, 5), elemInt(op.Reply, 6)
			if op.Kind == limiter.ReceiptError {
				rec.DecisionNs = op.Reply.TUs * 1000
			}
		case 2:
			rec.U = elemInt(op.Reply, 3)
		case 24:
			if op.Kind == limiter.CapacityError {
				rec.DecisionNs = op.Reply.TUs * 1000
			}
		}
	}
	if op.Kind == limiter.Allowed && op.Permit != nil {
		rec.DeductionID, rec.EpochID = op.Permit.DeductionID(), limitertest.PermitEpoch(op.Permit)
	}
	return rec
}

// logAcquire appends op's acquire record, made on instance in, and tracks its permit.
func (d *Driver) logAcquire(in *instance, op Op, tag string) {
	rec := acquireRecordOf(op, tag)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logged++
	d.trackLocked(in, rec)
	d.appendLocked(rec)
}

// logClock appends a clock record; the clock calls it after each write.
func (d *Driver) logClock(ns int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.appendLocked(clockRecord{Type: "clock", ValueNs: ns})
}

// appendLocked appends one line per record; d.mu is held.
func (d *Driver) appendLocked(recs ...any) {
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			d.t.Errorf("driver: op log record %T: %v", r, err)
			continue
		}
		d.oplog = append(d.oplog, b)
	}
}

// OpLog returns the op log so far, one record per element, without newlines.
func (d *Driver) OpLog() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.oplog))
	for i, b := range d.oplog {
		out[i] = string(b)
	}
	return out
}

// SaveOpLog writes the op log so far to path (mode 0600), one record per '\n'-terminated
// line: T-COR-1 keeps its logs under .out/.
func (d *Driver) SaveOpLog(path string) error {
	d.mu.Lock()
	lines := slices.Clone(d.oplog)
	d.mu.Unlock()
	if err := os.WriteFile(path, joinLines(lines), 0o600); err != nil {
		return fmt.Errorf("driver: writing the op log: %w", err)
	}
	return nil
}

// joinLines is the op log's file content.
func joinLines(lines [][]byte) []byte {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// RunRefModel runs python3 -m <module> <args…> from the repo root (the directory holding
// go.mod), within a minute of ctx, and returns its exit code and combined output. err
// reports that it could not run: no python3 on PATH, no repo root, or no exit status (a
// failure for the caller, never a skip).
func RunRefModel(ctx context.Context, module string, args ...string) (int, []byte, error) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		return 0, nil, fmt.Errorf("the repo root: %w", err)
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		return 0, nil, fmt.Errorf("the reference model needs python3, not found on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, append([]string{"-m", module}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, out, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		return exit.ExitCode(), out, nil
	}
	return 0, out, fmt.Errorf("python3 -m %s: %w", module, err)
}

// CheckRefModel is the Go ↔ Python bridge (DESIGN §C4): it writes the op log to
// <t.TempDir()>/oplog.jsonl and runs python3 -m refmodel.replay, then python3 -m
// refmodel.check_bud11 (S6 step 10), on it from the repo root. Both must exit 0; a non-zero
// exit, or no python3 on PATH, fails t with t.Fatalf, never a skip.
func (d *Driver) CheckRefModel(t testing.TB) {
	t.Helper()
	d.mu.Lock()
	lines := slices.Clone(d.oplog)
	d.mu.Unlock()
	path := filepath.Join(t.TempDir(), "oplog.jsonl")
	if err := os.WriteFile(path, joinLines(lines), 0o600); err != nil {
		t.Fatalf("driver: writing the op log: %v", err)
		return
	}
	for _, module := range []string{"refmodel.replay", "refmodel.check_bud11"} {
		code, out, err := RunRefModel(t.Context(), module, path)
		if err != nil {
			t.Fatalf("driver: %v (a failure, never a skip)", err)
			return
		}
		if code != 0 {
			t.Fatalf("driver: %s rejects the op log (exit status %d):\n%s\nop log, %d records:\n%s", module, code, out, len(lines), excerpt(lines))
			return
		}
	}
}

// excerpt is the op log for a failure message: numbered lines, at most the first 200.
func excerpt(lines [][]byte) string {
	var b strings.Builder
	for i, l := range lines {
		if i == 200 {
			b.WriteString("…\n")
			break
		}
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(": ")
		b.Write(l)
		b.WriteByte('\n')
	}
	return b.String()
}
