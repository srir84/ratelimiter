package receipt

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math"
)

// Receipt is a receipt in memory (§A5.9, RCP-1). Every field except MAC is authenticated.
type Receipt struct {
	EpochID  [16]byte
	Enc      string // the budget key's encoding (internal/keys)
	G, S     uint64 // bucket generation and sequence
	Cost     uint32 // always 1
	IssueUs  int64  // budget time of the acquire, µs
	ExpiryUs int64  // IssueUs + receipt lifetime, µs
	MAC      [32]byte
}

// Error is an error of this package; its values are constants, compared with errors.Is.
type Error string

func (e Error) Error() string { return string(e) }

const (
	// ErrEpochID: an epoch ID that is not 32 lowercase hex characters.
	ErrEpochID Error = "epoch ID must be 32 lowercase hex characters"
	// ErrUnencodable: a receipt MACInput cannot encode (an Enc over 65,535 bytes or a
	// negative time).
	ErrUnencodable Error = "receipt cannot be encoded"
	// ErrUnusable: a signer without a key.
	ErrUnusable Error = "signer has no key"
)

// New returns the unsigned receipt of a confirmed deduction: cost 1, issued at issueUs
// (the acquire's t_us) and expiring lifetimeUs later (RCP-1). Within §A3.1's bounds
// (t_us < 2^53 − 2^43, lifetime_us < 2^40) the sum is exact.
func New(epochID [16]byte, enc string, g, s uint64, issueUs, lifetimeUs int64) Receipt {
	return Receipt{EpochID: epochID, Enc: enc, G: g, S: s, Cost: 1, IssueUs: issueUs, ExpiryUs: issueUs + lifetimeUs}
}

// MACInput returns the bytes the MAC covers (§A5.9):
//
//	epoch_id (16) ‖ len(enc) (2) ‖ enc ‖ g (8) ‖ s (8) ‖ cost (4) ‖ issue_us (8) ‖ expiry_us (8)
//
// 54 + len(enc) bytes. It returns nil for a receipt no acquire issues: an Enc longer than
// 65,535 bytes (a valid encoding has at most 342) or a negative time (t_us ≥ 0).
func MACInput(r Receipt) []byte {
	n := len(r.Enc)
	if n > math.MaxUint16 {
		return nil
	}
	issue := r.IssueUs
	if issue < 0 {
		return nil
	}
	expiry := r.ExpiryUs
	if expiry < 0 {
		return nil
	}
	b := make([]byte, 0, 54+n)
	b = append(b, r.EpochID[:]...)
	b = binary.BigEndian.AppendUint16(b, uint16(n))
	b = append(b, r.Enc...)
	b = binary.BigEndian.AppendUint64(b, r.G)
	b = binary.BigEndian.AppendUint64(b, r.S)
	b = binary.BigEndian.AppendUint32(b, r.Cost)
	b = binary.BigEndian.AppendUint64(b, uint64(issue))
	b = binary.BigEndian.AppendUint64(b, uint64(expiry))
	return b
}

// DeductionID returns "d1_" + base64url(SHA-256(epoch_id ‖ enc ‖ g (8) ‖ s (8)))[:22]
// (§A5.9). Enc is the only variable-length field and the others have fixed lengths, so the
// hashed bytes determine (epoch_id, enc, g, s).
func DeductionID(r Receipt) string {
	h := sha256.New()
	h.Write(r.EpochID[:])
	h.Write([]byte(r.Enc))
	var gs [16]byte
	binary.BigEndian.PutUint64(gs[:8], r.G)
	binary.BigEndian.PutUint64(gs[8:], r.S)
	h.Write(gs[:])
	return "d1_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:22]
}

// ParseEpochID decodes an epoch ID: exactly 32 lowercase hex characters (§A3.2, §A5.9).
// Re-encoding must give the input back, which rejects uppercase digits.
func ParseEpochID(hex32 string) ([16]byte, error) {
	var id [16]byte
	if len(hex32) != 32 {
		return [16]byte{}, ErrEpochID
	}
	if _, err := hex.Decode(id[:], []byte(hex32)); err != nil || hex.EncodeToString(id[:]) != hex32 {
		return [16]byte{}, ErrEpochID
	}
	return id, nil
}
