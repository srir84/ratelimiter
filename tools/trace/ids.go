package main

import (
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"strings"
)

var (
	// idRowRe matches a defined ID: the first cell of a test-table row (§C7).
	idRowRe = regexp.MustCompile(`^\| (T-[A-Z]+-\d+)( \[D\])? \|`)
	// goIDRe and pyIDRe are the only shapes a test-ID function may take.
	goIDRe = regexp.MustCompile(`^TestT_([A-Z]+)_(\d+)$`)
	pyIDRe = regexp.MustCompile(`^test_t_([a-z]+)_(\d+)$`)
	// pyDefRe finds Python test-ID functions, whatever their shape.
	pyDefRe = regexp.MustCompile(`^[ \t]*(?:async[ \t]+)?def[ \t]+(test_t_[A-Za-z0-9_]*)[ \t]*\(`)
)

// idDef is one defined test ID and where the docs define it.
type idDef struct {
	id   string
	doc  string
	line int
}

// definedIDs reads the IDs defined under SPEC.md "## 10." and HARNESS.md "## 8.".
func definedIDs(ctx context.Context, root string) ([]idDef, error) {
	spec, err := sectionIDs(ctx, root, "SPEC.md", "## 10.")
	if err != nil {
		return nil, err
	}
	harness, err := sectionIDs(ctx, root, "HARNESS.md", "## 8.")
	if err != nil {
		return nil, err
	}
	return append(spec, harness...), nil
}

// sectionIDs returns the IDs in the first column of the tables in one "## " section
// (it ends at the next "## " heading; "### " subsections stay inside it).
func sectionIDs(ctx context.Context, root, doc, heading string) ([]idDef, error) {
	lines, err := readLines(ctx, root, doc)
	if err != nil {
		return nil, err
	}
	var out []idDef
	in, found := false, false
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			in = strings.HasPrefix(line, heading)
			found = found || in
			continue
		}
		if m := idRowRe.FindStringSubmatch(line); in && m != nil {
			out = append(out, idDef{id: m[1], doc: doc, line: i + 1})
		}
	}
	if !found {
		return nil, fmt.Errorf("%s has no %q section", doc, heading)
	}
	return out, nil
}

// idToGo maps T-HFLT-1 to TestT_HFLT_1.
func idToGo(id string) string { return "Test" + strings.ReplaceAll(id, "-", "_") }

// idToPy maps T-HFLT-1 to test_t_hflt_1.
func idToPy(id string) string { return "test_" + strings.ToLower(strings.ReplaceAll(id, "-", "_")) }

// goToID maps TestT_HFLT_1 to T-HFLT-1; ok is false for any other shape.
func goToID(name string) (string, bool) {
	m := goIDRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return "T-" + m[1] + "-" + m[2], true
}

// pyToID maps test_t_hflt_1 to T-HFLT-1; ok is false for any other shape.
func pyToID(name string) (string, bool) {
	m := pyIDRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return "T-" + strings.ToUpper(m[1]) + "-" + m[2], true
}

// testFunc is a function whose name claims a test ID: Go TestT_… or Python test_t_….
type testFunc struct {
	name string
	file string // slash path relative to the root
	line int
	id   string // "" when the name has neither ID shape
}

// scanTestFuncs finds every Go func TestT_… (any .go file, any build tag) and every
// Python def test_t_… under refmodel/ and report/. Files that do not parse are
// returned as problems.
func scanTestFuncs(ctx context.Context, root string) ([]testFunc, []string, error) {
	var funcs []testFunc
	files, problems, err := goFiles(ctx, root, nil)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		f, err := parseGo(ctx, fset, root, rel, 0)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "TestT_") {
				continue
			}
			id, _ := goToID(fd.Name.Name)
			funcs = append(funcs, testFunc{name: fd.Name.Name, file: rel, line: fset.Position(fd.Pos()).Line, id: id})
		}
	}
	pyFiles, pyWalkProblems, err := repoFiles(ctx, root, func(rel string) bool {
		return strings.HasSuffix(rel, ".py") && (under(rel, "refmodel") || under(rel, "report"))
	})
	if err != nil {
		return nil, nil, err
	}
	problems = appendNew(problems, pyWalkProblems...)
	for _, rel := range pyFiles {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		lines, err := readLines(ctx, root, rel)
		if err != nil {
			return nil, nil, err
		}
		for i, line := range lines {
			if m := pyDefRe.FindStringSubmatch(line); m != nil {
				id, _ := pyToID(m[1])
				funcs = append(funcs, testFunc{name: m[1], file: rel, line: i + 1, id: id})
			}
		}
	}
	return funcs, problems, nil
}

// idProblems: a function with no defined ID, two functions for one ID, or a TestT_ or
// test_t_ name of any other shape; with complete, also an ID with no function.
func idProblems(defs []idDef, funcs []testFunc, complete bool) []string {
	var problems []string
	defined := map[string]bool{}
	for _, d := range defs {
		if defined[d.id] {
			problems = append(problems, fmt.Sprintf("%s:%d: %s is defined twice", d.doc, d.line, d.id))
		}
		defined[d.id] = true
	}
	byID := map[string][]testFunc{}
	for _, f := range funcs {
		switch {
		case f.id == "":
			problems = append(problems, fmt.Sprintf("%s:%d: %s is not a test-ID name (Go TestT_<AREA>_<N>, Python test_t_<area>_<n>)", f.file, f.line, f.name))
		case !defined[f.id]:
			problems = append(problems, fmt.Sprintf("%s:%d: %s maps to %s, which SPEC.md §10 and HARNESS.md §8 do not define", f.file, f.line, f.name, f.id))
		default:
			byID[f.id] = append(byID[f.id], f)
		}
	}
	for _, d := range defs {
		fs := byID[d.id]
		if len(fs) > 1 {
			where := make([]string, len(fs))
			for i, f := range fs {
				where[i] = fmt.Sprintf("%s:%d %s", f.file, f.line, f.name)
			}
			problems = append(problems, fmt.Sprintf("%s has %d test functions: %s", d.id, len(fs), strings.Join(where, ", ")))
		}
		if len(fs) == 0 && complete {
			problems = append(problems, fmt.Sprintf("%s has no test function (%s or %s)", d.id, idToGo(d.id), idToPy(d.id)))
		}
		delete(byID, d.id)
	}
	return problems
}

// checkIDs is `trace ids [--list] [--complete]` (§C7).
func checkIDs(ctx context.Context, e *env, args []string) (report, error) {
	fs := flag.NewFlagSet("ids", flag.ContinueOnError)
	list := fs.Bool("list", false, "print the defined IDs, one per line")
	complete := fs.Bool("complete", false, "also fail on an ID with no test function (run at D1)")
	if err := parseFlags(fs, e, args); err != nil {
		return report{}, err
	}
	defs, err := definedIDs(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	funcs, problems, err := scanTestFuncs(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	r := report{problems: append(problems, idProblems(defs, funcs, *complete)...)}
	if *list {
		for _, d := range defs {
			r.out = append(r.out, d.id)
		}
	} else {
		r.out = []string{fmt.Sprintf("ids: %d IDs defined, %d test-ID functions", len(defs), len(funcs))}
	}
	return r, nil
}
