package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestRebaseInTempWorktree_SingleHelperOwnsLifecycle verifies the
// rebaseInTempWorktree helper is declared exactly once in the git package,
// and that both RebaseStack and RebaseBranchOntoRemote delegate to it via a
// rebaseSpec composite literal rather than duplicating the temp-worktree
// lifecycle (worktree add/remove, temp branch, fetch, rebase, force-push)
// inline. Prevents regression to the pre-unification state where both
// methods hand-rolled the same sequence.
func TestRebaseInTempWorktree_SingleHelperOwnsLifecycle(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	helperDecls := 0
	cleanupClosures := 0
	callers := map[string]bool{"RebaseStack": false, "RebaseBranchOntoRemote": false}
	specLiterals := map[string]bool{"RebaseStack": false, "RebaseBranchOntoRemote": false}

	for _, pkg := range pkgs {
		for filename, f := range pkg.Files {
			if !strings.HasSuffix(filename, "git_stack.go") {
				continue
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fd.Name.Name == "rebaseInTempWorktree" {
					helperDecls++
				}
				isCaller := false
				if _, want := callers[fd.Name.Name]; want {
					isCaller = true
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.CallExpr:
						if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "rebaseInTempWorktree" && isCaller {
							callers[fd.Name.Name] = true
							for _, arg := range node.Args {
								if cl, ok := arg.(*ast.CompositeLit); ok {
									if ident, ok := cl.Type.(*ast.Ident); ok && ident.Name == "rebaseSpec" {
										specLiterals[fd.Name.Name] = true
									}
								}
							}
						}
					case *ast.AssignStmt:
						for i, lhs := range node.Lhs {
							if ident, ok := lhs.(*ast.Ident); ok && ident.Name == "cleanup" {
								if i < len(node.Rhs) {
									if _, ok := node.Rhs[i].(*ast.FuncLit); ok {
										cleanupClosures++
									}
								}
							}
						}
					}
					return true
				})
			}
		}
	}

	if helperDecls != 1 {
		t.Errorf("expected exactly 1 declaration of rebaseInTempWorktree in git_stack.go, found %d", helperDecls)
	}
	for name, called := range callers {
		if !called {
			t.Errorf("%s does not call rebaseInTempWorktree — must delegate to the shared helper", name)
		}
	}
	for name, usesSpec := range specLiterals {
		if !usesSpec {
			t.Errorf("%s does not pass a rebaseSpec composite literal to rebaseInTempWorktree", name)
		}
	}
	if cleanupClosures != 1 {
		t.Errorf("expected the cleanup closure to be defined exactly once in git_stack.go, found %d", cleanupClosures)
	}
}
