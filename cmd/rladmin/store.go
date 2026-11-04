package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/store/resp"
)

// conn is rladmin's one connection to the store (DESIGN §A6; plan S8 step 3: no adapter):
// RESP2 over TCP, authenticated with AUTH, one command at a time. After an I/O or protocol
// error the stream position is unknown, so the command ends there (exit 3) and sends nothing
// more.
type conn struct {
	nc net.Conn
	r  *resp.Reader
}

// dial connects to addr and authenticates as user, all within ctx's deadline; cancelling ctx
// ends the wait at once, and dial then reports ctx's error. Errors never contain pass
// (resp.Handshake redacts the server's text).
func dial(ctx context.Context, addr, user, pass string) (*conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("connecting to the store: no deadline")
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to the store at %s: %w", addr, err)
	}
	// Cancelling ctx closes the connection, which ends any wait on it for good. A past
	// deadline would not do: resp.Handshake sets its own deadline first, which replaces one
	// that a cancel just before had set.
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	err = resp.Handshake(nc, user, pass, deadline, nil)
	stop()
	if err = ioCause(ctx, err); err != nil { // ctx ended: its error, not the closed connection's
		_ = nc.Close() // the connection is unusable either way; a second close changes nothing
		return nil, fmt.Errorf("authenticating to the store at %s: %w", addr, err)
	}
	return &conn{nc: nc, r: resp.NewReader(nc)}, nil
}

// close closes the connection; nothing rladmin sent waits on it any more.
func (c *conn) close() { _ = c.nc.Close() }

// do sends one command and reads its reply within ctx's deadline; cancelling ctx (a signal)
// ends the wait at once. label names the command in errors. An error reply is a reply, not
// an error.
func (c *conn) do(ctx context.Context, label string, args ...string) (resp.Value, error) {
	if err := ctx.Err(); err != nil {
		return resp.Value{}, fmt.Errorf("%s: %w", label, err)
	}
	deadline, _ := ctx.Deadline()
	if err := c.nc.SetDeadline(deadline); err != nil {
		return resp.Value{}, fmt.Errorf("%s: setting the deadline: %w", label, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.nc.SetDeadline(time.Now()) })
	defer stop()
	if _, err := c.nc.Write(resp.AppendCommand(nil, args...)); err != nil {
		return resp.Value{}, fmt.Errorf("%s: sending: %w", label, ioCause(ctx, err))
	}
	v, err := c.r.ReadValue()
	if err != nil {
		return resp.Value{}, fmt.Errorf("%s: reading the reply: %w", label, ioCause(ctx, err))
	}
	return v, nil
}

// ioCause is ctx's error when ctx has ended (its deadline or a signal), else err: an I/O
// error that ctx's end caused then reads as that end.
func ioCause(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// script runs s with EVALSHA and returns its reply as it came, error replies included. After
// NOSCRIPT (store.NoScriptReply), which proves the script never ran, it sends one SCRIPT LOAD
// and resends EVALSHA exactly once (AGENTS.md §6; plan S8 step 3); a second NOSCRIPT is
// returned like any other error reply. A SCRIPT LOAD error reply is returned in place of the
// script's reply; a load that confirms another SHA-1 is a *replyError.
func (c *conn) script(ctx context.Context, s scripts.Script, keys, args []string) (resp.Value, error) {
	cmd := make([]string, 0, 3+len(keys)+len(args))
	cmd = append(cmd, "EVALSHA", s.SHA, strconv.Itoa(len(keys)))
	cmd = append(cmd, keys...)
	cmd = append(cmd, args...)
	v, err := c.do(ctx, s.Name, cmd...)
	if err != nil || v.Kind != resp.Error || !store.NoScriptReply(v.Str) {
		return v, err
	}
	lv, err := c.do(ctx, "SCRIPT LOAD "+s.Name, "SCRIPT", "LOAD", s.Text)
	switch {
	case err != nil:
		return resp.Value{}, err
	case lv.Kind == resp.Error:
		return lv, nil
	case lv.Kind != resp.Bulk || lv.Null || lv.Str != s.SHA:
		return resp.Value{}, &replyError{what: "SCRIPT LOAD of " + s.Name + " did not confirm its SHA-1", raw: lv}
	}
	return c.do(ctx, s.Name, cmd...)
}

// replyError is a store reply rladmin cannot use: a store problem (exit 3), printed with the
// raw reply (plan S8 step 11).
type replyError struct {
	what string
	raw  resp.Value
}

func (e *replyError) Error() string { return e.what }
