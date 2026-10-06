package store

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"ratelimiter/internal/store/resp"
)

// connError is a constant error of the connection layer, matched with errors.Is.
type connError string

func (e connError) Error() string { return string(e) }

const (
	errAdapterClosed connError = "store adapter closed"
	errFailFast      connError = "no ready store connection, and the last dial failed within its backoff"
	errConnLost      connError = "store connection closed before the reply"
	errDeadline      connError = "call deadline passed after the command was sent"
	// errDependencyDeadline is the cause (context.Cause) of a call context that its own
	// dependency deadline, start + D, ended (SPEC API-1).
	errDependencyDeadline connError = "the dependency deadline passed"
)

// result is what a written command got: its reply, or the error that ended the wait.
type result struct {
	v   resp.Value
	err error
}

// call is one command in a connection's FIFO.
type call struct {
	label      string      // send-observer label (§A5.12)
	start, end int         // the command's byte range in its write
	writtenAt  time.Time   // when it joined the FIFO, right before its bytes were written
	res        chan result // capacity 1: the reader or close delivers exactly one result
	abandoned  atomic.Bool // its caller's deadline passed: the reply is read and discarded
}

// pipeConn is one pipelined store connection (DESIGN §A5.4). Writers hold the write lock
// from the FIFO append through the write, so the FIFO order is the byte order on the wire
// and a fast reply always finds its call; one reader goroutine matches replies to the FIFO
// in order. The connection is written directly, without a buffer, so byte counts are exact.
type pipeConn struct {
	conn    net.Conn
	r       *resp.Reader
	d       time.Duration           // dependency deadline D: the stall limit of the FIFO head
	obs     func(cmd string, n int) // send observer, may be nil
	onClose func(*pipeConn)         // takes it out of its set's ready list

	wlock chan struct{} // the write lock: capacity 1, so waiting for it can be abandoned
	load  atomic.Int64  // commands reserved by pick or in the FIFO, for fewest-in-flight

	mu     sync.Mutex
	fifo   []*call // in flight, oldest first
	closed bool

	done       chan struct{} // closed when the connection closes
	readerDone chan struct{} // closed when readLoop returns
}

func newPipeConn(c net.Conn, d time.Duration, obs func(string, int), onClose func(*pipeConn)) *pipeConn {
	return &pipeConn{
		conn:       c,
		r:          resp.NewReader(c),
		d:          d,
		obs:        obs,
		onClose:    onClose,
		wlock:      make(chan struct{}, 1),
		done:       make(chan struct{}),
		readerDone: make(chan struct{}),
	}
}

// lockWrite takes the write lock. It returns errConnLost when the connection closes first,
// so the caller can pick another (a call that never wrote may move), or ctx's cause
// (context.Cause), which tells the dependency deadline from the host's end.
func (pc *pipeConn) lockWrite(ctx context.Context) error {
	select {
	case pc.wlock <- struct{}{}:
		return nil
	case <-pc.done:
		return errConnLost
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (pc *pipeConn) unlockWrite() { <-pc.wlock }

// write appends calls to the FIFO and then writes buf, their concatenated commands, in one
// Write whose deadline is the batch's, ctx's deadline (plan choice: the earliest call
// deadline); the caller holds the write lock. It returns the bytes written and the write
// error: command i left when calls[i].start < n. closed reports that the connection had
// already closed, so nothing was appended or written and the caller may pick another. The
// observer hears once per command how many of its bytes were written. After an error, the
// commands whose bytes never left are taken back out of the FIFO, and an error after n > 0
// bytes closes the connection.
//
// When ctx ends during the Write, the host's cancellation included, the Write is cut short
// at once (interruptibleWrite), so a write blocked on a store that stopped reading neither
// outlasts the call nor sends more of its bytes after the call ended (SPEC API-1); n then
// says what left, as for any failed write.
func (pc *pipeConn) write(ctx context.Context, calls []*call, buf []byte) (n int, closed bool, err error) {
	deadline, _ := ctx.Deadline()
	pc.mu.Lock()
	if pc.closed {
		pc.mu.Unlock()
		return 0, true, nil
	}
	if err := pc.conn.SetWriteDeadline(deadline); err != nil {
		pc.mu.Unlock()
		pc.load.Add(int64(-len(calls))) // nothing joined the FIFO
		return 0, false, fmt.Errorf("setting the write deadline: %w", err)
	}
	now := time.Now()
	if len(pc.fifo) == 0 {
		pc.setHeadDeadline(now) // the head changes
	}
	for _, c := range calls {
		c.writtenAt = now
		pc.fifo = append(pc.fifo, c)
	}
	pc.mu.Unlock()

	n, err = pc.interruptibleWrite(ctx, buf)
	if pc.obs != nil {
		for _, c := range calls {
			pc.obs(c.label, min(max(n-c.start, 0), c.end-c.start))
		}
	}
	if err == nil {
		return n, false, nil
	}
	pc.unappend(calls, n)
	if n > 0 {
		pc.close()
	}
	return n, false, fmt.Errorf("writing to the store: %w", err)
}

// interruptibleWrite writes buf, and moves the write deadline to now as soon as ctx ends, which
// ends a Write blocked on a full socket buffer at once with the bytes it had written. The
// move cannot reach a later write: when ctx ended, the interrupt is waited for before
// returning, while the caller still holds the write lock, and every write sets its own
// deadline first.
func (pc *pipeConn) interruptibleWrite(ctx context.Context, buf []byte) (int, error) {
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		// A failure means the connection is broken, so the Write fails anyway.
		_ = pc.conn.SetWriteDeadline(time.Now())
	})
	n, err := pc.conn.Write(buf)
	if !stop() {
		<-interrupted // ctx ended: the interrupt has started; let it finish
	}
	return n, err
}

// unappend takes the calls whose bytes never left (start ≥ n) back out of the FIFO tail,
// where the write lock kept them; a call close has already drained is left alone.
func (pc *pipeConn) unappend(calls []*call, n int) {
	pc.mu.Lock()
	removed := 0
	for i := len(calls) - 1; i >= 0 && calls[i].start >= n; i-- {
		last := len(pc.fifo) - 1
		if last < 0 || pc.fifo[last] != calls[i] {
			break
		}
		pc.fifo[last] = nil
		pc.fifo = pc.fifo[:last]
		removed++
	}
	if removed > 0 && len(pc.fifo) == 0 && !pc.closed {
		pc.setHeadDeadline(time.Time{})
	}
	pc.mu.Unlock()
	pc.load.Add(int64(-removed))
}

// setHeadDeadline applies the stall rule after the FIFO head changed (DESIGN §A5.4): the
// read deadline is the new head's write time + D, or none when the FIFO is empty; writtenAt
// is the head's write time (zero for an empty FIFO). Called with mu held.
func (pc *pipeConn) setHeadDeadline(writtenAt time.Time) {
	var dl time.Time
	if !writtenAt.IsZero() {
		dl = writtenAt.Add(pc.d)
	}
	// A failure means the connection is broken; the reader then fails and closes it.
	_ = pc.conn.SetReadDeadline(dl)
}

// readLoop matches replies to the FIFO in order until the connection fails. A read timeout
// (the head waited D), EOF, a reset or a protocol error closes the connection. An abandoned
// call's reply is read and dropped, so the replies after it still match (API-1).
func (pc *pipeConn) readLoop() {
	defer close(pc.readerDone)
	for {
		v, err := pc.r.ReadValue()
		if err != nil {
			pc.close()
			return
		}
		pc.mu.Lock()
		if len(pc.fifo) == 0 {
			pc.mu.Unlock()
			pc.close() // a reply with no command in flight: the stream no longer matches
			return
		}
		c := pc.fifo[0]
		pc.fifo[0] = nil
		pc.fifo = pc.fifo[1:]
		if len(pc.fifo) == 0 {
			pc.setHeadDeadline(time.Time{})
		} else {
			pc.setHeadDeadline(pc.fifo[0].writtenAt)
		}
		pc.mu.Unlock()
		pc.load.Add(-1)
		if !c.abandoned.Load() {
			c.res <- result{v: v}
		}
	}
}

// close closes the connection once: it leaves its set's ready list, every call still in
// the FIFO gets errConnLost (its command was sent, so outcome_unknown), and writers waiting
// for the write lock pick another connection. The owning slot then redials.
func (pc *pipeConn) close() {
	pc.mu.Lock()
	if pc.closed {
		pc.mu.Unlock()
		return
	}
	pc.closed = true
	pending := pc.fifo
	pc.fifo = nil
	pc.mu.Unlock()

	pc.onClose(pc)
	_ = pc.conn.Close() // the connection is dropped either way
	close(pc.done)
	pc.load.Add(int64(-len(pending)))
	for _, c := range pending {
		if !c.abandoned.Load() {
			c.res <- result{err: errConnLost}
		}
	}
}

// await waits for c's result until ctx ends. Then c is abandoned: its command stays on the
// connection, and the reader drains its late reply (API-1).
//
// An ended ctx wins over a result already waiting (plan choice): a call whose ctx has ended
// by the time it takes its result ends with errDeadline, so outcome_unknown, and the reply
// is finished internally like one that arrives later (SPEC API-1: once the host's deadline
// or cancellation, or start + D, has ended a call that sent its command, the call is
// outcome_unknown; DESIGN §A5.4: "read reply in time?"). Which of the two came first cannot
// be told once both have, and outcome_unknown is right either way, since the command was
// sent; taking the reply could hand an allow to a host that had already given up.
func await(ctx context.Context, c *call) (resp.Value, error) {
	select {
	case r := <-c.res:
		if ctx.Err() == nil {
			return r.v, r.err
		}
	case <-ctx.Done():
	}
	c.abandoned.Store(true)
	return resp.Value{}, errDeadline
}
