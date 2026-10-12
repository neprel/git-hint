package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// GoDecl is one top-level Go declaration.
type GoDecl struct {
	Kind       string   `json:"kind"` // function, method, class, interface, variable
	Name       string   `json:"name"`
	Recv       string   `json:"recv,omitempty"`
	Line       int      `json:"line"`
	Lines      int      `json:"lines"`
	Params     int      `json:"params"`
	Cyclomatic int      `json:"cyclomatic"`
	Flags      []string `json:"flags,omitempty"`
	Hash       string   `json:"hash"`
}

// GoInfo is the analysis of one Go file.
type GoInfo struct {
	Package    string   `json:"package"`
	Imports    []string `json:"imports"`
	Decls      []GoDecl `json:"decls"`
	Generated  bool     `json:"generated"`
	ParseError bool     `json:"parse_error"`
}

// parseGo parses one Go file. A parse error keeps the file entity and drops the
// declarations (lower coverage, no failure).
func parseGo(name string, src []byte) *GoInfo {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	info := &GoInfo{}
	if f == nil {
		info.ParseError = true
		return info
	}
	if err != nil {
		info.ParseError = true
		return info
	}
	info.Package = f.Name.Name
	info.Generated = ast.IsGenerated(f)
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil {
			info.Imports = append(info.Imports, p)
		}
	}
	body := func(n ast.Node) (int, int, string) {
		start, end := fset.Position(n.Pos()), fset.Position(n.End())
		h := sha256.Sum256(src[start.Offset:end.Offset])
		return start.Line, end.Line - start.Line + 1, hex.EncodeToString(h[:])
	}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.FuncDecl:
			gd := GoDecl{Kind: "function", Name: x.Name.Name}
			if x.Recv != nil && len(x.Recv.List) > 0 {
				gd.Kind, gd.Recv = "method", recvName(x.Recv.List[0].Type)
			}
			gd.Line, gd.Lines, gd.Hash = body(x)
			gd.Params = fieldCount(x.Type.Params)
			gd.Cyclomatic = cyclomatic(x)
			if ast.IsExported(x.Name.Name) && info.Package != "main" && (gd.Recv == "" || ast.IsExported(gd.Recv)) {
				gd.Flags = append(gd.Flags, "public_api")
			}
			if info.Package == "main" && x.Name.Name == "main" && x.Recv == nil {
				gd.Flags = append(gd.Flags, "entrypoint")
			}
			if strings.HasSuffix(name, "_test.go") {
				gd.Flags = append(gd.Flags, "test")
			}
			info.Decls = append(info.Decls, gd)
		case *ast.GenDecl:
			for _, s := range x.Specs {
				ts, ok := s.(*ast.TypeSpec)
				if !ok {
					continue
				}
				gd := GoDecl{Kind: "class", Name: ts.Name.Name}
				if _, ok := ts.Type.(*ast.InterfaceType); ok {
					gd.Kind = "interface"
				}
				gd.Line, gd.Lines, gd.Hash = body(ts)
				if ast.IsExported(ts.Name.Name) && info.Package != "main" {
					gd.Flags = append(gd.Flags, "public_api")
				}
				info.Decls = append(info.Decls, gd)
			}
		}
	}
	return info
}

func recvName(t ast.Expr) string {
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return "_"
		}
	}
}

func fieldCount(fl *ast.FieldList) int {
	if fl == nil {
		return 0
	}
	n := 0
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			n++
		} else {
			n += len(f.Names)
		}
	}
	return n
}

// cyclomatic is 1 + the number of decision points (McCabe, syntax only).
func cyclomatic(fn *ast.FuncDecl) int {
	n := 1
	if fn.Body == nil {
		return n
	}
	ast.Inspect(fn.Body, func(x ast.Node) bool {
		switch y := x.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			n++
		case *ast.CaseClause:
			if y.List != nil {
				n++
			}
		case *ast.CommClause:
			if y.Comm != nil {
				n++
			}
		case *ast.BinaryExpr:
			if y.Op == token.LAND || y.Op == token.LOR {
				n++
			}
		case *ast.FuncLit:
			return true
		}
		return true
	})
	return n
}
