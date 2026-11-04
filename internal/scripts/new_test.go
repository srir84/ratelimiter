package scripts_test

import (
	"testing"

	"github.com/srir84/ratelimiter/internal/scripts"
)

func TestNewHashesText(t *testing.T) {
	// SHA-1 test vectors (FIPS 180-2 "abc", and the empty message).
	for _, c := range []struct{ text, sha string }{
		{"abc", "a9993e364706816aba3e25717850c26c9cd0d89d"},
		{"", "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
	} {
		got := scripts.New("acquire", c.text)
		if got != (scripts.Script{Name: "acquire", SHA: c.sha, Text: c.text}) {
			t.Errorf("New(acquire, %q) = %+v, want SHA %s", c.text, got, c.sha)
		}
	}
}
