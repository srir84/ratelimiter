// Package runlog writes the harness run logs (DESIGN §B3 "Run-log writer", §B8): callers
// hand JSON records to a Writer through a fixed-capacity channel, and one goroutine issues
// one write(2) per line on a file opened with O_APPEND, with no userspace buffer, so every
// line handed to the kernel survives SIGKILL. Enqueue never blocks: a full channel counts a
// drop, and any drop invalidates the run (§B8). Shared by hitapi (H1), hitgen (H2) and the
// sampler (H3). It also defines pids.json, the run directory's process list that the
// orchestrator writes and the sampler reads (pids.go; IMPLEMENTATION_PLAN H3 steps 4 and 7),
// and the command lines that start a run's store and instances, which the orchestrator starts
// them with and faultctl restarts them with (store.go; plan H4 step 3).
package runlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
)

// Capacity is the channel's size in lines (DESIGN §B3).
const Capacity = 100_000

// Writer is one run-log file and the goroutine that writes it. Close stops the goroutine
// after it has written every line already on the channel.
type Writer struct {
	dst  io.WriteCloser
	ch   chan []byte
	done chan struct{} // closed when the goroutine has returned

	mu        sync.Mutex // guards closed, encodeErr and accepted, and orders every send on ch before close(ch)
	closed    bool
	encodeErr error // the first record that failed to encode
	accepted  int64 // lines put on the channel

	drops atomic.Int64 // lines that never reached the file

	// progress guards the goroutine's account of the lines it has handled, in channel order,
	// and the drains waiting on it. Drain takes it while holding mu.
	progress sync.Mutex
	handled  int64 // lines whose write returned, written or not
	failed   int64 // of those, the lines whose write failed
	writeErr error // the first failed write
	// drains holds the Drain calls in progress whose lines are not all handled yet: one waiter
	// per caller blocked in Drain (prekill, Stop), not queued work.
	drains []*drainWait
}

// drainWait is one Drain call's lines: the first target lines put on the channel. Once the
// goroutine has handled them all it sets failed, how many of them failed to write, and closes
// done.
type drainWait struct {
	target int64
	failed int64
	done   chan struct{}
}

// Open opens (creating it, mode 0600) the run log at path for appending and starts its
// writer goroutine with a channel of Capacity lines.
func Open(path string) (*Writer, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	return newWriter(f, Capacity), nil
}

// openFile opens path write-only with O_APPEND: each write(2) lands at the end of the file,
// after any line another process appended (DESIGN §B6 pre_kill lines).
func openFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the run log: %w", err)
	}
	return f, nil
}

// Append writes rec as one JSON line to the run log at path, with one Write on a descriptor
// of its own opened with O_APPEND, so one write(2): the line lands whole at the end of the
// file, after the lines the log's own Writer handed over, as faultctl's pre_kill line does on
// a live hitapi's log (DESIGN §B6 FLT-6; IMPLEMENTATION_PLAN H4 step 2). The log must exist
// (plan choice): a line meant for a log its process opened never starts a new file. A record
// that fails to encode writes nothing.
func Append(path string, rec any) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding a run-log record: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("opening the run log: %w", err)
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("writing the run log: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("closing the run log: %w", cerr)
	}
	return nil
}

// newWriter starts the goroutine that writes dst from a channel of capacity lines.
func newWriter(dst io.WriteCloser, capacity int) *Writer {
	w := &Writer{dst: dst, ch: make(chan []byte, capacity), done: make(chan struct{})}
	go w.run()
	return w
}

// run writes each line with one Write call, which an *os.File turns into one write(2) on
// the unbuffered descriptor. A line whose write fails is lost, and counts as a drop.
func (w *Writer) run() {
	defer close(w.done)
	for line := range w.ch {
		_, err := w.dst.Write(line)
		w.handledOne(err)
	}
}

// handledOne counts the line just handled, as a failed write and a drop when err is not nil,
// and finishes each drain whose last line it was.
func (w *Writer) handledOne(err error) {
	w.progress.Lock()
	defer w.progress.Unlock()
	w.handled++
	if err != nil {
		w.drops.Add(1) // before any drain it finishes returns, so Drops includes it by then
		w.failed++
		if w.writeErr == nil {
			w.writeErr = fmt.Errorf("writing the run log: %w", err)
		}
	}
	waiting := w.drains[:0]
	for _, d := range w.drains {
		if d.target > w.handled {
			waiting = append(waiting, d)
			continue
		}
		d.failed = w.failed // the failed lines so far all come before this drain's last one
		close(d.done)
	}
	clear(w.drains[len(waiting):])
	w.drains = waiting
}

// Enqueue encodes rec as one JSON line and hands it to the writer goroutine. It never
// blocks: when the channel is full, or the Writer is closed, the line is dropped and
// counted. A record that fails to encode is dropped and counted too, and Close reports the
// first such error.
func (w *Writer) Enqueue(rec any) {
	line, err := json.Marshal(rec)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.drops.Add(1)
		if w.encodeErr == nil {
			w.encodeErr = fmt.Errorf("encoding a run-log record: %w", err)
		}
		return
	}
	if w.closed {
		w.drops.Add(1)
		return
	}
	select {
	case w.ch <- append(line, '\n'):
		w.accepted++
	default:
		w.drops.Add(1)
	}
}

// Drops returns how many lines never reached the file: refused because the channel was full
// or the Writer closed, not encodable, or lost to a failed write.
func (w *Writer) Drops() int64 { return w.drops.Load() }

// Len returns how many lines wait on the channel (/health log_queue_len).
func (w *Writer) Len() int { return len(w.ch) }

// Drain waits until every line accepted before the call has been handed to the kernel (its
// write returned), or ctx ends, and reports on those lines only: lines enqueued during the
// wait may still be pending, and a failed write of theirs does not count. It returns 0 and
// nil once they have all been written. Otherwise it returns how many of them are not on the
// file, with an error: the lines whose write failed (each also a drop), with the first write
// error, and, when ctx ends first, those not handed over yet (the line being written
// included), with ctx's error.
func (w *Writer) Drain(ctx context.Context) (int64, error) {
	d := w.snapshot()
	select {
	case <-d.done: // every one of its lines was handled already: ctx plays no part
		return w.drainResult(0, d.failed, nil)
	default:
	}
	select {
	case <-d.done:
		return w.drainResult(0, d.failed, nil)
	case <-ctx.Done():
		pending, failed := w.abandon(d)
		return w.drainResult(pending, failed, ctx.Err())
	}
}

// snapshot registers a drain of the lines put on the channel so far. Holding mu, no Enqueue
// is between its send and its count, so the goroutine has handled none past them: when it
// has handled them all, failed is exact for them and done is closed at once; otherwise the
// goroutine finishes the drain when it handles the last of them.
func (w *Writer) snapshot() *drainWait {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.progress.Lock()
	defer w.progress.Unlock()
	d := &drainWait{target: w.accepted, done: make(chan struct{})}
	if w.handled >= d.target {
		d.failed = w.failed
		close(d.done)
		return d
	}
	w.drains = append(w.drains, d)
	return d
}

// abandon unregisters d once its ctx has ended and returns how many of its lines the
// goroutine has not handled yet and how many failed to write. Every failed write so far is of
// one of its lines, since the goroutine has not reached its last one. A drain the goroutine
// finished meanwhile has only its failed lines left.
func (w *Writer) abandon(d *drainWait) (pending, failed int64) {
	w.progress.Lock()
	defer w.progress.Unlock()
	select {
	case <-d.done:
		return 0, d.failed
	default:
	}
	if i := slices.Index(w.drains, d); i >= 0 {
		w.drains = slices.Delete(w.drains, i, i+1)
	}
	return d.target - w.handled, w.failed
}

// drainResult is Drain's answer for its lines: pending of them not handed over yet when ctx
// ended (ctxErr), and failed of them lost to a failed write.
func (w *Writer) drainResult(pending, failed int64, ctxErr error) (int64, error) {
	left := pending + failed
	switch {
	case left == 0:
		return 0, nil
	case failed == 0:
		return left, fmt.Errorf("draining the run log: %w", ctxErr)
	}
	w.progress.Lock()
	writeErr := w.writeErr
	w.progress.Unlock()
	if pending == 0 {
		return left, fmt.Errorf("draining the run log: %d of its lines failed to write: %w", failed, writeErr)
	}
	return left, fmt.Errorf("draining the run log: %w, and %d of its lines failed to write: %w", ctxErr, failed, writeErr)
}

// Close refuses further lines, waits until the goroutine has written every line already on
// the channel, then closes the file. It returns the first encode and write errors and the
// close error; a second call does nothing and returns nil.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.ch)
	encodeErr := w.encodeErr
	w.mu.Unlock()
	<-w.done
	var closeErr error
	if err := w.dst.Close(); err != nil {
		closeErr = fmt.Errorf("closing the run log: %w", err)
	}
	w.progress.Lock()
	writeErr := w.writeErr
	w.progress.Unlock()
	return errors.Join(encodeErr, writeErr, closeErr)
}
