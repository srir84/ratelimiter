package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// TestRLAdminBin: RLAdminBin returns $RL_RLADMIN_BIN when it names a file, and
// <module root>/bin/test/rladmin when the variable is unset or empty; a path that does not
// exist, or a directory, is an error.
func TestRLAdminBin(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rladmin")
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(rladminEnv, file)
	if got, err := RLAdminBin(); err != nil || got != file {
		t.Errorf("with %s set to a file: %q, %v; want %q", rladminEnv, got, err, file)
	}
	for _, bad := range []string{filepath.Join(dir, "missing"), dir} {
		t.Setenv(rladminEnv, bad)
		if got, err := RLAdminBin(); err == nil {
			t.Errorf("with %s = %s: %q, no error; want one", rladminEnv, bad, got)
		}
	}
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "bin", "test", "rladmin")
	t.Setenv(rladminEnv, "")
	got, err := RLAdminBin()
	if _, statErr := os.Stat(want); statErr == nil {
		if err != nil || got != want {
			t.Errorf("with %s empty: %q, %v; want %q", rladminEnv, got, err, want)
		}
	} else if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("with %s empty and no %s: %q, %v; want an error naming it", rladminEnv, want, got, err)
	}
}
