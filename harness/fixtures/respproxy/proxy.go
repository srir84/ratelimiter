//go:build rltest

package respproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ratelimiter/internal/scripts"
)

// Limits (plan choice).
const (
	// PendingCap is the capacity of a connection's FIFO: the commands read from the client
	// whose replies it has not written yet.
	PendingCap = 4096
	// InjectCap is the most InjectError calls armed at once: calls with replies left.
	InjectCap = 64
	// dialWait bounds one dial to the store.
	dialWait = 5 * time.Second
	// acceptPause is the wait after an accept that failed for another reason than Close.
	acceptPause = 5 * time.Millisecond
	// maxLine bounds a RESP line: a simple string, an error, an integer or a length.
	maxLine = 64 << 10
	// maxBulk bounds a bulk string, maxElems an array's elements and maxDepth the nesting of
	// arrays in a reply.
	maxBulk  = 64 << 20
	maxElems = 1 << 20
	maxDepth = 16
)

// errProtocol ends a connection whose bytes are not RESP2 within the limits.
var errProtocol = errors.New("respproxy: not RESP2 within the proxy's limits")

// Proxy is one running proxy. Its methods are safe for concurrent use.
type Proxy struct {
	ln       net.Listener
	upstream string
	ctx      context.Context // the proxy's lifetime: dials to the store end with it
	cancel   context.CancelFunc
	wg       sync.WaitGroup // every goroutine of the proxy

	mu      sync.Mutex
	closed  bool
	hanging bool
	links   map[*link]struct{}    // proxied client connections
	hung    map[net.Conn]struct{} // client connections accepted while hanging
	injects []*injection          // armed error replies, oldest first; at most InjectCap
	refused int                   // connections closed because a queue of theirs was full
}

// injection is one InjectError call with the replies it has left.
type injection struct {
	sha  string
	line string
	left int
}

// link is one proxied client connection and, once dialed, its store connection.
type link struct {
	client net.Conn

	mu       sync.Mutex
	upstream net.Conn
	closed   bool
}

// entry is one command in a connection's FIFO: forwarded (its reply comes from the store) or
// injected (the proxy answers it with line).
type entry struct {
	injected bool
	line     string
}

// New starts a proxy on a free 127.0.0.1 port in front of the store at upstream. Its goroutines
// run until Close; ctx only bounds the listen (the proxy outlives it).
func New(ctx context.Context, upstream string) (*Proxy, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("respproxy: listening: %w", err)
	}
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Proxy{
		ln: ln, upstream: upstream, ctx: life, cancel: cancel,
		links: map[*link]struct{}{}, hung: map[net.Conn]struct{}{},
	}
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

// Addr returns the proxy's host:port.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// InjectError arms an error reply (IMPLEMENTATION_PLAN S9 step 5): the next count EVALSHA
// commands, on any connection, that name the SHA-1 of the script called script ("acquire", …)
// are answered with "-" + line + CRLF instead of being forwarded. Each call arms its own
// count; the replies of earlier calls for the same script come first. line is the reply's
// text without the leading "-", one non-empty line. With InjectCap calls armed already, it
// arms nothing and returns an error (AGENTS.md §1 Queues).
func (p *Proxy) InjectError(script, line string, count int) error {
	s, ok := scripts.ByName(script)
	if !ok {
		return fmt.Errorf("respproxy: no script %q", script)
	}
	if count < 1 {
		return fmt.Errorf("respproxy: injecting %d replies, want at least 1", count)
	}
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return fmt.Errorf("respproxy: the error reply %q is not one non-empty line", line)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.injects) >= InjectCap {
		return fmt.Errorf("respproxy: %d injections armed already, the most at once", InjectCap)
	}
	p.injects = append(p.injects, &injection{sha: s.SHA, line: line, left: count})
	return nil
}

// Hang closes every current client connection; from then on the proxy accepts connections and
// never answers them (IMPLEMENTATION_PLAN S9 step 5): it reads and discards what they send
// until the client or Close closes them.
func (p *Proxy) Hang() {
	p.mu.Lock()
	p.hanging = true
	links := make([]*link, 0, len(p.links))
	for l := range p.links {
		links = append(links, l)
	}
	p.mu.Unlock()
	for _, l := range links {
		l.close()
	}
}

// Refused returns how many client connections the proxy closed because a queue of theirs was
// full: the FIFO of commands, or the queue of the store's replies (more replies than commands).
func (p *Proxy) Refused() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused
}

// Close stops accepting, closes every connection and waits until every goroutine of the proxy
// has returned. Later calls do nothing.
func (p *Proxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	links := make([]*link, 0, len(p.links))
	for l := range p.links {
		links = append(links, l)
	}
	hung := make([]net.Conn, 0, len(p.hung))
	for c := range p.hung {
		hung = append(hung, c)
	}
	p.mu.Unlock()
	p.cancel()
	_ = p.ln.Close() // ends accept; nothing else is waiting on the listener
	for _, l := range links {
		l.close()
	}
	for _, c := range hung {
		_ = c.Close() // ends its hold
	}
	p.wg.Wait()
}

// accept admits connections until the listener closes.
func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			tm := time.NewTimer(acceptPause) // a failure such as too many open files: try again
			select {
			case <-p.ctx.Done():
				tm.Stop()
				return
			case <-tm.C:
			}
			continue
		}
		p.admit(c)
	}
}

// admit starts serving c: held while the proxy hangs, proxied otherwise.
func (p *Proxy) admit(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.closed:
		_ = c.Close() // accepted as Close began
	case p.hanging:
		p.hung[c] = struct{}{}
		p.wg.Add(1)
		go p.hold(c)
	default:
		l := &link{client: c}
		p.links[l] = struct{}{}
		p.wg.Add(1)
		go p.serve(l)
	}
}

// hold keeps a connection accepted while hanging: it reads and discards whatever the client
// sends and writes nothing, until the client or Close closes the connection.
func (p *Proxy) hold(c net.Conn) {
	defer p.wg.Done()
	_, _ = io.Copy(io.Discard, c) // ends with the connection; nothing to report
	_ = c.Close()
	p.mu.Lock()
	delete(p.hung, c)
	p.mu.Unlock()
}

// serve dials the store for l and passes l's traffic both ways until either side ends: this
// goroutine reads the commands (commands), a second one reads the store's replies (readStore)
// and a third writes the replies to the client (replies).
func (p *Proxy) serve(l *link) {
	defer p.wg.Done()
	defer p.forget(l)
	defer l.close()
	ctx, cancel := context.WithTimeout(p.ctx, dialWait)
	u, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.upstream)
	cancel()
	if err != nil {
		return // the store refused or did not answer: the client sees its connection close
	}
	if !l.attach(u) {
		_ = u.Close() // Hang or Close closed the link during the dial
		return
	}
	f := &flow{pending: make(chan entry, PendingCap), store: make(chan []byte, PendingCap+1)}
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		p.readStore(l, u, f)
	}()
	go func() {
		defer p.wg.Done()
		defer l.close()
		p.replies(l, f)
	}()
	p.commands(l, u, f)
	close(f.pending)
}

// flow is what a link's three goroutines share: the FIFO, the store's replies as they arrive,
// and the count of forwarded commands whose replies are not written yet.
type flow struct {
	pending     chan entry  // commands read from the client, in order (the FIFO)
	store       chan []byte // the store's replies, in order, each one whole
	outstanding atomic.Int64
	storeGone   atomic.Bool // the store's side ended (EOF, error, or a protocol fault)
}

// forget drops l from the proxy's links.
func (p *Proxy) forget(l *link) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.links, l)
}

// commands reads l's commands in order. Each one goes into the FIFO; a forwarded one is also
// counted outstanding (before it is queued) and written to the store. The bytes are buffered
// until the reader needs more from the client: before it waits for them, everything buffered
// goes to the store (flushFirst). So a pipeline that arrives whole reaches the store as one
// write, and no complete command waits behind the first bytes of the next one, nor is lost if
// the client then closes. A full FIFO closes the link (refuse).
func (p *Proxy) commands(l *link, u net.Conn, f *flow) {
	w := bufio.NewWriter(u)
	r := bufio.NewReader(flushFirst{w: w, src: l.client})
	var raw []byte
	for {
		var head [2]string
		var replies bool
		var err error
		raw, head, replies, err = readCommand(r, raw[:0])
		if err != nil {
			return
		}
		e := entry{}
		if line, ok := p.take(head); ok {
			e = entry{injected: true, line: line}
		}
		if replies {
			if !e.injected {
				f.outstanding.Add(1)
			}
			select {
			case f.pending <- e:
			default:
				p.refuse(l)
				return
			}
		}
		if !e.injected {
			if _, err := w.Write(raw); err != nil {
				return
			}
		}
	}
}

// flushFirst is the client side as commands reads it: each Read first flushes w, the commands
// written for the store so far, so none of them waits while the reader waits for the client.
// Only commands' goroutine uses it, w included. A failed flush ends the read with its error.
type flushFirst struct {
	w   *bufio.Writer
	src io.Reader
}

func (f flushFirst) Read(b []byte) (int, error) {
	if err := f.w.Flush(); err != nil {
		return 0, fmt.Errorf("writing to the store: %w", err)
	}
	return f.src.Read(b)
}

// readStore reads the store's replies as they arrive and queues each for replies, so it sees
// at once when the store closes its connection, as a client of the store itself would. Then,
// once no forwarded command is waiting for a reply, it closes the link (replies does so after
// writing the last one otherwise), so the client sees its connection close too. A reply with
// no room in the queue (more replies than forwarded commands) closes the link as a refusal.
func (p *Proxy) readStore(l *link, u net.Conn, f *flow) {
	r := bufio.NewReader(u)
	for {
		raw, err := readReply(r, nil, 0)
		if err != nil {
			break
		}
		select {
		case f.store <- raw:
			continue
		default:
			p.refuse(l)
		}
		break
	}
	f.storeGone.Store(true)
	close(f.store)
	if f.outstanding.Load() == 0 {
		l.close()
	}
}

// replies writes one reply per FIFO entry, in order: the store's next reply, byte for byte, for
// a forwarded command, the injected error for an injected one. Each reply is flushed at once,
// so no reply waits behind a later one the store has not sent yet. It returns when the client
// side ends, when a forwarded command's reply can no longer come, or once the store's side has
// ended and every forwarded command has its reply.
func (p *Proxy) replies(l *link, f *flow) {
	w := bufio.NewWriter(l.client)
	for e := range f.pending {
		var raw []byte
		if e.injected {
			raw = []byte("-" + e.line + "\r\n")
		} else {
			var ok bool
			if raw, ok = <-f.store; !ok {
				return
			}
		}
		if _, err := w.Write(raw); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
		if !e.injected && f.outstanding.Add(-1) == 0 && f.storeGone.Load() {
			return
		}
	}
}

// take uses up one armed injection for a command whose first two words are head: an EVALSHA
// of an armed script's SHA-1. It returns the error line to answer with.
func (p *Proxy) take(head [2]string) (string, bool) {
	if !strings.EqualFold(head[0], "EVALSHA") || head[1] == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, in := range p.injects {
		if strings.EqualFold(in.sha, head[1]) {
			if in.left--; in.left == 0 {
				p.injects = slices.Delete(p.injects, i, i+1)
			}
			return in.line, true
		}
	}
	return "", false
}

// refuse counts l and closes it because one of its queues is full, unless l was closed
// already. The count comes first, so a client that sees its connection close sees it counted.
func (p *Proxy) refuse(l *link) {
	l.closeAfter(func() {
		p.mu.Lock()
		p.refused++
		p.mu.Unlock()
	})
}

// attach records l's store connection; it reports false once l is closed.
func (l *link) attach(u net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.upstream = u
	return true
}

// close closes both of l's connections once.
func (l *link) close() { l.closeAfter(nil) }

// closeAfter closes both of l's connections once, running before first when it is not nil.
func (l *link) closeAfter(before func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if before != nil {
		before()
	}
	l.closed = true
	_ = l.client.Close() // nothing more goes either way
	if l.upstream != nil {
		_ = l.upstream.Close()
	}
}

// readCommand appends one client command, byte for byte, to raw and returns raw, the command's
// first two words (for InjectError) and whether the store answers it: a RESP2 array of bulk
// strings, or an inline command (one line). The store answers neither an empty inline line nor
// an array of length 0 or -1.
func readCommand(r *bufio.Reader, raw []byte) ([]byte, [2]string, bool, error) {
	var head [2]string
	b, err := r.Peek(1)
	if err != nil {
		return raw, head, false, fmt.Errorf("reading a command: %w", err)
	}
	if b[0] != '*' {
		var line []byte
		if raw, line, err = readLine(r, raw); err != nil {
			return raw, head, false, err
		}
		words := strings.Fields(string(line))
		copy(head[:], words)
		return raw, head, len(words) > 0, nil
	}
	var line []byte
	if raw, line, err = readLine(r, raw); err != nil {
		return raw, head, false, err
	}
	n, err := length(line[1:], maxElems)
	if err != nil {
		return raw, head, false, err
	}
	for i := range n {
		var arg []byte
		if raw, arg, err = readBulk(r, raw); err != nil {
			return raw, head, false, err
		}
		if i < len(head) {
			head[i] = string(arg)
		}
	}
	return raw, head, n > 0, nil
}

// readReply appends one RESP2 reply, byte for byte, to raw: a simple string, an error, an
// integer, a bulk string (null included) or an array of replies (null included), nested at
// most maxDepth deep.
func readReply(r *bufio.Reader, raw []byte, depth int) ([]byte, error) {
	raw, line, err := readLine(r, raw)
	if err != nil {
		return raw, err
	}
	if len(line) == 0 {
		return raw, errProtocol
	}
	switch line[0] {
	case '+', '-', ':':
		return raw, nil
	case '$':
		n, err := length(line[1:], maxBulk)
		if err != nil || n < 0 {
			return raw, err
		}
		raw, _, err = readN(r, raw, n)
		return raw, err
	case '*':
		n, err := length(line[1:], maxElems)
		if err != nil {
			return raw, err
		}
		if n > 0 && depth >= maxDepth {
			return raw, errProtocol
		}
		for range n {
			if raw, err = readReply(r, raw, depth+1); err != nil {
				return raw, err
			}
		}
		return raw, nil
	}
	return raw, errProtocol
}

// readBulk appends one bulk string of a command ($<n> CRLF, n bytes, CRLF) to raw and returns
// raw and its n bytes.
func readBulk(r *bufio.Reader, raw []byte) ([]byte, []byte, error) {
	raw, line, err := readLine(r, raw)
	if err != nil {
		return raw, nil, err
	}
	if len(line) == 0 || line[0] != '$' {
		return raw, nil, errProtocol
	}
	n, err := length(line[1:], maxBulk)
	if err != nil {
		return raw, nil, err
	}
	if n < 0 {
		return raw, nil, errProtocol // a command's arguments are never null
	}
	return readN(r, raw, n)
}

// readN appends n bytes and the CRLF after them to raw; it returns raw and the n bytes.
func readN(r *bufio.Reader, raw []byte, n int) ([]byte, []byte, error) {
	start := len(raw)
	raw = slices.Grow(raw, n+2)[:start+n+2]
	if _, err := io.ReadFull(r, raw[start:]); err != nil {
		return raw[:start], nil, fmt.Errorf("reading %d bytes: %w", n, err)
	}
	if raw[start+n] != '\r' || raw[start+n+1] != '\n' {
		return raw, nil, errProtocol
	}
	return raw, raw[start : start+n], nil
}

// readLine appends one CRLF-terminated line, the terminator included, to raw and returns raw
// and the line without its CRLF.
func readLine(r *bufio.Reader, raw []byte) ([]byte, []byte, error) {
	start := len(raw)
	for {
		frag, err := r.ReadSlice('\n')
		raw = append(raw, frag...)
		if len(raw)-start > maxLine {
			return raw, nil, errProtocol
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return raw, nil, fmt.Errorf("reading a line: %w", err)
		}
	}
	line := raw[start:]
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return raw, nil, errProtocol
	}
	return raw, line[:len(line)-2], nil
}

// length parses a RESP length: an integer from -1 to limit.
func length(b []byte, limit int) (int, error) {
	n, err := strconv.Atoi(string(b))
	if err != nil || n < -1 || n > limit {
		return 0, errProtocol
	}
	return n, nil
}
