//go:build rltest

package fixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ratelimiter/harness/runlog"
)

// API kill timing and routes (DESIGN §B6 FLT-6; IMPLEMENTATION_PLAN H4 step 2).
const (
	// prekillPath is the pre-kill route (plan H1 step 11): it stops /hit, waits up to P + 1 s
	// for the admitted requests and drains the run log, then answers the drop counts.
	prekillPath = "/_test/prekill"
	// healthPath is hitapi's /health: status "ready" once the instance can serve.
	healthPath = "/health"
	// maxReplyBody bounds a reply KillAPI reads, in bytes.
	maxReplyBody = 64 << 10
	// goneWait bounds the wait for the killed process to be gone; gonePoll paces it (plan
	// choice).
	goneWait = 10 * time.Second
	gonePoll = 10 * time.Millisecond
	// psWait bounds one ps run.
	psWait = 5 * time.Second
	// readyWait bounds the wait for a restarted instance's /health to show status ready (plan
	// H4 step 2: 30 s); readyPoll paces the reads and readyTry bounds one (plan choice).
	readyWait = 30 * time.Second
	readyPoll = 100 * time.Millisecond
	readyTry  = time.Second
	// statusReady is /health's status of an instance that can serve (DESIGN §A5.11).
	statusReady = "ready"
)

// ErrPrekillUnread is in KillAPI's error when the pre-kill read failed: the call failed, its
// reply was not 200 with every field, or the run log was not drained (log_queue_len not 0).
// The instance was still killed (and restarted when asked), and its pre_kill line says
// read_ok:false: the run's refund evidence is incomplete (HAR-13).
const ErrPrekillUnread Error = "fixtures: the pre-kill read failed (read_ok:false): the refund evidence is incomplete (HAR-13)"

// errNotRunning marks a PID that named no process: KillAPI refused it before the pre-kill call,
// or the process ended before the kill, so there was nothing left to kill.
const errNotRunning Error = "fixtures: the instance's process was not running"

// APITarget is the hitapi instance KillAPI kills (DESIGN §B6 FLT-6; IMPLEMENTATION_PLAN H4
// step 2).
type APITarget struct {
	// BaseURL is the instance's http://host:port: /_test/prekill and /health are under it.
	BaseURL string
	// PID is the running instance's process.
	PID int
	// Args is the exact command line that started the instance: a restart runs it again when
	// Start is nil.
	Args []string
	// Env is the whole environment of an instance started from Args, its RL_*_PASSWORD
	// variables included (plan choice): nil or empty gives it none, never the caller's.
	Env []string
	// OutPath, when not empty, is the file (created when missing, appended to) that takes the
	// output of an instance started from Args (plan choice); empty discards it.
	OutPath string
	// LogPath is the instance's run log, api-N.jsonl: the pre_kill line is appended to it.
	LogPath string
	// PrekillTimeout bounds the pre-kill call: P + 2 s (plan choice), as /_test/prekill may
	// wait P + 1 s for the admitted requests before it drains the run log.
	PrekillTimeout time.Duration
	// Client makes the two requests (AGENTS.md §1: built in main with explicit timeouts). A
	// Timeout of its own must not be shorter than PrekillTimeout.
	Client *http.Client
	// Start, when not nil, starts the instance again instead of Args and returns its PID (plan
	// choice): the caller keeps the process as its child and waits for it, as the orchestrator
	// keeps a restarted hitapi in its child table. It gets a context with its own 30 s
	// deadline: a child it starts must not end with it (AGENTS.md §2, context.WithoutCancel).
	Start func(context.Context) (int, error)
}

// preKillRecord is the lifecycle line KillAPI appends just before SIGKILL (DESIGN §B8 "pre-kill
// read"; plan H4 step 2): the pre-kill reply's drop counts, and read_ok.
type preKillRecord struct {
	Kind          string `json:"kind"`
	Event         string `json:"event"`
	ListenerDrops uint64 `json:"listener_drops"`
	LogDrops      int64  `json:"log_drops"`
	ReadOK        bool   `json:"read_ok"`
}

// prekillReply is POST /_test/prekill's answer (harness/httpapi), 200 or 503: every field
// must be present, and none other.
type prekillReply struct {
	ListenerDrops *uint64 `json:"listener_drops"`
	LogDrops      *int64  `json:"log_drops"`
	LogQueueLen   *int64  `json:"log_queue_len"`
}

// KillAPI kills one hitapi instance (FLT-6, DESIGN §B6; plan H4 step 2): POST /_test/prekill
// first, within t.PrekillTimeout; then one line {"kind":"lifecycle","event":"pre_kill",
// "listener_drops":…,"log_drops":…,"read_ok":…} appended to t.LogPath with a single O_APPEND
// write(2) (runlog.Append); then SIGKILL to t.PID, and a wait until the process is gone
// (reaped, or a zombie: it holds no socket any more); with restart, the instance started
// again (t.Start, or t.Args as a detached child) and a wait of at most 30 s until GET /health
// shows status ready.
//
// read_ok is true only when the call answered 200 with every reply field and log_queue_len
// 0. The drop counts are the reply's whenever its body decoded, 200 or 503, and 0 otherwise
// (plan choice: read_ok:false already marks them unread). A failed read still kills: the error
// then wraps ErrPrekillUnread.
//
// A t.PID that names no process (kill(2) with signal 0 reports ESRCH) is refused before the
// pre-kill call, which cannot be undone: nothing is called, written, killed or restarted (plan
// choice). A process that ends between that check and the kill is reported, and a restart
// still runs. The wait for readiness fails at once when the restarted process has exited, so a
// dead restart is never waited on for 30 s, nor another listener's ready taken for it.
//
// When ctx has ended before the call, nothing is done. Once the call is sent the instance
// serves no more /hit requests, so the line, the kill and the restart run whatever ctx does,
// each within its own deadline; only the wait for readiness ends with ctx. newPID is the
// restarted process's PID whenever one was started, even with an error, so the caller can
// stop it.
func KillAPI(ctx context.Context, t APITarget, restart bool) (newPID int, err error) {
	if err := t.check(restart); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("fixtures: api kill: %w", err)
	}
	if err := syscall.Kill(t.PID, 0); errors.Is(err, syscall.ESRCH) {
		return 0, fmt.Errorf("%w: process %d: %w; nothing was called or killed", errNotRunning, t.PID, err)
	}
	committed := context.WithoutCancel(ctx) // from the pre-kill call on, the instance must go
	rec, readErr := t.prekill(committed)
	errs := []error{readErr}
	if err := runlog.Append(t.LogPath, rec); err != nil {
		errs = append(errs, fmt.Errorf("fixtures: api kill: the pre_kill line: %w", err))
	}
	if err := killGone(committed, t.PID); err != nil {
		errs = append(errs, err)
		if !errors.Is(err, errNotRunning) {
			return 0, errors.Join(errs...) // it may still run: a restart could not bind its port
		}
	}
	if !restart {
		return 0, errors.Join(errs...)
	}
	newPID, err = t.start(committed)
	if err != nil {
		return newPID, errors.Join(append(errs, err)...)
	}
	if err := t.waitReady(ctx, newPID); err != nil {
		errs = append(errs, err)
	}
	return newPID, errors.Join(errs...)
}

// check refuses a target KillAPI cannot use; restart adds a way to start the instance again.
func (t APITarget) check(restart bool) error {
	u, err := url.Parse(t.BaseURL)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return fmt.Errorf("fixtures: api target base URL %q: want http://host:port", t.BaseURL)
	}
	if err := checkPID(t.PID); err != nil {
		return fmt.Errorf("fixtures: api target: %w", err)
	}
	switch {
	case t.LogPath == "":
		return errors.New("fixtures: api target: no run-log path")
	case t.PrekillTimeout <= 0:
		return fmt.Errorf("fixtures: api target: pre-kill timeout %v: want more than 0", t.PrekillTimeout)
	case t.Client == nil:
		return errors.New("fixtures: api target: no HTTP client")
	case restart && t.Start == nil && len(t.Args) == 0:
		return errors.New("fixtures: api target: no Start and no command line to restart the instance with")
	}
	return nil
}

// endpoint is the target's URL for path.
func (t APITarget) endpoint(path string) string { return strings.TrimSuffix(t.BaseURL, "/") + path }

// prekill makes the pre-kill call within t.PrekillTimeout and builds the pre_kill line from
// its reply. A read that failed returns the line with read_ok false and an error wrapping
// ErrPrekillUnread.
func (t APITarget) prekill(ctx context.Context) (preKillRecord, error) {
	rec := preKillRecord{Kind: "lifecycle", Event: "pre_kill"}
	unread := func(format string, args ...any) (preKillRecord, error) {
		return rec, fmt.Errorf("%w: POST %s: %s", ErrPrekillUnread, prekillPath, fmt.Sprintf(format, args...))
	}
	ctx, cancel := context.WithTimeout(ctx, t.PrekillTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint(prekillPath), http.NoBody)
	if err != nil {
		return unread("%v", err)
	}
	res, err := t.Client.Do(req)
	if err != nil {
		return unread("%v", err)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxReplyBody+1))
	_ = res.Body.Close() // read or failed: the read's error is the one that matters
	switch {
	case err != nil:
		return unread("reading the reply: %v", err)
	case len(body) > maxReplyBody:
		return unread("a reply longer than %d bytes", maxReplyBody)
	}
	reply, err := decodePrekill(body)
	if err != nil {
		return unread("status %d: %v", res.StatusCode, err)
	}
	rec.ListenerDrops, rec.LogDrops = *reply.ListenerDrops, *reply.LogDrops
	switch {
	case res.StatusCode != http.StatusOK:
		return unread("status %d, log_queue_len %d", res.StatusCode, *reply.LogQueueLen)
	case *reply.LogQueueLen != 0:
		return unread("log_queue_len %d: the run log was not drained", *reply.LogQueueLen)
	}
	rec.ReadOK = true
	return rec, nil
}

// decodePrekill decodes a pre-kill reply: one JSON object with exactly listener_drops, log_drops
// and log_queue_len, each a whole number of at least 0.
func decodePrekill(body []byte) (prekillReply, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var r prekillReply
	if err := dec.Decode(&r); err != nil {
		return prekillReply{}, fmt.Errorf("the reply is not the pre-kill object: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return prekillReply{}, errors.New("the reply has more after its object")
	}
	switch {
	case r.ListenerDrops == nil || r.LogDrops == nil || r.LogQueueLen == nil:
		return prekillReply{}, errors.New("the reply lacks listener_drops, log_drops or log_queue_len")
	case *r.LogDrops < 0 || *r.LogQueueLen < 0:
		return prekillReply{}, fmt.Errorf("the reply has log_drops %d and log_queue_len %d: want 0 or more", *r.LogDrops, *r.LogQueueLen)
	}
	return r, nil
}

// killGone sends SIGKILL to pid and waits until the process is gone (processGone), at most
// goneWait. A PID that names no process is errNotRunning.
func killGone(ctx context.Context, pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("%w: SIGKILL to process %d: %w", errNotRunning, pid, err)
		}
		return fmt.Errorf("fixtures: api kill: SIGKILL to process %d: %w", pid, err)
	}
	limit := time.Now().Add(goneWait)
	for {
		gone, err := processGone(ctx, pid)
		if gone {
			return nil
		}
		if !time.Now().Add(gonePoll).Before(limit) {
			if err != nil {
				return fmt.Errorf("fixtures: api kill: process %d still there %v after SIGKILL: %w", pid, goneWait, err)
			}
			return fmt.Errorf("fixtures: api kill: process %d still there %v after SIGKILL", pid, goneWait)
		}
		time.Sleep(gonePoll)
	}
}

// processGone reports whether process pid has exited: no such process (its parent reaped it),
// or ps shows a zombie, which holds no descriptor any more, its listening socket included.
func processGone(ctx context.Context, pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, psWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid))
	cmd.Env = []string{"LC_ALL=C"}
	out, err := cmd.Output()
	state := strings.TrimSpace(string(out))
	var exit *exec.ExitError
	switch {
	case err == nil:
		return strings.HasPrefix(state, "Z"), nil
	case errors.As(err, &exit) && state == "":
		return true, nil // ps found no such process: it was reaped since the kill(2) check
	}
	return false, fmt.Errorf("ps -o stat= -p %d: %w", pid, err)
}

// start starts the instance again: t.Start, or t.Args as a detached child (startDetached).
func (t APITarget) start(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	if t.Start != nil {
		pid, err := t.Start(ctx)
		if err != nil {
			return 0, fmt.Errorf("fixtures: api kill: restarting the instance: %w", err)
		}
		if err := checkPID(pid); err != nil {
			return 0, fmt.Errorf("fixtures: api kill: the restarted instance: %w", err)
		}
		return pid, nil
	}
	pid, err := startDetached(ctx, t.Args, t.Env, t.OutPath)
	if err != nil {
		return 0, fmt.Errorf("fixtures: api kill: restarting the instance: %w", err)
	}
	return pid, nil
}

// waitReady reads GET /health every readyPoll, each read within readyTry, until it answers
// 200 with status ready while the started process pid still runs, at most readyWait or until
// ctx ends. After every read it checks that process (processGone): once it has exited the wait
// fails at once, and a ready answer counts only while the process is known to run.
func (t APITarget) waitReady(ctx context.Context, pid int) error {
	ctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	tick := time.NewTicker(readyPoll)
	defer tick.Stop()
	for {
		err := t.ready(ctx)
		gone, perr := processGone(ctx, pid)
		switch {
		case gone:
			return fmt.Errorf("fixtures: api kill: the restarted process %d has exited", pid)
		case err == nil && perr == nil:
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("fixtures: api kill: the restarted instance was not ready: %w: %w", ctx.Err(), errors.Join(err, perr))
		case <-tick.C:
		}
	}
}

// ready reads GET /health once: nil when it answers 200 with a JSON object whose status is
// ready.
func (t APITarget) ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyTry)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint(healthPath), http.NoBody)
	if err != nil {
		return err
	}
	res, err := t.Client.Do(req)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxReplyBody))
	_ = res.Body.Close() // read or failed: the read's error is the one that matters
	if err != nil {
		return fmt.Errorf("GET %s: reading the reply: %w", healthPath, err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", healthPath, res.StatusCode)
	}
	var h struct {
		Status *string `json:"status"`
	}
	if err := json.Unmarshal(body, &h); err != nil || h.Status == nil {
		return fmt.Errorf("GET %s: no status in the reply", healthPath)
	}
	if *h.Status != statusReady {
		return fmt.Errorf("GET %s: status %q", healthPath, *h.Status)
	}
	return nil
}
