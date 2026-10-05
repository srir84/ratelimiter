package storeproc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRenderACL(t *testing.T) {
	root, err := ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(root, "deploy", "acl.conf.tmpl")
	out := filepath.Join(t.TempDir(), "acl.conf")
	// A file already there with a wider mode must end up 0600 too.
	if err := os.WriteFile(out, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pw := NewPasswords()
	if err := RenderACL(tmpl, out, pw); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %v, want 0600", mode)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, p := range aclProblems(text, pw) {
		t.Error(p)
	}

	// The failure messages never show the rendered file, which holds the passwords: break
	// it in each way aclProblems looks for, and no message may carry a password.
	lines := strings.Split(text, "\n")
	breaks := map[string]func(i int, line string) string{
		"token before on": func(i int, line string) string {
			return strings.Replace(line, "user rl_lib on ", "user rl_lib reset on ", 1)
		},
		"line 1 not default off": func(i int, line string) string {
			if i == 0 {
				return lines[1]
			}
			return line
		},
		"template action left": func(i int, line string) string {
			if i == 2 {
				return line + " {{.Missing}}"
			}
			return line
		},
		"password before the user": func(i int, line string) string {
			return strings.Replace(line, "user rl_ops on >"+pw.Ops, "user >"+pw.Ops+" rl_ops on", 1)
		},
	}
	for name, edit := range breaks {
		broken := make([]string, len(lines))
		for i, line := range lines {
			broken[i] = edit(i, line)
		}
		probs := aclProblems(strings.Join(broken, "\n"), pw)
		if len(probs) == 0 {
			t.Errorf("%s: aclProblems found nothing wrong", name)
		}
		for _, p := range probs {
			for _, secret := range [...]string{pw.Lib, pw.Admin, pw.Fixture, pw.Ops} {
				if strings.Contains(p, secret) {
					t.Errorf("%s: a failure message shows a password", name)
				}
			}
		}
	}

	if err := RenderACL(tmpl, filepath.Join(t.TempDir(), "bad.conf"), Passwords{Lib: "has space", Admin: "a", Fixture: "f", Ops: "o"}); err == nil {
		t.Error("a password with a space rendered; want an error")
	} else if strings.Contains(err.Error(), "has space") {
		t.Error("the RenderACL error shows the password")
	}
}

// aclProblems checks a rendered ACL file: no template action left, line 1
// "user default off", then one line per DESIGN §A2 user whose fourth field is
// ">" + that user's password from pw (four distinct 64-hex passwords). Each problem names
// a line number, a user or the check that failed, never rendered text: the file holds
// the passwords (AGENTS.md: no password in an error or a log).
func aclProblems(text string, pw Passwords) []string {
	var probs []string
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		if strings.Contains(line, "{{") {
			probs = append(probs, fmt.Sprintf("line %d still holds a template action", i+1))
		}
	}
	if lines[0] != "user default off" {
		probs = append(probs, `line 1 is not "user default off"`)
	}
	users := [...]string{"rl_lib", "rl_admin", "rl_fixture", "rl_ops"}
	want := map[string]string{"rl_lib": pw.Lib, "rl_admin": pw.Admin, "rl_fixture": pw.Fixture, "rl_ops": pw.Ops}
	got := map[string]string{}
	for i, line := range lines[1:] {
		n := i + 2
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != "user" {
			probs = append(probs, fmt.Sprintf("line %d is not a user line with four fields", n))
			continue
		}
		if _, ok := want[f[1]]; !ok {
			probs = append(probs, fmt.Sprintf("line %d: field 2 is not a user of DESIGN §A2", n))
			continue
		}
		if !strings.HasPrefix(f[3], ">") {
			probs = append(probs, fmt.Sprintf("line %d (user %s): field 4 is not the >password rule", n, f[1]))
			continue
		}
		got[f[1]] = strings.TrimPrefix(f[3], ">")
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]string{}
	for _, user := range users {
		p := want[user]
		if !hex64.MatchString(p) {
			probs = append(probs, user+": the password is not 64 hex characters")
		}
		if other, dup := seen[p]; dup {
			probs = append(probs, fmt.Sprintf("%s and %s share a password", user, other))
		}
		seen[p] = user
		if got[user] != p {
			probs = append(probs, user+": no line renders this user's password from Passwords")
		}
	}
	if len(got) != len(want) {
		probs = append(probs, fmt.Sprintf("%d users rendered, want the four of DESIGN §A2", len(got)))
	}
	return probs
}
