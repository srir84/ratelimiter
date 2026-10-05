package main

import (
	"context"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// isProductDir reports whether a package directory is product code, which never
// imports the harness (DESIGN §A1).
func isProductDir(dir string) bool {
	for _, p := range []string{"limiter", "limitertest", "internal", "cmd/rladmin"} {
		if dir == p || strings.HasPrefix(dir, p+"/") {
			return true
		}
	}
	return false
}

// isHarnessImport reports whether an import path is ratelimiter/harness/... or ratelimiter/cmd/hit*.
func isHarnessImport(module, imp string) bool {
	rel, ok := strings.CutPrefix(imp, module+"/")
	if !ok {
		return false
	}
	return rel == "harness" || strings.HasPrefix(rel, "harness/") || strings.HasPrefix(rel, "cmd/hit")
}

// importEdge is an in-module import and the first file that makes it.
type importEdge struct {
	to   string
	file string
}

// checkBoundary is `trace boundary` (§A1): it parses the imports of every non-test Go
// file, whatever its build tags, and fails when a product package reaches the harness
// directly or transitively, printing the chain. It also fails when a non-test file
// imports an in-module package that sits in a directory the walk skips (testdata/, a
// "." or "_" directory, or the root's bin/ and runs/): Go builds such an explicit
// import, but trace never reads that package, so it cannot follow the chain through it.
func checkBoundary(ctx context.Context, e *env, args []string) (report, error) {
	if err := parseFlags(flag.NewFlagSet("boundary", flag.ContinueOnError), e, args); err != nil {
		return report{}, err
	}
	module, err := modulePath(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	files, walkProblems, err := goFiles(ctx, e.root, func(rel string) bool { return !strings.HasSuffix(rel, "_test.go") })
	if err != nil {
		return report{}, err
	}
	r := report{problems: walkProblems}
	graph := map[string][]importEdge{}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return report{}, err
		}
		f, err := parseGo(ctx, fset, e.root, rel, parser.ImportsOnly)
		if err != nil {
			r.problems = append(r.problems, err.Error())
			continue
		}
		from := pkgPath(module, rel)
		if _, ok := graph[from]; !ok {
			graph[from] = nil
		}
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil || (imp != module && !strings.HasPrefix(imp, module+"/")) {
				continue
			}
			if key := from + " " + imp; !seen[key] {
				seen[key] = true
				graph[from] = append(graph[from], importEdge{to: imp, file: rel})
			}
		}
	}
	pkgs := make([]string, 0, len(graph))
	for p := range graph {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		sort.Slice(graph[p], func(i, j int) bool { return graph[p][i].to < graph[p][j].to })
	}
	for _, p := range pkgs {
		if isProductDir(moduleDir(module, p)) {
			r.problems = append(r.problems, harnessChains(module, graph, p)...)
			continue
		}
		// Other packages: each import of a package trace cannot read, as a chain of one.
		for _, ed := range graph[p] {
			if inGoSkippedDir(moduleDir(module, ed.to)) {
				r.problems = append(r.problems, unreadChain([]string{p, ed.to}, ed.file))
			}
		}
	}
	r.out = []string{fmt.Sprintf("boundary: %d packages, none of the product packages reaches the harness", len(pkgs))}
	if len(r.problems) > 0 {
		r.out = nil
	}
	return r, nil
}

// moduleDir is the root-relative directory of an in-module import path ("" for the root).
func moduleDir(module, imp string) string {
	return strings.TrimPrefix(strings.TrimPrefix(imp, module), "/")
}

// harnessChains walks the import graph breadth-first from one product package and
// returns one line per harness package it reaches, and one per package it reaches that
// sits in a skipped directory (whose imports trace never reads), with the shortest chain.
func harnessChains(module string, graph map[string][]importEdge, start string) []string {
	type step struct{ from, file string } // how the walk first reached a package
	prev := map[string]step{start: {}}
	queue := []string{start}
	var hits []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ed := range graph[cur] {
			if _, ok := prev[ed.to]; ok {
				continue
			}
			prev[ed.to] = step{from: cur, file: ed.file}
			if isHarnessImport(module, ed.to) || inGoSkippedDir(moduleDir(module, ed.to)) {
				hits = append(hits, ed.to)
				continue
			}
			queue = append(queue, ed.to)
		}
	}
	sort.Strings(hits)
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		chain := []string{h}
		for p := h; p != start; p = prev[p].from {
			chain = append([]string{prev[p].from}, chain...)
		}
		if !isHarnessImport(module, h) {
			out = append(out, unreadChain(chain, prev[h].file))
			continue
		}
		out = append(out, fmt.Sprintf("%s: product code never imports the harness (DESIGN §A1); %s imports %s",
			strings.Join(chain, " -> "), prev[h].file, h))
	}
	return out
}

// unreadChain is the problem line for a chain whose last package sits in a skipped
// directory; file is the one that imports it.
func unreadChain(chain []string, file string) string {
	last := chain[len(chain)-1]
	return fmt.Sprintf("%s: %s imports %s, which sits in a directory the walk skips (testdata/, a \".\" or \"_\" directory, or the root's bin/ or runs/): Go builds the import, but trace never reads that package, so it cannot follow the chain through it to the harness (DESIGN §A1); move the package",
		strings.Join(chain, " -> "), file, last)
}
