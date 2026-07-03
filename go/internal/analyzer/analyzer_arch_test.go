package analyzer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDeadToolCallRepeatCodeRemoved verifies the dead tool-call-repeat
// counting cluster (maxToolCallRepeats, firstN, extractToolTarget, and the
// parsedLog.ToolCalls field) was removed after tool-call-repeat detection
// was disabled in Analyze. Prevents regression back to defining these
// symbols without any caller.
func TestDeadToolCallRepeatCodeRemoved(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	removedFuncs := map[string]int{
		"maxToolCallRepeats": 0,
		"firstN":             0,
		"extractToolTarget":  0,
	}

	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok {
					if _, tracked := removedFuncs[fd.Name.Name]; tracked {
						removedFuncs[fd.Name.Name]++
					}
				}
				if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
					for _, spec := range gd.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok || ts.Name.Name != "parsedLog" {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok {
							continue
						}
						for _, field := range st.Fields.List {
							for _, name := range field.Names {
								if name.Name == "ToolCalls" {
									t.Errorf("parsedLog.ToolCalls field must be removed, still present")
								}
							}
						}
					}
				}
			}
		}
	}

	for name, count := range removedFuncs {
		if count != 0 {
			t.Errorf("%s must be removed, found %d declaration(s)", name, count)
		}
	}
}
