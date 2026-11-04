package fakeresp_test

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/store/resp"
	"github.com/srir84/ratelimiter/internal/testutil/fakeresp"
)

// newPassword returns a fresh store password: 32 random bytes in hex.
func newPassword(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("reading random bytes: %v", err)
	}
	return hex.EncodeToString(b)
}

// client is a raw connection to a fake store that sends one command at a time.
type client struct {
	t *testing.T
	c net.Conn
	r *resp.Reader
}

func dial(t *testing.T, srv *fakeresp.Server) *client {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dialing the fake store: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting the connection's deadline: %v", err)
	}
	return &client{t: t, c: c, r: resp.NewReader(c)}
}

// do sends args and returns the reply.
func (cl *client) do(args ...string) resp.Value {
	cl.t.Helper()
	if _, err := cl.c.Write(resp.AppendCommand(nil, args...)); err != nil {
		cl.t.Fatalf("writing %s: %v", fakeresp.Label(args), err)
	}
	v, err := cl.r.ReadValue()
	if err != nil {
		cl.t.Fatalf("reading the reply to %s: %v", fakeresp.Label(args), err)
	}
	return v
}

// TestAuthControls: the per-command delay and the scripted replies apply to AUTH too (plan
// S3 step 9). A scripted AUTH error answers in place of the password check and does not
// authenticate the connection; a delay holds the AUTH reply back.
func TestAuthControls(t *testing.T) {
	pw := newPassword(t)

	t.Run("scripted error", func(t *testing.T) {
		srv := fakeresp.Start(t, "rl_lib", pw)
		srv.ReplyError("AUTH", "WRONGPASS scripted refusal", 1)
		cl := dial(t, srv)
		if v := cl.do("AUTH", "rl_lib", pw); v.Kind != resp.Error || v.Str != "WRONGPASS scripted refusal" {
			t.Errorf("AUTH with the right password = %s, want the scripted -WRONGPASS scripted refusal", v)
		}
		if v := cl.do("PING"); v.Kind != resp.Error || !strings.HasPrefix(v.Str, "NOAUTH ") {
			t.Errorf("PING after the scripted AUTH error = %s, want -NOAUTH: the connection is not authenticated", v)
		}
		// The scripted reply is used up: the next AUTH is checked, and passes.
		if v := cl.do("AUTH", "rl_lib", pw); v.Kind != resp.Simple || v.Str != "OK" {
			t.Errorf("the second AUTH = %s, want +OK", v)
		}
		if v := cl.do("PING"); v.Kind != resp.Simple || v.Str != "PONG" {
			t.Errorf("PING after the second AUTH = %s, want +PONG", v)
		}
	})

	t.Run("delay", func(t *testing.T) {
		srv := fakeresp.Start(t, "rl_lib", pw)
		srv.Delay("AUTH", 200*time.Millisecond)
		cl := dial(t, srv)
		start := time.Now()
		v := cl.do("AUTH", "rl_lib", pw)
		if el := time.Since(start); el < 200*time.Millisecond {
			t.Errorf("AUTH answered after %v, want its delay of 200 ms or more", el)
		}
		if v.Kind != resp.Simple || v.Str != "OK" {
			t.Errorf("the delayed AUTH = %s, want +OK", v)
		}
		if v := cl.do("PING"); v.Kind != resp.Simple || v.Str != "PONG" {
			t.Errorf("PING after the delayed AUTH = %s, want +PONG", v)
		}
	})
}
