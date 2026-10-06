// Package resp is the limiter's minimal RESP2 client (DESIGN §0 store client row, §A1):
// command encoding, a bounded reply reader and the AUTH handshake. It speaks RESP2 only
// and never sends HELLO. The pipelined connections built on it live in internal/store.
package resp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Reader limits (plan choice, IMPLEMENTATION_PLAN S3 step 2). A reply that breaks one is a
// ProtocolError, so a broken or hostile stream cannot make the reader allocate without bound.
const (
	// MaxBulk is the largest bulk string the reader accepts, in bytes (1 MiB).
	MaxBulk = 1 << 20
	// MaxArray is the most elements one array may have.
	MaxArray = 100_000
	// MaxDepth is the deepest array nesting the reader accepts: a top-level array is depth 1.
	MaxDepth = 4
	// lineBuf is the read buffer; a simple string, error or length line longer than it is a
	// ProtocolError.
	lineBuf = 64 << 10
	// preallocMax caps the capacity reserved up front for an array's elements, so an
	// announced length allocates only as elements actually arrive.
	preallocMax = 64
)

// Kind is the RESP2 type of a Value. The zero Kind is not a valid type: a zero Value is
// never mistaken for a reply.
type Kind int

const (
	Simple Kind = iota + 1 // +OK
	Error                  // -ERR message: an error reply, not a Go error
	Int                    // :123
	Bulk                   // $3 abc, or the null bulk string $-1
	Array                  // *2 …, or the null array *-1
)

// String returns the type's name.
func (k Kind) String() string {
	switch k {
	case Simple:
		return "simple"
	case Error:
		return "error"
	case Int:
		return "integer"
	case Bulk:
		return "bulk"
	case Array:
		return "array"
	}
	return "invalid"
}

// Value is one RESP2 value. Str holds a simple string, an error reply's text or a bulk
// string; Int an integer; Elems an array's elements. Null marks the null bulk string ($-1)
// and the null array (*-1).
type Value struct {
	Kind  Kind
	Str   string
	Int   int64
	Null  bool
	Elems []Value
}

// String formats v compactly for diagnostics (Response.Raw, test messages): at most 16
// elements per array and 64 bytes per string are shown, so the result stays small.
func (v Value) String() string {
	var b strings.Builder
	v.format(&b)
	return b.String()
}

func (v Value) format(b *strings.Builder) {
	const maxElems, maxStr = 16, 64
	switch v.Kind {
	case Simple:
		b.WriteString("+" + clip(v.Str, maxStr))
	case Error:
		b.WriteString("-" + clip(v.Str, maxStr))
	case Int:
		b.WriteString(strconv.FormatInt(v.Int, 10))
	case Bulk:
		if v.Null {
			b.WriteString("nil")
			return
		}
		b.WriteString(strconv.Quote(clip(v.Str, maxStr)))
	case Array:
		if v.Null {
			b.WriteString("*nil")
			return
		}
		b.WriteByte('[')
		for i, e := range v.Elems {
			if i > 0 {
				b.WriteByte(' ')
			}
			if i == maxElems {
				b.WriteString("… " + strconv.Itoa(len(v.Elems)-maxElems) + " more")
				break
			}
			e.format(b)
		}
		b.WriteByte(']')
	default:
		b.WriteString("?")
	}
}

// clip shortens s to at most n bytes, marking a cut with "…".
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:min(n, len(s))] + "…"
}

// AppendCommand appends args to buf as a RESP2 array of bulk strings, the form every
// command takes on the wire, and returns the extended buffer.
func AppendCommand(buf []byte, args ...string) []byte {
	buf = append(buf, '*')
	buf = strconv.AppendInt(buf, int64(len(args)), 10)
	buf = append(buf, '\r', '\n')
	for _, a := range args {
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(a)), 10)
		buf = append(buf, '\r', '\n')
		buf = append(buf, a...)
		buf = append(buf, '\r', '\n')
	}
	return buf
}

// AppendValue appends v to buf in RESP2 form (plan choice: test servers reply with it, and
// FuzzReader round-trips through it). Simple and error strings must not hold CR or LF; a
// Value of no valid Kind appends nothing.
func AppendValue(buf []byte, v Value) []byte {
	switch v.Kind {
	case Simple:
		buf = append(buf, '+')
		buf = append(buf, v.Str...)
	case Error:
		buf = append(buf, '-')
		buf = append(buf, v.Str...)
	case Int:
		buf = append(buf, ':')
		buf = strconv.AppendInt(buf, v.Int, 10)
	case Bulk:
		if v.Null {
			return append(buf, "$-1\r\n"...)
		}
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(v.Str)), 10)
		buf = append(buf, '\r', '\n')
		buf = append(buf, v.Str...)
	case Array:
		if v.Null {
			return append(buf, "*-1\r\n"...)
		}
		buf = append(buf, '*')
		buf = strconv.AppendInt(buf, int64(len(v.Elems)), 10)
		buf = append(buf, '\r', '\n')
		for _, e := range v.Elems {
			buf = AppendValue(buf, e)
		}
		return buf
	default:
		return buf
	}
	return append(buf, '\r', '\n')
}

// ProtocolError reports a reply that breaks RESP2 framing or one of the reader's limits.
// Reason never quotes the stream's bytes.
type ProtocolError struct{ Reason string }

func (e *ProtocolError) Error() string { return "RESP protocol error: " + e.Reason }

func protoErr(format string, args ...any) error {
	return &ProtocolError{Reason: fmt.Sprintf(format, args...)}
}

// Reader reads RESP2 values from a stream through its own buffer.
type Reader struct{ br *bufio.Reader }

// NewReader returns a Reader that reads from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, lineBuf)}
}

// ReadValue reads the next value. It returns io.EOF only when the stream ends cleanly before
// a value starts; a stream that ends inside one returns io.ErrUnexpectedEOF; bad framing or a
// broken limit (MaxBulk, MaxArray, MaxDepth, a line longer than 64 KiB) returns a
// *ProtocolError; any other read error is returned wrapped. After an error the stream
// position is unknown: the caller drops the connection.
func (r *Reader) ReadValue() (Value, error) {
	return r.read(0)
}

// read reads one value inside depth enclosing arrays.
func (r *Reader) read(depth int) (Value, error) {
	line, err := r.line(depth > 0)
	if err != nil {
		return Value{}, err
	}
	if len(line) == 0 {
		return Value{}, protoErr("empty line")
	}
	kind, rest := line[0], line[1:]
	switch kind {
	case '+':
		return Value{Kind: Simple, Str: string(rest)}, nil
	case '-':
		return Value{Kind: Error, Str: string(rest)}, nil
	case ':':
		n, ok := parseInt(rest)
		if !ok {
			return Value{}, protoErr("bad integer")
		}
		return Value{Kind: Int, Int: n}, nil
	case '$':
		n, err := parseLen(rest, MaxBulk, "bulk string")
		if err != nil {
			return Value{}, err
		}
		if n < 0 {
			return Value{Kind: Bulk, Null: true}, nil
		}
		b := make([]byte, n+2)
		if _, err := io.ReadFull(r.br, b); err != nil {
			return Value{}, readErr(err, true)
		}
		if b[n] != '\r' || b[n+1] != '\n' {
			return Value{}, protoErr("bulk string of %d bytes not followed by CRLF", n)
		}
		return Value{Kind: Bulk, Str: string(b[:n])}, nil
	case '*':
		n, err := parseLen(rest, MaxArray, "array")
		if err != nil {
			return Value{}, err
		}
		if depth+1 > MaxDepth { // a null array is an array too
			return Value{}, protoErr("arrays nested deeper than %d", MaxDepth)
		}
		if n < 0 {
			return Value{Kind: Array, Null: true}, nil
		}
		elems := make([]Value, 0, min(n, preallocMax))
		for range n {
			e, err := r.read(depth + 1)
			if err != nil {
				return Value{}, err
			}
			elems = append(elems, e)
		}
		return Value{Kind: Array, Elems: elems}, nil
	}
	return Value{}, protoErr("unknown type byte 0x%02x", kind)
}

// line reads one CRLF-terminated line and returns it without the CRLF. The slice is valid
// only until the next read. inside says a value has already started, so the end of the
// stream is unexpected.
func (r *Reader) line(inside bool) ([]byte, error) {
	b, err := r.br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, protoErr("line longer than %d bytes", lineBuf)
		}
		return nil, readErr(err, inside || len(b) > 0)
	}
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return nil, protoErr("line not terminated by CRLF")
	}
	b = b[:len(b)-2]
	if bytes.IndexByte(b, '\r') >= 0 {
		return nil, protoErr("CR inside a line")
	}
	return b, nil
}

// readErr maps a read error: the end of the stream after a value started is
// io.ErrUnexpectedEOF; other errors are wrapped.
func readErr(err error, started bool) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		if started {
			return io.ErrUnexpectedEOF
		}
		return io.EOF
	}
	return fmt.Errorf("reading a reply: %w", err)
}

// parseInt parses a RESP2 integer: an optional '-', then decimal digits, within int64.
func parseInt(b []byte) (int64, bool) {
	s := string(b)
	digits := strings.TrimPrefix(s, "-")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// parseLen parses a bulk or array length: -1 (null) or 0 … limit.
func parseLen(b []byte, limit int, what string) (int, error) {
	s := string(b)
	digits := strings.TrimPrefix(s, "-")
	if digits == "" || len(digits) > 10 || strings.Trim(digits, "0123456789") != "" {
		return 0, protoErr("bad %s length", what)
	}
	n, err := strconv.Atoi(s)
	switch {
	case err != nil, n < -1:
		return 0, protoErr("bad %s length", what)
	case n > limit:
		return 0, protoErr("%s of %d over the limit of %d", what, n, limit)
	}
	return n, nil
}
