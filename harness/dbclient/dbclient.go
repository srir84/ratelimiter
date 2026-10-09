// Package dbclient is the harness's only PostgreSQL client (AGENTS.md §1 "Database"; DESIGN
// §B3): hitapi's insert-or-read of one operation's row, classified by what is known about the
// row afterwards, plus the table and settings helpers the harness and its tests need, so no
// other package imports pgx or holds SQL. Nothing here retries a statement (AGENTS.md §6):
// the caller maps the class to done() or release().
package dbclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The two statements of one /hit (DESIGN §B3), each its own implicit transaction. ON CONFLICT
// waits for a concurrent uncommitted inserter of the same key, so the SELECT that follows an
// empty RETURNING sees the committed row.
const (
	insertSQL = `INSERT INTO hits (user_id, request_id, arrival_ns) VALUES ($1, $2, $3)
ON CONFLICT (user_id, request_id) DO NOTHING RETURNING arrival_ns`
	selectSQL = `SELECT arrival_ns FROM hits WHERE user_id = $1 AND request_id = $2`
)

// Helper statements (tests, the orchestrator's db_rows.json and context.json).
const (
	truncateSQL = `TRUNCATE hits`
	countSQL    = `SELECT count(*) FROM hits`
	countKeySQL = `SELECT count(*) FROM hits WHERE user_id = $1 AND request_id = $2`
	holdSQL     = `INSERT INTO hits (user_id, request_id, arrival_ns) VALUES ($1, $2, 0)`
	settingSQL  = `SELECT current_setting($1)`
	// Sessions of this database waiting for a lock: an insert behind a held key (HoldKey).
	lockWaitersSQL = `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`
)

// rollbackTimeout bounds HoldKey's rollback, which runs even after the caller's ctx ended.
const rollbackTimeout = 10 * time.Second

// Open's refusals (CheckDSN). Neither quotes the DSN: it could hold a secret, and pgconn's own
// parse error quotes it, redacting a userinfo or key=value password but not a ?password=
// parameter.
var (
	// ErrDSNPassword: the harness database takes no password (the §B2 db_dsn has none; make
	// setup's cluster trusts loopback), and one in the config file would be a secret in TOML
	// (AGENTS.md §1).
	ErrDSNPassword = errors.New("the database DSN carries a password; the harness database takes none, so remove it")
	// ErrDSNParse: pgxpool cannot parse the DSN.
	ErrDSNParse = errors.New("the database DSN does not parse (its text is left out: it could hold a secret)")
)

// errCancelled is a Cancelled Hit's error: the caller's stop channel closed before the first
// statement was sent.
var errCancelled = errors.New("the work was cancelled before its first statement was sent")

// Class is what a Hit's outcome says about the row (DESIGN §B3, DB outcome classification).
// The zero value is Unknown, the class that never refunds.
type Class int

const (
	// Unknown: a timeout or connection loss after a statement was sent, or a server PANIC,
	// so the row may or may not exist. The caller calls done() and answers 503 ERR-16.
	Unknown Class = iota
	// Committed: the row was inserted, or the existing row was read. done(), 200.
	Committed
	// FailedNoRow: nothing was sent (pgconn.SafeToRetry, or no connection), or the server
	// failed the statement with a SQLSTATE and rolled it back (the statement_timeout too).
	// release(), 503 ERR-14.
	FailedNoRow
	// FailedRowMayRemain: a confirmed failure after which the row may remain. Only the
	// rltest DB fixture's "fail after commit" reports it; Hit never does. release(), 500 ERR-13.
	FailedRowMayRemain
	// Cancelled: the caller's stop channel (Hit's stop) closed before the first statement was
	// sent, while Hit waited for a pool connection or once it held one, so nothing was sent and
	// there is no row (hitapi: SIGTERM began, HAR-15). release(), 503 ERR-15. A ctx that ends
	// before a statement is sent is FailedNoRow, not Cancelled.
	Cancelled
)

// String returns the class's log name.
func (c Class) String() string {
	switch c {
	case Unknown:
		return "unknown"
	case Committed:
		return "committed"
	case FailedNoRow:
		return "failed_no_row"
	case FailedRowMayRemain:
		return "failed_row_may_remain"
	case Cancelled:
		return "cancelled"
	}
	return fmt.Sprintf("Class(%d)", int(c))
}

// Result is one Hit's outcome. ArrivalNs is the row's arrival_ns and Existed reports that the
// row was already there (both only for Committed); Err is nil only for Committed.
type Result struct {
	ArrivalNs int64
	Existed   bool
	Class     Class
	Err       error
}

// Client is a pool of connections to the harness database.
type Client struct {
	pool *pgxpool.Pool

	// afterAcquire, when not nil, runs once Hit holds its connection, just before its stop
	// check: a hook for this package's tests only, nil everywhere else.
	afterAcquire func()
}

// Open parses dsn (the §B2 db_dsn: pool_* keys configure the pool; others, such as
// statement_timeout, go to the server) and returns a client. It refuses what CheckDSN refuses,
// and its errors never quote the DSN. It connects lazily: an unreachable server shows up as
// FailedNoRow on the first Hit, not as an error here.
func Open(ctx context.Context, dsn string) (*Client, error) {
	cfg, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating the database pool: %w", err)
	}
	return &Client{pool: pool}, nil
}

// CheckDSN returns the error Open would refuse dsn with, without connecting: ErrDSNPassword
// when it carries a password (hasPassword), ErrDSNParse when pgxpool cannot parse it; nil for
// a DSN Open takes. Neither error quotes the DSN. The orchestrator checks a run's db_dsn with
// it before it writes any file that would hold the DSN (IMPLEMENTATION_PLAN H3 step 1).
func CheckDSN(dsn string) error {
	_, err := parseDSN(dsn)
	return err
}

// parseDSN is Open's parse of dsn, after the password check.
func parseDSN(dsn string) (*pgxpool.Config, error) {
	if hasPassword(dsn) {
		return nil, ErrDSNPassword
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgconn's error quotes the DSN, so none of it is passed on.
		return nil, ErrDSNParse
	}
	return cfg, nil
}

// hasPassword reports whether dsn names a password in a form pgconn reads: the URL userinfo
// (postgres://user:pw@host), a URL query parameter (?password=, its name percent-encoded or
// not), or a key=value pair (password=pw). It errs toward refusing: any mention of
// "password", in any case, counts.
func hasPassword(dsn string) bool {
	if strings.Contains(strings.ToLower(dsn), "password") {
		return true
	}
	u, err := url.Parse(dsn)
	if err != nil {
		// A key=value DSN names a password only as password=, caught above; pgconn refuses a
		// postgres:// URL that does not parse, and Open's parse error quotes none of it.
		return false
	}
	if _, set := u.User.Password(); set {
		return true
	}
	for name := range u.Query() {
		if strings.EqualFold(name, "password") {
			return true
		}
	}
	return false
}

// Close closes every connection; it waits for acquired connections to be released.
func (c *Client) Close() { c.pool.Close() }

// Hit inserts the row (userID, requestID, arrivalNs), or reads the row that already holds
// the key, and classifies the outcome exactly per the DESIGN §B3 table. It takes a pool
// connection first: when that fails, no statement was sent, so the class is FailedNoRow.
// stop, when not nil, is the caller's cancel signal for work not yet at its first statement
// (hitapi closes it when SIGTERM begins, HAR-15): once it is closed, a Hit still waiting for
// a connection stops waiting at once, and a Hit that holds one sees it in its last check just
// before the first statement; either way Hit sends nothing, gives back any connection it took
// and returns Cancelled. ctx bounds the whole call (hitapi passes its P deadline); pgx's
// default context handling interrupts a statement in flight, which leaves the outcome Unknown.
func (c *Client) Hit(ctx context.Context, userID, requestID string, arrivalNs int64, stop <-chan struct{}) Result {
	conn, err := c.acquire(ctx, stop)
	switch {
	case errors.Is(err, errCancelled):
		return Result{Class: Cancelled, Err: err}
	case err != nil:
		return Result{Class: FailedNoRow, Err: fmt.Errorf("taking a database connection: %w", err)}
	}
	defer conn.Release()
	if c.afterAcquire != nil {
		c.afterAcquire()
	}
	// The last check before the first statement: stop may have closed after the connection
	// was taken.
	if closed(stop) {
		return Result{Class: Cancelled, Err: errCancelled}
	}

	var got int64
	err = conn.QueryRow(ctx, insertSQL, userID, requestID, arrivalNs).Scan(&got)
	if err == nil {
		return Result{ArrivalNs: got, Class: Committed}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return failed(fmt.Errorf("inserting the row: %w", err))
	}
	err = conn.QueryRow(ctx, selectSQL, userID, requestID).Scan(&got)
	switch {
	case err == nil:
		return Result{ArrivalNs: got, Existed: true, Class: Committed}
	case errors.Is(err, pgx.ErrNoRows):
		// The insert found the key taken, yet the row is gone (a TRUNCATE in between):
		// both statements completed and neither wrote a row.
		return Result{Class: FailedNoRow, Err: errors.New("reading the existing row: the key's row vanished")}
	default:
		return failed(fmt.Errorf("reading the existing row: %w", err))
	}
}

// acquire takes a pool connection within ctx. With a stop channel, the wait runs under a
// child of ctx that a watcher cancels with errCancelled once stop closes, so a Hit waiting for
// a connection ends at once when the caller cancels its work; acquire then returns
// errCancelled. The watcher ends before acquire returns.
func (c *Client) acquire(ctx context.Context, stop <-chan struct{}) (*pgxpool.Conn, error) {
	if stop == nil {
		return c.pool.Acquire(ctx)
	}
	actx, cancel := context.WithCancelCause(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-stop:
			cancel(errCancelled)
		case <-actx.Done():
		}
	}()
	conn, err := c.pool.Acquire(actx)
	cancel(nil) // ends the watcher when stop has not closed; a taken connection outlives actx
	<-watched
	if err != nil && errors.Is(context.Cause(actx), errCancelled) {
		return nil, errCancelled
	}
	return conn, err
}

// closed reports whether stop is closed; a nil stop never is.
func closed(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// failed classifies a statement's error (DESIGN §B3): nothing sent, or a server error with a
// SQLSTATE (the statement failed and rolled back), is FailedNoRow; anything else after send
// (a timeout, a lost connection), or a PANIC, is Unknown.
func failed(err error) Result {
	if pgconn.SafeToRetry(err) {
		return Result{Class: FailedNoRow, Err: err}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && !serverPanicked(pgErr) {
		return Result{Class: FailedNoRow, Err: err}
	}
	return Result{Class: Unknown, Err: err}
}

// serverPanicked reports a server error of severity PANIC: the server crashed, perhaps inside
// the commit after its record reached the disk (a failed WAL flush there is reported after the
// INSERT's CommandComplete), so crash recovery decides whether the row exists. Severity may be
// translated (lc_messages); SeverityUnlocalized never is, but servers before PostgreSQL 9.6
// do not send it.
func serverPanicked(e *pgconn.PgError) bool {
	if e.SeverityUnlocalized != "" {
		return e.SeverityUnlocalized == "PANIC"
	}
	return e.Severity == "PANIC"
}

// Truncate empties the hits table.
func (c *Client) Truncate(ctx context.Context) error {
	if _, err := c.pool.Exec(ctx, truncateSQL); err != nil {
		return fmt.Errorf("truncating hits: %w", err)
	}
	return nil
}

// CountRows returns the number of rows in hits (db_rows.json).
func (c *Client) CountRows(ctx context.Context) (int64, error) {
	var n int64
	if err := c.pool.QueryRow(ctx, countSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting rows: %w", err)
	}
	return n, nil
}

// CountKey returns the number of rows holding (userID, requestID): 0 or 1.
func (c *Client) CountKey(ctx context.Context, userID, requestID string) (int64, error) {
	var n int64
	if err := c.pool.QueryRow(ctx, countKeySQL, userID, requestID).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting the key's rows: %w", err)
	}
	return n, nil
}

// HoldKey inserts (userID, requestID) in a transaction it leaves open, so another insert of
// the key waits for it (tests that block a request on the database). rollback ends the
// transaction, writing nothing, and returns its connection to the pool; it runs on its own
// deadline even after ctx has ended, and later calls return the first call's result.
func (c *Client) HoldKey(ctx context.Context, userID, requestID string) (rollback func() error, err error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("holding the key: beginning: %w", err)
	}
	end := func() error {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if err := tx.Rollback(rctx); err != nil {
			return fmt.Errorf("releasing the held key: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, holdSQL, userID, requestID); err != nil {
		return nil, errors.Join(fmt.Errorf("holding the key: inserting: %w", err), end())
	}
	return sync.OnceValue(end), nil
}

// RowArrival returns the arrival_ns of the row holding (userID, requestID); found is false
// when no row holds the key.
func (c *Client) RowArrival(ctx context.Context, userID, requestID string) (arrivalNs int64, found bool, err error) {
	err = c.pool.QueryRow(ctx, selectSQL, userID, requestID).Scan(&arrivalNs)
	switch {
	case err == nil:
		return arrivalNs, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("reading the key's row: %w", err)
	}
}

// LockWaiters returns how many sessions of the database wait for a lock, such as inserts
// behind a key that HoldKey holds: how a test sees that its requests reached the database.
func (c *Client) LockWaiters(ctx context.Context) (int64, error) {
	var n int64
	if err := c.pool.QueryRow(ctx, lockWaitersSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting lock waiters: %w", err)
	}
	return n, nil
}

// Settings returns the server's current value of each named setting, as SHOW reports it
// (current_setting is SHOW's documented equivalent, and takes the name as a bound parameter).
func (c *Client) Settings(ctx context.Context, names ...string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		var v string
		if err := c.pool.QueryRow(ctx, settingSQL, name).Scan(&v); err != nil {
			return nil, fmt.Errorf("reading setting %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}
