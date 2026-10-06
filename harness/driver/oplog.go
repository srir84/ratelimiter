//go:build rltest

package driver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
//     Permit.DeductionID() and epoch_id = limitertest.PermitEpoch, else ''.
//
// Ops are logged in completion order, so only tests that issue ops one at a time use the
// bridge. Writes through Fixture, rl:ctl:time included, are not logged: a test that makes them
// does not use the bridge.

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
		Type: "acquire", Instance: instance, KeyB64URL: base64.RawURLEncoding.EncodeToString([]byte(op.Key)),
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

// logAcquire appends op's acquire record.
func (d *Driver) logAcquire(op Op, tag string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.appendLocked(acquireRecordOf(op, tag))
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

// CheckRefModel is the Go ↔ Python bridge (DESIGN §C4): it writes the op log to
// <t.TempDir()>/oplog.jsonl and runs python3 -m refmodel.replay on it from the repo root (the
// directory holding go.mod). Exit 0 passes; a non-zero exit, or no python3 on PATH, fails t
// with t.Fatalf, never a skip.
func (d *Driver) CheckRefModel(t testing.TB) {
	t.Helper()
	d.mu.Lock()
	lines := slices.Clone(d.oplog)
	d.mu.Unlock()
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "oplog.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("driver: writing the op log: %v", err)
		return
	}
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatalf("driver: the repo root: %v", err)
		return
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("driver: the reference model needs python3, not found on PATH (a failure, never a skip): %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-m", "refmodel.replay", path)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driver: refmodel.replay rejects the op log (%v):\n%s\nop log, %d records:\n%s", err, out, len(lines), excerpt(lines))
		return
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
