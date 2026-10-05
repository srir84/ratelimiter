package main

import (
	"fmt"
	"strings"
)

// luaTok is one Lua 5.1 token: a name, a string literal's contents, or anything else
// (punctuation and numbers). Comments and white space are dropped.
type luaTok struct {
	kind byte // 'n' name, 's' string, 'p' punctuation or number
	text string
	line int
}

// luaComment is one comment: its text (after "--", or between a long comment's brackets,
// line breaks included) and the line it starts on.
type luaComment struct {
	text string
	line int
}

// luaLexError is a lexical error: Lua rejects the source from this line on.
type luaLexError struct {
	line int
	err  error
}

func (e *luaLexError) Error() string { return fmt.Sprintf("line %d: %v", e.line, e.err) }

func (e *luaLexError) Unwrap() error { return e.err }

// luaLex splits Lua source into tokens. A first line starting with '#' (the store's
// "#!lua …" header) is skipped, as Valkey strips it before compiling.
func luaLex(src string) ([]luaTok, error) {
	toks, _, err := luaScan(src)
	return toks, err
}

// luaScan is luaLex that also returns every comment, short or long, in source order.
// Its error is a *luaLexError.
func luaScan(src string) ([]luaTok, []luaComment, error) {
	var toks []luaTok
	var comments []luaComment
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
		case c == '-' && strings.HasPrefix(src[i:], "--"):
			i += 2
			if level, open := longBracket(src, i); open > 0 {
				body, end, err := longBody(src, i+open, level)
				if err != nil {
					return nil, nil, &luaLexError{line: line, err: fmt.Errorf("comment: %w", err)}
				}
				comments = append(comments, luaComment{text: body, line: line})
				line += strings.Count(body, "\n")
				i = end
				continue
			}
			start := i
			for i < len(src) && src[i] != '\n' {
				i++
			}
			comments = append(comments, luaComment{text: src[start:i], line: line})
		case c == '[':
			level, open := longBracket(src, i)
			if open == 0 {
				toks = append(toks, luaTok{kind: 'p', text: "[", line: line})
				i++
				continue
			}
			body, end, err := longBody(src, i+open, level)
			if err != nil {
				return nil, nil, &luaLexError{line: line, err: fmt.Errorf("string: %w", err)}
			}
			toks = append(toks, luaTok{kind: 's', text: body, line: line})
			line += strings.Count(body, "\n")
			i = end
		case c == '\'' || c == '"':
			val, end, nl, err := shortString(src, i)
			if err != nil {
				return nil, nil, &luaLexError{line: line, err: err}
			}
			toks = append(toks, luaTok{kind: 's', text: val, line: line})
			line += nl
			i = end
		case isNameStart(c):
			j := i + 1
			for j < len(src) && (isNameStart(src[j]) || isDigit(src[j])) {
				j++
			}
			toks = append(toks, luaTok{kind: 'n', text: src[i:j], line: line})
			i = j
		case strings.HasPrefix(src[i:], ".."):
			// Concatenation ".." and the vararg "..." are one token each, as in Lua's own
			// lexer, so "x .. redis.call(…)" is never taken for a field access.
			op := ".."
			if strings.HasPrefix(src[i:], "...") {
				op = "..."
			}
			toks = append(toks, luaTok{kind: 'p', text: op, line: line})
			i += len(op)
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			// A numeral as Lua 5.1's read_numeral reads it: digits and '.', then one
			// optional e or E with an optional sign, then letters, digits and '_'. So no
			// sign ever follows 0x… and there is no p exponent: 0xE-redis.call(…) is a
			// subtraction, and 0xe--… ends the numeral before a comment.
			j := i + 1
			for j < len(src) && (isDigit(src[j]) || src[j] == '.') {
				j++
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
			}
			for j < len(src) && (isNameStart(src[j]) || isDigit(src[j])) {
				j++
			}
			toks = append(toks, luaTok{kind: 'p', text: src[i:j], line: line})
			i = j
		default:
			toks = append(toks, luaTok{kind: 'p', text: string(c), line: line})
			i++
		}
	}
	return toks, comments, nil
}

// longBracket reports whether src[i:] opens a long bracket "[[" or "[==[": it returns
// the number of '=' and the opener's length (0 when it is not one).
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

// longBody returns the text up to the closing "]==]" of the given level and the index after it.
func longBody(src string, start, level int) (string, int, error) {
	closer := "]" + strings.Repeat("=", level) + "]"
	k := strings.Index(src[start:], closer)
	if k < 0 {
		return "", 0, fmt.Errorf("unterminated long bracket")
	}
	return src[start : start+k], start + k + len(closer), nil
}

// shortString reads a quoted string starting at src[i]; it returns its contents (escape
// backslashes dropped), the index after the closing quote, and the newlines it spans.
func shortString(src string, i int) (string, int, int, error) {
	q := src[i]
	var b strings.Builder
	nl := 0
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; {
		case c == q:
			return b.String(), j + 1, nl, nil
		case c == '\n':
			return "", 0, 0, fmt.Errorf("unterminated string")
		case c == '\\' && j+1 < len(src):
			j++
			if src[j] == '\n' {
				nl++
			}
			b.WriteByte(src[j])
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, 0, fmt.Errorf("unterminated string")
}

func isNameStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// luaCall is one store command a script calls, or a call trace cannot name.
type luaCall struct {
	cmd     string // lower case; "" when dynamic
	line    int
	dynamic string // the form that hides the command, when trace cannot name it
}

// luaCalls returns the commands a script calls. Only three forms name a command
// (AGENTS.md §3): redis.call('X', …), redis.pcall('X', …) and pcall(redis.call, 'X', …),
// where the string literal is the whole argument (the next token is "," or ")"). Every
// other use of redis.call or redis.pcall is dynamic, and so is any use of redis other
// than redis.<name> (local r = redis, redis[…], redis:…), since it hides later calls.
// Four more names reach the store, each dynamic wherever it is not a field or method
// name: server, the API's own name in Valkey (redis is the alias); _G, which reaches any
// global by name; and loadstring and load, which compile code at run time, out of trace's
// sight. So _G.redis.call(…), server.call(…) and loadstring('…')() cannot call the store
// unseen.
func luaCalls(src string) ([]luaCall, error) {
	toks, err := luaLex(src)
	if err != nil {
		return nil, err
	}
	at := func(i int, kind byte, text string) bool {
		return i >= 0 && i < len(toks) && toks[i].kind == kind && (text == "" || toks[i].text == text)
	}
	// field reports whether toks[i] is a field or method name (x.name, x:name).
	field := func(i int) bool { return at(i-1, 'p', ".") || at(i-1, 'p', ":") }
	// literalArg reports whether toks[i] is a string literal that is a whole argument.
	literalArg := func(i int) bool { return at(i, 's', "") && (at(i+1, 'p', ",") || at(i+1, 'p', ")")) }
	var calls []luaCall
	for i, t := range toks {
		if t.kind != 'n' || field(i) {
			continue
		}
		switch t.text {
		case "server":
			calls = append(calls, luaCall{line: t.line, dynamic: "server is the store API under its Valkey name, which no named form uses"})
			continue
		case "_G":
			calls = append(calls, luaCall{line: t.line, dynamic: "_G reaches any global by name, the store API among them"})
			continue
		case "loadstring", "load":
			calls = append(calls, luaCall{line: t.line, dynamic: t.text + " compiles code at run time, whose store calls trace cannot see"})
			continue
		}
		if t.text != "redis" {
			continue
		}
		if !at(i+1, 'p', ".") || !at(i+2, 'n', "") {
			calls = append(calls, luaCall{line: t.line, dynamic: "redis used other than as redis.<name>"})
			continue
		}
		fn := "redis." + toks[i+2].text
		if fn != "redis.call" && fn != "redis.pcall" {
			continue // redis.sha1hex, redis.status_reply, …: no store command
		}
		direct := at(i+3, 'p', "(")
		viaPcall := fn == "redis.call" && at(i-1, 'p', "(") && at(i-2, 'n', "pcall") && !field(i-2) && at(i+3, 'p', ",")
		switch {
		case (direct || viaPcall) && literalArg(i+4):
			calls = append(calls, luaCall{cmd: strings.ToLower(toks[i+4].text), line: t.line})
		case direct:
			calls = append(calls, luaCall{line: t.line, dynamic: fn + " with no string literal command"})
		case viaPcall:
			calls = append(calls, luaCall{line: t.line, dynamic: "pcall(redis.call, …) with no string literal command"})
		default:
			calls = append(calls, luaCall{line: t.line, dynamic: fn + " used other than called as " + fn + "('X', …)"})
		}
	}
	return calls, nil
}
