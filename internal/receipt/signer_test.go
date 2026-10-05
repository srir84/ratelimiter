package receipt

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// keyBytes returns 32 bytes counting up from first.
func keyBytes(first byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = first + byte(i)
	}
	return k
}

func newSigner(t *testing.T, key []byte) *HMACSigner {
	t.Helper()
	s, err := NewHMACSigner(bytes.NewReader(key))
	if err != nil {
		t.Fatalf("NewHMACSigner: %v", err)
	}
	return s
}

func TestSignVerify(t *testing.T) {
	s := newSigner(t, keyBytes(0))
	var _ Signer = s
	if !s.Usable() {
		t.Fatal("a signer with a key is not usable")
	}
	// HMAC-SHA256 with the key 00 01 … 1f, computed outside this package.
	for _, c := range []struct {
		name string
		r    Receipt
		mac  string
	}{
		{"layout", layoutReceipt(), "fd88c58e8e1e61e08b4c5f5e2e227b7d9310ca6d4d6fb74977fe792587160542"},
		{"acquire", New([16]byte{}, "YQ", 1, 1, 1_760_000_000_000_000, 60_000_000), "9d9d38653017750735a89dabafd0b1a157eafd733a67809f8482e8fd30a1bdfa"},
	} {
		in := c.r
		in.MAC[0] = 0x55 // the MAC field is not part of its own input
		signed, err := s.Sign(in)
		if err != nil {
			t.Fatalf("%s: Sign: %v", c.name, err)
		}
		if got := hex.EncodeToString(signed.MAC[:]); got != c.mac {
			t.Errorf("%s: MAC = %s, want %s", c.name, got, c.mac)
		}
		unsigned := signed
		unsigned.MAC = c.r.MAC
		if unsigned != c.r {
			t.Errorf("%s: Sign changed fields besides MAC: %+v", c.name, signed)
		}
		if !s.Verify(signed) {
			t.Fatalf("%s: Verify(signed receipt) = false", c.name)
		}
		if s.Verify(in) {
			t.Errorf("%s: Verify(unsigned receipt) = true", c.name)
		}
	}

	// A receipt MACInput cannot encode is neither signed nor verified.
	r := layoutReceipt()
	r.Enc = strings.Repeat("A", 65_536)
	if _, err := s.Sign(r); !errors.Is(err, ErrUnencodable) {
		t.Errorf("Sign(65,536-byte enc) = %v, want ErrUnencodable", err)
	}
	if s.Verify(r) {
		t.Error("Verify(65,536-byte enc) = true")
	}
	// A signer without a key signs nothing and verifies nothing.
	for _, u := range []*HMACSigner{nil, {}} {
		if u.Usable() {
			t.Errorf("%v: Usable() = true without a key", u)
		}
		if _, err := u.Sign(layoutReceipt()); !errors.Is(err, ErrUnusable) {
			t.Errorf("Sign without a key = %v, want ErrUnusable", err)
		}
		if u.Verify(layoutReceipt()) {
			t.Error("Verify without a key = true")
		}
	}
}

func TestVerifyRejectsEachFieldChange(t *testing.T) {
	s := newSigner(t, keyBytes(0))
	signed, err := s.Sign(New([16]byte{1, 2, 3}, "YWJj", 4, 5, 1_760_000_000_000_000, 60_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Verify(signed) {
		t.Fatal("Verify(signed) = false")
	}
	cases := []struct {
		name   string
		change func(r *Receipt)
	}{
		{"epoch_id", func(r *Receipt) { r.EpochID[15] ^= 1 }},
		{"enc", func(r *Receipt) { r.Enc = "YWJk" }},
		{"g", func(r *Receipt) { r.G++ }},
		{"s", func(r *Receipt) { r.S++ }},
		{"cost", func(r *Receipt) { r.Cost = 2 }},
		{"issue_us", func(r *Receipt) { r.IssueUs++ }},
		{"expiry_us", func(r *Receipt) { r.ExpiryUs++ }},
		{"one MAC bit", func(r *Receipt) { r.MAC[31] ^= 0x80 }},
	}
	if len(cases) != 8 {
		t.Fatalf("%d cases, want 8 (7 fields and one MAC bit)", len(cases))
	}
	for _, c := range cases {
		r := signed
		c.change(&r)
		if s.Verify(r) {
			t.Errorf("Verify after changing %s = true, want false", c.name)
		}
	}
}

func TestSignersDoNotCrossVerify(t *testing.T) {
	a, b := newSigner(t, keyBytes(0)), newSigner(t, keyBytes(1))
	r1, err := NewHMACSigner(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := NewHMACSigner(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rc := New([16]byte{9}, "YQ", 1, 1, 1_760_000_000_000_000, 60_000_000)
	for _, pair := range [][2]*HMACSigner{{a, b}, {b, a}, {r1, r2}, {r2, r1}, {a, r1}} {
		signed, err := pair[0].Sign(rc)
		if err != nil {
			t.Fatal(err)
		}
		if !pair[0].Verify(signed) {
			t.Error("a signer rejects its own receipt")
		}
		if pair[1].Verify(signed) {
			t.Error("a signer with another key accepts the receipt")
		}
	}
}

func TestNewHMACSignerKeyFailure(t *testing.T) {
	readErr := errors.New("entropy source failed")
	for _, c := range []struct {
		name string
		r    io.Reader
		want error
	}{
		{"error reader", iotest.ErrReader(readErr), readErr},
		{"31-byte reader", bytes.NewReader(keyBytes(0)[:31]), io.ErrUnexpectedEOF},
		{"empty reader", bytes.NewReader(nil), io.EOF},
		{"31 bytes then an error", io.MultiReader(bytes.NewReader(keyBytes(0)[:31]), iotest.ErrReader(readErr)), readErr},
		{"nil reader", nil, nil},
	} {
		s, err := NewHMACSigner(c.r)
		if err == nil || s != nil {
			t.Errorf("%s: NewHMACSigner = %v, %v; want an error and a nil signer", c.name, s, err)
			continue
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Fatalf("%s: error %v does not wrap %v", c.name, err, c.want)
		}
	}
	// io.ReadFull: a reader that hands out one byte at a time still gives the whole key.
	slow, err := NewHMACSigner(iotest.OneByteReader(bytes.NewReader(keyBytes(0))))
	if err != nil {
		t.Fatalf("one-byte reader: %v", err)
	}
	signed, err := slow.Sign(layoutReceipt())
	if err != nil || !newSigner(t, keyBytes(0)).Verify(signed) {
		t.Errorf("one-byte reader: the key differs from the same 32 bytes read at once (%v)", err)
	}
}

func TestSignerNotPrinted(t *testing.T) {
	key := keyBytes(0xa0)
	s := newSigner(t, key)
	secrets := []string{hex.EncodeToString(key), strings.ToUpper(hex.EncodeToString(key)), "160 161 162", "a0 a1 a2", string(key[:8])}
	type holder struct {
		Exported *HMACSigner
		value    HMACSigner
		iface    Signer
	}
	h := holder{Exported: s, value: *s, iface: s}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%X", "%q"} {
		for name, arg := range map[string]any{"pointer": s, "value": *s, "holder": h, "holder pointer": &h, "interface": Signer(s)} {
			out := fmt.Sprintf(verb, arg)
			for _, secret := range secrets {
				if strings.Contains(out, secret) {
					t.Errorf("Sprintf(%s, %s) = %q contains the key", verb, name, out)
				}
			}
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if out := fmt.Sprintf(verb, s); out != "HMACSigner{key:redacted}" {
			t.Errorf("Sprintf(%s, signer) = %q, want HMACSigner{key:redacted}", verb, out)
		}
	}
	if s.String() != "HMACSigner{key:redacted}" || s.GoString() != "HMACSigner{key:redacted}" {
		t.Errorf("String() = %q, GoString() = %q", s.String(), s.GoString())
	}
}
