package loop

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

// modulePackages is the set of go/internal sub-packages governed by
// TestNoFuncFieldsInModuleStructs. Claude, logging, server, and other
// utility packages are out of scope because they intentionally use func
// fields for well-understood cross-cutting concerns.
var modulePackages = map[string]bool{
	"loop":     true,
	"git":      true,
	"verifier": true,
	"state":    true,
	"tasks":    true,
	"analyzer": true,
	"config":   true,
}

// TestNoFuncFieldsInModuleStructs proves that no new func-typed struct field
// is introduced in the seven core module packages. It walks every non-test
// .go file in go/internal/{loop,git,verifier,state,tasks,analyzer,config}
// (testutil sub-packages excluded) and fails on any struct field whose type
// is a func — either a literal ast.FuncType or a named func-type alias
// defined in the same package.
//
// Two allowlists govern known exceptions (see below). Adding any new
// func-typed field outside both allowlists immediately fails this test;
// removing a field from code without removing its allowlist entry also fails
// (stale entry check keeps the lists honest).
func TestNoFuncFieldsInModuleStructs(t *testing.T) {
	root := internalDir(t)

	// callbackDebt: known legacy violations that are scheduled for removal
	// in tracked remediation beads. MUST ONLY SHRINK — never add to this
	// list; remove each entry as the corresponding remediation bead lands.
	callbackDebt := map[string]bool{
		// loop/loop_verify.go — fixLoopSpec callbacks (ralph-upxl)
		"loop.fixLoopSpec.spawn":    true,
		"loop.fixLoopSpec.onPushed": true,

		// git/git_merge.go — shipInfra helper cluster (ralph-ff8z)
		"git.shipInfra.push":                 true,
		"git.shipInfra.hasUncommitted":        true,
		"git.shipInfra.commitAll":             true,
		"git.shipInfra.branchHasUnmergedWork": true,

		// git/git_merge.go — ExecuteMergeOpts (ralph-ff8z)
		"git.ExecuteMergeOpts.AwaitCI": true,

		// git/git_merge.go — MergeRetryOpts (ralph-ff8z)
		"git.MergeRetryOpts.OnCIFailure":     true,
		"git.MergeRetryOpts.OnConflict":      true,
		"git.MergeRetryOpts.SleepFunc":       true,
		"git.MergeRetryOpts.ResolveConflict": true,
		"git.MergeRetryOpts.AwaitCI":         true,
	}

	// permanentExceptions: func-typed fields that are intentional
	// architectural patterns, not scheduled for removal.
	//
	//   - config.FlagDef.Apply / Read: FlagDef is a data-driven dispatch
	//     table; the func fields carry per-flag apply/read behaviour as
	//     part of the record itself, not as injected callbacks. They are
	//     intrinsic to the flag-registry pattern.
	//
	//   - verifier.Verifier.newRunner: a RunnerFactory submodule injected
	//     at construction time via New(). The verifier package doc
	//     explicitly endorses this as a peer-module pattern equivalent to
	//     git's github relationship. See docs/specs/orchestrator-modules.md.
	//
	//   - tasks.BD.RunBD: an explicit override point for testing that
	//     predates the func-field rule; retained as a named exception
	//     because replacing it requires restructuring BD's runner selection
	//     logic without any other driver for that change today.
	permanentExceptions := map[string]bool{
		"config.FlagDef.Apply":        true,
		"config.FlagDef.Read":         true,
		"verifier.Verifier.newRunner": true,
		"tasks.BD.RunBD":              true,
	}

	// Merge both sets into the working allowlist for the checks below.
	allowlist := make(map[string]bool, len(callbackDebt)+len(permanentExceptions))
	for k := range callbackDebt {
		allowlist[k] = true
	}
	for k := range permanentExceptions {
		allowlist[k] = true
	}

	// Pass 1: collect named func-type aliases per package so we can detect
	// fields whose type is a named alias (e.g. RunnerFactory, CommandRunner).
	namedFuncTypes := make(map[string]map[string]bool) // pkg → set of type names
	walkGoFiles(t, root, func(path string, f *ast.File) {
		rel, _ := filepath.Rel(root, path)
		pkg := scopedPackage(rel)
		if pkg == "" {
			return
		}
		if namedFuncTypes[pkg] == nil {
			namedFuncTypes[pkg] = make(map[string]bool)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, isFunc := ts.Type.(*ast.FuncType); isFunc {
					namedFuncTypes[pkg][ts.Name.Name] = true
				}
			}
		}
	})

	// Pass 2: check every struct field in the scoped packages.
	seen := make(map[string]bool)
	var violations []string

	walkGoFiles(t, root, func(path string, f *ast.File) {
		rel, _ := filepath.Rel(root, path)
		pkg := scopedPackage(rel)
		if pkg == "" {
			return
		}

		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				for _, field := range st.Fields.List {
					if !isFuncOrFuncAlias(field.Type, namedFuncTypes[pkg]) {
						continue
					}
					for _, name := range field.Names {
						key := pkg + "." + ts.Name.Name + "." + name.Name
						seen[key] = true
						if !allowlist[key] {
							violations = append(violations, rel+": "+ts.Name.Name+"."+name.Name+" is a func-typed field — remove the callback or add it to the allowlist with remediation plan")
						}
					}
				}
			}
		}
	})

	reportViolations(t, "func-typed struct field in module", violations)

	// Stale allowlist check: any entry not seen in code means the field was
	// removed. Force the list to shrink by failing on orphaned entries.
	for key := range allowlist {
		if !seen[key] {
			t.Errorf("stale allowlist entry %q — field no longer exists in code; remove it from the allowlist", key)
		}
	}
}

// scopedPackage returns the module package name (first directory component
// of rel) if it is in modulePackages and is not a testutil sub-package,
// otherwise "". testutil directories are already skipped by walkGoFiles,
// but the guard here makes scopedPackage self-contained so the exclusion
// is explicit rather than relying solely on the walk filter.
func scopedPackage(rel string) string {
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) < 2 {
		return ""
	}
	pkg := parts[0]
	if !modulePackages[pkg] {
		return ""
	}
	remainder := parts[1]
	if remainder == "testutil" || strings.HasPrefix(remainder, "testutil"+string(filepath.Separator)) {
		return ""
	}
	return pkg
}

// isFuncOrFuncAlias reports whether expr is a func type: either a literal
// ast.FuncType or an identifier whose name is a named func-type alias in the
// same package (namedFuncs).
func isFuncOrFuncAlias(expr ast.Expr, namedFuncs map[string]bool) bool {
	switch t := expr.(type) {
	case *ast.FuncType:
		return true
	case *ast.Ident:
		return namedFuncs[t.Name]
	default:
		return false
	}
}
