package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
)

var (
	goNameRe     = regexp.MustCompile(`^(?:Test|Fuzz|Example)[A-Za-z0-9_]*$`)
	braceItemsRe = regexp.MustCompile(`^[A-Za-z0-9_]+(?:,[A-Za-z0-9_]+)*$`)
	pyPathNameRe = regexp.MustCompile(`^([A-Za-z0-9_./-]+\.py)::(test_[A-Za-z0-9_]+)$`)
	pyBareNameRe = regexp.MustCompile(`^test_[A-Za-z0-9_]+$`)
	pyAnyDefRe   = regexp.MustCompile(`^[ \t]*(?:async[ \t]+)?def[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(`)
	// exampleOutputRe is go test's rule for an example that runs: an "Output:" comment.
	exampleOutputRe = regexp.MustCompile(`(?i)^[[:space:]]*(unordered )?output:`)
)

// testName is a test named in a Tests section. Go: Test…, Fuzz… or Example… names,
// optionally ending in _{A,B} (Name_A and Name_B, or subtests Name/A and Name/B) or _*
// (at least one Name_… function). Python: path.py::test_… or a bare test_….
type testName struct {
	goBase string
	items  []string
	star   bool
	pyFile string
	pyName string
}

// parseTestName classifies a whole backticked token. TestT_… and test_t_… names are left
// to the ID check, and a token with spaces or "…" is prose.
func parseTestName(tok string) (testName, bool) {
	if m := pyPathNameRe.FindStringSubmatch(tok); m != nil {
		return testName{pyFile: m[1], pyName: m[2]}, !strings.HasPrefix(m[2], "test_t_")
	}
	if pyBareNameRe.MatchString(tok) {
		return testName{pyName: tok}, !strings.HasPrefix(tok, "test_t_")
	}
	n := testName{goBase: tok}
	if base, ok := strings.CutSuffix(tok, "_*"); ok {
		n.goBase, n.star = base, true
	} else if i := strings.Index(tok, "_{"); i >= 0 && strings.HasSuffix(tok, "}") {
		items := tok[i+2 : len(tok)-1]
		if !braceItemsRe.MatchString(items) {
			return testName{}, false
		}
		n.goBase, n.items = tok[:i], strings.Split(items, ",")
	}
	if !goNameRe.MatchString(n.goBase) || strings.HasPrefix(n.goBase, "TestT_") {
		return testName{}, false
	}
	return n, true
}

// goTestFunc is a top-level func in a _test.go file.
type goTestFunc struct {
	name, file, pkg string
	example, output bool // an Example… func, and whether it has an Output: comment
}

// namedChecker checks the tests named in the Tests sections of the slices on the path.
type namedChecker struct {
	root    string
	module  string
	res     *testResults
	targets []string
	goFuncs map[string][]goTestFunc
	pyFuncs map[string][]string // bare Python name → files under refmodel/ and report/

	cmds      []goTestCmd // the go test commands of the targets, read on first use
	cmdsReady bool
	cmdsErr   error

	seen                      map[string]bool
	passed, existence, failed int
	problems                  []string
}

func newNamedChecker(ctx context.Context, root, module string, res *testResults, targets []string) (*namedChecker, error) {
	c := &namedChecker{root: root, module: module, res: res, targets: targets,
		goFuncs: map[string][]goTestFunc{}, pyFuncs: map[string][]string{}, seen: map[string]bool{}}
	testFiles, walkProblems, err := goFiles(ctx, root, withSuffix("_test.go"))
	if err != nil {
		return nil, err
	}
	c.problems = walkProblems
	fset := token.NewFileSet()
	for _, rel := range testFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := parseGo(ctx, fset, root, rel, parser.ParseComments)
		if err != nil {
			c.problems = append(c.problems, err.Error())
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			g := goTestFunc{name: fd.Name.Name, file: rel, pkg: pkgPath(module, rel), example: strings.HasPrefix(fd.Name.Name, "Example")}
			g.output = g.example && hasOutputComment(f, fd)
			c.goFuncs[g.name] = append(c.goFuncs[g.name], g)
		}
	}
	pyFiles, pyWalkProblems, err := repoFiles(ctx, root, func(rel string) bool {
		return strings.HasSuffix(rel, ".py") && (under(rel, "refmodel") || under(rel, "report"))
	})
	if err != nil {
		return nil, err
	}
	c.problems = appendNew(c.problems, pyWalkProblems...)
	for _, rel := range pyFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		defs, err := pyDefNames(ctx, root, rel)
		if err != nil {
			return nil, err
		}
		for _, n := range defs {
			c.pyFuncs[n] = append(c.pyFuncs[n], rel)
		}
	}
	return c, nil
}

// hasOutputComment reports whether an Example func's body has an Output: comment.
func hasOutputComment(f *ast.File, fd *ast.FuncDecl) bool {
	if fd.Body == nil {
		return false
	}
	for _, g := range f.Comments {
		if g.Pos() > fd.Body.Lbrace && g.End() < fd.Body.Rbrace && exampleOutputRe.MatchString(g.Text()) {
			return true
		}
	}
	return false
}

// pyDefNames returns the names of the functions a Python file defines.
func pyDefNames(ctx context.Context, root, rel string) ([]string, error) {
	lines, err := readLines(ctx, root, rel)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range lines {
		if m := pyAnyDefRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out, nil
}

func (c *namedChecker) problem(sec *planSection, tok planToken, format string, a ...any) {
	c.failed++
	c.problems = append(c.problems, fmt.Sprintf("%s (plan line %d): %s: %s", sec.id, tok.line, tok.text, fmt.Sprintf(format, a...)))
}

// slice checks every test named in one slice's Tests section. A done ctx stops it; the
// caller reports ctx's error.
func (c *namedChecker) slice(ctx context.Context, sec *planSection) {
	for _, tok := range sec.tests {
		if ctx.Err() != nil {
			return
		}
		n, ok := parseTestName(tok.text)
		if !ok || c.seen[tok.text] {
			continue
		}
		c.seen[tok.text] = true
		switch {
		case n.pyName != "":
			c.python(ctx, sec, tok, n)
		case n.star:
			var names []string
			for name := range c.goFuncs {
				if strings.HasPrefix(name, n.goBase+"_") {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			if len(names) == 0 {
				c.problem(sec, tok, "missing: no func %s_… in any _test.go file", n.goBase)
			}
			for _, name := range names {
				c.goUnit(ctx, sec, planToken{text: name, line: tok.line}, name, "")
			}
		case n.items != nil:
			for _, it := range n.items {
				unit := planToken{text: n.goBase + "_" + it, line: tok.line}
				switch {
				case c.goFuncs[unit.text] != nil:
					c.goUnit(ctx, sec, unit, unit.text, "")
				case c.goFuncs[n.goBase] != nil:
					c.goUnit(ctx, sec, unit, n.goBase, it)
				default:
					c.problem(sec, unit, "missing: no func %s, and no func %s for the subtest %s/%s", unit.text, n.goBase, n.goBase, it)
				}
			}
		default:
			if c.goFuncs[n.goBase] == nil {
				c.problem(sec, tok, "missing: no func %s in any _test.go file", n.goBase)
				continue
			}
			c.goUnit(ctx, sec, tok, n.goBase, "")
		}
	}
}

// goUnit checks one Go test (or subtest name/sub) in every package that declares name.
func (c *namedChecker) goUnit(ctx context.Context, sec *planSection, tok planToken, name, sub string) {
	test := name
	if sub != "" {
		test = name + "/" + sub
	}
	passed := false
	for _, g := range c.goFuncs[name] {
		if g.example && !g.output {
			continue // compiled but never run: existence is enough
		}
		o := c.res.goResult(g.pkg, test)
		switch {
		case o.fail:
			c.problem(sec, tok, "%s failed in %s", test, g.pkg)
			return
		case o.skip: // in any run or subtest, even beside a pass
			c.problem(sec, tok, "%s was skipped in %s: tests never call t.Skip (AGENTS.md §2)", test, g.pkg)
			return
		case o.pass:
			passed = true
		case !c.res.goRan[g.pkg]:
			// No target ran the package: existence is enough.
		case c.skipped(ctx, g.pkg, test):
			// The target command that runs the package excludes it with -skip: existence is enough.
		case c.cmdsErr != nil:
			c.problem(sec, tok, "reading the targets' go test commands: %v", c.cmdsErr)
			return
		default:
			c.problem(sec, tok, "%s ran (%s) but %s has no pass result", g.pkg, g.file, test)
			return
		}
	}
	if passed {
		c.passed++
	} else {
		c.existence++
	}
}

// skipped reports whether a go test command of a slice-check target (read from make -n
// <target> on first use) both runs package pkg and excludes test with its -skip: a -skip
// exempts only the packages of its own command.
func (c *namedChecker) skipped(ctx context.Context, pkg, test string) bool {
	if !c.cmdsReady {
		c.cmdsReady = true
		for _, t := range c.targets {
			cmds, err := targetGoTests(ctx, c.root, t)
			if err != nil {
				c.cmdsErr = err
				break
			}
			c.cmds = append(c.cmds, cmds...)
		}
	}
	for _, cmd := range c.cmds {
		if cmd.skip != "" && skipMatches(cmd.skip, test) && cmd.runs(c.module, pkg) {
			return true
		}
	}
	return false
}

// python checks a Python name: a def in that file (a bare test_… name: under refmodel/
// or report/), and a JUnit pass when any JUnit file has results for its file.
func (c *namedChecker) python(ctx context.Context, sec *planSection, tok planToken, n testName) {
	files := []string{n.pyFile}
	if n.pyFile == "" {
		files = c.pyFuncs[n.pyName]
		if len(files) == 0 {
			c.problem(sec, tok, "missing: no def %s under refmodel/ or report/", n.pyName)
			return
		}
	} else {
		exists, err := fileExists(ctx, c.root, n.pyFile)
		if err != nil {
			c.problem(sec, tok, "%v", err)
			return
		}
		if !exists {
			c.problem(sec, tok, "missing: no file %s", n.pyFile)
			return
		}
		defs, err := pyDefNames(ctx, c.root, n.pyFile)
		if err != nil {
			c.problem(sec, tok, "%v", err)
			return
		}
		found := false
		for _, d := range defs {
			found = found || d == n.pyName
		}
		if !found {
			c.problem(sec, tok, "missing: no def %s in %s", n.pyName, n.pyFile)
			return
		}
	}
	passed := false
	for _, f := range files {
		o, ran := c.res.pyResult(f, n.pyName)
		switch {
		case o.fail:
			c.problem(sec, tok, "%s::%s failed", f, n.pyName)
			return
		case o.skip: // in any case, even beside a pass
			c.problem(sec, tok, "%s::%s was skipped: tests never call pytest.skip (AGENTS.md §4)", f, n.pyName)
			return
		case o.pass:
			passed = true
		case ran:
			c.problem(sec, tok, "the JUnit results have %s but no pass for %s", f, n.pyName)
			return
		}
	}
	if passed {
		c.passed++
	} else {
		c.existence++
	}
}
