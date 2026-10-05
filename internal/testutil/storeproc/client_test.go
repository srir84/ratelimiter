package storeproc

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReplyParse(t *testing.T) {
	cases := []struct {
		name, in string
		want     Reply
	}{
		{"simple string", "+OK\r\n", Reply{Kind: '+', Str: "OK"}},
		{"error reply", "-NOSCRIPT No matching script.\r\n", Reply{Kind: '-', Str: "NOSCRIPT No matching script."}},
		{"integer", ":9007199254740991\r\n", Reply{Kind: ':', Int: 9007199254740991}},
		{"negative integer", ":-7\r\n", Reply{Kind: ':', Int: -7}},
		{"bulk string", "$5\r\nhello\r\n", Reply{Kind: '$', Str: "hello"}},
		{"bulk string with CRLF inside", "$4\r\na\r\nb\r\n", Reply{Kind: '$', Str: "a\r\nb"}},
		{"empty bulk string", "$0\r\n\r\n", Reply{Kind: '$', Str: ""}},
		{"nil bulk string", "$-1\r\n", Reply{Kind: '$', Nil: true}},
		{"nil array", "*-1\r\n", Reply{Kind: '*', Nil: true}},
		{"empty array", "*0\r\n", Reply{Kind: '*', Arr: []Reply{}}},
		{"nested arrays", "*3\r\n:1\r\n*2\r\n$1\r\na\r\n$-1\r\n*1\r\n*-1\r\n", Reply{Kind: '*', Arr: []Reply{
			{Kind: ':', Int: 1},
			{Kind: '*', Arr: []Reply{{Kind: '$', Str: "a"}, {Kind: '$', Nil: true}}},
			{Kind: '*', Arr: []Reply{{Kind: '*', Nil: true}}},
		}}},
		{"error inside an array", "*2\r\n-ERR boom\r\n+OK\r\n", Reply{Kind: '*', Arr: []Reply{{Kind: '-', Str: "ERR boom"}, {Kind: '+', Str: "OK"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(c.in + "+NEXT\r\n"))
			got, err := ReadReply(r)
			if err != nil {
				t.Fatalf("ReadReply(%q): %v", c.in, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ReadReply(%q) = %#v, want %#v", c.in, got, c.want)
			}
			// The reply ends exactly at its terminator: the next one reads cleanly.
			if next, err := ReadReply(r); err != nil || next.Str != "NEXT" {
				t.Fatalf("after %q: next reply %v, %v; want +NEXT", c.in, next, err)
			}
		})
	}

	bad := []struct{ name, in string }{
		{"empty input", ""},
		{"empty line", "\r\n"},
		{"unknown type", "!3\r\n"},
		{"LF without CR", "+OK\n"},
		{"bad integer", ":12x\r\n"},
		{"bulk length below -1", "$-2\r\n"},
		{"bulk length not a number", "$abc\r\n"},
		{"bulk too long", "$600000000\r\n"},
		{"bulk without terminator", "$2\r\nabcd"},
		{"array length below -1", "*-3\r\n"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadReply(bufio.NewReader(strings.NewReader(c.in)))
			if err == nil {
				t.Fatalf("ReadReply(%q) = %v, want an error", c.in, got)
			}
		})
	}

	// A stream that ends once a reply has started is io.ErrUnexpectedEOF, never io.EOF,
	// which only marks the end of the stream between replies (the end of an AOF file).
	truncated := []struct{ name, in string }{
		{"unterminated line", "+OK"},
		{"bulk length without payload", "$5\r\n"},
		{"truncated bulk", "$5\r\nhel"},
		{"bulk without its CRLF", "$5\r\nhello"},
		{"array header only", "*1\r\n"},
		{"array ending at an element boundary", "*2\r\n:1\r\n"},
		{"nested array header only", "*2\r\n*1\r\n"},
		{"AOF entry cut after an argument", "*4\r\n$4\r\nHSET\r\n"},
		{"AOF entry cut inside an argument length", "*4\r\n$4\r\nHSET\r\n$1"},
	}
	for _, c := range truncated {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadReply(bufio.NewReader(strings.NewReader(c.in)))
			if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				t.Fatalf("ReadReply(%q) = %v, %v; want io.ErrUnexpectedEOF, not io.EOF", c.in, got, err)
			}
		})
	}
	t.Run("clean EOF", func(t *testing.T) {
		if _, err := ReadReply(bufio.NewReader(strings.NewReader(""))); !errors.Is(err, io.EOF) {
			t.Fatalf("ReadReply on no input: %v, want io.EOF (the end of an AOF file)", err)
		}
		r := bufio.NewReader(strings.NewReader("*1\r\n$5\r\nMULTI\r\n"))
		if _, err := ReadReply(r); err != nil {
			t.Fatalf("a whole AOF entry: %v", err)
		}
		if _, err := ReadReply(r); !errors.Is(err, io.EOF) {
			t.Fatalf("after the last whole entry: %v, want io.EOF", err)
		}
	})
	t.Run("nesting bound", func(t *testing.T) {
		deep := strings.Repeat("*1\r\n", maxDepth+1) + ":1\r\n"
		if _, err := ReadReply(bufio.NewReader(strings.NewReader(deep))); err == nil {
			t.Fatalf("arrays nested %d deep parsed; want an error", maxDepth+1)
		}
	})
}

func TestAppendCommand(t *testing.T) {
	got := string(appendCommand(nil, []string{"EVALSHA", "", "a\r\nb"}))
	want := "*3\r\n$7\r\nEVALSHA\r\n$0\r\n\r\n$4\r\na\r\nb\r\n"
	if got != want {
		t.Fatalf("appendCommand = %q, want %q", got, want)
	}
}

// TestDial checks Dial(addr, user, pw), the plan's signature (S1 step 3), against a
// one-connection fake server: AUTH user pw is the first command, a refusal is an error
// that does not show the password, and no I/O runs past the deadline.
func TestDial(t *testing.T) {
	const pw = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Run("AUTH accepted", func(t *testing.T) {
		addr, got := fakeServer(t, "+OK\r\n")
		c, err := Dial(addr, "rl_ops", pw)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		_ = c.Close() // nothing was sent after AUTH
		if cmd := <-got; len(cmd) != 3 || cmd[0] != "AUTH" || cmd[1] != "rl_ops" || cmd[2] != pw {
			t.Fatalf("the server got %d arguments; want AUTH rl_ops <password>", len(cmd))
		}
	})
	t.Run("AUTH refused", func(t *testing.T) {
		addr, _ := fakeServer(t, "-WRONGPASS invalid username-password pair or user is disabled.\r\n")
		c, err := Dial(addr, "rl_ops", pw)
		if err == nil {
			_ = c.Close()
			t.Fatal("Dial succeeded after a WRONGPASS reply")
		}
		if msg := err.Error(); strings.Contains(msg, pw) || !strings.Contains(msg, "WRONGPASS") || !strings.Contains(msg, "rl_ops") {
			t.Fatal("the error must name the user and the refusal, never the password")
		}
	})
	t.Run("no user sends no AUTH", func(t *testing.T) {
		addr, got := fakeServer(t, "+PONG\r\n")
		c, err := Dial(addr, "", "")
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = c.Close() }() // the exchange is over
		if rep, err := c.Do("PING"); err != nil || rep.Str != "PONG" {
			t.Fatalf("PING: %v, %v", rep, err)
		}
		if cmd := <-got; len(cmd) != 1 || cmd[0] != "PING" {
			t.Fatalf("the first command was %q, want PING", cmd)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		addr, _ := fakeServer(t, "") // never answers
		t0 := time.Now()
		c, err := dial(addr, "rl_ops", pw, t0.Add(100*time.Millisecond))
		if err == nil {
			_ = c.Close()
			t.Fatal("dial succeeded without an AUTH reply")
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("dial: %v, want a deadline error", err)
		}
		if d := time.Since(t0); d > 5*time.Second {
			t.Fatalf("dial returned after %v, want about 100 ms", d)
		}
	})
}

// fakeServer accepts one connection, reads one command, sends its arguments on the
// returned channel and answers with reply. With reply "" it never answers and waits
// for the client to hang up.
func fakeServer(t *testing.T, reply string) (string, <-chan []string) {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer func() { _ = conn.Close() }() // the exchange is over either way
		var cmd []string
		if rep, err := ReadReply(bufio.NewReader(conn)); err == nil {
			for _, a := range rep.Arr {
				cmd = append(cmd, a.Str)
			}
		}
		got <- cmd
		if reply == "" {
			_, _ = io.Copy(io.Discard, conn) // until the client hangs up
			return
		}
		_, _ = conn.Write([]byte(reply)) // a failed write shows as the client's error
	}()
	t.Cleanup(func() {
		_ = l.Close() // ends an Accept still waiting
		<-done
	})
	return l.Addr().String(), got
}
