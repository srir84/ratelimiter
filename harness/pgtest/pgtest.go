//go:build rltest

// Package pgtest starts the harness PostgreSQL cluster for tests (IMPLEMENTATION_PLAN H1 step
// 5): the cluster make setup initialized in .data/pg (127.0.0.1:5433, role rl, database hits;
// DESIGN §C8). The server is shared by every test and left running; tests never record it as
// a child process. A missing pg_ctl or a server that cannot start is a failure, never a skip.
package pgtest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
)

// The cluster's directory and log, relative to the module root (DESIGN §C8).
const (
	dataDir = ".data/pg"
	logFile = ".data/pg.log"
)

// waitDelay bounds how long a pg_ctl run may keep its output open after it exits.
const waitDelay = 10 * time.Second

// Start runs `pg_ctl -D .data/pg status || pg_ctl -D .data/pg -l .data/pg.log -w start` from
// the module root (the nearest go.mod). A start that fails while another caller's start
// brought the server up counts as started. pg_ctl -w bounds the start itself (60 s by default).
func Start(ctx context.Context) error {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		return fmt.Errorf("pgtest: %w", err)
	}
	if pgCtl(ctx, root, "-D", dataDir, "status") == nil {
		return nil
	}
	startErr := pgCtl(ctx, root, "-D", dataDir, "-l", logFile, "-w", "start")
	if startErr == nil || pgCtl(ctx, root, "-D", dataDir, "status") == nil {
		return nil
	}
	return fmt.Errorf("pgtest: starting PostgreSQL in %s (see %s): %w",
		filepath.Join(root, dataDir), filepath.Join(root, logFile), startErr)
}

// Ensure starts the cluster unless it is running, and fails t if it cannot (StartStack and
// other tests that have a t; TestMain calls Start).
func Ensure(t testing.TB) {
	t.Helper()
	if err := Start(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// pgCtl runs pg_ctl with args in dir. Its error carries pg_ctl's output.
func pgCtl(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "pg_ctl", args...)
	cmd.Dir = dir
	cmd.WaitDelay = waitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_ctl %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
