//go:build rltest

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"ratelimiter/harness/dbclient"
	"ratelimiter/harness/pgtest"
	"ratelimiter/harness/runlog"
	"ratelimiter/limiter"
)

// The harness cluster (DESIGN §B2 db_dsn), and the same with a pool of one connection: a /hit
// whose insert waits for a held key keeps it, so the next /hit waits in the pool.
const (
	pgDSN        = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64"
	pgOneConnDSN = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=1"
)

// openPG opens a dbclient on dsn and closes it at cleanup.
func openPG(t *testing.T, dsn string) *dbclient.Client {
	t.Helper()
	c, err := dbclient.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("dbclient.Open: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// TestHTTPAPI_ShutdownEndsConnectionWait: a /hit still waiting for a database connection when
// SIGTERM begins (BeginShutdown) is refused at once, long before P: release() and 503
// service_aborted_after_acquire (ERR-15, HAR-15), with no statement sent. The real handler and
// the real dbclient run on the harness PostgreSQL with a pool of one connection, which a /hit
// blocked behind an uncommitted insert of its key holds; that /hit's statement was sent before
// the shutdown, so it finishes with 200 once the insert rolls back.
func TestHTTPAPI_ShutdownEndsConnectionWait(t *testing.T) {
	pgtest.Ensure(t)
	holder := openPG(t, pgDSN)
	if err := holder.Truncate(t.Context()); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	db := openPG(t, pgOneConnDSN)

	pa, pb := &fakePermit{id: "da"}, &fakePermit{id: "db"}
	lim := &fakeLimiter{acquire: func(_ context.Context, key string) Decision {
		p := pa
		if key == "sdw-b" {
			p = pb
		}
		return Decision{Kind: limiter.Allowed, DecisionNs: testDecisionNs, Permit: p}
	}, health: limiter.Health{Status: "ready", Reason: "ready", ClockSource: "store"}}
	logPath := filepath.Join(t.TempDir(), "api-1.jsonl")
	w, err := runlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("closing the run log: %v", err)
		}
	})
	s, err := New(testConfig(), lim, db, w)
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{s: s, h: s.Handler(), lim: lim, log: w, logPath: logPath}

	rollback, err := holder.HoldKey(t.Context(), "sdw-a", "r1")
	if err != nil {
		t.Fatalf("HoldKey: %v", err)
	}
	t.Cleanup(func() {
		if err := rollback(); err != nil {
			t.Errorf("rolling back the held key: %v", err)
		}
	})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- ts.get(t, ids1("sdw-a", "r1")) }()
	// The first /hit holds the pool's one connection once its insert waits for the held key.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		n, err := holder.LockWaiters(t.Context())
		if err != nil {
			t.Fatalf("LockWaiters: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first /hit's insert never waited for the held key")
		}
	}

	type answer struct {
		rec *httptest.ResponseRecorder
		at  time.Time
	}
	second := make(chan answer, 1)
	go func() {
		rec := ts.get(t, ids1("sdw-b", "r1"))
		second <- answer{rec, time.Now()}
	}()
	time.Sleep(300 * time.Millisecond) // the second /hit waits in the pool by now
	select {
	case a := <-second:
		t.Fatalf("the second /hit answered %d %s before SIGTERM, want it waiting for the pool's one connection", a.rec.Code, a.rec.Body)
	default:
	}
	begun := time.Now()
	ts.s.BeginShutdown()
	timer := time.NewTimer(waitFor)
	defer timer.Stop()
	select {
	case a := <-second:
		checkResponse(t, a.rec, http.StatusServiceUnavailable, errAborted)
		if took := a.at.Sub(begun); took > time.Second {
			t.Errorf("the waiting /hit answered %v after BeginShutdown, want at once (within 1 s; P is %v)", took, testP)
		}
	case <-timer.C:
		t.Fatal("the /hit waiting for a connection has not answered after BeginShutdown")
	}
	if d, r := pb.finished(); d != 0 || r != 1 {
		t.Errorf("the refused /hit's permit: done %d, released %d; want 0, 1", d, r)
	}
	if n, err := holder.CountKey(t.Context(), "sdw-b", "r1"); err != nil || n != 0 {
		t.Errorf("rows for (sdw-b, r1) = %d, %v; want 0: nothing may be sent", n, err)
	}

	if err := rollback(); err != nil {
		t.Fatalf("rolling back the held key: %v", err)
	}
	checkResponse(t, recvAnswer(t, first), http.StatusOK, "")
	if d, r := pa.finished(); d != 1 || r != 0 {
		t.Errorf("the /hit whose statement was sent: done %d, released %d; want 1, 0", d, r)
	}
	var releases []string
	for _, rec := range ts.records(t, kindRelease) {
		releases = append(releases, rec["deduction_id"].(string))
	}
	if len(releases) != 1 || releases[0] != "db" {
		t.Errorf("release records for %q, want one for db", releases)
	}
}
