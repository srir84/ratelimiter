package resp

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// Handshake authenticates a freshly dialed connection (DESIGN §A5.4): it sends
// AUTH user pass, reports ("AUTH", bytes written) to obs when obs is not nil, and expects
// +OK, all before deadline. Any failure or timeout fails the dial, and nothing that can
// change limiter state was sent (API-22). RESP2 only: no HELLO (§0). On success the
// connection's deadlines are cleared. Errors never contain pass, even when the server's
// refusal repeats it (redact).
func Handshake(c net.Conn, user, pass string, deadline time.Time, obs func(cmd string, n int)) error {
	if err := c.SetDeadline(deadline); err != nil {
		return fmt.Errorf("setting the handshake deadline: %w", err)
	}
	buf := AppendCommand(nil, "AUTH", user, pass)
	n, err := c.Write(buf)
	clear(buf) // the buffer holds the password
	if obs != nil {
		obs("AUTH", n)
	}
	if err != nil {
		return fmt.Errorf("sending AUTH: %w", err)
	}
	r := NewReader(c)
	v, err := r.ReadValue()
	if err != nil {
		return fmt.Errorf("reading the AUTH reply: %w", err)
	}
	if v.Kind != Simple || v.Str != "OK" {
		return fmt.Errorf("AUTH as %s refused: %s", user, redact(v, pass))
	}
	if r.br.Buffered() > 0 {
		return protoErr("data after the AUTH reply")
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clearing the handshake deadline: %w", err)
	}
	return nil
}

// redacted stands in for the password wherever a reply repeats it.
const redacted = "<password>"

// redact returns v with every occurrence of secret in its strings, nested ones included,
// replaced by "<password>", keeping the rest of the server's text for diagnostics, so a
// reply that echoes the password never carries it into an error (AGENTS.md §1 Secrets). It
// runs before Value.String, whose 64-byte clip could otherwise keep part of the password.
// A string in which the replacement itself re-forms secret is replaced whole.
func redact(v Value, secret string) Value {
	if secret == "" {
		return v
	}
	v.Str = strings.ReplaceAll(v.Str, secret, redacted)
	if strings.Contains(v.Str, secret) {
		v.Str = redacted
	}
	if v.Elems != nil {
		elems := make([]Value, len(v.Elems))
		for i, e := range v.Elems {
			elems[i] = redact(e, secret)
		}
		v.Elems = elems
	}
	return v
}
