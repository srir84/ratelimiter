//go:build rltest

package e2e

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/fixtures"
	"ratelimiter/limiter"
)

// T-HFLT-2 to T-HFLT-5's bounds (IMPLEMENTATION_PLAN H4 Tests; HARNESS §8; plan choices unless
// noted).
const (
	// statementTimeoutMs is T-HFLT-2 (a) and (b)'s statement_timeout (plan H4 Tests): longer
	// than P, so P, not the statement timeout, ends the database work.
	statementTimeoutMs = 10_000
	// cancelledAtMin and cancelledAtMax bound limiter_ns + db_ns of a request cancelled at P,
	// P measured from just before acquire (plan H4 Tests: [5,000, 5,300] ms at P = 5 s).
	cancelledAtMin = 5000 * time.Millisecond
	cancelledAtMax = 5300 * time.Millisecond
	// replyDelay is T-HFLT-2 (b)'s delay after the acquire's reply: most of min(P, D) = 2 s.
	replyDelay = 1800 * time.Millisecond
	// shortPause is T-HFLT-2 (c)'s store pause (plan H4 Tests): longer than D, so D passes
	// while an acquire sent at its start is in flight; sendWithin is how soon after the pause
	// begins its five requests go out, concurrently.
	shortPause = 2500 * time.Millisecond
	sendWithin = 100 * time.Millisecond
	concurrent = 5
	// drainExit bounds T-HFLT-5's hitapi from SIGTERM to its exit (plan H4 Tests: 45 s, its
	// grace P + drain_timeout + 5 s), and refuseWithin from SIGTERM to its port refusing new
	// connections (1 s).
	drainExit    = 45 * time.Second
	refuseWithin = time.Second
	// refusedRequests is how many /hit requests T-HFLT-5 sends to the instance it stops.
	refusedRequests = 20
)

// TestT_HFLT_1 (HARNESS §8): with max_in_flight = 2 and two /hit requests blocked on keys
// held by open uncommitted inserts, a third gets 503 service_overloaded at once with no
// acquire sent (the acquire total is unchanged by it) and writes nothing (HAR-14, ERR-7,
// HOST-7); once the inserts roll back, the two finish with 200.
func TestT_HFLT_1(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.API.MaxInFlight = 2 }})
	api := s.APIs[0]
	rollbackA := s.holdKey(t, "fa", "r1")
	rollbackB := s.holdKey(t, "fb", "r1")
	heldA := api.Go(t, http.MethodGet, "/hit", IDs("fa", "r1"), "")
	heldB := api.Go(t, http.MethodGet, "/hit", IDs("fb", "r1"), "")
	s.waitLockWaiters(t, 2)

	if h := api.Health(t); Int(t, h, "in_flight") != 2 {
		t.Errorf("/health in_flight %v with both requests held, want 2", h["in_flight"])
	}
	before := api.AcquireTotal(t)
	if before != 2 {
		t.Errorf("acquire total %d with both requests held, want 2", before)
	}
	third := api.Hit(t, "fc", "r1")
	wantError(t, "the third /hit", third, http.StatusServiceUnavailable, "service_overloaded")
	if took := third.After.Sub(third.Before); took > time.Second {
		t.Errorf("the third /hit answered after %v, want at once: there is no queue", took)
	}
	if after := api.AcquireTotal(t); after != before {
		t.Errorf("acquire total went from %d to %d over the refused /hit, want unchanged: no acquire sent", before, after)
	}
	if n := s.keyRows(t, "fc", "r1"); n != 0 {
		t.Errorf("rows for (fc, r1) = %d, want 0", n)
	}

	for _, rb := range []func() error{rollbackA, rollbackB} {
		if err := rb(); err != nil {
			t.Fatalf("rolling back a held key: %v", err)
		}
	}
	wantCreated(t, "the first held /hit", Wait(t, heldA, 10*time.Second, "the first held /hit"), "fa", "r1")
	wantCreated(t, "the second held /hit", Wait(t, heldB, 10*time.Second, "the second held /hit"), "fb", "r1")
	if n := s.rows(t); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}

// withStatementTimeout returns dsn, harness.toml's db_dsn (a postgres:// URL), with
// statement_timeout set to ms. A DSN that does not parse fails the test without quoting it.
func withStatementTimeout(t *testing.T, dsn string, ms int) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("harness.toml's db_dsn does not parse")
	}
	q := u.Query()
	q.Set("statement_timeout", strconv.Itoa(ms))
	u.RawQuery = q.Encode()
	return u.String()
}

// TestT_HFLT_2 (HARNESS §8 T-HFLT-2: HTTP-13, HAR-11, HOST-3; IMPLEMENTATION_PLAN H4 Tests), on
// bin/test hitapi stacks with a frozen controlled clock, P = 5 s and D = 2 s (harness.toml).
// (a) With statement_timeout = 10000 in db_dsn, the /hit's insert waits behind a test
// transaction holding the same (user_id, request_id) uncommitted (dbclient.HoldKey), and the
// API cancels it at P: limiter_ns + db_ns from 5,000 to 5,300 ms; either a confirmed rollback,
// 503 database_unavailable (ERR-14) with its release record and the refund applied before the
// receipt's expiry (back to 20 tokens), or 503 application_outcome_unknown (ERR-16) with no
// release and no refund record (19 tokens: done()). (b) The same with the acquire's reply
// delayed 1,800 ms (delay after_reply): limiter_ns at least 1,800 ms, still cancelled at P
// measured from before acquire (the same window). (c) Store pause 2.5s, in each failure mode,
// with five /hit requests sent concurrently within the first 100 ms of the pause: the send
// observer (/_test/sends; its positive control the allowed /hit before the pause) shows which
// acquires were sent, and exactly that many answers are 503 limiter_outcome_unknown (ERR-9), at
// least one; every other answer is 503 limiter_deadline_exceeded (ERR-9a) or, never sent,
// closed: 503 limiter_unavailable (ERR-8); open: 200 degraded, its request record with no
// deduction_id. In every case no decision record says allowed, there is no release record, and
// only a degraded request's key holds a row.
func TestT_HFLT_2(t *testing.T) {
	for _, c := range []struct {
		name  string
		delay bool // (b): the acquire's reply delayed replyDelay
	}{{"a", false}, {"b", true}} {
		t.Run(c.name, func(t *testing.T) {
			s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) {
				f.API.DBDSN = withStatementTimeout(t, f.API.DBDSN, statementTimeoutMs)
			}})
			api := s.APIs[0]
			rollback := s.holdKey(t, "h2", "r1")
			if c.delay {
				api.Control(t, "/_test/faults", `{"target":"store","mode":"delay","match":"acquire","count":1,"delay_ms":1800,"at":"after_reply"}`)
			}
			r := api.Hit(t, "h2", "r1")
			rec := requestOf(t, api, "h2", "r1")
			limiterNs, dbNs := time.Duration(Int(t, rec, "limiter_ns")), time.Duration(Int(t, rec, "db_ns"))
			if sum := limiterNs + dbNs; sum < cancelledAtMin || sum > cancelledAtMax {
				t.Errorf("limiter_ns %v + db_ns %v = %v, want %v to %v: cancelled at P from before acquire", limiterNs, dbNs, sum, cancelledAtMin, cancelledAtMax)
			}
			if c.delay && limiterNs < replyDelay {
				t.Errorf("limiter_ns %v, want at least the %v delay after the acquire's reply", limiterNs, replyDelay)
			}
			id, _ := rec["deduction_id"].(string)
			if rec["result"] != "allowed" || id == "" {
				t.Fatalf("the request record %v, want result allowed with a deduction_id", rec)
			}
			releases, refunds := recordsOf(t, api, "release", id), recordsOf(t, api, "refund", id)
			switch code, _ := r.JSON["error"].(string); {
			case r.Status == http.StatusServiceUnavailable && code == "database_unavailable": // ERR-14
				if len(releases) != 1 {
					t.Errorf("ERR-14: %d release records for %s, want 1", len(releases), id)
				}
				ref := refundOf(t, api, id)
				expiry := Int(t, rec, "decision_ns") + s.Config.Policy.ReceiptLifetimeNs
				if ref["result"] != "applied" || Int(t, ref, "credit_decision_ns") >= expiry {
					t.Errorf("ERR-14: the refund record %v, want applied, credited before the receipt's expiry %d", ref, expiry)
				}
				if err := rollback(); err != nil {
					t.Fatalf("rolling back the held key: %v", err)
				}
				wantRows(t, s, "h2", "r1", 0)
				wantTokens(t, s, "h2", 20, "charged, then refunded")
				t.Logf("ERR-14 after limiter_ns %v + db_ns %v = %v", limiterNs, dbNs, limiterNs+dbNs)
			case r.Status == http.StatusServiceUnavailable && code == "application_outcome_unknown": // ERR-16
				if len(releases) != 0 || len(refunds) != 0 {
					t.Errorf("ERR-16: release records %v and refund records %v for %s, want none: done(), no refund", releases, refunds, id)
				}
				wantTokens(t, s, "h2", 19, "done(): charged, never refunded")
				if err := rollback(); err != nil {
					t.Fatalf("rolling back the held key: %v", err)
				}
				if n := s.keyRows(t, "h2", "r1"); n > 1 {
					t.Errorf("ERR-16: rows for (h2, r1) = %d, want 0 or 1", n)
				}
				t.Logf("ERR-16 after limiter_ns %v + db_ns %v = %v", limiterNs, dbNs, limiterNs+dbNs)
			default:
				t.Errorf("the /hit whose database work outlasted P: %d %s, want 503 database_unavailable (ERR-14) or application_outcome_unknown (ERR-16)", r.Status, r.Body)
			}
		})
	}

	for _, mode := range []limiter.FailureMode{limiter.Closed, limiter.Open} {
		t.Run("c-"+mode.String(), func(t *testing.T) { checkPausedAcquires(t, mode) })
	}
}

// checkPausedAcquires is T-HFLT-2 (c) in one failure mode (TestT_HFLT_2).
func checkPausedAcquires(t *testing.T, mode limiter.FailureMode) {
	s := StartStack(t, StackOpts{Clock: Frozen, Edit: func(f *config.File) { f.Policy.FailureMode = mode }})
	api := s.APIs[0]
	d := time.Duration(s.Config.Instance.DependencyDeadlineNs)
	controlHit(t, api, "h2c-control", "before")
	since := lastSend(t, api)
	target, pid := storeTarget(t, s), s.Store.PID
	fault := startStoreFault(t, func(ctx context.Context) (fixtures.Outage, error) {
		return fixtures.StorePause(ctx, target, shortPause)
	})
	waitStopped(t, pid)
	var answers [concurrent]<-chan Answer
	for i := range answers {
		answers[i] = api.Go(t, http.MethodGet, "/hit", IDs("h2c", "c"+strconv.Itoa(i+1)), "")
	}
	var resps [concurrent]Response
	for i, ch := range answers {
		resps[i] = Wait(t, ch, d+stageWait, "paused /hit c"+strconv.Itoa(i+1))
	}
	out := awaitStoreFault(t, fault, "store pause 2.5s ("+mode.String()+")", shortPause)

	sent := acquiresSent(sendsSince(t, api, since))
	unknown, latest := 0, time.Duration(0)
	for i, r := range resps {
		request := "c" + strconv.Itoa(i+1)
		lag := time.Duration(r.Before.UnixNano() - out.StartNs)
		if lag < 0 || lag > sendWithin {
			t.Errorf("/hit %s went out %v after the pause began, want within the first %v", request, lag, sendWithin)
		}
		latest = max(latest, lag)
		rec := requestOf(t, api, "h2c", request)
		code, _ := r.JSON["error"].(string)
		rows := int64(0)
		switch {
		case r.Status == http.StatusServiceUnavailable && code == "limiter_outcome_unknown": // ERR-9: sent
			unknown++
		case r.Status == http.StatusServiceUnavailable && code == "limiter_deadline_exceeded": // ERR-9a
		case mode == limiter.Closed && r.Status == http.StatusServiceUnavailable && code == "limiter_unavailable": // ERR-8
		case mode == limiter.Open && r.Status == http.StatusOK && r.JSON["degraded"] == true: // degraded, ERR-3
			rows = 1
			if id, _ := rec["deduction_id"].(string); id != "" {
				t.Errorf("degraded /hit %s: the request record's deduction_id %q, want none", request, id)
			}
		default:
			t.Errorf("mode %v: /hit %s under the pause: %d %s, want ERR-9a, ERR-9, or %s", mode, request, r.Status, r.Body,
				map[limiter.FailureMode]string{limiter.Closed: "ERR-8", limiter.Open: "200 degraded"}[mode])
		}
		wantRows(t, s, "h2c", request, rows)
	}
	if unknown != sent || sent == 0 {
		t.Errorf("mode %v: the send observer shows %d acquires sent under the pause and %d answers are limiter_outcome_unknown; want the same count, at least 1: an acquire in flight when D passes is outcome_unknown, one never sent is not",
			mode, sent, unknown)
	}
	for _, rec := range api.Records(t, "decision") {
		if rec["user_id"] == "h2c" && rec["result"] == "allowed" {
			t.Errorf("mode %v: decision record %v, want none allowed under the pause", mode, rec)
		}
	}
	if recs := api.Records(t, "release"); len(recs) != 0 {
		t.Errorf("mode %v: release records %v, want none", mode, recs)
	}
	t.Logf("mode %v: the %d requests went out within %v of the pause's start; %d acquires sent, as many limiter_outcome_unknown", mode, concurrent, latest, sent)
}

// runHitgen runs bin/test/hitgen on the testdata scenario against the instance, waits up to
// hitgenWait for it to exit 0, and returns its --out directory.
func runHitgen(t *testing.T, s *Stack, scenario string, a *API) string {
	t.Helper()
	bin := s.bin("hitgen")
	if err := checkBinary(bin); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	args := []string{"--scenario", testdataPath(t, scenario), "--targets", a.Addr, "--out", out}
	c, err := startProcess(t, s.root, dir, bin, args, []string{}, "hitgen")
	if err != nil {
		t.Fatal(err)
	}
	if !c.exitedWithin(hitgenWait) {
		c.kill(t)
		t.Fatalf("hitgen did not exit within %v", hitgenWait)
	}
	c.checkRecord(t)
	output, err := os.ReadFile(c.Out)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := exitCode(c.waitErr); err != nil || code != 0 {
		t.Fatalf("hitgen exit code %d (%v), want 0; output:\n%s", code, err, output)
	}
	return out
}

// TestT_HFLT_3 (HARNESS §8 T-HFLT-3: ERR-16, FLT-9, HOST-2; IMPLEMENTATION_PLAN H4 Tests): with
// the database wrapper's lose_commit_reply armed (the insert commits, then an unknown outcome is
// returned), a one-request bin/test/hitgen run (testdata/hflt3.toml, its schedule one entry) gets
// 503 application_outcome_unknown and logs exactly 1 attempt, no retry (gen.jsonl: one attempt
// line, then the summary with unsent 0), and the instance saw that one attempt. Its request
// record carries ERR-16 (503 application_outcome_unknown, result allowed); its permit got
// done(): once the instance has stopped (SIGTERM, every refund final) its run log has no
// release and no refund record for the deduction_id, and the bucket holds 19 tokens, the
// charge kept; the key holds 0 or 1 rows.
func TestT_HFLT_3(t *testing.T) {
	s := StartStack(t, StackOpts{Clock: Frozen})
	api := s.APIs[0]
	api.Control(t, "/_test/faults", `{"target":"db","mode":"lose_commit_reply","count":1}`)
	out := runHitgen(t, s, "hflt3.toml", api)

	type entry struct {
		User      string `json:"user"`
		RequestID string `json:"request_id"`
	}
	schedule := readJSONLines[entry](t, filepath.Join(out, "schedule.jsonl"))
	if len(schedule) != 1 {
		t.Fatalf("schedule.jsonl holds %d entries %v, want the one request of testdata/hflt3.toml", len(schedule), schedule)
	}
	user, request := schedule[0].User, schedule[0].RequestID
	gen := readJSONLines[orchGenRecord](t, filepath.Join(out, "gen.jsonl"))
	if len(gen) != 2 || gen[1].Kind != "summary" || gen[1].Unsent == nil || *gen[1].Unsent != 0 {
		t.Fatalf("gen.jsonl %+v, want one attempt line, then the summary with unsent 0", gen)
	}
	if a := gen[0]; a.Kind != "initial" || a.Attempt != 1 || a.RequestID != request || a.SentNs == nil ||
		a.Status != http.StatusServiceUnavailable || nullable(a.Error) != "application_outcome_unknown" {
		t.Errorf("gen.jsonl's attempt %+v (error %s), want attempt 1 of %s, sent, 503 application_outcome_unknown: one attempt, no auto-retry",
			a, nullable(a.Error), request)
	}
	seen := 0
	for _, rec := range api.Records(t, "request") {
		if rec["request_id"] == request {
			seen++
		}
	}
	rec := requestOf(t, api, user, request)
	id, _ := rec["deduction_id"].(string)
	if seen != 1 || Int(t, rec, "http_status") != http.StatusServiceUnavailable || rec["error"] != "application_outcome_unknown" ||
		rec["result"] != "allowed" || id == "" {
		t.Errorf("%d request records for %s; the record %v; want one, 503 application_outcome_unknown (ERR-16), result allowed with a deduction_id",
			seen, request, rec)
	}

	if code, ok := api.Terminate(t); !ok || code != 0 {
		t.Fatalf("hitapi exited %d (in its grace %v) after SIGTERM, want 0", code, ok)
	}
	if releases, refunds := recordsOf(t, api, "release", id), recordsOf(t, api, "refund", id); len(releases) != 0 || len(refunds) != 0 {
		t.Errorf("release records %v and refund records %v for %s, want none: done(), no refund", releases, refunds, id)
	}
	wantTokens(t, s, user, 19, "done(): the charge kept, no refund")
	if n := s.keyRows(t, user, request); n > 1 {
		t.Errorf("rows for (%s, %s) = %d, want 0 or 1", user, request, n)
	}
}

// waitConnRefused polls connects to addr until one is refused, and fails the test unless that
// comes within d of since.
func waitConnRefused(t *testing.T, addr string, since time.Time, d time.Duration) time.Duration {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(t.Context(), d)
		var dialer net.Dialer
		c, err := dialer.DialContext(ctx, "tcp", addr)
		cancel()
		took := time.Since(since)
		if errors.Is(err, syscall.ECONNREFUSED) {
			if took > d {
				t.Errorf("connects to %s refused %v after SIGTERM, want within %v", addr, took, d)
			}
			return took
		}
		if err == nil {
			_ = c.Close() // only the connect mattered
		}
		if took > d {
			t.Fatalf("%s still took connections %v after SIGTERM (%v), want them refused within %v", addr, took, err, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestT_HFLT_5 (HARNESS §8 T-HFLT-5: HAR-15, HOST-6; IMPLEMENTATION_PLAN H4 Tests): three
// bin/test hitapi instances (frozen controlled clock); instance 1 has refuse_after_acquire_bp =
// 10000 (its own config file) and its store wrapper refuses refunds for 3 s (refuse
// match:refund duration_ms:3000). Its 20 /hit requests are each refused after the acquire, 503
// service_aborted_after_acquire, their permits released, so 20 refunds are pending (no refund
// record yet) when it gets SIGTERM. Then new connections to its port are refused within 1 s;
// instances 2 and 3 answer 200 while it drains; it exits 0 within 45 s, its run log holding
// one refund record, applied, for each of its 20 release records, and no other.
func TestT_HFLT_5(t *testing.T) {
	s := StartStack(t, StackOpts{Instances: 3, Clock: Frozen,
		PerInstanceConfig: map[int]string{1: "[api]\nrefuse_after_acquire_bp = 10000\n"}})
	api1 := s.APIs[0]
	api1.Control(t, "/_test/faults", `{"target":"store","mode":"refuse","match":"refund","duration_ms":3000}`)
	for i := 1; i <= refusedRequests; i++ {
		user := "h5-" + strconv.Itoa(i)
		wantError(t, "hitapi 1's /hit for "+user, api1.Hit(t, user, "r1"), http.StatusServiceUnavailable, "service_aborted_after_acquire")
	}
	releases := api1.WaitRecords(t, "release", func(rs []map[string]any) bool { return len(rs) == refusedRequests })
	if refunds := api1.Records(t, "refund"); len(refunds) != 0 {
		t.Fatalf("refund records %v before SIGTERM, want none: the store wrapper refuses refunds for 3 s", refunds)
	}

	api1.signal(t, syscall.SIGTERM)
	sigterm := time.Now()
	refusedAfter := waitConnRefused(t, api1.Addr, sigterm, refuseWithin)
	for _, a := range s.APIs[1:] {
		user := "h5-other-" + strconv.Itoa(a.N)
		wantCreated(t, "hitapi "+strconv.Itoa(a.N)+" during hitapi 1's drain", a.Hit(t, user, "r1"), user, "r1")
	}
	select {
	case <-api1.Exited():
		t.Errorf("hitapi 1 had exited when instances 2 and 3 answered, want them answered during its drain")
	default:
	}
	if !api1.exitedWithin(drainExit - time.Since(sigterm)) {
		api1.kill(t)
		t.Fatalf("hitapi 1 still ran %v after SIGTERM, want it exited within %v", time.Since(sigterm), drainExit)
	}
	exitedAfter := time.Since(sigterm)
	api1.checkRecord(t)
	if code, err := exitCode(api1.waitErr); err != nil || code != 0 {
		out, _ := os.ReadFile(api1.Out)
		t.Fatalf("hitapi 1 exited %d (%v) after SIGTERM, want 0; its output:\n%s", code, err, out)
	}

	refunds := map[string][]map[string]any{}
	all := api1.Records(t, "refund")
	for _, rec := range all {
		id, _ := rec["deduction_id"].(string)
		refunds[id] = append(refunds[id], rec)
	}
	for _, rel := range releases {
		id, _ := rel["deduction_id"].(string)
		if rs := refunds[id]; len(rs) != 1 || rs[0]["result"] != "applied" {
			t.Errorf("refund records for released %s: %v, want exactly one, applied", id, rs)
		}
	}
	if len(all) != len(releases) {
		t.Errorf("hitapi 1's run log: %d refund records, %d release records, want one refund per release", len(all), len(releases))
	}
	t.Logf("hitapi 1: connects refused %v and exit 0 %v after SIGTERM; %d release and %d refund records", refusedAfter, exitedAfter, len(releases), len(all))
}
