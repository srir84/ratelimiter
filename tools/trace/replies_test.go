package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repliesFixture is testdata/replies/base with the named overlays merged over it, and the
// named log of testdata/replies (if any) copied to .out/replies-l2.jsonl.
func repliesFixture(t *testing.T, log string, overlays ...string) string {
	t.Helper()
	names := []string{"replies/base"}
	for _, o := range overlays {
		names = append(names, "replies/"+o)
	}
	root := fixture(t, names...)
	if log != "" {
		data, err := os.ReadFile(filepath.Join(testdataDir(t, "replies"), log))
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, root, repliesLog, string(data))
	}
	return root
}

// TestReplies (plan S4 step 15; DESIGN §C3, §C9): the reply-code closure. A complete fixture
// passes, with the rows of scripts that have no .lua file (acquire, …) skipped, observations
// of such scripts ignored, data entries' tables and locals out of a return's scope left alone,
// and (c) skipped before S4; each must-fail fixture fails on exactly its rule: (a) a reply
// literal missing from the table, read from every form (reply(…), finish(…), quarantine(…),
// hold(…), return {…}, return false, {…}, a returned variable, an integer-first table returned
// inside a function), a non-literal the table declares no set for, a source's code with a
// literal reason that some code of the source has no row for, a script with no rows, a prelude
// literal no existing script lists with that code and reason, a reply built from a parameter
// anywhere but in the prelude's reply, finish, quarantine and hold, replies the check cannot
// read, and every reply shape it cannot follow (S4 repair F1, "(a) fails closed"); (b) a
// reason that is not in DESIGN §A3.3 or §A4.1–§A4.9 in an accepted form (a Lua block or a
// sibling heading outside them counts nowhere), and a missing section; (c) a row never
// observed, an observation that is no row, a malformed log line and a missing log. The
// repository's own scripts and DESIGN pass (a) and (b).
func TestReplies(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		root := repliesFixture(t, "complete.jsonl")
		code, stdout, stderr := runIn(t, root, "replies", "--upto", "S4")
		if code != 0 || !strings.Contains(stdout, "(c) 13 of 13 row(s) observed") || !strings.Contains(stdout, "3 script(s) with a .lua file") {
			t.Fatalf("complete fixture: exit %d, want 0 with all 13 rows of the 3 scripts observed\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
	})

	t.Run("(c) only from S4 on", func(t *testing.T) {
		t.Parallel()
		root := repliesFixture(t, "") // no log at all
		for _, args := range [][]string{{"replies", "--upto", "S3"}, {"replies"}} {
			code, stdout, stderr := runIn(t, root, args...)
			if code != 0 {
				t.Errorf("%s with no log: exit %d, want 0 ((c) skipped)\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, stdout, stderr)
			}
		}
		code, _, stderr := runIn(t, root, "replies", "--upto", "S5")
		if code != 1 || countContaining(strings.Split(stderr, "\n"), repliesLog+": missing") != 1 {
			t.Errorf("replies --upto S5 with no log: exit %d, want 1 naming the missing log\n%s", code, stderr)
		}
		if code, _, stderr := runIn(t, root, "replies", "--upto", "S99"); code != 2 {
			t.Errorf("replies --upto S99: exit %d, want 2 (usage)\n%s", code, stderr)
		}
	})

	t.Run("(a) reply literals", func(t *testing.T) {
		t.Parallel()
		root := repliesFixture(t, "", "literal")
		r, err := checkReplies(t.Context(), testEnv(root), nil)
		if err != nil {
			t.Fatal(err)
		}
		sameLines(t, "(a)", problemLines(t, r.problems), wantLines(t, root), r.problems)
		for _, want := range []string{
			"hold.lua:5: {…} replies 41 'epoch_gone', which is not in hold's rows",
			"hold.lua:6: reply(…) replies 40 'done'",
			"hold.lua:7: finish(…) replies 23 'quarantined'",
			"hold.lua:8: quarantine(…) replies 23 'malformed_bucket'",
			"hold.lua:9: hold(…) replies 22 'integrity_hold'",
			"hold.lua:10: quarantine(…) replies through raw, a non-literal for which the reply table declares no set for hold",
			"hold.lua:11: reply(…): the code is neither an integer nor a name",
			"hold.lua:12: reply(…): the reason is neither a string literal nor a name",
			"hold.lua:13: {…} replies 7 ''",
			"hold.lua:14: {…} replies through E_UNKNOWN",
			"hold.lua:15: reply(…): needs a code and a reason",
			"hold.lua:16: hold(…): needs the hold's reason",
			"hold.lua:17: {…} replies 41 'epoch_semicolon'",
			"hold.lua:18: reply(…): the reason is neither a string literal nor a name",
			"hold.lua:19: reply(…): the code is neither an integer nor a name",
			"hold.lua:21: {…} replies 41 'epoch_missing'",
			"hold.lua:23: {…}: a reply needs a code, t_us and a reason",
			"hold.lua:24: {…} replies through code_name",
			"read_epoch.lua:4: {…} replies code 23, but read_epoch's source state holds 0 ''",
			"unlisted.lua:1: unlisted has no rows in the reply table",
			"admin_start_epoch.lua:3: {…}: cannot read the table",
			"admin_start_epoch.lua:4: reply(…): cannot read its arguments",
			"prelude.lua:30: {…} replies 25 'keys_mismatch', which no script with a .lua file lists",
			"prelude.lua:31: {…} replies through why, which no script with a .lua file declares with code 22",
			"prelude.lua:37: {…} replies through held, which no script with a .lua file declares with code 23",
			"prelude.lua:47: {…} replies through state, which no script with a .lua file declares with code 23",
			// An integer-first table inside a function: given by a return, or by a variable it returns.
			"hold.lua:27: {…} replies 41 'not_a_design_reason', which is not in hold's rows",
			"hold.lua:30: {…} replies 41 'gone_later_reason', which is not in hold's rows",
			// A reply built from a parameter outside the prelude's reply, finish, quarantine and hold.
			"hold.lua:35: {…}: cannot read: reply built from parameter why of refuse_with",
			"hold.lua:38: reply(…): cannot read: reply built from parameter code of refuse_code",
			"hold.lua:41: {…}: cannot read: reply built from parameter code of vanished",
			"hold.lua:44: {…}: cannot read: reply built from parameter reason of quarantine",
			"read_epoch.lua:10: {…}: cannot read: reply built from parameter v of num",
			"prelude.lua:38: {…}: cannot read: reply built from parameter state of read",
			// A declared non-literal code with a literal reason: every code it holds needs that row.
			"read_epoch.lua:6: {…} replies through state with reason '', but read_epoch's rows lack 22 ''",
			"read_epoch.lua:7: {…} replies through state with reason 'hwm_missing', but read_epoch's rows lack 0 'hwm_missing'",
			"prelude.lua:58: reply(…) replies through state with reason 'hwm_missing', which no script with a .lua file declares with a row of that reason for every code state holds",
			// A prelude literal is one row of some script: code and reason together.
			"prelude.lua:52: {…} replies 22 'hwm_gone', which no script with a .lua file lists",
			"prelude.lua:53: {…} replies 23 'epoch_changed', which no script with a .lua file lists",
		} {
			if countContaining(r.problems, want) != 1 {
				t.Errorf("want one problem containing %q:\n%s", want, strings.Join(r.problems, "\n"))
			}
		}
	})

	// Both sides of the source rules, with a table that declares two sources for hold: a site
	// with a source's code and a literal reason passes only when every code the source holds has
	// a row with that reason (in the script's file, and in the prelude for some script); an
	// integer-first table inside a function is a reply only when a return gives it.
	t.Run("(a) sources with literal reasons", func(t *testing.T) {
		t.Parallel()
		root := repliesFixture(t, "", "sources")
		table := replyTable()
		for i := range table {
			if table[i].script == "hold" {
				table[i].sources = []replySource{
					{"st", []replyCode{{40, []string{""}}, {41, []string{"epoch_changed"}}}},
					{"gone", []replyCode{{41, []string{"epoch_changed"}}}},
				}
			}
		}
		r, err := replyProblems(t.Context(), root, table, false, "")
		if err != nil {
			t.Fatal(err)
		}
		sameLines(t, "(a) sources", problemLines(t, r.problems), wantLines(t, root), r.problems)
		for _, want := range []string{
			"hold.lua:7: {…} replies through st with reason 'epoch_changed', but hold's rows lack 40 'epoch_changed'",
			"hold.lua:8: {…} replies through st with reason '', but hold's rows lack 41 ''",
			"hold.lua:9: {…} replies through gone with reason '', but hold's rows lack 41 ''",
			"prelude.lua:35: reply(…) replies through st with reason 'epoch_changed', which no script with a .lua file declares with a row of that reason for every code st holds",
		} {
			if countContaining(r.problems, want) != 1 {
				t.Errorf("want one problem containing %q:\n%s", want, strings.Join(r.problems, "\n"))
			}
		}
	})

	// S4 repair F1: (a) fails closed. Each fixture, over base, holds the replies of one kind that
	// (a) cannot read or that are not in the rows, each a want line, beside pass-side lines that
	// must stay quiet: a name first in a table a return gives inside a function, also through
	// and/or or another local, the local a returned name means chosen by Lua's scoping (if
	// branches, local x = x, a for loop's variables) (name_first); an expression, a string or a key
	// first (expr_code); a
	// reply filled in or changed by index, also a script's own to_map and a data entry's variable
	// that holds a table with a code, beside appends and tables no return gives (index_built); any
	// use of reply, finish, quarantine or hold but a call with parentheses, beside fields, keys
	// and definitions of those names (alias); and what the value flow cannot follow, beside what
	// it never needs to: a call's result, a parameter, KEYS, ARGV, a code name (flow).
	t.Run("(a) fails closed", func(t *testing.T) {
		for _, c := range []struct {
			overlay string
			want    []string
		}{
			{"name_first", []string{
				"hold.lua:8: {…} replies through code, a non-literal for which the reply table declares no set for hold",
				"hold.lua:12: {…} replies through code,",
				"hold.lua:16: {…} replies through cols,",
				"hold.lua:19: {…} replies through cols,",
				"hold.lua:26: {…} replies through cols,",
				"hold.lua:30: {…} replies through code,",
				"hold.lua:33: {…} replies through cols,",
				"hold.lua:39: {…} replies through cols,",
				"hold.lua:47: {…} replies through cols,",
				"hold.lua:53: {…} replies through cols,",
			}},
			{"expr_code", []string{
				"hold.lua:5: {…}: cannot read: a returned table whose first field is no code",
				"hold.lua:6: {…}: cannot read: a returned table whose first field is no code",
				"hold.lua:8: {…}: cannot read: a returned table whose first field is no code",
				"hold.lua:11: {…}: cannot read: a returned table whose first field is no code",
				"hold.lua:15: {…}: cannot read: a returned table whose first field is no code",
				"hold.lua:18: {…}: cannot read: a returned table whose first field is no code",
			}},
			{"index_built", []string{
				"hold.lua:6: {…}: cannot read: a returned table built empty",
				"hold.lua:7: r[…]: cannot read: r is returned, and a reply changed by index after its constructor cannot be read",
				"hold.lua:8: r[…]: cannot read",
				"hold.lua:9: r[…]: cannot read",
				"hold.lua:13: {…}: cannot read: a returned table built empty",
				"hold.lua:21: {…}: cannot read: a returned table built empty",
				"hold.lua:23: m[…]: cannot read: m is returned",
				"hold.lua:28: out[…]: cannot read: out is returned",
				"hold.lua:29: out[…]: cannot read: out is returned",
				"prelude.lua:42: {…} replies through code with reason 'not_a_design_reason', which no script with a .lua file declares",
			}},
			{"alias", []string{
				"hold.lua:4: reply: cannot read: reply is used other than in a call with parentheses",
				"hold.lua:5: finish: cannot read: finish is used other than in a call with parentheses",
				"hold.lua:9: quarantine: cannot read: quarantine is used other than in a call with parentheses",
				"hold.lua:13: hold: cannot read: hold is used other than in a call with parentheses",
				"hold.lua:14: reply: cannot read",
				"hold.lua:15: hold: cannot read",
				"hold.lua:17: hold: cannot read",
			}},
			{"flow", []string{
				"hold.lua:7: rec: cannot read: a return gives a field or element of rec, a local that holds a table",
				"hold.lua:11: list: cannot read: a return gives a field or element of list, a local that holds a table",
				"hold.lua:14: E: cannot read: a return gives E, which is no local of this file",
				"hold.lua:17: K: cannot read: a return gives a field or element of K, which is no local of this file",
				"hold.lua:19: E: cannot read: a table or a local is assigned to E, which is no local of this file",
				"hold.lua:21: T: cannot read: a table or a local is assigned to T, which is no local of this file",
				"hold.lua:29: local: cannot read the values it assigns",
				"hold.lua:32: return: cannot read the values it gives",
			}},
		} {
			t.Run(c.overlay, func(t *testing.T) {
				t.Parallel()
				root := repliesFixture(t, "", c.overlay)
				if code, stdout, stderr := runIn(t, root, "replies"); code != 1 {
					t.Errorf("replies on %s: exit %d, want 1\nstdout:\n%s\nstderr:\n%s", c.overlay, code, stdout, stderr)
				}
				r, err := checkReplies(t.Context(), testEnv(root), nil)
				if err != nil {
					t.Fatal(err)
				}
				sameLines(t, c.overlay, problemLines(t, r.problems), wantLines(t, root), r.problems)
				for _, w := range c.want {
					if countContaining(r.problems, w) != 1 {
						t.Errorf("want one problem containing %q:\n%s", w, strings.Join(r.problems, "\n"))
					}
				}
			})
		}
	})

	t.Run("(a) without prelude.lua", func(t *testing.T) {
		t.Parallel()
		root := repliesFixture(t, "")
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(preludeRel))); err != nil {
			t.Fatal(err)
		}
		r, err := checkReplies(t.Context(), testEnv(root), nil)
		if err != nil {
			t.Fatal(err)
		}
		if countContaining(r.problems, preludeRel+": missing, so the reply code names of the scripts cannot resolve") != 1 ||
			countContaining(r.problems, "hold.lua:4: {…} replies through REFUSED") != 1 {
			t.Errorf("no prelude.lua: want it named missing, and REFUSED unresolved in hold.lua:\n%s", strings.Join(r.problems, "\n"))
		}
	})

	t.Run("(b) reasons in DESIGN", func(t *testing.T) {
		t.Parallel()
		root := repliesFixture(t, "", "reason")
		r, err := checkReplies(t.Context(), testEnv(root), nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"DESIGN.md: no section §A4.7",
			"admin_status 41 'epoch_record_malformed': the reason is not in DESIGN",
			"admin_status 41 'shared_key_malformed': the reason is not in DESIGN",
			"read_epoch 22 'epoch_record_malformed': the reason is not in DESIGN",
		}
		for _, w := range want {
			if countContaining(r.problems, w) != 1 {
				t.Errorf("want one problem containing %q:\n%s", w, strings.Join(r.problems, "\n"))
			}
		}
		if len(r.problems) != len(want) {
			t.Errorf("got %d problems, want %d:\n%s", len(r.problems), len(want), strings.Join(r.problems, "\n"))
		}
	})

	t.Run("(c) observed rows", func(t *testing.T) {
		t.Parallel()
		root := repliesFixture(t, "partial.jsonl")
		r, err := checkReplies(t.Context(), testEnv(root), []string{"--upto", "S4"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"admin_status 41 'shared_key_malformed': never observed in " + repliesLog,
			"read_epoch 22 'hwm_malformed': never observed in " + repliesLog,
			repliesLog + ": observed hold 41 'epoch_gone', which is not in hold's rows",
			repliesLog + ":13: not a {script, code, reason} line",
			repliesLog + ":14: needs script, code and reason",
			repliesLog + ":15: not a {script, code, reason} line",
			repliesLog + ":16: text after the JSON object",
			repliesLog + ":17: not a {script, code, reason} line",
			repliesLog + ":18: not a {script, code, reason} line",
			repliesLog + ":19: code 122 is not a reply code",
			repliesLog + ":20: empty script name",
		}
		for _, w := range want {
			if countContaining(r.problems, w) != 1 {
				t.Errorf("want one problem containing %q:\n%s", w, strings.Join(r.problems, "\n"))
			}
		}
		if len(r.problems) != len(want) {
			t.Errorf("got %d problems, want %d:\n%s", len(r.problems), len(want), strings.Join(r.problems, "\n"))
		}
	})

	// The table itself: a source holding a row the script's rows lack, and a script listed twice.
	t.Run("table consistency", func(t *testing.T) {
		t.Parallel()
		root := repliesFixture(t, "")
		table := []replyScript{
			{script: "hold", codes: []replyCode{{40, []string{""}}, {41, []string{"epoch_changed"}}},
				sources: []replySource{{"raw", []replyCode{{41, []string{"epoch_gone"}}}}}},
			{script: "hold", codes: []replyCode{{40, []string{""}}}},
		}
		r, err := replyProblems(t.Context(), root, table, false, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{
			"hold's source raw holds 41 'epoch_gone', which is not in hold's rows",
			"hold has two entries in the reply table",
			"admin_status.lua:1: admin_status has no rows in the reply table",
			"read_epoch.lua:1: read_epoch has no rows in the reply table",
		} {
			if countContaining(r.problems, w) != 1 {
				t.Errorf("want one problem containing %q:\n%s", w, strings.Join(r.problems, "\n"))
			}
		}
	})

	// The code names: only a top-level local set to a lone decimal integer is a constant.
	t.Run("code names", func(t *testing.T) {
		t.Parallel()
		src := strings.Join([]string{
			"#!lua",
			"local A = 1",
			"local B = 2 + 3",             // an expression
			"local C, D = 4, 5",           // a list
			"local E = 6; local F = 7",    // ';' ends a statement
			"local function f()",          //
			"  local G = 8",               // inside a function
			"  if G then local H = 9 end", // inside a block
			"  for _ = 1, 2 do local P = 9 end",
			"end",
			"local I = -1",                   // a negative value is an expression
			"local J = 0x10",                 // not decimal
			"local K = 99999999999999999999", // too large
			"local L = 10 -- a comment",
			"local M = 11",
			"(f)()", // a statement that starts with '('
			"local N = 12 .. ''",
		}, "\n")
		got, err := luaIntConstants(src)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"A": 1, "E": 6, "F": 7, "L": 10, "M": 11}
		if len(got) != len(want) {
			t.Errorf("luaIntConstants = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("luaIntConstants[%s] = %d, want %d (all: %v)", k, got[k], v, got)
			}
		}
	})

	t.Run("the repository's scripts and DESIGN", func(t *testing.T) {
		t.Parallel()
		code, stdout, stderr := runIn(t, repoRoot(t), "replies")
		if code != 0 {
			t.Fatalf("replies on the repository: exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		// Every table script is one of DESIGN §A4.1's.
		for _, s := range replyTable() {
			if !slices.Contains(scriptNames(), s.script) {
				t.Errorf("reply table script %q is not a §A4.1 script", s.script)
			}
		}
	})
}
