//go:build rltest

package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/dbclient"
	"ratelimiter/harness/httpapi"
	"ratelimiter/internal/testutil/storeproc"
	"ratelimiter/limiter"
)

// forced is the request ID of the /hit each T-HTTP-9 case forces to its ERR row.
const forced = "forced"

// tBoundNs is the controlled time that gives clock_error (ERR-12b; IMPLEMENTATION_PLAN H4
// Tests): TBOUND µs × 1,000, TBOUND = 2^53 − 2^43 µs being the smallest time the scripts'
// read_clock refuses (DESIGN §A4.2).
const tBoundNs = int64(8_998_403_161_718_784_000)

// errRow is one HARNESS §3 ERR row as T-HTTP-9 checks a forced /hit against it: the status, the
// error code ("" for a 200), and the limiter result the request record names (the SPEC name;
// HAR-18).
type errRow struct {
	id     string
	status int
	code   string
	result string
}

// wantRow checks the forced /hit r and its request record against row: the response's status
// and error code (a 200 has none), the record's http_status, error and result, and that 429
// comes with ERR-4 alone (only limited gives 429).
func wantRow(t *testing.T, a *API, user string, row errRow, r Response) {
	t.Helper()
	if row.status == http.StatusOK {
		if r.Status != http.StatusOK || r.JSON == nil || r.JSON["error"] != nil {
			t.Errorf("%s: %d %s, want 200 without an error", row.id, r.Status, r.Body)
		}
	} else {
		wantError(t, row.id, r, row.status, row.code)
	}
	if (r.Status == http.StatusTooManyRequests) != (row.id == "ERR-4") {
		t.Errorf("%s: status %d: only ERR-4 (limited) gives 429", row.id, r.Status)
	}
	rec := requestOf(t, a, user, forced)
	if Int(t, rec, "http_status") != int64(row.status) || rec["error"] != row.code || rec["result"] != row.result {
		t.Errorf("%s: the request record %v, want http_status %d, error %q, result %s", row.id, rec, row.status, row.code, row.result)
	}
}

// requestOf waits for the instance's request record of (user, request) and returns it.
func requestOf(t *testing.T, a *API, user, request string) map[string]any {
	t.Helper()
	var rec map[string]any
	a.WaitRecords(t, "request", func(rs []map[string]any) bool {
		for _, r := range rs {
			if r["user_id"] == user && r["request_id"] == request {
				rec = r
				return true
			}
		}
		return false
	})
	return rec
}

// recordsOf returns the instance's records of kind for deduction id.
func recordsOf(t *testing.T, a *API, kind, id string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range a.Records(t, kind) {
		if r["deduction_id"] == id {
			out = append(out, r)
		}
	}
	return out
}

// inspectKey returns rladmin inspect --key key --json's fields (ADM-4): present is 0 when the
// key has no bucket, and u, its balance in units, is then null.
func inspectKey(t *testing.T, s *Stack, key string) map[string]any {
	t.Helper()
	code, out := s.RLAdmin(t, "inspect", "--key", key, "--json")
	if code != 0 {
		t.Fatalf("rladmin inspect --key %s exited %d: %s", key, code, out)
	}
	m, err := decodeObject([]byte(out))
	if err != nil {
		t.Fatalf("rladmin inspect --key %s --json printed %q: %v", key, out, err)
	}
	return m
}

// wantTokens checks that key's bucket holds want tokens (rladmin inspect u = want × token).
func wantTokens(t *testing.T, s *Stack, key string, want int64, why string) {
	t.Helper()
	if u := s.Balance(t, key); u != want*token {
		t.Errorf("rladmin inspect --key %s: u = %d, want %d (%d tokens: %s)", key, u, want*token, want, why)
	}
}

// wantNoBucket checks that key has no bucket: nothing was ever charged to it.
func wantNoBucket(t *testing.T, s *Stack, key string) {
	t.Helper()
	if m := inspectKey(t, s, key); Int(t, m, "present") != 0 || m["u"] != nil {
		t.Errorf("rladmin inspect --key %s: %v, want no bucket (present 0, u null): nothing charged", key, m)
	}
}

// wantRows checks how many rows (user, request) holds.
func wantRows(t *testing.T, s *Stack, user, request string, want int64) {
	t.Helper()
	if n := s.keyRows(t, user, request); n != want {
		t.Errorf("rows for (%s, %s) = %d, want %d", user, request, n, want)
	}
}

// waitHealth polls the instance's /health until it shows status, at most stageWait.
func waitHealth(t *testing.T, a *API, status string) {
	t.Helper()
	deadline := time.Now().Add(stageWait)
	for h := a.Health(t); h["status"] != status; h = a.Health(t) {
		if time.Now().After(deadline) {
			t.Fatalf("hitapi %d's /health %v after %v, want status %s", a.N, h, stageWait, status)
		}
		time.Sleep(pollEvery)
	}
}

// TestT_HTTP_9 (HARNESS §8 T-HTTP-9: ERR-3, ERR-7, ERR-8, ERR-9, ERR-9a, ERR-10, ERR-11, ERR-12,
// ERR-12a, ERR-12b, ERR-12c, ERR-12d, ERR-15, HAR-18, HOST-4; IMPLEMENTATION_PLAN H4 Tests):
// every limiter result and post-acquire outcome, forced through bin/test hitapi's /hit (each
// case but ERR-12d on a stack of its own, on a frozen controlled clock), maps to exactly its ERR
// row: the status and error code, the request record's result, the tokens it leaves charged
// (rladmin inspect's u; a control /hit allowed first leaves 19 of the 20) and the rows its key
// holds (dbclient.CountKey); 429 comes with ERR-4 alone. ERR-12d, which /hit cannot produce (the
// header checks pass only IDs the limiter accepts), is a table test of harness/httpapi's
// result-to-ERR mapping over every result (plan choice).
func TestT_HTTP_9(t *testing.T) {
	// ERR-3: open mode, the store wrapper refuses the acquire before sending (unavailable): the
	// request goes on degraded, its database work succeeds: 200 degraded, nothing charged, its
	// row created.
	t.Run("ERR-3", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.Policy.FailureMode = limiter.Open }})
		api := s.APIs[0]
		wantCreated(t, "the control /hit", api.Hit(t, "e3", "before"), "e3", "before")
		api.Control(t, "/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":1}`)
		r := api.Hit(t, "e3", forced)
		wantRow(t, api, "e3", errRow{"ERR-3", http.StatusOK, "", "degraded"}, r)
		if r.JSON["degraded"] != true || r.JSON["existed"] != false {
			t.Errorf("ERR-3: %s, want degraded true and existed false: the row created", r.Body)
		}
		wantTokens(t, s, "e3", 19, "none charged, the control's one only")
		wantRows(t, s, "e3", forced, 1)
	})

	// ERR-4: on a frozen clock nothing refills; the user's 20 tokens go to 20 /hit requests, and
	// the 21st is limited: 429 rate_limited with its Retry-After, nothing charged, no row.
	t.Run("ERR-4", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		for i := 1; i <= 20; i++ {
			request := "r" + strconv.Itoa(i)
			wantCreated(t, "/hit "+request, api.Hit(t, "e4", request), "e4", request)
		}
		r := api.Hit(t, "e4", forced)
		wantRow(t, api, "e4", errRow{"ERR-4", http.StatusTooManyRequests, "rate_limited", "limited"}, r)
		if r.Header.Get("Retry-After") == "" {
			t.Errorf("ERR-4: no Retry-After header, want the retry hint (HTTP-9)")
		}
		wantTokens(t, s, "e4", 0, "the 20 allowed requests took them all; the limited one none")
		wantRows(t, s, "e4", forced, 0)
	})

	// ERR-7: with max_held_receipts = max_pending_refunds = 1, a first /hit whose insert waits
	// behind a held key keeps the one receipt slot; the forced /hit finds no slot: busy, 503
	// service_overloaded, nothing charged (the held request's one token only), no row. The held
	// request finishes 200 once the key is rolled back.
	t.Run("ERR-7", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) {
			f.Instance.MaxHeldReceipts, f.Instance.MaxPendingRefunds = 1, 1
		}})
		api := s.APIs[0]
		rollback := s.holdKey(t, "e7", "held")
		held := api.Go(t, http.MethodGet, "/hit", IDs("e7", "held"), "")
		s.waitLockWaiters(t, 1)
		wantTokens(t, s, "e7", 19, "the held request's acquire charged one")
		r := api.Hit(t, "e7", forced)
		wantRow(t, api, "e7", errRow{"ERR-7", http.StatusServiceUnavailable, "service_overloaded", "busy"}, r)
		wantTokens(t, s, "e7", 19, "the busy request charged none")
		wantRows(t, s, "e7", forced, 0)
		if err := rollback(); err != nil {
			t.Fatalf("rolling back the held key: %v", err)
		}
		wantCreated(t, "the held /hit", Wait(t, held, stageWait, "the held /hit"), "e7", "held")
	})

	// ERR-8: closed mode, the store wrapper refuses the acquire before sending: unavailable,
	// 503 limiter_unavailable, nothing charged, no row.
	t.Run("ERR-8", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		wantCreated(t, "the control /hit", api.Hit(t, "e8", "before"), "e8", "before")
		api.Control(t, "/_test/faults", `{"target":"store","mode":"refuse","match":"acquire","count":1}`)
		wantRow(t, api, "e8", errRow{"ERR-8", http.StatusServiceUnavailable, "limiter_unavailable", "unavailable"}, api.Hit(t, "e8", forced))
		wantTokens(t, s, "e8", 19, "none charged")
		wantRows(t, s, "e8", forced, 0)
	})

	// ERR-9: the store wrapper drops the acquire's reply after the store deducted: 503
	// limiter_outcome_unknown, 1 token charged, no row.
	t.Run("ERR-9", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		wantCreated(t, "the control /hit", api.Hit(t, "e9", "before"), "e9", "before")
		api.Control(t, "/_test/faults", `{"target":"store","mode":"drop_reply","match":"acquire","count":1}`)
		wantRow(t, api, "e9", errRow{"ERR-9", http.StatusServiceUnavailable, "limiter_outcome_unknown", "outcome_unknown"}, api.Hit(t, "e9", forced))
		wantTokens(t, s, "e9", 18, "the dropped acquire charged one")
		wantRows(t, s, "e9", forced, 0)
	})

	// ERR-9a: P = 1 s, shorter than D = 2 s, and a 1.5 s delay before the acquire is sent: P
	// ends first, the wrapper returns without sending: cancelled, 503 limiter_deadline_exceeded
	// after P (limiter_ns from 1 s to under 1.5 s), nothing charged, no row.
	t.Run("ERR-9a", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.Instance.MaxReleaseDelayNs = int64(time.Second) }})
		api := s.APIs[0]
		wantCreated(t, "the control /hit", api.Hit(t, "e9a", "before"), "e9a", "before")
		api.Control(t, "/_test/faults", `{"target":"store","mode":"delay","match":"acquire","count":1,"delay_ms":1500,"at":"before_send"}`)
		wantRow(t, api, "e9a", errRow{"ERR-9a", http.StatusServiceUnavailable, "limiter_deadline_exceeded", "cancelled"}, api.Hit(t, "e9a", forced))
		if ns := Int(t, requestOf(t, api, "e9a", forced), "limiter_ns"); ns < int64(time.Second) || ns >= int64(1500*time.Millisecond) {
			t.Errorf("ERR-9a: limiter_ns %d, want from P = 1 s to under the 1.5 s delay", ns)
		}
		wantTokens(t, s, "e9a", 19, "none charged")
		wantRows(t, s, "e9a", forced, 0)
	})

	// ERR-10: max_tracked_buckets = 1 holds the first user's bucket; a second user's /hit is
	// capacity_error: 503 limiter_capacity_exceeded, no bucket for it (nothing charged), the
	// first user's bucket kept, no row.
	t.Run("ERR-10", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.Policy.MaxTrackedBuckets = 1 }})
		api := s.APIs[0]
		wantCreated(t, "the first user's /hit", api.Hit(t, "e10-a", "before"), "e10-a", "before")
		wantRow(t, api, "e10-b", errRow{"ERR-10", http.StatusServiceUnavailable, "limiter_capacity_exceeded", "capacity_error"}, api.Hit(t, "e10-b", forced))
		wantNoBucket(t, s, "e10-b")
		wantTokens(t, s, "e10-a", 19, "still tracked, nothing evicted")
		wantRows(t, s, "e10-b", forced, 0)
	})

	// ERR-11: the tamper tool deletes the key's ix member: state_integrity_error, 503
	// limiter_state_integrity_error, nothing charged, no row.
	t.Run("ERR-11", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		wantCreated(t, "the control /hit", api.Hit(t, "e11", "before"), "e11", "before")
		tamper(t, s, api, "del-ix", "--key", "e11")
		wantRow(t, api, "e11", errRow{"ERR-11", http.StatusServiceUnavailable, "limiter_state_integrity_error", "state_integrity_error"}, api.Hit(t, "e11", forced))
		wantTokens(t, s, "e11", 19, "none charged")
		wantRows(t, s, "e11", forced, 0)
	})

	// ERR-12: the signer wrapper reports the signer unusable: config_error before anything is
	// sent, 503 limiter_config_error, nothing charged, no row.
	t.Run("ERR-12", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		wantCreated(t, "the control /hit", api.Hit(t, "e12", "before"), "e12", "before")
		api.Control(t, "/_test/faults", `{"target":"signer","mode":"unusable","count":1}`)
		wantRow(t, api, "e12", errRow{"ERR-12", http.StatusServiceUnavailable, "limiter_config_error", "config_error"}, api.Hit(t, "e12", forced))
		wantTokens(t, s, "e12", 19, "none charged")
		wantRows(t, s, "e12", forced, 0)
	})

	// ERR-12a: an instance on a store with no epoch is not ready: not_ready, 503
	// limiter_not_ready, no acquire sent (the send observer; its positive control is the
	// /hit allowed once the epoch is started and joined), no row; after that /hit the bucket
	// holds 19 tokens, the control's one charge.
	t.Run("ERR-12a", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen, NoEpoch: true})
		api := s.APIs[0]
		since := lastSend(t, api)
		wantRow(t, api, "e12a", errRow{"ERR-12a", http.StatusServiceUnavailable, "limiter_not_ready", "not_ready"}, api.Hit(t, "e12a", forced))
		if n := acquiresSent(sendsSince(t, api, since)); n != 0 {
			t.Errorf("ERR-12a: %d acquires sent for the not-ready /hit, want 0", n)
		}
		s.startEpoch()
		waitHealth(t, api, "ready")
		controlHit(t, api, "e12a", "after")
		wantTokens(t, s, "e12a", 19, "the control's one charge only")
		wantRows(t, s, "e12a", forced, 0)
	})

	// ERR-12b: the controlled clock set to TBOUND (clock set 8998403161718784000): clock_error,
	// 503 limiter_clock_error, nothing charged, no row.
	t.Run("ERR-12b", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		wantCreated(t, "the control /hit", api.Hit(t, "e12b", "before"), "e12b", "before")
		if err := s.Clock.Set(tBoundNs); err != nil {
			t.Fatalf("clock set %d: %v", tBoundNs, err)
		}
		wantRow(t, api, "e12b", errRow{"ERR-12b", http.StatusServiceUnavailable, "limiter_clock_error", "clock_error"}, api.Hit(t, "e12b", forced))
		wantTokens(t, s, "e12b", 19, "none charged")
		wantRows(t, s, "e12b", forced, 0)
	})

	// ERR-12c: the signer wrapper fails after the confirmed deduction: receipt_error, 503
	// limiter_receipt_error, 1 token charged, no row.
	t.Run("ERR-12c", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen})
		api := s.APIs[0]
		wantCreated(t, "the control /hit", api.Hit(t, "e12c", "before"), "e12c", "before")
		api.Control(t, "/_test/faults", `{"target":"signer","mode":"fail","count":1}`)
		wantRow(t, api, "e12c", errRow{"ERR-12c", http.StatusServiceUnavailable, "limiter_receipt_error", "receipt_error"}, api.Hit(t, "e12c", forced))
		wantTokens(t, s, "e12c", 18, "the deduction stays charged")
		wantRows(t, s, "e12c", forced, 0)
	})

	// ERR-12d: harness/httpapi's handler, its limiter answering each result in turn: every
	// result maps to its ERR row, validation_error to 400 invalid_request with no database
	// work (no row) and no permit (nothing charged); only limited gives 429, and only allowed
	// and degraded reach the database.
	t.Run("ERR-12d", func(t *testing.T) { checkResultMapping(t) })

	// ERR-15: refuse_after_acquire_bp = 10000 refuses every request after its acquire: its
	// permit released, 503 service_aborted_after_acquire, no row; the release record, then the
	// refund applied, so the bucket is back to its 20 tokens.
	t.Run("ERR-15", func(t *testing.T) {
		s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.API.RefuseAfterAcquireBP = config.MaxRefuseBP }})
		api := s.APIs[0]
		wantRow(t, api, "e15", errRow{"ERR-15", http.StatusServiceUnavailable, "service_aborted_after_acquire", "allowed"}, api.Hit(t, "e15", forced))
		id := deductionOf(t, api, "e15")
		if n := len(recordsOf(t, api, "release", id)); n != 1 {
			t.Errorf("ERR-15: %d release records for %s, want 1", n, id)
		}
		if rec := refundOf(t, api, id); rec["result"] != "applied" {
			t.Errorf("ERR-15: the refund record %v, want applied", rec)
		}
		wantTokens(t, s, "e15", 20, "charged, then refunded")
		wantRows(t, s, "e15", forced, 0)
	})
}

// checkResultMapping is ERR-12d (plan choice): harness/httpapi's /hit handler, built by
// httpapi.New with harness.toml's [api] and [instance] around a limiter that answers one result
// kind, a database that counts its calls and a run log that keeps nothing, answers each kind
// with its HARNESS §3 ERR row.
func checkResultMapping(t *testing.T) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(filepath.Join(root, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind   limiter.ResultKind
		status int
		code   string // "" for a 200
	}{
		{limiter.Allowed, http.StatusOK, ""},                                                          // ERR-1
		{limiter.Degraded, http.StatusOK, ""},                                                         // ERR-3
		{limiter.Limited, http.StatusTooManyRequests, "rate_limited"},                                 // ERR-4
		{limiter.Busy, http.StatusServiceUnavailable, "service_overloaded"},                           // ERR-7
		{limiter.Unavailable, http.StatusServiceUnavailable, "limiter_unavailable"},                   // ERR-8
		{limiter.OutcomeUnknown, http.StatusServiceUnavailable, "limiter_outcome_unknown"},            // ERR-9
		{limiter.Cancelled, http.StatusServiceUnavailable, "limiter_deadline_exceeded"},               // ERR-9a
		{limiter.CapacityError, http.StatusServiceUnavailable, "limiter_capacity_exceeded"},           // ERR-10
		{limiter.StateIntegrityError, http.StatusServiceUnavailable, "limiter_state_integrity_error"}, // ERR-11
		{limiter.ConfigError, http.StatusServiceUnavailable, "limiter_config_error"},                  // ERR-12
		{limiter.NotReady, http.StatusServiceUnavailable, "limiter_not_ready"},                        // ERR-12a
		{limiter.ClockError, http.StatusServiceUnavailable, "limiter_clock_error"},                    // ERR-12b
		{limiter.ReceiptError, http.StatusServiceUnavailable, "limiter_receipt_error"},                // ERR-12c
		{limiter.ValidationError, http.StatusBadRequest, "invalid_request"},                           // ERR-12d
	} {
		db := &mappingDB{}
		srv, err := httpapi.New(httpapi.Config{API: f.API, Instance: f.Instance, RunID: "e2e-err-12d", InstanceNum: 1,
			ProcessStart: time.Now(), NofileLimit: 8192}, mappingLimiter{kind: c.kind}, db, mappingLog{})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hit", nil)
		req.Header = IDs("e12d", forced)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		m, err := decodeObject(w.Body.Bytes())
		if err != nil {
			t.Fatalf("%v: the body %q: %v", c.kind, w.Body.Bytes(), err)
		}
		code, _ := m["error"].(string)
		if w.Code != c.status || code != c.code {
			t.Errorf("%v: %d with error %q, want %d with %q", c.kind, w.Code, code, c.status, c.code)
		}
		if (w.Code == http.StatusTooManyRequests) != (c.kind == limiter.Limited) {
			t.Errorf("%v gave %d: only limited gives 429", c.kind, w.Code)
		}
		wantDB := int64(0)
		if c.kind == limiter.Allowed || c.kind == limiter.Degraded {
			wantDB = 1
		}
		if n := db.calls.Load(); n != wantDB {
			t.Errorf("%v: %d database calls, want %d", c.kind, n, wantDB)
		}
	}
}

// mappingLimiter answers every Acquire with kind, as the library would: a permit for allowed
// and degraded, a decision time for allowed and limited, and limited's retry delay.
type mappingLimiter struct{ kind limiter.ResultKind }

func (m mappingLimiter) Acquire(context.Context, string) httpapi.Decision {
	d := httpapi.Decision{Kind: m.kind}
	switch m.kind {
	case limiter.Allowed:
		d.DecisionNs, d.Permit = Frozen, mappingPermit{id: "d1_e2e"}
	case limiter.Degraded:
		d.Permit = mappingPermit{}
	case limiter.Limited:
		d.DecisionNs, d.RetryAfterNs = Frozen, int64(time.Second)
	case limiter.ValidationError, limiter.ClockError, limiter.Busy, limiter.Cancelled, limiter.ConfigError,
		limiter.NotReady, limiter.StateIntegrityError, limiter.CapacityError, limiter.Unavailable,
		limiter.OutcomeUnknown, limiter.ReceiptError:
	}
	return d
}

func (mappingLimiter) Health() limiter.Health           { return limiter.Health{} }
func (mappingLimiter) Metrics() limiter.MetricsSnapshot { return limiter.MetricsSnapshot{} }
func (mappingLimiter) Close()                           {}

// mappingPermit is an allowed (id set) or degraded permit that ignores how it is finished.
type mappingPermit struct{ id string }

func (mappingPermit) Done()                 {}
func (mappingPermit) Release()              {}
func (p mappingPermit) DeductionID() string { return p.id }

// mappingDB commits every operation and counts them.
type mappingDB struct{ calls atomic.Int64 }

func (d *mappingDB) Hit(_ context.Context, _, _ string, arrivalNs int64, _ <-chan struct{}) dbclient.Result {
	d.calls.Add(1)
	return dbclient.Result{ArrivalNs: arrivalNs, Class: dbclient.Committed}
}

// mappingLog is a run log that keeps nothing.
type mappingLog struct{}

func (mappingLog) Enqueue(any)                          {}
func (mappingLog) Drops() int64                         { return 0 }
func (mappingLog) Len() int                             { return 0 }
func (mappingLog) Drain(context.Context) (int64, error) { return 0, nil }
