package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is the real repository root (the tests run in tools/trace).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRoot(t.Context(), ".")
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	return root
}

// testdataDir is the absolute path of a fixture directory under testdata/.
func testdataDir(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("fixture path: %v", err)
	}
	return p
}

// fixture copies the named testdata directories, merged, into a fresh temporary root.
func fixture(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		copyTree(t, testdataDir(t, n), root)
	}
	return root
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copying %s: %v", src, err)
	}
}

// copyFile copies one file of the real repository into root at the same relative path.
func copyFile(t *testing.T, root, rel string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	writeTestFile(t, root, rel, string(data))
}

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := writeFile(t.Context(), root, rel, content); err != nil {
		t.Fatal(err)
	}
}

// walkFiles is repoFiles for a test: an error or a walk problem fails the test.
func walkFiles(t *testing.T, root string, keep func(string) bool) []string {
	t.Helper()
	files, problems, err := repoFiles(t.Context(), root, keep)
	if err != nil || len(problems) > 0 {
		t.Fatalf("walking %s: %v\n%s", root, err, strings.Join(problems, "\n"))
	}
	return files
}

// runIn runs one trace command against root and returns its exit code and output.
func runIn(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	e := &env{root: root, dir: root, stdout: &stdout, stderr: &stderr}
	code := dispatch(t.Context(), e, args)
	return code, stdout.String(), stderr.String()
}

// testEnv is an env rooted at root whose output the test does not read.
func testEnv(root string) *env {
	return &env{root: root, dir: root, stdout: &strings.Builder{}, stderr: &strings.Builder{}}
}

var (
	wantRe    = regexp.MustCompile(`\bwant\b`)
	problemRe = regexp.MustCompile(`^([^\s:]+):(\d+)`)
)

// wantLines returns "file:line" for every line of root's files (outside testdata/)
// that carries the marker word want, sorted.
func wantLines(t *testing.T, root string) []string {
	t.Helper()
	files := walkFiles(t, root, func(string) bool { return true })
	var out []string
	for _, rel := range files {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			if wantRe.MatchString(sc.Text()) {
				out = append(out, rel+":"+strconv.Itoa(n))
			}
		}
		_ = f.Close()
	}
	sort.Strings(out)
	return out
}

// problemLines returns the "file:line" each problem starts with, sorted.
func problemLines(t *testing.T, problems []string) []string {
	t.Helper()
	out := make([]string, 0, len(problems))
	for _, p := range problems {
		m := problemRe.FindStringSubmatch(p)
		if m == nil {
			t.Fatalf("problem without a file:line position: %q", p)
		}
		out = append(out, m[1]+":"+m[2])
	}
	sort.Strings(out)
	return out
}

// sameLines fails the test unless the two sorted lists are equal.
func sameLines(t *testing.T, what string, got, want []string, problems []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s: reported at\n  %s\nwant\n  %s\nproblems:\n  %s", what,
			strings.Join(got, "\n  "), strings.Join(want, "\n  "), strings.Join(problems, "\n  "))
	}
}

// countContaining is the number of lines in list that contain every one of subs.
func countContaining(list []string, subs ...string) int {
	n := 0
	for _, l := range list {
		all := true
		for _, s := range subs {
			all = all && strings.Contains(l, s)
		}
		if all {
			n++
		}
	}
	return n
}
