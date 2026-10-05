package main

import (
	"strings"
	"testing"
)

func luaText(t *testing.T, root string) []string {
	t.Helper()
	files := walkFiles(t, root, withSuffix(".lua"))
	problems, err := luaTextProblems(t.Context(), root, files)
	if err != nil {
		t.Fatal(err)
	}
	return problems
}

func TestLua_Header(t *testing.T) {
	problems := luaText(t, testdataDir(t, "lua/header"))
	if len(problems) != 1 || !strings.HasPrefix(problems[0], `internal/scripts/acquire.lua:1: header "#!lua flags=allow-oom", want "#!lua"`) {
		t.Fatalf("acquire.lua with the allow-oom header was not reported:\n%s", strings.Join(problems, "\n"))
	}
	// prelude.lua has no header: the loader sends the script's own line 1 before it.
	problems = luaText(t, testdataDir(t, "lua/prelude_header"))
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "internal/scripts/prelude.lua:1: prelude.lua has no header") {
		t.Fatalf("prelude.lua starting with #!lua was not reported:\n%s", strings.Join(problems, "\n"))
	}
	// The 12 scripts and headers of DESIGN §A4.1, transcribed from its table: each script
	// passes with its own header and fails with either of the other two.
	const (
		plain    = "#!lua"
		allowOOM = "#!lua flags=allow-oom"
		noWrites = "#!lua flags=no-writes"
	)
	for _, s := range []struct{ name, header string }{
		{"acquire", plain},
		{"refund", plain},
		{"due_entries", allowOOM},
		{"cleanup_entry", allowOOM},
		{"reclaim", allowOOM},
		{"read_epoch", noWrites},
		{"hold", allowOOM},
		{"admin_start_epoch", allowOOM},
		{"admin_restore", allowOOM},
		{"admin_clear", allowOOM},
		{"admin_status", noWrites},
		{"admin_inspect", noWrites},
	} {
		for _, h := range []string{plain, allowOOM, noWrites} {
			root := t.TempDir()
			writeTestFile(t, root, "internal/scripts/"+s.name+".lua", h+"\nreturn 1\n")
			p := luaText(t, root)
			switch {
			case h == s.header && len(p) > 0:
				t.Errorf("%s with its own header %q: %v", s.name, h, p)
			case h != s.header && (len(p) != 1 || !strings.HasPrefix(p[0],
				"internal/scripts/"+s.name+".lua:1: header \""+h+"\", want \""+s.header+"\"")):
				t.Errorf("%s with %q, want one problem asking for %q: %v", s.name, h, s.header, p)
			}
		}
	}
}

func TestLua_Banned(t *testing.T) {
	problems := luaText(t, testdataDir(t, "lua/banned"))
	const unpack = ": unpack, called or as a value, only on a chunked write"
	for _, want := range []string{
		"scripts/tostring.lua:1: tostring( is banned",
		"scripts/unpack.lua:2" + unpack,
		"scripts/midmark.lua:2" + unpack,     // the mark is not at the line's end
		"scripts/alias.lua:1" + unpack,       // local u = unpack: the alias's own calls are on other lines
		"scripts/pcall.lua:2" + unpack,       // pcall(unpack, t)
		"scripts/tableunpack.lua:2" + unpack, // table.unpack(t), unpack(t): one problem per line
		"scripts/badlex.lua:2: string: unterminated long bracket: trace cannot read the tokens from here on",
		"scripts/zrange.lua:1: ZRANGEBYSCORE is banned",
	} {
		if countContaining(problems, want) != 1 {
			t.Errorf("want one problem %q", want)
		}
	}
	// An annotated unpack( passes, and so do comments and strings that mention unpack.
	if len(problems) != 8 || countContaining(problems, "chunked.lua") > 0 || countContaining(problems, "prose.lua") > 0 {
		t.Fatalf("got:\n%s", strings.Join(problems, "\n"))
	}
}

func TestLua_Luacheck(t *testing.T) {
	root := fixture(t, "lua/luacheck")
	copyFile(t, root, ".luacheckrc")
	files := walkFiles(t, root, withSuffix(".lua"))
	findings, err := luacheckPass(t.Context(), root, files)
	if err != nil {
		t.Fatalf("luacheck pass: %v", err)
	}
	for _, want := range []string{
		"internal/scripts/acquire.lua:3:1: (W111) setting non-standard global variable 'leaked'", // a global in the body
		"internal/scripts/acquire.lua:4:7: (W211) unused variable 'spare'",                       // an unused body local
		"internal/scripts/prelude.lua:8:9: (W211) unused variable 'scratch'",                     // inside a prelude helper
	} {
		if countContaining(findings, want) != 1 {
			t.Errorf("want the finding %q once", want)
		}
	}
	// The unused top-level helper (prelude.lua:3) is dropped, and the prelude finding is
	// reported once although both scripts include it.
	if len(findings) != 3 || countContaining(findings, "unused_helper") > 0 {
		t.Fatalf("findings:\n%s", strings.Join(findings, "\n"))
	}
	// The assembled scripts are what the store receives: header, prelude, body.
	data, err := readLines(t.Context(), root, ".out/lua/refund.lua")
	if err != nil {
		t.Fatal(err)
	}
	if data[0] != "#!lua" || data[1] != "local CHUNK = 500" || data[len(data)-1] != "return with_scratch()" {
		t.Fatalf(".out/lua/refund.lua:\n%s", strings.Join(data, "\n"))
	}

	// With no prelude.lua, every .lua file is linted as it is, a §A4.1 script or not, and
	// each finding stays at its own line.
	root = fixture(t, "lua/asis")
	copyFile(t, root, ".luacheckrc")
	files = walkFiles(t, root, withSuffix(".lua"))
	findings, err = luacheckPass(t.Context(), root, files)
	want := "internal/scripts/read_epoch.lua:3:1: (W111) setting non-standard global variable 'leaked'\n" +
		"tools/other.lua:1:7: (W211) unused variable 'spare'"
	if err != nil || strings.Join(findings, "\n") != want {
		t.Fatalf("files linted as they are: %v\n%s\nwant\n%s", err, strings.Join(findings, "\n"), want)
	}

	// A prelude.lua on disk with unused helpers gives no finding: it is never linted alone.
	root = fixture(t, "lua/prelude_only")
	copyFile(t, root, ".luacheckrc")
	files = walkFiles(t, root, withSuffix(".lua"))
	findings, err = luacheckPass(t.Context(), root, files)
	if err != nil || len(findings) > 0 {
		t.Fatalf("prelude.lua alone: %v\n%s", err, strings.Join(findings, "\n"))
	}

	// A prelude.lua with no final newline: its last line shares an assembled line with
	// script line 2, and the body's findings still map to their own script lines.
	root = fixture(t, "lua/nofinalnl")
	copyFile(t, root, ".luacheckrc")
	files = walkFiles(t, root, withSuffix(".lua"))
	findings, err = luacheckPass(t.Context(), root, files)
	want = "internal/scripts/acquire.lua:3:1: (W111) setting non-standard global variable 'leaked'\n" +
		"internal/scripts/acquire.lua:4:8: (W113) accessing undefined variable 'leaked'"
	if err != nil || strings.Join(findings, "\n") != want {
		t.Fatalf("prelude.lua without a final newline: %v\n%s\nwant\n%s", err, strings.Join(findings, "\n"), want)
	}

	// luacheck absent from PATH: an error that names make setup, even with no .lua file
	// to check.
	t.Setenv("PATH", t.TempDir())
	empty := t.TempDir()
	if _, err := luacheckPass(t.Context(), empty, nil); err == nil || !strings.Contains(err.Error(), "make setup") {
		t.Fatalf("without luacheck and with no .lua file: err = %v, want one naming make setup", err)
	}
	if code, _, stderr := runIn(t, empty, "lua"); code != 1 || !strings.Contains(stderr, "run make setup") {
		t.Fatalf("trace lua without luacheck, no .lua file: exit %d\n%s", code, stderr)
	}
	root = fixture(t, "lua/luacheck")
	copyFile(t, root, ".luacheckrc")
	files = walkFiles(t, root, withSuffix(".lua"))
	if _, err := luacheckPass(t.Context(), root, files); err == nil || !strings.Contains(err.Error(), "make setup") {
		t.Fatalf("without luacheck: err = %v, want one naming make setup", err)
	}
	if code, _, stderr := runIn(t, root, "lua"); code != 1 || !strings.Contains(stderr, "run make setup") {
		t.Fatalf("trace lua without luacheck: exit %d\n%s", code, stderr)
	}
}
