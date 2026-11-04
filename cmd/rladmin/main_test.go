package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/store/resp"
	"github.com/srir84/ratelimiter/internal/testutil/fakeresp"
)

// signalChildEnv marks the child copy of TestSecondSignalEnds (any value).
const signalChildEnv = "RL_RLADMIN_SIGNAL_CHILD"

// mainChildEnv marks a child of TestMainSecondSignalEnds or TestMainSignalEndsStoreWait:
// TestMain then runs the real main() on the child's own command line instead of the tests. Its
// value is mainChildWriting or mainChildStore, where main is to wait.
const mainChildEnv = "RL_RLADMIN_MAIN_CHILD"

// The main children's waits.
const (
	mainChildWriting = "writing" // TestMainSecondSignalEnds: main waits in its write to stdout
	mainChildStore   = "store"   // TestMainSignalEndsStoreWait: main waits for the store's AUTH reply
)

// ignoreSIGINTEnv marks a child that must start with SIGINT ignored, as a script's background
// job does (any value). TestMain then ignores SIGINT and re-executes the test binary without
// the mark (reexecIgnoringSIGINT): same pid, arguments and descriptors. An ignored signal stays
// ignored across execve, and Go's runtime leaves a SIGINT that is ignored at start ignored, so
// the new image starts with SIGINT ignored, and its start line says so.
const ignoreSIGINTEnv = "RL_RLADMIN_IGNORE_SIGINT"

// The children's lines. TestSecondSignalEnds's child writes to stdout: first
// childSIGINTDefault or childSIGINTIgnored, whether SIGINT was ignored when it started (read
// before signalContext, whose Notify changes that); then childReady once signalContext has
// trapped the signals, just before it blocks; then childCancelled once a signal has cancelled
// that context. A main child writes to stderr: first childSIGINTDefault or childSIGINTIgnored
// (TestMain), then what main writes; TestMainSecondSignalEnds's child also writes childWriting
// once main() is in its write to stdout (reportMainWriting).
const (
	childSIGINTDefault = "SIGINT not ignored at start"
	childSIGINTIgnored = "SIGINT ignored at start"
	childReady         = "ready"
	childCancelled     = "cancelled"
	childWriting       = "main is writing to stdout"
)

// The children's time bounds.
const (
	startWait  = 30 * time.Second       // the child's start, to its first awaited line
	cancelWait = 10 * time.Second       // the first signal, to its childCancelled line
	stillRuns  = 250 * time.Millisecond // how long the child must outlive a signal that must not end it
	endWait    = 5 * time.Second        // the signal that must end it, to its end
)

// TestMain runs the tests, or, in a main child, the real main(), which exits or is ended by a
// signal, so it never returns here. A main child first reports whether SIGINT was ignored when
// it started, read before main's signalContext changes that. A child marked with
// ignoreSIGINTEnv first re-executes itself with SIGINT ignored.
func TestMain(m *testing.M) {
	if os.Getenv(ignoreSIGINTEnv) != "" {
		reexecIgnoringSIGINT()
	}
	if wait := os.Getenv(mainChildEnv); wait != "" {
		start := childSIGINTDefault
		if signal.Ignored(os.Interrupt) {
			start = childSIGINTIgnored
		}
		_, _ = os.Stderr.WriteString(start + "\n") // a lost line fails the parent test
		if wait == mainChildWriting {
			go reportMainWriting() // ends after its one line; the parent test ends this process
		}
		main()
	}
	m.Run()
}

// reexecIgnoringSIGINT ignores SIGINT and replaces this process with a new image of the test
// binary (syscall.Exec), with the same arguments and the environment less ignoreSIGINTEnv. If
// that fails, it writes why to stderr and exits 2, which fails the parent test.
func reexecIgnoringSIGINT() {
	signal.Ignore(os.Interrupt)
	bin, err := os.Executable()
	if err == nil {
		env := slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, ignoreSIGINTEnv+"=") })
		err = syscall.Exec(bin, os.Args, env)
	}
	_, _ = os.Stderr.WriteString("re-executing the test binary with SIGINT ignored: " + err.Error() + "\n")
	os.Exit(2)
}

// TestSecondSignalEnds (F1; DESIGN §A6 exit codes): signalContext's first SIGINT or SIGTERM
// cancels rladmin's root context, which ends no wait that observes no context, and releases
// both signals, so a second one sent after the first has been handled takes its default action
// and ends the process by that signal. A child copy of this test traps the signals with
// signalContext and blocks in read(2) on its stdin, a pipe the test never writes to, as
// loadPolicy's read blocks on a hung mount. After the first signal its context is cancelled
// and it is still running 250 ms later; the second must end it by that signal within 5 s. The
// cases are SIGINT then SIGTERM, SIGTERM then SIGTERM, and SIGINT then SIGINT (Ctrl-C twice).
// A SIGINT that was ignored when the child started (a script's background job) still cancels
// the first time, and is ignored again once released (os/signal): then the second SIGINT must
// leave the child running 250 ms later, and a SIGTERM must end it. The child reports which
// holds, and neither is skipped. The last case, SIGINT then SIGINT again, starts the child
// with SIGINT ignored (ignoreSIGINTEnv) and requires that it did, so every run takes that path.
func TestSecondSignalEnds(t *testing.T) {
	if os.Getenv(signalChildEnv) != "" {
		secondSignalChild(t)
		return
	}
	for _, c := range []struct {
		name          string
		first, second syscall.Signal
		ignoreSIGINT  bool // the child starts with SIGINT ignored (ignoreSIGINTEnv)
	}{
		{"SIGINT then SIGTERM", syscall.SIGINT, syscall.SIGTERM, false},
		{"SIGTERM then SIGTERM", syscall.SIGTERM, syscall.SIGTERM, false},
		{"SIGINT then SIGINT", syscall.SIGINT, syscall.SIGINT, false},
		{"started ignoring SIGINT, SIGINT then SIGINT", syscall.SIGINT, syscall.SIGINT, true},
	} {
		t.Run(c.name, func(t *testing.T) { secondSignal(t, c.first, c.second, c.ignoreSIGINT) })
	}
}

// secondSignalChild is the child copy: it reports whether SIGINT was ignored when it started,
// traps the signals with signalContext, prints childReady and blocks in read(2) on stdin. A
// signal that cancels the context prints childCancelled (context.AfterFunc) and leaves the
// read blocked.
func secondSignalChild(t *testing.T) {
	start := childSIGINTDefault
	if signal.Ignored(os.Interrupt) { // read before signalContext: its Notify clears it
		start = childSIGINTIgnored
	}
	if _, err := os.Stdout.WriteString(start + "\n"); err != nil {
		t.Fatal(err)
	}
	ctx, stop := signalContext()
	defer stop()
	context.AfterFunc(ctx, func() { _, _ = os.Stdout.WriteString(childCancelled + "\n") })
	if _, err := os.Stdout.WriteString(childReady + "\n"); err != nil {
		t.Fatal(err)
	}
	n, err := os.Stdin.Read(make([]byte, 1))
	t.Errorf("the read of stdin returned (%d bytes, %v), want it blocked until a signal ends this process", n, err)
}

// secondSignal runs one case of TestSecondSignalEnds: first, then second, with the child
// started with SIGINT ignored if ignoreSIGINT.
func secondSignal(t *testing.T, first, second syscall.Signal, ignoreSIGINT bool) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	env := []string{signalChildEnv + "=1"}
	if ignoreSIGINT {
		env = append(env, ignoreSIGINTEnv+"=1")
	}
	cmd := childCommand(ctx, t, env, "-test.run=^TestSecondSignalEnds$", "-test.count=1", "-test.timeout=1m")
	var stderr bytes.Buffer // read only once Wait has returned
	cmd.Stderr = &stderr
	if _, err := cmd.StdinPipe(); err != nil { // never written to: the child's read(2) waits on it
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	w := watchChild(cmd, stdout)
	defer w.stop(cmd)

	if err := w.await(ctx, childReady, startWait); err != nil {
		w.stop(cmd) // then stderr is complete
		t.Fatalf("before the first signal: %v; stderr:\n%s", err, stderr.Bytes())
	}
	if len(w.seen) != 2 {
		t.Fatalf("the child's lines before the first signal: %q, want [%q or %q, %q]", w.seen, childSIGINTDefault, childSIGINTIgnored, childReady)
	}
	ignored := sigintIgnoredAtStart(t, w.seen[0], ignoreSIGINT)
	sendSignal(t, cmd, first)
	if err := w.await(ctx, childCancelled, cancelWait); err != nil {
		t.Fatalf("after the first signal (%v): %v; want its context cancelled and the child still running", first, err)
	}
	if w.endsWithin(ctx, stillRuns) {
		t.Fatalf("the child ended (%v) within %v of the first signal (%v), want it still running; stdout %q, stderr:\n%s",
			w.err, stillRuns, first, w.seen, stderr.Bytes())
	}
	sendSignal(t, cmd, second)
	last := second // the signal that must end the child
	if second == syscall.SIGINT && ignored {
		if w.endsWithin(ctx, stillRuns) {
			t.Fatalf("the child ended (%v) within %v of the second SIGINT, want it still running: SIGINT was ignored when it started, so once released it is ignored again; stdout %q, stderr:\n%s",
				w.err, stillRuns, w.seen, stderr.Bytes())
		}
		sendSignal(t, cmd, syscall.SIGTERM)
		last = syscall.SIGTERM
	}
	if !w.endsWithin(ctx, endWait) {
		t.Fatalf("the child still ran %v after the last signal (%v), want it ended by that signal", endWait, last)
	}
	if !killedBy(w.err, last) {
		t.Errorf("the child ended with %v after the last signal (%v), want it killed by that signal; stdout %q, stderr:\n%s",
			w.err, last, w.seen, stderr.Bytes())
	}
}

// TestMainSecondSignalEnds (F1; DESIGN §A6 exit codes): main itself traps both signals the way
// signalContext does. The child runs the real main() as `rladmin --help` (TestMain), with its
// stdout a pipe that the test filled first and never reads, so main waits in its write of the
// usage text: a wait that observes no context, before any store connection. Once the child
// reports that main is in that write (reportMainWriting), the first signal must leave it
// running 250 ms later (it only cancels main's context), and the second must end it by that
// signal within 5 s (the first one released both signals). The cases are SIGTERM then SIGTERM,
// and SIGINT then SIGTERM (Ctrl-C first, so main must trap SIGINT too). Notify stops SIGINT
// being ignored, so both hold whether or not SIGINT was ignored when the child started; the
// child reports which. A FIFO given as --policy cannot stand in for the wait: loadPolicy
// refuses one unopened (TestPolicyFileBounded).
func TestMainSecondSignalEnds(t *testing.T) {
	for _, c := range []struct {
		name          string
		first, second syscall.Signal
	}{
		{"SIGTERM then SIGTERM", syscall.SIGTERM, syscall.SIGTERM},
		{"SIGINT then SIGTERM", syscall.SIGINT, syscall.SIGTERM},
	} {
		t.Run(c.name, func(t *testing.T) { mainSecondSignal(t, c.first, c.second) })
	}
}

// mainSecondSignal runs one case of TestMainSecondSignalEnds: first, then second.
func mainSecondSignal(t *testing.T, first, second syscall.Signal) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outR.Close() }() // open until the child is gone: its write waits rather than failing
	defer func() { _ = outW.Close() }() // a second close, after the one below, changes nothing
	// Fill the pipe: a write that takes some bytes and is then cut short by its deadline met a
	// full pipe, which nothing reads from then on.
	for filled := 0; filled == 0; {
		if err := outW.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		n, err := outW.Write(make([]byte, 1<<20))
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("filling the child's stdout: wrote %d bytes (%v), want the write cut short by its deadline", n, err)
		}
		filled += n
	}
	cmd := childCommand(ctx, t, []string{mainChildEnv + "=" + mainChildWriting}, "--help")
	cmd.Stdout = outW
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	_ = outW.Close() // the child holds its own copy
	w := watchChild(cmd, stderr)
	defer w.stop(cmd)

	if err := w.await(ctx, childWriting, startWait); err != nil {
		t.Fatalf("before the first signal: %v", err)
	}
	if len(w.seen) != 2 {
		t.Fatalf("the child's lines before the first signal: %q, want [%q or %q, %q]", w.seen, childSIGINTDefault, childSIGINTIgnored, childWriting)
	}
	sigintIgnoredAtStart(t, w.seen[0], false)
	sendSignal(t, cmd, first)
	if w.endsWithin(ctx, stillRuns) {
		t.Fatalf("main ended (%v) within %v of the first signal (%v), want it still running: that signal only cancels its context; stderr %q",
			w.err, stillRuns, first, w.seen)
	}
	sendSignal(t, cmd, second)
	if !w.endsWithin(ctx, endWait) {
		t.Fatalf("main still ran %v after the second signal (%v), want it ended by that signal", endWait, second)
	}
	if !killedBy(w.err, second) {
		t.Errorf("main ended with %v after the second signal (%v), want it killed by that signal; stderr %q", w.err, second, w.seen)
	}
}

// TestMainSignalEndsStoreWait (F1; DESIGN §A6 exit codes): main hands run the context that
// signalContext cancels, so one SIGINT or SIGTERM ends a store wait with exit 3. The child runs
// the real main() as `rladmin status` against a fake store that reads AUTH and never answers
// it (HangAuth). Once its AUTH has reached the store and it still waits 250 ms later, one
// signal must make it exit 3 within 5 s, far inside the command's own deadline
// (commandTimeout, 30 s), with the failed AUTH named. Notify stops SIGINT being ignored, so the
// SIGINT case holds whether or not SIGINT was ignored when the child started; the child reports
// which. The last case starts the child with SIGINT ignored (ignoreSIGINTEnv) and requires
// that it did, so every run takes that path.
func TestMainSignalEndsStoreWait(t *testing.T) {
	for _, c := range []struct {
		name         string
		sig          syscall.Signal
		ignoreSIGINT bool // the child starts with SIGINT ignored (ignoreSIGINTEnv)
	}{
		{"SIGTERM", syscall.SIGTERM, false},
		{"SIGINT", syscall.SIGINT, false},
		{"started ignoring SIGINT, SIGINT", syscall.SIGINT, true},
	} {
		t.Run(c.name, func(t *testing.T) { mainStoreWait(t, c.sig, c.ignoreSIGINT) })
	}
}

// mainStoreWait runs one case of TestMainSignalEndsStoreWait: one sig, with the child started
// with SIGINT ignored if ignoreSIGINT.
func mainStoreWait(t *testing.T, sig syscall.Signal, ignoreSIGINT bool) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	srv := fakeresp.Start(t, "rl_admin", testPW)
	srv.HangAuth()
	env := []string{mainChildEnv + "=" + mainChildStore, "RL_ADMIN_PASSWORD=" + testPW}
	if ignoreSIGINT {
		env = append(env, ignoreSIGINTEnv+"=1")
	}
	cmd := childCommand(ctx, t, env, "status", "--store", srv.Addr())
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	w := watchChild(cmd, stderr)
	defer w.stop(cmd)

	// The child writes its start line before main runs, so before its AUTH: wait for both.
	for end := time.Now().Add(startWait); srv.Count("AUTH") == 0 || len(w.seen) == 0; {
		if w.endsWithin(ctx, 5*time.Millisecond) {
			t.Fatalf("the child ended (%v) before its AUTH reached the store; stderr %q", w.err, w.seen)
		}
		if ctx.Err() != nil || time.Now().After(end) {
			t.Fatalf("no AUTH from the child reached the store within %v; stderr %q", startWait, w.seen)
		}
	}
	sigintIgnoredAtStart(t, w.seen[0], ignoreSIGINT)
	if w.endsWithin(ctx, stillRuns) {
		t.Fatalf("the child ended (%v) within %v of its AUTH reaching the store, want it waiting for the reply; stderr %q",
			w.err, stillRuns, w.seen)
	}
	sendSignal(t, cmd, sig)
	if !w.endsWithin(ctx, endWait) {
		t.Fatalf("main still ran %v after one signal (%v), want that signal to end its wait for the AUTH reply (exit 3) long before its own %v deadline",
			endWait, sig, commandTimeout)
	}
	var exit *exec.ExitError
	if !errors.As(w.err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("main ended with %v after one signal (%v), want exit 3; stderr %q", w.err, sig, w.seen)
	}
	if !strings.Contains(strings.Join(w.seen, "\n"), "rladmin: authenticating to the store") {
		t.Errorf("main's stderr after one signal (%v) is %q, want the failed AUTH named", sig, w.seen)
	}
}

// sigintIgnoredAtStart checks that line is a child's start line, logs it and reports whether
// SIGINT was ignored when the child started. With mustIgnore (the child was started with
// ignoreSIGINTEnv), it must have been: otherwise the case would test the default instead.
func sigintIgnoredAtStart(t *testing.T, line string, mustIgnore bool) bool {
	t.Helper()
	if line != childSIGINTDefault && line != childSIGINTIgnored {
		t.Fatalf("the child's first line is %q, want %q or %q", line, childSIGINTDefault, childSIGINTIgnored)
	}
	ignored := line == childSIGINTIgnored
	t.Logf("SIGINT ignored when the child started: %v", ignored)
	if mustIgnore && !ignored {
		t.Fatalf("the child's first line is %q, want %q: it was re-executed with SIGINT ignored (%s)", line, childSIGINTIgnored, ignoreSIGINTEnv)
	}
	return ignored
}

// reportMainWriting writes childWriting to stderr once the goroutine running main() is in
// run's write to stdout, which it looks for in the goroutine dump every millisecond for up to
// startWait: main has then done all it does before run, signalContext included. It returns
// after one line.
func reportMainWriting() {
	runFrame := []byte(runtime.FuncForPC(reflect.ValueOf(run).Pointer()).Name() + "(")
	writeFrame := []byte("os.(*File).Write")
	buf := make([]byte, 1<<20)
	for end := time.Now().Add(startWait); time.Now().Before(end); time.Sleep(time.Millisecond) {
		for _, g := range bytes.Split(buf[:runtime.Stack(buf, true)], []byte("\n\n")) {
			if bytes.Contains(g, runFrame) && bytes.Contains(g, writeFrame) {
				_, _ = os.Stderr.WriteString(childWriting + "\n")
				return
			}
		}
	}
	_, _ = os.Stderr.WriteString("main did not reach its write to stdout within " + startWait.String() + "\n")
}

// childCommand runs this test binary again with args, and env added to its environment (an
// entry there wins over an inherited one); it is SIGKILLed if ctx ends first.
func childCommand(ctx context.Context, t *testing.T, env []string, args ...string) *exec.Cmd {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// sendSignal sends sig to the child.
func sendSignal(t *testing.T, cmd *exec.Cmd, sig syscall.Signal) {
	t.Helper()
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("sending %v to the child: %v", sig, err)
	}
}

// killedBy reports whether err, from Wait, says that the signal sig killed the child.
func killedBy(err error, sig syscall.Signal) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	ws, ok := exit.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == sig
}

// childWatch follows a child's lines (on stdout or stderr) and its end. Only the test's
// goroutine reads seen; err is set before done closes.
type childWatch struct {
	lines chan string   // the child's lines; closed at EOF
	done  chan struct{} // closed once Wait has returned
	err   error         // Wait's error
	seen  []string      // the lines the test has read so far
}

// watchChild reads the child's lines from out on a goroutine of its own, which ends after Wait
// once out has closed: stop makes sure it does.
func watchChild(cmd *exec.Cmd, out io.Reader) *childWatch {
	w := &childWatch{lines: make(chan string, 64), done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			w.lines <- sc.Text()
		}
		close(w.lines)
		w.err = cmd.Wait()
		close(w.done)
	}()
	return w
}

// await reads lines until want, the child's output closing, d passing or ctx ending.
func (w *childWatch) await(ctx context.Context, want string, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case l, ok := <-w.lines:
			if !ok {
				<-w.done
				if w.err == nil {
					return fmt.Errorf("the child exited 0 before its %q line; lines %q", want, w.seen)
				}
				return fmt.Errorf("the child ended (%w) before its %q line; lines %q", w.err, want, w.seen)
			}
			w.seen = append(w.seen, l)
			if l == want {
				return nil
			}
		case <-timer.C:
			return fmt.Errorf("no %q line within %v; lines %q", want, d, w.seen)
		case <-ctx.Done():
			return fmt.Errorf("waiting for the %q line: %w; lines %q", want, ctx.Err(), w.seen)
		}
	}
}

// endsWithin reports whether the child ends (and is reaped) within d, reading its lines
// meanwhile, the last ones included; false when ctx ends first.
func (w *childWatch) endsWithin(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	lines := w.lines
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				lines = nil // closed: only done or the timer is left
				continue
			}
			w.seen = append(w.seen, l)
		case <-w.done:
			for l := range w.lines { // closed before done: what the child wrote last, if not yet read
				w.seen = append(w.seen, l)
			}
			return true
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// stop SIGKILLs a child still running and waits until it is reaped and the reader is done.
func (w *childWatch) stop(cmd *exec.Cmd) {
	select {
	case <-w.done:
		return
	default:
	}
	_ = cmd.Process.Kill() // a failed case left it running: SIGKILL ends it, whatever it traps
	for range w.lines {    // lets the reader reach Wait
	}
	<-w.done
}

// TestSignalBeforeConnect (F1): loadPolicy takes no ctx, but a signal that lands while the
// policy file loads is still honoured before any connection, because run hands the cancelled
// ctx to dial. start-epoch with a valid policy and a cancelled ctx exits 3 having opened no
// connection to the store; with a live ctx the same command connects once and exits 0.
func TestSignalBeforeConnect(t *testing.T) {
	for _, c := range []struct {
		name      string
		cancelled bool
		code      int
		conns     int
	}{
		{"live ctx", false, 0, 1},
		{"cancelled ctx", true, 3, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newRLEnv(t)
			e.handle(scripts.AdminStartEpoch, func(_, argv []string) resp.Value {
				return arr(ival(40), ival(tSample), bulk(""), bulk(argv[0]))
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if c.cancelled {
				cancel() // the signal came while the policy loaded
			}
			lookup := func(k string) (string, bool) {
				v, ok := e.env[k]
				return v, ok
			}
			var stdout, stderr bytes.Buffer
			code := run(ctx, []string{"start-epoch", "--policy", e.policy}, lookup, &stdout, &stderr)
			if code != c.code || e.srv.Accepted() != c.conns {
				t.Errorf("start-epoch: exit %d with %d connections to the store, want %d with %d; stderr:\n%s",
					code, e.srv.Accepted(), c.code, c.conns, stderr.String())
			}
			if c.cancelled && !strings.Contains(stderr.String(), "connecting to the store") {
				t.Errorf("start-epoch with a cancelled ctx printed %q, want the failed connection named", stderr.String())
			}
		})
	}
}
