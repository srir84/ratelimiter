package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aclFixture is a root holding the real ACL template and one testdata script set.
func aclFixture(t *testing.T, name string) string {
	t.Helper()
	root := fixture(t, "acl/"+name)
	copyFile(t, root, aclTemplate)
	return root
}

func aclProblems(t *testing.T, root string) []string {
	t.Helper()
	r, err := checkACL(t.Context(), testEnv(root), nil)
	if err != nil {
		t.Fatalf("acl: %v", err)
	}
	return r.problems
}

func TestACL_RealTemplate(t *testing.T) {
	problems, err := aclTemplateProblems(t.Context(), filepath.Join(repoRoot(t), aclTemplate))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("deploy/acl.conf.tmpl:\n%s", strings.Join(problems, "\n"))
	}
}

func TestACL_MissingExtraCommand(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), aclTemplate))
	if err != nil {
		t.Fatal(err)
	}
	edit := func(user string, change func(string) string) string {
		lines := strings.Split(string(data), "\n")
		for i, l := range lines {
			if strings.HasPrefix(l, "user "+user+" ") {
				lines[i] = change(l)
			}
		}
		return strings.Join(lines, "\n")
	}
	line := func(user string) string {
		for _, l := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(l, "user "+user+" ") {
				return l
			}
		}
		t.Fatalf("%s has no line for %s", aclTemplate, user)
		return ""
	}
	for _, c := range []struct {
		name, tmpl, want string
	}{
		{"rl_admin without +hget", edit("rl_admin", func(l string) string { return strings.Replace(l, " +hget ", " ", 1) }),
			"rl_admin: +hget is missing"},
		{"rl_lib with +flushall", edit("rl_lib", func(l string) string { return l + " +flushall" }),
			"rl_lib: +flushall is not in DESIGN §A2"},
		{"line 1 not default off", strings.Replace(string(data), "user default off", "user default on", 1),
			"line 1 must be exactly"},
		{"rl_ops with a key pattern", edit("rl_ops", func(l string) string { return l + " ~rl:*" }),
			"rl_ops: key patterns [~rl:*], want none"},
		{"rl_lib with a command category", edit("rl_lib", func(l string) string { return l + " +@read" }),
			"rl_lib: +@read grants a whole command set"},
		{"rl_fixture with every key", edit("rl_fixture", func(l string) string { return strings.Replace(l, "~rl:*", "~*", 1) }),
			"rl_fixture: ~* grants every key"},
		{"rl_admin with another key pattern", edit("rl_admin", func(l string) string { return strings.Replace(l, "~rl:*", "~rl:x*", 1) }),
			"rl_admin: key patterns [~rl:x*], want exactly ~rl:*"},
		{"rl_fixture without its key pattern", edit("rl_fixture", func(l string) string { return strings.Replace(l, " ~rl:*", "", 1) }),
			"rl_fixture: key patterns [], want exactly ~rl:*"},
		// Only the §A2 rules pass, keywords in any case (the store reads them that way):
		// no command set, every-key, read/write pattern, selector, other template action,
		// literal password, or any other rule.
		{"rl_lib with allcommands", edit("rl_lib", func(l string) string { return l + " allcommands" }),
			"rl_lib: allcommands grants a whole command set"},
		{"rl_lib with ALLCOMMANDS", edit("rl_lib", func(l string) string { return l + " ALLCOMMANDS" }),
			"rl_lib: ALLCOMMANDS grants a whole command set"},
		{"rl_admin with +@ALL", edit("rl_admin", func(l string) string { return l + " +@ALL" }),
			"rl_admin: +@ALL grants a whole command set"},
		{"rl_ops with AllKeys", edit("rl_ops", func(l string) string { return l + " AllKeys" }),
			"rl_ops: AllKeys grants every key"},
		{"rl_admin with %R~", edit("rl_admin", func(l string) string { return strings.Replace(l, "~rl:*", "%R~rl:*", 1) }),
			"rl_admin: %R~rl:*: no %R~, %W~ or %RW~ key patterns"},
		{"rl_fixture with %W~", edit("rl_fixture", func(l string) string { return l + " %W~rl:x" }),
			"rl_fixture: %W~rl:x: no %R~, %W~ or %RW~ key patterns"},
		{"rl_lib with %rw~", edit("rl_lib", func(l string) string { return l + " %rw~rl:*" }),
			"rl_lib: %rw~rl:*: no %R~, %W~ or %RW~ key patterns"},
		{"rl_lib with a selector", edit("rl_lib", func(l string) string { return l + " (~* +hgetall +del )" }),
			`rl_lib: selector "(~* +hgetall +del )": DESIGN §A2 grants only flat rules`},
		{"rl_ops with a template action", edit("rl_ops", func(l string) string { return l + ` {{"+flushall"}}` }),
			`rl_ops: template action "{{\"+flushall\"}}"`},
		{"rl_ops with a spaced template action", edit("rl_ops", func(l string) string { return l + ` {{ "+flushall" }}` }),
			`rl_ops: template action "{{ \"+flushall\" }}"`},
		// Lines split as the store splits an ACL file: at single spaces only, quotes kept
		// (quotes inside a template action belong to text/template).
		{"a quoted rule", edit("rl_lib", func(l string) string { return l + ` "+ping"` }),
			`deploy/acl.conf.tmpl:2: a quote (" or ') outside a template action`},
		{"an unterminated quote", string(data) + "user rl_x \"on\n",
			`deploy/acl.conf.tmpl:6: a quote (" or ') outside a template action`},
		{"a tab between rules", edit("rl_lib", func(l string) string { return strings.Replace(l, " -@all", "\t-@all", 1) }),
			"rl_lib: no -@all rule"},
		{"two spaces after user", edit("rl_ops", func(l string) string { return strings.Replace(l, "user ", "user  ", 1) }),
			`deploy/acl.conf.tmpl:5: unknown user ""`},
		// A token that can carry a password prints as ">***" (TestACL_NoPasswordInOutput).
		{"rl_lib with another user's password", edit("rl_lib", func(l string) string { return strings.Replace(l, "{{.LibPassword}}", "{{.OpsPassword}}", 1) }),
			`rl_lib: template action ">***": the only one allowed is the user's own >{{.LibPassword}}`},
		{"rl_lib with a literal password", edit("rl_lib", func(l string) string { return l + " >hunter2" }),
			"rl_lib: >***: the only password is the user's own >{{.LibPassword}} field"},
		{"rl_ops with nopass", edit("rl_ops", func(l string) string { return l + " nopass" }),
			`rl_ops: "nopass" is not a rule DESIGN §A2 uses`},
		{"rl_admin with %RW~*", edit("rl_admin", func(l string) string { return strings.Replace(l, "~rl:*", "%RW~*", 1) }),
			"rl_admin: %RW~* grants every key"},
		// The lines themselves: one per §A2 user, each once, and nothing else.
		{"rl_lib twice", string(data) + line("rl_lib") + "\n",
			"deploy/acl.conf.tmpl:6: user rl_lib appears twice"},
		{"an unknown user", string(data) + "user rl_x on >{{.LibPassword}} resetkeys -@all +ping\n",
			`deploy/acl.conf.tmpl:6: unknown user "rl_x"`},
		{"a line that is not a user line", string(data) + "foo rl_lib on\n",
			`deploy/acl.conf.tmpl:6: not a user line: "foo rl_lib on"`},
		{"no rl_ops line", strings.Replace(string(data), line("rl_ops")+"\n", "", 1),
			"deploy/acl.conf.tmpl: no line for user rl_ops"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "acl.conf.tmpl")
			if err := os.WriteFile(p, []byte(c.tmpl), 0o644); err != nil {
				t.Fatal(err)
			}
			problems, err := aclTemplateProblems(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			if countContaining(problems, c.want) != 1 {
				t.Fatalf("want one problem containing %q, got:\n%s", c.want, strings.Join(problems, "\n"))
			}
		})
	}
}

// TestACL_LineShape: each user line reads exactly "user <name> on >{{.XPassword}}
// resetkeys ~rl:* -@all +…" (S0 step 3): a rule missing, repeated or out of place fails,
// and so does a command listed twice. Each edit of the real template gives exactly the
// one problem listed.
func TestACL_LineShape(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), aclTemplate))
	if err != nil {
		t.Fatal(err)
	}
	edit := func(user string, change func(string) string) string {
		lines := strings.Split(string(data), "\n")
		for i, l := range lines {
			if strings.HasPrefix(l, "user "+user+" ") {
				lines[i] = change(l)
			}
		}
		return strings.Join(lines, "\n")
	}
	replace := func(user, old, new string) string {
		return edit(user, func(l string) string {
			if !strings.Contains(l, old) {
				t.Fatalf("the %s line has no %q", user, old)
			}
			return strings.Replace(l, old, new, 1)
		})
	}
	const lib = "the line reads user rl_lib on >{{.LibPassword}} resetkeys ~rl:* -@all +<command> …"
	for _, c := range []struct {
		name, tmpl, want string
	}{
		{"on missing", replace("rl_lib", " on ", " "), "rl_lib: no on rule: " + lib},
		{"password missing", replace("rl_lib", " >{{.LibPassword}}", ""), "rl_lib: no >{{.LibPassword}} rule: " + lib},
		{"resetkeys missing", replace("rl_lib", " resetkeys", ""), "rl_lib: no resetkeys rule: " + lib},
		{"-@all missing", replace("rl_lib", " -@all", ""), "rl_lib: no -@all rule: " + lib},
		{"-@all missing for rl_ops", replace("rl_ops", " -@all", ""),
			"rl_ops: no -@all rule: the line reads user rl_ops on >{{.OpsPassword}} resetkeys -@all +<command> …"},
		// In place of the grants' -@all, a -@all after them revokes every grant before it.
		{"-@all after the grants", edit("rl_lib", func(l string) string { return strings.Replace(l, " -@all", "", 1) + " -@all" }),
			"rl_lib: +evalsha where -@all belongs: " + lib + ", in that order"},
		// A resetkeys after the key pattern drops it.
		{"resetkeys after the key pattern", replace("rl_fixture", "resetkeys ~rl:*", "~rl:* resetkeys"),
			"rl_fixture: ~rl:* where resetkeys belongs: the line reads user rl_fixture on >{{.FixturePassword}} resetkeys ~rl:* -@all +<command> …, in that order"},
		{"on after the password", replace("rl_admin", "on >{{.AdminPassword}}", ">{{.AdminPassword}} on"),
			"rl_admin: >*** where on belongs"},
		{"ON in upper case", replace("rl_lib", " on ", " ON "), "rl_lib: ON where on belongs"},
		// Appended, the pair clears the grants and the key pattern again.
		{"-@all resetkeys appended (-@all)", edit("rl_lib", func(l string) string { return l + " -@all resetkeys" }),
			"rl_lib: -@all appears 2 times: " + lib + ", each rule once"},
		{"-@all resetkeys appended (resetkeys)", edit("rl_lib", func(l string) string { return l + " -@all resetkeys" }),
			"rl_lib: resetkeys appears 2 times: " + lib + ", each rule once"},
		{"-@ALL appended", edit("rl_ops", func(l string) string { return l + " -@ALL" }), "rl_ops: -@all appears 2 times"},
		{"on twice", edit("rl_admin", func(l string) string { return l + " on" }), "rl_admin: on appears 2 times"},
		{"the password field twice", edit("rl_fixture", func(l string) string { return l + " >{{.FixturePassword}}" }),
			"rl_fixture: >{{.FixturePassword}} appears 2 times"},
		{"a command twice", edit("rl_lib", func(l string) string { return l + " +GET" }),
			"rl_lib: +get appears twice: list each command once"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "acl.conf.tmpl")
			if err := os.WriteFile(p, []byte(c.tmpl), 0o644); err != nil {
				t.Fatal(err)
			}
			problems, err := aclTemplateProblems(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			if countContaining(problems, c.want) != 1 {
				t.Fatalf("want one problem containing %q, got:\n%s", c.want, strings.Join(problems, "\n"))
			}
		})
	}
}

// TestACL_NoPasswordInOutput: a password in the template never reaches trace's stdout or
// stderr, whichever rule carries it (>pass, <pass, #hash, !hash, inside a template action
// or a selector, as a user name, a command or a key pattern, or on a line that is not a
// user line); each such token prints as its mark and "***" (AGENTS.md §1). No part of a
// quoted password (with spaces, in single or double quotes, closed or not) or of one
// holding a tab is printed either.
func TestACL_NoPasswordInOutput(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), aclTemplate))
	if err != nil {
		t.Fatal(err)
	}
	hash := "5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8"
	secrets := []string{"hunter2", "hunter3", hash, "hunter5", "hunter6", "hunter7", "hunter8", "hunter9", "hunter10", "hunter11", "hunter12",
		"hunter13", "tail13", "hunter14", "mid14", "tail14", "hunter15", "tail15", "hunter16", "tail16", "hunter17", "tail17", "hunter18", "tail18"}
	tmpl := string(data)
	for _, r := range []struct{ old, new string }{
		{" >{{.LibPassword}}", " >hunter2"},                        // in place of the field
		{" >{{.AdminPassword}}", " <hunter3"},                      // a password removal
		{"+type +eval", "+type +eval #" + hash + " !" + hash},      // a hash and its removal
		{"+shutdown", `+shutdown >{{ "hunter5" }} {{">hunter6"}}`}, // template actions
		{"+zcard", "+zcard ( >hunter7 ~* ) +>hunter8 ~>hunter9"},   // a selector, a command, a key pattern
	} {
		if !strings.Contains(tmpl, r.old) {
			t.Fatalf("%s has no %q", aclTemplate, r.old)
		}
		tmpl = strings.Replace(tmpl, r.old, r.new, 1)
	}
	tmpl += "foo rl_lib on >hunter10\nuser >hunter11 on\nuser\t>hunter12\n" +
		// The store splits at spaces only and keeps quotes, so a quoted password is cut into
		// pieces: a line with a quote outside a template action is never shown, whether the
		// quote is closed or not. A tab splits nothing, so >hunter18<tab>tail18 is one rule.
		"user rl_lib on \">hunter13 tail13\" resetkeys\n" +
		"user rl_ops on '>hunter14 mid14 tail14' -@all\n" +
		"user rl_admin on \">hunter15 tail15\n" +
		"foo \">hunter16\"tail16\n" +
		"user rl_fixture on {{ \">hunter17 tail17\" }} >hunter18\ttail18\n"
	root := t.TempDir()
	writeTestFile(t, root, aclTemplate, tmpl)
	code, stdout, stderr := runIn(t, root, "acl")
	if code != 1 || !strings.Contains(stderr, ">***") || !strings.Contains(stderr, "<***") ||
		!strings.Contains(stderr, "#***") || !strings.Contains(stderr, "!***") ||
		strings.Count(stderr, "a quote (\" or ') outside a template action") != 4 {
		t.Fatalf("exit %d, want 1 with each token redacted and four lines with quotes\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, s := range secrets {
		if strings.Contains(stdout, s) || strings.Contains(stderr, s) {
			t.Errorf("trace acl printed the secret %q\nstdout: %s\nstderr: %s", s, stdout, stderr)
		}
	}
}

func TestACL_ScriptNotAllowed(t *testing.T) {
	// Each fixture's commands outside its user's inner list, and nothing else: an rl_lib
	// script calling HGET, plainly or right after a concatenation (".." is one token, so
	// the call is not taken for a field access x.redis); one calling INCR right after a hex
	// numeral (Lua 5.1 ends 0xE before the minus, so the call is an operand); an rl_admin
	// script calling UNLINK (rl_lib's, not rl_admin's); and read_epoch, which both users
	// run, held to each list: HGET is rl_admin's only, UNLINK rl_lib's only.
	for _, c := range []struct {
		fixture string
		want    []string
	}{
		{"not_allowed", []string{"internal/scripts/acquire.lua:2: HGET is not in rl_lib's inner commands"}},
		{"not_allowed_concat", []string{"internal/scripts/acquire.lua:2: HGET is not in rl_lib's inner commands"}},
		{"not_allowed_hex", []string{"internal/scripts/acquire.lua:2: INCR is not in rl_lib's inner commands"}},
		{"admin_not_allowed", []string{
			"internal/scripts/admin_status.lua:2: UNLINK is not in rl_admin's inner commands",
		}},
		{"read_epoch_both", []string{
			"internal/scripts/read_epoch.lua:2: HGET is not in rl_lib's inner commands",
			"internal/scripts/read_epoch.lua:3: UNLINK is not in rl_admin's inner commands",
		}},
	} {
		problems := aclProblems(t, aclFixture(t, c.fixture))
		for _, w := range c.want {
			if countContaining(problems, w) != 1 {
				t.Errorf("%s: want one problem %q", c.fixture, w)
			}
		}
		if len(problems) != len(c.want) {
			t.Errorf("%s: problems:\n%s", c.fixture, strings.Join(problems, "\n"))
		}
	}
}

func TestACL_DynamicCommand(t *testing.T) {
	// Every line marked want, and nothing else: redis.call(cmd, …), each other form that
	// does not name its command with a whole string literal argument, the API reached as
	// server or through _G, and code compiled at run time by loadstring or load; redis,
	// server, _G, loadstring and load as field or method names pass.
	root := aclFixture(t, "dynamic")
	problems := aclProblems(t, root)
	sameLines(t, "dynamic calls", problemLines(t, problems), wantLines(t, root), problems)
	for _, want := range []string{
		"acquire.lua:3: redis.call with no string literal command",
		"acquire.lua:4: redis.call with no string literal command",
		"acquire.lua:5: redis.call with no string literal command",
		"acquire.lua:6: redis.call with no string literal command",
		"acquire.lua:7: redis.pcall with no string literal command",
		"acquire.lua:8: pcall(redis.call, …) with no string literal command",
		"acquire.lua:9: redis.pcall used other than called as redis.pcall('X', …)",
		"acquire.lua:10: redis used other than as redis.<name>",
		"acquire.lua:12: redis.call used other than called as redis.call('X', …)",
		"acquire.lua:14: redis used other than as redis.<name>",
		"acquire.lua:15: redis used other than as redis.<name>",
		"acquire.lua:16: redis.call used other than called as redis.call('X', …)",
		"acquire.lua:17: redis.call used other than called as redis.call('X', …)",
		"acquire.lua:18: _G reaches any global by name, the store API among them",
		"acquire.lua:19: server is the store API under its Valkey name, which no named form uses",
		"acquire.lua:20: loadstring compiles code at run time, whose store calls trace cannot see",
		"acquire.lua:21: load compiles code at run time, whose store calls trace cannot see",
	} {
		if countContaining(problems, "internal/scripts/"+want) != 1 {
			t.Errorf("want one problem %q", want)
		}
	}
}

func TestACL_SubsetThenEqual(t *testing.T) {
	// A partial rl_lib set that uses a subset of the list passes (all three call forms).
	if problems := aclProblems(t, aclFixture(t, "partial")); len(problems) > 0 {
		t.Fatalf("a partial script set was refused:\n%s", strings.Join(problems, "\n"))
	}
	// All seven rl_lib scripts, with no call to UNLINK: the union misses a listed command.
	problems := aclProblems(t, aclFixture(t, "full_missing"))
	if len(problems) != 1 || !strings.Contains(problems[0], "all of rl_lib's scripts exist, but none calls UNLINK") {
		t.Fatalf("the full set missing UNLINK was not reported:\n%s", strings.Join(problems, "\n"))
	}
}

func TestACL_Fragment(t *testing.T) {
	problems := aclProblems(t, aclFixture(t, "fragment"))
	if len(problems) != 1 || !strings.Contains(problems[0], "internal/scripts/prelude.lua:2: UNLINK is not in rl_admin's inner commands") {
		t.Fatalf("a prelude command outside rl_admin's list was not reported:\n%s", strings.Join(problems, "\n"))
	}
	problems = aclProblems(t, aclFixture(t, "unknown"))
	if len(problems) != 1 || !strings.Contains(problems[0], "internal/scripts/foo.lua: not a DESIGN §A4.1 script name") {
		t.Fatalf("an unknown foo.lua was not reported:\n%s", strings.Join(problems, "\n"))
	}
}
