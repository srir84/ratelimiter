package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// mutant is one mutated file: the file (root-relative, slashes) and its whole new content.
type mutant struct {
	id      string // PB1, or the sweep's s0001, …
	file    string
	line    int    // the line it changes (the sweep's reports)
	desc    string // what it changes
	content []byte
}

// verdict is what a mutant run came to (DESIGN §C9).
type verdict int

const (
	killed    verdict = iota // a named test failed with an assertion
	survived                 // every test it ran passed
	notKilled                // a failure that is no kill: a panic, a timeout, no named test failing
	invalid                  // it does not parse or build
)

func (v verdict) String() string {
	switch v {
	case killed:
		return "killed"
	case survived:
		return "survived"
	case notKilled:
		return "not killed"
	case invalid:
		return "invalid"
	}
	return "verdict(" + strconv.Itoa(int(v)) + ")"
}

// result is one mutant's outcome.
type result struct {
	verdict verdict
	why     string            // for notKilled and invalid: panic, timeout, does not build, …
	by      []string          // the tests whose assertions killed it
	overlay map[string]string // the go -overlay replacements it ran with (original → mutated copy)
	log     string            // the saved output of its run
}

func (r result) whyText() string {
	if r.why == "" {
		return ""
	}
	return " (" + r.why + ")"
}

// mutantTimeout bounds a mutant's test run by its baseline: three times the baseline and 30 s,
// at most cfg.maxTimeout, so a mutant that hangs costs little.
func mutantTimeout(cfg config, baseline time.Duration) time.Duration {
	return min(cfg.maxTimeout, 3*baseline+30*time.Second)
}

// logPath is where a run's output goes: .out/mutate/<id>.<phase>[.<n>].log.
func logPath(cfg config, id, phase string, n int) string {
	name := id + "." + phase
	if n > 0 {
		name += "." + strconv.Itoa(n)
	}
	return filepath.Join(cfg.outDir, name+".log")
}

// resetDir empties dir (creating it when missing). A done ctx stops it before it starts.
func resetDir(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clearing %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return nil
}

// withOverlay writes m's mutated copy and a go -overlay file naming it into a fresh temp
// directory (never inside the tree), runs fn with them, and removes the directory. A done ctx
// stops it before it writes anything.
func withOverlay(ctx context.Context, cfg config, m mutant, fn func(overlayFile string, replace map[string]string) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(cfg.tmpDir, "mutate-")
	if err != nil {
		return fmt.Errorf("creating the overlay directory: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the overlay directory: %w", rerr))
		}
	}()
	orig := filepath.Join(cfg.root, filepath.FromSlash(m.file))
	cp := filepath.Join(dir, filepath.Base(m.file))
	if err := os.WriteFile(cp, m.content, 0o644); err != nil {
		return fmt.Errorf("writing the mutated copy: %w", err)
	}
	replace := map[string]string{orig: cp}
	data, err := json.Marshal(struct{ Replace map[string]string }{replace})
	if err != nil {
		return fmt.Errorf("encoding the overlay: %w", err)
	}
	overlay := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlay, data, 0o644); err != nil {
		return fmt.Errorf("writing the overlay: %w", err)
	}
	return fn(overlay, replace)
}

// runGoMutant runs a mutant of a .go or embedded .lua file against named tests (curated): each
// run first passes on the unmutated code, its baseline answering for the names it selects, and
// every name must report a pass in some run; then the mutant runs through the overlay. A .lua
// mutant that does not parse is invalid before any run.
func runGoMutant(ctx context.Context, cfg config, m mutant, runs []goRun, names []string) (result, error) {
	var slowest time.Duration
	passed := map[string]bool{} // the top-level tests that passed in some baseline run
	for i, r := range runs {
		r.timeout = cfg.maxTimeout
		res, err := goTest(ctx, cfg, r, logPath(cfg, m.id, "baseline", i))
		if err != nil {
			return result{}, err
		}
		if p := baselineProblem(res, r.names); p != "" {
			return result{}, fmt.Errorf("baseline go test %s: %s (see %s)", r, p, res.log)
		}
		maps.Copy(passed, res.pass)
		slowest = max(slowest, res.elapsed)
	}
	for _, n := range names {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(passed)), func(t string) bool { return matchesName(t, n) }) {
			return result{}, fmt.Errorf("baseline: %s reports no pass in any go test run", n)
		}
	}
	if strings.HasSuffix(m.file, ".lua") {
		why, err := luaSyntaxError(ctx, cfg, m)
		if err != nil {
			return result{}, err
		}
		if why != "" {
			return result{verdict: invalid, why: "does not parse: " + why}, nil
		}
	}
	timeout := mutantTimeout(cfg, slowest)
	var res result
	err := withOverlay(ctx, cfg, m, func(overlay string, replace map[string]string) error {
		for i, r := range runs {
			r.overlay, r.timeout = overlay, timeout
			gr, err := goTest(ctx, cfg, r, logPath(cfg, m.id, "mutant", i))
			if err != nil {
				return err
			}
			res = judgeGo(gr, names)
			res.overlay, res.log = replace, gr.log
			if res.verdict != survived {
				break // the first run that is not all passes decides
			}
		}
		return nil
	})
	return res, err
}
