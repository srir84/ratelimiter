//go:build rltest

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"ratelimiter/harness/config"
)

// digits matches a decimal string of digits (arrival_ns, decision_ns; HTTP-5, HTTP-6).
var digits = regexp.MustCompile(`^[0-9]+$`)

// headers builds /hit headers with every value given, so a header can be missing (nil) or
// repeated.
func headers(users, requests []string) http.Header {
	h := http.Header{}
	for _, v := range users {
		h.Add("X-User-ID", v)
	}
	for _, v := range requests {
		h.Add("X-Request-ID", v)
	}
	return h
}

// one is a single header value.
func one(v string) []string { return []string{v} }

// wantError checks an error response's status and stable code (HTTP-7, ERR table).
func wantError(t *testing.T, what string, r Response, status int, code string) {
	t.Helper()
	if r.Status != status || r.JSON == nil || r.JSON["error"] != code {
		t.Errorf("%s: %d %s, want %d with error %q", what, r.Status, r.Body, status, code)
	}
}

// wantCreated checks a 200 that inserted the row (existed false) for user.
func wantCreated(t *testing.T, what string, r Response, user, request string) {
	t.Helper()
	if r.Status != http.StatusOK || r.JSON == nil || r.JSON["existed"] != false ||
		r.JSON["user_id"] != user || r.JSON["request_id"] != request {
		t.Errorf("%s: %d %s, want 200 with existed false for (%s, %s)", what, r.Status, r.Body, user, request)
	}
}

// TestT_HTTP_1 (HARNESS §8): missing or duplicate X-User-ID or X-Request-ID, invalid
// characters, length 0 and 65 → 400 invalid_request (ERR-5); POST /hit → 405 (ERR-6); an
// unknown route → 404 (ERR-6). None acquires (rl_acquire_results_total unchanged) or writes a
// row. Lengths 64 and 1 pass, as controls.
func TestT_HTTP_1(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	long64, long65 := strings.Repeat("a", 64), strings.Repeat("b", 65)

	before := api.AcquireTotal(t)
	for _, c := range []struct {
		name, method, path string
		h                  http.Header
		status             int
		code               string
	}{
		{"no headers", http.MethodGet, "/hit", headers(nil, nil), 400, "invalid_request"},
		{"no X-User-ID", http.MethodGet, "/hit", headers(nil, one("r1")), 400, "invalid_request"},
		{"no X-Request-ID", http.MethodGet, "/hit", headers(one("u1"), nil), 400, "invalid_request"},
		{"two X-User-ID", http.MethodGet, "/hit", headers([]string{"u1", "u2"}, one("r1")), 400, "invalid_request"},
		{"two equal X-User-ID", http.MethodGet, "/hit", headers([]string{"u1", "u1"}, one("r1")), 400, "invalid_request"},
		{"two X-Request-ID", http.MethodGet, "/hit", headers(one("u1"), []string{"r1", "r2"}), 400, "invalid_request"},
		{"space in X-User-ID", http.MethodGet, "/hit", headers(one("u 1"), one("r1")), 400, "invalid_request"},
		{"dot in X-User-ID", http.MethodGet, "/hit", headers(one("u.1"), one("r1")), 400, "invalid_request"},
		{"non-ASCII X-User-ID", http.MethodGet, "/hit", headers(one("ü1"), one("r1")), 400, "invalid_request"},
		{"slash in X-Request-ID", http.MethodGet, "/hit", headers(one("u1"), one("r/1")), 400, "invalid_request"},
		{"colon in X-Request-ID", http.MethodGet, "/hit", headers(one("u1"), one("r:1")), 400, "invalid_request"},
		{"plus in X-Request-ID", http.MethodGet, "/hit", headers(one("u1"), one("r+1")), 400, "invalid_request"},
		{"empty X-User-ID", http.MethodGet, "/hit", headers(one(""), one("r1")), 400, "invalid_request"},
		{"empty X-Request-ID", http.MethodGet, "/hit", headers(one("u1"), one("")), 400, "invalid_request"},
		{"65-character X-User-ID", http.MethodGet, "/hit", headers(one(long65), one("r1")), 400, "invalid_request"},
		{"65-character X-Request-ID", http.MethodGet, "/hit", headers(one("u1"), one(long65)), 400, "invalid_request"},
		{"POST /hit", http.MethodPost, "/hit", headers(one("u1"), one("r1")), 405, "method_not_allowed"},
		{"DELETE /hit", http.MethodDelete, "/hit", headers(one("u1"), one("r1")), 405, "method_not_allowed"},
		{"GET /nope", http.MethodGet, "/nope", headers(one("u1"), one("r1")), 404, "not_found"},
		{"GET /hits", http.MethodGet, "/hits", headers(one("u1"), one("r1")), 404, "not_found"},
		{"GET /", http.MethodGet, "/", nil, 404, "not_found"},
	} {
		r := api.Do(t, c.method, c.path, c.h, "")
		wantError(t, c.name, r, c.status, c.code)
		if c.status == http.StatusMethodNotAllowed && r.Header.Get("Allow") != http.MethodGet {
			t.Errorf("%s: Allow %q, want GET", c.name, r.Header.Get("Allow"))
		}
	}
	if after := api.AcquireTotal(t); after != before {
		t.Errorf("rl_acquire_results_total went from %d to %d over the invalid requests, want unchanged", before, after)
	}
	if n := s.rows(t); n != 0 {
		t.Errorf("%d rows after the invalid requests, want 0", n)
	}
	if recs := api.Records(t, "decision"); len(recs) != 0 {
		t.Errorf("decision records %v after the invalid requests, want none", recs)
	}

	// Lengths 64 and 1 are valid.
	wantCreated(t, "64-character X-User-ID, 1-character X-Request-ID", api.Hit(t, long64, "r"), long64, "r")
	wantCreated(t, "1-character X-User-ID, 64-character X-Request-ID", api.Hit(t, "u", long64), "u", long64)
	if after := api.AcquireTotal(t); after != before+2 {
		t.Errorf("rl_acquire_results_total %d after the two controls, want %d", after, before+2)
	}
	if n := s.rows(t); n != 2 || s.keyRows(t, long64, "r") != 1 || s.keyRows(t, "u", long64) != 1 {
		t.Errorf("%d rows after the controls, want one row for each", n)
	}
}

// TestT_HTTP_2 (HARNESS §8): users a and b both send request ID r1 → two rows, both new
// (existed false): the database key is (user_id, request_id) (HTTP-4).
func TestT_HTTP_2(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	for _, user := range []string{"a", "b"} {
		wantCreated(t, "user "+user, api.Hit(t, user, "r1"), user, "r1")
	}
	if n := s.rows(t); n != 2 || s.keyRows(t, "a", "r1") != 1 || s.keyRows(t, "b", "r1") != 1 {
		t.Errorf("%d rows, want 2: one for (a, r1) and one for (b, r1)", n)
	}
}

// TestT_HTTP_3 (HARNESS §8): 10 concurrent identical requests → 1 row; 10 × 200, exactly one
// with existed false; every arrival_ns is the row's (HTTP-10); each attempt was charged: 10
// distinct deduction_ids in the decision records, and the bucket holds 10 of its 20 tokens
// (u = 10,000,000,000 units on the frozen clock, rladmin inspect; HTTP-12, BUD-9).
func TestT_HTTP_3(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	const n = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	answers := make([]Answer, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := api.try(t.Context(), http.MethodGet, "/hit", IDs("dup", "op-1"), "")
			answers[i] = Answer{r, err}
		}()
	}
	close(start)
	wg.Wait()

	rowNs, found := s.rowArrival(t, "dup", "op-1")
	if !found {
		t.Fatal("no row holds (dup, op-1), want the row")
	}
	created := 0
	for i, a := range answers {
		if a.Err != nil {
			t.Fatalf("request %d: %v", i+1, a.Err)
		}
		if a.Status != http.StatusOK || a.JSON == nil {
			t.Errorf("request %d: %d %s, want 200", i+1, a.Status, a.Body)
			continue
		}
		if a.JSON["existed"] == false {
			created++
		}
		if a.JSON["arrival_ns"] != strconv.FormatInt(rowNs, 10) {
			t.Errorf("request %d: arrival_ns %v, want the row's %d", i+1, a.JSON["arrival_ns"], rowNs)
		}
	}
	if created != 1 {
		t.Errorf("%d responses with existed false, want exactly 1", created)
	}
	if rows := s.rows(t); rows != 1 {
		t.Errorf("%d rows, want 1", rows)
	}
	decisions := api.WaitRecords(t, "decision", func(rs []map[string]any) bool { return len(rs) >= n })
	ids := map[string]bool{}
	for _, d := range decisions {
		id, _ := d["deduction_id"].(string)
		if d["user_id"] != "dup" || d["result"] != "allowed" || id == "" {
			t.Errorf("decision record %v, want an allowed decision for dup with a deduction_id", d)
		}
		ids[id] = true
	}
	if len(decisions) != n || len(ids) != n {
		t.Errorf("%d decision records with %d distinct deduction_ids, want %d and %d", len(decisions), len(ids), n, n)
	}
	if u := s.Balance(t, "dup"); u != 10_000_000_000 {
		t.Errorf("rladmin inspect --key dup: u = %d, want 10,000,000,000 (10 tokens left of 20)", u)
	}
}

// TestT_HTTP_4 (HARNESS §8): one user sends 25 new operations at capacity 20 on the frozen
// clock, plus 5 invalid requests → 20 × 200 with one row each, 5 × 429 and 5 × 400 that write
// nothing; 20 rows in all (ERR-1, ERR-4, REF-1, HOST-1).
func TestT_HTTP_4(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	invalid := []http.Header{
		headers(one("u4"), one("bad id")),
		headers(one("u4"), nil),
		headers(one("u4"), one("")),
		headers([]string{"u4", "u4"}, one("op-x")),
		headers(one("u4"), one(strings.Repeat("r", 65))),
	}
	var ok, limited, bad int
	for i := 1; i <= 25; i++ {
		req := fmt.Sprintf("op-%02d", i)
		r := api.Hit(t, "u4", req)
		switch {
		case i <= 20:
			wantCreated(t, "operation "+req, r, "u4", req)
			if r.Status == http.StatusOK {
				ok++
			}
		default:
			wantError(t, "operation "+req, r, http.StatusTooManyRequests, "rate_limited")
			if r.Status == http.StatusTooManyRequests {
				limited++
			}
		}
		if i%5 == 0 {
			r := api.Do(t, http.MethodGet, "/hit", invalid[i/5-1], "")
			wantError(t, fmt.Sprintf("invalid request %d", i/5), r, http.StatusBadRequest, "invalid_request")
			if r.Status == http.StatusBadRequest {
				bad++
			}
		}
	}
	if ok != 20 || limited != 5 || bad != 5 {
		t.Errorf("%d × 200, %d × 429, %d × 400; want 20, 5, 5", ok, limited, bad)
	}
	if n := s.rows(t); n != 20 {
		t.Errorf("%d rows, want 20", n)
	}
	for i := 1; i <= 25; i++ {
		want := int64(1)
		if i > 20 {
			want = 0
		}
		if n := s.keyRows(t, "u4", fmt.Sprintf("op-%02d", i)); n != want {
			t.Errorf("rows for (u4, op-%02d) = %d, want %d", i, n, want)
		}
	}
}

// TestT_HTTP_5 (HARNESS §8): with every refund held before its send (POST /_test/barrier
// {at: before_send, match: refund}), (a) a database fault after commit → 500 internal_error,
// the row present (ERR-13); (b) a database fault before the insert → 503
// database_unavailable, no row (ERR-14) (FLT-5). Each response comes while its refund is still
// held, so there is no refund record yet (HTTP-14); after the release each gets a refund
// record applied within 5 s whose credit_decision_ns is its decision_ns (frozen clock, REF-2),
// and the bucket is back to its 20 tokens. The rows are exactly the requests logged 200 with
// existed false, plus the ERR-13 one.
func TestT_HTTP_5(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	api.Control(t, "/_test/barrier", `{"name":"refunds","at":"before_send","match":"refund"}`)
	for _, req := range []string{"ok-1", "ok-2"} {
		wantCreated(t, "healthy operation "+req, api.Hit(t, "h5", req), "h5", req)
	}

	api.Control(t, "/_test/faults", `{"target":"db","mode":"fail_after_commit","count":1}`)
	wantError(t, "fail_after_commit", api.Hit(t, "u5", "a"), http.StatusInternalServerError, "internal_error")
	api.Control(t, "/_test/faults", `{"target":"db","mode":"fail_before","count":1}`)
	wantError(t, "fail_before", api.Hit(t, "u5", "b"), http.StatusServiceUnavailable, "database_unavailable")
	if s.keyRows(t, "u5", "a") != 1 || s.keyRows(t, "u5", "b") != 0 {
		t.Errorf("rows for (u5, a) and (u5, b) = %d and %d, want 1 (the row remains) and 0", s.keyRows(t, "u5", "a"), s.keyRows(t, "u5", "b"))
	}

	// Each failed request's decision and release records (both written before its response).
	decisionNs := map[string]int64{}
	for _, d := range api.WaitRecords(t, "decision", func(rs []map[string]any) bool { return len(rs) >= 4 }) {
		if d["user_id"] == "u5" {
			decisionNs[d["deduction_id"].(string)] = Int(t, d, "decision_ns")
		}
	}
	if len(decisionNs) != 2 {
		t.Fatalf("decision records for u5: %v, want 2 with distinct deduction_ids", decisionNs)
	}
	api.WaitRecords(t, "release", func(rs []map[string]any) bool {
		n := 0
		for _, r := range rs {
			if _, ok := decisionNs[r["deduction_id"].(string)]; ok {
				n++
			}
		}
		return n == 2
	})
	if refunds := api.Records(t, "refund"); len(refunds) != 0 {
		t.Errorf("refund records %v while the refunds are held, want none: the responses come before the refunds", refunds)
	}
	if n := api.metricSum(t, "rl_pending_refunds"); n != 2 {
		t.Errorf("rl_pending_refunds = %d while the refunds are held, want 2", n)
	}

	api.Control(t, "/_test/barrier/release", `{"name":"refunds"}`)
	refunds := api.WaitRecords(t, "refund", func(rs []map[string]any) bool { return len(rs) >= 2 })
	if len(refunds) != 2 {
		t.Errorf("refund records %v, want 2", refunds)
	}
	// The applied records' deduction_ids are exactly the two failed decisions', one record each.
	applied := map[string]int{}
	for _, r := range refunds {
		id, _ := r["deduction_id"].(string)
		want, ok := decisionNs[id]
		if !ok || r["result"] != "applied" || Int(t, r, "credit_decision_ns") != want {
			t.Errorf("refund record %v, want applied with credit_decision_ns = its decision_ns %d", r, want)
			continue
		}
		applied[id]++
	}
	for id := range decisionNs {
		if applied[id] != 1 {
			t.Errorf("deduction %s has %d applied refund records, want exactly 1 (refund records %v)", id, applied[id], refunds)
		}
	}
	if u := s.Balance(t, "u5"); u != 20_000_000_000 {
		t.Errorf("rladmin inspect --key u5: u = %d, want 20,000,000,000 (back to 20 tokens)", u)
	}

	// The rows are the requests logged 200 with existed false, plus the ERR-13 one.
	want := map[[2]string]bool{}
	for _, r := range api.WaitRecords(t, "request", func(rs []map[string]any) bool { return len(rs) >= 4 }) {
		created := Int(t, r, "http_status") == 200 && r["existed"] == false
		if created || r["error"] == "internal_error" {
			want[[2]string{r["user_id"].(string), r["request_id"].(string)}] = true
		}
	}
	if n := s.rows(t); n != int64(len(want)) || len(want) != 3 {
		t.Errorf("%d rows for the %d logged keys %v, want 3 rows: (h5, ok-1), (h5, ok-2), (u5, a)", n, len(want), want)
	}
	for k := range want {
		if n := s.keyRows(t, k[0], k[1]); n != 1 {
			t.Errorf("rows for %v = %d, want 1", k, n)
		}
	}
}

// TestT_HTTP_6 (HARNESS §8): real 429s from bin/test/hitapi with capacity 1, rate_tokens 1000
// and a frozen controlled clock, so the second /hit waits retry_after_ns =
// ceil(rate_period_ns / 1000): 1 ns, 999,999 ns, 1 ms and 1 s + 1 ns for the four stacks.
// Retry-After = ceil(ns / 10^9) s and retry_after_ms = ceil(ns / 10^6), never 0 (HTTP-9).
func TestT_HTTP_6(t *testing.T) {
	for _, c := range []struct {
		periodNs              int64
		retryAfterS, retryMs  int64
		retryAfterNs, comment string
	}{
		{1_000, 1, 1, "1", "1 ns"},
		{999_999_000, 1, 1, "999999", "999,999 ns"},
		{1_000_000_000, 1, 1, "1000000", "1 ms"},
		{1_000_000_001_000, 2, 1001, "1000000001", "1 s + 1 ns"},
	} {
		t.Run(c.comment, func(t *testing.T) {
			s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) {
				f.Policy.Capacity = 1
				f.Policy.RateTokens = 1000
				f.Policy.RatePeriodNs = c.periodNs
			}})
			api := s.APIs[0]
			wantCreated(t, "the first /hit", api.Hit(t, "u6", "r1"), "u6", "r1")
			r := api.Hit(t, "u6", "r2")
			wantError(t, "the second /hit", r, http.StatusTooManyRequests, "rate_limited")
			if got := r.Header.Get("Retry-After"); got != strconv.FormatInt(c.retryAfterS, 10) {
				t.Errorf("retry_after_ns %s: Retry-After %q, want %d", c.retryAfterNs, got, c.retryAfterS)
			}
			if r.JSON != nil && Int(t, r.JSON, "retry_after_ms") != c.retryMs {
				t.Errorf("retry_after_ns %s: retry_after_ms %v, want %d", c.retryAfterNs, r.JSON["retry_after_ms"], c.retryMs)
			}
		})
	}
}

// TestT_HTTP_7 (HARNESS §8): the 200, 429, 400, 404, 405 and 503 bodies have exactly the
// HTTP-6/HTTP-7 fields with their JSON types (arrival_ns and decision_ns strings of digits)
// and the stable ERR codes; every response, /health and /metrics included, carries
// Cache-Control: no-store (HTTP-8); arrival_ns lies between the client's wall time before and
// after the call (HTTP-5). Capacity 1 on the frozen clock gives the 429; a store refusal
// (FLT-1 b) the 503.
func TestT_HTTP_7(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.Policy.Capacity = 1 }})
	api := s.APIs[0]
	check := func(what string, r Response, status int, fields []string) {
		t.Helper()
		if r.Status != status || r.JSON == nil {
			t.Errorf("%s: %d %s, want %d with a JSON object", what, r.Status, r.Body, status)
			return
		}
		if got := keys(r.JSON); !slices.Equal(got, fields) {
			t.Errorf("%s: fields %v, want exactly %v", what, got, fields)
		}
		if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", what, cc)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type %q, want application/json", what, ct)
		}
		for k, v := range r.JSON {
			switch k {
			case "arrival_ns", "decision_ns":
				if s, ok := v.(string); !ok || !digits.MatchString(s) {
					t.Errorf("%s: %s = %v (%T), want a string of digits", what, k, v, v)
				}
			case "existed", "degraded":
				if _, ok := v.(bool); !ok {
					t.Errorf("%s: %s = %v (%T), want a bool", what, k, v, v)
				}
			case "retry_after_ms":
				if _, ok := v.(json.Number); !ok {
					t.Errorf("%s: %s = %v (%T), want a number", what, k, v, v)
				}
			default:
				if _, ok := v.(string); !ok {
					t.Errorf("%s: %s = %v (%T), want a string", what, k, v, v)
				}
			}
		}
	}

	ok := api.Hit(t, "u7", "r1")
	check("200", ok, 200, []string{"arrival_ns", "decision_ns", "degraded", "existed", "request_id", "user_id"})
	if ok.JSON["user_id"] != "u7" || ok.JSON["request_id"] != "r1" || ok.JSON["existed"] != false || ok.JSON["degraded"] != false {
		t.Errorf("200 body %s, want user u7, request r1, existed false, degraded false", ok.Body)
	}
	if arrival, err := strconv.ParseInt(fmt.Sprint(ok.JSON["arrival_ns"]), 10, 64); err != nil ||
		arrival < ok.Before.UnixNano() || arrival > ok.After.UnixNano() {
		t.Errorf("200 arrival_ns %v, want between the client's %d and %d", ok.JSON["arrival_ns"], ok.Before.UnixNano(), ok.After.UnixNano())
	}
	if ok.JSON["decision_ns"] != strconv.FormatInt(Frozen, 10) {
		t.Errorf("200 decision_ns %v, want the frozen controlled time %d", ok.JSON["decision_ns"], Frozen)
	}

	limited := api.Hit(t, "u7", "r2")
	check("429", limited, 429, []string{"decision_ns", "error", "message", "request_id", "retry_after_ms", "user_id"})
	wantError(t, "429", limited, 429, "rate_limited")
	// Capacity 1, 10 tokens per second: the next token comes 100 ms later.
	if limited.JSON != nil && (Int(t, limited.JSON, "retry_after_ms") != 100 || limited.Header.Get("Retry-After") != "1") {
		t.Errorf("429: retry_after_ms %v, Retry-After %q; want 100 and 1", limited.JSON["retry_after_ms"], limited.Header.Get("Retry-After"))
	}

	check("400 without headers", api.Do(t, http.MethodGet, "/hit", nil, ""), 400, []string{"error", "message"})
	bad := api.Do(t, http.MethodGet, "/hit", headers(one("u7"), one("r 1")), "")
	check("400 with one valid ID", bad, 400, []string{"error", "message", "user_id"})
	wantError(t, "400", bad, 400, "invalid_request")
	nf := api.Do(t, http.MethodGet, "/nope", nil, "")
	check("404", nf, 404, []string{"error", "message"})
	wantError(t, "404", nf, 404, "not_found")
	na := api.Do(t, http.MethodPost, "/hit", nil, "")
	check("405", na, 405, []string{"error", "message"})
	wantError(t, "405", na, 405, "method_not_allowed")

	api.Control(t, "/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":1}`)
	un := api.Hit(t, "u7b", "r1")
	check("503", un, 503, []string{"error", "message", "request_id", "user_id"})
	wantError(t, "503", un, 503, "limiter_unavailable")

	for _, path := range []string{"/health", "/metrics"} {
		r := api.Do(t, http.MethodGet, path, nil, "")
		if r.Status != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d, Cache-Control %q; want 200 and no-store", path, r.Status, r.Header.Get("Cache-Control"))
		}
	}
}

// TestT_HTTP_8 (HARNESS §8), store clock: 500 /hit from 32 goroutines while /health and
// /metrics are called 100 times each → the acquire total equals the valid /hit count, the
// rows equal the 200s with existed false (health and metrics spend no allowance and write no
// rows, HAR-16), and /health shows the library's readiness and clock source throughout.
func TestT_HTTP_8(t *testing.T) {
	s := StartStack(t, StackOpts{})
	api := s.APIs[0]
	const hits, workers, probes = 500, 32, 100
	before := api.AcquireTotal(t)

	jobs := make(chan int, hits)
	for i := range hits {
		jobs <- i
	}
	close(jobs)
	type outcome struct {
		status  int
		created bool
		err     error
	}
	outcomes := make(chan outcome, hits)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				r, err := api.try(t.Context(), http.MethodGet, "/hit", IDs(fmt.Sprintf("u8-%d", i%25), fmt.Sprintf("r-%d", i)), "")
				outcomes <- outcome{r.Status, r.Status == http.StatusOK && r.JSON["existed"] == false, err}
			}
		}()
	}
	type probe struct {
		path string
		r    Response
		err  error
	}
	probed := make(chan probe, 2*probes)
	for _, path := range []string{"/health", "/metrics"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range probes {
				r, err := api.try(t.Context(), http.MethodGet, path, nil, "")
				probed <- probe{path, r, err}
			}
		}()
	}
	wg.Wait()
	close(outcomes)
	close(probed)

	var valid, created int64
	statuses := map[int]int{}
	for o := range outcomes {
		if o.err != nil {
			t.Fatalf("a /hit failed: %v", o.err)
		}
		valid++
		statuses[o.status]++
		if o.created {
			created++
		}
	}
	if valid != hits || statuses[200]+statuses[429] != hits {
		t.Errorf("%d /hit answers by status %v, want %d, each 200 or 429", valid, statuses, hits)
	}
	for p := range probed {
		switch {
		case p.err != nil:
			t.Fatalf("%s failed: %v", p.path, p.err)
		case p.r.Status != http.StatusOK:
			t.Errorf("%s: %d %s, want 200", p.path, p.r.Status, p.r.Body)
		case p.path == "/health" && (p.r.JSON["status"] != "ready" || p.r.JSON["clock_source"] != "store"):
			t.Errorf("/health under load: status %v, clock_source %v; want ready and store", p.r.JSON["status"], p.r.JSON["clock_source"])
		}
	}
	if got := api.AcquireTotal(t) - before; got != valid {
		t.Errorf("acquire total grew by %d, want the %d valid /hit requests", got, valid)
	}
	if n := s.rows(t); n != created {
		t.Errorf("%d rows, want the %d 200s with existed false", n, created)
	}
	if h := api.Health(t); h["status"] != "ready" || h["clock_source"] != "store" {
		t.Errorf("/health after the load: status %v, clock_source %v; want ready and store", h["status"], h["clock_source"])
	}
}
