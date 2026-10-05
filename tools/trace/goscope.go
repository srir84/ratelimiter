package main

import (
	"go/ast"
	"go/token"
	"path"
	"strconv"
	"strings"
)

// scopeWalker walks one Go file with its own scope tracking. It is syntax only: type
// checking breaks on tagged and stub files that declare the same names, and staticcheck
// rejects the parser's deprecated object resolution.
type scopeWalker struct {
	pkgVars   map[string]bool // package-level variables of the file's package
	pkgOther  map[string]bool // its package-level constants, types and functions
	pkgTypes  map[string]bool // its package-level types (also in pkgOther)
	imps      imports         // the file's imports (useImports)
	dotImport bool            // the file dot-imports a package
	scopes    []map[string]bool

	// onWrite gets each write to a package-level variable (of the file's package or an
	// imported one): an assignment to it or to a field, element or pointee of it (also
	// through a conversion that keeps its storage), ++/--, &v, a send on it,
	// delete/clear/copy/close on it, passing it as the argument that a standard-library
	// function changes in place (sort.Strings, slices.Sort, slices.Delete, maps.Copy's
	// destination, …), the function also in parentheses or given type arguments (callee),
	// or a call x.m(…) or method value x.m of a pointer-receiver method (ptrMethod).
	onWrite func(at ast.Node, name string)
	// onSelect gets each selector X.Sel whose X is an identifier no local declares.
	onSelect func(sel *ast.SelectorExpr, x *ast.Ident)
	// ptrMethod, when set, reports whether m is a pointer-receiver method of the type of
	// the package-level variable v of the package at import path imp ("" for the file's
	// own package): x.m(…) and the method value x.m then take &x (takesAddress).
	ptrMethod func(imp, v, m string) bool
}

// useImports records the file's imports, which name the standard-library functions and
// types the walker knows.
func (w *scopeWalker) useImports(f *ast.File) {
	w.imps = fileImports(f)
	w.dotImport = len(w.imps.dot) > 0
}

func (w *scopeWalker) push() { w.scopes = append(w.scopes, map[string]bool{}) }

func (w *scopeWalker) pop() { w.scopes = w.scopes[:len(w.scopes)-1] }

func (w *scopeWalker) declare(name string) {
	if name != "_" && len(w.scopes) > 0 {
		w.scopes[len(w.scopes)-1][name] = true
	}
}

func (w *scopeWalker) isLocal(name string) bool {
	for i := len(w.scopes) - 1; i >= 0; i-- {
		if w.scopes[i][name] {
			return true
		}
	}
	return false
}

func (w *scopeWalker) fields(fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		for _, n := range f.Names {
			w.declare(n.Name)
		}
	}
}

// file walks every declaration. Package-level declarations are not writes; their
// initializers are walked, since a function literal there may write.
func (w *scopeWalker) file(f *ast.File) {
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			w.push()
			w.fields(d.Recv)
			w.fields(d.Type.TypeParams)
			w.fields(d.Type.Params)
			w.fields(d.Type.Results)
			if d.Body != nil {
				w.stmts(d.Body.List)
			}
			w.pop()
		case *ast.GenDecl:
			w.genDecl(d, false)
		}
	}
}

func (w *scopeWalker) genDecl(d *ast.GenDecl, local bool) {
	for _, s := range d.Specs {
		switch s := s.(type) {
		case *ast.ValueSpec:
			for _, v := range s.Values {
				w.expr(v)
			}
			if local {
				for _, n := range s.Names {
					w.declare(n.Name)
				}
			}
		case *ast.TypeSpec:
			if local {
				w.declare(s.Name.Name)
			}
		}
	}
}

func (w *scopeWalker) stmts(list []ast.Stmt) {
	for _, s := range list {
		w.stmt(s)
	}
}

func (w *scopeWalker) stmt(s ast.Stmt) {
	switch s := s.(type) {
	case *ast.BlockStmt:
		w.push()
		w.stmts(s.List)
		w.pop()
	case *ast.ExprStmt:
		w.expr(s.X)
	case *ast.SendStmt:
		w.write(s.Chan)
		w.expr(s.Chan)
		w.expr(s.Value)
	case *ast.IncDecStmt:
		w.write(s.X)
		w.expr(s.X)
	case *ast.AssignStmt:
		for _, x := range s.Rhs {
			w.expr(x)
		}
		for _, x := range s.Lhs {
			if id, ok := x.(*ast.Ident); ok && s.Tok == token.DEFINE {
				w.declare(id.Name)
				continue
			}
			w.write(x)
			w.expr(x)
		}
	case *ast.GoStmt:
		w.expr(s.Call)
	case *ast.DeferStmt:
		w.expr(s.Call)
	case *ast.ReturnStmt:
		for _, x := range s.Results {
			w.expr(x)
		}
	case *ast.DeclStmt:
		if d, ok := s.Decl.(*ast.GenDecl); ok {
			w.genDecl(d, true)
		}
	case *ast.LabeledStmt:
		w.stmt(s.Stmt)
	case *ast.IfStmt:
		w.push()
		w.stmt(s.Init)
		w.expr(s.Cond)
		w.stmt(s.Body)
		w.stmt(s.Else)
		w.pop()
	case *ast.ForStmt:
		w.push()
		w.stmt(s.Init)
		w.expr(s.Cond)
		w.stmt(s.Post)
		w.stmt(s.Body)
		w.pop()
	case *ast.RangeStmt:
		w.expr(s.X)
		w.push()
		for _, x := range []ast.Expr{s.Key, s.Value} {
			if x == nil {
				continue
			}
			if id, ok := x.(*ast.Ident); ok && s.Tok == token.DEFINE {
				w.declare(id.Name)
				continue
			}
			w.write(x)
			w.expr(x)
		}
		w.stmt(s.Body)
		w.pop()
	case *ast.SwitchStmt:
		w.push()
		w.stmt(s.Init)
		w.expr(s.Tag)
		w.clauses(s.Body, "")
		w.pop()
	case *ast.TypeSwitchStmt:
		w.push()
		w.stmt(s.Init)
		bound := ""
		switch a := s.Assign.(type) {
		case *ast.AssignStmt: // v := x.(type)
			for _, x := range a.Rhs {
				w.expr(x)
			}
			if len(a.Lhs) == 1 {
				if id, ok := a.Lhs[0].(*ast.Ident); ok {
					bound = id.Name
				}
			}
		case *ast.ExprStmt:
			w.expr(a.X)
		}
		w.clauses(s.Body, bound)
		w.pop()
	case *ast.SelectStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CommClause); ok {
				w.push()
				w.stmt(cc.Comm)
				w.stmts(cc.Body)
				w.pop()
			}
		}
	}
}

// clauses walks the case clauses of a switch; bound is a type switch's variable.
func (w *scopeWalker) clauses(body *ast.BlockStmt, bound string) {
	for _, c := range body.List {
		cc, ok := c.(*ast.CaseClause)
		if !ok {
			continue
		}
		w.push()
		w.declare(bound)
		for _, x := range cc.List {
			w.expr(x)
		}
		w.stmts(cc.Body)
		w.pop()
	}
}

func (w *scopeWalker) expr(e ast.Expr) {
	switch e := e.(type) {
	case *ast.FuncLit:
		w.push()
		w.fields(e.Type.TypeParams)
		w.fields(e.Type.Params)
		w.fields(e.Type.Results)
		w.stmts(e.Body.List)
		w.pop()
	case *ast.CompositeLit:
		for _, x := range e.Elts {
			w.expr(x)
		}
	case *ast.KeyValueExpr:
		w.expr(e.Key)
		w.expr(e.Value)
	case *ast.ParenExpr:
		w.expr(e.X)
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok && !w.isLocal(x.Name) && w.onSelect != nil {
			w.onSelect(e, x)
		}
		if w.takesAddress(e) {
			w.write(e.X)
		}
		w.expr(e.X)
	case *ast.IndexExpr:
		w.expr(e.X)
		w.expr(e.Index)
	case *ast.IndexListExpr:
		w.expr(e.X)
		for _, x := range e.Indices {
			w.expr(x)
		}
	case *ast.SliceExpr:
		w.expr(e.X)
		w.expr(e.Low)
		w.expr(e.High)
		w.expr(e.Max)
	case *ast.TypeAssertExpr:
		w.expr(e.X)
	case *ast.CallExpr:
		w.expr(e.Fun)
		for _, x := range e.Args {
			w.expr(x)
		}
		if len(e.Args) == 0 {
			break
		}
		fun := callee(e.Fun)
		if id, ok := fun.(*ast.Ident); ok && w.isBuiltin(id.Name) {
			switch id.Name {
			case "delete", "clear", "copy", "close":
				w.write(e.Args[0])
			}
		}
		if _, _, ok := w.imps.ref(fun, w.declared, mutatesFirstArg); ok {
			w.write(w.unwrapReverse(e.Args[0]))
		}
	case *ast.StarExpr:
		w.expr(e.X)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			w.write(e.X)
		}
		w.expr(e.X)
	case *ast.BinaryExpr:
		w.expr(e.X)
		w.expr(e.Y)
	}
}

// takesAddress reports whether the selector x.m, called or taken as a method value,
// names a pointer-receiver method m of a package-level variable x (ptrMethod), so that it
// takes &x: x of the file's package, pkg.V of an imported package or V under a dot
// import, also written (x) or *x.
func (w *scopeWalker) takesAddress(e *ast.SelectorExpr) bool {
	if w.ptrMethod == nil {
		return false
	}
	x := ast.Unparen(e.X)
	if star, ok := x.(*ast.StarExpr); ok {
		x = ast.Unparen(star.X)
	}
	switch x := x.(type) {
	case *ast.Ident:
		switch {
		case w.isLocal(x.Name), w.pkgOther[x.Name]:
			return false
		case w.pkgVars[x.Name]:
			return w.ptrMethod("", x.Name, e.Sel.Name)
		case w.imps.named[x.Name] != "":
			return false // pkg.Name: a member of the package, not a method
		}
		for _, p := range w.imps.dot {
			if w.ptrMethod(p, x.Name, e.Sel.Name) {
				return true
			}
		}
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok || w.declared(pkg.Name) {
			return false
		}
		if p := w.imps.named[pkg.Name]; p != "" {
			return w.ptrMethod(p, x.Sel.Name, e.Sel.Name)
		}
	}
	return false
}

// callee strips parentheses and explicit type arguments from a call's function part, so
// (close)(ch), slices.Sort[[]int](s) and (maps.Copy[M, M])(dst, src) name the function
// they call. On a type expression it gives the type a generic instance instantiates.
func callee(fun ast.Expr) ast.Expr {
	for {
		switch f := fun.(type) {
		case *ast.ParenExpr:
			fun = f.X
		case *ast.IndexExpr:
			fun = f.X
		case *ast.IndexListExpr:
			fun = f.X
		default:
			return fun
		}
	}
}

// isBuiltin reports whether name still means the predeclared function here.
func (w *scopeWalker) isBuiltin(name string) bool {
	return !w.declared(name)
}

// declared reports whether a local or the file's package declares name here.
func (w *scopeWalker) declared(name string) bool {
	return w.isLocal(name) || w.pkgVars[name] || w.pkgOther[name]
}

// unwrapReverse strips sort.Reverse(x) calls from the argument of an in-place sort: they
// sort x itself.
func (w *scopeWalker) unwrapReverse(e ast.Expr) ast.Expr {
	for {
		c, ok := e.(*ast.CallExpr)
		if !ok || len(c.Args) != 1 {
			return e
		}
		if _, _, ok := w.imps.ref(callee(c.Fun), w.declared, func(p, n string) bool { return p == "sort" && n == "Reverse" }); !ok {
			return e
		}
		e = c.Args[0]
	}
}

// mutatesFirstArg reports whether the standard-library function pkg.name changes its
// first argument in place: it sorts or reverses that slice, deletes, compacts, replaces
// or inserts elements in its backing array (slices.Insert when it has spare capacity),
// or copies or inserts into that map or deletes from it.
func mutatesFirstArg(pkg, name string) bool {
	switch pkg {
	case "sort":
		switch name {
		case "Sort", "Stable", "Strings", "Ints", "Float64s", "Slice", "SliceStable":
			return true
		}
	case "slices":
		switch name {
		case "Sort", "SortFunc", "SortStableFunc", "Reverse",
			"Delete", "DeleteFunc", "Compact", "CompactFunc", "Replace", "Insert":
			return true
		}
	case "maps":
		return name == "Copy" || name == "DeleteFunc" || name == "Insert"
	}
	return false
}

// write reports e when its root is a package-level variable: of the file's package
// (an identifier no local declares), of an imported package (pkg.V), or reached through
// a dot import.
func (w *scopeWalker) write(e ast.Expr) {
	id, sel := w.rootIdent(e)
	if id == nil || id.Name == "_" || w.isLocal(id.Name) || w.onWrite == nil {
		return
	}
	switch {
	case w.pkgVars[id.Name]:
		w.onWrite(id, id.Name)
	case w.pkgOther[id.Name]:
		// Constants, types and functions cannot be written.
	case sel != nil:
		w.onWrite(sel, id.Name+"."+sel.Sel.Name)
	case w.dotImport:
		w.onWrite(id, id.Name)
	}
}

// rootIdent strips selectors, indexes, slice expressions, type assertions,
// dereferences, parentheses and conversions that keep their operand's storage from e and
// returns the identifier at its root, with the selector applied directly to it (if any):
// v[1:][0], v.(*T).f and ([]int)(v)[0] write v's elements.
func (w *scopeWalker) rootIdent(e ast.Expr) (*ast.Ident, *ast.SelectorExpr) {
	var sel *ast.SelectorExpr
	for {
		switch x := e.(type) {
		case *ast.CallExpr:
			arg, ok := w.conversionArg(x)
			if !ok {
				return nil, nil
			}
			sel, e = nil, arg
		case *ast.Ident:
			return x, sel
		case *ast.SelectorExpr:
			sel, e = x, x.X
		case *ast.IndexExpr:
			sel, e = nil, x.X
		case *ast.IndexListExpr:
			sel, e = nil, x.X
		case *ast.SliceExpr:
			sel, e = nil, x.X
		case *ast.TypeAssertExpr:
			sel, e = nil, x.X
		case *ast.StarExpr:
			sel, e = nil, x.X
		case *ast.ParenExpr:
			sel, e = nil, x.X
		default:
			return nil, nil
		}
	}
}

// conversionArg returns the operand of call when call is a conversion trace can tell
// from a function call by syntax alone, so the result shares the operand's storage: to a
// type expression (array or slice, map, chan, func, interface or struct type, or a
// parenthesised pointer type: a * in a call's function part only comes in parentheses),
// to the predeclared any (unless the package or a local declares the name), to a type
// the file's package declares (unless a local hides its name; a generic one with its
// type arguments too), or to one of sort's slice types (sort.IntSlice, sort.StringSlice,
// sort.Float64Slice).
func (w *scopeWalker) conversionArg(call *ast.CallExpr) (ast.Expr, bool) {
	if len(call.Args) != 1 || call.Ellipsis.IsValid() {
		return nil, false
	}
	switch f := callee(call.Fun).(type) {
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType, *ast.StarExpr:
		return call.Args[0], true
	case *ast.Ident:
		if f.Name == "any" && !w.declared("any") { // the same type as interface{}
			return call.Args[0], true
		}
		return call.Args[0], w.pkgTypes[f.Name] && !w.isLocal(f.Name)
	case *ast.SelectorExpr:
		_, _, ok := w.imps.ref(f, w.declared, func(p, n string) bool {
			return p == "sort" && (n == "IntSlice" || n == "StringSlice" || n == "Float64Slice")
		})
		return call.Args[0], ok
	}
	return nil, false
}

// imports are the packages one file imports.
type imports struct {
	named map[string]string // local name → import path ("_" imports left out)
	dot   []string          // the paths imported with "."
}

// fileImports reads a file's imports. An import without a name goes by importName.
func fileImports(f *ast.File) imports {
	imps := imports{named: map[string]string{}}
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			continue
		}
		switch {
		case s.Name == nil:
			imps.named[importName(p)] = p
		case s.Name.Name == ".":
			imps.dot = append(imps.dot, p)
		case s.Name.Name != "_":
			imps.named[s.Name.Name] = p
		}
	}
	return imps
}

// importName is the name an import without one goes by: its path's last element, or the
// element before a major-version suffix (math/rand/v2 is package rand). That is the
// package name of every standard-library package trace knows, and of an in-module
// package named after its directory, as Go code names them.
func importName(p string) string {
	base, dir := path.Base(p), path.Dir(p)
	if dir == "." || len(base) < 2 || base[0] != 'v' || base[1] < '1' || base[1] > '9' || base == "v1" {
		return base
	}
	for _, c := range base[2:] {
		if c < '0' || c > '9' {
			return base
		}
	}
	return path.Base(dir)
}

// ref reports whether e names a package-level object of an imported package that match
// accepts, and which: pkg.Name through the package's local name, or a bare Name under a
// dot import. declared reports whether the file's package or a local scope declares a
// name, which then hides the import.
func (imps imports) ref(e ast.Expr, declared func(string) bool, match func(pkg, name string) bool) (pkg, name string, ok bool) {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		x, isIdent := e.X.(*ast.Ident)
		if !isIdent || declared(x.Name) {
			return "", "", false
		}
		if p := imps.named[x.Name]; p != "" && match(p, e.Sel.Name) {
			return p, e.Sel.Name, true
		}
	case *ast.Ident:
		if declared(e.Name) {
			return "", "", false
		}
		for _, p := range imps.dot {
			if match(p, e.Name) {
				return p, e.Name, true
			}
		}
	}
	return "", "", false
}

// pkgNames are the package-level names declared in a package's files.
type pkgNames struct {
	vars       map[string]bool            // variables
	other      map[string]bool            // constants, types and functions
	types      map[string][]typeDecl      // types, each with the imports of the file declaring it
	varDecls   map[string][]varDecl       // variables, each declaration of one
	ptrMethods map[string]map[string]bool // type name → its pointer-receiver methods
}

// typeDecl is a package-level type: its type expression and its file's imports.
type typeDecl struct {
	expr ast.Expr
	imps imports
}

// varDecl is one declaration of a package-level variable: its declared type and its
// initial value (each nil when absent) and its file's imports.
type varDecl struct {
	typ, value ast.Expr
	imps       imports
}

// pkgScope returns the package-level names declared in a package's files, whatever
// their build tags.
func pkgScope(files []*ast.File) *pkgNames {
	p := &pkgNames{vars: map[string]bool{}, other: map[string]bool{}, types: map[string][]typeDecl{},
		varDecls: map[string][]varDecl{}, ptrMethods: map[string]map[string]bool{}}
	for _, f := range files {
		imps := fileImports(f)
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.ValueSpec:
						for i, n := range s.Names {
							if d.Tok != token.VAR {
								p.other[n.Name] = true
								continue
							}
							p.vars[n.Name] = true
							v := varDecl{typ: s.Type, imps: imps}
							if len(s.Values) == len(s.Names) {
								v.value = s.Values[i]
							}
							p.varDecls[n.Name] = append(p.varDecls[n.Name], v)
						}
					case *ast.TypeSpec:
						p.other[s.Name.Name] = true
						// A tagged file and its stub may both declare the name: each declaration counts.
						p.types[s.Name.Name] = append(p.types[s.Name.Name], typeDecl{expr: s.Type, imps: imps})
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil {
					p.other[d.Name.Name] = true
				} else if t, ok := ptrRecv(d.Recv); ok {
					if p.ptrMethods[t] == nil {
						p.ptrMethods[t] = map[string]bool{}
					}
					p.ptrMethods[t][d.Name.Name] = true
				}
			}
		}
	}
	return p
}

// ptrRecv returns the type name of a pointer receiver, *T or *T[…] of a generic type;
// false for a value receiver.
func ptrRecv(recv *ast.FieldList) (string, bool) {
	if len(recv.List) != 1 {
		return "", false
	}
	star, ok := ast.Unparen(recv.List[0].Type).(*ast.StarExpr)
	if !ok {
		return "", false
	}
	id, ok := callee(star.X).(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// ptrMethod reports whether m is a pointer-receiver method of the type that a
// declaration of the package-level variable v gives it: its declared type, or the type
// of its initial value T{…}, &T{…} or new(T), once at most one pointer is removed, when
// that type is declared in this package (T or T[…]) or, written q.T, in a package that
// pkgOf finds by import path (nil for any other).
func (p *pkgNames) ptrMethod(v, m string, pkgOf func(path string) []*pkgNames) bool {
	for _, d := range p.varDecls[v] {
		for _, t := range []ast.Expr{d.typ, p.valueType(d.value)} {
			if t == nil {
				continue
			}
			if star, ok := ast.Unparen(t).(*ast.StarExpr); ok {
				t = star.X
			}
			switch t := callee(t).(type) {
			case *ast.Ident:
				if p.ptrMethods[t.Name][m] {
					return true
				}
			case *ast.SelectorExpr:
				q, ok := t.X.(*ast.Ident)
				if !ok {
					continue
				}
				for _, o := range pkgOf(d.imps.named[q.Name]) {
					if o.ptrMethods[t.Sel.Name][m] {
						return true
					}
				}
			}
		}
	}
	return false
}

// valueType is the type of a package-level variable's initial value when its top level
// shows it: T for T{…}, *T for &T{…} and new(T); nil for any other value.
func (p *pkgNames) valueType(v ast.Expr) ast.Expr {
	switch v := ast.Unparen(v).(type) {
	case *ast.CompositeLit:
		return v.Type
	case *ast.UnaryExpr:
		if lit, ok := ast.Unparen(v.X).(*ast.CompositeLit); ok && v.Op == token.AND && lit.Type != nil {
			return &ast.StarExpr{X: lit.Type}
		}
	case *ast.CallExpr:
		if id, ok := callee(v.Fun).(*ast.Ident); ok && id.Name == "new" && len(v.Args) == 1 && !p.declared("new") {
			return &ast.StarExpr{X: v.Args[0]}
		}
	}
	return nil
}

// declared reports whether the package declares name at package level.
func (p *pkgNames) declared(name string) bool { return p.vars[name] || p.other[name] }

// typeNames is the set of the package's type names.
func (p *pkgNames) typeNames() map[string]bool {
	out := make(map[string]bool, len(p.types))
	for n := range p.types {
		out[n] = true
	}
	return out
}

// mutableStdType reports whether pkg.name is a standard-library type whose values exist
// to be mutated: every type of sync and sync/atomic, strings.Builder, bytes.Buffer, and
// math/rand/v2's generators (Rand, PCG, ChaCha8, Zipf), whose every draw advances their
// state.
func mutableStdType(pkg, name string) bool {
	switch pkg {
	case "sync":
		switch name {
		case "Cond", "Locker", "Map", "Mutex", "Once", "Pool", "RWMutex", "WaitGroup":
			return true
		}
	case "sync/atomic":
		switch name {
		case "Bool", "Int32", "Int64", "Pointer", "Uint32", "Uint64", "Uintptr", "Value":
			return true
		}
	case "strings":
		return name == "Builder"
	case "bytes":
		return name == "Buffer"
	case "math/rand/v2":
		switch name {
		case "Rand", "PCG", "ChaCha8", "Zipf":
			return true
		}
	}
	return false
}

// mutableStdResult is the mutable type a standard-library constructor returns a
// pointer to ("" for any other function): bytes.NewBuffer and bytes.NewBufferString
// return a bytes.Buffer, sync.NewCond a sync.Cond, and math/rand/v2's New, NewPCG,
// NewChaCha8 and NewZipf a Rand, PCG, ChaCha8 and Zipf.
func mutableStdResult(pkg, name string) string {
	switch {
	case pkg == "bytes" && (name == "NewBuffer" || name == "NewBufferString"):
		return "bytes.Buffer"
	case pkg == "sync" && name == "NewCond":
		return "sync.Cond"
	case pkg == "math/rand/v2":
		switch name {
		case "New":
			return "math/rand/v2.Rand"
		case "NewPCG", "NewChaCha8", "NewZipf":
			return "math/rand/v2." + strings.TrimPrefix(name, "New")
		}
	}
	return ""
}

// heldMutable returns what a package-level variable of type t holds itself that exists
// to be mutated, "" when nothing: what a value of type t holds (valueMutable), once at
// most one top-level pointer is removed (a *sync.Mutex variable holds its mutex). imps are
// the imports of t's file.
func (p *pkgNames) heldMutable(t ast.Expr, imps imports) string {
	if star, ok := ast.Unparen(t).(*ast.StarExpr); ok {
		t = star.X
	}
	return p.valueMutable(t, imps, map[string]bool{})
}

// valueMutable returns what a value of type t holds itself that exists to be mutated, ""
// when nothing: a channel, or a type mutableStdType lists (also as a generic type's
// instance, atomic.Pointer[T]), or a struct type with a value field, named or embedded,
// that holds one, or a fixed-length array of such values ([4]sync.Mutex, [...]T), which
// holds its elements by value. A type the file's package declares counts as its
// definition; seen stops a recursive one. It never follows a pointer, a slice or map
// element type, a type argument, or a type another package declares: the value does not
// hold those itself, so a read-only table whose elements point at a value that holds an
// atomic passes. Function and interface types hold no such value either.
func (p *pkgNames) valueMutable(t ast.Expr, imps imports, seen map[string]bool) string {
	switch t := t.(type) {
	case *ast.ParenExpr:
		return p.valueMutable(t.X, imps, seen)
	case *ast.ChanType:
		return "a channel"
	case *ast.ArrayType:
		if t.Len != nil { // an array, not a slice
			return p.valueMutable(t.Elt, imps, seen)
		}
	case *ast.SelectorExpr:
		if pkg, name, ok := imps.ref(t, p.declared, mutableStdType); ok {
			return pkg + "." + name
		}
	case *ast.Ident:
		if pkg, name, ok := imps.ref(t, p.declared, mutableStdType); ok { // under a dot import
			return pkg + "." + name
		}
		if seen[t.Name] {
			return ""
		}
		seen[t.Name] = true
		for _, d := range p.types[t.Name] {
			if what := p.valueMutable(d.expr, d.imps, seen); what != "" {
				return what + " (through type " + t.Name + ")"
			}
		}
	case *ast.IndexExpr:
		return p.valueMutable(t.X, imps, seen)
	case *ast.IndexListExpr:
		return p.valueMutable(t.X, imps, seen)
	case *ast.StructType:
		for _, f := range t.Fields.List {
			if what := p.valueMutable(f.Type, imps, seen); what != "" {
				return what
			}
		}
	}
	return ""
}

// mutableValue is heldMutable for a package-level variable's initial value, read at its
// top level only: T{…}, &T{…}, new(T) and make(T, …) hold what a value of type T holds
// (valueMutable), and a standard-library constructor of a mutable type holds that type
// (mutableStdResult). The elements of a composite literal are not read.
func (p *pkgNames) mutableValue(v ast.Expr, imps imports) string {
	switch v := ast.Unparen(v).(type) {
	case *ast.UnaryExpr:
		if lit, ok := ast.Unparen(v.X).(*ast.CompositeLit); ok && v.Op == token.AND && lit.Type != nil {
			return p.valueMutable(lit.Type, imps, map[string]bool{})
		}
	case *ast.CompositeLit:
		if v.Type != nil {
			return p.valueMutable(v.Type, imps, map[string]bool{})
		}
	case *ast.CallExpr:
		fun := callee(v.Fun)
		if id, ok := fun.(*ast.Ident); ok && (id.Name == "new" || id.Name == "make") && len(v.Args) > 0 && !p.declared(id.Name) {
			return p.valueMutable(v.Args[0], imps, map[string]bool{})
		}
		if pkg, name, ok := imps.ref(fun, p.declared, func(pkg, name string) bool { return mutableStdResult(pkg, name) != "" }); ok {
			return mutableStdResult(pkg, name)
		}
	}
	return ""
}

// mutableVars calls found for each package-level variable that the file declares and
// that holds itself a value existing to be mutated: by its declared type (heldMutable)
// or, when that holds none, by its initial value (mutableValue).
func (p *pkgNames) mutableVars(f *ast.File, found func(name *ast.Ident, what string)) {
	imps := fileImports(f)
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}
		for _, s := range g.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name == "_" {
					continue
				}
				what := ""
				if vs.Type != nil {
					what = p.heldMutable(vs.Type, imps)
				}
				if what == "" && len(vs.Values) == len(vs.Names) {
					what = p.mutableValue(vs.Values[i], imps)
				}
				if what != "" {
					found(n, what)
				}
			}
		}
	}
}
