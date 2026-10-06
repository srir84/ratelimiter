//go:build scripttest

package lua

import (
	"strings"
	"testing"
)

// TestHello checks the assembled script's text: every line but the 'GT' and write_hwm() ones.
func TestHello(t *testing.T) {
	s, err := Script("hello")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"local OK = 40", "local REFUSED = 41", "local n = 1", "return reply(OK, '')",
		"redis.call('HSET', KEYS[1], 'f', order)", "pcall(redis.call, 'HSET', KEYS[1], 'p', 1)",
		"redis.pcall('HDEL', KEYS[1], 'q')", "redis.call('ZADD', KEYS[2], 'XX', 1, 'm')", "return finish(REFUSED, 'no')",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the assembled script lacks %q", want)
		}
	}
}
