package keys

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEncode(t *testing.T) {
	if got, err := Encode("a"); err != nil || got != "YQ" {
		t.Errorf(`Encode("a") = %q, %v; want "YQ"`, got, err)
	}
	long := strings.Repeat("k", 256)
	got, err := Encode(long)
	if err != nil || len(got) != 342 {
		t.Errorf("Encode(256 bytes) = %d characters, %v; want 342", len(got), err)
	}
	if err == nil && !IsEnc(got) {
		t.Errorf("Encode(256 bytes) fails IsEnc")
	}
	// The limit counts bytes, not runes (API-1): 128 two-byte runes are 256 bytes.
	if got, err := Encode(strings.Repeat("é", 128)); err != nil || len(got) != 342 {
		t.Errorf("Encode(128 × \"é\", 256 bytes) = %d characters, %v; want 342", len(got), err)
	}
	for _, c := range []struct {
		name, key string
		want      error
	}{
		{"empty", "", ErrKeyLength},
		{"257 bytes", strings.Repeat("k", 257), ErrKeyLength},
		{"258 bytes in 129 runes", strings.Repeat("é", 129), ErrKeyLength},
		{`"\xff"`, "\xff", ErrKeyUTF8},
		{"truncated UTF-8", "\xc3", ErrKeyUTF8},
		{"256 bytes ending in a broken rune", strings.Repeat("k", 255) + "\xe2", ErrKeyUTF8},
	} {
		got, err := Encode(c.key)
		if !errors.Is(err, c.want) || got != "" {
			t.Errorf("Encode(%s) = %q, %v; want error %v", c.name, got, err, c.want)
		}
	}
	// Any characters are safe: each key's encoding is a valid member and decodes back.
	for _, k := range []string{"é", "😀", "rl:e:x:k:y", "a b", "\x00", "a/b+c=d"} {
		enc, err := Encode(k)
		if err != nil || !IsEnc(enc) {
			t.Errorf("Encode(%q) = %q, %v; want a valid member", k, enc, err)
		}
	}
}

// isEncRef is is_enc written another way: the grammar as a regular expression and the
// standard library's strict decoder, which rejects set trailing bits.
func isEncRef(s string) bool {
	if len(s) < 1 || len(s) > 342 || len(s)%4 == 1 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(s) {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil
}

type isEncVector struct {
	S     string `json:"s"`
	Valid bool   `json:"valid"`
	Note  string `json:"note"`
}

func TestIsEncVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/is_enc_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []isEncVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("testdata/is_enc_vectors.json: %v", err)
	}
	valid, invalid := 0, 0
	seen := map[string]bool{}
	for _, v := range vectors {
		if seen[v.S] {
			t.Errorf("duplicate vector %q", v.S)
		}
		seen[v.S] = true
		if got := IsEnc(v.S); got != v.Valid {
			t.Errorf("IsEnc(%q) = %v, want %v (%s)", v.S, got, v.Valid, v.Note)
		}
		if got := isEncRef(v.S); got != v.Valid {
			t.Errorf("vector %q marked valid=%v, but the reference grammar says %v (%s)", v.S, v.Valid, got, v.Note)
		}
		if v.Valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid < 12 || invalid < 12 {
		t.Errorf("%d valid and %d invalid vectors, want at least 12 of each", valid, invalid)
	}
	// The plan names these rejections; the file must keep them.
	a343 := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 257)))
	for _, s := range []string{"Y", "YR", "YQ==", "Y+Q", a343} {
		if !seen[s] {
			t.Errorf("vector %q (%d characters) is missing from the file", s, len(s))
		}
		if IsEnc(s) {
			t.Errorf("IsEnc(%q) = true, want false", s)
		}
	}
	if len(a343) != 343 {
		t.Fatalf("the 257-byte encoding has %d characters, want 343", len(a343))
	}
}

func TestKeyNames(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, c := range []struct{ got, want string }{
		{EpochKey, "rl:epoch"},
		{CtlTimeKey, "rl:ctl:time"},
		{BucketKey(id, "YQ"), "rl:e:0123456789abcdef0123456789abcdef:k:YQ"},
		{MarkerIndexKey(id, "YQ"), "rl:e:0123456789abcdef0123456789abcdef:mi:YQ"},
		{IndexKey(id), "rl:e:0123456789abcdef0123456789abcdef:ix"},
		{QuarantineKey(id), "rl:e:0123456789abcdef0123456789abcdef:q"},
		{BucketPlaceholder(id), "rl:e:0123456789abcdef0123456789abcdef:k:!"},
		{MarkerIndexPlaceholder(id), "rl:e:0123456789abcdef0123456789abcdef:mi:!"},
		{QuarantinePlaceholder, "rl:e:!:q"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if IsEnc("!") {
		t.Errorf(`IsEnc("!") = true: a placeholder could name a real key`)
	}
}

func FuzzEncodeInjective(f *testing.F) {
	for _, s := range [][2]string{
		{"a", "b"}, {"", "a"}, {"\xff", "\xfe"}, {"é", "e"}, {"YQ", "a"}, {"YR", "Y"},
		{strings.Repeat("a", 256), strings.Repeat("a", 257)}, {"a\x00", "a"}, {"😀", "\xf0\x9f\x98"},
		{strings.Repeat("é", 129), strings.Repeat("é", 128)},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		encA, okA := checkEncode(t, a)
		encB, okB := checkEncode(t, b)
		if okA && okB && a != b && encA == encB {
			t.Fatalf("Encode(%q) = Encode(%q) = %q", a, b, encA)
		}
		// The inputs double as candidate members: IsEnc agrees with the reference grammar.
		for _, s := range []string{a, b} {
			if IsEnc(s) != isEncRef(s) {
				t.Fatalf("IsEnc(%q) = %v, reference %v", s, IsEnc(s), isEncRef(s))
			}
		}
	})
}

// checkEncode checks Encode(k) against API-1 and decodes it back with the standard
// library; ok reports whether k was a valid key.
func checkEncode(t *testing.T, k string) (enc string, ok bool) {
	t.Helper()
	valid := len(k) >= 1 && len(k) <= 256 && utf8.ValidString(k)
	enc, err := Encode(k)
	switch {
	case err != nil && valid:
		t.Fatalf("Encode(%q) = %v for a valid key", k, err)
	case err == nil && !valid:
		t.Fatalf("Encode(%q) = %q for an invalid key", k, enc)
	case err != nil:
		return "", false
	}
	dec, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || string(dec) != k {
		t.Fatalf("decode(Encode(%q)) = %q, %v", k, dec, err)
	}
	if !IsEnc(enc) {
		t.Fatalf("IsEnc(Encode(%q)) = false for %q", k, enc)
	}
	return enc, true
}
