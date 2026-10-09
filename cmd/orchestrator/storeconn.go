package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ratelimiter/internal/store/resp"
)

// The store's ACL user the orchestrator itself uses (DESIGN §A2): rl_ops for CONFIG GET,
// BGREWRITEAOF, INFO persistence and SHUTDOWN. The rltest build's clock driver uses
// rl_fixture (clock_rltest.go).
const (
	// opsTimeout bounds one rl_ops connection that sends a few quick commands: the dial, AUTH
	// and the commands (plan choice).
	opsTimeout = 10 * time.Second
	// rewriteTimeout bounds one forced rewrite: BGREWRITEAOF and the polls of INFO persistence
	// until it ends (plan choice: a §A7-sized AOF rewrites in about a second).
	rewriteTimeout = 5 * time.Minute
	// rewritePoll is the interval between two polls of INFO persistence during a forced
	// rewrite (IMPLEMENTATION_PLAN H3 step 3).
	rewritePoll = 100 * time.Millisecond
	// storeStartWait bounds the wait for a fresh store to answer PING as rl_ops (plan choice).
	storeStartWait = 30 * time.Second
	// storeStartPoll is the interval between two connection attempts while the store starts.
	storeStartPoll = 50 * time.Millisecond
	// storeMaxClients is the maxclients the store must report (DESIGN §A2, §B1): Valkey lowers
	// it by itself when its file limit is too low, so the run checks the value in force.
	storeMaxClients = "4096"
)

// storeSettingKeys are the §A2 profile settings context.json records from CONFIG GET (plan H3
// step 5), maxclients among them.
func storeSettingKeys() []string {
	return []string{
		"appendonly", "appendfsync", "no-appendfsync-on-rewrite", "aof-use-rdb-preamble", "save",
		"maxmemory", "maxmemory-policy", "maxclients", "auto-aof-rewrite-percentage",
		"auto-aof-rewrite-min-size", "bind", "protected-mode", "port", "aclfile",
	}
}

// storeConn is a RESP2 connection to the run's store as one ACL user (DESIGN §A2): the
// orchestrator's rl_ops connections and, in an rltest build, the clock driver's rl_fixture
// one. It sends one command at a time; every exchange ends by the deadline of the ctx it was
// dialed with, and that ctx ending ends a wait at once. After an error the stream position is
// unknown: the caller closes it.
type storeConn struct {
	ctx context.Context
	nc  net.Conn
	r   *resp.Reader
}

// dialStore connects to addr and authenticates as user within ctx, which must have a
// deadline. Errors never hold the password (resp.Handshake redacts the store's reply).
func dialStore(ctx context.Context, addr, user, pass string) (*storeConn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("connecting to the store: no deadline")
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to the store at %s: %w", addr, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Now()) })
	err = resp.Handshake(nc, user, pass, deadline, nil)
	stop()
	if cerr := ctx.Err(); cerr != nil {
		err = cerr
	}
	if err != nil {
		_ = nc.Close() // unusable either way; the handshake error is reported
		return nil, fmt.Errorf("authenticating to the store at %s as %s: %w", addr, user, err)
	}
	return &storeConn{ctx: ctx, nc: nc, r: resp.NewReader(nc)}, nil
}

// do sends one command and returns its reply; an error reply is a reply, not an error.
func (c *storeConn) do(args ...string) (resp.Value, error) {
	if len(args) == 0 {
		return resp.Value{}, errors.New("empty command")
	}
	if err := c.ctx.Err(); err != nil {
		return resp.Value{}, fmt.Errorf("%s: %w", args[0], err)
	}
	deadline, _ := c.ctx.Deadline() // dialStore checked that there is one
	if err := c.nc.SetDeadline(deadline); err != nil {
		return resp.Value{}, fmt.Errorf("%s: setting the deadline: %w", args[0], err)
	}
	stop := context.AfterFunc(c.ctx, func() { _ = c.nc.SetDeadline(time.Now()) })
	defer stop()
	if _, err := c.nc.Write(resp.AppendCommand(nil, args...)); err != nil {
		return resp.Value{}, fmt.Errorf("%s: sending: %w", args[0], c.cause(err))
	}
	v, err := c.r.ReadValue()
	if err != nil {
		return resp.Value{}, fmt.Errorf("%s: reading the reply: %w", args[0], c.cause(err))
	}
	return v, nil
}

// cause is the ctx's error when it ended (that made the I/O fail), else err.
func (c *storeConn) cause(err error) error {
	if cerr := c.ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

func (c *storeConn) close() { _ = c.nc.Close() } // nothing waits on the connection any more

// command sends one command and returns its reply, with an error reply as an error naming
// the command (the replies of the commands rl_ops sends never hold a password).
func (c *storeConn) command(args ...string) (resp.Value, error) {
	v, err := c.do(args...)
	if err != nil {
		return resp.Value{}, err
	}
	if v.Kind == resp.Error {
		return resp.Value{}, fmt.Errorf("%s: the store replied %s", args[0], v)
	}
	return v, nil
}

// storeArgs returns valkey-server's arguments (IMPLEMENTATION_PLAN H3 step 3; DESIGN §A2,
// §B5): the durable profile's file, the port, the run's store directory and ACL file, and
// --auto-aof-rewrite-percentage 0 unless the scenario forces rewrites, which keeps the
// profile's production rewrite settings (§B5 "Forced rewrites"). The paths are absolute:
// Valkey changes into --dir before it reads the ACL file.
func storeArgs(conf string, port int, dir, aclPath string, forcedRewrites bool) []string {
	args := []string{conf, "--port", strconv.Itoa(port), "--dir", dir, "--aclfile", aclPath}
	if !forcedRewrites {
		args = append(args, "--auto-aof-rewrite-percentage", "0")
	}
	return args
}

// pingStore reports whether the store at addr answers PING as rl_ops within ctx.
func pingStore(ctx context.Context, addr, pass string) error {
	c, err := dialStore(ctx, addr, userOps, pass)
	if err != nil {
		return err
	}
	defer c.close()
	v, err := c.command("PING")
	if err != nil {
		return err
	}
	if v.Kind != resp.Simple || v.Str != "PONG" {
		return fmt.Errorf("PING: the store replied %s, want +PONG", v)
	}
	return nil
}

// configGet reads the store's current value of each named setting as rl_ops (CONFIG GET with
// several names: a flat array of name and value pairs), within opsTimeout.
func configGet(ctx context.Context, addr, pass string, names ...string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, opsTimeout)
	defer cancel()
	c, err := dialStore(ctx, addr, userOps, pass)
	if err != nil {
		return nil, err
	}
	defer c.close()
	v, err := c.command(append([]string{"CONFIG", "GET"}, names...)...)
	if err != nil {
		return nil, err
	}
	if v.Kind != resp.Array || len(v.Elems)%2 != 0 {
		return nil, fmt.Errorf("CONFIG GET: the store replied %s, want name and value pairs", v)
	}
	m := make(map[string]string, len(v.Elems)/2)
	for i := 0; i < len(v.Elems); i += 2 {
		k, val := v.Elems[i], v.Elems[i+1]
		if k.Kind != resp.Bulk || val.Kind != resp.Bulk || k.Null || val.Null {
			return nil, fmt.Errorf("CONFIG GET: the store replied %s, want bulk strings", v)
		}
		m[k.Str] = val.Str
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			return nil, fmt.Errorf("CONFIG GET: the store did not report %s", n)
		}
	}
	return m, nil
}

// The forced rewrite's replies and statuses (DESIGN §B5 "Forced rewrites"; plan choice for the
// joined status).
const (
	// rewriteInProgress is the store's error reply to BGREWRITEAOF while a rewrite runs already,
	// such as an automatic one (TRF-S7 keeps the production auto-rewrite settings).
	rewriteInProgress = "ERR Background append only file rewriting already in progress"
	// rewriteJoined is the status of a forced rewrite that found a rewrite running: the window
	// recorded is that rewrite's, under the same load.
	rewriteJoined = "joined"
	// rewriteOK is aof_last_bgrewrite_status after a rewrite that succeeded.
	rewriteOK = "ok"
)

// rewriteResult is one forced rewrite as the store showed it.
type rewriteResult struct {
	startNs, endNs int64  // wall clock: just before BGREWRITEAOF; the poll that saw it end
	status         string // aof_last_bgrewrite_status at the end ("ok", "err"), or "joined"
}

// forceRewrite runs one forced AOF rewrite as rl_ops (IMPLEMENTATION_PLAN H3 step 3; DESIGN
// §B5 "Forced rewrites"): BGREWRITEAOF, then INFO persistence at once and every poll interval
// until aof_rewrite_in_progress:0 (and aof_rewrite_scheduled:0 where the store reports it: a
// rewrite the store has only scheduled has not run yet). A rewrite already running (the
// rewriteInProgress reply) is joined: it is polled the same way and its window recorded with
// the status "joined"; any other error reply fails the rewrite. A rewrite that ends with an
// aof_last_bgrewrite_status other than ok fails too. now reads the wall clock for the record.
// The whole rewrite ends within rewriteTimeout or when ctx ends.
func forceRewrite(ctx context.Context, addr, pass string, poll time.Duration, now func() time.Time) (rewriteResult, error) {
	ctx, cancel := context.WithTimeout(ctx, rewriteTimeout)
	defer cancel()
	c, err := dialStore(ctx, addr, userOps, pass)
	if err != nil {
		return rewriteResult{}, err
	}
	defer c.close()
	res := rewriteResult{startNs: now().UnixNano()}
	v, err := c.do("BGREWRITEAOF")
	if err != nil {
		return res, err
	}
	joined := v.Kind == resp.Error && v.Str == rewriteInProgress
	if v.Kind == resp.Error && !joined {
		return res, fmt.Errorf("BGREWRITEAOF: the store replied %s", v)
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		info, err := persistenceInfo(c)
		if err != nil {
			return res, err
		}
		if info["aof_rewrite_in_progress"] == "0" && info["aof_rewrite_scheduled"] != "1" {
			last := info["aof_last_bgrewrite_status"]
			res.endNs, res.status = now().UnixNano(), last
			if joined {
				res.status = rewriteJoined
			}
			if last != rewriteOK {
				return res, fmt.Errorf("the rewrite ended with aof_last_bgrewrite_status %q", last)
			}
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, fmt.Errorf("waiting for the rewrite to end: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// persistenceInfo reads INFO persistence on c: its key:value fields, which must include
// aof_rewrite_in_progress as 0 or 1.
func persistenceInfo(c *storeConn) (map[string]string, error) {
	v, err := c.command("INFO", "persistence")
	if err != nil {
		return nil, err
	}
	if v.Kind != resp.Bulk || v.Null {
		return nil, fmt.Errorf("INFO persistence: the store replied %s, want a bulk string", v)
	}
	m := map[string]string{}
	for _, line := range strings.Split(v.Str, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if k, val, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, "#") {
			m[k] = val
		}
	}
	if p := m["aof_rewrite_in_progress"]; p != "0" && p != "1" {
		return nil, fmt.Errorf("INFO persistence: aof_rewrite_in_progress %q, want 0 or 1", p)
	}
	return m, nil
}

// shutdownStore sends SHUTDOWN as rl_ops (IMPLEMENTATION_PLAN H3 step 3: the AOF is kept). A
// store that shuts down closes the connection without a reply; an error reply means it
// refused.
func shutdownStore(ctx context.Context, addr, pass string) error {
	ctx, cancel := context.WithTimeout(ctx, opsTimeout)
	defer cancel()
	c, err := dialStore(ctx, addr, userOps, pass)
	if err != nil {
		return err
	}
	defer c.close()
	v, err := c.do("SHUTDOWN")
	switch {
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET):
		return nil // the store closed the connection as it shut down
	case err != nil:
		return err
	case v.Kind == resp.Error:
		return fmt.Errorf("SHUTDOWN: the store replied %s", v)
	}
	return fmt.Errorf("SHUTDOWN: the store replied %s and kept running", v)
}
