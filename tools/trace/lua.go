package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	preludeRel = scriptsDir + "/prelude.lua"
	// chunkedMark is the only annotation that allows unpack on a line (§C3, §A4.5).
	chunkedMark = "-- lint:chunked(500)"
)

var (
	tostringRe = regexp.MustCompile(`(?i)tostring\s*\(`)
	zrangeRe   = regexp.MustCompile(`(?i)zrangebyscore`)
	// luacheckRe parses one line of luacheck's plain formatter with --codes.
	luacheckRe = regexp.MustCompile(`^(.+?):(\d+):(\d+): \(([EW]\d+)\) (.*)$`)
)

// scriptHeader is the §A4.1 header (line 1) of each script; ok is false for other names.
func scriptHeader(name string) (string, bool) {
	switch name {
	case "acquire", "refund":
		return "#!lua", true
	case "due_entries", "cleanup_entry", "reclaim", "hold", "admin_start_epoch", "admin_restore", "admin_clear":
		return "#!lua flags=allow-oom", true
	case "read_epoch", "admin_status", "admin_inspect":
		return "#!lua flags=no-writes", true
	}
	return "", false
}

// checkLua is `trace lua` (§A4.8 rule 1, §C3, AGENTS.md §3): the text rules, then luacheck.
func checkLua(ctx context.Context, e *env, args []string) (report, error) {
	if err := parseFlags(flag.NewFlagSet("lua", flag.ContinueOnError), e, args); err != nil {
		return report{}, err
	}
	files, walkProblems, err := repoFiles(ctx, e.root, withSuffix(".lua"))
	if err != nil {
		return report{}, err
	}
	problems, err := luaTextProblems(ctx, e.root, files)
	if err != nil {
		return report{}, err
	}
	problems = append(walkProblems, problems...)
	findings, err := luacheckPass(ctx, e.root, files)
	if err != nil {
		return report{problems: problems}, err
	}
	r := report{problems: append(problems, findings...)}
	if len(r.problems) == 0 {
		r.out = []string{fmt.Sprintf("lua: %d .lua file(s) checked", len(files))}
	}
	return r, nil
}

// luaTextProblems: a §A4.1 script's line 1 equals its header exactly (prelude.lua has
// none); no .lua file contains tostring( or ZRANGEBYSCORE (any case); and the name
// unpack appears only on a line ending with the chunked annotation (unpackProblems).
func luaTextProblems(ctx context.Context, root string, files []string) ([]string, error) {
	var problems []string
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lines, err := readLines(ctx, root, rel)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(path.Base(rel), ".lua")
		if path.Dir(rel) == scriptsDir {
			if want, ok := scriptHeader(name); ok && lines[0] != want {
				problems = append(problems, fmt.Sprintf("%s:1: header %q, want %q (DESIGN §A4.1, §A4.8 rule 1)", rel, lines[0], want))
			}
			if name == "prelude" && strings.HasPrefix(lines[0], "#!") {
				problems = append(problems, fmt.Sprintf("%s:1: prelude.lua has no header: the loader sends the script's header line before it", rel))
			}
		}
		for i, line := range lines {
			if tostringRe.MatchString(line) {
				problems = append(problems, fmt.Sprintf("%s:%d: tostring( is banned: use string.format('%%d', n) (DESIGN §A4.8 rule 4)", rel, i+1))
			}
			if zrangeRe.MatchString(line) {
				problems = append(problems, fmt.Sprintf("%s:%d: ZRANGEBYSCORE is banned: use ZRANGE … BYSCORE (DESIGN §A4.8 rule 4)", rel, i+1))
			}
		}
		problems = append(problems, unpackProblems(rel, lines)...)
	}
	return problems, nil
}

// unpackProblems: every unpack name token, called or used as a value (local u = unpack,
// pcall(unpack, t), table.unpack), sits on a line ending with the chunked annotation
// (§C3, §A4.8 rule 3), so an alias cannot carry unpack to an unmarked line. Comments and
// strings do not count. A file that does not lex fails at the error's line, since trace
// cannot read its tokens from there on.
func unpackProblems(rel string, lines []string) []string {
	toks, err := luaLex(strings.Join(lines, "\n"))
	if err != nil {
		line := 1
		var lexErr *luaLexError
		if errors.As(err, &lexErr) {
			line, err = lexErr.line, lexErr.err
		}
		return []string{fmt.Sprintf("%s:%d: %v: trace cannot read the tokens from here on, so cannot rule out an unpack in them; fix the syntax", rel, line, err)}
	}
	var problems []string
	last := 0
	for _, t := range toks {
		if t.kind != 'n' || t.text != "unpack" || t.line == last || t.line > len(lines) {
			continue
		}
		last = t.line
		if !strings.HasSuffix(strings.TrimRight(lines[t.line-1], " \t\r"), chunkedMark) {
			problems = append(problems, fmt.Sprintf("%s:%d: unpack, called or as a value, only on a chunked write marked %q (DESIGN §C3)", rel, t.line, chunkedMark))
		}
	}
	return problems
}

// lintTarget is one file given to luacheck and how its lines map back to sources.
type lintTarget struct {
	path    string // path given to luacheck, relative to the root
	source  string // the script it stands for
	prelude int    // P, the lines prelude.lua adds inside it; -1 when linted as it is
}

// sourceLine maps a luacheck line back to the file and line it came from. An assembled
// script is its line 1, then prelude.lua (lines 2…P+1), then the rest of the script
// (line n is script line n − P).
func (t lintTarget) sourceLine(line int) (string, int) {
	switch {
	case t.prelude < 0 || line == 1:
		return t.source, line
	case line <= t.prelude+1:
		return preludeRel, line - 1
	default:
		return t.source, line - t.prelude
	}
}

// luacheckPass runs luacheck on each script as the store receives it. When prelude.lua
// exists, each existing §A4.1 script is assembled as the loader will (line 1, "\n",
// prelude.lua, the rest of the script) into .out/lua/<name>.lua; every other .lua file
// except prelude.lua is checked as it is. W211, W231 and W311 on a top-level declaration
// line of prelude.lua are dropped (each script uses part of the prelude); every other
// prelude finding counts, once.
func luacheckPass(ctx context.Context, root string, files []string) ([]string, error) {
	hasPrelude, err := fileExists(ctx, root, preludeRel)
	if err != nil {
		return nil, err
	}
	var prelude string
	var preludeLines []string
	if hasPrelude {
		data, err := readFile(ctx, root, preludeRel)
		if err != nil {
			return nil, err
		}
		prelude = string(data)
		preludeLines = strings.Split(prelude, "\n")
	}
	assembled := map[string]bool{}
	var targets []lintTarget
	if hasPrelude {
		// P counts prelude.lua's newlines: without a final one, its last line shares an
		// assembled line with script line 2, and later lines still map to line n − P.
		p := strings.Count(prelude, "\n")
		for _, name := range scriptNames() {
			rel := scriptsDir + "/" + name + ".lua"
			ok, err := fileExists(ctx, root, rel)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			src, err := readFile(ctx, root, rel)
			if err != nil {
				return nil, err
			}
			header, rest, _ := strings.Cut(string(src), "\n")
			out := ".out/lua/" + name + ".lua"
			if err := writeFile(ctx, root, out, header+"\n"+prelude+rest); err != nil {
				return nil, err
			}
			targets = append(targets, lintTarget{path: out, source: rel, prelude: p})
			assembled[rel] = true
		}
	}
	for _, rel := range files {
		if rel != preludeRel && !assembled[rel] {
			targets = append(targets, lintTarget{path: rel, source: rel, prelude: -1})
		}
	}
	// luacheck must be there even when no file needs it yet (S0 step 6).
	bin, err := exec.LookPath("luacheck")
	if err != nil {
		return nil, errors.New("luacheck is not on PATH: run make setup")
	}
	if len(targets) == 0 {
		return nil, nil
	}
	args := []string{"--config", ".luacheckrc", "--codes", "--no-color", "--formatter", "plain"}
	byPath := map[string]lintTarget{}
	for _, t := range targets {
		args = append(args, t.path)
		byPath[t.path] = t
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = root
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() > 2) {
		return nil, fmt.Errorf("luacheck: %w: %s%s", err, out, stderr.String())
	}
	var findings []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		m := luacheckRe.FindStringSubmatch(line)
		if m == nil {
			if strings.TrimSpace(line) != "" {
				findings = append(findings, "luacheck: "+line)
			}
			continue
		}
		t, ok := byPath[m[1]]
		if !ok {
			t = lintTarget{path: m[1], source: m[1], prelude: -1}
		}
		n, _ := strconv.Atoi(m[2])
		file, srcLine := t.sourceLine(n)
		if file == preludeRel {
			if (m[4] == "W211" || m[4] == "W231" || m[4] == "W311") && srcLine-1 < len(preludeLines) &&
				strings.HasPrefix(preludeLines[srcLine-1], "local") {
				continue
			}
		}
		f := fmt.Sprintf("%s:%d:%s: (%s) %s", file, srcLine, m[3], m[4], m[5])
		if !seen[f] {
			seen[f] = true
			findings = append(findings, f)
		}
	}
	return findings, nil
}
