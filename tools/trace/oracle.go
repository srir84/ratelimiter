package main

import (
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

// checkOracle is `trace oracle` (§C7, AGENTS.md §5): in _test.go files under
// harness/spectest/ and internal/scripts/, a selector on the ratelimiter/internal/core
// import (whatever its local name) naming RetryAfterNs or DecisionNs fails, and so does
// a dot import of it. Field reads (res.RetryAfterNs) and composite-literal keys are not
// selectors on the package, so they pass.
func checkOracle(ctx context.Context, e *env, args []string) (report, error) {
	if err := parseFlags(flag.NewFlagSet("oracle", flag.ContinueOnError), e, args); err != nil {
		return report{}, err
	}
	module, err := modulePath(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	core := module + "/internal/core"
	files, walkProblems, err := goFiles(ctx, e.root, func(rel string) bool {
		return strings.HasSuffix(rel, "_test.go") && (under(rel, "harness/spectest") || under(rel, "internal/scripts"))
	})
	if err != nil {
		return report{}, err
	}
	const why = "expected values come from literals, math/big or the reference model (AGENTS.md §5)"
	r := report{problems: walkProblems}
	fset := token.NewFileSet()
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return report{}, err
		}
		f, err := parseGo(ctx, fset, e.root, rel, 0)
		if err != nil {
			r.problems = append(r.problems, err.Error())
			continue
		}
		names := map[string]bool{}
		for _, imp := range f.Imports {
			if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != core {
				continue
			}
			switch {
			case imp.Name == nil:
				names["core"] = true
			case imp.Name.Name == ".":
				r.problems = append(r.problems, fmt.Sprintf("%s: dot import of %s: %s", fset.Position(imp.Pos()), core, why))
			case imp.Name.Name != "_":
				names[imp.Name.Name] = true
			}
		}
		if len(names) == 0 {
			continue
		}
		w := &scopeWalker{onSelect: func(sel *ast.SelectorExpr, x *ast.Ident) {
			if names[x.Name] && (sel.Sel.Name == "RetryAfterNs" || sel.Sel.Name == "DecisionNs") {
				r.problems = append(r.problems, fmt.Sprintf("%s: %s.%s from %s: %s", fset.Position(sel.Pos()), x.Name, sel.Sel.Name, core, why))
			}
		}}
		w.file(f)
	}
	if len(r.problems) == 0 {
		r.out = []string{fmt.Sprintf("oracle: %d test file(s) under harness/spectest and internal/scripts, none takes expected values from internal/core", len(files))}
	}
	return r, nil
}
