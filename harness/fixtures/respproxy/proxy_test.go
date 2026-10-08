//go:build rltest

package respproxy

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
)

const (
	user = "rl_lib"
	pass = "proxy-test-password"
	// ioWait bounds every read and write of these tests.
	ioWait = 5 * time.Second
)

// start runs a proxy in front of upstream, closed in t.Cleanup.
func start(t *testing.T, upstream string) *Proxy {
	t.Helper()
	p, err := New(t.Context(), upstream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if host, _, err := net.SplitHostPort(p.Addr()); err != nil || host != "127.0.0.1" {
		t.Fatalf("proxy address %q (%v), want 127.0.0.1:<port>", p.Addr(), err)
	}
	return p
}

// client is one raw client connection.
type client struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

// dial connects to addr; t.Cleanup closes the connection.
func dial(t *testing.T, addr string) *client {
	t.Helper()
	c, err := (&net.Dialer{Timeout: ioWait}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &client{t: t, c: c, r: bufio.NewReader(c)}
}

// command is the RESP2 array of bulk strings of args.
func command(args ...string) []byte {
	b := []byte("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b = append(b, "$"+strconv.Itoa(len(a))+"\r\n"+a+"\r\n"...)
	}
	return b
}

// send writes b in one write.
func (c *client) send(b []byte) {
	c.t.Helper()
	if err := c.c.SetWriteDeadline(time.Now().Add(ioWait)); err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.c.Write(b); err != nil {
		c.t.Fatal(err)
	}
}

// replies reads n replies and returns each one's bytes.
func (c *client) replies(n int) []string {
	c.t.Helper()
	if err := c.c.SetReadDeadline(time.Now().Add(ioWait)); err != nil {
		c.t.Fatal(err)
	}
	out := make([]string, 0, n)
	for range n {
		raw, err := readReply(c.r, nil, 0)
		if err != nil {
			c.t.Fatalf("reply %d of %d: %v (so far %q)", len(out)+1, n, err, out)
		}
		out = append(out, string(raw))
	}
	return out
}

// closedWithin fails the test unless the peer closes the connection within d, sending nothing
// more first.
func (c *client) closedWithin(d time.Duration, what string) {
	c.t.Helper()
	if err := c.c.SetReadDeadline(time.Now().Add(d)); err != nil {
		c.t.Fatal(err)
	}
	b, err := c.r.ReadByte()
	if err == nil {
		c.t.Fatalf("%s: read %q, want the connection closed", what, b)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		c.t.Fatalf("%s: the connection is still open after %v", what, d)
	}
}

// silentFor fails the test unless nothing arrives for d and the connection stays open.
func (c *client) silentFor(d time.Duration, what string) {
	c.t.Helper()
	if err := c.c.SetReadDeadline(time.Now().Add(d)); err != nil {
		c.t.Fatal(err)
	}
	b, err := c.r.ReadByte()
	var ne net.Error
	if err == nil || !errors.As(err, &ne) || !ne.Timeout() {
		c.t.Fatalf("%s: read %q (%v), want nothing for %v on an open connection", what, b, err, d)
	}
}

// argsOf returns the arguments of every command the fake received, in order.
func argsOf(srv *fakeresp.Server) [][]string {
	var out [][]string
	for _, c := range srv.Commands() {
		out = append(out, c.Args)
	}
	return out
}

// mixed is a reply with every RESP2 type: a nested array holding a bulk string, an integer, a
// null bulk string, a simple string, an error, an empty bulk string, a null array and an empty
// array.
func mixed(_, _ []string) resp.Value {
	return resp.Value{Kind: resp.Array, Elems: []resp.Value{
		{Kind: resp.Bulk, Str: "bulk\r\nwith CRLF"},
		{Kind: resp.Int, Int: -42},
		{Kind: resp.Bulk, Null: true},
		{Kind: resp.Array, Elems: []resp.Value{
			{Kind: resp.Simple, Str: "OK"},
			{Kind: resp.Error, Str: "ERR inner"},
			{Kind: resp.Bulk, Str: ""},
			{Kind: resp.Array, Null: true},
			{Kind: resp.Array},
		}},
	}}
}

// TestProxyPassesBytesUnchanged (plan S9 step 5): the same pipeline, written once straight to a
// fake store and once through the proxy, gets byte-identical replies (every RESP2 type,
// nested arrays, CRLF inside a bulk string, a NOSCRIPT and an unknown-command error), and the
// store receives the same commands from both connections.
func TestProxyPassesBytesUnchanged(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	sha := srv.Load(scripts.Acquire.Text)
	srv.Handle(sha, mixed)
	p := start(t, srv.Addr())
	big := strings.Repeat("x", 200_000) // longer than a bufio buffer
	pipeline := slices.Concat(
		command("AUTH", user, pass),
		command("PING"),
		command("EVALSHA", sha, "1", "rl:k", "arg"),
		command("SCRIPT", "LOAD", big),
		command("EVALSHA", strings.Repeat("0", 40), "0"),
		command("NOSUCH", "x"),
	)
	const n = 6
	direct := dial(t, srv.Addr())
	direct.send(pipeline)
	want := direct.replies(n)
	sentDirect := argsOf(srv)
	proxied := dial(t, p.Addr())
	proxied.send(pipeline)
	got := proxied.replies(n)
	if !slices.Equal(got, want) {
		t.Errorf("replies through the proxy:\n%q\nwant the store's own:\n%q", got, want)
	}
	if want[2][0] != '*' || !strings.HasPrefix(want[4], "-NOSCRIPT") || !strings.HasPrefix(want[5], "-ERR unknown command") {
		t.Fatalf("the fake's replies %q are not the mix the test needs", want)
	}
	all := argsOf(srv)
	if fromProxy := all[len(sentDirect):]; !slices.EqualFunc(fromProxy, sentDirect, slices.Equal[[]string]) {
		t.Errorf("the store received %q through the proxy, want %q as sent directly", fromProxy, sentDirect)
	}
}

// TestProxyCompleteCommandNotHeldBack (plan S9 step 5: the proxy passes everything through): a
// complete command that arrives in one write with the first bytes of the next one goes on to
// the store at once. Its reply comes back while the second command is still incomplete, which
// the store has not received; once the rest arrives it is answered too. A client that closes
// right after such a write still has its complete command reach the store, as it would have
// sent straight to the store.
func TestProxyCompleteCommandNotHeldBack(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	p := start(t, srv.Addr())
	ping := command("PING")
	const cut = 3 // the bytes of a PING sent with the complete command: "*1\r"
	c := dial(t, p.Addr())
	c.send(slices.Concat(command("AUTH", user, pass), ping[:cut]))
	if got := c.replies(1); got[0] != "+OK\r\n" {
		t.Errorf("AUTH sent with the first %d bytes of a PING: %q, want +OK", cut, got)
	}
	if n := srv.Count("PING"); n != 0 {
		t.Errorf("the store received %d PING while the PING was incomplete, want 0", n)
	}
	c.send(ping[cut:])
	if got := c.replies(1); got[0] != "+PONG\r\n" {
		t.Errorf("the PING once complete: %q, want +PONG", got)
	}

	closing := dial(t, p.Addr())
	closing.send(command("AUTH", user, pass))
	if got := closing.replies(1); got[0] != "+OK\r\n" {
		t.Fatalf("AUTH: %q", got)
	}
	closing.send(slices.Concat(ping, ping[:cut]))
	if err := closing.c.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(ioWait)
	for srv.Count("PING") != 2 {
		if !time.Now().Before(deadline) {
			t.Fatalf("the store received %d PING in all within %v of a client closing right after a complete PING and part of another, want 2 (that PING too)",
				srv.Count("PING"), ioWait)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestProxyInjectErrorKeepsOrder (plan S9 step 5): with 2 READONLY replies armed for acquire, a
// pipeline AUTH, EVALSHA acquire, PING (answered 100 ms late by the store), EVALSHA acquire,
// EVALSHA acquire, EVALSHA refund gets its replies in command order: the two injected errors
// each in its command's place (the second one waits behind the late PONG), then the store's
// reply to the third acquire and its NOSCRIPT for refund. The store never receives the two
// injected acquires. The FIFO keeps the order on a second pipelined connection too, and once
// the count is used up the next acquire reaches the store.
func TestProxyInjectErrorKeepsOrder(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	srv.Load(scripts.Acquire.Text)
	srv.Handle(scripts.Acquire.SHA, func(_, _ []string) resp.Value { return resp.Value{Kind: resp.Int, Int: 7} })
	srv.Delay("PING", 100*time.Millisecond)
	p := start(t, srv.Addr())
	const line = "READONLY You can't write against a read only replica."
	if err := p.InjectError("acquire", line, 2); err != nil {
		t.Fatal(err)
	}
	acq := command("EVALSHA", scripts.Acquire.SHA, "0")
	c := dial(t, p.Addr())
	c.send(slices.Concat(command("AUTH", user, pass), acq, command("PING"), acq, acq, command("EVALSHA", scripts.Refund.SHA, "0")))
	got := c.replies(6)
	want := []string{"+OK\r\n", "-" + line + "\r\n", "+PONG\r\n", "-" + line + "\r\n", ":7\r\n"}
	if !slices.Equal(got[:5], want) || !strings.HasPrefix(got[5], "-NOSCRIPT") {
		t.Errorf("replies %q, want %q then NOSCRIPT", got, want)
	}
	if n := srv.Count("EVALSHA"); n != 2 {
		t.Errorf("the store received %d EVALSHA, want 2 (the third acquire and refund; the injected ones never reach it)", n)
	}

	if err := p.InjectError("acquire", "ERR first", 1); err != nil {
		t.Fatal(err)
	}
	if err := p.InjectError("acquire", "BUSYKEY second", 1); err != nil {
		t.Fatal(err)
	}
	c2 := dial(t, p.Addr())
	c2.send(slices.Concat(command("AUTH", user, pass), command("PING"), acq, acq, acq))
	got = c2.replies(5)
	want = []string{"+OK\r\n", "+PONG\r\n", "-ERR first\r\n", "-BUSYKEY second\r\n", ":7\r\n"}
	if !slices.Equal(got, want) {
		t.Errorf("second connection: replies %q, want %q (earlier injections first)", got, want)
	}
	if n := srv.Count("EVALSHA"); n != 3 {
		t.Errorf("the store received %d EVALSHA, want 3", n)
	}
}

// TestProxyInjectErrorOnlyItsScript (plan S9 step 5: the next count EVALSHA of that script's
// SHA): with one error armed for acquire and then one for refund, the pipeline AUTH, EVALSHA
// read_epoch, refund, refund, acquire, acquire gets, in order: the store's reply to read_epoch
// (not armed), refund's injected error, the store's reply to the second refund (refund's count
// used up), acquire's injected error and the store's reply to the second acquire. The store
// receives exactly the three EVALSHA not injected, in that order.
func TestProxyInjectErrorOnlyItsScript(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	for _, s := range []struct {
		script scripts.Script
		reply  int64
	}{{scripts.ReadEpoch, 3}, {scripts.Refund, 2}, {scripts.Acquire, 1}} {
		srv.Load(s.script.Text)
		srv.Handle(s.script.SHA, func(_, _ []string) resp.Value { return resp.Value{Kind: resp.Int, Int: s.reply} })
	}
	p := start(t, srv.Addr())
	if err := p.InjectError(scripts.Acquire.Name, "ERR for acquire", 1); err != nil {
		t.Fatal(err)
	}
	if err := p.InjectError(scripts.Refund.Name, "ERR for refund", 1); err != nil {
		t.Fatal(err)
	}
	evalsha := func(s scripts.Script) []byte { return command("EVALSHA", s.SHA, "0") }
	c := dial(t, p.Addr())
	c.send(slices.Concat(command("AUTH", user, pass), evalsha(scripts.ReadEpoch), evalsha(scripts.Refund),
		evalsha(scripts.Refund), evalsha(scripts.Acquire), evalsha(scripts.Acquire)))
	got := c.replies(6)
	want := []string{"+OK\r\n", ":3\r\n", "-ERR for refund\r\n", ":2\r\n", "-ERR for acquire\r\n", ":1\r\n"}
	if !slices.Equal(got, want) {
		t.Errorf("replies %q, want %q", got, want)
	}
	var shas []string
	for _, cmd := range srv.Commands() {
		if fakeresp.Label(cmd.Args) == "EVALSHA" {
			shas = append(shas, cmd.Args[1])
		}
	}
	if w := []string{scripts.ReadEpoch.SHA, scripts.Refund.SHA, scripts.Acquire.SHA}; !slices.Equal(shas, w) {
		t.Errorf("the store received EVALSHA of %q, want %q (read_epoch, the second refund, the second acquire)", shas, w)
	}
}

// TestProxyInjectErrorChecks: InjectError refuses an unknown script, a count below 1 and a
// reply that is empty or not one line, and arms nothing then.
func TestProxyInjectErrorChecks(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	srv.Load(scripts.Acquire.Text)
	srv.Handle(scripts.Acquire.SHA, func(_, _ []string) resp.Value { return resp.Value{Kind: resp.Int, Int: 1} })
	p := start(t, srv.Addr())
	for _, c := range []struct {
		script, line string
		count        int
	}{
		{"nosuch", "ERR x", 1},
		{"acquire", "ERR x", 0},
		{"acquire", "ERR x", -1},
		{"acquire", "", 1},
		{"acquire", "ERR a\r\n+OK", 1},
		{"acquire", "ERR a\nb", 1},
	} {
		if err := p.InjectError(c.script, c.line, c.count); err == nil {
			t.Errorf("InjectError(%q, %q, %d) = nil, want an error", c.script, c.line, c.count)
		}
	}
	cl := dial(t, p.Addr())
	cl.send(slices.Concat(command("AUTH", user, pass), command("EVALSHA", scripts.Acquire.SHA, "0")))
	if got := cl.replies(2); got[1] != ":1\r\n" {
		t.Errorf("after the refused calls, acquire got %q, want the store's :1", got[1])
	}
}

// TestProxyInjectErrorCap (AGENTS.md §1 Queues: every queue has a fixed capacity, and a full one
// refuses explicitly): InjectError arms up to InjectCap calls at once, each accepted; one more is
// refused with an error and arms nothing. A pipeline of InjectCap + 1 acquires then gets the
// InjectCap injected errors in arming order, and the last acquire reaches the store, which
// answers it. With the replies used up, InjectError arms again.
func TestProxyInjectErrorCap(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	srv.Load(scripts.Acquire.Text)
	srv.Handle(scripts.Acquire.SHA, func(_, _ []string) resp.Value { return resp.Value{Kind: resp.Int, Int: 7} })
	p := start(t, srv.Addr())
	for k := range InjectCap {
		if err := p.InjectError(scripts.Acquire.Name, "ERR armed "+strconv.Itoa(k+1), 1); err != nil {
			t.Fatalf("InjectError %d of %d: %v, want it armed", k+1, InjectCap, err)
		}
	}
	if err := p.InjectError(scripts.Acquire.Name, "ERR one too many", 1); err == nil {
		t.Fatalf("InjectError %d with %d armed = nil, want an error", InjectCap+1, InjectCap)
	}
	acq := command("EVALSHA", scripts.Acquire.SHA, "0")
	c := dial(t, p.Addr())
	c.send(slices.Concat(command("AUTH", user, pass), bytes.Repeat(acq, InjectCap+1)))
	got := c.replies(InjectCap + 2)
	want := []string{"+OK\r\n"}
	for k := range InjectCap {
		want = append(want, "-ERR armed "+strconv.Itoa(k+1)+"\r\n")
	}
	want = append(want, ":7\r\n") // not "-ERR one too many": the refused call armed nothing
	if !slices.Equal(got, want) {
		t.Errorf("replies %q,\nwant %q", got, want)
	}
	if n := srv.Count("EVALSHA"); n != 1 {
		t.Errorf("the store received %d EVALSHA, want 1 (the one after the %d injected)", n, InjectCap)
	}
	if err := p.InjectError(scripts.Acquire.Name, "ERR again", 1); err != nil {
		t.Errorf("InjectError with every armed reply used up: %v, want it armed", err)
	}
}

// TestProxyHang (plan S9 step 5): Hang closes the open client connection; a connection made
// afterwards connects, and its AUTH gets no reply and never reaches the store, until Close
// closes it.
func TestProxyHang(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	p := start(t, srv.Addr())
	before := dial(t, p.Addr())
	before.send(command("AUTH", user, pass))
	if got := before.replies(1); got[0] != "+OK\r\n" {
		t.Fatalf("AUTH before the hang: %q", got)
	}
	accepted := srv.Accepted()
	p.Hang()
	before.closedWithin(ioWait, "the connection open at Hang")
	after := dial(t, p.Addr())
	after.send(command("AUTH", user, pass))
	after.silentFor(300*time.Millisecond, "a connection made after Hang")
	if n := srv.Accepted(); n != accepted {
		t.Errorf("the store accepted %d connections after Hang, want none (%d before)", n-accepted, accepted)
	}
	p.Close()
	after.closedWithin(ioWait, "the hung connection at Close")
}

// TestProxyStoreDown: when the store cannot be reached, the proxy closes the client
// connection it accepted (as a refused store would end the dial).
func TestProxyStoreDown(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	p := start(t, srv.Addr())
	srv.Stop()
	c := dial(t, p.Addr())
	c.send(command("AUTH", user, pass))
	c.closedWithin(ioWait, "a connection while the store is down")
}

// TestProxyStoreCloses: when the store closes its connection, the client's connection through
// the proxy closes too, at once and without a command in flight, as a direct client would see
// it; a reply the store sent before closing still reaches the client first.
func TestProxyStoreCloses(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	p := start(t, srv.Addr())
	idle := dial(t, p.Addr())
	idle.send(command("AUTH", user, pass))
	if got := idle.replies(1); got[0] != "+OK\r\n" {
		t.Fatalf("AUTH: %q", got)
	}
	srv.DropConns()
	idle.closedWithin(ioWait, "an idle connection whose store connection closed")

	last := dial(t, p.Addr())
	last.send(command("AUTH", user, pass))
	if got := last.replies(1); got[0] != "+OK\r\n" {
		t.Fatalf("AUTH: %q", got)
	}
	srv.After("PING", srv.DropConns)
	last.send(command("PING"))
	if got := last.replies(1); got[0] != "+PONG\r\n" {
		t.Errorf("the reply sent before the store closed: %q, want +PONG", got)
	}
	last.closedWithin(ioWait, "after the store's last reply")
}

// TestProxyFullFIFORefuses (AGENTS.md §1 Queues): while the store answers nothing, a client
// that pipelines more commands than a connection's FIFO holds has its connection closed, and
// Refused counts it; the proxy goes on serving other connections.
func TestProxyFullFIFORefuses(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	p := start(t, srv.Addr())
	c := dial(t, p.Addr())
	c.send(command("AUTH", user, pass))
	if got := c.replies(1); got[0] != "+OK\r\n" {
		t.Fatalf("AUTH: %q", got)
	}
	srv.Stall()
	c.send(bytes.Repeat(command("PING"), PendingCap+10))
	c.closedWithin(ioWait, "a connection past its FIFO's capacity")
	if n := p.Refused(); n != 1 {
		t.Errorf("Refused() = %d, want 1", n)
	}
	other := dial(t, p.Addr())
	other.send(slices.Concat(command("AUTH", user, pass), command("PING")))
	if got := other.replies(2); !slices.Equal(got, []string{"+OK\r\n", "+PONG\r\n"}) {
		t.Errorf("a new connection after the refusal: %q, want +OK and +PONG", got)
	}
}

// TestProxyUnsolicitedRepliesRefused (AGENTS.md §1 Queues): a store that answers a command and
// then sends more replies than the queue of replies holds, with no command waiting, has the
// client's connection closed after the one reply the command got, and Refused counts it.
func TestProxyUnsolicitedRepliesRefused(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	flood := make(chan struct{})
	served := make(chan struct{})
	go func() { // a store that answers its first command, then floods replies on the test's word
		defer close(served)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(ioWait))
		if _, err := c.Read(make([]byte, 64)); err != nil {
			return
		}
		if _, err := c.Write([]byte("+OK\r\n")); err != nil {
			return
		}
		select {
		case <-flood:
		case <-t.Context().Done():
			return
		}
		_, _ = c.Write(bytes.Repeat([]byte("+OK\r\n"), PendingCap+10))
		_, _ = c.Read(make([]byte, 64)) // until the proxy closes the connection
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-served
	})
	p := start(t, ln.Addr().String())
	c := dial(t, p.Addr())
	c.send(command("PING"))
	if got := c.replies(1); got[0] != "+OK\r\n" {
		t.Errorf("the command's reply: %q, want +OK", got)
	}
	close(flood)
	c.closedWithin(ioWait, "a connection whose store sent more replies than commands")
	if n := p.Refused(); n != 1 {
		t.Errorf("Refused() = %d, want 1", n)
	}
}

// TestProxyMalformed: bytes that are not RESP2 within the limits end the connection: an array
// element that is not a bulk string, a null argument, a line without CR, a length past the
// limits.
func TestProxyMalformed(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	p := start(t, srv.Addr())
	for _, raw := range []string{
		"*1\r\n:5\r\n",
		"*1\r\n$-1\r\n",
		"*1\n",
		"*2000000\r\n",
		"*1\r\n$99999999999\r\n",
		"*1\r\n$3\r\nPINGX\r\n",
	} {
		c := dial(t, p.Addr())
		c.send([]byte(raw))
		c.closedWithin(ioWait, "after "+strconv.Quote(raw))
	}
}

// proxyGoroutines returns the stacks of running goroutines that are in this package's code.
func proxyGoroutines() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "respproxy.(*Proxy)") || strings.Contains(g, "respproxy.(*link)") {
			out = append(out, g)
		}
	}
	return out
}

// TestProxyClose: Close ends every goroutine of the proxy, with a proxied connection, a hung
// one and a stalled store; the port then refuses connections, and a second Close does nothing.
func TestProxyClose(t *testing.T) {
	srv := fakeresp.Start(t, user, pass)
	p, err := New(t.Context(), srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	proxied := dial(t, p.Addr())
	proxied.send(command("AUTH", user, pass))
	proxied.replies(1)
	srv.Stall()
	proxied.send(command("PING")) // waits for a reply that never comes
	p.Hang()
	hung := dial(t, p.Addr())
	hung.send(command("AUTH", user, pass))
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Close()
	}()
	select {
	case <-done:
	case <-time.After(ioWait):
		t.Fatalf("Close still running after %v", ioWait)
	}
	if left := proxyGoroutines(); len(left) > 0 {
		t.Errorf("%d goroutines of the proxy after Close:\n%s", len(left), strings.Join(left, "\n\n"))
	}
	if c, err := (&net.Dialer{Timeout: ioWait}).DialContext(t.Context(), "tcp", p.Addr()); err == nil {
		_ = c.Close()
		t.Error("the proxy's port accepts connections after Close")
	}
	p.Close()
	hung.closedWithin(ioWait, "the hung connection after Close")
	proxied.closedWithin(ioWait, "the proxied connection after Close")
}
