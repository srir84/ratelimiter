package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// codeNameRe is a §A4.9 reply-code name as the prelude defines it.
var codeNameRe = regexp.MustCompile(`^(?:E_[A-Z0-9_]+|OK|REFUSED|HEALTHY)$`)

// luaToken is one Lua 5.1 token with its byte range in the source (a string's range holds its
// quotes; text is what lies between them). Comments and white space are dropped.
type luaToken struct {
	kind       byte // 'n' name, 's' string, 'p' punctuation or number
	text       string
	start, end int
	line       int
}

// scanLua splits Lua source into tokens. A first line starting with '#' (the store's "#!lua …"
// header) is skipped, as the store strips it.
func scanLua(src string) ([]luaToken, error) {
	var toks []luaToken
	line, i := 1, 0
	if strings.HasPrefix(src, "#") {
		for i < len(src) && src[i] != '\n' {
			i++
		}
	}
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
		case strings.HasPrefix(src[i:], "--"):
			i += 2
			if level, open := longBracket(src, i); open > 0 {
				end, err := longEnd(src, i+open, level)
				if err != nil {
					return nil, fmt.Errorf("line %d: comment: %w", line, err)
				}
				line += strings.Count(src[i:end], "\n")
				i = end
				continue
			}
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '[' && longOpen(src, i):
			level, open := longBracket(src, i)
			end, err := longEnd(src, i+open, level)
			if err != nil {
				return nil, fmt.Errorf("line %d: string: %w", line, err)
			}
			toks = append(toks, luaToken{kind: 's', text: src[i+open : end-open], start: i, end: end, line: line})
			line += strings.Count(src[i:end], "\n")
			i = end
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\n' {
					return nil, fmt.Errorf("line %d: unterminated string", line)
				}
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("line %d: unterminated string", line)
			}
			toks = append(toks, luaToken{kind: 's', text: src[i+1 : j], start: i, end: j + 1, line: line})
			i = j + 1
		case isNameStart(c):
			j := i + 1
			for j < len(src) && (isNameStart(src[j]) || isDigit(src[j])) {
				j++
			}
			toks = append(toks, luaToken{kind: 'n', text: src[i:j], start: i, end: j, line: line})
			i = j
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			j := i + 1
			for j < len(src) && (isDigit(src[j]) || src[j] == '.' || isNameStart(src[j]) ||
				((src[j] == '+' || src[j] == '-') && (src[j-1] == 'e' || src[j-1] == 'E'))) {
				j++
			}
			toks = append(toks, luaToken{kind: 'p', text: src[i:j], start: i, end: j, line: line})
			i = j
		default:
			n := 1
			for _, op := range []string{"...", "..", "==", "~=", "<=", ">="} {
				if strings.HasPrefix(src[i:], op) {
					n = len(op)
					break
				}
			}
			toks = append(toks, luaToken{kind: 'p', text: src[i : i+n], start: i, end: i + n, line: line})
			i += n
		}
	}
	return toks, nil
}

func longOpen(src string, i int) bool {
	_, n := longBracket(src, i)
	return n > 0
}

// longBracket reports whether src[i:] opens a long bracket "[[" or "[==[": its level and the
// opener's length (0 when it is none).
func longBracket(src string, i int) (level, length int) {
	if i >= len(src) || src[i] != '[' {
		return 0, 0
	}
	j := i + 1
	for j < len(src) && src[j] == '=' {
		j++
	}
	if j < len(src) && src[j] == '[' {
		return j - i - 1, j - i + 1
	}
	return 0, 0
}

// longEnd returns the index just after the "]==]" of the given level that closes a long bracket.
func longEnd(src string, start, level int) (int, error) {
	closer := "]" + strings.Repeat("=", level) + "]"
	k := strings.Index(src[start:], closer)
	if k < 0 {
		return 0, errors.New("unterminated long bracket")
	}
	return start + k + len(closer), nil
}

// swapPair returns b for a, and a for anything else.
func swapPair(s, a, b string) string {
	if s == a {
		return b
	}
	return a
}

func isNameStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// luaCodeNames returns the reply-code names a prelude defines (top-level local NAME = <integer>).
func luaCodeNames(prelude string) (map[string]bool, error) {
	toks, err := scanLua(prelude)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	depth := 0
	for i, t := range toks {
		if t.kind == 'n' {
			switch t.text {
			case "function", "if", "do", "repeat":
				depth++
			case "end", "until":
				depth--
			}
		}
		if depth == 0 && t.kind == 'n' && t.text == "local" && i+3 < len(toks) && codeNameRe.MatchString(toks[i+1].text) &&
			toks[i+2].text == "=" && toks[i+3].kind == 'p' && isDigit(toks[i+3].text[0]) {
			names[toks[i+1].text] = true
		}
	}
	return names, nil
}

// writeCommand reports whether a store command is a write (§A4.8 rule 2).
func writeCommand(cmd string) bool {
	switch strings.ToUpper(cmd) {
	case "HSET", "HDEL", "HINCRBY", "ZADD", "ZREM", "DEL", "UNLINK":
		return true
	}
	return false
}

// edit replaces src[start:end] with text.
type edit struct {
	start, end int
	text       string
}

// applyEdits applies non-overlapping edits to src.
func applyEdits(src string, edits []edit) string {
	slices.SortFunc(edits, func(a, b edit) int { return a.start - b.start })
	var b strings.Builder
	at := 0
	for _, e := range edits {
		b.WriteString(src[at:e.start])
		b.WriteString(e.text)
		at = e.end
	}
	b.WriteString(src[at:])
	return b.String()
}

// lineRange returns the byte range of lines first … last (1-based), with the final newline.
func lineRange(src string, first, last int) (int, int) {
	start, line := 0, 1
	for line < first {
		i := strings.IndexByte(src[start:], '\n')
		if i < 0 {
			return len(src), len(src)
		}
		start += i + 1
		line++
	}
	end := start
	for line <= last {
		i := strings.IndexByte(src[end:], '\n')
		if i < 0 {
			return start, len(src)
		}
		end += i + 1
		line++
	}
	return start, end
}

// luaMutants applies the Lua operators of DESIGN §C9 to one file: delete one write-call line (a
// call spanning lines goes whole), delete one write_hwm(), finish( ↔ reply(, 'GT' ↔ 'LT',
// delete 'XX', and swap two reply-code names (every use of the two in the file).
func luaMutants(file, src string, codeNames map[string]bool) ([]mutant, error) {
	toks, err := scanLua(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	at := func(i int, kind byte, text string) bool {
		return i >= 0 && i < len(toks) && toks[i].kind == kind && (text == "" || toks[i].text == text)
	}
	field := func(i int) bool { return at(i-1, 'p', ".") || at(i-1, 'p', ":") }
	closing := func(open int) int { // the index of the ")" matching toks[open]
		depth := 0
		for j := open; j < len(toks); j++ {
			switch {
			case toks[j].kind == 'p' && (toks[j].text == "(" || toks[j].text == "[" || toks[j].text == "{"):
				depth++
			case toks[j].kind == 'p' && (toks[j].text == ")" || toks[j].text == "]" || toks[j].text == "}"):
				if depth--; depth == 0 {
					return j
				}
			}
		}
		return -1
	}
	var ms []mutant
	add := func(line int, desc string, edits ...edit) {
		ms = append(ms, mutant{file: file, line: line, desc: desc, content: []byte(applyEdits(src, edits))})
	}
	deleteLines := func(first, last int) edit {
		s, e := lineRange(src, first, last)
		return edit{s, e, ""}
	}
	for i, t := range toks {
		switch {
		case t.kind == 'n' && t.text == "redis" && !field(i) && at(i+1, 'p', ".") && (at(i+2, 'n', "call") || at(i+2, 'n', "pcall")) &&
			at(i+3, 'p', "(") && at(i+4, 's', "") && writeCommand(toks[i+4].text):
			if c := closing(i + 3); c > 0 {
				add(t.line, "delete the write call "+strings.ToUpper(toks[i+4].text), deleteLines(t.line, toks[c].line))
			}
		case t.kind == 'n' && t.text == "pcall" && !field(i) && at(i+1, 'p', "(") && at(i+2, 'n', "redis") && at(i+3, 'p', ".") &&
			at(i+4, 'n', "call") && at(i+5, 'p', ",") && at(i+6, 's', "") && writeCommand(toks[i+6].text):
			if c := closing(i + 1); c > 0 {
				add(t.line, "delete the write call "+strings.ToUpper(toks[i+6].text), deleteLines(t.line, toks[c].line))
			}
		case t.kind == 'n' && t.text == "write_hwm" && !field(i) && !at(i-1, 'n', "function") && at(i+1, 'p', "(") && at(i+2, 'p', ")"):
			s, e := lineRange(src, t.line, t.line)
			rest := src[s:t.start] + src[toks[i+2].end:e]
			if strings.TrimSpace(rest) == "" {
				add(t.line, "delete write_hwm()", edit{s, e, ""})
			} else {
				add(t.line, "delete write_hwm()", edit{t.start, toks[i+2].end, ""})
			}
		case t.kind == 'n' && (t.text == "finish" || t.text == "reply") && !field(i) && !at(i-1, 'n', "function") && at(i+1, 'p', "("):
			other := swapPair(t.text, "finish", "reply")
			add(t.line, t.text+"( → "+other+"(", edit{t.start, t.end, other})
		case t.kind == 's' && (t.text == "GT" || t.text == "LT") && t.end-t.start == 4:
			other := swapPair(t.text, "GT", "LT")
			add(t.line, "'"+t.text+"' → '"+other+"'", edit{t.start + 1, t.end - 1, other})
		case t.kind == 's' && t.text == "XX" && at(i+1, 'p', ",") && i+2 < len(toks):
			add(t.line, "delete 'XX'", edit{t.start, toks[i+2].start, ""})
		}
	}
	// Swap two reply-code names: every use of X becomes Y and every use of Y becomes X. A
	// definition (local X = …) stays.
	uses := map[string][]int{}
	var order []string
	for i, t := range toks {
		if t.kind != 'n' || !codeNames[t.text] || field(i) || (at(i-1, 'n', "local") && at(i+1, 'p', "=")) {
			continue
		}
		if uses[t.text] == nil {
			order = append(order, t.text)
		}
		uses[t.text] = append(uses[t.text], i)
	}
	for a := 0; a < len(order); a++ {
		for b := a + 1; b < len(order); b++ {
			x, y := order[a], order[b]
			var edits []edit
			for _, i := range uses[x] {
				edits = append(edits, edit{toks[i].start, toks[i].end, y})
			}
			for _, i := range uses[y] {
				edits = append(edits, edit{toks[i].start, toks[i].end, x})
			}
			add(toks[uses[x][0]].line, "swap the reply-code names "+x+" and "+y, edits...)
		}
	}
	return ms, nil
}

// luacheckRe is one line of luacheck's plain formatter with --codes.
var luacheckRe = regexp.MustCompile(`^(.+?):(\d+):(\d+): \((E\d+)\) (.*)$`)

// luaTarget is one file given to luacheck: the script it stands for and, when it was assembled,
// the number of lines the prelude adds (-1 when it is checked as it is).
type luaTarget struct {
	script  string
	prelude int
}

// sourceLine maps a line of an assembled script back to the file it came from: line 1 is the
// script's, then the prelude's lines, then the rest of the script.
func (t luaTarget) sourceLine(line int) (string, int) {
	switch {
	case t.prelude < 0 || line == 1:
		return t.script, line
	case line <= t.prelude+1:
		return "prelude.lua", line - 1
	}
	return t.script, line - t.prelude
}

// luaSyntaxError reports why a .lua mutant does not parse ("" when it does): luacheck on each
// script as the store receives it (its first line, the prelude, the rest), so a prelude mutant
// is checked inside every script of its directory. The reason names the source file and line.
func luaSyntaxError(ctx context.Context, cfg config, m mutant) (why string, err error) {
	dir := filepath.Join(cfg.root, filepath.FromSlash(path.Dir(m.file)))
	tmp, err := os.MkdirTemp(cfg.tmpDir, "mutate-lua-")
	if err != nil {
		return "", fmt.Errorf("creating the luacheck directory: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(tmp); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the luacheck directory: %w", rerr))
		}
	}()
	targets := map[string]luaTarget{}
	var paths []string
	write := func(name, text string, t luaTarget) error {
		p := filepath.Join(tmp, name)
		paths = append(paths, p)
		targets[p] = t
		return os.WriteFile(p, []byte(text), 0o644)
	}
	prelude, err := os.ReadFile(filepath.Join(dir, "prelude.lua"))
	hasPrelude := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	switch {
	case path.Base(m.file) == "prelude.lua":
		scripts, err := filepath.Glob(filepath.Join(dir, "*.lua"))
		if err != nil {
			return "", err
		}
		p := strings.Count(string(m.content), "\n")
		for _, s := range scripts {
			name := filepath.Base(s)
			if name == "prelude.lua" {
				continue
			}
			src, err := os.ReadFile(s)
			if err != nil {
				return "", err
			}
			header, rest, _ := strings.Cut(string(src), "\n")
			if err := write(name, header+"\n"+string(m.content)+rest, luaTarget{name, p}); err != nil {
				return "", err
			}
		}
		if len(paths) == 0 {
			if err := write("prelude.lua", string(m.content), luaTarget{"prelude.lua", -1}); err != nil {
				return "", err
			}
		}
	case hasPrelude:
		name := path.Base(m.file)
		header, rest, _ := strings.Cut(string(m.content), "\n")
		if err := write(name, header+"\n"+string(prelude)+rest, luaTarget{name, strings.Count(string(prelude), "\n")}); err != nil {
			return "", err
		}
	default:
		name := path.Base(m.file)
		if err := write(name, string(m.content), luaTarget{name, -1}); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := append([]string{"--no-config", "--codes", "--no-color", "--formatter", "plain"}, paths...)
	cmd := exec.CommandContext(ctx, cfg.luacheck, args...)
	cmd.Dir, cmd.Env = tmp, cfg.environ
	out, runErr := cmd.Output()
	var exitErr *exec.ExitError
	if runErr != nil && (!errors.As(runErr, &exitErr) || exitErr.ExitCode() > 2) {
		return "", fmt.Errorf("luacheck: %w: %s", runErr, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		mm := luacheckRe.FindStringSubmatch(strings.TrimSpace(line))
		if mm == nil || !strings.HasPrefix(mm[4], "E0") {
			continue
		}
		t, ok := targets[mm[1]]
		if !ok {
			t, ok = targets[filepath.Join(tmp, mm[1])]
		}
		if !ok {
			return strings.TrimSpace(line), nil
		}
		n, err := strconv.Atoi(mm[2])
		if err != nil {
			return strings.TrimSpace(line), nil
		}
		file, at := t.sourceLine(n)
		return fmt.Sprintf("%s:%d:%s: (%s) %s", file, at, mm[3], mm[4], mm[5]), nil
	}
	if exitErr != nil && exitErr.ExitCode() == 2 {
		return "luacheck reports an error", nil
	}
	return "", nil
}
