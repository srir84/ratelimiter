package receipt

import (
	"bytes"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// layoutReceipt has a distinct byte pattern in every field.
func layoutReceipt() Receipt {
	return Receipt{
		EpochID:  [16]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f},
		Enc:      "YWJj",
		G:        0x0102030405060708,
		S:        0x1112131415161718,
		Cost:     1,
		IssueUs:  0x2122232425262728,
		ExpiryUs: 0x3132333435363738,
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMACInputLayout(t *testing.T) {
	// epoch (16) ‖ len(enc) (2) ‖ enc ‖ g (8) ‖ s (8) ‖ cost (4) ‖ issue (8) ‖ expiry (8), big-endian.
	want := mustHex(t, "000102030405060708090a0b0c0d0e0f 0004 59574a6a 0102030405060708 1112131415161718 00000001 2122232425262728 3132333435363738")
	got := MACInput(layoutReceipt())
	if !bytes.Equal(got, want) {
		t.Fatalf("MACInput =\n%x\nwant\n%x", got, want)
	}
	for _, c := range []struct {
		field     string
		off, size int
		want      string
	}{
		{"epoch_id", 0, 16, "000102030405060708090a0b0c0d0e0f"},
		{"len(enc)", 16, 2, "0004"},
		{"enc", 18, 4, "59574a6a"},
		{"g", 22, 8, "0102030405060708"},
		{"s", 30, 8, "1112131415161718"},
		{"cost", 38, 4, "00000001"},
		{"issue_us", 42, 8, "2122232425262728"},
		{"expiry_us", 50, 8, "3132333435363738"},
	} {
		if h := hex.EncodeToString(got[c.off : c.off+c.size]); h != c.want {
			t.Errorf("%s at offset %d = %s, want %s", c.field, c.off, h, c.want)
		}
	}
	for _, n := range []int{0, 2, 342, 65_535} {
		r := layoutReceipt()
		r.Enc = strings.Repeat("A", n)
		in := MACInput(r)
		if len(in) != 54+n {
			t.Errorf("len(MACInput) with a %d-byte enc = %d, want %d", n, len(in), 54+n)
		}
		if n == 342 && !bytes.Equal(in[16:18], []byte{0x01, 0x56}) {
			t.Errorf("length prefix of a 342-byte enc = %x, want 0156", in[16:18])
		}
	}
	// Time 0 is a valid time.
	zero := layoutReceipt()
	zero.IssueUs, zero.ExpiryUs = 0, 0
	if in := MACInput(zero); len(in) != 58 || !bytes.Equal(in[42:], make([]byte, 16)) {
		t.Errorf("MACInput with times 0 = %x, want 58 bytes ending in 16 zero bytes", in)
	}
	// Receipts no acquire issues have no MAC input.
	long, neg, negExp := layoutReceipt(), layoutReceipt(), layoutReceipt()
	long.Enc = strings.Repeat("A", 65_536)
	neg.IssueUs = -1
	negExp.ExpiryUs = -1
	for _, r := range []Receipt{long, neg, negExp} {
		if in := MACInput(r); in != nil {
			t.Errorf("MACInput(enc %d bytes, issue %d, expiry %d) = %d bytes, want nil", len(r.Enc), r.IssueUs, r.ExpiryUs, len(in))
		}
	}
}

func TestNewReceipt(t *testing.T) {
	epoch := [16]byte{0xaa}
	r := New(epoch, "YQ", 3, 7, 1_760_000_000_000_000, 60_000_000)
	want := Receipt{EpochID: epoch, Enc: "YQ", G: 3, S: 7, Cost: 1, IssueUs: 1_760_000_000_000_000, ExpiryUs: 1_760_000_060_000_000}
	if r != want {
		t.Fatalf("New = %+v, want %+v", r, want)
	}
}

func TestDeductionID(t *testing.T) {
	base := Receipt{Enc: "YQ", G: 1, S: 1, Cost: 1, IssueUs: 5, ExpiryUs: 9}
	id := DeductionID(base)
	if !regexp.MustCompile(`^d1_[A-Za-z0-9_-]{22}$`).MatchString(id) {
		t.Errorf("DeductionID = %q, want d1_ and 22 base64url characters", id)
	}
	if id != "d1_P-u-urcOo2n1wNN1mumE6H" {
		t.Errorf("DeductionID(epoch 0…0, YQ, 1, 1) = %q, want d1_P-u-urcOo2n1wNN1mumE6H", id)
	}
	changed := map[string]Receipt{}
	r := base
	r.EpochID[15] = 1
	changed["epoch"] = r
	r = base
	r.Enc = "YR"
	changed["enc"] = r
	r = base
	r.G = 2
	changed["g"] = r
	r = base
	r.S = 2
	changed["s"] = r
	r = base // the bytes between enc and g/s move: "YQ" ‖ g=1 versus "YQA" ‖ …
	r.Enc = "YQA"
	changed["enc length"] = r
	for name, r := range changed {
		if got := DeductionID(r); got == id {
			t.Errorf("changing %s leaves the deduction ID at %s", name, id)
		}
	}
	// The ID names the deduction, not the receipt's times or MAC.
	r = base
	r.IssueUs, r.ExpiryUs, r.Cost, r.MAC[0] = 6, 10, 2, 0xff
	if got := DeductionID(r); got != id {
		t.Errorf("changing issue, expiry, cost and MAC changed the deduction ID to %s", got)
	}
}

func TestParseEpochID(t *testing.T) {
	const s = "000102030405060708090a0b0c0d0eff"
	id, err := ParseEpochID(s)
	want := [16]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0xff}
	if err != nil || id != want {
		t.Fatalf("ParseEpochID(%q) = %x, %v; want %x", s, id, err, want)
	}
	for _, bad := range []string{
		"", "000102030405060708090A0B0C0D0EFF", "000102030405060708090a0b0c0d0eF", "000102030405060708090a0b0c0d0e",
		"000102030405060708090a0b0c0d0eff00", "000102030405060708090a0b0c0d0eg0", " 00102030405060708090a0b0c0d0eff",
		"0x0102030405060708090a0b0c0d0eff",
	} {
		if id, err := ParseEpochID(bad); !errors.Is(err, ErrEpochID) || id != ([16]byte{}) {
			t.Errorf("ParseEpochID(%q) = %x, %v; want ErrEpochID and a zero ID", bad, id, err)
		}
	}
}
