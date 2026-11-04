package resp_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/srir84/ratelimiter/internal/store/resp"
)

// The reader limits of IMPLEMENTATION_PLAN S3 step 2, written out.
const (
	maxBulk  = 1 << 20
	maxArray = 100_000
	maxDepth = 4
)

func TestAppendCommand(t *testing.T) {
	got := resp.AppendCommand(nil, "EVALSHA", "a9993e364706816aba3e25717850c26c9cd0d89d", "2", "k1", "k:2", "", "1760000000000000")
	want := "*7\r\n" +
		"$7\r\nEVALSHA\r\n" +
		"$40\r\na9993e364706816aba3e25717850c26c9cd0d89d\r\n" +
		"$1\r\n2\r\n" +
		"$2\r\nk1\r\n" +
		"$3\r\nk:2\r\n" +
		"$0\r\n\r\n" +
		"$16\r\n1760000000000000\r\n"
	if string(got) != want {
		t.Errorf("AppendCommand = %q, want %q", got, want)
	}
	if got := resp.AppendCommand([]byte("x"), "PING"); string(got) != "x*1\r\n$4\r\nPING\r\n" {
		t.Errorf("AppendCommand onto a buffer = %q, want %q", got, "x*1\r\n$4\r\nPING\r\n")
	}
}

func TestReaderTypes(t *testing.T) {
	bulk := func(s string) resp.Value { return resp.Value{Kind: resp.Bulk, Str: s} }
	integer := func(n int64) resp.Value { return resp.Value{Kind: resp.Int, Int: n} }
	array := func(e ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: e} }
	big := strings.Repeat("b", maxBulk)
	cases := []struct {
		name, in string
		want     resp.Value
	}{
		{"simple", "+OK\r\n", resp.Value{Kind: resp.Simple, Str: "OK"}},
		{"error", "-ERR x\r\n", resp.Value{Kind: resp.Error, Str: "ERR x"}},
		{"integer", ":123\r\n", integer(123)},
		{"negative integer", ":-7\r\n", integer(-7)},
		{"null bulk", "$-1\r\n", resp.Value{Kind: resp.Bulk, Null: true}},
		{"empty bulk", "$0\r\n\r\n", bulk("")},
		{"bulk with CRLF inside", "$4\r\na\r\nb\r\n", bulk("a\r\nb")},
		{"bulk of 1 MiB", "$1048576\r\n" + big + "\r\n", bulk(big)},
		{"null array", "*-1\r\n", resp.Value{Kind: resp.Array, Null: true}},
		{"empty array", "*0\r\n", array()},
		{"nested arrays", "*3\r\n:1\r\n*2\r\n$1\r\na\r\n*1\r\n$-1\r\n+x\r\n",
			array(integer(1), array(bulk("a"), array(resp.Value{Kind: resp.Bulk, Null: true})), resp.Value{Kind: resp.Simple, Str: "x"})},
		{"depth 4", "*1\r\n*1\r\n*1\r\n*1\r\n:9\r\n", array(array(array(array(integer(9)))))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := resp.NewReader(strings.NewReader(tc.in))
			got, err := r.ReadValue()
			if err != nil {
				t.Fatalf("ReadValue: %v", err)
			}
			if !sameValue(got, tc.want) {
				t.Errorf("ReadValue = %s, want %s", got, tc.want)
			}
			if _, err := r.ReadValue(); !errors.Is(err, io.EOF) {
				t.Errorf("after the value: %v, want io.EOF", err)
			}
		})
	}
	t.Run("array of 100,000", func(t *testing.T) {
		in := "*100000\r\n" + strings.Repeat(":1\r\n", maxArray)
		got, err := resp.NewReader(strings.NewReader(in)).ReadValue()
		if err != nil || got.Kind != resp.Array || len(got.Elems) != maxArray {
			t.Fatalf("ReadValue: %d elements, %v; want %d", len(got.Elems), err, maxArray)
		}
	})
	t.Run("stream of values", func(t *testing.T) {
		r := resp.NewReader(strings.NewReader("+OK\r\n:5\r\n$1\r\nz\r\n"))
		for _, want := range []resp.Value{{Kind: resp.Simple, Str: "OK"}, integer(5), bulk("z")} {
			if got, err := r.ReadValue(); err != nil || !sameValue(got, want) {
				t.Fatalf("ReadValue = %s, %v; want %s", got, err, want)
			}
		}
	})
}

func TestReaderRejects(t *testing.T) {
	// Each Reason names the rule the reply broke, so a rejection for the wrong rule fails too.
	cases := []struct{ name, in, reason string }{
		{"bad prefix", "?x\r\n", "unknown type byte 0x3f"},
		{"RESP3 map", "%1\r\n+a\r\n+b\r\n", "unknown type byte 0x25"},
		{"empty line", "\r\n", "empty line"},
		{"missing CRLF: bare LF", "+OK\n", "line not terminated by CRLF"},
		{"missing CRLF: a line that is a bare LF", "\n", "line not terminated by CRLF"},
		{"missing CRLF after a bulk", "$3\r\nabcXY", "bulk string of 3 bytes not followed by CRLF"},
		{"bulk followed by CR and not LF", "$3\r\nabc\rX", "bulk string of 3 bytes not followed by CRLF"},
		{"bulk followed by LF without CR", "$3\r\nabcX\n", "bulk string of 3 bytes not followed by CRLF"},
		{"CR inside a line", "+O\rK\r\n", "CR inside a line"},
		{"CR first in a line", "\r\r\n", "CR inside a line"},
		{"bulk length -2", "$-2\r\n", "bad bulk string length"},
		{"bulk over 1 MiB", "$1048577\r\n", "bulk string of 1048577 over the limit of 1048576"},
		{"bulk length of 10 digits", "$1000000000\r\n", "bulk string of 1000000000 over the limit of 1048576"},
		{"bulk length of 11 digits", "$10000000000\r\n", "bad bulk string length"},
		{"bulk length not a number", "$x\r\n", "bad bulk string length"},
		{"bulk length +3", "$+3\r\nabc\r\n", "bad bulk string length"},
		{"array over 100,000", "*100001\r\n", "array of 100001 over the limit of 100000"},
		{"array length of 10 digits", "*1000000000\r\n", "array of 1000000000 over the limit of 100000"},
		{"array length -2", "*-2\r\n", "bad array length"},
		{"depth 5", "*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n:1\r\n", "arrays nested deeper than 4"},
		{"null array at depth 5", "*1\r\n*1\r\n*1\r\n*1\r\n*-1\r\n", "arrays nested deeper than 4"},
		{"integer with a letter", ":12a\r\n", "bad integer"},
		{"integer with a plus", ":+1\r\n", "bad integer"},
		{"empty integer", ":\r\n", "bad integer"},
		{"integer over int64", ":9223372036854775808\r\n", "bad integer"},
		{"line over 64 KiB", "+" + strings.Repeat("a", 64<<10) + "\r\n", "line longer than 65536 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resp.NewReader(strings.NewReader(tc.in)).ReadValue()
			var pe *resp.ProtocolError
			switch {
			case !errors.As(err, &pe):
				t.Errorf("ReadValue error = %v, want a *resp.ProtocolError", err)
			case pe.Reason != tc.reason:
				t.Errorf("ProtocolError reason %q, want %q", pe.Reason, tc.reason)
			}
		})
	}
	t.Run("read error", func(t *testing.T) {
		_, err := resp.NewReader(failingReader{}).ReadValue()
		var pe *resp.ProtocolError
		if !errors.Is(err, errRead) || errors.As(err, &pe) {
			t.Errorf("ReadValue error = %v, want the reader's own error, wrapped", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		for _, in := range []string{"$3\r\nab", "*2\r\n:1\r\n", "+OK"} {
			if _, err := resp.NewReader(strings.NewReader(in)).ReadValue(); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("ReadValue(%q) error = %v, want io.ErrUnexpectedEOF", in, err)
			}
		}
	})
}

// errRead is the error failingReader returns.
type readError struct{}

func (readError) Error() string { return "read failed (test)" }

var errRead = readError{}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errRead }

func TestValueString(t *testing.T) {
	long := strings.Repeat("x", 70)
	elems := make([]resp.Value, 18)
	for i := range elems {
		elems[i] = resp.Value{Kind: resp.Int, Int: int64(i)}
	}
	for _, c := range []struct {
		v    resp.Value
		want string
	}{
		{resp.Value{Kind: resp.Simple, Str: "OK"}, "+OK"},
		{resp.Value{Kind: resp.Error, Str: "ERR x"}, "-ERR x"},
		{resp.Value{Kind: resp.Int, Int: -12}, "-12"},
		{resp.Value{Kind: resp.Bulk, Str: "a b"}, `"a b"`},
		{resp.Value{Kind: resp.Bulk, Str: strings.Repeat("y", 64)}, `"` + strings.Repeat("y", 64) + `"`},
		{resp.Value{Kind: resp.Bulk, Str: long}, `"` + long[:64] + `…"`},
		{resp.Value{Kind: resp.Bulk, Null: true}, "nil"},
		{resp.Value{Kind: resp.Array, Null: true}, "*nil"},
		{resp.Value{Kind: resp.Array}, "[]"},
		{resp.Value{Kind: resp.Array, Elems: []resp.Value{{Kind: resp.Int, Int: 1}, {Kind: resp.Bulk}, {Kind: resp.Array, Elems: []resp.Value{{Kind: resp.Simple, Str: "s"}}}}},
			`[1 "" [+s]]`},
		{resp.Value{Kind: resp.Array, Elems: elems}, "[0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 … 2 more]"},
		{resp.Value{}, "?"},
	} {
		if got := c.v.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}

func FuzzReader(f *testing.F) {
	for _, seed := range []string{
		"+OK\r\n", "-ERR x\r\n", ":123\r\n", ":-1\r\n", "$-1\r\n", "$0\r\n\r\n", "$3\r\nabc\r\n", "*-1\r\n", "*0\r\n",
		"*2\r\n*1\r\n:1\r\n$1\r\na\r\n", "*1\r\n*1\r\n*1\r\n*1\r\n:9\r\n", "*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n:1\r\n",
		"$-2\r\n", "$1048577\r\n", "*100001\r\n", "+OK\n", "$3\r\nabcXY", "?x\r\n", "+OK\r\n:5\r\n",
		"*7\r\n:1\r\n:1760000000000000\r\n$0\r\n\r\n:0\r\n:5\r\n:1\r\n:1760000060000000\r\n",
		"$3\r\nabc\rX", "$3\r\nabcX\n", "\n", "*1\r\n*1\r\n*1\r\n*1\r\n*-1\r\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := resp.NewReader(bytes.NewReader(data))
		for range 64 {
			v, err := r.ReadValue()
			if err != nil {
				return
			}
			checkLimits(t, v, 0)
			back, err := resp.NewReader(bytes.NewReader(resp.AppendValue(nil, v))).ReadValue()
			if err != nil || !sameValue(back, v) {
				t.Fatalf("round trip of %s gave %s, %v", v, back, err)
			}
		}
	})
}

// checkLimits fails when a value the reader returned breaks a reader limit.
func checkLimits(t *testing.T, v resp.Value, depth int) {
	t.Helper()
	switch v.Kind {
	case resp.Simple, resp.Error, resp.Int:
	case resp.Bulk:
		if len(v.Str) > maxBulk {
			t.Fatalf("bulk of %d bytes", len(v.Str))
		}
	case resp.Array:
		if depth+1 > maxDepth || len(v.Elems) > maxArray {
			t.Fatalf("array of %d elements at depth %d", len(v.Elems), depth+1)
		}
		for _, e := range v.Elems {
			checkLimits(t, e, depth+1)
		}
	default:
		t.Fatalf("value of kind %d", v.Kind)
	}
}

// sameValue compares two values; an empty array equals another however it was built.
func sameValue(a, b resp.Value) bool {
	if a.Kind != b.Kind || a.Str != b.Str || a.Int != b.Int || a.Null != b.Null || len(a.Elems) != len(b.Elems) {
		return false
	}
	for i := range a.Elems {
		if !sameValue(a.Elems[i], b.Elems[i]) {
			return false
		}
	}
	return true
}
