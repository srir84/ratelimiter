//go:build rltest

package driver

import (
	"context"
	"slices"
	"testing"

	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/limiter"
)

// lateGuardAdapter is a store adapter whose every call returns res: the driver's recorder
// sits on it as it sits on an instance's store wrapper.
type lateGuardAdapter struct {
	store.Adapter
	res store.Response
}

func (c lateGuardAdapter) Call(context.Context, store.Request) store.Response { return c.res }

// TestDriverLateRefundGuard (orchestrator decision, S6 task 2; F1; AGENTS.md §5, checkers are
// tested too): the guard for a refund call that returns after its permit's first final event
// reached the driver (the recorder's late branch and lateLocked). The limiter no longer gets
// there, since its retention event waits for the call in flight, so this synthetic test is
// what covers the guard. A refund record is written through noteEvent for an unresolved
// (retention) event after one refused call, and a done record follows it. A late call, through
// the recorder, is captured and added to the written record as its lateRecord: the op-log line
// is rewritten in place, as written here by hand, with a call that sent a command as
// {"outcome_unknown", 0, 0} whatever its reply, and one that sent nothing with its own code.
// The other lines are unchanged, and no line is added.
func TestDriverLateRefundGuard(t *testing.T) {
	const epochHex = "0123456789abcdef0123456789abcdef"
	// A refund call's ARGV (§A4.1): epoch_id, fp, enc, the receipt's epoch_id, g, s, expiry_us.
	req := store.Request{
		Kind: store.Refund, Script: scripts.Refund,
		Args: []string{epochHex, "fp", "aw", epochHex, "1", "7", "1760000060000000"},
	}
	id := deductionOf(req)
	if id == "" {
		t.Fatal("deductionOf found no deduction_id in the refund call's ARGV")
	}
	releaseLine := `{"type":"release","instance":"0","deduction_id":"` + id + `"}`
	refundLine := func(attempts string) string {
		return `{"type":"refund","deduction_id":"` + id + `","key_b64url":"aw","g":1,"s":7,"expiry_us":1760000060000000,"attempts":` +
			attempts + `,"result":"unresolved"}`
	}
	doneLine := `{"type":"done","deduction_id":"d1_other"}`
	for _, c := range []struct {
		name  string
		late  store.Response
		entry string // the late call's entry, by hand
	}{
		{"E_APPLIED, one command sent", capturedReply(3, 1_760_000_000_000_000, "", 1_000_000_000), unknownEntry},
		{"E_RECEIPT_EXPIRED, one command sent", capturedReply(11, 1_760_000_000_000_000, "expired"), unknownEntry},
		{"no reply, one command sent", store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1}, unknownEntry},
		{"refused, nothing sent", store.Response{Outcome: store.Unavailable}, refusedEntry},
		{"cancelled, nothing sent", store.Response{Outcome: store.Cancelled}, `{"code":"cancelled","t_us":0,"credited":0}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &Driver{t: t, permits: map[string]*permitRec{}, attempts: map[string][]RefundAttempt{}}
			in := &instance{}
			d.mu.Lock()
			d.trackLocked(in, acquireRecord{
				Type: "acquire", Instance: "0", KeyB64URL: "aw", Result: limiter.Allowed.String(),
				G: 1, S: 7, ExpiryUs: 1_760_000_060_000_000, DeductionID: id,
			})
			d.mu.Unlock()
			recorder{Adapter: lateGuardAdapter{res: store.Response{Outcome: store.Unavailable}}, d: d, inst: in}.refund(t.Context(), req)
			d.noteEvent(in, limiter.RefundEvent{DeductionID: id, Result: limiter.Unresolved, Reason: limiter.UnresolvedRetention})
			d.mu.Lock()
			d.appendLocked(doneRecord{Type: "done", DeductionID: "d1_other"})
			d.mu.Unlock()
			want := []string{releaseLine, refundLine(attemptsList(refusedEntry)), doneLine}
			if got := d.OpLog(); !slices.Equal(got, want) {
				t.Fatalf("op log before the late call\n got %q\nwant %q", got, want)
			}

			res := recorder{Adapter: lateGuardAdapter{res: c.late}, d: d, inst: in}.refund(t.Context(), req)
			if res.Outcome != c.late.Outcome || res.SentCommands != c.late.SentCommands {
				t.Errorf("the recorder returned %v with %d commands sent, want the adapter's %v with %d", res.Outcome, res.SentCommands, c.late.Outcome, c.late.SentCommands)
			}
			want = []string{releaseLine, refundLine(attemptsList(refusedEntry, c.entry)), doneLine}
			if got := d.OpLog(); !slices.Equal(got, want) {
				t.Errorf("op log after the late call\n got %q\nwant %q", got, want)
			}
			d.mu.Lock()
			captured := d.attempts[id]
			d.mu.Unlock()
			if len(captured) != 2 {
				t.Fatalf("%d calls captured for the permit, want 2", len(captured))
			}
			if last := captured[1]; last.Response.Outcome != c.late.Outcome || last.DeductionID != id {
				t.Errorf("the last call captured: %v for %q, want the late call's %v for %q", last.Response.Outcome, last.DeductionID, c.late.Outcome, id)
			}
		})
	}
}
