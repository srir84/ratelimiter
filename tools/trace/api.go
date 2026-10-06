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
	"sort"
	"strconv"
	"strings"
)

// apiInterim lists the §A5.2 methods whose staged signature passes until a later slice
// completes them (plan S5 step 15): S5's Close() takes no options yet, and S6 step 7 empties
// the list. --complete ignores it.
func apiInterim() map[string]string {
	return map[string]string{"Limiter.Close": "func()"}
}

// apiDocs is what the docs allow (DESIGN §A5.2, §A5.11, §A5.12, §C7): the normalized words
// of the §A5.2 Go listing (code and comments), §A5.11, §A5.12 and every Build item of the
// plan; and the declarations of the §A5.2 listing.
type apiDocs struct {
	words   map[string]bool
	types   []string                     // every type the listing declares, in order
	structs map[string][]string          // struct name → its "Name Type" fields
	funcs   map[string]string            // function name → signature
	methods map[string]map[string]string // receiver type → method name → signature
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
//   - an item that does not exist yet passes, except under --complete (S9, D1).
//
// It passes while limiter/ does not exist, and fails closed: a package that does not load or
// type-check, or docs without the §A5.2 listing, fail it.
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
	d := &apiDocs{words: map[string]bool{}, structs: map[string][]string{}, funcs: map[string]string{}, methods: map[string]map[string]string{}}
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
	return d, nil
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
// declares. The listing is Go source; its functions have no bodies, which Go's parser
// accepts.
func (d *apiDocs) parseListing(src string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "DESIGN.md §A5.2", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parsing the DESIGN §A5.2 listing: %w", err)
	}
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
