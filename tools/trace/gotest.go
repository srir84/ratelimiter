package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// gotestArgs builds the go command line: go test -json -shuffle=on <args>, without
// -shuffle=on when the arguments already set -shuffle (§C1).
func gotestArgs(args []string) []string {
	out := []string{"test", "-json"}
	shuffled := false
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		shuffled = shuffled || name == "shuffle" || name == "test.shuffle"
	}
	if !shuffled {
		out = append(out, "-shuffle=on")
	}
	return append(out, args...)
}

// runGotest is `trace gotest -o <file> -- <args>`: it runs go test -json with the
// arguments in the working directory, saves the stream (each package's
// "-test.shuffle <seed>" line included) to <file>, prints the failed and skipped tests
// and the totals, and exits with go test's code.
func runGotest(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("gotest", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	outFile := fs.String("o", "", "file that receives the go test -json stream")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *outFile == "" {
		fmt.Fprintln(e.stderr, "gotest: -o <file> is required: trace gotest -o <file> -- <go test args>")
		return 2
	}
	p := *outFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.dir, p)
	}
	code, err := gotest(ctx, e, p, gotestArgs(fs.Args()))
	if err != nil {
		fmt.Fprintf(e.stderr, "gotest: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// testTally keeps the outcome of each test seen in a go test -json stream. A test that
// runs more than once (-count=N, -cpu 1,4) keeps its worst result: fail beats skip,
// which beats pass, so a failure in any run is listed and counted.
type testTally struct {
	worst    map[string]string // package + " " + test → pass, skip or fail
	pkgFails []string          // packages whose package-level result is fail
}

// outcomeRank orders the terminal actions: a higher rank wins.
func outcomeRank(action string) int {
	switch action {
	case "fail":
		return 3
	case "skip":
		return 2
	case "pass":
		return 1
	}
	return 0
}

func (t *testTally) add(line []byte) {
	var ev struct {
		Action  string
		Package string
		Test    string
	}
	if json.Unmarshal(line, &ev) != nil || outcomeRank(ev.Action) == 0 {
		return
	}
	if ev.Test != "" {
		k := ev.Package + " " + ev.Test
		if outcomeRank(ev.Action) > outcomeRank(t.worst[k]) {
			t.worst[k] = ev.Action
		}
	} else if ev.Action == "fail" {
		t.pkgFails = append(t.pkgFails, ev.Package)
	}
}

func gotest(ctx context.Context, e *env, outPath string, goArgs []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 2, fmt.Errorf("creating stream file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return 2, fmt.Errorf("creating %s: %w", filepath.Dir(outPath), err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return 2, fmt.Errorf("creating stream file: %w", err)
	}
	cmd := exec.CommandContext(ctx, "go", goArgs...)
	cmd.Dir = e.dir
	cmd.Stderr = e.stderr
	// On interrupt, let go test stop its test binaries instead of killing it outright.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = f.Close()
		return 2, fmt.Errorf("go test output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return 2, fmt.Errorf("starting go test: %w", err)
	}
	tally := &testTally{worst: map[string]string{}}
	rd := bufio.NewReader(stdout)
	var writeErr error
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := f.Write(line); werr != nil && writeErr == nil {
				writeErr = werr
			}
			tally.add(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && writeErr == nil {
				writeErr = err
			}
			break
		}
	}
	waitErr := cmd.Wait()
	if err := f.Close(); err != nil && writeErr == nil {
		writeErr = err
	}
	tally.print(e.stdout, outPath)
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(waitErr, &exitErr):
		if code = exitErr.ExitCode(); code < 0 {
			code = 1 // killed by a signal
		}
	case waitErr != nil:
		return 1, fmt.Errorf("go test: %w", waitErr)
	}
	if writeErr != nil {
		return code, fmt.Errorf("saving the stream to %s: %w", outPath, writeErr)
	}
	return code, nil
}

func (t *testTally) print(w io.Writer, outPath string) {
	keys := make([]string, 0, len(t.worst))
	for k := range t.worst {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	counts := map[string]int{}
	for _, k := range keys {
		a := t.worst[k]
		counts[a]++
		if a != "pass" {
			fmt.Fprintf(w, "gotest: %s %s\n", strings.ToUpper(a), k)
		}
	}
	for _, p := range t.pkgFails {
		fmt.Fprintf(w, "gotest: FAIL %s (package)\n", p)
	}
	fmt.Fprintf(w, "gotest: %d passed, %d failed, %d skipped; stream saved to %s\n", counts["pass"], counts["fail"], counts["skip"], outPath)
}
