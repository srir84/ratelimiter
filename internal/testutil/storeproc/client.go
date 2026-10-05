package storeproc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// A test-only RESP2 client (plan choice: S3's internal/store/resp does not exist yet).

const (
	// ioTimeout is the I/O deadline of every Do and Pipeline call, and the dial bound.
	ioTimeout = 30 * time.Second
	// Reply bounds: the store is trusted, but a broken stream must not exhaust memory.
	maxBulk  = 512 << 20
	maxArray = 1 << 24
	maxDepth = 32
	// readBuf is the read buffer; a status or error line longer than this is an error.
	readBuf = 64 << 10
)

// Reply is one RESP2 reply. Kind is its type byte: '+' simple string, '-' error,
// ':' integer, '$' bulk string, '*' array. An error reply is Kind '-' with its text in
// Str, not a Go error. Nil marks the nil bulk string ($-1) and the nil array (*-1).
type Reply struct {
	Kind byte
	Str  string
	Int  int64
	Arr  []Reply
	Nil  bool
}

// String formats a reply compactly for test messages.
func (r Reply) String() string {
	switch r.Kind {
	case '+', '-':
		return string(r.Kind) + r.Str
	case ':':
		return ":" + strconv.FormatInt(r.Int, 10)
	case '$':
		if r.Nil {
			return "$nil"
		}
		if len(r.Str) > 200 {
			return strconv.Quote(r.Str[:200]) + "…"
		}
		return strconv.Quote(r.Str)
	case '*':
		if r.Nil {
			return "*nil"
		}
		parts := make([]string, 0, min(len(r.Arr), 21))
		for i, e := range r.Arr {
			if i == 20 {
				parts = append(parts, fmt.Sprintf("… %d more", len(r.Arr)-20))
				break
			}
			parts = append(parts, e.String())
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	return fmt.Sprintf("Reply{Kind: %q}", r.Kind)
}

// Client is one connection to the store. It is not safe for concurrent use. After an
// error from Do or Pipeline the stream position is unknown: close the client.
type Client struct {
	conn  net.Conn
	r     *bufio.Reader
	buf   []byte
	limit time.Time // when set, no I/O deadline goes past it
}

// Dial connects to addr and, when user is not empty, authenticates with AUTH user pw.
// Connecting and the AUTH exchange end within the 30 s I/O deadline.
func Dial(addr, user, pw string) (*Client, error) {
	return dial(addr, user, pw, time.Now().Add(ioTimeout))
}

// dial is Dial with an explicit deadline that neither connecting nor AUTH passes.
func dial(addr, user, pw string, deadline time.Time) (*Client, error) {
	d := net.Dialer{Deadline: deadline}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	c := &Client{conn: conn, r: bufio.NewReaderSize(conn, readBuf)}
	if user == "" {
		return c, nil
	}
	c.limit = deadline
	rep, err := c.Do("AUTH", user, pw)
	c.limit = time.Time{}
	if err == nil && (rep.Kind != '+' || rep.Str != "OK") {
		err = fmt.Errorf("refused: %s", rep)
	}
	if err != nil {
		_ = c.conn.Close()
		return nil, fmt.Errorf("AUTH as %s at %s: %w", user, addr, err)
	}
	return c, nil
}

// Do sends one command and reads its reply.
func (c *Client) Do(args ...string) (Reply, error) {
	reps, err := c.Pipeline([][]string{args})
	if err != nil {
		return Reply{}, err
	}
	return reps[0], nil
}

// Pipeline sends every command in one write, then reads one reply per command, in order.
func (c *Client) Pipeline(cmds [][]string) ([]Reply, error) {
	if len(cmds) == 0 {
		return nil, errors.New("empty pipeline")
	}
	c.buf = c.buf[:0]
	for _, args := range cmds {
		if len(args) == 0 {
			return nil, errors.New("empty command")
		}
		c.buf = appendCommand(c.buf, args)
	}
	deadline := time.Now().Add(ioTimeout)
	if !c.limit.IsZero() && c.limit.Before(deadline) {
		deadline = c.limit
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting the I/O deadline: %w", err)
	}
	if _, err := c.conn.Write(c.buf); err != nil {
		return nil, fmt.Errorf("sending %s: %w", cmds[0][0], err)
	}
	reps := make([]Reply, len(cmds))
	for i := range reps {
		rep, err := ReadReply(c.r)
		if err != nil {
			return nil, fmt.Errorf("reading the reply to %s: %w", cmds[i][0], err)
		}
		reps[i] = rep
	}
	return reps, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// appendCommand appends args as a RESP array of bulk strings.
func appendCommand(b []byte, args []string) []byte {
	b = append(b, '*')
	b = strconv.AppendInt(b, int64(len(args)), 10)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = strconv.AppendInt(b, int64(len(a)), 10)
		b = append(b, '\r', '\n')
		b = append(b, a...)
		b = append(b, '\r', '\n')
	}
	return b
}

// ReadReply reads one RESP2 reply. It also reads an AOF file, which is a sequence of
// commands written as RESP arrays. It returns a bare io.EOF only when the stream ends
// before the reply's first byte; a stream that ends inside a reply (a line, a bulk
// payload or an array's elements) is io.ErrUnexpectedEOF, so a truncated last AOF entry
// is an error, not the end of the file.
func ReadReply(r *bufio.Reader) (Reply, error) {
	return readReply(r, 0)
}

func readReply(r *bufio.Reader, depth int) (Reply, error) {
	line, err := readLine(r)
	if err != nil {
		if depth > 0 && errors.Is(err, io.EOF) {
			return Reply{}, io.ErrUnexpectedEOF // inside an array the reply has started
		}
		return Reply{}, err
	}
	if line == "" {
		return Reply{}, errors.New("empty reply line")
	}
	kind, rest := line[0], line[1:]
	switch kind {
	case '+', '-':
		return Reply{Kind: kind, Str: rest}, nil
	case ':':
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return Reply{}, fmt.Errorf("integer reply %q: %w", rest, err)
		}
		return Reply{Kind: ':', Int: n}, nil
	case '$':
		n, err := parseLength(rest, maxBulk)
		if err != nil {
			return Reply{}, err
		}
		if n < 0 {
			return Reply{Kind: '$', Nil: true}, nil
		}
		b := make([]byte, n+2)
		if _, err := io.ReadFull(r, b); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // the length line came, the payload did not
			}
			return Reply{}, fmt.Errorf("bulk string of %d bytes: %w", n, err)
		}
		if b[n] != '\r' || b[n+1] != '\n' {
			return Reply{}, fmt.Errorf("bulk string of %d bytes: missing CRLF terminator", n)
		}
		return Reply{Kind: '$', Str: string(b[:n])}, nil
	case '*':
		n, err := parseLength(rest, maxArray)
		if err != nil {
			return Reply{}, err
		}
		if n < 0 {
			return Reply{Kind: '*', Nil: true}, nil
		}
		if depth >= maxDepth {
			return Reply{}, fmt.Errorf("arrays nested deeper than %d", maxDepth)
		}
		arr := make([]Reply, 0, min(n, 4096))
		for range n {
			e, err := readReply(r, depth+1)
			if err != nil {
				return Reply{}, err
			}
			arr = append(arr, e)
		}
		return Reply{Kind: '*', Arr: arr}, nil
	}
	return Reply{}, fmt.Errorf("unknown reply type %q", kind)
}

// parseLength reads a bulk or array length: -1 (nil) or 0…limit.
func parseLength(s string, limit int) (int, error) {
	n, err := strconv.Atoi(s)
	switch {
	case err != nil:
		return 0, fmt.Errorf("length %q: %w", s, err)
	case n < -1 || n > limit:
		return 0, fmt.Errorf("length %d out of range -1…%d", n, limit)
	}
	return n, nil
}

// readLine reads one CRLF-terminated line and returns it without the CRLF.
func readLine(r *bufio.Reader) (string, error) {
	b, err := r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", fmt.Errorf("reply line longer than %d bytes", readBuf)
		}
		if errors.Is(err, io.EOF) && len(b) > 0 {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return "", errors.New("reply line not terminated by CRLF")
	}
	return string(b[:len(b)-2]), nil
}

// ParseInfo parses INFO output ("name:value" lines; "#" section headers) into a map.
func ParseInfo(text string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[k] = v
		}
	}
	return m
}
