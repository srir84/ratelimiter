package main

import (
	"math/rand/v2"
	"net/http"
	"testing"
	"time"
)

// TestRetry_WaitAndHintChoice (plan H2 step 5, TRF-5): the hint is the larger of
// retry_after_ms and Retry-After × 1,000 ms, a tie going to retry_after_ms: (Retry-After: 1,
// retry_after_ms: 200) waits 1,000 ms from Retry-After, (1, 1,500) 1,500 ms from
// retry_after_ms; a missing or malformed value does not count, and a 429 with neither is not
// retried. Sent for real, each retry goes to its entry's instance with the same IDs, keeps its
// entry's planned_ns and instance in gen.jsonl, and reaches the server at least hint + jitter
// after the 429 left it, the 429 leaving 300 ms after its request arrived (so a wait counted
// from the request's write or the attempt's start is too short); a 503 and a 400 carrying the
// same hints are not retried; each jitter is in [0, 50] ms, drawn from its own (i, attempt)
// stream, so two runs of the same schedule and seed draw the same jitters.
func TestRetry_WaitAndHintChoice(t *testing.T) {
	ms := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		name     string
		bodyMs   *int64
		header   []string
		wantMs   int64
		wantFrom string // "" for no hint
	}{
		{"Retry-After larger", ms(200), []string{"1"}, 1000, "Retry-After"},
		{"retry_after_ms larger", ms(1500), []string{"1"}, 1500, "retry_after_ms"},
		{"a tie goes to retry_after_ms", ms(1000), []string{"1"}, 1000, "retry_after_ms"},
		{"no Retry-After", ms(200), nil, 200, "retry_after_ms"},
		{"no retry_after_ms", nil, []string{"2"}, 2000, "Retry-After"},
		{"Retry-After 0", nil, []string{"0"}, 0, "Retry-After"},
		{"retry_after_ms 0", ms(0), nil, 0, "retry_after_ms"},
		{"a negative retry_after_ms", ms(-1), []string{"1"}, 1000, "Retry-After"},
		{"retry_after_ms past the largest hint", ms(1_000_000_000_001), []string{"1"}, 1000, "Retry-After"},
		{"retry_after_ms at the largest hint", ms(1_000_000_000_000), nil, 1_000_000_000_000, "retry_after_ms"},
		{"Retry-After at the largest hint", nil, []string{"1000000000"}, 1_000_000_000_000, "Retry-After"},
		{"Retry-After past the largest hint", ms(5), []string{"1000000001"}, 5, "retry_after_ms"},
		{"Retry-After as a date", ms(200), []string{"Wed, 21 Oct 2026 07:28:00 GMT"}, 200, "retry_after_ms"},
		{"Retry-After with a sign", ms(200), []string{"+3"}, 200, "retry_after_ms"},
		{"Retry-After with a fraction", ms(200), []string{"1.5"}, 200, "retry_after_ms"},
		{"Retry-After empty", ms(200), []string{""}, 200, "retry_after_ms"},
		{"two Retry-After values", ms(200), []string{"1", "2"}, 200, "retry_after_ms"},
		{"neither", nil, nil, 0, ""},
		{"neither valid", ms(-5), []string{"soon"}, 0, ""},
	} {
		h, ok := chooseHint(tc.bodyMs, tc.header)
		switch {
		case tc.wantFrom == "" && ok:
			t.Errorf("%s: hint %d ms from %s, want none", tc.name, h.ms, h.source)
		case tc.wantFrom != "" && (!ok || h.ms != tc.wantMs || h.source != tc.wantFrom):
			t.Errorf("%s: hint %d ms from %q (ok %v), want %d ms from %s", tc.name, h.ms, h.source, ok, tc.wantMs, tc.wantFrom)
		}
	}

	// The jitter depends on (seed, i, attempt) alone, and stays in [0, jitter_max_ms].
	for i := range 200 {
		for attempt := 2; attempt <= 3; attempt++ {
			j := jitterNs(5, i, attempt, 50)
			if j < 0 || j > 50_000_000 {
				t.Fatalf("jitter of entry %d attempt %d is %d ns, want 0 to 50 ms", i, attempt, j)
			}
			if again := jitterNs(5, i, attempt, 50); again != j {
				t.Fatalf("jitter of entry %d attempt %d drawn twice: %d then %d, want equal", i, attempt, j, again)
			}
		}
	}
	if a, b := jitterNs(5, 7, 2, 50), jitterNs(5, 7, 3, 50); a == b {
		t.Errorf("entry 7's attempts 2 and 3 draw the same jitter %d, want each attempt its own stream", a)
	}
	if a, b := jitterNs(5, 7, 2, 50), jitterNs(6, 7, 2, 50); a == b {
		t.Errorf("seeds 5 and 6 draw the same jitter %d for entry 7, want the seed to matter", a)
	}
	if j := jitterNs(5, 7, 2, 0); j != 0 {
		t.Errorf("jitter_max_ms 0 draws %d ns, want 0", j)
	}

	// Entry 0 goes to instance 1, entry 1 to instance 2. Entry 0's 429 says (1 s, 200 ms) and
	// entry 1's (1 s, 1,500 ms); each retry is answered 200. Entry 2 gets a 503 and entry 3 a
	// 400 with the same hints as entry 0, and neither is retried: only a 429 is. sendAll's seed is 7, so retry
	// (i, 2)'s jitter is rand.New(rand.NewPCG(7, 1<<32 | i<<2 | 2)).Int64N(50,000,001) ns (plan H2
	// step 5, with the retry's own attempt number).
	entries := []Entry{
		{I: 0, PlannedNs: 10_000_000, User: "u000000", RequestID: "r000000000", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 1, PlannedNs: 20_000_000, User: "u000001", RequestID: "r000000001", Instance: 2, Kind: "initial", DuplicateOf: -1},
		{I: 2, PlannedNs: 30_000_000, User: "u000002", RequestID: "r000000002", Instance: 1, Kind: "initial", DuplicateOf: -1},
		{I: 3, PlannedNs: 40_000_000, User: "u000003", RequestID: "r000000003", Instance: 2, Kind: "initial", DuplicateOf: -1},
	}
	hints := map[string]struct {
		i      int
		header string
		bodyMs int64
		wantMs int64
		from   string
	}{
		"r000000000": {0, "1", 200, 1000, "Retry-After"},
		"r000000001": {1, "1", 1500, 1500, "retry_after_ms"},
	}
	answer := func(_ *stub, w http.ResponseWriter, r *http.Request, q stubReq) {
		switch {
		case q.request == "r000000002":
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "limiter_unavailable", "message": "unavailable",
				"retry_after_ms": 200})
			return
		case q.request == "r000000003":
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": "invalid request",
				"retry_after_ms": 200})
			return
		case q.seen > 0:
			answerOK(w, r)
			return
		}
		select { // the 429 leaves 300 ms after its request arrived
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
		}
		h := hints[q.request]
		answerLimited(w, r, h.header, h.bodyMs)
	}
	var jitters [2]map[string]int64
	for run := range jitters {
		stubs := []*stub{newStub(t, answer), newStub(t, answer)}
		lines := sendAll(t, entries, []string{stubs[0].URL + "/hit", stubs[1].URL + "/hit"}, nil)
		jitters[run] = map[string]int64{}
		recs := attempts(t, lines)
		if n, logged := len(stubs[0].requests("r000000002")), linesFor(recs, "r000000002"); n != 1 || len(logged) != 1 ||
			logged[0].Status != http.StatusServiceUnavailable {
			t.Errorf("run %d: the 503 with hints arrived %d times with %d lines, want 1 and 1 line with status 503", run, n, len(logged))
		}
		if n, logged := len(stubs[1].requests("r000000003")), linesFor(recs, "r000000003"); n != 1 || len(logged) != 1 ||
			logged[0].Status != http.StatusBadRequest {
			t.Errorf("run %d: the 400 with hints arrived %d times with %d lines, want 1 and 1 line with status 400", run, n, len(logged))
		}
		for _, l := range recs {
			if l.Attempt != 2 {
				continue
			}
			want := hints[l.RequestID]
			if l.Kind != "retry" || l.HintMs == nil || *l.HintMs != want.wantMs || l.HintSource == nil || *l.HintSource != want.from {
				t.Errorf("run %d: %s's retry: kind %s, hint %s from %s, want a retry with %d ms from %s",
					run, l.RequestID, l.Kind, show(l.HintMs), show(l.HintSource), want.wantMs, want.from)
			}
			if e := entries[want.i]; l.PlannedNs != e.PlannedNs || l.Instance != e.Instance {
				t.Errorf("run %d: %s's retry line has planned_ns %d and instance %d, want its entry's (and attempt 1's) %d and %d",
					run, l.RequestID, l.PlannedNs, l.Instance, e.PlannedNs, e.Instance)
			}
			if first := linesFor(recs, l.RequestID); len(first) == 0 || first[0].Attempt != 1 || first[0].PlannedNs != l.PlannedNs ||
				first[0].Instance != l.Instance {
				t.Errorf("run %d: %s's retry line differs from its attempt-1 line in planned_ns or instance", run, l.RequestID)
			}
			stream := uint64(1)<<32 | uint64(want.i)<<2 | 2
			wantJitter := rand.New(rand.NewPCG(7, stream)).Int64N(50_000_001)
			if l.JitterNs == nil || *l.JitterNs != wantJitter {
				t.Errorf("run %d: %s's retry has jitter_ns %s, want %d from its own stream", run, l.RequestID, show(l.JitterNs), wantJitter)
				continue
			}
			jitters[run][l.RequestID] = *l.JitterNs
			own, other := stubs[entries[want.i].Instance-1], stubs[2-entries[want.i].Instance]
			reqs := own.requests(l.RequestID)
			if len(reqs) != 2 || len(other.requests(l.RequestID)) != 0 {
				t.Errorf("run %d: %s arrived %d times at its instance and %d times at the other, want 2 and 0",
					run, l.RequestID, len(reqs), len(other.requests(l.RequestID)))
				continue
			}
			if reqs[1].user != reqs[0].user || reqs[1].user != entries[want.i].User {
				t.Errorf("run %d: %s arrived as %s, then %s; want %s both times", run, l.RequestID, reqs[0].user, reqs[1].user, entries[want.i].User)
			}
			wait := time.Duration(want.wantMs)*time.Millisecond + time.Duration(*l.JitterNs)
			if gap := reqs[1].at - reqs[0].answered; gap < wait {
				t.Errorf("run %d: %s's retry arrived %v after its slow 429 was answered, want at least hint + jitter = %v",
					run, l.RequestID, gap, wait)
			}
		}
		if len(jitters[run]) != 2 {
			t.Errorf("run %d: %d retries with the expected jitter, want 2", run, len(jitters[run]))
		}
	}
	for id, j := range jitters[0] {
		if jitters[1][id] != j {
			t.Errorf("%s's retry jitter is %d ns in run 0 and %d ns in run 1, want identical", id, j, jitters[1][id])
		}
	}
}

// linesFor returns the attempt lines of request ID id.
func linesFor(recs []genRec, id string) []genRec {
	var out []genRec
	for _, r := range recs {
		if r.RequestID == id {
			out = append(out, r)
		}
	}
	return out
}
