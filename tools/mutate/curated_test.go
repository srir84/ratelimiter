package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the repository root (the tests run in tools/mutate).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// writeFiles writes path → content under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCurated_MatchesDesign (plan S4 step 16; DESIGN §C9): every §C9 curated row whose file
// exists has exactly one curated.go entry with the same ID, file and tests, every entry has a
// row, and each entry's From matches exactly one line of its file while To is one line, so the
// edit changes or deletes exactly one line. The must-fail cases build a table and files of
// their own: each mismatch the check must catch is reported.
func TestCurated_MatchesDesign(t *testing.T) {
	t.Run("curated.go and DESIGN §C9", func(t *testing.T) {
		root := repoRoot(t)
		design, err := os.ReadFile(filepath.Join(root, "DESIGN.md"))
		if err != nil {
			t.Fatal(err)
		}
		problems, err := curatedProblems(t.Context(), root, string(design), curatedEntries())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range problems {
			t.Error(p)
		}
		// The rows parse as DESIGN writes them: PB5's prefix name and PB17's Python test among them.
		rows, err := curatedRows(string(design))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"PB1":  "internal/scripts/acquire.lua TestAcquireDecisions",
			"PB4":  "internal/scripts/prelude.lua TestPreludeValidators",
			"PB5":  "internal/scripts/refund.lua TestRefundScript_*",
			"PB14": "internal/receipt/receipt.go TestVerifyRejectsEachFieldChange",
			"PB16": "refmodel/check_bud11.py refmodel/tests/test_check_bud11.py",
			"PB17": "refmodel/replay.py refmodel/tests/test_replay.py::test_detects_each_field",
		}
		if len(rows) != 18 {
			t.Errorf("DESIGN §C9's curated table: %d rows, want 18 (PB1…PB18)", len(rows))
		}
		for _, r := range rows {
			if w, ok := want[r.id]; ok && r.file+" "+strings.Join(r.tests, " ") != w {
				t.Errorf("row %s: %s %v, want %s", r.id, r.file, r.tests, w)
			}
		}
	})

	t.Run("each mismatch is caught", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{
			"a.go":   "package a\n\nfunc f() int {\n\treturn 1\n}\n\nfunc g() int {\n\treturn 1\n}\n",
			"b.lua":  "local x = 1\nreturn x\n",
			"c.lua":  "return 3\n",
			"d.py":   "x = 1\n",
			"e.lua":  "return 5\n",
			"ok.lua": "local y = 2\nreturn y\n",
		})
		design := strings.Join([]string{
			"Text before the table.",
			"",
			"| ID | File | Planted bug | Rule | Must fail |",
			"|---|---|---|---|---|",
			"| PX1 | `a.go` | `return 1` → `return 2` | R | `TestF` (a note with `{1, t}`) |",
			"| PX2 | `b.lua` | x | R | `TestB_*` |",
			"| PX3 | `c.lua` | a `|` in a span | R | `TestC` |",
			"| PX4 | `d.py` | x | R | `tests/test_d.py::test_x` |",
			"| PX5 | `missing.lua` | not written yet | R | `TestM` |",
			"| PX6 | `e.lua` | x | R | `TestE` |",
			"| PX7 | `ok.lua` | x | R | `TestOK` |",
			"",
			"| ID | File | Other table |",
		}, "\n")
		entries := []curatedEntry{
			{ID: "PX1", File: "a.go", Tests: []string{"TestF"}, From: "return 1", To: "return 2"},        // From matches two lines
			{ID: "PX2", File: "b.lua", Tests: []string{"TestB"}, From: "local x = 1", To: "local x = 2"}, // tests differ
			{ID: "PX3", File: "x.lua", Tests: []string{"TestC"}, From: "return 3", To: "return 4"},       // file differs
			{ID: "PX4", File: "d.py", Tests: []string{"tests/test_d.py::test_x"}, From: "x = 2", To: ""}, // From matches no line
			{ID: "PX7", File: "ok.lua", Tests: []string{"TestOK"}, From: "local y = 2", To: "local y = 3"},
			{ID: "PX7", File: "ok.lua", Tests: []string{"TestOK"}, From: "return y", To: "return y\nreturn 0"}, // a second PX7; To spans lines
			{ID: "PX8", File: "ok.lua", Tests: []string{"TestOK"}, From: "return y", To: "return 0"},           // no row
		}
		problems, err := curatedProblems(t.Context(), root, design, entries)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"PX1: a.go: the From line matches lines 4 and 8",
			"PX2: tests [TestB], but DESIGN §C9 names [TestB_*]",
			"PX3: file x.lua, but DESIGN §C9 names c.lua",
			"PX3: x.lua: open",
			"PX4: d.py: the From line matches no line",
			"PX6: e.lua exists, but curated.go has no entry for it",
			"PX7: 2 entries in curated.go, want one",
			"PX7: ok.lua: the To text spans more than one line",
			"PX8: no row in DESIGN §C9's curated table",
		}
		for _, w := range want {
			n := 0
			for _, p := range problems {
				if strings.Contains(p, w) {
					n++
				}
			}
			if n != 1 {
				t.Errorf("want one problem containing %q, got %d:\n%s", w, n, strings.Join(problems, "\n"))
			}
		}
		if len(problems) != len(want) {
			t.Errorf("got %d problems, want %d:\n%s", len(problems), len(want), strings.Join(problems, "\n"))
		}
		rows, err := curatedRows(design)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 7 || strings.Join(rows[0].tests, ",") != "TestF" || rows[2].file != "c.lua" {
			t.Errorf("rows = %+v, want the 7 rows of the first table, PX1's only test TestF", rows)
		}
		if _, err := curatedRows("no table here\n"); err == nil {
			t.Error("curatedRows on a text with no curated table: no error")
		}
	})

	t.Run("an edit that changes nothing", func(t *testing.T) {
		if _, err := applyLineEdit([]byte("a\n  b\n"), "b", "b"); err == nil || !strings.Contains(err.Error(), "changes nothing") {
			t.Errorf("applyLineEdit(b → b) = %v, want an edit that changes nothing", err)
		}
		got, err := applyLineEdit([]byte("a\n\tb // c\nd"), "b // c", "x")
		if err != nil || string(got) != "a\n\tx\nd" {
			t.Errorf("applyLineEdit kept the indentation? got %q, %v; want %q", got, err, "a\n\tx\nd")
		}
		got, err = applyLineEdit([]byte("a\nb\nc\n"), " b ", "")
		if err != nil || string(got) != "a\nc\n" {
			t.Errorf("applyLineEdit deleting b: got %q, %v; want %q", got, err, "a\nc\n")
		}
	})
}
