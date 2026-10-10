package keys

import (
	"encoding/base64"
	"unicode/utf8"
)

// Key limits (API-1) and the encoded length they allow (§A3.2).
const (
	MaxKeyBytes = 256
	MaxEncLen   = 342 // base64url of 256 bytes, without padding
)

// KeyError says why a budget key is invalid (API-1: validation_error). Its values are
// constants, compared with errors.Is; none contains the key.
type KeyError string

func (e KeyError) Error() string { return string(e) }

const (
	// ErrKeyLength: the key is empty or longer than 256 bytes.
	ErrKeyLength KeyError = "budget key must be 1–256 bytes"
	// ErrKeyUTF8: the key is not valid UTF-8.
	ErrKeyUTF8 KeyError = "budget key must be valid UTF-8"
)

// Encode returns enc(key), the base64url encoding without padding of the key's bytes. The
// key must be valid UTF-8 of 1–256 bytes; the error never contains the key.
func Encode(key string) (string, error) {
	if len(key) < 1 || len(key) > MaxKeyBytes {
		return "", ErrKeyLength
	}
	if !utf8.ValidString(key) {
		return "", ErrKeyUTF8
	}
	return base64.RawURLEncoding.EncodeToString([]byte(key)), nil
}

// IsEnc reports whether s follows the member grammar is_enc, identical in Go and in the
// scripts' prelude (§A3.2): 1–342 characters of [A-Za-z0-9_-], length mod 4 ≠ 1, and
// decoding then re-encoding gives s back. The last rule rejects non-canonical strings such
// as "YR", which Go's decoder accepts. It does not check that the bytes are UTF-8.
func IsEnc(s string) bool {
	n := len(s)
	if n < 1 || n > MaxEncLen || n%4 == 1 {
		return false
	}
	for i := range n {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && base64.RawURLEncoding.EncodeToString(b) == s
}

// Store key names (§A3.2). id is an epoch ID (32 lowercase hex characters) and enc a
// budget key's encoding; the builders only join strings, so callers pass checked values.
const (
	EpochKey   = "rl:epoch"    // the epoch record hash
	CtlTimeKey = "rl:ctl:time" // the controlled clock value, test deployments only

	// QuarantinePlaceholder stands in for the q hash in read_epoch's KEYS before the
	// instance has joined an epoch (§A4.1).
	QuarantinePlaceholder = "rl:e:!:q"
)

// BucketKey is rl:e:<id>:k:<enc>, the key's bucket and markers hash.
func BucketKey(id, enc string) string { return "rl:e:" + id + ":k:" + enc }

// MarkerIndexKey is rl:e:<id>:mi:<enc>, the key's marker index sorted set.
func MarkerIndexKey(id, enc string) string { return "rl:e:" + id + ":mi:" + enc }

// IndexKey is rl:e:<id>:ix, the epoch's bucket index sorted set.
func IndexKey(id string) string { return "rl:e:" + id + ":ix" }

// QuarantineKey is rl:e:<id>:q, the epoch's quarantine hash.
func QuarantineKey(id string) string { return "rl:e:" + id + ":q" }

// BucketPlaceholder is rl:e:<id>:k:!, passed to cleanup_entry instead of the bucket key
// when an ix member fails IsEnc (§A4.1); "!" is never a valid enc.
func BucketPlaceholder(id string) string { return "rl:e:" + id + ":k:!" }

// MarkerIndexPlaceholder is rl:e:<id>:mi:!, the marker index counterpart of
// BucketPlaceholder.
func MarkerIndexPlaceholder(id string) string { return "rl:e:" + id + ":mi:!" }
