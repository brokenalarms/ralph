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

// TestAutoMergeCurrentBranch_DelegatesToStagedHelpers verifies
// AutoMergeCurrentBranch was decomposed into resolvePRForMerge, checkStacked,
// rebasePushAndComputeAwaitWindow, and gateOnCI — rather than inlining PR
// resolution, the stacked-branch guard, the rebase+push+await-window
// computation, and the CI gate in one body — and that gateOnCI folds its
// three CI-infrastructure-failure escape hatches into a single
// mergeAsInfrastructureFailure helper, so the admin-override merge call
// (executeMerge with admin=true) exists in exactly one place rather than
// being pasted at each escape hatch.
func TestAutoMergeCurrentBranch_DelegatesToStagedHelpers(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	var autoMergeBody, gateOnCIBody ast.Node
	mergeAsInfraDecls := 0
	adminTrueCallsOutsideHelper := 0

	for _, pkg := range pkgs {
		for filename, f := range pkg.Files {
			if !strings.HasSuffix(filename, "git_merge.go") {
				continue
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil {
					continue
				}
				switch fd.Name.Name {
				case "AutoMergeCurrentBranch":
					autoMergeBody = fd.Body
				case "gateOnCI":
					gateOnCIBody = fd.Body
				case "mergeAsInfrastructureFailure":
					mergeAsInfraDecls++
				}
				if fd.Name.Name == "mergeAsInfrastructureFailure" {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "executeMerge" || len(call.Args) != 4 {
						return true
					}
					if lit, ok := call.Args[3].(*ast.Ident); ok && lit.Name == "true" {
						adminTrueCallsOutsideHelper++
					}
					return true
				})
			}
		}
	}

	if autoMergeBody == nil {
		t.Fatal("AutoMergeCurrentBranch not found in git_merge.go")
	}
	if gateOnCIBody == nil {
		t.Fatal("gateOnCI not found in git_merge.go")
	}
	for _, name := range []string{"resolvePRForMerge", "checkStacked", "rebasePushAndComputeAwaitWindow", "gateOnCI"} {
		if got := countMethodCalls(autoMergeBody, name); got != 1 {
			t.Errorf("AutoMergeCurrentBranch calls %s %d times, want exactly 1", name, got)
		}
	}

	if mergeAsInfraDecls != 1 {
		t.Errorf("expected exactly 1 mergeAsInfrastructureFailure declaration, found %d", mergeAsInfraDecls)
	}
	if got := countMethodCalls(gateOnCIBody, "mergeAsInfrastructureFailure"); got != 3 {
		t.Errorf("gateOnCI calls mergeAsInfrastructureFailure %d times, want exactly 3 (one per CI-infrastructure-failure escape hatch)", got)
	}
	if adminTrueCallsOutsideHelper != 0 {
		t.Errorf("found %d executeMerge(..., true) call(s) outside mergeAsInfrastructureFailure — the admin-override merge path must exist in exactly one place", adminTrueCallsOutsideHelper)
	}
}

// TestShip_DelegatesReviewAndMergeClassification verifies Ship delegates
// reviewer polling to pollReviewers and merge-error demuxing to
// classifyMergeOutcome, and shares the stacked-branch guard (checkStacked)
// with AutoMergeCurrentBranch rather than duplicating the prBase-not-default
// check.
func TestShip_DelegatesReviewAndMergeClassification(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	var shipBody ast.Node
	for _, pkg := range pkgs {
		for filename, f := range pkg.Files {
			if !strings.HasSuffix(filename, "git_merge.go") {
				continue
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || fd.Name.Name != "Ship" {
					continue
				}
				shipBody = fd.Body
			}
		}
	}

	if shipBody == nil {
		t.Fatal("Ship not found in git_merge.go")
	}
	for _, name := range []string{"pollReviewers", "checkStacked", "classifyMergeOutcome"} {
		if got := countMethodCalls(shipBody, name); got != 1 {
			t.Errorf("Ship calls %s %d times, want exactly 1", name, got)
		}
	}
}

// countMethodCalls counts calls of the form r.<name>(...) within body.
func countMethodCalls(body ast.Node, name string) int {
	count := 0
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		count++
		return true
	})
	return count
}
