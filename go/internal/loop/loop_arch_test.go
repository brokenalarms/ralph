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
// loop package do not carry module references. Only data (primitives, data
// structs without methods) and *logging.Logger are allowed. Function types
// are forbidden — they are module references in disguise.
//
// Each section is commented out until the corresponding bead lands.
// The agent completing each bead uncomments their section.
func TestOrchestratorParamsNoModules(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)

	forbidden := map[string]string{
		// --- Bead 1: git.BranchForTask (ralph-sh1e) ---
		// "branchParams": "git.GitOps",

		// --- Bead 2: git.ResumeTask (ralph-o8sb) ---
		// "resumeViaPRParams":      "git.GitOps",
		// "resolveByPRStateParams": "git.GitOps",

		// --- Bead 3: git.Ship (ralph-bk7m) ---
		// "finalizePRParams": "git.GitOps",

		// --- Bead 4: completeTask (ralph-6a80) ---
		// Landed but replaced module refs with 22 func fields.
		// Universal no-func-types check catches these.
		// Needs re-work: completeTaskParams must carry only data.

		// --- Bead 6: task selection → tasks module ---
		// "selectNextTaskParams":  "tasks.Backend",
		// "pollForTasksParams":    "tasks.Backend",
		// "waitForTasksParams":    "state.Store",
		// "beginIterationParams":  "state.Store",
		// "logIterationBannerParams": "tasks.Backend",

		// --- Bead 7: state/attempts out of params ---
		// "processRunOutcomeParams": "state.Store",
		// "handleRunResultParams":   "attempts.Tracker",
		// "initParams":              "state.Store",
		// "initWorktreeParams":      "state.Store",
		// "flushUnpushedWorkParams": "state.Store",

		// --- Bead 8: limiter/analyzer out of params ---
		// "waitForRateParams":       "ratelimit.Limiter",
		// "maybeRefactorParams":     "ratelimit.Limiter",
		// "analyzeIterationParams":  "analyzer.Analyzer",
		// "prepareAndBuildPromptParams": "ratelimit.Limiter",
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
				if !isParamsOrOpts(structName) {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					typStr := typeString(field.Type)

					// Check bead-specific forbidden types.
					if wantGone, tracked := forbidden[structName]; tracked {
						if strings.Contains(typStr, wantGone) {
							for _, name := range field.Names {
								t.Errorf("%s.%s carries module %s — must be data, not a module reference",
									structName, name.Name, wantGone)
							}
						}
					}

					// Universal rule: no func types in any params/opts struct.
					// Callbacks are module references in disguise.
					if isFuncType(field.Type) {
						for _, name := range field.Names {
							t.Errorf("%s.%s is a func type — params/opts structs must carry only data, not callbacks",
								structName, name.Name)
						}
					}

					// Universal rule: no interface types.
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
