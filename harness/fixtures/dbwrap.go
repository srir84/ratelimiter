//go:build rltest

package fixtures

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"ratelimiter/harness/dbclient"
)

// DBMode is a database fault (DESIGN §B6 FLT-5, FLT-9; the names are the /_test/faults modes
// of target "db", IMPLEMENTATION_PLAN H1 step 11).
type DBMode string

// The database fault modes.
const (
	// FailBefore fails the operation before its insert, so nothing is sent and there is no
	// row (FLT-5): dbclient.FailedNoRow, which hitapi answers with release() and 503 ERR-14.
	FailBefore DBMode = "fail_before"
	// FailAfterCommit runs the operation, then fails it once it committed (FLT-5): a confirmed
	// failure after which the row remains, dbclient.FailedRowMayRemain, which hitapi answers
	// with release() and 500 ERR-13.
	FailAfterCommit DBMode = "fail_after_commit"
	// LoseCommitReply runs the operation, then loses its reply once it committed (FLT-9):
	// dbclient.Unknown, which hitapi answers with done() and 503 ERR-16.
	LoseCommitReply DBMode = "lose_commit_reply"
)

// The errors the database faults report: fixed texts, compared with errors.Is.
var (
	errFailBefore      = errors.New("fixtures: database fault fail_before: failed before the insert, nothing sent")
	errFailAfterCommit = errors.New("fixtures: database fault fail_after_commit: failed after the commit")
	errLostCommitReply = errors.New("fixtures: database fault lose_commit_reply: the commit's reply was lost")
)

// HitDB is the database operation DBWrap stands in front of (dbclient.Client.Hit): stop is
// the caller's cancel signal for work not yet at its first statement.
type HitDB interface {
	Hit(ctx context.Context, userID, requestID string, arrivalNs int64, stop <-chan struct{}) dbclient.Result
}

// maxDBFaults is how many armed database faults may wait at once (plan choice; AGENTS.md §1
// Queues): Arm refuses another until an operation has used up the oldest.
const maxDBFaults = 64

// dbFault is one armed database fault and the operations left to it.
type dbFault struct {
	mode DBMode
	left int
}

// DBWrap stands between hitapi and its database client (DESIGN §B6) and applies the armed
// faults to the next database operations: each operation takes the oldest armed fault that
// has operations left. At most maxDBFaults faults wait at once. An operation with no fault
// armed, and every operation a fault runs through to the database, passes the caller's stop
// channel on, so dbclient's cancellation before the first statement (SIGTERM, ERR-15) works as
// it does without the wrapper. It is safe for concurrent use.
type DBWrap struct {
	inner HitDB

	mu     sync.Mutex
	faults []*dbFault // oldest first; at most maxDBFaults
}

// NewDBWrap returns a wrapper around inner with no fault armed.
func NewDBWrap(inner HitDB) *DBWrap {
	return &DBWrap{inner: inner}
}

// Arm applies mode to the next count database operations, after the faults already armed.
// It fails on an unknown mode, on a count below 1, and when maxDBFaults faults are armed
// already; a refused fault arms nothing.
func (w *DBWrap) Arm(mode DBMode, count int) error {
	switch mode {
	case FailBefore, FailAfterCommit, LoseCommitReply:
	default:
		return fmt.Errorf("fixtures: unknown database fault mode %q", mode)
	}
	if count < 1 {
		return fmt.Errorf("fixtures: database fault %q: count %d, want at least 1", mode, count)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.faults) >= maxDBFaults {
		return fmt.Errorf("fixtures: database fault %q refused: %d faults are armed already, the most that may wait", mode, maxDBFaults)
	}
	w.faults = append(w.faults, &dbFault{mode: mode, left: count})
	return nil
}

// Armed returns how many operations the armed faults still apply to.
func (w *DBWrap) Armed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, f := range w.faults {
		n += f.left
	}
	return n
}

// Disarm removes every armed database fault (hitapi's DELETE /_test/faults; IMPLEMENTATION_PLAN
// H4 step 4): the next operations run straight through.
func (w *DBWrap) Disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.faults = nil
}

// take uses up one operation of the oldest armed fault and returns its mode; ok is false
// when no fault is armed.
func (w *DBWrap) take() (mode DBMode, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.faults) == 0 {
		return "", false
	}
	f := w.faults[0]
	if f.left--; f.left == 0 {
		w.faults = w.faults[1:]
	}
	return f.mode, true
}

// Hit runs one database operation through the oldest armed fault, or straight through when
// none is armed. FailBefore never calls the database; FailAfterCommit and LoseCommitReply run
// the operation and change only a committed outcome, so a real failure (or a cancellation)
// comes back as it is.
func (w *DBWrap) Hit(ctx context.Context, userID, requestID string, arrivalNs int64, stop <-chan struct{}) dbclient.Result {
	mode, ok := w.take()
	if !ok {
		return w.inner.Hit(ctx, userID, requestID, arrivalNs, stop)
	}
	if mode == FailBefore {
		return dbclient.Result{Class: dbclient.FailedNoRow, Err: errFailBefore}
	}
	r := w.inner.Hit(ctx, userID, requestID, arrivalNs, stop)
	if r.Class != dbclient.Committed {
		return r
	}
	if mode == FailAfterCommit {
		return dbclient.Result{Class: dbclient.FailedRowMayRemain, Err: errFailAfterCommit}
	}
	return dbclient.Result{Class: dbclient.Unknown, Err: errLostCommitReply}
}
