package resp

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Handshake authenticates a freshly dialed connection (DESIGN §A5.4): it sends
// AUTH user pass, reports ("AUTH", bytes written) to obs when obs is not nil, and expects
// +OK, all before deadline. Any failure or timeout fails the dial, and nothing that can
// change limiter state was sent (API-22). RESP2 only: no HELLO (§0). On success the
// connection's deadlines are cleared. Errors never repeat pass from what the server sent: a
// refusal that repeats it is redacted (redact), and a reply that breaks RESP2 is reported
// with a fixed reason when the reader's reason holds pass, or a decimal pass's significant
// digits, since a length the server sent can echo it (withhold). The reader's other errors,
// the end of the stream or an I/O error, hold nothing the server sent.
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
		return fmt.Errorf("reading the AUTH reply: %w", withhold(err, pass))
	}
	if v.Kind != Simple || v.Str != "OK" {
		red := redact(v, pass)
		if red.Kind == Error {
			return fmt.Errorf("AUTH as %s refused: %w", user, &ReplyError{Text: red.Str})
		}
		return fmt.Errorf("AUTH as %s refused: %s", user, red)
	}
	if r.br.Buffered() > 0 {
		return protoErr("data after the AUTH reply")
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clearing the handshake deadline: %w", err)
	}
	return nil
}

// ReplyError is the error reply that refused a handshake (Handshake wraps it): Text is the
// reply's text without its leading "-", with the password redacted as in the error, so a
// caller can keep it as the store's error text (plan S9 step 1) without ever holding the
// password. Its Error is the reply as Value.String writes it.
type ReplyError struct{ Text string }

func (e *ReplyError) Error() string { return Value{Kind: Error, Str: e.Text}.String() }

// redacted stands in for the password wherever a reply repeats it.
const redacted = "<password>"

// redact returns v with every occurrence of secret in its strings, nested ones included,
// replaced by "<password>", keeping the rest of the server's text for diagnostics, so a
// reply that echoes the password never carries it into an error (AGENTS.md §1 Secrets). It
// runs before Value.String, whose 64-byte clip could otherwise keep part of the password.
// A string in which the replacement itself re-forms secret is replaced whole. An integer,
// nested ones included, becomes the string "<password>" when its decimal form holds the
// significant digits of a decimal secret (holdsDigits), since a peer may echo such a
// password as an integer without its zeros or sign (007 as :7); any other integer stays, and
// every integer does when secret is not decimal.
func redact(v Value, secret string) Value {
	if secret == "" {
		return v
	}
	if v.Kind == Int && holdsDigits(strconv.FormatInt(v.Int, 10), secret) {
		return Value{Kind: Bulk, Str: redacted}
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

// withheldReason replaces a protocol error's reason that may repeat the password (withhold).
const withheldReason = "reason withheld: it may repeat the password"

// withhold returns err, the failed read of an AUTH reply, unchanged unless it is a
// *ProtocolError whose text holds secret, or the significant digits of a decimal secret
// (holdsDigits): its reason can give a length the peer sent, which can echo the password.
// Such an error becomes a *ProtocolError with withheldReason. The reader's other errors, the
// end of the stream or an I/O error, hold nothing the peer sent.
func withhold(err error, secret string) error {
	var pe *ProtocolError
	if !errors.As(err, &pe) || secret == "" {
		return err
	}
	if text := err.Error(); strings.Contains(text, secret) || holdsDigits(text, secret) {
		return &ProtocolError{Reason: withheldReason}
	}
	return err
}

// holdsDigits reports whether text holds the significant digits of secret when secret is a
// decimal integer as a peer could parse it, ASCII digits after an optional sign: its digits
// without the sign and leading zeros, or "0" when all are zeros. It is false for any other
// secret.
func holdsDigits(text, secret string) bool {
	if secret != "" && (secret[0] == '+' || secret[0] == '-') {
		secret = secret[1:]
	}
	if secret == "" || strings.Trim(secret, "0123456789") != "" {
		return false
	}
	digits := strings.TrimLeft(secret, "0")
	if digits == "" {
		digits = "0"
	}
	return strings.Contains(text, digits)
}
