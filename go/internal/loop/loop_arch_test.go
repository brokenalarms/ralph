package loop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestOrchestratorParamsNoModules enforces that every *Params/*Opts struct
// in the loop package carries only data — no func types (callbacks are
// module references in disguise) and no interface types (interfaces are
// usually module abstractions).
//
// The orchestrator/module-boundary refactor that introduced this test landed
// bead-by-bead with an explicit allowlist. The refactor is now complete,
// so the test walks every *Params/*Opts struct in the loop package
// unconditionally. Adding a new params struct that holds a callback or
// interface should fail this test immediately.
func TestOrchestratorParamsNoModules(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read dir: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}

		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structName := ts.Name.Name
				if !isParamsOrOpts(structName) {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					if isFuncType(field.Type) {
						for _, name := range field.Names {
							t.Errorf("%s.%s is a func type — params/opts structs must carry only data, not callbacks",
								structName, name.Name)
						}
					}
					if isInterfaceType(field.Type) {
						for _, name := range field.Names {
							t.Errorf("%s.%s is an interface — params/opts structs must carry only data, not module references",
								structName, name.Name)
						}
					}
				}
			}
		}
	}
}

// isParamsOrOpts returns true for struct names ending in Params or Opts.
func isParamsOrOpts(name string) bool {
	return strings.HasSuffix(name, "Params") || strings.HasSuffix(name, "Opts")
}

func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	case *ast.FuncType:
		return "func"
	case *ast.InterfaceType:
		return "interface"
	case *ast.MapType:
		return "map"
	default:
		return ""
	}
}

func isFuncType(expr ast.Expr) bool {
	_, ok := expr.(*ast.FuncType)
	return ok
}

func isInterfaceType(expr ast.Expr) bool {
	_, ok := expr.(*ast.InterfaceType)
	return ok
}

// TestNoGitInFunctionArgs ensures git module references are never passed as
// function arguments anywhere in the loop package. The Loop holds git on its
// struct (constructor injection); helpers reach it via the receiver, not via
// a parameter. Catches the escape hatch where agents wrap git in a free
// function param to dodge the params-struct rule.
//
// Constructors (functions whose name starts with "New") are exempt — that's
// where dependency injection happens.
func TestNoGitInFunctionArgs(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)

	forbidden := map[string]bool{
		"git.Ops":     true,
		"git.Repo":    true,
		"git.Manager": true,
		"git.GitOps":  true,
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read dir: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}

		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Type.Params == nil {
				continue
			}
			if strings.HasPrefix(fd.Name.Name, "New") {
				continue
			}
			for _, field := range fd.Type.Params.List {
				ts := typeString(field.Type)
				bare := strings.TrimPrefix(ts, "*")
				if !forbidden[ts] && !forbidden[bare] {
					continue
				}
				names := []string{"_"}
				if len(field.Names) > 0 {
					names = nil
					for _, n := range field.Names {
						names = append(names, n.Name)
					}
				}
				for _, name := range names {
					t.Errorf("%s: %s parameter %q has type %s — git references must not be passed as function args; use l.git on a Loop method or pass pre-fetched data instead",
						e.Name(), fd.Name.Name, name, ts)
				}
			}
		}
	}
}
