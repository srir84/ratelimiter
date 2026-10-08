package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"ratelimiter/harness/dbclient"
	"ratelimiter/internal/testutil/golden"
	"ratelimiter/limiter"
)

// goldenAPI is the hand-written golden api.jsonl (DESIGN §C5; plan H1 step 8).
const goldenAPI = "../runlog/testdata/golden/api.jsonl"

// TestRunlogGolden_Requests: one allowed request whose insert fails with no row (FailedNoRow:
// release() and 503 database_unavailable), then its applied refund event, write 4 lines,
// each matching its golden api.jsonl line (golden.Match on kind, result, reason and error).
// The decision record is written before the database work, and the lines carry the
// request's values: one user_id and deduction_id, the decision's decision_ns, the IDs and
// arrival_ns the database got, durations covering the fakes' sleeps (§B8).
func TestRunlogGolden_Requests(t *testing.T) {
	const acquireSleep, dbSleep = 2 * time.Millisecond, 3 * time.Millisecond
	p := &fakePermit{id: "d1_Kx3vQ9mZt2LwB7nR5yHc0A"}
	cfg := testConfig()
	cfg.InstanceNum = 3
	lim := &fakeLimiter{acquire: func(context.Context, string) Decision {
		time.Sleep(acquireSleep)
		return Decision{Kind: limiter.Allowed, DecisionNs: testDecisionNs, Permit: p}
	}}
	var ts *testServer
	decisionBeforeDB := false
	db := &fakeDB{hit: func(context.Context, string, string, int64) dbclient.Result {
		for _, line := range ts.lines(t) { // every line enqueued before the database work
			if m := decodeJSON(t, line); m["kind"] == kindDecision && m["deduction_id"] == p.id {
				decisionBeforeDB = true
			}
		}
		time.Sleep(dbSleep)
		return dbclient.Result{Class: dbclient.FailedNoRow, Err: errors.New("connection refused")}
	}}
	ts = newTestServer(t, cfg, lim, db)
	checkResponse(t, ts.get(t, ids1("alice", "req-1")), http.StatusServiceUnavailable, errDBUnavailable)
	age := time.Since(cfg.ProcessStart).Nanoseconds()
	if done, released := p.finished(); done != 0 || released != 1 {
		t.Fatalf("done %d, released %d; want 0, 1", done, released)
	}
	if !decisionBeforeDB {
		t.Error("the decision record was not on the run log when the database work began")
	}
	RefundListener(ts.log)(limiter.RefundEvent{
		DeductionID:      p.id,
		Result:           limiter.Applied,
		Attempts:         1,
		CreditDecisionNs: testDecisionNs,
		ReleaseMonoNs:    int64(time.Second),
		FinalMonoNs:      int64(2 * time.Second),
	})
	lines := ts.lines(t)
	if len(lines) != 4 {
		t.Fatalf("%d lines, want 4 (decision, release, request, refund): %q", len(lines), lines)
	}
	golden.Match(t, lines, goldenAPI, "kind", "result", "reason", "error")
	var kinds []string
	for _, line := range lines {
		kinds = append(kinds, decodeJSON(t, line)["kind"].(string))
	}
	if want := []string{kindDecision, kindRelease, kindRequest, kindRefund}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}

	dec, rel, req, ref := decodeJSON(t, lines[0]), decodeJSON(t, lines[1]), decodeJSON(t, lines[2]), decodeJSON(t, lines[3])
	for i, m := range []map[string]any{dec, rel, req, ref} {
		if m["deduction_id"] != p.id {
			t.Errorf("%s deduction_id %v, want %s", kinds[i], m["deduction_id"], p.id)
		}
	}
	if dec["user_id"] != "alice" || req["user_id"] != "alice" {
		t.Errorf("user_id %v (decision), %v (request), want alice in both", dec["user_id"], req["user_id"])
	}
	if num(t, dec, "decision_ns") != testDecisionNs || num(t, req, "decision_ns") != testDecisionNs {
		t.Errorf("decision_ns %d (decision), %d (request), want the acquire's %d in both",
			num(t, dec, "decision_ns"), num(t, req, "decision_ns"), testDecisionNs)
	}
	calls := ts.db.recorded()
	if len(calls) != 1 || calls[0].userID != "alice" || calls[0].requestID != "req-1" {
		t.Fatalf("database calls %+v, want one for (alice, req-1)", calls)
	}
	if req["request_id"] != calls[0].requestID || num(t, req, "arrival_ns") != calls[0].arrivalNs {
		t.Errorf("request record request_id %v, arrival_ns %d; want the database call's %s, %d",
			req["request_id"], num(t, req, "arrival_ns"), calls[0].requestID, calls[0].arrivalNs)
	}
	if req["run_id"] != "20261007-120000-test-1" || num(t, req, "instance") != 3 || req["epoch_id"] != "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f" {
		t.Errorf("request record run_id %v, instance %d, epoch_id %v; want the --run-id, --instance 3 and Health's epoch",
			req["run_id"], num(t, req, "instance"), req["epoch_id"])
	}
	limiterNs, dbNs, totalNs := num(t, req, "limiter_ns"), num(t, req, "db_ns"), num(t, req, "total_ns")
	if limiterNs < int64(acquireSleep) || dbNs < int64(dbSleep) || totalNs < limiterNs+dbNs {
		t.Errorf("limiter_ns %d, db_ns %d, total_ns %d; want at least %d, %d and their sum", limiterNs, dbNs, totalNs, acquireSleep, dbSleep)
	}
	if got := num(t, rel, "release_mono_ns"); got <= 0 || got > age {
		t.Errorf("release_mono_ns %d, want in (0, %d], ns since the process started", got, age)
	}
	if num(t, ref, "credit_decision_ns") != num(t, dec, "decision_ns") || num(t, ref, "attempts") != 1 ||
		num(t, ref, "release_mono_ns") != int64(time.Second) || num(t, ref, "final_mono_ns") != int64(2*time.Second) {
		t.Errorf("refund record %v, want the event's credit_decision_ns (the decision's), attempts 1, release 1 s, final 2 s", ref)
	}
}

// TestRunlogGolden_Lifecycle: the start, ready and stop records match their golden lines
// (golden.Match on kind and event), and start carries the configured limits, queue length 0
// and the file limit; stop carries both drop counts after Stop closed the limiter.
func TestRunlogGolden_Lifecycle(t *testing.T) {
	lim := &fakeLimiter{acquire: allowedWith(&fakePermit{})}
	ts := newTestServer(t, testConfig(), lim, &fakeDB{hit: committed()})
	lim.health.ListenerDrops = 3
	ts.s.LogStart()
	ts.s.LogReady()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := ts.s.Stop(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	lines := ts.lines(t)
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3: %q", len(lines), lines)
	}
	golden.Match(t, lines, goldenAPI, "kind", "event")
	start, stop := decodeJSON(t, lines[0]), decodeJSON(t, lines[2])
	for key, want := range map[string]int64{
		"max_in_flight":          512,
		"queue_len":              0,
		"max_release_delay_ns":   5_000_000_000,
		"dependency_deadline_ns": 2_000_000_000,
		"drain_timeout_ns":       35_000_000_000,
		"nofile_limit":           8192,
	} {
		if got := num(t, start, key); got != want {
			t.Errorf("start %s = %d, want %d", key, got, want)
		}
	}
	if num(t, stop, "listener_drops") != 3 || num(t, stop, "log_drops") != 0 {
		t.Errorf("stop %v, want listener_drops 3 and log_drops 0", stop)
	}
	if lim.closed != 1 {
		t.Errorf("limiter closed %d times, want 1", lim.closed)
	}
}
