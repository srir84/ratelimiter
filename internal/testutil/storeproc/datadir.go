package storeproc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// aofDirName is the store's data directory inside Store.Dir: Valkey's appenddirname, which
// the deploy/ profiles leave at its default. With save "" (DESIGN §A2) it holds all the data.
const aofDirName = "appendonlydir"

// errRunning refuses a data-directory helper while the store process runs: its files are
// still being written.
var errRunning = errors.New("the store is running: stop it first (Stop or Kill)")

// CopyDataDir copies the store's data directory, appendonlydir/, to dst (IMPLEMENTATION_PLAN
// S8 step 6), which must not exist yet or be an empty directory; afterwards dst holds a copy
// of every file. The store must be stopped (Stop, Kill, or a process that exited), so the
// files are complete; ReplaceDataDir(dst) brings the copy back.
func (s *Store) CopyDataDir(dst string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.stoppedLocked(); err != nil {
		return fmt.Errorf("copying the data directory: %w", err)
	}
	if err := copyDir(filepath.Join(s.Dir, aofDirName), dst); err != nil {
		return fmt.Errorf("copying the data directory: %w", err)
	}
	return nil
}

// ReplaceDataDir replaces the store's appendonlydir/ with a copy of src, a directory that
// CopyDataDir wrote (IMPLEMENTATION_PLAN S8 step 6); src itself is left as it is. The store
// must be stopped; Restart then loads src's data.
func (s *Store) ReplaceDataDir(src string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.stoppedLocked(); err != nil {
		return fmt.Errorf("replacing the data directory: %w", err)
	}
	aof := filepath.Join(s.Dir, aofDirName)
	si, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("replacing the data directory: %w", err)
	}
	if !si.IsDir() {
		return fmt.Errorf("replacing the data directory: %s is not a directory", src)
	}
	if ai, err := os.Stat(aof); err == nil && os.SameFile(si, ai) {
		return fmt.Errorf("replacing the data directory: %s is the store's own data directory", src)
	}
	if err := os.RemoveAll(aof); err != nil {
		return fmt.Errorf("replacing the data directory: %w", err)
	}
	if err := copyDir(src, aof); err != nil {
		return fmt.Errorf("replacing the data directory: %w", err)
	}
	return nil
}

// WipeDataDir removes the store's appendonlydir/ (IMPLEMENTATION_PLAN S8 step 6), as a lost
// disk would; the store must be stopped. Restart then starts it with no data.
func (s *Store) WipeDataDir() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.stoppedLocked(); err != nil {
		return fmt.Errorf("wiping the data directory: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(s.Dir, aofDirName)); err != nil {
		return fmt.Errorf("wiping the data directory: %w", err)
	}
	return nil
}

// stoppedLocked returns errRunning while the store process runs; s.mu is held, so no Restart
// can start it meanwhile.
func (s *Store) stoppedLocked() error {
	if s.proc == nil {
		return nil
	}
	select {
	case <-s.proc.done:
		return nil
	default:
		return errRunning
	}
}

// copyDir copies the regular files of the flat directory src into dst, which must not exist
// or be empty. Anything but a regular file in src is an error: the data directory holds only
// the AOF manifest and its files.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if left, err := os.ReadDir(dst); err != nil {
		return err
	} else if len(left) > 0 {
		return fmt.Errorf("%s is not empty", dst)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			return fmt.Errorf("%s holds %s, which is not a regular file", src, e.Name())
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies src to dst, a new file, and syncs it.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // read-only: nothing to flush
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copying %s: %w", filepath.Base(src), err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", filepath.Base(dst), err)
	}
	return nil
}
