package resp_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
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

// cmdLen is the length of args as a RESP array of bulk strings, counted by hand.
func cmdLen(args ...string) int {
	n := len("*") + len(strconv.Itoa(len(args))) + len("\r\n")
	for _, a := range args {
		n += len("$") + len(strconv.Itoa(len(a))) + len("\r\n") + len(a) + len("\r\n")
	}
	return n
}

func dialServer(t *testing.T, srv *fakeresp.Server) net.Conn {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dialing the fake store: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// writeFailConn fails every write, writing nothing.
type writeFailConn struct{ net.Conn }

func (writeFailConn) Write([]byte) (int, error) { return 0, errors.New("write refused (test)") }

// deadlineConn fails every SetDeadline after the first.
type deadlineConn struct {
	net.Conn
	calls int
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.calls++
	if c.calls > 1 {
		return errors.New("deadline refused (test)")
	}
	return c.Conn.SetDeadline(t)
}

func TestHandshake(t *testing.T) {
	pw := newPassword(t)
	srv := fakeresp.Start(t, "rl_lib", pw)

	t.Run("accepted", func(t *testing.T) {
		c := dialServer(t, srv)
		var seen []int
		err := resp.Handshake(c, "rl_lib", pw, time.Now().Add(5*time.Second), func(cmd string, n int) {
			if cmd != "AUTH" {
				t.Errorf("observer heard %q, want AUTH", cmd)
			}
			seen = append(seen, n)
		})
		if err != nil {
			t.Fatalf("Handshake: %v", err)
		}
		auths := 0
		for _, cmd := range srv.Commands() {
			if fakeresp.Label(cmd.Args) != "AUTH" {
				continue
			}
			auths++
			// Compared, never printed: the password stays out of test output.
			if !slices.Equal(cmd.Args, []string{"AUTH", "rl_lib", pw}) {
				t.Errorf("the server saw %s, want AUTH rl_lib <password>", cmd)
			}
		}
		if auths != 1 {
			t.Errorf("the server saw %d AUTH commands, want 1", auths)
		}
		if want := []int{cmdLen("AUTH", "rl_lib", pw)}; !slices.Equal(seen, want) {
			t.Errorf("observer bytes = %v, want %v", seen, want)
		}
		// The connection is usable at once: nothing was left unread.
		if _, err := c.Write(resp.AppendCommand(nil, "PING")); err != nil {
			t.Fatalf("writing PING: %v", err)
		}
		if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("setting a read deadline: %v", err)
		}
		if v, err := resp.NewReader(c).ReadValue(); err != nil || v.Kind != resp.Simple || v.Str != "PONG" {
			t.Errorf("PING after the handshake = %s, %v; want +PONG", v, err)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		c := dialServer(t, srv)
		wrong := newPassword(t)
		err := resp.Handshake(c, "rl_lib", wrong, time.Now().Add(5*time.Second), nil)
		if err == nil {
			t.Fatal("Handshake with a wrong password succeeded")
		}
		if !strings.Contains(err.Error(), "WRONGPASS") {
			t.Errorf("Handshake error does not name the WRONGPASS reply")
		}
		if strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), pw) {
			t.Errorf("Handshake error contains a password")
		}
	})

	t.Run("refusal echoes the password", func(t *testing.T) {
		echo := fakeresp.Start(t, "rl_lib", pw)
		// The refusal repeats the password twice, first at its start, where Value.String's
		// 64-byte clip would keep all of it.
		echo.ReplyError("AUTH", pw+" is not accepted: WRONGPASS for "+pw, 1)
		c := dialServer(t, echo)
		err := resp.Handshake(c, "rl_lib", pw, time.Now().Add(5*time.Second), nil)
		if err == nil {
			t.Fatal("Handshake refused by the store succeeded")
		}
		// Compared, never printed: no 8-byte piece of the password is in the error.
		msg := err.Error()
		for i := 0; i+8 <= len(pw); i++ {
			if strings.Contains(msg, pw[i:i+8]) {
				t.Fatal("Handshake error contains the password, or part of it")
			}
		}
		if !strings.Contains(msg, "is not accepted: WRONGPASS for") {
			t.Errorf("Handshake error lost the rest of the store's refusal")
		}
	})

	t.Run("array refusal echoes the password", func(t *testing.T) {
		c, peer := net.Pipe()
		t.Cleanup(func() { _ = c.Close(); _ = peer.Close() })
		done := make(chan struct{})
		go func() { // a peer that answers AUTH with an array repeating the password, nested too
			defer close(done)
			if _, err := resp.NewReader(peer).ReadValue(); err != nil {
				return
			}
			reply := resp.Value{Kind: resp.Array, Elems: []resp.Value{
				{Kind: resp.Bulk, Str: pw + " is not accepted"},
				{Kind: resp.Array, Elems: []resp.Value{{Kind: resp.Bulk, Str: pw}}},
			}}
			_, _ = peer.Write(resp.AppendValue(nil, reply))
		}()
		err := resp.Handshake(c, "rl_lib", pw, time.Now().Add(5*time.Second), nil)
		<-done
		if err == nil {
			t.Fatal("Handshake refused by the store succeeded")
		}
		// Compared, never printed: no 8-byte piece of the password is in the error.
		msg := err.Error()
		for i := 0; i+8 <= len(pw); i++ {
			if strings.Contains(msg, pw[i:i+8]) {
				t.Fatal("Handshake error contains the password, or part of it")
			}
		}
		if !strings.Contains(msg, "is not accepted") {
			t.Errorf("Handshake error lost the rest of the store's refusal")
		}
	})

	// A peer that answers AUTH with an integer, bare or nested: one whose decimal form holds a
	// decimal password's significant digits (a peer may drop its zeros or its sign) is
	// redacted; any other integer stays visible, as every integer does for a password that is
	// not decimal.
	for _, c := range []struct {
		name, pass string
		reply      resp.Value
		want       string // the whole error
	}{
		{"integer echo", "4815162342", resp.Value{Kind: resp.Int, Int: 4815162342}, `AUTH as rl_lib refused: "<password>"`},
		{"nested integer echo", "4815162342", resp.Value{Kind: resp.Array, Elems: []resp.Value{{Kind: resp.Int, Int: 4815162342}}}, `AUTH as rl_lib refused: ["<password>"]`},
		{"integer echo without zeros", "007", resp.Value{Kind: resp.Int, Int: 7}, `AUTH as rl_lib refused: "<password>"`},
		{"integer echo without a plus sign", "+007", resp.Value{Kind: resp.Int, Int: 7}, `AUTH as rl_lib refused: "<password>"`},
		{"negative integer echo without zeros", "-007", resp.Value{Kind: resp.Int, Int: -7}, `AUTH as rl_lib refused: "<password>"`},
		{"integer echo of an all-zero password", "000", resp.Value{Kind: resp.Int, Int: 0}, `AUTH as rl_lib refused: "<password>"`},
		{"other integer refusal to a zero-padded password", "007", resp.Value{Kind: resp.Int, Int: 42}, `AUTH as rl_lib refused: 42`},
		{"integer refusal to a zero-padded password", "007", resp.Value{Kind: resp.Int, Int: 1}, `AUTH as rl_lib refused: 1`},
		{"integer refusal to an all-zero password", "000", resp.Value{Kind: resp.Int, Int: 1}, `AUTH as rl_lib refused: 1`},
		// A hex password whose last digit is f, so it is never a decimal integer.
		{"integer refusal to a hex password", pw[:len(pw)-1] + "f", resp.Value{Kind: resp.Int, Int: 1}, `AUTH as rl_lib refused: 1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			peer := fakeresp.Start(t, "rl_lib", c.pass)
			peer.ReplyValue("AUTH", c.reply, 1)
			err := resp.Handshake(dialServer(t, peer), "rl_lib", c.pass, time.Now().Add(5*time.Second), nil)
			// Compared, never printed: a wrong error may hold the password.
			if err == nil || err.Error() != c.want {
				t.Errorf("Handshake answered %s: error nil %v, or not %q", c.name, err == nil, c.want)
			}
		})
	}

	// A peer that answers AUTH with bytes that break RESP2: when the reader's reason holds the
	// password, or a decimal password's significant digits (a bulk or array length the peer
	// sent can echo it), the error is still a protocol error but with a fixed reason; any other
	// reason is kept.
	const readErr = "reading the AUTH reply: RESP protocol error: "
	for _, c := range []struct {
		name, pass, raw string
		want            string // the whole error
	}{
		{"bulk length echo", "4815162342", "$4815162342\r\n", readErr + "reason withheld: it may repeat the password"},
		{"array length echo without zeros", "00123456", "*123456\r\n", readErr + "reason withheld: it may repeat the password"},
		{"array length refusal to a zero-padded password", "007", "*123456\r\n", readErr + "array of 123456 over the limit of 100000"},
		{"bulk length refusal to a hex password", pw[:len(pw)-1] + "f", "$4815162342\r\n", readErr + "bulk string of 4815162342 over the limit of 1048576"},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, peer := net.Pipe()
			done := make(chan struct{})
			go func() { // a peer that answers AUTH with c.raw
				defer close(done)
				if _, err := resp.NewReader(peer).ReadValue(); err != nil {
					return
				}
				_, _ = peer.Write([]byte(c.raw))
			}()
			t.Cleanup(func() { _ = conn.Close(); _ = peer.Close(); <-done })
			err := resp.Handshake(conn, "rl_lib", c.pass, time.Now().Add(5*time.Second), nil)
			// Compared, never printed: a wrong error may hold the password.
			var pe *resp.ProtocolError
			if !errors.As(err, &pe) || err.Error() != c.want {
				t.Errorf("Handshake answered %s: a *resp.ProtocolError %v; the error is %q: %v", c.name, pe != nil, c.want, err != nil && err.Error() == c.want)
			}
		})
	}

	t.Run("closed connection", func(t *testing.T) {
		c := dialServer(t, srv)
		if err := c.Close(); err != nil {
			t.Fatalf("closing: %v", err)
		}
		if err := resp.Handshake(c, "rl_lib", pw, time.Now().Add(time.Second), nil); err == nil {
			t.Error("Handshake on a closed connection succeeded")
		}
	})

	t.Run("write fails", func(t *testing.T) {
		c := writeFailConn{Conn: dialServer(t, srv)}
		var seen []int
		err := resp.Handshake(c, "rl_lib", pw, time.Now().Add(time.Second), func(_ string, n int) { seen = append(seen, n) })
		if err == nil {
			t.Error("Handshake whose AUTH write failed succeeded")
		}
		if !slices.Equal(seen, []int{0}) {
			t.Errorf("observer bytes = %v, want [0]", seen)
		}
	})

	t.Run("data after the reply", func(t *testing.T) {
		c, peer := net.Pipe()
		t.Cleanup(func() { _ = c.Close(); _ = peer.Close() })
		done := make(chan struct{})
		go func() { // a peer that answers AUTH and then sends more
			defer close(done)
			if _, err := resp.NewReader(peer).ReadValue(); err == nil {
				_, _ = peer.Write([]byte("+OK\r\n:1\r\n"))
			}
		}()
		err := resp.Handshake(c, "rl_lib", pw, time.Now().Add(5*time.Second), nil)
		<-done
		var pe *resp.ProtocolError
		if !errors.As(err, &pe) {
			t.Errorf("Handshake error = %v, want a *resp.ProtocolError", err)
		}
	})

	t.Run("deadline not cleared", func(t *testing.T) {
		c := &deadlineConn{Conn: dialServer(t, srv)}
		if err := resp.Handshake(c, "rl_lib", pw, time.Now().Add(5*time.Second), nil); err == nil {
			t.Error("Handshake that could not clear its deadline succeeded")
		}
	})

	t.Run("no reply", func(t *testing.T) {
		hang := fakeresp.Start(t, "rl_lib", pw)
		hang.HangAuth()
		c := dialServer(t, hang)
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- resp.Handshake(c, "rl_lib", pw, start.Add(200*time.Millisecond), nil) }()
		var err error
		select {
		case err = <-done:
		case <-time.After(2 * time.Second): // a Handshake that ignores its deadline fails here, not by timeout
			_ = c.Close() // ends the Handshake
			<-done
			t.Fatal("Handshake still waiting 2 s after its 200 ms deadline")
		}
		if err == nil {
			t.Fatal("Handshake without a reply succeeded")
		}
		if el := time.Since(start); el < 200*time.Millisecond || el > 2*time.Second {
			t.Errorf("Handshake gave up after %v, want its 200 ms deadline", el)
		}
		// The read's own error is kept: only a protocol error's reason can repeat the password.
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("Handshake error = %v, want one that wraps the read's deadline error", err)
		}
	})
}

// TestHandshakeReplyError (plan S9 step 1: the store adapter keeps the error reply that refused
// a dial for Diag.StoreError and Health.store_error): an AUTH refused with an error reply gives
// an error wrapping a *resp.ReplyError whose Text is the reply's text without its "-", with the
// password it repeats redacted (WRONGPASS from a real refusal, or a refusal that echoes the
// password); an AUTH answered with any other unexpected reply wraps none.
func TestHandshakeReplyError(t *testing.T) {
	pw := newPassword(t)
	for _, c := range []struct{ name, reply, want string }{
		{"WRONGPASS", "WRONGPASS invalid username-password pair or user is disabled.", "WRONGPASS invalid username-password pair or user is disabled."},
		{"refusal echoes the password", pw + " is not accepted: WRONGPASS for " + pw, "<password> is not accepted: WRONGPASS for <password>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := fakeresp.Start(t, "rl_lib", pw)
			srv.ReplyError("AUTH", c.reply, 1)
			err := resp.Handshake(dialServer(t, srv), "rl_lib", pw, time.Now().Add(5*time.Second), nil)
			var re *resp.ReplyError
			if !errors.As(err, &re) {
				t.Fatal("Handshake refused with an error reply: no *resp.ReplyError in its error")
			}
			for i := 0; i+8 <= len(pw); i++ { // compared, never printed
				if strings.Contains(re.Text, pw[i:i+8]) {
					t.Fatal("ReplyError.Text holds the password, or part of it")
				}
			}
			if re.Text != c.want {
				t.Errorf("ReplyError.Text = %q, want %q", re.Text, c.want)
			}
		})
	}
	t.Run("not an error reply", func(t *testing.T) {
		srv := fakeresp.Start(t, "rl_lib", pw)
		srv.ReplyValue("AUTH", resp.Value{Kind: resp.Simple, Str: "QUEUED"}, 1)
		err := resp.Handshake(dialServer(t, srv), "rl_lib", pw, time.Now().Add(5*time.Second), nil)
		var re *resp.ReplyError
		if err == nil || errors.As(err, &re) {
			t.Errorf("Handshake answered +QUEUED: error %v, wraps a *resp.ReplyError %v; want an error wrapping none", err, re != nil)
		}
	})
}
