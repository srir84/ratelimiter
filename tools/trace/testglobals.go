package main

import (
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"strings"
)

// testHelperDirs are the test-helper package trees: testglobals checks every file in
// them, test or not (AGENTS.md §2, §5).
func testHelperDirs() []string {
	return []string{"internal/testutil", "harness/driver", "harness/fixtures", "harness/spectest", "harness/e2e", "harness/pgtest"}
}

// allowedHook reports whether a package-level variable is one of the two hooks that
// tests may write (plan choices): S1's collector and H2's in-flight cap.
func allowedHook(dir, name string) bool {
	return (dir == "internal/testutil/conformance" && name == "measurements") ||
		(dir == "cmd/hitgen" && name == "maxInFlight")
}

// inHelperDir reports whether the root-relative slash path rel (a file or a directory)
// lies in a test-helper package tree.
func inHelperDir(rel string) bool {
	for _, d := range testHelperDirs() {
		if rel == d || under(rel, d) {
			return true
		}
	}
	return false
}

// checkTestGlobals is `trace testglobals` (§C7, AGENTS.md §5): in every _test.go file
// and every file of the test-helper packages, whatever its build tags, a write to a
// package-level variable outside its declaration fails (scopeWalker.onWrite lists the
// forms), and so does a package-level variable declared there that holds itself a value
// which exists to be mutated, written through its methods (a sync or sync/atomic type, a
// channel, strings.Builder, bytes.Buffer or a math/rand/v2 generator, directly, in a
// value field of a struct or as the elements of an array: pkgNames.mutableVars). Among
// the writes is x.m(…) or the method value x.m for a package-level variable x of the
// file's package or of a test-helper package, when the type x is declared with (or its
// initial value shows), with one pointer removed, is declared in x's package or in a
// test-helper package with a pointer-receiver method m (pkgNames.ptrMethod; the methods
// come from every file of that package). The two hooks are allowed by name in their own
// packages, whatever their type.
func checkTestGlobals(ctx context.Context, e *env, args []string) (report, error) {
	if err := parseFlags(flag.NewFlagSet("testglobals", flag.ContinueOnError), e, args); err != nil {
		return report{}, err
	}
	module, err := modulePath(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	files, walkProblems, err := goFiles(ctx, e.root, nil)
	if err != nil {
		return report{}, err
	}
	checkedDirs := map[string]bool{}
	var checked []string
	for _, rel := range files {
		if inHelperDir(rel) || strings.HasSuffix(rel, "_test.go") {
			checked = append(checked, rel)
			checkedDirs[path.Dir(rel)] = true
		}
	}
	// Parse every file of each directory that holds a checked file: the package-level
	// names of a file's package come from all of its files.
	r := report{problems: walkProblems}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	byPkg := map[string][]*ast.File{} // directory + " " + package name → files
	for _, rel := range files {
		if !checkedDirs[path.Dir(rel)] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return report{}, err
		}
		f, err := parseGo(ctx, fset, e.root, rel, 0)
		if err != nil {
			r.problems = append(r.problems, err.Error())
			continue
		}
		parsed[rel] = f
		key := path.Dir(rel) + " " + f.Name.Name
		byPkg[key] = append(byPkg[key], f)
	}
	scopes := map[string]*pkgNames{}       // directory + " " + package name → its names
	importable := map[string][]*pkgNames{} // directory → its packages other than an external test's
	for key, fs := range byPkg {
		scopes[key] = pkgScope(fs)
		if dir, name, _ := strings.Cut(key, " "); !strings.HasSuffix(name, "_test") {
			importable[dir] = append(importable[dir], scopes[key])
		}
	}
	// helperPkg finds a test-helper package by import path (nil for any other): every file
	// of one is checked, so all of them are parsed.
	helperPkg := func(imp string) []*pkgNames {
		if dir, ok := strings.CutPrefix(imp, module+"/"); ok && inHelperDir(dir) {
			return importable[dir]
		}
		return nil
	}
	for _, rel := range checked {
		if err := ctx.Err(); err != nil {
			return report{}, err
		}
		f, ok := parsed[rel]
		if !ok {
			continue
		}
		dir := path.Dir(rel)
		names := scopes[dir+" "+f.Name.Name]
		names.mutableVars(f, func(n *ast.Ident, what string) {
			if !allowedHook(dir, n.Name) {
				r.problems = append(r.problems, fmt.Sprintf("%s: package-level variable %s holds %s, which exists to be mutated: keep it in a value the test or helper owns (AGENTS.md §5)", fset.Position(n.Pos()), n.Name, what))
			}
		})
		w := &scopeWalker{pkgVars: names.vars, pkgOther: names.other, pkgTypes: names.typeNames()}
		w.useImports(f)
		w.onWrite = func(at ast.Node, name string) {
			if !allowedHook(dir, name) {
				r.problems = append(r.problems, fmt.Sprintf("%s: writes package-level variable %s outside its declaration (AGENTS.md §5)", fset.Position(at.Pos()), name))
			}
		}
		w.ptrMethod = func(imp, v, m string) bool {
			if imp == "" {
				return names.ptrMethod(v, m, helperPkg)
			}
			for _, p := range helperPkg(imp) {
				if p.ptrMethod(v, m, helperPkg) {
					return true
				}
			}
			return false
		}
		w.file(f)
	}
	if len(r.problems) == 0 {
		r.out = []string{fmt.Sprintf("testglobals: %d test or helper file(s), no writes to package-level variables and none that holds a mutable value", len(checked))}
	}
	return r, nil
}
