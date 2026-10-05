package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/build/constraint"
	"path"
	"sort"
	"strings"
)

// rltestDirs are the trees whose every file is rltest-only (AGENTS.md §2).
func rltestDirs() []string {
	return []string{"limitertest", "harness/fixtures", "harness/driver", "harness/spectest", "harness/pgtest", "harness/e2e"}
}

// needsRltest reports whether a file must carry a build line that requires rltest:
// every *_rltest*.go file but a *_stub.go one (AGENTS.md §2), which covers *_rltest.go
// and *_rltest_test.go, and every .go file under the rltest-only trees.
func needsRltest(rel string) bool {
	base := path.Base(rel)
	if strings.Contains(base, "_rltest") && !strings.HasSuffix(base, "_stub.go") {
		return true
	}
	for _, d := range rltestDirs() {
		if rel == d || under(rel, d) {
			return true
		}
	}
	return false
}

// checkTags is `trace tags` (AGENTS.md §2): a file name alone never tags a file.
func checkTags(ctx context.Context, e *env, args []string) (report, error) {
	if err := parseFlags(flag.NewFlagSet("tags", flag.ContinueOnError), e, args); err != nil {
		return report{}, err
	}
	files, walkProblems, err := goFiles(ctx, e.root, nil)
	if err != nil {
		return report{}, err
	}
	r := report{problems: walkProblems}
	checked := 0
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return report{}, err
		}
		stub := strings.HasSuffix(path.Base(rel), "_stub.go")
		need := needsRltest(rel)
		if !stub && !need {
			continue
		}
		checked++
		x, err := buildLine(ctx, e.root, rel)
		if err != nil {
			r.problems = append(r.problems, err.Error())
			continue
		}
		if need && (x == nil || satisfiable(x, false)) {
			r.problems = append(r.problems, fmt.Sprintf("%s: needs a //go:build line that requires rltest (AGENTS.md §2)", rel))
		}
		if stub && (x == nil || satisfiable(x, true)) {
			r.problems = append(r.problems, fmt.Sprintf("%s: a *_stub.go file needs a //go:build line that excludes rltest (!rltest) (AGENTS.md §2)", rel))
		}
	}
	if len(r.problems) == 0 {
		r.out = []string{fmt.Sprintf("tags: %d rltest-only or stub file(s), all with their build line", checked)}
	}
	return r, nil
}

// buildLine returns the file's //go:build expression (nil when it has none).
func buildLine(ctx context.Context, root, rel string) (constraint.Expr, error) {
	data, err := readFile(ctx, root, rel)
	if err != nil {
		return nil, err
	}
	line, err := goBuildLine(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	if line == "" {
		return nil, nil
	}
	x, err := constraint.Parse(line)
	if err != nil {
		return nil, fmt.Errorf("%s: build line: %w", rel, err)
	}
	return x, nil
}

// goBuildLine finds the //go:build line of a Go file the way go/build does (its
// parseFileHeader): only in the header, the leading blank lines and // or /* */
// comments, which ends at the first other text (the package clause, whatever white
// space follows "package"); lines inside a /* */ comment never count. It returns ""
// when there is none.
func goBuildLine(content string) (string, error) {
	found, inSlashStar := "", false
	for rest := content; rest != ""; {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		line = strings.TrimSpace(line)
		if !inSlashStar && constraint.IsGoBuild(line) {
			if found != "" {
				return "", errors.New("more than one //go:build line")
			}
			found = line
		}
		for line != "" {
			if inSlashStar {
				i := strings.Index(line, "*/")
				if i < 0 {
					break // the comment goes on past this line
				}
				inSlashStar = false
				line = strings.TrimSpace(line[i+len("*/"):])
				continue
			}
			if strings.HasPrefix(line, "//") {
				break
			}
			if strings.HasPrefix(line, "/*") {
				inSlashStar = true
				line = strings.TrimSpace(line[len("/*"):])
				continue
			}
			return found, nil // non-comment text: the header ends
		}
	}
	return found, nil
}

// satisfiable reports whether some assignment of x's other tags, with rltest fixed to
// the given value, satisfies x. "Requires rltest" is !satisfiable(x, false), and
// "excludes rltest" is !satisfiable(x, true).
func satisfiable(x constraint.Expr, rltest bool) bool {
	set := map[string]bool{}
	collectTags(x, set)
	delete(set, "rltest")
	tags := make([]string, 0, len(set))
	for t := range set {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	if len(tags) > 16 {
		return true // too many tags to try every assignment: count it as not proven
	}
	for mask := 0; mask < 1<<len(tags); mask++ {
		val := map[string]bool{"rltest": rltest}
		for i, t := range tags {
			val[t] = mask&(1<<i) != 0
		}
		if x.Eval(func(tag string) bool { return val[tag] }) {
			return true
		}
	}
	return false
}

// collectTags adds every tag in x to set.
func collectTags(x constraint.Expr, set map[string]bool) {
	switch x := x.(type) {
	case *constraint.TagExpr:
		set[x.Tag] = true
	case *constraint.NotExpr:
		collectTags(x.X, set)
	case *constraint.AndExpr:
		collectTags(x.X, set)
		collectTags(x.Y, set)
	case *constraint.OrExpr:
		collectTags(x.X, set)
		collectTags(x.Y, set)
	}
}
