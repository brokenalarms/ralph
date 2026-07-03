package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestExecuteMerge_SingleMethodOwnsAncestorCheck verifies executeMerge and
// executeMergeWithAdminOverride were collapsed into one *repo method: no
// executeMergeWithAdminOverride declaration remains, exactly one *repo
// executeMerge method exists, and it is the only place in git_merge.go that
// calls assertMergedAncestor. Prevents regression to the pre-collapse state
// where both methods duplicated the same executeMerge-plus-ancestor-check
// tail.
func TestExecuteMerge_SingleMethodOwnsAncestorCheck(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	repoExecuteMergeDecls := 0
	adminOverrideDecls := 0
	ancestorCheckCalls := 0

	for _, pkg := range pkgs {
		for filename, f := range pkg.Files {
			if !strings.HasSuffix(filename, "git_merge.go") {
				continue
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fd.Name.Name == "executeMergeWithAdminOverride" {
					adminOverrideDecls++
				}
				isRepoExecuteMerge := fd.Name.Name == "executeMerge" && fd.Recv != nil
				if isRepoExecuteMerge {
					repoExecuteMergeDecls++
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "assertMergedAncestor" {
						return true
					}
					ancestorCheckCalls++
					if !isRepoExecuteMerge {
						t.Errorf("assertMergedAncestor called from %s — must be called only from the repo.executeMerge method", fd.Name.Name)
					}
					return true
				})
			}
		}
	}

	if adminOverrideDecls != 0 {
		t.Errorf("expected executeMergeWithAdminOverride to be removed, found %d declarations", adminOverrideDecls)
	}
	if repoExecuteMergeDecls != 1 {
		t.Errorf("expected exactly 1 repo.executeMerge method declaration, found %d", repoExecuteMergeDecls)
	}
	if ancestorCheckCalls != 1 {
		t.Errorf("expected assertMergedAncestor to be called exactly once in git_merge.go, found %d", ancestorCheckCalls)
	}
}
