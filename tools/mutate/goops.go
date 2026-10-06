package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
)

// flipped is the §C9 flip of a comparison operator: < ↔ <=, > ↔ >=, == ↔ !=.
func flipped(tok token.Token) (string, bool) {
	switch tok {
	case token.LSS:
		return "<=", true
	case token.LEQ:
		return "<", true
	case token.GTR:
		return ">=", true
	case token.GEQ:
		return ">", true
	case token.EQL:
		return "!=", true
	case token.NEQ:
		return "==", true
	}
	return "", false
}

// goMutants applies the Go operators of DESIGN §C9 to one file: flip each comparison, negate
// each if condition, and make each return of a function whose last result is an error return
// nil there instead.
func goMutants(file, src string) ([]mutant, error) {
	var ms []mutant
	add := func(line int, desc string, edits ...edit) {
		ms = append(ms, mutant{file: file, line: line, desc: desc, content: []byte(applyEdits(src, edits))})
	}

	// Comparisons, from the tokens.
	sfset := token.NewFileSet()
	sfile := sfset.AddFile(file, -1, len(src))
	var sc scanner.Scanner
	var scanErr error
	sc.Init(sfile, []byte(src), func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = fmt.Errorf("%s: %s", pos, msg)
		}
	}, 0)
	for {
		pos, tok, _ := sc.Scan()
		if tok == token.EOF {
			break
		}
		if to, ok := flipped(tok); ok {
			off := sfile.Offset(pos)
			add(sfile.Line(pos), "flip "+tok.String()+" to "+to, edit{off, off + len(tok.String()), to})
		}
	}
	if scanErr != nil {
		return nil, scanErr
	}

	// if conditions and error returns, from the syntax tree.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	tf := fset.File(f.Pos())
	off := func(p token.Pos) int { return tf.Offset(p) }
	var walk func(ft *ast.FuncType, body *ast.BlockStmt)
	walk = func(ft *ast.FuncType, body *ast.BlockStmt) {
		n, errLast := errorResults(ft)
		ast.Inspect(body, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.FuncLit:
				walk(x.Type, x.Body)
				return false // its returns are its own
			case *ast.IfStmt:
				add(tf.Line(x.Cond.Pos()), "negate the if condition",
					edit{off(x.Cond.Pos()), off(x.Cond.Pos()), "!("}, edit{off(x.Cond.End()), off(x.Cond.End()), ")"})
			case *ast.ReturnStmt:
				if !errLast || len(x.Results) != n {
					return true
				}
				last := x.Results[n-1]
				if id, ok := last.(*ast.Ident); ok && id.Name == "nil" {
					return true
				}
				add(tf.Line(last.Pos()), "return nil instead of the error", edit{off(last.Pos()), off(last.End()), "nil"})
			}
			return true
		})
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			walk(fd.Type, fd.Body)
		}
	}
	return ms, nil
}

// errorResults returns how many results ft has and whether the last is of type error.
func errorResults(ft *ast.FuncType) (int, bool) {
	if ft.Results == nil || len(ft.Results.List) == 0 {
		return 0, false
	}
	n := 0
	for _, field := range ft.Results.List {
		n += max(len(field.Names), 1)
	}
	last := ft.Results.List[len(ft.Results.List)-1]
	id, ok := last.Type.(*ast.Ident)
	return n, ok && id.Name == "error"
}
