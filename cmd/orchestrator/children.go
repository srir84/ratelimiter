package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ratelimiter/harness/runlog"
)

// A run's child processes (DESIGN §B1, §C8 "left to the plan"; IMPLEMENTATION_PLAN H3 step 4).
const (
	// stderrSuffix ends a child's saved output file: runs/<id>/<name>.stderr (plan choice).
	stderrSuffix = ".stderr"
	// outputWait is how long Wait waits for a child's output to close after it exits
	// (cmd.WaitDelay): only hitgen's output goes through a pipe.
	outputWait = 5 * time.Second
	// reapWait bounds the wait for a SIGKILLed process to be reaped.
	reapWait = 10 * time.Second
	// maxCaptureLine is the longest line of hitgen's output the orchestrator reads for its
	// "sending" line; every byte still goes to the file.
	maxCaptureLine = 64 << 10
	// maxCommandOutput bounds the output kept from a short command (rladmin start-epoch,
	// pg_ctl, sysctl, the version commands), per stream.
	maxCommandOutput = 64 << 10
)

// child is one of a run's long-running processes (valkey-server, each hitapi, the sampler,
// hitgen): in its own process group, so a terminal's SIGINT reaches only the orchestrator,
// which stops every child in order; its stdout and stderr saved in runs/<id>/<name>.stderr.
// A waiter goroutine reaps it; stop ends it (AGENTS.md §2 "Child processes").
type child struct {
	name string      // the output file's stem: valkey-server, hitapi-N, sampler, hitgen
	proc runlog.Proc // its pids.json entry

	cmd           *exec.Cmd
	stopRequested atomic.Bool    // set before the orchestrator signals it or asks it to exit
	done          chan struct{}  // closed once Wait has returned
	waitErr       error          // Wait's result, set before done closes
	early         bool           // it exited before stopRequested was set; set before done closes
	capture       *captureWriter // hitgen's output writer; nil for the others
	closeOut      func()         // closes the capture file once Wait has returned; nil without one
}

// childRecord is one child's end as context.json records it (plan choice): how it exited,
// whether it exited before the orchestrator stopped it, and whether it had to be killed. A
// child a [[faults]] faultctl command ended on purpose (store down's SHUTDOWN, api kill's
// SIGKILL; IMPLEMENTATION_PLAN H4 step 7) names that command in fault: the fault's own record
// judges it, and the process that replaced it, under the same name, has a record of its own.
type childRecord struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Instance int    `json:"instance,omitempty"`
	PID      int    `json:"pid"`
	Exit     string `json:"exit"`
	Early    bool   `json:"exited_before_stop"`
	GraceNs  int64  `json:"grace_ns"`
	Killed   bool   `json:"killed"`
	Fault    string `json:"fault,omitempty"`
	Error    string `json:"error,omitempty"`
}

// childSpec is how to start a child.
type childSpec struct {
	name     string // output file stem
	role     string // pids.json role
	instance int    // hitapi only
	path     string // the program
	args     []string
	env      []string
	ulimit   bool           // start through the file-limit shell, runlog.FileLimitCommand (valkey-server, hitapi)
	capture  *captureWriter // hitgen: its output goes through this writer, which reads its lines
	// appendOut appends the output to an existing file: a process a fault restarted continues
	// the output file of the one it replaces, as faultctl's restart does (plan H4 step 7).
	appendOut bool
}

// startChild starts sp in dir (the run directory, which holds its output file) and returns it
// running. It is started on a context that never ends (context.WithoutCancel): the
// orchestrator stops it explicitly (stop), so a SIGINT that ends ctx never kills a child
// before its turn. Its output file is new, or with sp.appendOut appended to.
func startChild(ctx context.Context, dir string, sp childSpec) (*child, error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if sp.appendOut {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	out, err := os.OpenFile(filepath.Join(dir, sp.name+stderrSuffix), flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("starting %s: its output file: %w", sp.name, err)
	}
	prog, args := sp.path, sp.args
	if sp.ulimit {
		line := runlog.FileLimitCommand(sp.path, sp.args...)
		prog, args = line[0], line[1:]
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), prog, args...)
	cmd.Env = sp.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = outputWait
	c := &child{name: sp.name, cmd: cmd, done: make(chan struct{})}
	if sp.capture != nil {
		sp.capture.f = out
		cmd.Stdout, cmd.Stderr = sp.capture, sp.capture // one pipe for both
		c.capture = sp.capture
		c.closeOut = func() { _ = out.Close() } // its writes are checked in captureWriter
	} else {
		cmd.Stdout, cmd.Stderr = out, out // the child writes the file itself: nothing to drain
	}
	err = cmd.Start()
	if sp.capture == nil || err != nil {
		_ = out.Close() // the child holds its own descriptor, or there is no child
	}
	if err != nil {
		return nil, fmt.Errorf("starting %s: %w", sp.name, err)
	}
	c.proc = runlog.Proc{Role: sp.role, Instance: sp.instance, PID: cmd.Process.Pid}
	go func() {
		err := cmd.Wait()
		c.waitErr, c.early = err, !c.stopRequested.Load()
		if c.closeOut != nil {
			c.closeOut()
		}
		close(c.done)
	}()
	return c, nil
}

// exited reports whether c has exited (and been reaped).
func (c *child) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// stop stops c (AGENTS.md §2 "Child processes"; plan H3 step 4): SIGTERM to its process
// group, up to grace for it to exit, then SIGKILL to the group and up to reapWait for it to be
// reaped. Members of its group still alive after it are killed too. It returns c's record.
func (c *child) stop(grace time.Duration) childRecord {
	c.stopRequested.Store(true)
	rec := childRecord{Name: c.name, Role: c.proc.Role, Instance: c.proc.Instance, PID: c.proc.PID, GraceNs: grace.Nanoseconds()}
	if !c.exited() {
		_ = syscall.Kill(-c.proc.PID, syscall.SIGTERM) // ESRCH: it exited meanwhile, which done shows
		rec.Killed = !c.waitFor(grace)
	}
	return c.finish(rec)
}

// finish ends c once it was asked to exit, or has: a SIGKILL to the group when rec.Killed, up
// to reapWait for it to be reaped, a SIGKILL to what is left of its group, then the record of
// how it ended.
func (c *child) finish(rec childRecord) childRecord {
	if rec.Killed {
		_ = syscall.Kill(-c.proc.PID, syscall.SIGKILL)
	}
	if !c.waitFor(reapWait) {
		rec.Error = fmt.Sprintf("not reaped %v after SIGKILL", reapWait)
		return rec
	}
	// A process of its group that outlived it, such as a store's rewrite child: the group's
	// leader is reaped, so its PID names nothing else yet.
	if syscall.Kill(-c.proc.PID, 0) == nil {
		_ = syscall.Kill(-c.proc.PID, syscall.SIGKILL)
	}
	rec.Exit, rec.Early = exitText(c.waitErr), c.early
	return rec
}

// waitFor waits up to d for c to exit and reports whether it did.
func (c *child) waitFor(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.done:
		return true
	case <-t.C:
		return false
	}
}

// exitText describes how a process ended, from Wait's error: "exit N", "signal: NAME", or the
// error itself.
func exitText(err error) string {
	if err == nil {
		return "exit 0"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return "signal: " + ws.Signal().String()
		}
		return fmt.Sprintf("exit %d", ee.ExitCode())
	}
	return err.Error()
}

// captureWriter takes hitgen's stdout and stderr (one pipe): it writes every byte to the
// output file and hands each complete line of up to maxCaptureLine bytes to onLine (the
// orchestrator reads hitgen's "sending" line, its t0; plan choice). Write never fails and
// waits on nothing but the file, so the pipe is always drained and the child never blocks on
// it: a failed file write is kept, and later bytes are only read. exec calls Write from one
// goroutine at a time.
type captureWriter struct {
	f      *os.File
	onLine func(line []byte)

	mu   sync.Mutex
	err  error  // the first failed file write
	line []byte // the current line so far, at most maxCaptureLine bytes
	long bool   // the current line passed maxCaptureLine: it is not read
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		if _, err := w.f.Write(p); err != nil {
			w.err = err
		}
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if !w.long {
			if len(w.line)+len(part) > maxCaptureLine {
				w.long, w.line = true, w.line[:0]
			} else {
				w.line = append(w.line, part...)
			}
		}
		if i < 0 {
			break
		}
		if !w.long && w.onLine != nil {
			w.onLine(w.line)
		}
		w.line, w.long = w.line[:0], false
		p = p[i+1:]
	}
	return n, nil
}

// writeErr returns the first failed write to the output file, nil when every write succeeded.
func (w *captureWriter) writeErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// runCommand runs a short command within ctx, in dir ("" for the orchestrator's own), in its
// own process group (a terminal's SIGINT reaches only the orchestrator) and returns its
// stdout, at most maxCommandOutput bytes. Its stderr goes to errOut when not nil (a run file),
// else is kept and quoted, shortened, in the error of a failed run. ctx ending kills it.
func runCommand(ctx context.Context, dir string, env []string, errOut io.Writer, prog string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = outputWait
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxCommandOutput, maxCommandOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if errOut != nil {
		cmd.Stderr = errOut
	}
	err := cmd.Run()
	if err != nil {
		if msg := bytes.TrimSpace(stderr.buf.Bytes()); len(msg) > 0 {
			return stdout.buf.Bytes(), fmt.Errorf("%s: %w: %.400s", filepath.Base(prog), err, msg)
		}
		return stdout.buf.Bytes(), fmt.Errorf("%s: %w", filepath.Base(prog), err)
	}
	return stdout.buf.Bytes(), nil
}

// limitedBuffer keeps the first max bytes written to it and drops the rest; Write never fails,
// so the command's pipe is always drained.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
