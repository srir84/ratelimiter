package testutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"ratelimiter/internal/testutil/storeproc"
)

// rladminEnv overrides the rladmin binary the harness driver runs (IMPLEMENTATION_PLAN S8 step
// 5): only internal/testutil reads the environment in test helpers (AGENTS.md §1).
const rladminEnv = "RL_RLADMIN_BIN"

// RLAdminBin returns the rladmin binary the harness driver runs (IMPLEMENTATION_PLAN S8 step
// 5): $RL_RLADMIN_BIN when it is set, else <module root>/bin/test/rladmin, which make build
// writes (make test-integration builds first). A file that does not exist, or is a directory,
// is an error: a failure for the caller, never a skip.
func RLAdminBin() (string, error) {
	path, ok := os.LookupEnv(rladminEnv)
	if !ok || path == "" {
		root, err := storeproc.ModuleRoot()
		if err != nil {
			return "", fmt.Errorf("finding rladmin: %w", err)
		}
		path = filepath.Join(root, "bin", "test", "rladmin")
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("rladmin binary %s does not exist: run make build, or set %s", path, rladminEnv)
	case err != nil:
		return "", fmt.Errorf("rladmin binary %s: %w", path, err)
	case fi.IsDir():
		return "", fmt.Errorf("rladmin binary %s is a directory", path)
	}
	return path, nil
}
