//go:build scripttest

package scripts_test

import (
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"strings"
	"testing"

	"ratelimiter/internal/scripts"
	"ratelimiter/internal/testutil/scriptrun"
)

// luaFiles are the script sources as the package embeds them.
//
//go:embed *.lua
var luaFiles embed.FS

// TestScriptSHA1 (plan S4, L2; S6 adds refund): for all 7 scripts, SCRIPT LOAD as the script's §A2 user
// returns Script.SHA, the SHA-1 of the assembled text (§A4.1); the text is the file's first
// line, "\n", prelude.lua and the rest of the file (plan S4 step 3); the header line is the
// one §A4.8 rule 1 gives the script; ByName finds each script and nothing else.
func TestScriptSHA1(t *testing.T) {
	h := scriptrun.Store(t)
	conns := map[string]bool{}
	prelude, err := luaFiles.ReadFile("prelude.lua")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		s            scripts.Script
		name, header string
		user         string
	}{
		{scripts.Acquire, "acquire", "#!lua", "rl_lib"},
		{scripts.Refund, "refund", "#!lua", "rl_lib"},
		{scripts.Hold, "hold", "#!lua flags=allow-oom", "rl_lib"},
		{scripts.ReadEpoch, "read_epoch", "#!lua flags=no-writes", "rl_lib"},
		{scripts.ReadEpoch, "read_epoch", "#!lua flags=no-writes", "rl_admin"},
		{scripts.AdminStartEpoch, "admin_start_epoch", "#!lua flags=allow-oom", "rl_admin"},
		{scripts.AdminStatus, "admin_status", "#!lua flags=no-writes", "rl_admin"},
		{scripts.AdminInspect, "admin_inspect", "#!lua flags=no-writes", "rl_admin"},
	} {
		t.Run(c.name+"/"+c.user, func(t *testing.T) {
			src, err := luaFiles.ReadFile(c.name + ".lua")
			if err != nil {
				t.Fatal(err)
			}
			first, rest, _ := strings.Cut(string(src), "\n")
			if want := first + "\n" + string(prelude) + rest; c.s.Text != want {
				t.Errorf("Text is not %s.lua's first line, \"\\n\", prelude.lua and the rest of %s.lua", c.name, c.name)
			}
			if got, _, _ := strings.Cut(c.s.Text, "\n"); got != c.header {
				t.Errorf("header line %q, want %q (DESIGN §A4.8 rule 1)", got, c.header)
			}
			sum := sha1.Sum([]byte(c.s.Text))
			if c.s.Name != c.name || c.s.SHA != hex.EncodeToString(sum[:]) {
				t.Errorf("Name %q, SHA %s; want %q and the SHA-1 of Text, %x", c.s.Name, c.s.SHA, c.name, sum)
			}
			if got, ok := scripts.ByName(c.name); !ok || got != c.s {
				t.Errorf("ByName(%q) = %+v, %v; want the script", c.name, got.Name, ok)
			}
			conn := dial(t, h, c.user)
			conns[c.user] = true
			r := mustDo(t, conn, "SCRIPT", "LOAD", c.s.Text)
			if r.Kind != '$' || r.Str != c.s.SHA {
				t.Errorf("SCRIPT LOAD as %s returned %s, want %s", c.user, r, c.s.SHA)
			}
		})
	}
	for _, name := range []string{"prelude", "due_entries", "cleanup_entry", "reclaim", "admin_restore", "admin_clear", ""} {
		if s, ok := scripts.ByName(name); ok {
			t.Errorf("ByName(%q) = %s, want no script", name, s.Name)
		}
	}
	if !conns["rl_lib"] || !conns["rl_admin"] {
		t.Errorf("SCRIPT LOAD ran as %v, want rl_lib and rl_admin", conns)
	}
}
