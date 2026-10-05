package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// findRoot returns the nearest directory at or above dir that holds a go.mod, with every
// symlink in its path resolved: from a symlinked checkout (os.Getwd gives $PWD) the walks
// then read the real tree, as filepath.WalkDir never descends into a root that is a
// symlink. A done ctx stops the search with its error.
func findRoot(ctx context.Context, dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("absolute path of %s: %w", dir, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("looking for go.mod: %w", err)
		}
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			real, err := filepath.EvalSymlinks(d)
			if err != nil {
				return "", fmt.Errorf("resolving the repository root %s: %w", d, err)
			}
			return real, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod in %s or any parent directory", dir)
		}
		d = parent
	}
}

// modulePath reads the module line of root/go.mod as the go command does: a trailing
// "//" comment is not part of the path, and a quoted path is unquoted
// (`module ratelimiter // …` and `module "ratelimiter"` both give ratelimiter).
func modulePath(ctx context.Context, root string) (string, error) {
	data, err := readFile(ctx, root, "go.mod")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		code, _, _ := strings.Cut(line, "//")
		f := strings.Fields(code)
		if len(f) < 2 || f[0] != "module" {
			continue
		}
		p := f[1]
		if strings.HasPrefix(p, `"`) || strings.HasPrefix(p, "`") {
			if p, err = strconv.Unquote(p); err != nil {
				return "", fmt.Errorf("go.mod: module path %s: %w", f[1], err)
			}
		}
		return p, nil
	}
	return "", errors.New("go.mod has no module line")
}

// skipDir reports whether every walk skips the directory at the root-relative slash path
// rel (S0 step 6): bin/ and runs/ only directly under the root; testdata/ (so trace's own
// fixtures never count) and every directory whose name starts with "." (.git, .data and
// .out among them) at any depth.
func skipDir(rel string) bool {
	name := path.Base(rel)
	switch {
	case name == "testdata", strings.HasPrefix(name, "."):
		return true
	case strings.Contains(rel, "/"):
		return false
	}
	return name == "bin" || name == "runs"
}

// goSkipDir reports whether walks of .go files skip the directory rel: one that skipDir
// names, or one whose name starts with "_", which go ./... leaves out. The "_" rule holds
// for .go files only: mypy, ruff, pytest and stylua read such a directory, so the walks
// of .py and .lua files do too.
func goSkipDir(rel string) bool {
	return skipDir(rel) || strings.HasPrefix(path.Base(rel), "_")
}

// inGoSkippedDir reports whether the directory at the root-relative slash path dir is
// one that walks of .go files skip, or lies inside one.
func inGoSkippedDir(dir string) bool {
	for d := dir; d != "." && d != ""; d = path.Dir(d) {
		if goSkipDir(d) {
			return true
		}
	}
	return false
}

// repoFiles walks the tree under root (never git ls-files) and returns the
// slash-separated, root-relative paths of the files that keep accepts, in lexical
// order. It leaves out the directories skipDir names, and a .go file in a "_" directory
// (inGoSkippedDir), while a .py or .lua file there is kept. A symlink to a regular file
// counts as that file (os.Stat), since the go tool reads it as one. A symlink to a
// directory is returned as a problem (plan choice) unless its name is one the walk skips:
// the walk does not follow it, while an import can still reach the code behind it. A
// symlink whose target cannot be read is passed over, as the go tool either ignores it or
// fails on it. A done ctx stops the walk with its error.
func repoFiles(ctx context.Context, root string, keep func(rel string) bool) (files, problems []string, err error) {
	return walkRepo(ctx, root, skipDir, func(rel string) bool {
		if strings.HasSuffix(rel, ".go") && inGoSkippedDir(path.Dir(rel)) {
			return false // go ./... leaves it out
		}
		return keep(rel)
	})
}

// goFiles is repoFiles for a walk of .go files only: it returns the .go files that keep
// accepts (nil keeps them all), and it skips every directory goSkipDir names, as go ./...
// does, so it also passes over a symlink named like a "_" directory.
func goFiles(ctx context.Context, root string, keep func(rel string) bool) (files, problems []string, err error) {
	return walkRepo(ctx, root, goSkipDir, func(rel string) bool {
		return strings.HasSuffix(rel, ".go") && (keep == nil || keep(rel))
	})
}

// walkRepo is the walk behind repoFiles and goFiles; skip names the directories it
// leaves out, and the directory symlinks it passes over.
func walkRepo(ctx context.Context, root string, skip, keep func(rel string) bool) (files, problems []string, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("relative path of %s: %w", p, err)
		}
		rel = filepath.ToSlash(rel)
		mode := d.Type()
		if mode&fs.ModeSymlink != 0 {
			info, serr := os.Stat(p)
			if serr != nil {
				return nil
			}
			if info.IsDir() {
				if !skip(rel) { // a symlink named like a skipped directory is passed over as one
					problems = append(problems, fmt.Sprintf("%s: a symlink to a directory: trace does not follow it, and go ./... ignores it, yet an import reaches the code behind it; put the directory itself there", rel))
				}
				return nil
			}
			mode = info.Mode().Type()
		}
		if d.IsDir() {
			if skip(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if mode.IsRegular() && keep(rel) {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walking %s: %w", root, err)
	}
	return files, problems, nil
}

// appendNew appends the items that list does not hold yet: two walks of one check
// report the same symlink once.
func appendNew(list []string, items ...string) []string {
	for _, it := range items {
		if !slices.Contains(list, it) {
			list = append(list, it)
		}
	}
	return list
}

// withSuffix keeps the files whose path ends with suffix.
func withSuffix(suffix string) func(string) bool {
	return func(rel string) bool { return strings.HasSuffix(rel, suffix) }
}

// under reports whether the slash path rel lies inside the directory dir.
func under(rel, dir string) bool {
	return strings.HasPrefix(rel, strings.TrimSuffix(dir, "/")+"/")
}

// pkgPath is the import path of the package holding the root-relative file rel.
func pkgPath(module, rel string) string {
	if dir := path.Dir(rel); dir != "." {
		return module + "/" + dir
	}
	return module
}

// parseGo parses root/rel, naming the file by its relative path so that positions
// and parse errors read "rel:line:col". Build constraints are ignored: every file is
// parsed, whatever its tags.
func parseGo(ctx context.Context, fset *token.FileSet, root, rel string, mode parser.Mode) (*ast.File, error) {
	src, err := readFile(ctx, root, rel)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(fset, rel, src, mode|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %w", err)
	}
	return f, nil
}

// rootPath resolves a slash path relative to root; an absolute path stays as it is.
func rootPath(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, filepath.FromSlash(p))
}

// readFile reads rel (relative to root, or absolute) for a caller. A done ctx stops it
// before the read, with its error.
func readFile(ctx context.Context, root, rel string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", rel, err)
	}
	data, err := os.ReadFile(rootPath(root, rel))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", rel, err)
	}
	return data, nil
}

// readLines returns the lines of rel (relative to root, or absolute), without their line endings.
func readLines(ctx context.Context, root, rel string) ([]string, error) {
	data, err := readFile(ctx, root, rel)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

// writeFile writes root/rel, creating its directory. A done ctx stops it before it
// writes anything, with its error.
func writeFile(ctx context.Context, root, rel, content string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(rel), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	return nil
}

// fileExists reports whether root/rel is an existing regular file. A done ctx stops it
// before it looks, with its error.
func fileExists(ctx context.Context, root, rel string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("looking for %s: %w", rel, err)
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.Mode().IsRegular(), nil
}
