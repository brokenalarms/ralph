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

// TestOrchestratorParamsNoModules enforces that params/opts structs in the
// loop package carry only data — no module references, no interfaces, and
// no func types (callbacks are module references in disguise).
//
// Structs are added to the `checked` set as each bead lands. The agent
// completing each bead uncomments their structs, making the test enforce
// the rule for those structs only. Once all beads land, every *Params/*Opts
// struct will be checked.
func TestOrchestratorParamsNoModules(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)

	// checked lists structs that must pass the data-only rule.
	// Uncomment each section when the corresponding bead lands.
	checked := map[string]bool{
		// --- Bead 1: git.BranchForTask (ralph-sh1e) ---
		// "branchParams": true,

		// --- Bead 2: git.ResumeTask (ralph-o8sb) ---
		"resumeViaPRParams":      true,
		"resolveByPRStateParams": true,

		// --- Bead 3: git.Ship (ralph-bk7m) ---
		"finalizePRParams": true,

		// --- Bead 4 rework: completeTaskParams data-only (ralph-93jq) ---
		"completeTaskParams": true,

		// --- Bead 6: task selection → tasks module (ralph-u4c7) ---
		// selectNextTaskParams and logIterationBannerParams are data-only structs.
		// pollForTasksParams, waitForTasksParams, and beginIterationParams were
		// converted to Loop methods — no longer exist as params structs.
		"selectNextTaskParams":     true,
		"pollForTasksParams":       true,
		"waitForTasksParams":       true,
		"beginIterationParams":     true,
		"logIterationBannerParams": true,

		// --- Bead 7: state/attempts out of params (ralph-eycr) ---
		// Structs converted to Loop methods — no longer exist as params structs.
		"processRunOutcomeParams": true,
		"handleRunResultParams":   true,
		"initParams":              true,
		"initWorktreeParams":      true,
		"flushUnpushedWorkParams": true,

		// --- Bead 8: limiter/analyzer out of params (ralph-r7my) ---
		"waitForRateParams":           true,
		"maybeRefactorParams":         true,
		"llmShouldRefactorParams":     true,
		"analyzeIterationParams":      true,
		"prepareAndBuildPromptParams": true,

		// --- Bead 9: merge/iteration params (ralph-bk7m / ralph-93jq) ---
		// "mergeWithRetryParams": true,
	}

	if len(checked) == 0 {
		t.Skip("all sections commented out — uncomment as beads land")
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
				if !checked[structName] {
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
