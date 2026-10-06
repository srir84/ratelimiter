package replies

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRecordReply records a reply where internal/testutil/scriptrun's Run records one: in the
// file named by the variable that $MUTFIX_REPLIES_ENV names (scriptrun.RepliesEnv, which
// TestMutate_RepliesFile passes, so this reads the variable scriptrun reads) or, when that
// variable is unset, in the module's .out/replies-l2.jsonl. It lists that file in $MUTFIX_SEEN,
// which TestMutate_RepliesFile reads, then checks Code.
func TestRecordReply(t *testing.T) {
	seen := os.Getenv("MUTFIX_SEEN")
	if seen == "" {
		t.Fatal("MUTFIX_SEEN is not set")
	}
	name := os.Getenv("MUTFIX_REPLIES_ENV")
	if name == "" {
		t.Fatal("MUTFIX_REPLIES_ENV is not set")
	}
	path := os.Getenv(name)
	if path == "" {
		root, err := moduleRoot()
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(root, ".out", "replies-l2.jsonl")
	}
	appendLine(t, path, `{"script":"fixture","code":1,"reason":""}`)
	appendLine(t, seen, path)
	if got := Code(1); got != 1 {
		t.Errorf("Code(1) = %d, want 1", got)
	}
	if got := Code(0); got != 0 {
		t.Errorf("Code(0) = %d, want 0", got)
	}
}

// appendLine appends line to the file at path with one write, creating its directory.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, werr := f.WriteString(line + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		t.Fatal(err)
	}
}

// moduleRoot returns the nearest directory at or above the working directory that holds go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod at or above the working directory")
		}
		dir = parent
	}
}
