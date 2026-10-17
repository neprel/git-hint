// Package apicheck computes the exported API of Go packages, one line per declaration.
// The eval builder calls it to compute the expected API of a refactoring, and the
// generated verifier runs this same source (package line changed to main, see build) with
// `go run` inside the task container, so both sides produce identical lines.
package apicheck

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

// Lines returns the sorted API lines of package directories (relative to root).
func Lines(root string, dirs []string) ([]string, error) {
	var lines []string
	for _, dir := range dirs {
		l, err := API(root, dir)
		if err != nil {
			return nil, err
		}
		lines = append(lines, l...)
	}
	sort.Strings(lines)
	return lines, nil
}

// API returns the exported declarations of the non-test Go files in root/rel, labeled
// with rel.
func API(root, rel string) ([]string, error) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob(filepath.Join(root, rel, "*.go"))
	sort.Strings(files)
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s", filepath.Base(f))
		}
		pkg := filepath.ToSlash(filepath.Clean(rel)) + " " + af.Name.Name
		for _, d := range af.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				recv := ""
				if d.Recv != nil && len(d.Recv.List) > 0 {
					recv = "(" + typeString(fset, d.Recv.List[0].Type) + ") "
					if !receiverExported(d.Recv.List[0].Type) {
						continue
					}
				}
				out = append(out, pkg+" func "+recv+d.Name.Name+strings.TrimPrefix(typeString(fset, d.Type), "func"))
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					switch sp := sp.(type) {
					case *ast.TypeSpec:
						if sp.Name.IsExported() {
							out = append(out, pkg+" type "+sp.Name.Name+" "+typeString(fset, sp.Type))
						}
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							if n.IsExported() {
								kind := "var"
								if d.Tok == token.CONST {
									kind = "const"
								}
								t := ""
								if sp.Type != nil {
									t = " " + typeString(fset, sp.Type)
								}
								out = append(out, pkg+" "+kind+" "+n.Name+t)
							}
						}
					}
				}
			}
		}
	}
	return out, nil
}

func receiverExported(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.IsExported()
		default:
			return false
		}
	}
}

func typeString(fset *token.FileSet, e ast.Node) string {
	var b bytes.Buffer
	printer.Fprint(&b, fset, e)
	return strings.Join(strings.Fields(b.String()), " ")
}
