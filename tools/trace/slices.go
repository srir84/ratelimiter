package main

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// sliceTargets is one slice, in plan order, with the make targets it adds.
type sliceTargets struct {
	id      string
	targets []string
}

// slicePlan lists every slice in plan order with the make targets each adds (plan
// choice, S0 step 6). test-store-quick hangs off S4, not S1, so the S2 and S3
// slice-checks do not need S1's targets.
func slicePlan() []sliceTargets {
	return []sliceTargets{
		{id: "S0", targets: []string{"test-unit"}},
		{id: "S1"}, {id: "S2"}, {id: "S3"},
		{id: "S4", targets: []string{"test-store-quick", "test-scripts"}},
		{id: "S5", targets: []string{"test-integration", "test-py"}},
		{id: "S6"}, {id: "S7"}, {id: "S8"}, {id: "S9"},
		{id: "H1", targets: []string{"test-e2e"}},
		{id: "H2"}, {id: "H3"}, {id: "H4"},
		{id: "H5", targets: []string{"test-report"}},
		{id: "H6"}, {id: "D1"},
	}
}

func knownSlice(id string) bool {
	for _, s := range slicePlan() {
		if s.id == id {
			return true
		}
	}
	return false
}

// targetsUpTo returns the make targets of every slice up to id, in plan order.
func targetsUpTo(id string) ([]string, error) {
	var out []string
	for _, s := range slicePlan() {
		out = append(out, s.targets...)
		if s.id == id {
			return out, nil
		}
	}
	return nil, fmt.Errorf("unknown slice %q (S0…S9, H1…H6, D1)", id)
}

// runTargets is `trace targets --upto <id>`: the targets on one line, space-separated.
func runTargets(e *env, args []string) int {
	fs := flag.NewFlagSet("targets", flag.ContinueOnError)
	upto := fs.String("upto", "", "last slice: S0…S9, H1…H6 or D1")
	if err := parseFlags(fs, e, args); err != nil {
		return 2
	}
	ts, err := targetsUpTo(*upto)
	if err != nil {
		fmt.Fprintf(e.stderr, "targets: %v\n", err)
		return 2
	}
	fmt.Fprintln(e.stdout, strings.Join(ts, " "))
	return 0
}

// planRow is one row of the plan's "| Test | Slice | Where it lives |" table.
type planRow struct {
	id, slice, where string
	line             int
}

// planSection is one slice's section of the plan.
type planSection struct {
	id      string
	deps    []string
	hasDeps bool
	tests   []planToken // backticked tokens of its Tests section
}

type planToken struct {
	text string
	line int
}

type planDoc struct {
	name     string
	order    []string // slice sections in document order
	sections map[string]*planSection
	rows     []planRow
	table    bool
}

var (
	sliceHeadRe = regexp.MustCompile(`^###\s+([SHD]\d+)\b`)
	sliceRefRe  = regexp.MustCompile(`\b[SHD]\d+\b`)
	backtickRe  = regexp.MustCompile("`([^`]+)`")
)

// parsePlan reads the slice sections (Depends on, and the backticked tokens of the
// Tests section: from a line starting "- **Tests:**" to the next line starting
// "- **Verify:**") and the first table whose header row is "| Test | Slice | Where it
// lives |" (found by its header, not by section number).
func parsePlan(ctx context.Context, root, name string) (*planDoc, error) {
	lines, err := readLines(ctx, root, name)
	if err != nil {
		return nil, err
	}
	p := &planDoc{name: name, sections: map[string]*planSection{}}
	var cur *planSection
	inTests, inFence := false, false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "#") {
			cur, inTests = nil, false
			if m := sliceHeadRe.FindStringSubmatch(line); m != nil && knownSlice(m[1]) {
				if _, dup := p.sections[m[1]]; dup {
					return nil, fmt.Errorf("%s:%d: a second section for slice %s", name, i+1, m[1])
				}
				cur = &planSection{id: m[1]}
				p.sections[m[1]] = cur
				p.order = append(p.order, m[1])
			}
			continue
		}
		if !p.table && strings.TrimSpace(line) == "| Test | Slice | Where it lives |" {
			p.table = true
			j := i + 1
			if j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "|-") {
				j++
			}
			for ; j < len(lines) && strings.HasPrefix(lines[j], "|"); j++ {
				cells := strings.Split(strings.Trim(strings.TrimSpace(lines[j]), "|"), "|")
				if len(cells) != 3 {
					return nil, fmt.Errorf("%s:%d: a Test → slice row needs 3 cells: %q", name, j+1, lines[j])
				}
				p.rows = append(p.rows, planRow{id: strings.TrimSpace(cells[0]), slice: strings.TrimSpace(cells[1]),
					where: strings.TrimSpace(cells[2]), line: j + 1})
			}
			i = j - 1
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "- **Depends on:**"):
			cur.hasDeps = true
			for _, d := range sliceRefRe.FindAllString(line, -1) {
				if knownSlice(d) && d != cur.id {
					cur.deps = append(cur.deps, d)
				}
			}
		case strings.HasPrefix(line, "- **Tests:**"):
			inTests = true
		case strings.HasPrefix(line, "- **Verify:**"):
			inTests = false
		}
		if inTests {
			for _, m := range backtickRe.FindAllStringSubmatch(line, -1) {
				cur.tests = append(cur.tests, planToken{text: m[1], line: i + 1})
			}
		}
	}
	if !p.table {
		return nil, fmt.Errorf("%s has no table with the header row | Test | Slice | Where it lives |", name)
	}
	return p, nil
}

// path returns the slices on id's dependency path: id and everything it depends on,
// directly or not (S2 and S3 do not depend on S1).
func (p *planDoc) path(id string) (map[string]bool, error) {
	out := map[string]bool{}
	var visit func(string) error
	visit = func(s string) error {
		if out[s] {
			return nil
		}
		sec, ok := p.sections[s]
		if !ok {
			return fmt.Errorf("%s has no section for slice %s", p.name, s)
		}
		if !sec.hasDeps {
			return fmt.Errorf("%s: slice %s has no \"- **Depends on:**\" line", p.name, s)
		}
		out[s] = true
		for _, d := range sec.deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		return nil
	}
	return out, visit(id)
}

// checkSlices is `trace slices --upto <id>` (§C7): every defined ID appears exactly
// once in the plan's table with a known slice; every ID of a slice on <id>'s dependency
// path has its function where the table says and a pass result; and every test named in
// those slices' Tests sections exists and, when its package (Python: its file) has
// results, passed.
func checkSlices(ctx context.Context, e *env, args []string) (report, error) {
	fs := flag.NewFlagSet("slices", flag.ContinueOnError)
	upto := fs.String("upto", "", "last slice: S0…S9, H1…H6 or D1")
	planName := fs.String("plan", "IMPLEMENTATION_PLAN.md", "plan file, relative to the repository root")
	resultsDir := fs.String("results", ".out", "results directory (go test -json *.json, JUnit *.xml), relative to the root")
	if err := parseFlags(fs, e, args); err != nil {
		return report{}, err
	}
	if !knownSlice(*upto) {
		fmt.Fprintf(e.stderr, "slices: --upto %q is not a slice (S0…S9, H1…H6, D1)\n", *upto)
		return report{}, errUsage
	}
	module, err := modulePath(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	plan, err := parsePlan(ctx, e.root, *planName)
	if err != nil {
		return report{}, err
	}
	onPath, err := plan.path(*upto)
	if err != nil {
		return report{}, err
	}
	defs, err := definedIDs(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	res, err := loadResults(ctx, rootPath(e.root, *resultsDir))
	if err != nil {
		return report{}, err
	}
	targets, err := targetsUpTo(*upto)
	if err != nil {
		return report{}, err
	}
	var r report
	r.problems = append(r.problems, tableProblems(plan, defs)...)

	funcs, parseProblems, err := scanTestFuncs(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	r.problems = append(r.problems, parseProblems...)
	ids := idResults(plan, onPath, funcs, res, module)
	r.problems = append(r.problems, ids.problems...)

	nc, err := newNamedChecker(ctx, e.root, module, res, targets)
	if err != nil {
		return report{}, err
	}
	for _, s := range plan.order {
		if onPath[s] {
			nc.slice(ctx, plan.sections[s])
		}
	}
	if err := ctx.Err(); err != nil {
		return report{}, err
	}
	r.problems = appendNew(r.problems, nc.problems...) // a file both scans read is reported once

	units := nc.passed + nc.existence + nc.failed
	r.out = []string{
		fmt.Sprintf("slices: %d of %d IDs up to %s passed, %d failed, %d skipped", ids.passed, ids.total, *upto, ids.failed, ids.skipped),
		fmt.Sprintf("slices: %d named tests up to %s: %d passed, %d existence only", units, *upto, nc.passed, nc.existence),
	}
	if nc.failed > 0 {
		r.out[1] += fmt.Sprintf(", %d with problems", nc.failed)
	}
	return r, nil
}

// tableProblems: every defined ID appears exactly once with a known slice, and every
// row names a defined ID.
func tableProblems(plan *planDoc, defs []idDef) []string {
	var problems []string
	defined := map[string]bool{}
	for _, d := range defs {
		defined[d.id] = true
	}
	count := map[string]int{}
	for _, row := range plan.rows {
		count[row.id]++
		if !defined[row.id] {
			problems = append(problems, fmt.Sprintf("%s:%d: %s is not an ID that SPEC.md §10 or HARNESS.md §8 defines", plan.name, row.line, row.id))
		}
		if !knownSlice(row.slice) {
			problems = append(problems, fmt.Sprintf("%s:%d: %s has unknown slice %q", plan.name, row.line, row.id, row.slice))
		}
	}
	for _, d := range defs {
		if count[d.id] != 1 {
			problems = append(problems, fmt.Sprintf("%s: %s appears %d times in the Test → slice table, want exactly once", plan.name, d.id, count[d.id]))
		}
	}
	return problems
}

type idTally struct {
	total, passed, failed, skipped int
	problems                       []string
}

// idResults checks each ID of a slice on the path: its function exists, lives under the
// first path token of "Where it lives", and has a pass result (Go: a top-level test
// event in a .json stream; Python: a JUnit testcase) and no fail or skip result in any
// run, subtest or parametrized case.
func idResults(plan *planDoc, onPath map[string]bool, funcs []testFunc, res *testResults, module string) idTally {
	var t idTally
	byID := map[string][]testFunc{}
	for _, f := range funcs {
		if f.id != "" {
			byID[f.id] = append(byID[f.id], f)
		}
	}
	for _, row := range plan.rows {
		if !onPath[row.slice] {
			continue
		}
		t.total++
		fs := byID[row.id]
		if len(fs) == 0 {
			t.problems = append(t.problems, fmt.Sprintf("%s (%s): missing: no test function %s or %s", row.id, row.slice, idToGo(row.id), idToPy(row.id)))
			continue
		}
		f := fs[0]
		where := ""
		if w := strings.Fields(row.where); len(w) > 0 {
			where = strings.TrimSuffix(w[0], "/")
		}
		if !under(f.file, where) {
			t.problems = append(t.problems, fmt.Sprintf("%s (%s): wrongly located: %s is in %s, but the plan's table says it lives under %s/", row.id, row.slice, f.name, f.file, where))
			continue
		}
		var o testOutcome
		if strings.HasSuffix(f.file, ".py") {
			o, _ = res.pyResult(f.file, f.name)
		} else {
			o = res.goResult(pkgPath(module, f.file), f.name)
		}
		switch { // fail beats skip beats pass: a skip in any run, subtest or case is a skip
		case o.fail:
			t.failed++
			t.problems = append(t.problems, fmt.Sprintf("%s (%s): %s failed", row.id, row.slice, f.name))
		case o.skip:
			t.skipped++
			t.problems = append(t.problems, fmt.Sprintf("%s (%s): %s was skipped", row.id, row.slice, f.name))
		case o.pass:
			t.passed++
		default:
			t.problems = append(t.problems, fmt.Sprintf("%s (%s): %s has no result in the results directory", row.id, row.slice, f.name))
		}
	}
	return t
}

// goTestCmd is one go test command of a slice-check target: the package patterns it
// runs and its -skip pattern ("" for none).
type goTestCmd struct {
	pkgs []string
	skip string
}

// runs reports whether the command runs the in-module package pkg.
func (c goTestCmd) runs(module, pkg string) bool {
	for _, p := range c.pkgs {
		if goPatternMatch(module, p, pkg) {
			return true
		}
	}
	return false
}

// targetGoTests runs make -n <target> in root and returns the go test commands its
// recipe runs.
func targetGoTests(ctx context.Context, root, target string) ([]goTestCmd, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "-n", target)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("make -n %s: %w", target, err)
	}
	return goTestCommands(string(out)), nil
}

// goTestCommands finds the go test runs in a recipe as make -n prints it.
func goTestCommands(recipe string) []goTestCmd {
	var cmds []goTestCmd
	for _, words := range shellCommands(recipe) {
		if args, ok := goTestInvocation(words); ok {
			cmds = append(cmds, parseGoTestArgs(args))
		}
	}
	return cmds
}

// shellCommands splits shell text into commands, each as its words, the way the shell
// reads them without expanding anything: a backslash-newline joins two lines, quotes
// are removed, an unquoted # that starts a word starts a comment, which runs to the end
// of the line, and a newline or an unquoted ;, &, |, ( or ) ends a command.
func shellCommands(s string) [][]string {
	var cmds [][]string
	var words []string
	var w strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, w.String())
			w.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '\\':
			if i++; i < len(s) && s[i] != '\n' {
				w.WriteByte(s[i])
				inWord = true
			}
		case '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				end = len(s) - i - 1
			}
			w.WriteString(s[i+1 : i+1+end])
			inWord = true
			i += end + 1
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\\\"$`\n", s[i+1]) >= 0 {
					if i++; s[i] == '\n' {
						continue
					}
				}
				w.WriteByte(s[i])
			}
			inWord = true
		case ' ', '\t':
			endWord()
		case '#':
			if inWord { // a # inside a word is part of it
				w.WriteByte(ch)
				continue
			}
			for i+1 < len(s) && s[i+1] != '\n' { // the newline still ends the command
				i++
			}
		case '\n', ';', '&', '|', '(', ')':
			endWord()
			if len(words) > 0 {
				cmds = append(cmds, words)
				words = nil
			}
		default:
			w.WriteByte(ch)
			inWord = true
		}
	}
	endWord()
	if len(words) > 0 {
		cmds = append(cmds, words)
	}
	return cmds
}

// goTestInvocation returns the go test arguments of one command: those after "<go> test"
// (any path to go, as $(GO) gives it), or those a trace gotest run hands go test. That
// run is known by its form, whatever $(TRACE) names the program: its first argument is
// "gotest" and a later one is "--" (gotest -o <file> -- <args>); without a "--" it is
// not read, and neither is it when its program never runs its arguments
// (neverRunsArgs). Its go test arguments come after its own flags (-o <file>), which
// end at "--" (dropped) or at the first other argument: in the documented form, the
// arguments after "--". go or the gotest program must be the command word
// (commandWord): a go test or gotest run that an echo prints, or that a comment
// mentions, is not a run.
func goTestInvocation(words []string) ([]string, bool) {
	i := commandWord(words)
	if i+1 >= len(words) {
		return nil, false
	}
	switch {
	case path.Base(words[i]) == "go" && words[i+1] == "test":
		return words[i+2:], true
	case words[i+1] == "gotest" && slices.Contains(words[i+2:], "--") && !neverRunsArgs(words[i]):
		rest := words[i+2:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "--" {
			name, _, hasValue := strings.Cut(strings.TrimLeft(rest[0], "-"), "=")
			rest = rest[1:]
			if name == "o" && !hasValue && len(rest) > 0 {
				rest = rest[1:]
			}
		}
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		return rest, true
	}
	return nil, false
}

// neverRunsArgs reports whether a command word, by any path, names a program that never
// runs its arguments as a command: echo, printf, :, true or false. Their "gotest … --"
// arguments are text, not a gotest run.
func neverRunsArgs(word string) bool {
	switch path.Base(word) {
	case "echo", "printf", ":", "true", "false":
		return true
	}
	return false
}

// commandWord returns the index of a simple command's command word (len(words) when it
// has none): past the reserved words after which the shell reads a command (if, then,
// else, elif, while, until, do, { and !; shellCommands already splits at ( and at the
// separators), then past the VAR=value assignments before it, and past env, time and
// nice prefixes with their options (a value option's value too: env -u NAME, -C DIR,
// -P PATH and -S STRING, time -o FILE and -f FORMAT, nice -n N) and env's assignments.
func commandWord(words []string) int {
	i := 0
	for i < len(words) && precedesCommand(words[i]) {
		i++
	}
	for i < len(words) && shellAssignment(words[i]) {
		i++
	}
	for i < len(words) {
		var valueOpts string
		switch path.Base(words[i]) {
		case "env":
			valueOpts = "uCPS"
		case "time":
			valueOpts = "of"
		case "nice":
			valueOpts = "n"
		default:
			return i
		}
		for i++; i < len(words); i++ {
			w := words[i]
			if w == "--" {
				i++
				break
			}
			if !strings.HasPrefix(w, "-") && !shellAssignment(w) {
				break
			}
			if len(w) == 2 && w[0] == '-' && strings.IndexByte(valueOpts, w[1]) >= 0 {
				i++ // the option's value
			}
		}
	}
	return i
}

// precedesCommand reports whether word, first in a command, is a reserved word after
// which the shell reads a command: an if, elif, while or until condition, a then, else
// or do body, a { group or a ! pipeline. for, case, select and in are followed by a
// name or words instead.
func precedesCommand(word string) bool {
	switch word {
	case "if", "then", "else", "elif", "while", "until", "do", "{", "!":
		return true
	}
	return false
}

// shellAssignment reports whether word is a VAR=value assignment: a name (a letter or _,
// then letters, digits or _) and "=".
func shellAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for k, c := range name {
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (k == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// parseGoTestArgs reads go test's arguments as the go command does
// (cmd/go/internal/test/testflag.go): the package patterns are the first run of
// arguments that are not flags, and none is read after the list ends; "--" or "-args"
// hands the rest to the test binary (binarySkip); a flag that takes a value takes the
// next argument unless written -flag=value; a repeated -skip keeps its last value. With
// no pattern, go test runs the package in its directory, ".".
func parseGoTestArgs(args []string) goTestCmd {
	var c goTestCmd
	inPkgs, sawPkgs := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == "-args" || a == "--args" {
			c.skip = binarySkip(args[i+1:], c.skip)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if sawPkgs && !inPkgs {
				break // a test binary argument, and so is everything after it
			}
			inPkgs, sawPkgs = true, true
			c.pkgs = append(c.pkgs, a)
			continue
		}
		inPkgs = false
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		name = strings.TrimPrefix(name, "test.")
		if !hasValue && goTestValueFlag(name) && i+1 < len(args) {
			i++
			value, hasValue = args[i], true
		}
		if name == "skip" && hasValue {
			c.skip = value
		}
	}
	if len(c.pkgs) == 0 {
		c.pkgs = []string{"."}
	}
	return c
}

// binarySkip reads the arguments go test hands the test binary after -args as the
// binary's flag package does, up to "--" or the first argument that is not a flag: a
// -test.skip there comes after the go command's own -skip, so it wins.
func binarySkip(args []string, skip string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || a == "-" || !strings.HasPrefix(a, "-") {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		name, ok := strings.CutPrefix(name, "test.")
		if !ok {
			continue // not a test binary flag: the binary fails, and the stream shows it
		}
		if !hasValue && goTestValueFlag(name) && i+1 < len(args) {
			i++
			value, hasValue = args[i], true
		}
		if name == "skip" && hasValue {
			skip = value
		}
	}
	return skip
}

// goTestValueFlag reports whether a go test flag (named without dashes or "test.") takes
// a value (go help build, go help testflag); the others are boolean.
func goTestValueFlag(name string) bool {
	switch name {
	case "C", "p", "asmflags", "buildmode", "compiler", "gccgoflags", "gcflags", "installsuffix", "ldflags",
		"mod", "modfile", "overlay", "pgo", "pkgdir", "tags", "toolexec", "covermode", "coverpkg", "exec", "o",
		"bench", "benchtime", "count", "cpu", "fuzz", "fuzzminimizetime", "fuzztime", "list", "parallel", "run",
		"shuffle", "skip", "timeout", "vet", "blockprofile", "blockprofilerate", "coverprofile", "cpuprofile",
		"memprofile", "memprofilerate", "mutexprofile", "mutexprofilefraction", "outputdir", "trace":
		return true
	}
	return false
}

// goPatternMatch reports whether a go test package pattern, as a recipe run at the root
// gives it, names the in-module package pkg (go help packages): "./..." names every
// package, "./x/..." x and the packages below it, "./x" (or "ratelimiter/x") just x, "."
// the root package and "all" every package of the module; "..." matches any string. A
// relative pattern that leaves the root names none.
func goPatternMatch(module, pattern, pkg string) bool {
	if pattern == "all" {
		return true
	}
	if pattern == "." || pattern == ".." || strings.HasPrefix(pattern, "./") || strings.HasPrefix(pattern, "../") {
		switch rel := path.Clean(pattern); {
		case rel == ".":
			pattern = module
		case rel == ".." || strings.HasPrefix(rel, "../"):
			return false
		default:
			pattern = module + "/" + rel
		}
	}
	re := regexp.QuoteMeta(pattern)
	if strings.HasSuffix(re, `/\.\.\.`) {
		re = strings.TrimSuffix(re, `/\.\.\.`) + `(/\.\.\.)?`
	}
	re = strings.ReplaceAll(re, `\.\.\.`, `.*`)
	ok, err := regexp.MatchString(`^`+re+`$`, pkg)
	return err == nil && ok
}

// skipMatches reports whether go test's -skip pattern excludes test (Name or Name/Sub),
// by the testing package's rules (testing/match.go): the pattern splits into top-level
// '|' alternatives, each into one regexp per '/' level (neither splits inside [] or ()
// or after a backslash), and each element is rewritten as go test does. The first
// alternative whose levels all match the name's decides: it skips the test when it has
// no more levels than the name. An empty pattern skips nothing, and so does one that
// does not compile (go test then runs nothing at all).
func skipMatches(pattern, test string) bool {
	if pattern == "" {
		return false
	}
	var alts [][]*regexp.Regexp
	for _, alt := range splitSkipPattern(pattern) {
		var levels []*regexp.Regexp
		for _, el := range alt {
			re, err := regexp.Compile(rewriteSkip(el))
			if err != nil {
				return false
			}
			levels = append(levels, re)
		}
		alts = append(alts, levels)
	}
	name := strings.Split(test, "/")
	for _, levels := range alts {
		ok := true
		for i := 0; i < len(name) && i < len(levels) && ok; i++ {
			ok = levels[i].MatchString(name[i])
		}
		if ok {
			return len(name) >= len(levels)
		}
	}
	return false
}

// splitSkipPattern is testing's splitRegexp: the alternatives of a -skip pattern, each
// as its per-level elements.
func splitSkipPattern(s string) [][]string {
	var alts [][]string
	var levels []string
	brackets, parens := 0, 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '[':
			brackets++
		case ']':
			if brackets--; brackets < 0 { // an unmatched ']' is legal
				brackets = 0
			}
		case '(':
			if brackets == 0 {
				parens++
			}
		case ')':
			if brackets == 0 {
				parens--
			}
		case '\\':
			i++
		case '/', '|':
			if brackets == 0 && parens == 0 {
				levels = append(levels, s[:i])
				if s[i] == '|' {
					alts, levels = append(alts, levels), nil
				}
				s, i = s[i+1:], 0
				continue
			}
		}
		i++
	}
	return append(alts, append(levels, s))
}

// rewriteSkip is testing's rewrite, applied to each pattern element: white space becomes
// '_' and a rune that is not printable its quoted form.
func rewriteSkip(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case testingSpace(r):
			b.WriteByte('_')
		case !strconv.IsPrint(r):
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// testingSpace is the testing package's isSpace (not the same as Unicode's Z class).
func testingSpace(r rune) bool {
	if r < 0x2000 {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xA0, 0x1680:
			return true
		}
		return false
	}
	if r <= 0x200a {
		return true
	}
	switch r {
	case 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return false
}
