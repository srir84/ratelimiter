package receipt

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// Signer signs and verifies receipts. The limiter uses the HMACSigner; S5's test-only
// WithSignerWrapper wraps it to make signing fail or the signer unusable.
type Signer interface {
	Usable() bool                  // false: acquire returns config_error with no charge (API-1a)
	Sign(Receipt) (Receipt, error) // the receipt with MAC set; an error gives receipt_error (API-12a)
	Verify(Receipt) bool           // MAC check, before any refund is sent (API-2a step 1)
}

// HMACSigner signs with HMAC-SHA256 under a 32-byte key that never leaves the process
// (RCP-4): no method returns it, and every fmt verb prints HMACSigner{key:redacted}. The
// key lives only inside the mac closure, which reflection cannot open, so even a struct
// that holds the signer by value prints no key bytes. The zero value has no key and is not
// usable.
type HMACSigner struct {
	mac func(in []byte) []byte // HMAC-SHA256(key, in)
}

// NewHMACSigner reads the 32-byte key from rnd with io.ReadFull: crypto/rand.Reader in
// production, the test reader of WithKeyReader (§A5.12). A missing, short or failing reader
// gives an error and a nil signer (RCP-4: the instance never becomes ready).
func NewHMACSigner(rnd io.Reader) (*HMACSigner, error) {
	if rnd == nil {
		return nil, errors.New("reading the signing key: no reader")
	}
	var key [32]byte
	if _, err := io.ReadFull(rnd, key[:]); err != nil {
		return nil, fmt.Errorf("reading the signing key: %w", err)
	}
	return &HMACSigner{mac: func(in []byte) []byte {
		m := hmac.New(sha256.New, key[:])
		m.Write(in)
		return m.Sum(nil)
	}}, nil
}

// Usable reports whether the signer holds a key.
func (s *HMACSigner) Usable() bool { return s != nil && s.mac != nil }

// Sign returns r with MAC = HMAC-SHA256(key, MACInput(r)).
func (s *HMACSigner) Sign(r Receipt) (Receipt, error) {
	if !s.Usable() {
		return Receipt{}, ErrUnusable
	}
	in := MACInput(r)
	if in == nil {
		return Receipt{}, ErrUnencodable
	}
	copy(r.MAC[:], s.mac(in))
	return r, nil
}

// Verify reports whether r.MAC is this signer's MAC of r, compared in constant time.
func (s *HMACSigner) Verify(r Receipt) bool {
	if !s.Usable() {
		return false
	}
	in := MACInput(r)
	return in != nil && hmac.Equal(s.mac(in), r.MAC[:])
}

const redacted = "HMACSigner{key:redacted}"

// String prints HMACSigner{key:redacted}, never the key.
func (s HMACSigner) String() string { return redacted }

// GoString prints HMACSigner{key:redacted} for %#v, never the key.
func (s HMACSigner) GoString() string { return redacted }

// Format prints HMACSigner{key:redacted} for every verb, %d and %x included. A failed write
// to the formatter's buffer has nowhere to go, so its error is dropped.
func (s HMACSigner) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }
