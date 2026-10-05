package storeproc

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

// Passwords are the four ACL users' passwords (DESIGN §A2), fresh for every store. They
// live only in the rendered ACL file (mode 0600, in the store's directory) and reach
// other processes only through Env, never through code, TOML, errors or logs.
type Passwords struct {
	Lib, Admin, Fixture, Ops string
}

// NewPasswords draws four passwords, each 32 crypto/rand bytes in hex (plan choice).
func NewPasswords() Passwords {
	return Passwords{Lib: newPassword(), Admin: newPassword(), Fixture: newPassword(), Ops: newPassword()}
}

func newPassword() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error and always fills b
	return hex.EncodeToString(b)
}

// RenderACL renders the ACL template tmplPath (deploy/acl.conf.tmpl: Go text/template
// with the fields LibPassword, AdminPassword, FixturePassword and OpsPassword) into
// outPath with mode 0600. Errors never contain a password.
func RenderACL(tmplPath, outPath string, pw Passwords) error {
	for _, p := range [...]struct{ user, pw string }{
		{"rl_lib", pw.Lib}, {"rl_admin", pw.Admin}, {"rl_fixture", pw.Fixture}, {"rl_ops", pw.Ops},
	} {
		if err := checkPassword(p.pw); err != nil {
			return fmt.Errorf("rendering the ACL file: %s password: %w", p.user, err)
		}
	}
	src, err := os.ReadFile(tmplPath)
	if err != nil {
		return fmt.Errorf("rendering the ACL file: %w", err)
	}
	tmpl, err := template.New(filepath.Base(tmplPath)).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return fmt.Errorf("rendering the ACL file: parsing %s: %w", tmplPath, err)
	}
	var out bytes.Buffer
	data := struct{ LibPassword, AdminPassword, FixturePassword, OpsPassword string }{pw.Lib, pw.Admin, pw.Fixture, pw.Ops}
	if err := tmpl.Execute(&out, data); err != nil {
		return fmt.Errorf("rendering the ACL file: executing %s: %w", tmplPath, err)
	}
	f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("rendering the ACL file: %w", err)
	}
	// An existing file keeps its old mode through O_TRUNC, so set it explicitly.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("rendering the ACL file: %w", err)
	}
	if _, err := f.Write(out.Bytes()); err != nil {
		_ = f.Close()
		return fmt.Errorf("rendering the ACL file: writing %s: %w", outPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("rendering the ACL file: closing %s: %w", outPath, err)
	}
	return nil
}

// checkPassword accepts a password the ACL file's ">password" rule can carry: non-empty
// printable ASCII without spaces.
func checkPassword(pw string) error {
	if pw == "" {
		return errors.New("empty")
	}
	for i := 0; i < len(pw); i++ {
		if pw[i] <= ' ' || pw[i] > '~' {
			return errors.New("must be printable ASCII without spaces")
		}
	}
	return nil
}

// Env returns the store's passwords as the RL_*_PASSWORD variables (DESIGN §A2), for a
// child process's explicit environment.
func (s *Store) Env() []string {
	return []string{
		"RL_LIB_PASSWORD=" + s.PW.Lib,
		"RL_ADMIN_PASSWORD=" + s.PW.Admin,
		"RL_FIXTURE_PASSWORD=" + s.PW.Fixture,
		"RL_OPS_PASSWORD=" + s.PW.Ops,
	}
}
