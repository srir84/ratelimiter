// Package storeproc starts and stops throwaway stores for tests (IMPLEMENTATION_PLAN S1,
// DESIGN §C1): it finds the store binary, renders the ACL file with fresh passwords,
// spawns the store on a free loopback port with a deploy/ profile, records the child in
// .out/procs/ for bin/trace procs, pauses and resumes it and fills it with padding (S9:
// Pause, Resume, LoadPadding), and offers a small RESP2 client.
//
// It is a test helper: production code never imports it.
package storeproc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// versionTimeout bounds `<bin> --version`.
const versionTimeout = 10 * time.Second

// Bin returns the store binary (DESIGN §C1): $RL_STORE_BIN when it is set, else
// valkey-server, else redis-server, each looked up on PATH. A redis-server fallback
// may run the other test layers but is not conformance-eligible (§C2).
func Bin() (string, error) {
	if v := os.Getenv("RL_STORE_BIN"); v != "" {
		p, err := exec.LookPath(v)
		if err != nil {
			return "", fmt.Errorf("RL_STORE_BIN %q: %w", v, err)
		}
		return p, nil
	}
	for _, name := range [...]string{"valkey-server", "redis-server"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no store binary: set RL_STORE_BIN or install valkey-server (make setup)")
}

// Version runs `<bin> --version` and returns the product and its X.Y.Z version from the
// "<Product> server v=X.Y.Z ..." line, for example "Valkey" and "9.1.2".
func Version(ctx context.Context, bin string) (product, version string, err error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", "", fmt.Errorf("running %s --version: %w", bin, err)
	}
	return parseVersion(string(out))
}

// parseVersion reads "<Product> server v=X.Y.Z ..." (the first line of --version).
func parseVersion(out string) (product, version string, err error) {
	line, _, _ := strings.Cut(out, "\n")
	f := strings.Fields(line)
	if len(f) >= 3 && f[1] == "server" && isWord(f[0]) {
		if v, ok := strings.CutPrefix(f[2], "v="); ok && isVersion(v) {
			return f[0], v, nil
		}
	}
	if len(line) > 120 {
		line = line[:120] + "…"
	}
	return "", "", fmt.Errorf("store version line %q: want \"<Product> server v=X.Y.Z ...\"", line)
}

// isWord reports whether s is a non-empty run of ASCII letters (a product name).
func isWord(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return s != ""
}

// isVersion reports whether s is X.Y.Z, three non-empty runs of decimal digits.
func isVersion(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}

// ModuleRoot returns the module root: the nearest directory at or above the working
// directory that holds go.mod. deploy/, .out/procs/ and .data/ resolve from it.
func ModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("finding the module root: %w", err)
	}
	for {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("finding the module root: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("finding the module root: no go.mod at or above the working directory")
		}
		dir = parent
	}
}
