//go:build rltest

package dbclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"ratelimiter/harness/pgtest"
)

// testDSN is the DESIGN §B2 db_dsn: the harness cluster, a 2.5 s statement_timeout.
const testDSN = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64"

// oneConnDSN is testDSN with a pool of one connection: a connection that a Hit keeps leaves
// none for the next Hit.
const oneConnDSN = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=1"

// closeLimit bounds a client's Close at cleanup.
const closeLimit = 10 * time.Second

// The tests share the hits table, so none runs in parallel; each truncates it first and uses
// its own keys, so a statement another test left running cannot change its counts.
func TestMain(m *testing.M) {
	if err := pgtest.Start(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbclient tests: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// openClient opens a client on the harness cluster, closed at cleanup, and truncates hits.
func openClient(t *testing.T) *Client {
	t.Helper()
	c := openDSN(t, testDSN)
	if err := c.Truncate(t.Context()); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	return c
}

// openDSN opens a client on dsn and closes it at cleanup. Close waits for every acquired
// connection, so a connection never given back would hang the test binary: the test fails
// instead once Close has not returned within closeLimit.
func openDSN(t *testing.T, dsn string) *Client {
	t.Helper()
	c, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		timer := time.NewTimer(closeLimit)
		defer timer.Stop()
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			c.Close()
		}()
		select {
		case <-closed:
		case <-timer.C:
			t.Errorf("Close has not returned after %v: a pool connection was never given back", closeLimit)
		}
	})
	return c
}

// hold holds the key in an open transaction and rolls it back at cleanup at the latest.
func hold(t *testing.T, c *Client, userID, requestID string) func() error {
	t.Helper()
	rollback, err := c.HoldKey(t.Context(), userID, requestID)
	if err != nil {
		t.Fatalf("HoldKey: %v", err)
	}
	t.Cleanup(func() {
		if err := rollback(); err != nil {
			t.Errorf("rolling back the held key: %v", err)
		}
	})
	return rollback
}

func countKey(t *testing.T, c *Client, userID, requestID string) int64 {
	t.Helper()
	n, err := c.CountKey(t.Context(), userID, requestID)
	if err != nil {
		t.Fatalf("CountKey: %v", err)
	}
	return n
}

// TestDBClient_DSNSecrets: Open refuses a db_dsn that carries a password, in each form pgconn
// reads one (the URL userinfo, a ?password= parameter, its name percent-encoded too, a
// key=value pair), with a fixed error; and a DSN that does not parse gets a fixed error too.
// No error quotes the DSN (AGENTS.md §1 Secrets): pgconn's own parse error redacts a userinfo
// or key=value password but quotes a ?password= parameter. Nothing here connects.
func TestDBClient_DSNSecrets(t *testing.T) {
	const mark = "SECRETMARK"
	for _, dsn := range []string{
		"postgres://rl:" + mark + "@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64",
		"postgres://rl@127.0.0.1:5433/hits?password=" + mark + "&pool_max_conns=bad", // the review's probe
		"postgresql://rl@127.0.0.1:5433/hits?pass%77ord=" + mark,
		"POSTGRES://rl:" + mark + "@127.0.0.1:5433/hits",
		"host=127.0.0.1 port=5433 user=rl dbname=hits password=" + mark,
		"host=127.0.0.1 port=5433 user=rl dbname=hits password='" + mark + " 2' pool_max_conns=bad",
	} {
		c, err := Open(t.Context(), dsn)
		if err == nil {
			c.Close()
			t.Errorf("Open(%s) succeeded, want the DSN refused for its password", dsn)
			continue
		}
		if !errors.Is(err, errDSNPassword) || strings.Contains(err.Error(), mark) {
			t.Errorf("Open(%s) = %q, want the fixed password refusal, which quotes no part of the DSN", dsn, err)
		}
	}
	for _, dsn := range []string{
		"postgres://rl:" + mark + "@127.0.0.1:bad/hits",            // a userinfo password in a URL that does not parse
		"postgres://rl@127.0.0.1:5433/hits?pool_max_conns=" + mark, // no password; a pool size that does not parse
	} {
		c, err := Open(t.Context(), dsn)
		if err == nil {
			c.Close()
			t.Errorf("Open(%s) succeeded, want a parse error", dsn)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, mark) || strings.Contains(msg, "127.0.0.1") {
			t.Errorf("Open(%s) = %q, want a fixed error that quotes no part of the DSN", dsn, err)
		}
	}
}

func TestDBClient_InsertThenExisting(t *testing.T) {
	c := openClient(t)
	ctx := t.Context()
	steps := []struct {
		user    string
		arrival int64
		want    Result
	}{
		{"ite-a", 1_000, Result{ArrivalNs: 1_000, Existed: false, Class: Committed}},
		// The same key again keeps the original arrival (HTTP-10).
		{"ite-a", 2_000, Result{ArrivalNs: 1_000, Existed: true, Class: Committed}},
		// Another user may reuse the request ID (HTTP-4).
		{"ite-b", 3_000, Result{ArrivalNs: 3_000, Existed: false, Class: Committed}},
	}
	for i, s := range steps {
		if got := c.Hit(ctx, s.user, "r1", s.arrival, nil); got != s.want {
			t.Errorf("step %d: Hit(%s, r1, %d) = %+v, want %+v", i+1, s.user, s.arrival, got, s.want)
		}
	}
	for _, u := range []string{"ite-a", "ite-b"} {
		if n := countKey(t, c, u, "r1"); n != 1 {
			t.Errorf("%d rows for (%s, r1), want 1", n, u)
		}
	}
}

func TestDBClient_ConcurrentSameKey(t *testing.T) {
	const n = 20
	c := openClient(t)
	ctx := t.Context()
	results := make([]Result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = c.Hit(ctx, "csk", "r1", int64(1_000*(i+1)), nil)
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, r := range results {
		if r.Class != Committed || r.Err != nil {
			t.Errorf("goroutine %d: %+v, want Committed", i, r)
		}
		if !r.Existed {
			if winner >= 0 {
				t.Errorf("goroutines %d and %d both inserted the row", winner, i)
			}
			winner = i
		}
	}
	if winner < 0 {
		t.Fatal("no goroutine inserted the row")
	}
	want := int64(1_000 * (winner + 1))
	for i, r := range results {
		if r.ArrivalNs != want {
			t.Errorf("goroutine %d: ArrivalNs %d, want the inserted row's %d", i, r.ArrivalNs, want)
		}
	}
	if got := countKey(t, c, "csk", "r1"); got != 1 {
		t.Errorf("%d rows for the key, want 1", got)
	}
}

func TestDBClient_RefusedIsNoRow(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Open(t.Context(), fmt.Sprintf("postgres://rl@127.0.0.1:%d/hits?connect_timeout=5&pool_max_conns=4", port))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(c.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got := c.Hit(ctx, "ref", "r1", 1_000, nil)
	if got.Class != FailedNoRow || got.Existed || got.ArrivalNs != 0 {
		t.Errorf("Hit on a closed port = %+v, want FailedNoRow", got)
	}
	if !errors.Is(got.Err, syscall.ECONNREFUSED) {
		t.Errorf("Err = %v, want a refused connection", got.Err)
	}
}

func TestDBClient_StatementTimeoutIsNoRow(t *testing.T) {
	c := openClient(t)
	ctx := t.Context()
	rollback := hold(t, c, "stmt", "r1")
	held := time.Now()
	// The other transaction holds the key for 3 s, longer than the 2.5 s statement_timeout.
	released := make(chan error, 1)
	go func() {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		released <- rollback()
	}()

	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	got := c.Hit(hctx, "stmt", "r1", 1_000, nil)
	elapsed := time.Since(held)
	var pgErr *pgconn.PgError
	if got.Class != FailedNoRow || !errors.As(got.Err, &pgErr) || pgErr.Code != "57014" {
		t.Errorf("Hit behind the held key = %+v, want FailedNoRow with SQLSTATE 57014", got)
	}
	if elapsed < 2500*time.Millisecond || elapsed >= 3*time.Second {
		t.Errorf("Hit returned %v after the hold began, want the 2.5 s statement_timeout, before the hold ends at 3 s", elapsed)
	}
	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("rolling back the held key: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held key was not released")
	}
	if n := countKey(t, c, "stmt", "r1"); n != 0 {
		t.Errorf("%d rows for the key after the statement timed out, want 0", n)
	}
}

func TestDBClient_DeadlineAfterSendIsUnknown(t *testing.T) {
	c := openClient(t)
	rollback := hold(t, c, "late", "r1")
	hctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := c.Hit(hctx, "late", "r1", 1_000, nil)
	elapsed := time.Since(start)
	var pgErr *pgconn.PgError
	if got.Class != Unknown || !errors.Is(got.Err, context.DeadlineExceeded) || errors.As(got.Err, &pgErr) ||
		pgconn.SafeToRetry(got.Err) {
		t.Errorf("Hit cut off by its deadline behind the held key = %+v, want Unknown from the deadline", got)
	}
	if elapsed < 200*time.Millisecond || elapsed >= 2500*time.Millisecond {
		t.Errorf("Hit returned after %v, want the 200 ms deadline, before the 2.5 s statement_timeout", elapsed)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rolling back the held key: %v", err)
	}
	// The server still runs the abandoned insert: once the key is free it may write the row
	// (which is why the class is Unknown). Wait for it, so it lands before the next test.
	deadline := time.Now().Add(5 * time.Second)
	for countKey(t, c, "late", "r1") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("rows for the key after the hold ended: %d (the abandoned insert may commit)", countKey(t, c, "late", "r1"))
}

// TestDBClient_RejectedValueIsNoRow: every server error with a SQLSTATE is FailedNoRow, not
// only the statement_timeout (DESIGN §B3). The server refuses a NUL byte in a text parameter
// (SQLSTATE 22021, character_not_in_repertoire), so the insert never runs.
func TestDBClient_RejectedValueIsNoRow(t *testing.T) {
	c := openClient(t)
	got := c.Hit(t.Context(), "bad\x00user", "r1", 1_000, nil)
	var pgErr *pgconn.PgError
	if got.Class != FailedNoRow || got.Existed || got.ArrivalNs != 0 || !errors.As(got.Err, &pgErr) || pgErr.Code != "22021" {
		t.Errorf("Hit with a NUL byte in the user ID = %+v, want FailedNoRow with SQLSTATE 22021", got)
	}
	// No text value can hold that key, so it cannot be looked up: count the table openClient
	// emptied instead.
	if n, err := c.CountRows(t.Context()); err != nil || n != 0 {
		t.Errorf("CountRows after the rejected Hit = %d, %v; want 0", n, err)
	}
}

// TestDBClient_PanicIsUnknown: a server error is FailedNoRow unless its severity is PANIC. A
// PANIC means the server crashed, perhaps inside the commit after its record reached the disk
// (reported after the INSERT's CommandComplete), so the row may exist: Unknown. The errors are
// built here; the shared server is never crashed.
func TestDBClient_PanicIsUnknown(t *testing.T) {
	cases := []struct {
		name string
		err  *pgconn.PgError
		want Class
	}{
		{"ERROR", &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "22021"}, FailedNoRow},
		{"FATAL", &pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01"}, FailedNoRow},
		{"PANIC", &pgconn.PgError{Severity: "PANIC", SeverityUnlocalized: "PANIC", Code: "58030"}, Unknown},
		// lc_messages translates Severity; SeverityUnlocalized never is.
		{"translated PANIC", &pgconn.PgError{Severity: "PANIK", SeverityUnlocalized: "PANIC", Code: "58030"}, Unknown},
		{"translated ERROR", &pgconn.PgError{Severity: "FEHLER", SeverityUnlocalized: "ERROR", Code: "22021"}, FailedNoRow},
		// Servers before PostgreSQL 9.6 send no SeverityUnlocalized.
		{"PANIC, Severity only", &pgconn.PgError{Severity: "PANIC", Code: "58030"}, Unknown},
		{"FATAL, Severity only", &pgconn.PgError{Severity: "FATAL", Code: "57P01"}, FailedNoRow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := fmt.Errorf("inserting the row: %w", c.err)
			if got := failed(err); got.Class != c.want || !errors.Is(got.Err, c.err) {
				t.Errorf("failed(%v) = %+v, want class %v with that error", err, got, c.want)
			}
		})
	}
}

// TestDBClient_ReleasesConnection: Hit gives its connection back to the pool whatever the
// outcome. With a pool of one connection, the Hit after a FailedNoRow Hit and the Hit after
// an Unknown Hit each get that connection within a short deadline and commit.
func TestDBClient_ReleasesConnection(t *testing.T) {
	holder := openClient(t)
	c := openDSN(t, oneConnDSN)
	commits := func(after, requestID string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		want := Result{ArrivalNs: 1_000, Class: Committed}
		if got := c.Hit(ctx, "rel", requestID, 1_000, nil); got != want {
			t.Errorf("Hit after %s = %+v, want %+v: the pool's one connection was not given back", after, got, want)
		}
	}

	var pgErr *pgconn.PgError
	if got := c.Hit(t.Context(), "bad\x00user", "r1", 1_000, nil); got.Class != FailedNoRow || !errors.As(got.Err, &pgErr) {
		t.Errorf("Hit with a NUL byte in the user ID = %+v, want FailedNoRow from a server error", got)
	}
	commits("a FailedNoRow Hit", "r1")

	rollback := hold(t, holder, "rel-held", "r1")
	hctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if got := c.Hit(hctx, "rel-held", "r1", 1_000, nil); got.Class != Unknown {
		t.Errorf("Hit cut off by its deadline behind the held key = %+v, want Unknown", got)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rolling back the held key: %v", err)
	}
	// The abandoned insert may commit now that the key is free: let it land before the next test.
	deadline := time.Now().Add(5 * time.Second)
	for countKey(t, holder, "rel-held", "r1") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	commits("an Unknown Hit", "r2")
}

// TestDBClient_CancelledSendsNothing: a stop channel that closes once Hit holds its connection
// cancels the work before its first statement: Cancelled, no row written, and the connection
// given back (hitapi's SIGTERM refusal, ERR-15). With a pool of one connection, a Hit made
// from the after-acquire hook finds no connection free, so the last check runs while Hit
// holds it; a stop channel that stays open lets the work go on.
func TestDBClient_CancelledSendsNothing(t *testing.T) {
	holder := openClient(t)
	c := openDSN(t, oneConnDSN)
	stop := make(chan struct{})
	checks := 0
	var inner Result
	c.afterAcquire = func() {
		checks++
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		inner = c.Hit(ctx, "cxl-inner", "r1", 2_000, nil)
		close(stop)
	}
	got := c.Hit(t.Context(), "cxl", "r1", 1_000, stop)
	c.afterAcquire = nil
	if got.Class != Cancelled || got.Existed || got.ArrivalNs != 0 || !errors.Is(got.Err, errCancelled) {
		t.Errorf("Hit whose stop closed once it held its connection = %+v, want Cancelled", got)
	}
	if checks != 1 {
		t.Errorf("the hook ran %d times, want 1", checks)
	}
	if inner.Class != FailedNoRow || !errors.Is(inner.Err, context.DeadlineExceeded) {
		t.Errorf("a Hit from the hook = %+v, want FailedNoRow from its deadline: the check must run while Hit holds the connection", inner)
	}
	for _, user := range []string{"cxl", "cxl-inner"} {
		if n := countKey(t, holder, user, "r1"); n != 0 {
			t.Errorf("%d rows for (%s, r1), want 0: nothing may be sent", n, user)
		}
	}
	// The connection went back to the pool, and a stop channel that stays open lets the insert
	// run.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if got := c.Hit(ctx, "cxl", "r1", 3_000, make(chan struct{})); got != (Result{ArrivalNs: 3_000, Class: Committed}) {
		t.Errorf("Hit after the cancelled Hit = %+v, want a new row", got)
	}
	if n := countKey(t, holder, "cxl", "r1"); n != 1 {
		t.Errorf("%d rows for (cxl, r1) after the committed Hit, want 1", n)
	}
	// A stop channel already closed when Hit starts sends nothing either.
	if got := c.Hit(ctx, "cxl", "r2", 4_000, stop); got.Class != Cancelled || !errors.Is(got.Err, errCancelled) {
		t.Errorf("Hit with a stop channel closed before it = %+v, want Cancelled", got)
	}
	if n := countKey(t, holder, "cxl", "r2"); n != 0 {
		t.Errorf("%d rows for (cxl, r2), want 0", n)
	}
}

// TestDBClient_StopEndsConnectionWait: a Hit still waiting for a pool connection when its stop
// channel closes stops waiting at once and returns Cancelled, long before its ctx ends, with
// nothing sent (hitapi: SIGTERM began while the pool was busy, ERR-15). The pool's one
// connection is held by a Hit blocked behind an uncommitted insert of its key; once that
// insert rolls back, the blocked Hit commits.
func TestDBClient_StopEndsConnectionWait(t *testing.T) {
	holder := openClient(t)
	c := openDSN(t, oneConnDSN)
	rollback := hold(t, holder, "wait-held", "r1")

	first := make(chan Result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		first <- c.Hit(ctx, "wait-held", "r1", 1_000, nil)
	}()
	// The first Hit holds the pool's one connection once its insert waits for the lock.
	if !waitUntil(5*time.Second, func() bool { return lockWaiters(t, holder) > 0 }) {
		t.Fatal("the first Hit's insert never waited for the held key")
	}

	stop := make(chan struct{})
	type answer struct {
		res Result
		at  time.Time
	}
	second := make(chan answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		res := c.Hit(ctx, "wait-stop", "r1", 2_000, stop)
		second <- answer{res, time.Now()}
	}()
	time.Sleep(300 * time.Millisecond) // the second Hit is waiting in the pool by now
	select {
	case a := <-second:
		t.Fatalf("the second Hit returned %+v before its stop closed, want it waiting for the pool's one connection", a.res)
	default:
	}
	closedAt := time.Now()
	close(stop)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case a := <-second:
		if a.res.Class != Cancelled || !errors.Is(a.res.Err, errCancelled) {
			t.Errorf("the waiting Hit after its stop closed = %+v, want Cancelled", a.res)
		}
		if took := a.at.Sub(closedAt); took > time.Second {
			t.Errorf("the waiting Hit returned %v after its stop closed, want at once (within 1 s; its ctx allows 10 s)", took)
		}
	case <-timer.C:
		t.Fatal("the waiting Hit has not returned 5 s after its stop closed")
	}

	if err := rollback(); err != nil {
		t.Fatalf("rolling back the held key: %v", err)
	}
	select {
	case got := <-first:
		if got != (Result{ArrivalNs: 1_000, Class: Committed}) {
			t.Errorf("the blocked Hit after the rollback = %+v, want a new row", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked Hit has not returned 10 s after the rollback")
	}
	if n := countKey(t, holder, "wait-stop", "r1"); n != 0 {
		t.Errorf("%d rows for (wait-stop, r1), want 0: nothing may be sent", n)
	}
}

// lockWaiters returns how many sessions of the database wait for a lock.
func lockWaiters(t *testing.T, c *Client) int64 {
	t.Helper()
	n, err := c.LockWaiters(t.Context())
	if err != nil {
		t.Fatalf("LockWaiters: %v", err)
	}
	return n
}

// waitUntil polls cond every 10 ms until it holds or d has passed, and reports whether it held.
func waitUntil(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// TestDBClient_Helpers: Truncate, CountRows, CountKey, HoldKey and Settings, which the
// harness uses where it may not import pgx.
func TestDBClient_Helpers(t *testing.T) {
	c := openClient(t)
	ctx := t.Context()
	if n, err := c.CountRows(ctx); err != nil || n != 0 {
		t.Fatalf("CountRows after Truncate = %d, %v; want 0", n, err)
	}
	rollback := hold(t, c, "help", "r1")
	if n := countKey(t, c, "help", "r1"); n != 0 {
		t.Errorf("CountKey sees the held, uncommitted row: %d", n)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := rollback(); err != nil {
		t.Errorf("a second rollback call: %v", err)
	}
	if got := c.Hit(ctx, "help", "r1", 5_000, nil); got != (Result{ArrivalNs: 5_000, Class: Committed}) {
		t.Errorf("Hit after the rollback = %+v, want a new row", got)
	}
	if n, err := c.CountRows(ctx); err != nil || n != 1 {
		t.Errorf("CountRows = %d, %v; want 1", n, err)
	}
	if ns, found, err := c.RowArrival(ctx, "help", "r1"); err != nil || !found || ns != 5_000 {
		t.Errorf("RowArrival(help, r1) = %d, %v, %v; want 5000, true", ns, found, err)
	}
	if ns, found, err := c.RowArrival(ctx, "help", "r2"); err != nil || found || ns != 0 {
		t.Errorf("RowArrival(help, r2) = %d, %v, %v; want 0, false: no row holds the key", ns, found, err)
	}
	// No key is held now; a statement another test abandoned may still be finishing.
	if !waitUntil(5*time.Second, func() bool { return lockWaiters(t, c) == 0 }) {
		t.Errorf("LockWaiters with no held key = %d after 5 s, want 0", lockWaiters(t, c))
	}
	if rb, err := c.HoldKey(ctx, "help", "r1"); err == nil {
		// End its transaction first: left open, it would block Close and every TRUNCATE.
		if rerr := rb(); rerr != nil {
			t.Errorf("rolling back the wrongly held key: %v", rerr)
		}
		t.Error("HoldKey of a committed key succeeded, want a unique violation")
	}

	// The DESIGN §C8 cluster settings, and the DSN's statement_timeout.
	got, err := c.Settings(ctx, "statement_timeout", "fsync", "synchronous_commit", "max_connections")
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	want := map[string]string{"statement_timeout": "2500ms", "fsync": "on", "synchronous_commit": "on", "max_connections": "300"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("setting %s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := c.Settings(ctx, "no_such_setting"); err == nil {
		t.Error("Settings of an unknown name succeeded")
	}
}
