package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestConfigIsDataOnly parses git.go and verifies that Config contains
// only data fields (strings, durations, bools) plus the Logger (Rule 5
// exception). Fails if an interface, func, or module type is added to
// Config — preventing regression to the old pattern where GitHub,
// StateStore, PrePusher, and Runner were injected via Config.
func TestConfigIsDataOnly(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "git.go", nil, 0)
	if err != nil {
		t.Fatalf("parse git.go: %v", err)
	}

	allowedTypes := map[string]bool{
		"string":        true,
		"bool":          true,
		"time.Duration": true,
		"Log":           true, // Rule 5 exception: logger
	}

	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Config" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatal("Config is not a struct")
			}
			for _, field := range st.Fields.List {
				typeName := typeStr(field.Type)
				if allowedTypes[typeName] {
					continue
				}
				names := fieldNames(field)
				t.Errorf("Config.%s has type %s — Config must be data-only (no interfaces, funcs, or module types)", names, typeName)
			}
		}
	}
}

// TestNoExportedRepoConstructors verifies that no exported function in the
// git package returns *Repo. New() returns Ops. Any exported function
// returning *Repo (like the deleted NewRepoForTesting) would leak the
// concrete type outside the package.
func TestNoExportedRepoConstructors(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse dir: %v", err)
	}
	for _, pkg := range pkgs {
		for filename, f := range pkg.Files {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || !fd.Name.IsExported() {
					continue
				}
				if fd.Type.Results == nil {
					continue
				}
				for _, result := range fd.Type.Results.List {
					tn := typeStr(result.Type)
					if tn == "*Repo" || tn == "Repo" {
						t.Errorf("%s: exported function %s returns %s — only Ops should escape the package", filename, fd.Name.Name, tn)
					}
				}
			}
		}
	}
}

func typeStr(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
		return t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeStr(t.X)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		return "func"
	case *ast.ArrayType:
		return "[]" + typeStr(t.Elt)
	case *ast.MapType:
		return "map"
	default:
		return "unknown"
	}
}

func fieldNames(field *ast.Field) string {
	if len(field.Names) == 0 {
		return "(embedded)"
	}
	names := ""
	for i, n := range field.Names {
		if i > 0 {
			names += ", "
		}
		names += n.Name
	}
	return names
}
