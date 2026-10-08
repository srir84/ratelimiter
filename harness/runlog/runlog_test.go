package runlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testRecord is one test line; Pad makes an interleaved write visible as broken JSON.
type testRecord struct {
	Kind string `json:"kind"`
	G    int    `json:"g"`
	I    int    `json:"i"`
	Pad  string `json:"pad"`
}

// lineChecker passes writes through to a file and records every write that is not exactly
// one JSON line: one object, then the line's only newline.
type lineChecker struct {
	f      *os.File
	writes int
	bad    []string
}

func (c *lineChecker) Write(p []byte) (int, error) {
	c.writes++
	if len(p) == 0 || p[len(p)-1] != '\n' || bytes.Count(p, []byte{'\n'}) != 1 || !json.Valid(p[:len(p)-1]) {
		c.bad = append(c.bad, string(p))
	}
	return c.f.Write(p)
}

func (c *lineChecker) Close() error { return c.f.Close() }

// failingWriter is a destination whose every Write fails with err, like a full disk.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func (failingWriter) Close() error { return nil }

// readLines returns the file's lines, failing the test unless it ends with a newline.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != '\n' {
		t.Fatalf("%s does not end with a newline: ...%q", path, data[max(len(data)-80, 0):])
	}
	return strings.Split(string(data[:len(data)-1]), "\n")
}

// waitFor polls cond until it holds, failing the test after limit.
func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", limit, what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunlog_OneLinePerWrite(t *testing.T) {
	const goroutines, perGoroutine = 8, 1250
	path := filepath.Join(t.TempDir(), "api-1.jsonl")
	// A line already in the file stays first: the writer appends (O_APPEND), never truncates.
	if err := os.WriteFile(path, []byte(`{"kind":"earlier"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openFile(path)
	if err != nil {
		t.Fatal(err)
	}
	check := &lineChecker{f: f}
	w := newWriter(check, Capacity)

	pad := strings.Repeat("x", 200)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perGoroutine {
				w.Enqueue(testRecord{Kind: "test", G: g, I: i, Pad: pad})
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if w.Drops() != 0 {
		t.Errorf("Drops() = %d, want 0", w.Drops())
	}
	if check.writes != goroutines*perGoroutine {
		t.Errorf("%d writes, want one per line: %d", check.writes, goroutines*perGoroutine)
	}
	for _, b := range check.bad[:min(len(check.bad), 3)] {
		t.Errorf("a write that is not exactly one JSON line: %q", b)
	}
	lines := readLines(t, path)
	if len(lines) != 1+goroutines*perGoroutine {
		t.Fatalf("%d lines in the file, want %d", len(lines), 1+goroutines*perGoroutine)
	}
	if lines[0] != `{"kind":"earlier"}` {
		t.Errorf("first line %q, want the line written before Open", lines[0])
	}
	next := make([]int, goroutines) // each goroutine's lines arrive in its own order
	for n, l := range lines[1:] {
		var r testRecord
		dec := json.NewDecoder(strings.NewReader(l))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil || r.Kind != "test" || r.Pad != pad || r.G < 0 || r.G >= goroutines {
			t.Fatalf("line %d is not one whole record (%v): %.120q", n+2, err, l)
		}
		if r.I != next[r.G] {
			t.Fatalf("line %d: goroutine %d's line %d, want its line %d", n+2, r.G, r.I, next[r.G])
		}
		next[r.G]++
	}
}

func TestRunlog_DropCountWhenFull(t *testing.T) {
	const capacity = 10
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w := newWriter(pw, capacity)
	read := make(chan []byte, 2)
	t.Cleanup(func() {
		// Unblock the writer if the test ended early: read the pipe until Close closes it.
		go func() {
			b, _ := io.ReadAll(r)
			read <- b
		}()
		_ = w.Close()
		// Close has closed pw; closing it again ends every reader even when Close did not,
		// so that defect fails the EOF check in the test instead of hanging here.
		_ = pw.Close()
		<-read
		_ = r.Close()
	})

	// A 1 MiB line fills the pipe's buffer, so the goroutine blocks in its write.
	big := testRecord{Kind: "big", Pad: strings.Repeat("b", 1<<20)}
	w.Enqueue(big)
	waitFor(t, 5*time.Second, "the goroutine to take the big line", func() bool { return w.Len() == 0 })
	for i := range capacity {
		w.Enqueue(testRecord{Kind: "kept", I: i})
	}
	if w.Drops() != 0 || w.Len() != capacity {
		t.Fatalf("after %d lines: Drops() = %d, Len() = %d; want 0 and %d", capacity, w.Drops(), w.Len(), capacity)
	}

	// The channel is full and the file blocked: 25 more lines are dropped, without blocking.
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := range 25 {
			w.Enqueue(testRecord{Kind: "dropped", I: i})
		}
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked on a full channel")
	}
	if w.Drops() != 25 || w.Len() != capacity {
		t.Fatalf("Drops() = %d, Len() = %d; want 25 and %d", w.Drops(), w.Len(), capacity)
	}

	// Unblock the file: Close writes the 11 accepted lines, then closes the pipe.
	go func() {
		b, _ := io.ReadAll(r)
		read <- b
	}()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := pw.Write([]byte("after Close\n")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("a write to the destination after Close returned %v, want os.ErrClosed", err)
	}
	var got []byte
	select {
	case got = <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("the pipe did not reach EOF after Close")
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 1+capacity {
		t.Fatalf("%d lines written, want %d", len(lines), 1+capacity)
	}
	for i, l := range lines[1:] {
		if want := fmt.Sprintf(`{"kind":"kept","g":0,"i":%d,"pad":""}`, i); l != want {
			t.Errorf("line %d = %q, want %q", i+2, l, want)
		}
	}
	w.Enqueue(testRecord{Kind: "late"})
	if w.Drops() != 26 {
		t.Errorf("after an Enqueue on a closed Writer Drops() = %d, want 26", w.Drops())
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// pipeWriter returns a Writer on the write end of a pipe that nothing reads yet, so a 1 MiB
// line blocks the goroutine in its write; the read end, for a test to read part of; and
// unblock, which starts reading the pipe to its end. Cleanup unblocks it, closes the Writer
// and waits for the reader.
func pipeWriter(t *testing.T, capacity int) (w *Writer, r *os.File, unblock func()) {
	t.Helper()
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w = newWriter(pw, capacity)
	read := make(chan struct{})
	var once sync.Once
	unblock = func() {
		once.Do(func() {
			go func() {
				defer close(read)
				_, _ = io.Copy(io.Discard, r) // to EOF: the test checks the lines elsewhere
			}()
		})
	}
	t.Cleanup(func() {
		unblock()
		_ = w.Close()
		_ = pw.Close() // ends the reader even if Close did not close pw
		<-read
		_ = r.Close()
	})
	return w, r, unblock
}

// doneSignal is a ctx that closes waiting the first time its Done is called. Drain calls Done
// only once it has taken its snapshot of the lines to wait for and found some unwritten, so a
// line enqueued after waiting closes is one enqueued during the drain.
type doneSignal struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *doneSignal) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// TestRunlog_DrainReportsItsLines: Drain reports on the lines accepted before it began (H1's
// prekill replies its result as log_queue_len). When they have all been handed to the kernel
// it returns 0 and nil, even while lines enqueued during the wait are still pending; when its
// ctx ends first it returns how many of them have not, the line still in its write included
// (which Len, the channel's length, leaves out), with the ctx's error.
func TestRunlog_DrainReportsItsLines(t *testing.T) {
	big := testRecord{Kind: "big", Pad: strings.Repeat("b", 1<<20)}
	shortDrain := func(t *testing.T, w *Writer) (int64, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		return w.Drain(ctx)
	}

	t.Run("lines left when the wait ends", func(t *testing.T) {
		w, _, unblock := pipeWriter(t, 10)
		w.Enqueue(big)
		waitFor(t, 5*time.Second, "the goroutine to take the big line", func() bool { return w.Len() == 0 })
		if left, err := shortDrain(t, w); left != 1 || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Drain with the big line in its write and the channel empty = %d, %v; want 1 and the deadline's error", left, err)
		}
		for i := range 2 {
			w.Enqueue(testRecord{Kind: "queued", I: i})
		}
		if left, err := shortDrain(t, w); left != 3 || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Drain with the big line in its write and 2 lines queued = %d, %v; want 3 and the deadline's error", left, err)
		}
		if n := w.Len(); n != 2 {
			t.Errorf("Len() = %d, want the 2 queued lines", n)
		}
		unblock()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if left, err := w.Drain(ctx); left != 0 || err != nil {
			t.Errorf("Drain once the pipe is read = %d, %v; want 0, nil", left, err)
		}
	})

	t.Run("lines enqueued during the wait", func(t *testing.T) {
		w, r, _ := pipeWriter(t, 10)
		bigLine, err := json.Marshal(big)
		if err != nil {
			t.Fatal(err)
		}
		w.Enqueue(big) // in its write until the test reads it
		waitFor(t, 5*time.Second, "the goroutine to take the big line", func() bool { return w.Len() == 0 })
		w.Enqueue(testRecord{Kind: "before", I: 1})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		sig := &doneSignal{Context: ctx, waiting: make(chan struct{})}
		type result struct {
			left int64
			err  error
		}
		res := make(chan result, 1)
		go func() {
			left, err := w.Drain(sig)
			res <- result{left, err}
		}()
		select {
		case <-sig.waiting:
		case <-time.After(5 * time.Second):
			t.Fatal("Drain did not wait for the 2 lines queued before it")
		}
		// Drain waits for those 2 lines; these 2 come after its snapshot. The second big line
		// will block in its write, so the last line stays on the channel.
		w.Enqueue(big)
		w.Enqueue(testRecord{Kind: "during", I: 3})
		// Read exactly the first big line: the line after it fits the pipe's buffer, and the
		// second big line fills it.
		if _, err := io.ReadFull(r, make([]byte, len(bigLine)+1)); err != nil {
			t.Fatalf("reading the first big line: %v", err)
		}
		select {
		case got := <-res:
			if got.left != 0 || got.err != nil {
				t.Errorf("Drain = %d, %v; want 0, nil: both lines queued before it were written", got.left, got.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Drain has not returned although the lines queued before it were written")
		}
		if n := w.Len(); n < 1 {
			t.Errorf("Len() = %d after Drain returned, want the line enqueued during it still queued", n)
		}
	})
}

// scriptedWriter is a destination that fails the write of each line of kind "lost" with err,
// like a disk that is full for a moment, holds the write of each line of kind "held" until
// release is closed, and writes the others.
type scriptedWriter struct {
	err     error
	release chan struct{}
}

func (s scriptedWriter) Write(p []byte) (int, error) {
	var r testRecord
	if err := json.Unmarshal(p, &r); err != nil {
		return 0, err
	}
	switch r.Kind {
	case "lost":
		return 0, s.err
	case "held":
		<-s.release
	}
	return len(p), nil
}

func (scriptedWriter) Close() error { return nil }

// TestRunlog_DrainCountsFailedWrites: a line of Drain's own whose write failed never reaches
// the file, so Drain counts it among its lines not written and returns an error wrapping the
// write error, and the line is a drop as well (H1's prekill then answers 503 with
// log_queue_len 1, not 200 with 0). The lost line stays among the lines of every later drain.
// A line enqueued after the drain began does not count, even when its write fails. When the
// wait ends first, the count holds both the failed lines and those not yet handed over.
func TestRunlog_DrainCountsFailedWrites(t *testing.T) {
	errDisk := errors.New("no space left on device")
	// scripted returns a Writer on a scriptedWriter and release, which ends the held writes;
	// Cleanup releases them and closes the Writer.
	scripted := func(t *testing.T) (*Writer, func()) {
		t.Helper()
		held := make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(held) }) }
		w := newWriter(scriptedWriter{err: errDisk, release: held}, 10)
		t.Cleanup(func() {
			release()
			_ = w.Close() // the write error, checked through Drain
		})
		return w, release
	}
	drain := func(t *testing.T, w *Writer, limit time.Duration) (int64, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), limit)
		defer cancel()
		return w.Drain(ctx)
	}

	t.Run("a failed write among its lines", func(t *testing.T) {
		w, _ := scripted(t)
		w.Enqueue(testRecord{Kind: "kept", I: 0})
		w.Enqueue(testRecord{Kind: "lost", I: 1})
		w.Enqueue(testRecord{Kind: "kept", I: 2})
		if left, err := drain(t, w, 10*time.Second); left != 1 || !errors.Is(err, errDisk) {
			t.Errorf("Drain with 1 of its 3 lines lost to a failed write = %d, %v; want 1 and the write error", left, err)
		}
		if got := w.Drops(); got != 1 {
			t.Errorf("Drops() = %d, want 1: the lost line", got)
		}
		w.Enqueue(testRecord{Kind: "kept", I: 3})
		if left, err := drain(t, w, 10*time.Second); left != 1 || !errors.Is(err, errDisk) {
			t.Errorf("a later Drain = %d, %v; want 1 and the write error: the lost line is still not on the file", left, err)
		}
	})

	t.Run("a failed write after the drain began", func(t *testing.T) {
		w, release := scripted(t)
		w.Enqueue(testRecord{Kind: "held", I: 0})
		waitFor(t, 5*time.Second, "the goroutine to take the held line", func() bool { return w.Len() == 0 })
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		sig := &doneSignal{Context: ctx, waiting: make(chan struct{})}
		type result struct {
			left int64
			err  error
		}
		res := make(chan result, 1)
		go func() {
			left, err := w.Drain(sig)
			res <- result{left, err}
		}()
		select {
		case <-sig.waiting:
		case <-time.After(5 * time.Second):
			t.Fatal("Drain did not wait for the held line")
		}
		w.Enqueue(testRecord{Kind: "lost", I: 1}) // after the drain's snapshot
		release()
		select {
		case got := <-res:
			if got.left != 0 || got.err != nil {
				t.Errorf("Drain = %d, %v; want 0, nil: its one line was written, and the failed one came after it began", got.left, got.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Drain has not returned although its line was written")
		}
		waitFor(t, 5*time.Second, "the lost line's write to fail", func() bool { return w.Drops() == 1 })
	})

	t.Run("lines left when the wait ends, a failed one included", func(t *testing.T) {
		w, _ := scripted(t)
		w.Enqueue(testRecord{Kind: "lost", I: 0})
		w.Enqueue(testRecord{Kind: "held", I: 1})
		w.Enqueue(testRecord{Kind: "kept", I: 2})
		waitFor(t, 5*time.Second, "the lost line to fail and the held one to be in its write", func() bool {
			return w.Drops() == 1 && w.Len() == 1
		})
		left, err := drain(t, w, 100*time.Millisecond)
		if left != 3 || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errDisk) {
			t.Errorf("Drain with 1 line lost, 1 in its write and 1 queued = %d, %v; want 3 and both the deadline's and the write error", left, err)
		}
	})
}

// TestRunlog_LostLinesAreDrops: a line whose write fails and a record that cannot be encoded
// never reach the file, so each counts as a drop (any drop invalidates the run, DESIGN §B8),
// and Close reports the error.
func TestRunlog_LostLinesAreDrops(t *testing.T) {
	t.Run("failed writes", func(t *testing.T) {
		errDisk := errors.New("no space left on device")
		w := newWriter(failingWriter{err: errDisk}, 10)
		for i := range 3 {
			w.Enqueue(testRecord{Kind: "lost", I: i})
		}
		err := w.Close()
		if got := w.Drops(); got != 3 {
			t.Errorf("Drops() after 3 failed writes = %d, want 3", got)
		}
		if !errors.Is(err, errDisk) {
			t.Errorf("Close = %v, want the write error", err)
		}
	})
	t.Run("unencodable record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "api-1.jsonl")
		w, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		w.Enqueue(testRecord{Kind: "kept", I: 0})
		w.Enqueue(math.NaN())
		if got := w.Drops(); got != 1 {
			t.Errorf("Drops() after an unencodable record = %d, want 1", got)
		}
		w.Enqueue(testRecord{Kind: "kept", I: 1})
		err = w.Close()
		var unsupported *json.UnsupportedValueError
		if !errors.As(err, &unsupported) {
			t.Errorf("Close = %v, want the encode error", err)
		}
		if got := w.Drops(); got != 1 {
			t.Errorf("Drops() after Close = %d, want 1", got)
		}
		want := []string{`{"kind":"kept","g":0,"i":0,"pad":""}`, `{"kind":"kept","g":0,"i":1,"pad":""}`}
		if got := readLines(t, path); !slices.Equal(got, want) {
			t.Errorf("file lines %q, want %q", got, want)
		}
	})
}

// TestRunlog_OpenCapacity: Open's channel holds the 100,000 lines of DESIGN §B3.
func TestRunlog_OpenCapacity(t *testing.T) {
	w, err := Open(filepath.Join(t.TempDir(), "api-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cap(w.ch); got != 100_000 {
		t.Errorf("channel capacity %d lines, want 100000", got)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestRunlog_AppendsAfterAnotherWriter: a line another descriptor appends while the Writer is
// open (H4's faultctl appends pre_kill to a live hitapi's log) stays whole, and the Writer's
// later lines follow it: each write lands at the end of the file (O_APPEND), not at an offset
// the Writer keeps.
func TestRunlog_AppendsAfterAnotherWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-1.jsonl")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	line := func(i int) string { return fmt.Sprintf(`{"kind":"test","g":0,"i":%d,"pad":""}`, i) }
	var want []string
	for i := range 3 {
		w.Enqueue(testRecord{Kind: "test", I: i})
		want = append(want, line(i))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	const external = `{"kind":"external"}`
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, werr := f.WriteString(external + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		t.Fatalf("appending the external line: %v", err)
	}
	want = append(want, external)

	for i := 3; i < 6; i++ {
		w.Enqueue(testRecord{Kind: "test", I: i})
		want = append(want, line(i))
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readLines(t, path); !slices.Equal(got, want) {
		t.Errorf("file lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// sigkillChildEnv carries the run-log path to the child copy of TestRunlog_SurvivesSIGKILL.
const sigkillChildEnv = "RL_RUNLOG_SIGKILL_CHILD"

// handedOver is the child's signal that its 1,000 lines have been handed to the kernel.
const handedOver = "handed over\n"

func TestRunlog_SurvivesSIGKILL(t *testing.T) {
	const lines = 1000
	if path := os.Getenv(sigkillChildEnv); path != "" {
		sigkillChild(t, path, lines)
		return
	}
	path := filepath.Join(t.TempDir(), "api-1.jsonl")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^TestRunlog_SurvivesSIGKILL$", "-test.count=1", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), sigkillChildEnv+"="+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Read the child's stdout until its signal (or EOF: the child failed or the timeout hit).
	var out []byte
	buf := make([]byte, 4096)
	for !bytes.Contains(out, []byte(handedOver)) {
		n, err := stdout.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			waited = true
			werr := cmd.Wait()
			t.Fatalf("the child ended (%v, %v) before its %q signal; stdout:\n%s\nstderr:\n%s",
				err, werr, strings.TrimSpace(handedOver), out, stderr.Bytes())
		}
	}
	if err := cmd.Process.Kill(); err != nil { // SIGKILL: no Close, no exit handlers
		t.Fatalf("SIGKILL: %v", err)
	}
	waited = true
	werr := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(werr, &exitErr) || exitErr.String() != "signal: killed" {
		t.Fatalf("child ended with %v, want signal: killed; stderr:\n%s", werr, stderr.Bytes())
	}

	got := readLines(t, path)
	if len(got) != lines {
		t.Fatalf("%d lines survived SIGKILL, want all %d", len(got), lines)
	}
	for i, l := range got {
		var r testRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil || r.Kind != "test" || r.I != i {
			t.Fatalf("line %d = %q (%v), want record %d", i+1, l, err, i)
		}
	}
}

// sigkillChild runs in the child: it writes the lines through a Writer, waits until every
// one has been handed to the kernel, signals the parent, and waits to be killed without ever
// closing the Writer.
func sigkillChild(t *testing.T, path string, lines int) {
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range lines {
		w.Enqueue(testRecord{Kind: "test", I: i})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if w.Drops() != 0 {
		t.Fatalf("Drops() = %d, want 0", w.Drops())
	}
	if _, err := os.Stdout.WriteString(handedOver); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	<-timer.C
	t.Error("the parent did not kill this child within 30 s")
}
