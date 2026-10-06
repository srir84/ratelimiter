package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// sweepTarget is one file of a sweep, with its mutants and the go test commands they run.
type sweepTarget struct {
	file    string
	mutants []mutant
	runs    []goRun // each with its baseline's timeout set
}

// sweepResult is one tried mutant.
type sweepResult struct {
	m   mutant
	res result
}

// sweepCmd is `mutate --sweep <file>… [--budget d]` (DESIGN §C9; plan S4 step 16): the
// operators on each file; each mutant runs the L2 packages (.lua, -tags scripttest) or its own
// package's tests (.go; also with -tags rltest where the package has rltest tests) until the
// budget is spent. Survivors and untried mutants go to the mutants file; they never fail the run.
func sweepCmd(ctx context.Context, cfg config, files []string, budget time.Duration, out, errOut *printer) int {
	if err := resetDir(ctx, cfg.outDir); err != nil {
		errOut.printf("mutate: %v\n", err)
		return 1
	}
	idx := &testIndex{root: cfg.root}
	var targets []sweepTarget
	total := 0
	for _, f := range files {
		t, note, err := sweepFile(ctx, cfg, idx, f)
		if err != nil {
			errOut.printf("mutate: %s: %v\n", f, err)
			return 1
		}
		if note != "" {
			out.printf("mutate: %s: %s\n", f, note)
			continue
		}
		for i := range t.mutants {
			total++
			t.mutants[i].id = fmt.Sprintf("s%04d", total)
		}
		targets = append(targets, t)
	}
	// Baselines: each distinct command once, unmutated; its time bounds its mutants' runs.
	timeouts := map[string]time.Duration{}
	for ti := range targets {
		for ri, r := range targets[ti].runs {
			key := r.String()
			if _, ok := timeouts[key]; !ok {
				r.timeout = cfg.maxTimeout
				res, err := goTest(ctx, cfg, r, logPath(cfg, fmt.Sprintf("baseline%02d", len(timeouts)+1), "sweep", 0))
				if err != nil {
					errOut.printf("mutate: baseline go test %s: %v\n", r, err)
					return 1
				}
				if p := baselineProblem(res, nil); p != "" {
					errOut.printf("mutate: baseline go test %s: %s (see %s)\n", r, p, res.log)
					return 1
				}
				timeouts[key] = mutantTimeout(cfg, res.elapsed)
			}
			targets[ti].runs[ri].timeout = timeouts[key]
		}
	}
	start := time.Now()
	var done []sweepResult
	var untried []mutant
	for _, t := range targets {
		for _, m := range t.mutants {
			if time.Since(start) >= budget || ctx.Err() != nil {
				untried = append(untried, m)
				continue
			}
			res, err := sweepOne(ctx, cfg, m, t.runs)
			if err != nil {
				if ctx.Err() != nil {
					untried = append(untried, m)
					continue
				}
				errOut.printf("mutate: %s %s:%d (%s): %v\n", m.id, m.file, m.line, m.desc, err)
				return 1
			}
			if res.verdict == killed || res.verdict == invalid {
				if err := removeLogs(ctx, cfg, m.id); err != nil && ctx.Err() == nil {
					errOut.printf("mutate: %v\n", err)
					return 1
				}
			}
			done = append(done, sweepResult{m, res})
		}
	}
	summary, err := writeMutants(context.WithoutCancel(ctx), cfg, len(targets), total, budget, done, untried)
	if err != nil {
		errOut.printf("mutate: %v\n", err)
		return 1
	}
	out.printf("mutate: %s; survivors and untried mutants in %s\n", summary, cfg.mutantsFile)
	if err := ctx.Err(); err != nil {
		errOut.printf("mutate: stopped: %v\n", err)
		return 1
	}
	return 0
}

// sweepFile reads one file of a sweep and builds its mutants and commands; note says why a
// file gets no operators.
func sweepFile(ctx context.Context, cfg config, idx *testIndex, f string) (sweepTarget, string, error) {
	rel := filepath.ToSlash(filepath.Clean(f))
	if filepath.IsAbs(f) {
		r, err := filepath.Rel(cfg.root, f)
		if err != nil {
			return sweepTarget{}, "", err
		}
		rel = filepath.ToSlash(r)
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return sweepTarget{}, "", errors.New("outside the module")
	}
	src, err := os.ReadFile(filepath.Join(cfg.root, filepath.FromSlash(rel)))
	if err != nil {
		return sweepTarget{}, "", err
	}
	t := sweepTarget{file: rel}
	switch {
	case strings.HasSuffix(rel, ".lua"):
		preludeFile := path.Join(path.Dir(rel), "prelude.lua")
		prelude := src
		if rel != preludeFile {
			prelude, err = os.ReadFile(filepath.Join(cfg.root, filepath.FromSlash(preludeFile)))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return sweepTarget{}, "", err
			}
		}
		names, err := luaCodeNames(string(prelude))
		if err != nil {
			return sweepTarget{}, "", fmt.Errorf("%s: %w", preludeFile, err)
		}
		if t.mutants, err = luaMutants(rel, string(src), names); err != nil {
			return sweepTarget{}, "", err
		}
		t.runs = []goRun{{pkgs: cfg.luaPackages, tags: "scripttest"}}
	case strings.HasSuffix(rel, "_test.go"):
		return t, "a test file: no operators", nil
	case strings.HasSuffix(rel, ".go"):
		dir := path.Dir(rel)
		inScope := false
		for _, s := range cfg.goScope {
			inScope = inScope || dir == s || strings.HasPrefix(dir, s+"/")
		}
		if !inScope {
			return t, "outside " + strings.Join(cfg.goScope, ", ") + ": no Go operators (DESIGN §C9)", nil
		}
		if t.mutants, err = goMutants(rel, string(src)); err != nil {
			return sweepTarget{}, "", err
		}
		t.runs = []goRun{{pkgs: []string{pkgPattern(dir)}}}
		tagged, err := idx.hasTagged(ctx, dir, "rltest")
		if err != nil {
			return sweepTarget{}, "", err
		}
		if tagged {
			t.runs = append(t.runs, goRun{pkgs: []string{pkgPattern(dir)}, tags: "rltest"})
		}
	default:
		return t, "no §C9 operators for this kind of file", nil
	}
	if len(t.mutants) == 0 {
		return t, "no operator applies", nil
	}
	return t, "", nil
}

// sweepOne runs one mutant: a .lua mutant that does not parse is invalid; otherwise each
// command runs with the overlay, and the first that is not all passes decides.
func sweepOne(ctx context.Context, cfg config, m mutant, runs []goRun) (result, error) {
	if strings.HasSuffix(m.file, ".lua") {
		why, err := luaSyntaxError(ctx, cfg, m)
		if err != nil {
			return result{}, err
		}
		if why != "" {
			return result{verdict: invalid, why: "does not parse: " + why}, nil
		}
	}
	var res result
	err := withOverlay(ctx, cfg, m, func(overlay string, replace map[string]string) error {
		for i, r := range runs {
			r.overlay = overlay
			gr, err := goTest(ctx, cfg, r, logPath(cfg, m.id, "mutant", i))
			if err != nil {
				return err
			}
			res = judgeGo(gr, nil)
			res.overlay, res.log = replace, gr.log
			if res.verdict != survived {
				break
			}
		}
		return nil
	})
	return res, err
}

// removeLogs deletes the saved output of a mutant's runs. A done ctx stops it before it starts.
func removeLogs(ctx context.Context, cfg config, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(cfg.outDir, id+".*.log"))
	if err != nil {
		return err
	}
	for _, p := range matches {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

// writeMutants writes the sweep's report: a summary, then one line per survivor (a mutant
// that is not killed, its reason given) and per untried mutant. It returns the summary. A done
// ctx stops it before it writes (the caller passes one that an interrupt leaves running, so an
// interrupted sweep still lists what it left untried).
func writeMutants(ctx context.Context, cfg config, files, total int, budget time.Duration, done []sweepResult, untried []mutant) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	count := map[verdict]int{}
	for _, d := range done {
		count[d.res.verdict]++
	}
	summary := fmt.Sprintf("%d mutant(s) of %d file(s), budget %s: %d killed, %d survived, %d not killed, %d invalid, %d untried",
		total, files, budget, count[killed], count[survived], count[notKilled], count[invalid], len(untried))
	var b strings.Builder
	b.WriteString("# bin/mutate --sweep (DESIGN §C9): mutants that were not killed, then mutants left untried.\n")
	b.WriteString("# Each survivor needs a killing test or a one-line \"equivalent because …\" in the slice report.\n")
	b.WriteString("# " + summary + "\n")
	for _, d := range done {
		if d.res.verdict == killed || d.res.verdict == invalid {
			continue
		}
		fmt.Fprintf(&b, "%-10s %s:%d  %s%s\n", d.res.verdict, d.m.file, d.m.line, d.m.desc, d.res.whyText())
	}
	for _, m := range untried {
		fmt.Fprintf(&b, "%-10s %s:%d  %s\n", "untried", m.file, m.line, m.desc)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.mutantsFile), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(cfg.mutantsFile, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", cfg.mutantsFile, err)
	}
	return summary, nil
}
