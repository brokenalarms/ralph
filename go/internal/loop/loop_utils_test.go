package loop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestLoopUtils_NoExecCommand parses loop_utils.go and fails if it contains
// any exec.Command or exec.CommandContext call — the loop package must
// delegate all process execution through injected module interfaces
// (git.Ops, verify) rather than shelling out directly.
func TestLoopUtils_NoExecCommand(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "loop_utils.go", nil, 0)
	if err != nil {
		t.Fatalf("parse loop_utils.go: %v", err)
	}

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkgIdent.Name == "exec" && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext") {
			t.Errorf("loop_utils.go calls exec.%s — the loop package must not shell out directly", sel.Sel.Name)
		}
		return true
	})
}
