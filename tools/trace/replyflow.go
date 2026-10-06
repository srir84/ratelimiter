package main

import (
	"fmt"
	"slices"
)

// The value flow that check (a) of trace replies follows (plan S4 step 15 (a); S4 repair F1),
// so that every table constructor a return can give is read as a reply or reported. A return
// gives the operands of its expressions that pass a value through: split at and/or and taken
// out of parentheses, each is a table constructor; a local variable, and so every value
// assigned to it anywhere in the file (the variable a name means is the local in scope there,
// as in Lua); or a field or element of a local (t.x, t[k]). A call's result, the value a
// parameter or a for loop's variable brings in, a literal and a function are out of reach (no
// call-graph expansion), and an arithmetic, comparison or concatenation, not or # is never a
// table. Field and element returns, names that are no local of the file, tables or locals
// assigned to such a name, and assignments by index to a returned local are what (a) cannot
// read; a statement the walk cannot parse is reported too.

// flowRef is one operand that passes a value through.
type flowRef struct {
	ctor int    // the '{' of a table constructor, or -1
	decl int    // the local variable the name means, or -1 (none of the file's locals)
	name string // the name, for messages
	elem bool   // a field or element of the name (t.x, t[k]), not its value
	line int
}

// flowDecl is one local variable: of a local statement, a local function, a parameter or a
// for loop.
type flowDecl struct {
	name string
	fn   int // the function it belongs to (0: the file's top level)
}

// flowSet is an assignment by index to a variable, NAME[key] = value.
type flowSet struct {
	decl           int // the local NAME means, or -1
	name           string
	keyFrom, keyTo int // the key's tokens: toks[keyFrom:keyTo]
	line           int
}

// flowFrame is one open block of the walk, with the locals declared in it, or one open bracket.
type flowFrame struct {
	kind byte           // 'f' a function's body, 'b' a block, 'i' a branch of an if; '(', '[' or '{'
	fn   int            // the function it lies in
	vars map[string]int // the locals declared in it; nil for a bracket
}

// luaFlow is what the walk of one Lua file found.
type luaFlow struct {
	toks    []luaTok
	decls   []flowDecl
	binds   map[int][]flowRef // per local, the operands of every value assigned to it
	rets    []flowRef         // the operands of every return
	sets    []flowSet         // the assignments by index
	inTable []bool            // per token: the innermost open bracket is a table constructor's
	unread  []siteProblem     // statements the walk could not parse
	sinks   []siteProblem     // tables and locals assigned to names that are no local of the file
	stack   []flowFrame
}

// luaFlowOf walks toks once, with Lua's scoping: a local statement's names are visible after
// it, a local function's inside its own body, the parameters and a for loop's variables in
// their body, and an if's locals in their branch only.
func luaFlowOf(toks []luaTok) *luaFlow {
	f := &luaFlow{toks: toks, binds: map[int][]flowRef{}, inTable: make([]bool, len(toks)),
		stack: []flowFrame{{kind: 'f', vars: map[string]int{}}}}
	type pending struct { // a local statement's names, visible from token at on
		at   int
		ids  []int
		vars map[string]int
	}
	var later []pending
	owned := map[int]bool{}        // the '=' of local and for statements
	loopVars := map[int][]string{} // a for loop's do → its variables
	nextFn := 1                    // luaScopes's numbering: one per function keyword
	for i := 0; i < len(toks); i++ {
		later = slices.DeleteFunc(later, func(p pending) bool {
			if p.at > i {
				return false
			}
			for _, id := range p.ids {
				p.vars[f.decls[id].name] = id
			}
			return true
		})
		f.inTable[i] = f.top().kind == '{'
		t := toks[i]
		switch {
		case t.kind == 's':
		case t.kind == 'p':
			switch t.text {
			case "(", "[", "{":
				f.stack = append(f.stack, flowFrame{kind: t.text[0], fn: f.top().fn})
			case ")", "]", "}":
				if k := f.top().kind; k == '(' || k == '[' || k == '{' {
					f.stack = f.stack[:len(f.stack)-1]
				}
			case "=":
				if !owned[i] && f.statement() && soleEq(toks, i) {
					f.assign(i)
				}
			}
		case t.text == "local":
			if tokIs(toks, i+1, 'n', "function") {
				if tokIs(toks, i+2, 'n', "") && !luaKeyword(toks[i+2].text) {
					f.innermost()[toks[i+2].text] = f.newDecl(toks[i+2].text)
				}
				continue
			}
			names, j := nameList(toks, i+1)
			ids := make([]int, len(names))
			for k, n := range names {
				ids[k] = f.newDecl(n)
			}
			if !tokIs(toks, j, 'p', "=") || !soleEq(toks, j) {
				for _, id := range ids {
					f.innermost()[f.decls[id].name] = id
				}
				continue
			}
			owned[j] = true
			exprs, end, ok := scanExprList(toks, j+1)
			if !ok {
				f.unread = append(f.unread, siteProblem{t.line, "local: cannot read the values it assigns, so the tables they may hold cannot be followed"})
			}
			for k, id := range ids {
				if k < len(exprs) {
					f.binds[id] = append(f.binds[id], f.refs(exprs[k][0], exprs[k][1])...)
				}
			}
			later = append(later, pending{at: end, ids: ids, vars: f.innermost()})
		case t.text == "function":
			params, j := paramList(toks, i)
			f.stack = append(f.stack, flowFrame{kind: 'f', fn: nextFn, vars: map[string]int{}})
			nextFn++
			for _, p := range params {
				f.innermost()[p] = f.newDecl(p)
			}
			i = j // the function's name and parameters refer to nothing
		case t.text == "if":
			f.stack = append(f.stack, flowFrame{kind: 'i', fn: f.top().fn, vars: map[string]int{}})
		case t.text == "elseif" || t.text == "else":
			if top := &f.stack[len(f.stack)-1]; top.kind == 'i' {
				top.vars = map[string]int{} // the next branch: the last one's locals are gone
			}
		case t.text == "do":
			f.stack = append(f.stack, flowFrame{kind: 'b', fn: f.top().fn, vars: map[string]int{}})
			for _, n := range loopVars[i] {
				f.innermost()[n] = f.newDecl(n)
			}
		case t.text == "repeat":
			f.stack = append(f.stack, flowFrame{kind: 'b', fn: f.top().fn, vars: map[string]int{}})
		case t.text == "for":
			names, j := nameList(toks, i+1)
			if tokIs(toks, j, 'p', "=") || tokIs(toks, j, 'n', "in") {
				owned[j] = true
				if _, end, ok := scanExprList(toks, j+1); ok && tokIs(toks, end, 'n', "do") {
					loopVars[end] = names
				}
			}
		case t.text == "end" || t.text == "until":
			f.closeBlock()
		case t.text == "return":
			if !f.statement() {
				continue
			}
			exprs, _, ok := scanExprList(toks, i+1)
			if !ok {
				f.unread = append(f.unread, siteProblem{t.line, "return: cannot read the values it gives, so the tables they may hold cannot be followed"})
			}
			for _, e := range exprs {
				f.rets = append(f.rets, f.refs(e[0], e[1])...)
			}
		}
	}
	return f
}

func (f *luaFlow) top() flowFrame { return f.stack[len(f.stack)-1] }

// statement reports whether the walk is at the level of statements, not inside a bracket.
func (f *luaFlow) statement() bool {
	k := f.top().kind
	return k == 'f' || k == 'b' || k == 'i'
}

// innermost returns the locals of the innermost open block.
func (f *luaFlow) innermost() map[string]int {
	for k := len(f.stack) - 1; ; k-- {
		if f.stack[k].vars != nil {
			return f.stack[k].vars
		}
	}
}

func (f *luaFlow) newDecl(name string) int {
	f.decls = append(f.decls, flowDecl{name: name, fn: f.top().fn})
	return len(f.decls) - 1
}

// resolve returns the local that name means at this point of the walk, or -1.
func (f *luaFlow) resolve(name string) int {
	for k := len(f.stack) - 1; k >= 0; k-- {
		if id, ok := f.stack[k].vars[name]; ok {
			return id
		}
	}
	return -1
}

// closeBlock ends the innermost block (end or until), with any bracket left open in it; the
// file's top level never ends.
func (f *luaFlow) closeBlock() {
	for len(f.stack) > 1 {
		k := f.top().kind
		f.stack = f.stack[:len(f.stack)-1]
		if k == 'f' || k == 'b' || k == 'i' {
			return
		}
	}
}

// assign reads the assignment whose '=' is toks[eq]: each target that is a name gets the
// operands of its value; one that is NAME[key] is an assignment by index.
func (f *luaFlow) assign(eq int) {
	line := f.toks[eq].line
	targets := assignTargets(f.toks, eq)
	exprs, _, ok := scanExprList(f.toks, eq+1)
	if targets == nil || !ok {
		f.unread = append(f.unread, siteProblem{line, "=: cannot read the assignment, so the tables it may store cannot be followed"})
	}
	for k, tg := range targets {
		var refs []flowRef
		if k < len(exprs) {
			refs = f.refs(exprs[k][0], exprs[k][1])
		}
		from, to := tg[0], tg[1]
		name := f.toks[from].text
		switch {
		case from == to && f.toks[from].kind == 'n':
			if id := f.resolve(name); id >= 0 {
				f.binds[id] = append(f.binds[id], refs...)
			} else if slices.ContainsFunc(refs, func(r flowRef) bool { return r.ctor >= 0 || r.decl >= 0 }) {
				f.sinks = append(f.sinks, siteProblem{line, fmt.Sprintf("%s: cannot read: a table or a local is assigned to %s, which is no local of this file, so where it goes cannot be followed", name, name)})
			}
		case f.toks[from].kind == 'n' && tokIs(f.toks, from+1, 'p', "[") && matchClose(f.toks, from+1) == to:
			f.sets = append(f.sets, flowSet{decl: f.resolve(name), name: name, keyFrom: from + 2, keyTo: to, line: line})
		}
	}
}

// refs returns the operands of the expression toks[s:e] that pass a value through: split at
// and/or (outside brackets and function bodies), each operand read by operand.
func (f *luaFlow) refs(s, e int) []flowRef {
	var out []flowRef
	start := s
	for k := s; k <= e; k++ {
		if k < e {
			t := f.toks[k]
			switch {
			case t.kind == 'p' && (t.text == "(" || t.text == "[" || t.text == "{"):
				if c := matchClose(f.toks, k); c > k && c < e {
					k = c
				}
				continue
			case t.kind == 'n' && t.text == "function":
				if c := funcEnd(f.toks, k); c > k && c < e {
					k = c
				}
				continue
			case t.kind != 'n' || (t.text != "and" && t.text != "or"):
				continue
			}
		}
		out = append(out, f.operand(start, k)...)
		start = k + 1
	}
	return out
}

// operand reads one operand of and/or, toks[a:b].
func (f *luaFlow) operand(a, b int) []flowRef {
	if a >= b || isUnary(f.toks[a]) {
		return nil
	}
	for k := a; k < b; k++ {
		t := f.toks[k]
		switch {
		case t.kind == 'p' && (t.text == "(" || t.text == "[" || t.text == "{"):
			if c := matchClose(f.toks, k); c > k && c < b {
				k = c
			}
		case t.kind == 'n' && t.text == "function":
			if c := funcEnd(f.toks, k); c > k && c < b {
				k = c
			}
		case t.kind == 'p' && isOperatorChar(t.text):
			return nil // arithmetic, comparison or concatenation: never a table
		}
	}
	t := f.toks[a]
	var base []flowRef
	k := a + 1
	switch {
	case t.kind == 'p' && t.text == "(":
		c := matchClose(f.toks, a)
		if c < 0 || c >= b {
			return nil
		}
		base, k = f.refs(a+1, c), c+1
	case t.kind == 'p' && t.text == "{":
		return []flowRef{{ctor: a, decl: -1, name: "{…}", line: t.line}}
	case t.kind == 'n' && !luaKeyword(t.text):
		base = []flowRef{{ctor: -1, decl: f.resolve(t.text), name: t.text, line: t.line}}
	default:
		return nil // a literal, a function or '...'
	}
	elem := false
	for k < b {
		s := f.toks[k]
		switch {
		case s.kind == 'p' && s.text == "." && k+1 < b:
			k, elem = k+2, true
		case s.kind == 'p' && s.text == "[":
			c := matchClose(f.toks, k)
			if c < 0 || c >= b {
				return nil
			}
			k, elem = c+1, true
		default:
			return nil // a call: its result is out of reach
		}
	}
	for n := range base {
		if elem && base[n].ctor < 0 {
			base[n].elem = true // a field or element of what the name or parentheses give
		}
	}
	return base
}

// returned follows the returns' operands through the values of the locals they name, and
// returns the table constructors a return can give, and the problems: a field or element of a
// local that holds a table or another value assigned in the file, a name that is no local of
// the file (but KEYS, ARGV or a code name), and an assignment by index, other than an append
// NAME[#NAME + 1], to a local a return gives. data reports a local declared in replyData: its
// tables built empty or from name = value fields are data, never replies, and its assignments
// by index are not checked.
func (f *luaFlow) returned(data func(decl int) bool, consts map[string]int) (map[int]bool, []siteProblem) {
	ctors, whole, elems := map[int]bool{}, map[int]bool{}, map[int]bool{}
	var problems []siteProblem
	work := slices.Clone(f.rets)
	for len(work) > 0 {
		r := work[len(work)-1]
		work = work[:len(work)-1]
		switch {
		case r.ctor >= 0:
			ctors[r.ctor] = true
		case r.decl < 0:
			if _, code := consts[r.name]; r.name == "KEYS" || r.name == "ARGV" || (code && !r.elem) {
				continue // the caller's strings, or a code name's number
			}
			what := r.name
			if r.elem {
				what = "a field or element of " + r.name
			}
			problems = append(problems, siteProblem{r.line, fmt.Sprintf("%s: cannot read: a return gives %s, which is no local of this file, so the tables it may hold cannot be read", r.name, what)})
		case r.elem:
			if elems[r.decl] || data(r.decl) {
				continue
			}
			elems[r.decl] = true
			if len(f.binds[r.decl]) > 0 {
				problems = append(problems, siteProblem{r.line, fmt.Sprintf("%s: cannot read: a return gives a field or element of %s, a local that holds a table or another value of this file (a reply is returned whole)", r.name, r.name)})
			}
		case !whole[r.decl]:
			whole[r.decl] = true
			for _, b := range f.binds[r.decl] {
				if b.ctor >= 0 && data(r.decl) && f.dataShape(b.ctor) {
					continue
				}
				work = append(work, b)
			}
		}
	}
	for _, s := range f.sets {
		if s.decl < 0 || !whole[s.decl] || data(s.decl) || f.appends(s) {
			continue
		}
		problems = append(problems, siteProblem{s.line, fmt.Sprintf("%s[…]: cannot read: %s is returned, and a reply changed by index after its constructor cannot be read (only an append, %s[#%s + 1], may follow it)",
			s.name, s.name, s.name, s.name)})
	}
	return ctors, problems
}

// dataShape reports whether the constructor at toks[open] is empty or holds only name = value
// fields: no positional field, so no code.
func (f *luaFlow) dataShape(open int) bool {
	fields, ok := splitList(f.toks, open, "}")
	if !ok {
		return false
	}
	for _, fl := range fields {
		if len(fl) < 3 || fl[0].kind != 'n' || luaKeyword(fl[0].text) || fl[1].kind != 'p' || fl[1].text != "=" || (fl[2].kind == 'p' && fl[2].text == "=") {
			return false
		}
	}
	return true
}

// appends reports whether s is NAME[#NAME + 1] = value.
func (f *luaFlow) appends(s flowSet) bool {
	k := f.toks[s.keyFrom:s.keyTo]
	return len(k) == 4 && k[0].text == "#" && k[1].kind == 'n' && k[1].text == s.name && k[2].text == "+" && k[3].text == "1"
}

// scanExprList reads the expression list that starts at toks[i]: the [start, end) of each
// expression, the index after the list, and false when it cannot be read. An empty list (a
// bare return) ends at ';', a keyword that ends a block, or the end of the file.
func scanExprList(toks []luaTok, i int) ([][2]int, int, bool) {
	if i >= len(toks) || tokIs(toks, i, 'p', ";") || (toks[i].kind == 'n' && blockEnd(toks[i].text)) {
		return nil, i, true
	}
	var exprs [][2]int
	for {
		e := exprEnd(toks, i)
		if e < 0 {
			return exprs, i, false
		}
		exprs = append(exprs, [2]int{i, e})
		if !tokIs(toks, e, 'p', ",") {
			return exprs, e, true
		}
		i = e + 1
	}
}

// exprEnd returns the index just past the expression that starts at toks[i], or -1 when none
// does: operands (after any not, - or #) joined by binary operators, an operand being a
// literal, '...', a table constructor, a function, or a name or parenthesized expression with
// its fields, indexes and calls.
func exprEnd(toks []luaTok, i int) int {
	for {
		for i < len(toks) && isUnary(toks[i]) {
			i++
		}
		if i >= len(toks) {
			return -1
		}
		t := toks[i]
		switch {
		case t.kind == 'p' && t.text == "(":
			i = suffixesEnd(toks, matchClose(toks, i)+1)
		case t.kind == 'p' && t.text == "{":
			if i = matchClose(toks, i) + 1; i == 0 {
				return -1
			}
		case t.kind == 'n' && t.text == "function":
			if i = funcEnd(toks, i) + 1; i == 0 {
				return -1
			}
		case t.kind == 'n' && (t.text == "nil" || t.text == "true" || t.text == "false"):
			i++
		case t.kind == 'n' && !luaKeyword(t.text):
			i = suffixesEnd(toks, i+1)
		case t.kind == 's', t.kind == 'p' && (t.text == "..." || isDigit(t.text[0]) || (len(t.text) > 1 && t.text[0] == '.' && isDigit(t.text[1]))):
			i++
		default:
			return -1
		}
		if i <= 0 {
			return -1
		}
		n := binaryOp(toks, i)
		if n == 0 {
			return i
		}
		i += n
	}
}

// suffixesEnd returns the index past the fields, indexes and calls that follow a name or a
// parenthesized expression ending just before toks[i]; -1 when one cannot be read (i == 0 is
// a parenthesis that never closed).
func suffixesEnd(toks []luaTok, i int) int {
	if i <= 0 {
		return -1
	}
	for i < len(toks) {
		t := toks[i]
		switch {
		case t.kind == 'p' && t.text == "." && tokIs(toks, i+1, 'n', ""):
			i += 2
		case t.kind == 'p' && t.text == ":" && tokIs(toks, i+1, 'n', ""):
			i += 2
			switch {
			case tokIs(toks, i, 's', ""):
				i++
			case tokIs(toks, i, 'p', "(") || tokIs(toks, i, 'p', "{"):
				c := matchClose(toks, i)
				if c < 0 {
					return -1
				}
				i = c + 1
			default:
				return -1
			}
		case t.kind == 'p' && (t.text == "[" || t.text == "(" || t.text == "{"):
			c := matchClose(toks, i)
			if c < 0 {
				return -1
			}
			i = c + 1
		case t.kind == 's':
			i++
		default:
			return i
		}
	}
	return i
}

// binaryOp returns the number of tokens of the binary operator at toks[i] (== ~= <= >= are
// two), or 0.
func binaryOp(toks []luaTok, i int) int {
	if i >= len(toks) {
		return 0
	}
	t := toks[i]
	switch {
	case t.kind == 'n' && (t.text == "and" || t.text == "or"):
		return 1
	case t.kind != 'p':
		return 0
	}
	switch t.text {
	case "+", "-", "*", "/", "%", "^", "..":
		return 1
	case "=", "~":
		if tokIs(toks, i+1, 'p', "=") {
			return 2
		}
	case "<", ">":
		if tokIs(toks, i+1, 'p', "=") {
			return 2
		}
		return 1
	}
	return 0
}

// isUnary reports whether t is a unary operator: not, - or #.
func isUnary(t luaTok) bool {
	return (t.kind == 'n' && t.text == "not") || (t.kind == 'p' && (t.text == "-" || t.text == "#"))
}

// isOperatorChar reports whether a punctuation token is (part of) an arithmetic, comparison
// or concatenation operator.
func isOperatorChar(text string) bool {
	switch text {
	case "+", "-", "*", "/", "%", "^", "..", "=", "~", "<", ">", "#":
		return true
	}
	return false
}

// matchClose returns the index of the bracket that closes the one at toks[i], or -1 when it
// never closes or another bracket closes first.
func matchClose(toks []luaTok, i int) int {
	if !tokIs(toks, i, 'p', "(") && !tokIs(toks, i, 'p', "[") && !tokIs(toks, i, 'p', "{") {
		return -1
	}
	var want []string
	for k := i; k < len(toks); k++ {
		t := toks[k]
		if t.kind != 'p' {
			continue
		}
		switch t.text {
		case "(":
			want = append(want, ")")
		case "[":
			want = append(want, "]")
		case "{":
			want = append(want, "}")
		case ")", "]", "}":
			if len(want) == 0 || want[len(want)-1] != t.text {
				return -1
			}
			want = want[:len(want)-1]
			if len(want) == 0 {
				return k
			}
		}
	}
	return -1
}

// matchOpen returns the index of the bracket that opens the one closing at toks[i], or -1.
func matchOpen(toks []luaTok, i int) int {
	if !tokIs(toks, i, 'p', ")") && !tokIs(toks, i, 'p', "]") && !tokIs(toks, i, 'p', "}") {
		return -1
	}
	var want []string
	for k := i; k >= 0; k-- {
		t := toks[k]
		if t.kind != 'p' {
			continue
		}
		switch t.text {
		case ")":
			want = append(want, "(")
		case "]":
			want = append(want, "[")
		case "}":
			want = append(want, "{")
		case "(", "[", "{":
			if len(want) == 0 || want[len(want)-1] != t.text {
				return -1
			}
			want = want[:len(want)-1]
			if len(want) == 0 {
				return k
			}
		}
	}
	return -1
}

// funcEnd returns the index of the end that closes the function whose keyword is toks[i], or
// -1.
func funcEnd(toks []luaTok, i int) int {
	depth := 0
	for k := i; k < len(toks); k++ {
		if toks[k].kind != 'n' {
			continue
		}
		switch toks[k].text {
		case "function", "if", "do", "repeat":
			depth++
		case "end", "until":
			if depth--; depth == 0 {
				return k
			}
		}
	}
	return -1
}

// assignTargets returns the targets of the assignment whose '=' is toks[eq], each as the
// [first, last] of its tokens: a name, maybe followed by fields, indexes and calls ending in a
// field or index. nil when they cannot be read.
func assignTargets(toks []luaTok, eq int) [][2]int {
	var targets [][2]int
	j := eq - 1
	for {
		last := j
		for {
			if j < 0 {
				return nil
			}
			t := toks[j]
			if t.kind == 'n' && !luaKeyword(t.text) {
				if j > 0 && toks[j-1].kind == 'p' && (toks[j-1].text == "." || toks[j-1].text == ":") {
					j -= 2
					continue
				}
				break // the target's first name
			}
			if t.kind != 'p' || (t.text != "]" && t.text != ")") {
				return nil
			}
			o := matchOpen(toks, j)
			if o <= 0 {
				return nil
			}
			if p := toks[o-1]; (p.kind == 'n' && !luaKeyword(p.text)) || (p.kind == 'p' && (p.text == "]" || p.text == ")")) {
				j = o - 1 // an index or a call's arguments: the target goes on before it
				continue
			}
			if t.text == "]" {
				return nil
			}
			j = o // a parenthesized expression starts the target
			break
		}
		targets = append([][2]int{{j, last}}, targets...)
		if j < 2 || !tokIs(toks, j-1, 'p', ",") {
			return targets
		}
		j -= 2
	}
}

// nameList reads the names, separated by ',', that start at toks[j]; it returns them and the
// index after the list.
func nameList(toks []luaTok, j int) ([]string, int) {
	var names []string
	for tokIs(toks, j, 'n', "") && !luaKeyword(toks[j].text) {
		names = append(names, toks[j].text)
		if !tokIs(toks, j+1, 'p', ",") {
			return names, j + 1
		}
		j += 2
	}
	return names, j
}

// paramList returns the parameters of the function whose keyword is toks[i] (self first for
// a:b) and the index of the ')' that closes them.
func paramList(toks []luaTok, i int) ([]string, int) {
	var params []string
	j := i + 1
	for ; j < len(toks) && !tokIs(toks, j, 'p', "("); j++ {
		if tokIs(toks, j, 'p', ":") {
			params = append(params, "self")
		}
	}
	for j++; j < len(toks) && !tokIs(toks, j, 'p', ")"); j++ {
		if toks[j].kind == 'n' {
			params = append(params, toks[j].text)
		}
	}
	return params, j
}

// soleEq reports whether toks[i] is an '=' of its own: no part of ==, ~=, <= or >=.
func soleEq(toks []luaTok, i int) bool {
	if !tokIs(toks, i, 'p', "=") || tokIs(toks, i+1, 'p', "=") {
		return false
	}
	return i == 0 || toks[i-1].kind != 'p' || (toks[i-1].text != "=" && toks[i-1].text != "~" && toks[i-1].text != "<" && toks[i-1].text != ">")
}

// tokIs reports whether toks[i] exists, is of kind and, unless text is "", is text.
func tokIs(toks []luaTok, i int, kind byte, text string) bool {
	return i >= 0 && i < len(toks) && toks[i].kind == kind && (text == "" || toks[i].text == text)
}
