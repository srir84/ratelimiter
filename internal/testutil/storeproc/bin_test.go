package storeproc

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeBinary writes an executable shell script named name into dir.
func fakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho \"Valkey server v=9.1.2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBinOrder(t *testing.T) {
	pathDir := t.TempDir()
	valkey := fakeBinary(t, pathDir, "valkey-server")
	redis := fakeBinary(t, pathDir, "redis-server")
	custom := fakeBinary(t, t.TempDir(), "custom-store")
	t.Setenv("PATH", pathDir)

	want := func(step, path string) {
		t.Helper()
		got, err := Bin()
		if err != nil {
			t.Fatalf("%s: Bin() error %v, want %s", step, err, path)
		}
		if got != path {
			t.Fatalf("%s: Bin() = %s, want %s", step, got, path)
		}
	}

	t.Setenv("RL_STORE_BIN", custom)
	want("RL_STORE_BIN set", custom)

	t.Setenv("RL_STORE_BIN", filepath.Join(t.TempDir(), "missing"))
	if got, err := Bin(); err == nil {
		t.Fatalf("RL_STORE_BIN naming a missing file: Bin() = %s, want an error (no fallback)", got)
	}

	t.Setenv("RL_STORE_BIN", "")
	want("RL_STORE_BIN empty, both on PATH", valkey)

	if err := os.Remove(valkey); err != nil {
		t.Fatal(err)
	}
	want("only redis-server on PATH", redis)

	if err := os.Remove(redis); err != nil {
		t.Fatal(err)
	}
	if got, err := Bin(); err == nil {
		t.Fatalf("no store binary: Bin() = %s, want an error", got)
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		name, out        string
		product, version string
		ok               bool
	}{
		{"valkey 9.1.2", "Valkey server v=9.1.2 sha=00000000:1 malloc=libc bits=64 build=1af85cd1d1bb3237\n", "Valkey", "9.1.2", true},
		{"redis 8.6.2", "Redis server v=8.6.2 sha=00000000:1 malloc=libc bits=64 build=5d2d9a5aed5a5a8b\n", "Redis", "8.6.2", true},
		{"multi-digit parts", "Valkey server v=10.12.103 sha=x\n", "Valkey", "10.12.103", true},
		{"empty", "", "", "", false},
		{"no v=", "Valkey server 9.1.2\n", "", "", false},
		{"two parts", "Valkey server v=9.1 sha=x\n", "", "", false},
		{"letters in version", "Valkey server v=9.1.x sha=x\n", "", "", false},
		{"not a server line", "Valkey client v=9.1.2\n", "", "", false},
		{"product not a word", "valkey-server server v=9.1.2\n", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			product, version, err := parseVersion(c.out)
			if (err == nil) != c.ok {
				t.Fatalf("parseVersion(%q) error = %v, want ok=%v", c.out, err, c.ok)
			}
			if product != c.product || version != c.version {
				t.Fatalf("parseVersion(%q) = %q, %q; want %q, %q", c.out, product, version, c.product, c.version)
			}
		})
	}
}
