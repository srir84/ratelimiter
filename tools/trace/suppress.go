package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

var (
	// pySuppressRe and luaSuppressRe find inline suppressions in Python and Lua lines.
	// The Python one also takes the isort:, fmt: and yapf: directives (ruff honours all
	// three); the Lua one also matches long-bracket openers (--[[ stylua: ignore ]]).
	pySuppressRe  = regexp.MustCompile(`(?i)#\s*noqa|flake8:\s*noqa|#\s*type:\s*ignore|#\s*(ruff|mypy|pyright|isort|fmt|yapf):`)
	luaSuppressRe = regexp.MustCompile(`(?i)--+(\[=*\[)?\s*(luacheck|stylua):`)
	// luaDirectiveRe finds a directive at the start of a comment line, once white space
	// and dashes are trimmed from its left.
	luaDirectiveRe = regexp.MustCompile(`(?i)^(luacheck|stylua):`)
)

// luaSuppressions returns a Lua source's inline suppressions by line: each line that
// luaSuppressRe matches (string literals included), and each line of every comment,
// short or long (any level, any number of lines), that starts with luacheck: or stylua:
// in any case once Unicode white space and dashes are trimmed from its left. That takes
// in every directive the tools honour: luacheck reads a short comment whose text starts
// with "luacheck:" after ASCII white space, the vertical tab among it (RE2's \s lacks
// it); stylua reads any line of any comment that is exactly "stylua: ignore" (or
// "… start", "… end") once Unicode white space is trimmed, so on an inner line of a long
// comment too. Its error is a *luaLexError, with the hits found so far.
func luaSuppressions(src string) (map[int]string, error) {
	found := map[int]string{}
	for i, line := range strings.Split(src, "\n") {
		if m := luaSuppressRe.FindString(line); m != "" {
			found[i+1] = m
		}
	}
	_, comments, err := luaScan(src)
	if err != nil {
		return found, err
	}
	for _, c := range comments {
		for k, line := range strings.Split(c.text, "\n") {
			t := strings.TrimLeftFunc(line, func(r rune) bool { return r == '-' || unicode.IsSpace(r) })
			if m := luaDirectiveRe.FindString(t); m != "" {
				if _, ok := found[c.line+k]; !ok {
					found[c.line+k] = m
				}
			}
		}
	}
	return found, nil
}

// goSuppression reports whether a Go comment's text is an inline suppression. The text
// is normalised as golangci-lint does before it looks for nolint: every leading '/',
// space and tab is stripped (and for a /* */ comment its markers, leading '*' and line
// breaks), so ///nolint, // //nolint and /*nolint*/ count. It is a suppression when it
// then starts with a linter directive, or carries gosec's marker anywhere, in any case.
func goSuppression(text string) bool {
	t := strings.TrimLeft(text, "/ \t")
	if body, ok := strings.CutPrefix(text, "/*"); ok {
		t = strings.TrimLeft(strings.TrimSuffix(body, "*/"), "/* \t\r\n")
	}
	t = strings.ToLower(t)
	for _, p := range []string{"nolint", "lint:ignore", "lint:file-ignore", "exhaustive:ignore", "gosec:disable"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(text), "#nosec")
}

// generatedHeader returns the comment that marks f as generated code
// (go.dev/s/generatedcode), or nil: golangci-lint's strict generated-file exclusion
// skips such a file wholesale, so the marker suppresses every linter. It reads the
// marker as go/ast.IsGenerated does (a line "// Code generated … DO NOT EDIT." in a
// comment before the package clause) and also returns its position.
func generatedHeader(f *ast.File) *ast.Comment {
	for _, g := range f.Comments {
		for _, c := range g.List {
			if c.Pos() > f.Package {
				return nil
			}
			for _, line := range strings.Split(c.Text, "\n") {
				if rest, ok := strings.CutPrefix(line, "// Code generated "); ok && strings.HasSuffix(rest, " DO NOT EDIT.") {
					return c
				}
			}
		}
	}
	return nil
}

// checkSuppress is `trace suppress` (AGENTS.md §0): no inline lint suppression in a
// .go, .py or .lua file outside testdata/. Go comments come from go/parser, so string
// literals never count. A .lua file that does not lex fails at the error's line, since
// trace cannot read its comments from there on.
func checkSuppress(ctx context.Context, e *env, args []string) (report, error) {
	if err := parseFlags(flag.NewFlagSet("suppress", flag.ContinueOnError), e, args); err != nil {
		return report{}, err
	}
	files, walkProblems, err := repoFiles(ctx, e.root, func(rel string) bool {
		return strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, ".py") || strings.HasSuffix(rel, ".lua")
	})
	if err != nil {
		return report{}, err
	}
	const fix = "fix the code; an exception goes in the lint config, with its reason, by the owner (AGENTS.md §0)"
	r := report{problems: walkProblems}
	fset := token.NewFileSet()
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return report{}, err
		}
		if strings.HasSuffix(rel, ".go") {
			f, err := parseGo(ctx, fset, e.root, rel, parser.ParseComments)
			if err != nil {
				r.problems = append(r.problems, err.Error())
				continue
			}
			if c := generatedHeader(f); c != nil {
				r.problems = append(r.problems, fmt.Sprintf("%s: generated-code marker %q: golangci-lint skips the whole file; %s", fset.Position(c.Pos()), c.Text, fix))
			}
			for _, g := range f.Comments {
				for _, c := range g.List {
					if goSuppression(c.Text) {
						r.problems = append(r.problems, fmt.Sprintf("%s: inline suppression %q: %s", fset.Position(c.Pos()), c.Text, fix))
					}
				}
			}
			continue
		}
		if strings.HasSuffix(rel, ".lua") {
			src, err := readFile(ctx, e.root, rel)
			if err != nil {
				return report{}, err
			}
			found, err := luaSuppressions(string(src))
			var lexErr *luaLexError
			switch {
			case errors.As(err, &lexErr):
				r.problems = append(r.problems, fmt.Sprintf("%s:%d: %v: trace cannot read the comments from here on, so cannot rule out an inline suppression in them; fix the syntax", rel, lexErr.line, lexErr.err))
			case err != nil:
				return report{}, fmt.Errorf("reading the comments of %s: %w", rel, err)
			}
			for _, n := range slices.Sorted(maps.Keys(found)) {
				r.problems = append(r.problems, fmt.Sprintf("%s:%d: inline suppression %q: %s", rel, n, found[n], fix))
			}
			continue
		}
		lines, err := readLines(ctx, e.root, rel)
		if err != nil {
			return report{}, err
		}
		for i, line := range lines {
			if m := pySuppressRe.FindString(line); m != "" {
				r.problems = append(r.problems, fmt.Sprintf("%s:%d: inline suppression %q: %s", rel, i+1, m, fix))
			}
		}
	}
	if len(r.problems) == 0 {
		r.out = []string{fmt.Sprintf("suppress: %d file(s), no inline suppressions", len(files))}
	}
	return r, nil
}
