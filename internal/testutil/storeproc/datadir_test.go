package storeproc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFiles writes name → content files into dir, creating it.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// readFiles returns dir's files as name → content, nil when dir does not exist.
func readFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func sameFiles(a, b map[string]string) bool {
	if len(a) != len(b) || (a == nil) != (b == nil) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestDataDirHelpers (IMPLEMENTATION_PLAN S8 step 6): CopyDataDir, ReplaceDataDir and
// WipeDataDir refuse while the store process runs and then change nothing; once it is
// stopped (killed, or exited by itself) they copy, replace and remove appendonlydir/ exactly,
// and touch nothing else in the store's directory; a source that is no directory, or a
// destination that is not empty, is refused with nothing changed. The process is /bin/sleep:
// the helpers judge only by it and the files.
func TestDataDirHelpers(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Bin: "/bin/sleep", Args: []string{"/bin/sleep", "30"}, tb: t, root: t.TempDir()}
	aof := filepath.Join(s.Dir, "appendonlydir")
	gen1 := map[string]string{
		"appendonly.aof.1.base.rdb": "base 1", "appendonly.aof.1.incr.aof": "incr 1", "appendonly.aof.manifest": "manifest 1",
	}
	writeFiles(t, aof, gen1)
	others := map[string]string{"acl.conf": "acl", "store.log": "log"}
	writeFiles(t, s.Dir, others)
	src := filepath.Join(t.TempDir(), "src")
	writeFiles(t, src, map[string]string{"appendonly.aof.manifest": "other"})

	// Running: every helper refuses, and nothing changes.
	if err := s.spawn(); err != nil {
		t.Fatal(err)
	}
	copyDst := filepath.Join(t.TempDir(), "copy")
	for name, err := range map[string]error{
		"CopyDataDir": s.CopyDataDir(copyDst), "ReplaceDataDir": s.ReplaceDataDir(src), "WipeDataDir": s.WipeDataDir(),
	} {
		if !errors.Is(err, errRunning) {
			t.Errorf("%s while running: %v, want errRunning", name, err)
		}
	}
	if got := readFiles(t, aof); !sameFiles(got, gen1) {
		t.Errorf("appendonlydir/ after the refusals: %v, want %v", got, gen1)
	}
	if _, err := os.Stat(copyDst); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused copy created %s (%v)", copyDst, err)
	}
	s.Kill()

	// Stopped: copy, then change the data, then replace with the copy.
	if err := s.CopyDataDir(copyDst); err != nil {
		t.Fatal(err)
	}
	if got := readFiles(t, copyDst); !sameFiles(got, gen1) {
		t.Errorf("the copy holds %v, want %v", got, gen1)
	}
	gen2 := map[string]string{
		"appendonly.aof.2.base.rdb": "base 2", "appendonly.aof.2.incr.aof": "incr 2", "appendonly.aof.manifest": "manifest 2",
	}
	if err := os.RemoveAll(aof); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, aof, gen2)
	if err := s.ReplaceDataDir(copyDst); err != nil {
		t.Fatal(err)
	}
	if got := readFiles(t, aof); !sameFiles(got, gen1) {
		t.Errorf("appendonlydir/ after the replace: %v, want the copy %v", got, gen1)
	}
	if got := readFiles(t, copyDst); !sameFiles(got, gen1) {
		t.Errorf("the replace changed its source: %v", got)
	}
	// A copy goes only into a new or empty directory; the store's own directory is no source.
	if err := s.CopyDataDir(copyDst); err == nil {
		t.Error("CopyDataDir into a directory that holds files succeeded")
	}
	if err := s.ReplaceDataDir(aof); err == nil {
		t.Error("ReplaceDataDir from the store's own appendonlydir/ succeeded")
	}
	if got := readFiles(t, aof); !sameFiles(got, gen1) {
		t.Errorf("appendonlydir/ after the refused calls: %v, want %v", got, gen1)
	}
	// A regular file is no source, and the data stays; a destination holding any other file
	// gets nothing copied.
	notDir := filepath.Join(t.TempDir(), "appendonly.aof.manifest")
	if err := os.WriteFile(notDir, []byte("manifest 3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceDataDir(notDir); err == nil {
		t.Error("ReplaceDataDir from a regular file succeeded")
	}
	if got := readFiles(t, aof); !sameFiles(got, gen1) {
		t.Errorf("appendonlydir/ after ReplaceDataDir from a regular file: %v, want %v", got, gen1)
	}
	busy := filepath.Join(t.TempDir(), "busy")
	unrelated := map[string]string{"notes.txt": "keep"}
	writeFiles(t, busy, unrelated)
	if err := s.CopyDataDir(busy); err == nil {
		t.Error("CopyDataDir into a directory holding an unrelated file succeeded")
	}
	if got := readFiles(t, busy); !sameFiles(got, unrelated) {
		t.Errorf("CopyDataDir into a directory holding an unrelated file left %v, want only %v", got, unrelated)
	}

	// A process that exited by itself is stopped too: wipe.
	s.Args = []string{"/usr/bin/true"}
	if err := s.spawn(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.proc.done:
	case <-time.After(10 * time.Second):
		t.Fatal("/usr/bin/true still running after 10s")
	}
	if err := s.WipeDataDir(); err != nil {
		t.Fatal(err)
	}
	if got := readFiles(t, aof); got != nil {
		t.Errorf("appendonlydir/ after the wipe: %v, want none", got)
	}
	if err := s.WipeDataDir(); err != nil {
		t.Errorf("a second wipe: %v, want nil (nothing left to remove)", err)
	}
	if err := s.CopyDataDir(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("CopyDataDir with no data directory succeeded")
	}
	for name, content := range others {
		if b, err := os.ReadFile(filepath.Join(s.Dir, name)); err != nil || string(b) != content {
			t.Errorf("%s changed: %q, %v", name, b, err)
		}
	}
	s.Stop()
}
