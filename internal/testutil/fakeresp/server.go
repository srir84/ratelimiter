// Package fakeresp is a fake RESP2 store for the L1 tests of internal/store
// (IMPLEMENTATION_PLAN S3 step 9, DESIGN §C1): a TCP server on 127.0.0.1 that answers
// AUTH, PING, SCRIPT LOAD and EVALSHA, with knobs that delay, fail, stall, hold or drop
// replies and connections, and counters of what arrived.
//
// It is a test helper: production code never imports it.
package fakeresp

import (
	"crypto/sha1"
	"encoding/hex"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/store/resp"
)

// holdWait is how long hold mode waits after a connection's first held command for the
// rest to arrive (plan choice).
const holdWait = 2 * time.Second

// Handler answers one EVALSHA of a known script with its reply.
type Handler func(keys, argv []string) resp.Value

// Command is one command the server received.
type Command struct {
	Conn int // the connection's number: 1 for the first one accepted
	Args []string
}

// String formats the command for test messages, with AUTH's password left out.
func (c Command) String() string {
	args := c.Args
	if Label(args) == "AUTH" && len(args) > 2 {
		args = append(slices.Clone(args[:2]), "<password>")
	}
	return "conn " + strconv.Itoa(c.Conn) + ": " + strings.Join(args, " ")
}

// Label names a command the way the send observer does (DESIGN §A5.12): its name in upper
// case, with the subcommand for SCRIPT ("SCRIPT LOAD"). Delays, scripted replies, hooks and
// Count select commands by it.
func Label(args []string) string {
	if len(args) == 0 {
		return ""
	}
	name := strings.ToUpper(args[0])
	if name == "SCRIPT" && len(args) > 1 {
		return name + " " + strings.ToUpper(args[1])
	}
	return name
}

type scripted struct {
	label string
	v     resp.Value
	left  int
}

type hook struct {
	label string
	fn    func()
}

// Server is a running fake store. Its methods are safe for concurrent use.
type Server struct {
	t              testing.TB
	user, password string
	addr           string

	mu       sync.Mutex
	ln       net.Listener // nil while stopped
	conns    map[*conn]struct{}
	accepted int
	received []Command
	scripts  map[string]bool
	handlers map[string]Handler
	delays   map[string]time.Duration
	replies  []scripted
	hooks    []hook
	hangAuth bool
	hold     int

	wg sync.WaitGroup // the accept loop and two goroutines per connection
}

// conn is one accepted connection.
type conn struct {
	id      int
	nc      net.Conn
	stalled atomic.Bool
	done    chan struct{}
	once    sync.Once
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.nc.Close()
	})
}

// reply writes v unless the connection is stalled. A failed write means the connection is
// gone, which its reader notices.
func (c *conn) reply(v resp.Value) {
	if c.stalled.Load() {
		return
	}
	_, _ = c.nc.Write(resp.AppendValue(nil, v))
}

// sleep waits d, or reports false when the connection closes first.
func (c *conn) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.done:
		return false
	}
}

// Start starts a server on a free 127.0.0.1 port that accepts AUTH user password; t's
// cleanup stops it.
func Start(t testing.TB, user, password string) *Server {
	t.Helper()
	s := &Server{
		t:        t,
		user:     user,
		password: password,
		conns:    map[*conn]struct{}{},
		scripts:  map[string]bool{},
		handlers: map[string]Handler{},
		delays:   map[string]time.Duration{},
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeresp: listening: %v", err)
	}
	s.addr = ln.Addr().String()
	s.ln = ln
	s.wg.Add(1)
	go s.accept(ln)
	t.Cleanup(s.Stop)
	return s
}

// Addr returns the server's host:port, the same after Restart.
func (s *Server) Addr() string { return s.addr }

// Load makes a script known, as if it had been loaded earlier, and returns its SHA-1.
func (s *Server) Load(text string) string {
	sha := sha1Hex(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[sha] = true
	return sha
}

// Handle sets the handler that answers EVALSHA of the script with this SHA-1 once the
// script is known (by Load or SCRIPT LOAD); until then EVALSHA gets NOSCRIPT.
func (s *Server) Handle(sha string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[sha] = h
}

// Delay makes the server wait d before answering each command with this label, AUTH
// included (0 removes the delay). A connection answers in order, so the delay holds up the
// commands behind it.
func (s *Server) Delay(label string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays[label] = d
}

// ReplyError answers the next n commands with this label with the error reply text instead
// of running them; for AUTH, instead of checking the password, so the connection stays
// unauthenticated.
func (s *Server) ReplyError(label, text string, n int) {
	s.ReplyValue(label, resp.Value{Kind: resp.Error, Str: text}, n)
}

// ReplyValue answers the next n commands with this label with v instead of running them
// (AUTH: instead of checking the password, which leaves the connection unauthenticated).
func (s *Server) ReplyValue(label string, v resp.Value, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies = append(s.replies, scripted{label: label, v: v, left: n})
}

// After runs fn once, right after the server answers the next command with this label.
func (s *Server) After(label string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, hook{label: label, fn: fn})
}

// Stall makes every connection open now keep reading commands but never answer again.
// Connections accepted later answer normally.
func (s *Server) Stall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.stalled.Store(true)
	}
}

// HoldReplies puts connections that authenticate from now on in hold mode: a connection
// withholds every reply until n commands have arrived on it, then answers them in order and
// later ones at once. If fewer than n have arrived 2 s after the first, each held command
// gets an error reply and the hold ends. 0 turns hold mode off.
func (s *Server) HoldReplies(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = n
}

// HangAuth makes the server read every later AUTH and never answer it.
func (s *Server) HangAuth() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hangAuth = true
}

// DropConns closes every open connection; the server keeps accepting.
func (s *Server) DropConns() {
	for _, c := range s.openConns() {
		c.close()
	}
}

// Stop closes the listener and every connection and waits for the server's goroutines.
// Handlers still running are waited for, so a blocking handler must also end when its
// test's context ends. Stop is safe to call more than once.
func (s *Server) Stop() {
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	s.DropConns()
	s.wg.Wait()
}

// Restart listens again on the same address after Stop.
func (s *Server) Restart() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return
	}
	ln, err := (&net.ListenConfig{}).Listen(s.t.Context(), "tcp", s.addr)
	if err != nil {
		s.t.Fatalf("fakeresp: listening again on %s: %v", s.addr, err)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.accept(ln)
}

// Accepted returns how many connections the server has accepted.
func (s *Server) Accepted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// Commands returns every command received so far, in arrival order (AUTH included).
func (s *Server) Commands() []Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.received)
}

// Count returns how many received commands have this label.
func (s *Server) Count(label string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.received {
		if Label(c.Args) == label {
			n++
		}
	}
	return n
}

func (s *Server) openConns() []*conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		out = append(out, c)
	}
	return out
}

func (s *Server) accept(ln net.Listener) {
	defer s.wg.Done()
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.ln != ln { // stopped meanwhile
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		s.accepted++
		c := &conn{id: s.accepted, nc: nc, done: make(chan struct{})}
		s.conns[c] = struct{}{}
		s.wg.Add(2)
		s.mu.Unlock()
		cmds := make(chan []string)
		go s.read(c, cmds)
		go s.process(c, cmds)
	}
}

// read parses the connection's commands, records each, and hands it to process.
func (s *Server) read(c *conn, cmds chan<- []string) {
	defer s.wg.Done()
	defer close(cmds)
	r := resp.NewReader(c.nc)
	for {
		v, err := r.ReadValue()
		if err != nil {
			c.close()
			return
		}
		args, ok := words(v)
		if !ok {
			c.close()
			return
		}
		s.mu.Lock()
		s.received = append(s.received, Command{Conn: c.id, Args: args})
		s.mu.Unlock()
		select {
		case cmds <- args:
		case <-c.done:
			return
		}
	}
}

// process answers the connection's commands in order: AUTH first, then hold mode or each
// command at once.
func (s *Server) process(c *conn, cmds <-chan []string) {
	defer s.wg.Done()
	defer func() {
		c.close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	authed, hold := false, 0
	var held [][]string
	var timer *time.Timer
	var expired <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case args, ok := <-cmds:
			switch {
			case !ok:
				return
			case !authed:
				authed, hold = s.auth(c, args)
			case hold > 0:
				held = append(held, args)
				if len(held) == 1 {
					timer = time.NewTimer(holdWait)
					expired = timer.C
				}
				if len(held) == hold {
					timer.Stop()
					expired = nil
					for _, h := range held {
						s.answer(c, h)
					}
					held, hold = nil, 0
				}
			default:
				s.answer(c, args)
			}
		case <-expired:
			expired = nil
			for range held {
				c.reply(resp.Value{Kind: resp.Error, Str: "ERR fakeresp: hold of " + strconv.Itoa(hold) + " replies timed out with " + strconv.Itoa(len(held)) + " commands"})
			}
			held, hold = nil, 0
		case <-c.done:
			return
		}
	}
}

// auth answers a command on a connection not yet authenticated; it returns whether the
// connection is now authenticated and, if so, its hold count. Under HangAuth, AUTH gets no
// reply. Otherwise AUTH waits out its Delay, and a scripted reply (ReplyError, ReplyValue)
// answers it in place of the password check, so the connection stays unauthenticated.
func (s *Server) auth(c *conn, args []string) (bool, int) {
	if Label(args) != "AUTH" {
		c.reply(resp.Value{Kind: resp.Error, Str: "NOAUTH Authentication required."})
		return false, 0
	}
	s.mu.Lock()
	hang, d := s.hangAuth, s.delays["AUTH"]
	s.mu.Unlock()
	if hang {
		return false, 0
	}
	if d > 0 && !c.sleep(d) {
		return false, 0
	}
	if v, ok := s.takeScripted("AUTH"); ok {
		c.reply(v)
		return false, 0
	}
	s.mu.Lock()
	hold := s.hold
	ok := len(args) == 3 && args[1] == s.user && args[2] == s.password
	s.mu.Unlock()
	if !ok {
		c.reply(resp.Value{Kind: resp.Error, Str: "WRONGPASS invalid username-password pair or user is disabled."})
		return false, 0
	}
	c.reply(resp.Value{Kind: resp.Simple, Str: "OK"})
	return true, hold
}

// answer runs one command after its delay, or uses its scripted reply, and writes the
// reply; a stalled connection does neither. Then it runs the command's After hook.
func (s *Server) answer(c *conn, args []string) {
	if c.stalled.Load() {
		return
	}
	label := Label(args)
	s.mu.Lock()
	d := s.delays[label]
	s.mu.Unlock()
	if d > 0 && !c.sleep(d) {
		return
	}
	v, ok := s.takeScripted(label)
	if !ok {
		v = s.run(args)
	}
	if c.stalled.Load() {
		return
	}
	c.reply(v)
	if fn := s.takeHook(label); fn != nil {
		fn()
	}
}

func (s *Server) takeScripted(label string) (resp.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.replies {
		if r := &s.replies[i]; r.label == label && r.left > 0 {
			r.left--
			return r.v, true
		}
	}
	return resp.Value{}, false
}

func (s *Server) takeHook(label string) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, h := range s.hooks {
		if h.label == label {
			s.hooks = slices.Delete(s.hooks, i, i+1)
			return h.fn
		}
	}
	return nil
}

// run executes PING, SCRIPT LOAD or EVALSHA; any other command is unknown.
func (s *Server) run(args []string) resp.Value {
	switch Label(args) {
	case "PING":
		return resp.Value{Kind: resp.Simple, Str: "PONG"}
	case "SCRIPT LOAD":
		if len(args) != 3 {
			return resp.Value{Kind: resp.Error, Str: "ERR wrong number of arguments for 'script|load' command"}
		}
		return resp.Value{Kind: resp.Bulk, Str: s.Load(args[2])}
	case "EVALSHA":
		return s.evalsha(args)
	}
	return resp.Value{Kind: resp.Error, Str: "ERR unknown command '" + args[0] + "'"}
}

func (s *Server) evalsha(args []string) resp.Value {
	if len(args) < 3 {
		return resp.Value{Kind: resp.Error, Str: "ERR wrong number of arguments for 'evalsha' command"}
	}
	nkeys, err := strconv.Atoi(args[2])
	if err != nil || nkeys < 0 || nkeys > len(args)-3 {
		return resp.Value{Kind: resp.Error, Str: "ERR Number of keys can't be greater than number of args"}
	}
	s.mu.Lock()
	known, h := s.scripts[args[1]], s.handlers[args[1]]
	s.mu.Unlock()
	switch {
	case !known:
		return resp.Value{Kind: resp.Error, Str: "NOSCRIPT No matching script. Please use EVAL."}
	case h == nil:
		return resp.Value{Kind: resp.Error, Str: "ERR fakeresp: no handler for this script"}
	}
	return h(args[3:3+nkeys], args[3+nkeys:])
}

// words returns a command's words: a non-empty array of bulk strings.
func words(v resp.Value) ([]string, bool) {
	if v.Kind != resp.Array || v.Null || len(v.Elems) == 0 {
		return nil, false
	}
	out := make([]string, len(v.Elems))
	for i, e := range v.Elems {
		if e.Kind != resp.Bulk || e.Null {
			return nil, false
		}
		out[i] = e.Str
	}
	return out, true
}

func sha1Hex(text string) string {
	sum := sha1.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}
