//go:build rltest

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
)

// buildWait bounds one go build or go version run of these tests.
const buildWait = 5 * time.Minute

// goCommand runs the go tool in the repository root with args, within buildWait, and returns
// its combined output; GOFLAGS is cleared so no inherited -tags changes the build.
func goCommand(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), buildWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %q: %v\n%s", args, err, out)
	}
	return out
}

// listsTag reports whether go version -m output lists tag in the binary's build settings: a
// "build" line whose -tags setting holds tag among its comma-separated tags. No other line
// counts, the first one, which names the binary's path, included.
func listsTag(info []byte, tag string) bool {
	for _, line := range strings.Split(string(info), "\n") {
		kind, setting, ok := strings.Cut(strings.TrimLeft(line, " \t"), "\t")
		if !ok || kind != "build" {
			continue
		}
		key, value, ok := strings.Cut(setting, "=")
		if !ok || key != "-tags" {
			continue
		}
		if uq, err := strconv.Unquote(value); err == nil {
			value = uq // a value with special characters is quoted
		}
		if slices.Contains(strings.Split(value, ","), tag) {
			return true
		}
	}
	return false
}

// TestDesign_NoTestRoutesInBench (DESIGN §B1, §B6; HARNESS HAR-24; IMPLEMENTATION_PLAN H4):
// ./cmd/hitapi built without tags, as bin/bench/hitapi is, holds no "/_test/" string, so no
// /_test route or fixture is compiled in, and its build info (go version -m) lists no rltest
// tag in its build settings, which the orchestrator's benchmark guard checks. The same package
// built with -tags rltest, under a name without "rltest" so only its build settings can show
// the tag, holds the routes and lists the tag, so both checks can see what they look for.
func TestDesign_NoTestRoutesInBench(t *testing.T) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, c := range []struct {
		name   string
		tagged bool
	}{
		{"hitapi-bench", false},
		{"hitapi-tagged", true},
	} {
		bin := filepath.Join(dir, c.name)
		args := []string{"build", "-o", bin}
		if c.tagged {
			args = append(args, "-tags", "rltest")
		}
		goCommand(t, root, append(args, "./cmd/hitapi")...)
		data, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		routes := bytes.Contains(data, []byte("/_test/"))
		info := goCommand(t, root, "version", "-m", bin)
		tag := listsTag(info, "rltest")
		switch {
		case !c.tagged && routes:
			t.Errorf("./cmd/hitapi built without tags holds a /_test/ string: the benchmark binary has test routes")
		case !c.tagged && tag:
			t.Errorf("go version -m of ./cmd/hitapi built without tags lists rltest:\n%s", info)
		case c.tagged && (!routes || !tag):
			t.Errorf("./cmd/hitapi built with -tags rltest: /_test/ string %v, rltest in go version -m's build settings %v; want both, or the checks see nothing:\n%s",
				routes, tag, info)
		}
	}
}
