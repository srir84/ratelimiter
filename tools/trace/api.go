package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// apiInterim lists the §A5.2 methods whose staged signature passes until a later slice
// completes them (plan S5 step 15). S5 staged Close() without options there, and S6 step 7
// emptied the list, so every listed method must now have its §A5.2 signature. --complete
// ignores it.
func apiInterim() map[string]string {
	return map[string]string{}
}

// apiDocs is what the docs allow (DESIGN §A5.2, §A5.11, §A5.12, §C7): the normalized words
// of the §A5.2 Go listing (code and comments), §A5.11, §A5.12 and every Build item of the
// plan; and the declarations of the §A5.2 listing.
type apiDocs struct {
	words    map[string]bool
	types    []string                     // every type the listing declares, in order
	structs  map[string][]string          // struct name → its "Name Type" fields
	funcs    map[string]string            // function name → signature
	methods  map[string]map[string]string // receiver type → method name → signature
	notes    map[string][]string          // "Struct.Field" → the exported names its comment gives (apiCommentNames)
	wrappers []string                     // the thin wrappers §A5.2 says package limiter holds
}

// apiWordRe matches a word of the docs: a Go identifier or a snake_case setting name.
var apiWordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// apiNorm normalizes a name for comparison: lower case, underscores removed
// (ListenerDrops matches listener_drops).
func apiNorm(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }

// checkAPI is `trace api` (DESIGN §A5.2, §A5.11, §A5.12, §C7; plan S5 step 15). It loads
// ratelimiter/limiter (untagged) and ratelimiter/limitertest (rltest) with go/types, the
// module's packages parsed under each build's tags and the standard library through the
// standard-library source importer, and resolves aliases to their core types:
//   - every exported name of either package, and every exported field and method of the
//     types they export, must be a word of the §A5.2 listing, §A5.11, §A5.12 or a plan Build
//     item;
//   - every struct and function the §A5.2 listing declares must match it exactly once it
//     exists: the (name, type) set of the exported fields, and the signature; a type the
//     listing declares methods on carries exactly those exported methods;
//   - an item that does not exist yet passes, except under --complete (S9, D1);
//   - under --complete, also (aliases, wrappers, constants): every module type that a listed
//     field or signature uses has an exported limiter alias of that very type; every thin
//     wrapper the §A5.2 text names exists as a limiter function; and every constant the
//     listing's comment on an enum-typed field names exists in limiter with that field's type
//     (Closed and Open, Store and Controlled, the 14 result kinds, the refund results and the
//     unresolved reasons).
//
// It passes while limiter/ does not exist, and fails closed: a package that does not load or
// type-check, or docs without the §A5.2 listing or its list of thin wrappers, fail it.
func checkAPI(ctx context.Context, e *env, args []string) (report, error) {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	complete := fs.Bool("complete", false, "also fail when a §A5.2 item is missing (end of the build)")
	if err := parseFlags(fs, e, args); err != nil {
		return report{}, err
	}
	if err := ctx.Err(); err != nil {
		return report{}, fmt.Errorf("api: %w", err)
	}
	if !isDir(filepath.Join(e.root, "limiter")) {
		return report{out: []string{"api: limiter/ does not exist yet, nothing to check"}}, nil
	}
	module, err := modulePath(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	docs, err := readAPIDocs(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	fset := token.NewFileSet()
	std, ok := importer.ForCompiler(fset, "source", nil).(types.ImporterFrom)
	if !ok {
		return report{}, errors.New("the source importer does not implement types.ImporterFrom")
	}
	c := &apiChecker{root: e.root, module: module, fset: fset, docs: docs, complete: *complete, seen: map[string]bool{}}

	lim, err := newAPILoader(ctx, e.root, module, nil, fset, std).load(module + "/limiter")
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return report{}, fmt.Errorf("api: %w", cerr)
		}
		c.problems = append(c.problems, fmt.Sprintf("limiter: does not load (untagged build): %v", err))
		return report{problems: c.problems}, nil
	}
	nLim := c.names(lim)
	missing := c.listing(lim)
	if c.complete {
		c.aliases(lim)
		c.wrappers(lim)
		c.constants(lim)
	}
	nTest := 0
	if isDir(filepath.Join(e.root, "limitertest")) {
		lt, err := newAPILoader(ctx, e.root, module, []string{"rltest"}, fset, std).load(module + "/limitertest")
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return report{}, fmt.Errorf("api: %w", cerr)
			}
			c.problems = append(c.problems, fmt.Sprintf("limitertest: does not load (rltest build): %v", err))
		} else {
			nTest = c.names(lt)
		}
	}
	if len(c.problems) > 0 {
		return report{problems: c.problems}, nil
	}
	line := fmt.Sprintf("api: limiter (%d exported names) and limitertest (%d) match DESIGN §A5.2, §A5.11, §A5.12 and the plan", nLim, nTest)
	if len(missing) > 0 {
		line += "; §A5.2 items not built yet: " + strings.Join(missing, ", ")
	}
	return report{out: []string{line}}, nil
}

// isDir reports whether p is a directory.
func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// readAPIDocs reads the allowed words and the §A5.2 declarations from DESIGN.md and
// IMPLEMENTATION_PLAN.md.
func readAPIDocs(ctx context.Context, root string) (*apiDocs, error) {
	design, err := readLines(ctx, root, "DESIGN.md")
	if err != nil {
		return nil, err
	}
	plan, err := readLines(ctx, root, "IMPLEMENTATION_PLAN.md")
	if err != nil {
		return nil, err
	}
	a52, ok := docSection(design, "### A5.2 ")
	if !ok {
		return nil, errors.New("DESIGN.md has no section ### A5.2")
	}
	listing, ok := goListing(a52)
	if !ok {
		return nil, errors.New("DESIGN.md §A5.2 has no ```go listing")
	}
	a511, ok511 := docSection(design, "### A5.11 ")
	a512, ok512 := docSection(design, "### A5.12 ")
	if !ok511 || !ok512 {
		return nil, errors.New("DESIGN.md lacks section ### A5.11 or ### A5.12")
	}
	build := buildItems(plan)
	if len(build) == 0 {
		return nil, errors.New("IMPLEMENTATION_PLAN.md has no - **Build:** item")
	}
	d := &apiDocs{words: map[string]bool{}, structs: map[string][]string{}, funcs: map[string]string{}, methods: map[string]map[string]string{},
		notes: map[string][]string{}}
	for _, part := range [][]string{listing, a511, a512, build} {
		for _, line := range part {
			for _, w := range apiWordRe.FindAllString(line, -1) {
				d.words[apiNorm(w)] = true
			}
		}
	}
	if err := d.parseListing(strings.Join(listing, "\n")); err != nil {
		return nil, err
	}
	m := apiWrappersRe.FindStringSubmatch(strings.Join(a52, " "))
	if m == nil {
		return nil, errors.New("DESIGN.md §A5.2 names no thin wrappers (…)")
	}
	d.wrappers = apiWordRe.FindAllString(m[1], -1)
	return d, nil
}

// apiWrappersRe finds the list of thin wrappers in the §A5.2 text: "thin wrappers (`New`, …)".
var apiWrappersRe = regexp.MustCompile(`thin wrappers \(([^)]*)\)`)

// apiCommentNames returns the exported Go identifiers a listing comment names outside
// parentheses, in order: "Closed (default) | Open" gives Closed and Open, and "Closed,
// Retention, Expired (Unresolved only)" gives Closed, Retention and Expired.
func apiCommentNames(comment string) []string {
	var b strings.Builder
	depth := 0
	for _, r := range comment {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	var out []string
	for _, w := range apiWordRe.FindAllString(b.String(), -1) {
		if token.IsExported(w) {
			out = append(out, w)
		}
	}
	return out
}

// docSection returns the lines of the section whose heading starts with prefix, up to the
// next heading of any level; lines inside code fences are never headings.
func docSection(lines []string, prefix string) ([]string, bool) {
	var out []string
	in, fence, found := false, false, false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(line, "#") {
			if in {
				break
			}
			if strings.HasPrefix(line, prefix) {
				in, found = true, true
				continue
			}
		}
		if in {
			out = append(out, line)
		}
	}
	return out, found
}

// goListing returns the lines of the first ```go fence in section.
func goListing(section []string) ([]string, bool) {
	var out []string
	in := false
	for _, line := range section {
		t := strings.TrimSpace(line)
		switch {
		case !in && t == "```go":
			in = true
		case in && strings.HasPrefix(t, "```"):
			return out, true
		case in:
			out = append(out, line)
		}
	}
	return nil, false
}

// buildItems returns the lines of every slice's Build section: from a line "- **Build:**" to
// the next top-level bullet ("- **…") or heading, outside code fences.
func buildItems(plan []string) []string {
	var out []string
	in, fence := false, false
	for _, line := range plan {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			fence = !fence
		}
		if !fence {
			switch {
			case strings.HasPrefix(line, "- **Build:**"):
				in = true
			case strings.HasPrefix(line, "- **"), strings.HasPrefix(line, "#"):
				in = false
			}
		}
		if in {
			out = append(out, line)
		}
	}
	return out
}

// parseListing records the types, struct fields, functions and methods the §A5.2 listing
// declares, and the names each struct field's comment gives (notes): the comment on the
// field's line and on the comment-only lines right after it (Result.Kind's runs over three).
// The listing is Go source; its functions have no bodies, which Go's parser accepts.
func (d *apiDocs) parseListing(src string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "DESIGN.md §A5.2", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parsing the DESIGN §A5.2 listing: %w", err)
	}
	lines := strings.Split(src, "\n")
	for _, decl := range f.Decls {
		switch dl := decl.(type) {
		case *ast.GenDecl:
			if dl.Tok != token.TYPE {
				continue
			}
			for _, spec := range dl.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				d.types = append(d.types, ts.Name.Name)
				if st, ok := ts.Type.(*ast.StructType); ok && !ts.Assign.IsValid() {
					d.structs[ts.Name.Name] = astFields(st.Fields)
					for _, fld := range st.Fields.List {
						names := apiCommentNames(fieldComment(lines, fset.Position(fld.Pos()).Line))
						if len(names) == 0 {
							continue
						}
						for _, n := range fld.Names {
							d.notes[ts.Name.Name+"."+n.Name] = names
						}
					}
				}
			}
		case *ast.FuncDecl:
			sig := astSig(dl.Type)
			if dl.Recv == nil || len(dl.Recv.List) == 0 {
				d.funcs[dl.Name.Name] = sig
				continue
			}
			recv := strings.TrimPrefix(astType(dl.Recv.List[0].Type), "*")
			if d.methods[recv] == nil {
				d.methods[recv] = map[string]string{}
			}
			d.methods[recv][dl.Name.Name] = sig
		}
	}
	if len(d.structs) == 0 || len(d.funcs) == 0 || len(d.methods) == 0 {
		return errors.New("the DESIGN §A5.2 listing declares no struct, function or method")
	}
	return nil
}

// fieldComment returns the comment of the field on line (1-based) of lines: the text after
// "//" on that line, then that of each comment-only line right after it.
func fieldComment(lines []string, line int) string {
	if line < 1 || line > len(lines) {
		return ""
	}
	_, text, ok := strings.Cut(lines[line-1], "//")
	if !ok {
		return ""
	}
	for _, next := range lines[line:] {
		rest, isComment := strings.CutPrefix(strings.TrimSpace(next), "//")
		if !isComment {
			break
		}
		text += " " + rest
	}
	return text
}

// astFields returns a struct's fields as "Name Type", one per name; an embedded field is
// named by its type.
func astFields(fl *ast.FieldList) []string {
	var out []string
	for _, f := range fl.List {
		t := astType(f.Type)
		if len(f.Names) == 0 {
			name := strings.TrimPrefix(t, "*")
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			out = append(out, name+" "+t)
		}
		for _, n := range f.Names {
			out = append(out, n.Name+" "+t)
		}
	}
	return out
}

// astTypes returns the types of a parameter or result list, one per name.
func astTypes(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, f := range fl.List {
		t := astType(f.Type)
		for range max(len(f.Names), 1) {
			out = append(out, t)
		}
	}
	return out
}

// astSig formats a function type without parameter names, as apiSig does.
func astSig(ft *ast.FuncType) string { return sigText(astTypes(ft.Params), astTypes(ft.Results)) }

// astType formats a type expression of the listing.
func astType(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return astType(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + astType(t.X)
	case *ast.ParenExpr:
		return astType(t.X)
	case *ast.Ellipsis:
		return "..." + astType(t.Elt)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + astType(t.Elt)
		}
		return "[" + astType(t.Len) + "]" + astType(t.Elt)
	case *ast.BasicLit:
		return t.Value
	case *ast.MapType:
		return "map[" + astType(t.Key) + "]" + astType(t.Value)
	case *ast.ChanType:
		return chanText(t.Dir == ast.SEND, t.Dir == ast.RECV) + astType(t.Value)
	case *ast.FuncType:
		return astSig(t)
	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return "interface{}"
		}
		return "interface{…}"
	case *ast.StructType:
		return "struct{" + strings.Join(astFields(t.Fields), "; ") + "}"
	}
	return fmt.Sprintf("%T", x)
}

// chanText is a channel type's prefix.
func chanText(send, recv bool) string {
	switch {
	case send && !recv:
		return "chan<- "
	case recv && !send:
		return "<-chan "
	}
	return "chan "
}

// sigText formats a signature from its parameter and result types: func(A, ...B) R or
// func(A) (R1, R2).
func sigText(params, results []string) string {
	s := "func(" + strings.Join(params, ", ") + ")"
	switch len(results) {
	case 0:
		return s
	case 1:
		return s + " " + results[0]
	}
	return s + " (" + strings.Join(results, ", ") + ")"
}

// apiChecker holds one run's state.
type apiChecker struct {
	root, module string
	fset         *token.FileSet
	docs         *apiDocs
	complete     bool
	seen         map[string]bool // problems reported, so a core object reached twice counts once
	problems     []string
}

func (c *apiChecker) problem(pos token.Pos, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if p := c.fset.Position(pos); p.IsValid() {
		file := p.Filename
		if rel, err := filepath.Rel(c.root, file); err == nil {
			file = filepath.ToSlash(rel)
		}
		msg = file + ":" + strconv.Itoa(p.Line) + ": " + msg
	}
	if !c.seen[msg] {
		c.seen[msg] = true
		c.problems = append(c.problems, msg)
	}
}

// names checks every exported name of pkg, and the exported fields and methods of the
// types it exports (resolved through aliases), against the docs' words. It returns how many
// names it checked.
func (c *apiChecker) names(pkg *types.Package) int {
	n := 0
	check := func(obj types.Object, label string) {
		n++
		if !c.docs.words[apiNorm(obj.Name())] {
			c.problem(obj.Pos(), "%s: exported name %s appears in none of the DESIGN §A5.2 listing, §A5.11, §A5.12 or a plan Build item", label, obj.Name())
		}
	}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		label := pkg.Name() + "." + name
		check(obj, label)
		named := namedOf(obj)
		if named == nil {
			continue
		}
		if iface, ok := named.Underlying().(*types.Interface); ok {
			for i := range iface.NumMethods() {
				if m := iface.Method(i); m.Exported() {
					check(m, label+"."+m.Name())
				}
			}
			continue
		}
		ms := types.NewMethodSet(types.NewPointer(named))
		for i := range ms.Len() {
			if m := ms.At(i).Obj(); m.Exported() {
				check(m, label+"."+m.Name())
			}
		}
		if st, ok := named.Underlying().(*types.Struct); ok {
			for i := range st.NumFields() {
				if f := st.Field(i); f.Exported() {
					check(f, label+"."+f.Name())
				}
			}
		}
	}
	return n
}

// namedOf returns the defined type behind a type name, through any aliases; nil for other
// objects and for aliases of unnamed types.
func namedOf(obj types.Object) *types.Named {
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	named, _ := types.Unalias(tn.Type()).(*types.Named)
	return named
}

// listing checks limiter against the §A5.2 declarations and returns the items that do not
// exist yet (each a problem under --complete).
func (c *apiChecker) listing(pkg *types.Package) []string {
	var missing []string
	miss := func(item string) {
		missing = append(missing, item)
		if c.complete {
			c.problems = append(c.problems, fmt.Sprintf("limiter: §A5.2 item %s is missing (--complete)", item))
		}
	}
	scope := pkg.Scope()
	for _, name := range c.docs.types {
		obj := scope.Lookup(name)
		if obj == nil {
			miss("type " + name)
			continue
		}
		if _, ok := obj.(*types.TypeName); !ok {
			c.problem(obj.Pos(), "limiter.%s: §A5.2 declares a type, this is a %s", name, objKind(obj))
			continue
		}
		want, isStruct := c.docs.structs[name]
		if !isStruct {
			continue
		}
		named := namedOf(obj)
		var st *types.Struct
		if named != nil {
			st, _ = named.Underlying().(*types.Struct)
		}
		if st == nil {
			c.problem(obj.Pos(), "limiter.%s: §A5.2 declares a struct, this is %s", name, c.typeText(types.Unalias(obj.Type()).Underlying()))
			continue
		}
		var got []string
		for i := range st.NumFields() {
			if f := st.Field(i); f.Exported() {
				got = append(got, f.Name()+" "+c.typeText(f.Type()))
			}
		}
		if extra, absent := diffSets(got, want); len(extra)+len(absent) > 0 {
			c.problem(named.Obj().Pos(), "limiter.%s: fields differ from the DESIGN §A5.2 listing: not listed [%s], listed but absent [%s]",
				name, strings.Join(extra, "; "), strings.Join(absent, "; "))
		}
	}
	for _, name := range sortedKeys(c.docs.funcs) {
		want := c.docs.funcs[name]
		obj := scope.Lookup(name)
		if obj == nil {
			miss("func " + name)
			continue
		}
		fn, ok := obj.(*types.Func)
		if !ok {
			c.problem(obj.Pos(), "limiter.%s: §A5.2 declares a function, this is a %s", name, objKind(obj))
			continue
		}
		if got := c.sigOf(fn); got != want {
			c.problem(obj.Pos(), "limiter.%s: signature %s, the DESIGN §A5.2 listing has %s", name, got, want)
		}
	}
	interim := apiInterim()
	for _, recv := range sortedKeys(c.docs.methods) {
		listed := c.docs.methods[recv]
		obj := scope.Lookup(recv)
		named := (*types.Named)(nil)
		if obj != nil {
			named = namedOf(obj)
		}
		if named == nil {
			for _, m := range sortedKeys(listed) {
				miss("method " + recv + "." + m)
			}
			continue
		}
		ms := types.NewMethodSet(types.NewPointer(named))
		have := map[string]bool{}
		for i := range ms.Len() {
			m, ok := ms.At(i).Obj().(*types.Func)
			if !ok || !m.Exported() {
				continue
			}
			have[m.Name()] = true
			want, ok := listed[m.Name()]
			if !ok {
				c.problem(m.Pos(), "limiter.%s.%s: the DESIGN §A5.2 listing declares no method %s on %s, so its method set differs", recv, m.Name(), m.Name(), recv)
				continue
			}
			got := c.sigOf(m)
			staged, isStaged := interim[recv+"."+m.Name()]
			if got != want && (c.complete || !isStaged || got != staged) {
				c.problem(m.Pos(), "limiter.%s.%s: signature %s, the DESIGN §A5.2 listing has %s", recv, m.Name(), got, want)
			}
		}
		for _, m := range sortedKeys(listed) {
			if !have[m] {
				miss("method " + recv + "." + m)
			}
		}
	}
	return missing
}

// aliases is --complete's first rule: every type of the module that the fields of a listed
// struct or the signature of a listed function or method uses (a method's receiver
// included) has an exported limiter alias of that very type (DESIGN §A5.2: package limiter
// re-exports them as type aliases). Items that do not exist are listing's to report.
func (c *apiChecker) aliases(pkg *types.Package) {
	scope := pkg.Scope()
	uses := map[*types.TypeName][]string{}
	for _, name := range sortedKeys(c.docs.structs) {
		if st := structOf(scope.Lookup(name)); st != nil {
			for i := range st.NumFields() {
				if f := st.Field(i); f.Exported() {
					c.typesIn(f.Type(), name+"."+f.Name(), uses)
				}
			}
		}
	}
	for _, name := range sortedKeys(c.docs.funcs) {
		if fn, ok := scope.Lookup(name).(*types.Func); ok {
			c.typesIn(fn.Type(), name, uses)
		}
	}
	for _, recv := range sortedKeys(c.docs.methods) {
		named := namedOf(scope.Lookup(recv))
		if named == nil {
			continue
		}
		ms := types.NewMethodSet(types.NewPointer(named))
		for _, m := range sortedKeys(c.docs.methods[recv]) {
			if sel := ms.Lookup(nil, m); sel != nil {
				c.typesIn(named, recv+"."+m, uses)
				c.typesIn(sel.Obj().Type(), recv+"."+m, uses)
			}
		}
	}
	objs := make([]*types.TypeName, 0, len(uses))
	for obj := range uses {
		objs = append(objs, obj)
	}
	sort.Slice(objs, func(i, j int) bool {
		if objs[i].Name() != objs[j].Name() {
			return objs[i].Name() < objs[j].Name()
		}
		return objs[i].Pkg().Path() < objs[j].Pkg().Path()
	})
	for _, obj := range objs {
		where := strings.Join(sortedSet(uses[obj]), ", ")
		got := scope.Lookup(obj.Name())
		if got == nil {
			c.problems = append(c.problems, fmt.Sprintf("limiter: §A5.2 type %s, used in %s, has no exported limiter alias (--complete)", obj.Name(), where))
			continue
		}
		if tn, ok := got.(*types.TypeName); !ok || !tn.IsAlias() || !types.Identical(types.Unalias(tn.Type()), obj.Type()) {
			c.problem(got.Pos(), "limiter.%s is not an alias of the %s used in %s (--complete)", obj.Name(), obj.Name(), where)
		}
	}
}

// typesIn records use for every named type of the module in t, through pointers, slices,
// arrays, maps, channels, signatures and struct literals; aliases resolve to their type.
func (c *apiChecker) typesIn(t types.Type, use string, uses map[*types.TypeName][]string) {
	switch t := t.(type) {
	case *types.Alias:
		c.typesIn(types.Unalias(t), use, uses)
	case *types.Named:
		if p := t.Obj().Pkg(); p != nil && (p.Path() == c.module || strings.HasPrefix(p.Path(), c.module+"/")) {
			uses[t.Obj()] = append(uses[t.Obj()], use)
		}
	case *types.Pointer:
		c.typesIn(t.Elem(), use, uses)
	case *types.Slice:
		c.typesIn(t.Elem(), use, uses)
	case *types.Array:
		c.typesIn(t.Elem(), use, uses)
	case *types.Map:
		c.typesIn(t.Key(), use, uses)
		c.typesIn(t.Elem(), use, uses)
	case *types.Chan:
		c.typesIn(t.Elem(), use, uses)
	case *types.Signature:
		for i := range t.Params().Len() {
			c.typesIn(t.Params().At(i).Type(), use, uses)
		}
		for i := range t.Results().Len() {
			c.typesIn(t.Results().At(i).Type(), use, uses)
		}
	case *types.Struct:
		for i := range t.NumFields() {
			c.typesIn(t.Field(i).Type(), use, uses)
		}
	}
}

// wrappers is --complete's second rule: every thin wrapper the §A5.2 text names is a limiter
// function. Those the listing declares are listing's to report.
func (c *apiChecker) wrappers(pkg *types.Package) {
	for _, name := range c.docs.wrappers {
		if _, listed := c.docs.funcs[name]; listed {
			continue
		}
		switch obj := pkg.Scope().Lookup(name); obj.(type) {
		case nil:
			c.problems = append(c.problems, fmt.Sprintf("limiter: §A5.2 wrapper %s is missing (--complete)", name))
		case *types.Func:
		default:
			c.problem(obj.Pos(), "limiter.%s: §A5.2 names a wrapper function, this is a %s (--complete)", name, objKind(obj))
		}
	}
}

// constants is --complete's third rule: for each field of a listed struct whose type is an
// enum of the module (a defined type over a basic type), every name the listing's comment on
// the field gives is an exported limiter constant of that type, named as given or after a
// leading part of the type's name (plan S5 step 8: UnresolvedReason's Closed is
// UnresolvedClosed, since Closed is FailureMode's).
func (c *apiChecker) constants(pkg *types.Package) {
	scope := pkg.Scope()
	for _, key := range sortedKeys(c.docs.notes) {
		structName, field, _ := strings.Cut(key, ".")
		st := structOf(scope.Lookup(structName))
		if st == nil {
			continue
		}
		var enum *types.Named
		for i := range st.NumFields() {
			if f := st.Field(i); f.Name() == field {
				enum, _ = types.Unalias(f.Type()).(*types.Named)
			}
		}
		if enum == nil || enum.Obj().Pkg() == nil || !strings.HasPrefix(enum.Obj().Pkg().Path()+"/", c.module+"/") {
			continue
		}
		if _, basic := enum.Underlying().(*types.Basic); !basic {
			continue
		}
		var consts []string
		for _, name := range scope.Names() {
			if k, ok := scope.Lookup(name).(*types.Const); ok && k.Exported() && types.Identical(types.Unalias(k.Type()), enum) {
				consts = append(consts, name)
			}
		}
		var absent []string
		for _, w := range c.docs.notes[key] {
			if !slices.ContainsFunc(consts, func(name string) bool { return constFor(name, w, enum.Obj().Name()) }) {
				absent = append(absent, w)
			}
		}
		if len(absent) > 0 {
			c.problems = append(c.problems, fmt.Sprintf("limiter: §A5.2 constants of %s named in the listing's comment on %s are missing: %s (--complete)",
				enum.Obj().Name(), key, strings.Join(absent, ", ")))
		}
	}
}

// constFor reports whether a constant named name stands for the listing's word w of the enum
// typeName: the word itself, or the word after a non-empty leading part of the type's name.
func constFor(name, w, typeName string) bool {
	if name == w {
		return true
	}
	prefix, ok := strings.CutSuffix(name, w)
	return ok && prefix != "" && strings.HasPrefix(typeName, prefix)
}

// structOf returns the struct behind a type name, through aliases; nil for anything else.
func structOf(obj types.Object) *types.Struct {
	named := namedOf(obj)
	if named == nil {
		return nil
	}
	st, _ := named.Underlying().(*types.Struct)
	return st
}

// sortedSet returns xs sorted, each value once.
func sortedSet(xs []string) []string {
	out := slices.Clone(xs)
	sort.Strings(out)
	return slices.Compact(out)
}

// objKind names an object's kind for messages.
func objKind(obj types.Object) string {
	switch obj.(type) {
	case *types.Const:
		return "constant"
	case *types.Var:
		return "variable"
	case *types.Func:
		return "function"
	case *types.TypeName:
		return "type"
	}
	return "declaration"
}

// sigOf formats a function's or method's signature without parameter names or receiver.
func (c *apiChecker) sigOf(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return fn.Type().String()
	}
	return c.sigText(sig)
}

func (c *apiChecker) sigText(sig *types.Signature) string {
	var params, results []string
	for i := range sig.Params().Len() {
		t := sig.Params().At(i).Type()
		if sl, ok := t.(*types.Slice); ok && sig.Variadic() && i == sig.Params().Len()-1 {
			params = append(params, "..."+c.typeText(sl.Elem()))
			continue
		}
		params = append(params, c.typeText(t))
	}
	for i := range sig.Results().Len() {
		results = append(results, c.typeText(sig.Results().At(i).Type()))
	}
	return sigText(params, results)
}

// typeText formats a type as the listing writes it: the module's named types by their bare
// name, other packages' with the package name (context.Context).
func (c *apiChecker) typeText(t types.Type) string {
	switch t := t.(type) {
	case *types.Basic:
		return t.Name()
	case *types.Alias:
		return c.objText(t.Obj())
	case *types.Named:
		return c.objText(t.Obj())
	case *types.Pointer:
		return "*" + c.typeText(t.Elem())
	case *types.Slice:
		return "[]" + c.typeText(t.Elem())
	case *types.Array:
		return "[" + strconv.FormatInt(t.Len(), 10) + "]" + c.typeText(t.Elem())
	case *types.Map:
		return "map[" + c.typeText(t.Key()) + "]" + c.typeText(t.Elem())
	case *types.Chan:
		return chanText(t.Dir() == types.SendOnly, t.Dir() == types.RecvOnly) + c.typeText(t.Elem())
	case *types.Signature:
		return c.sigText(t)
	case *types.Interface:
		if t.Empty() {
			return "interface{}"
		}
		return "interface{…}"
	case *types.Struct:
		var fields []string
		for i := range t.NumFields() {
			fields = append(fields, t.Field(i).Name()+" "+c.typeText(t.Field(i).Type()))
		}
		return "struct{" + strings.Join(fields, "; ") + "}"
	}
	return t.String()
}

func (c *apiChecker) objText(obj *types.TypeName) string {
	p := obj.Pkg()
	if p == nil || p.Path() == c.module || strings.HasPrefix(p.Path(), c.module+"/") {
		return obj.Name()
	}
	return p.Name() + "." + obj.Name()
}

// diffSets returns the entries of got not in want and of want not in got, each sorted;
// repeated entries count.
func diffSets(got, want []string) (extra, absent []string) {
	left := map[string]int{}
	for _, w := range want {
		left[w]++
	}
	for _, g := range got {
		if left[g] > 0 {
			left[g]--
			continue
		}
		extra = append(extra, g)
	}
	for w, n := range left {
		for range n {
			absent = append(absent, w)
		}
	}
	sort.Strings(extra)
	sort.Strings(absent)
	return extra, absent
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// apiLoader type-checks the module's packages from source under one build's tags, and
// takes every other package from the standard-library source importer.
type apiLoader struct {
	ctx          context.Context
	root, module string
	tags         []string
	fset         *token.FileSet
	std          types.ImporterFrom
	pkgs         map[string]*types.Package
	loading      map[string]bool
}

func newAPILoader(ctx context.Context, root, module string, tags []string, fset *token.FileSet, std types.ImporterFrom) *apiLoader {
	return &apiLoader{ctx: ctx, root: root, module: module, tags: tags, fset: fset, std: std,
		pkgs: map[string]*types.Package{}, loading: map[string]bool{}}
}

func (ld *apiLoader) Import(path string) (*types.Package, error) {
	return ld.ImportFrom(path, ld.root, 0)
}

func (ld *apiLoader) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path == ld.module || strings.HasPrefix(path, ld.module+"/") {
		return ld.load(path)
	}
	if err := ld.ctx.Err(); err != nil {
		return nil, fmt.Errorf("importing %s: %w", path, err)
	}
	return ld.std.ImportFrom(path, dir, mode)
}

// load type-checks one package of the module: the .go files its build (GOOS, GOARCH, the
// loader's tags, no cgo) selects, without test files and function bodies.
func (ld *apiLoader) load(path string) (*types.Package, error) {
	if err := ld.ctx.Err(); err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	if p, ok := ld.pkgs[path]; ok {
		return p, nil
	}
	if ld.loading[path] {
		return nil, fmt.Errorf("an import cycle through %s", path)
	}
	ld.loading[path] = true
	defer delete(ld.loading, path)
	dir := filepath.Join(ld.root, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(path, ld.module), "/")))
	bctx := build.Default
	bctx.BuildTags = ld.tags
	bctx.CgoEnabled = false
	bp, err := bctx.ImportDir(dir, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	files := make([]*ast.File, 0, len(bp.GoFiles))
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(ld.fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %w", err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: ld, IgnoreFuncBodies: true}
	pkg, err := conf.Check(path, ld.fset, files, nil)
	if err != nil {
		return nil, fmt.Errorf("type-checking %s: %w", path, err)
	}
	ld.pkgs[path] = pkg
	return pkg, nil
}
