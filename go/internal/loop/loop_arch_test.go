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
// loop package do not carry module references (interfaces or pointers to
// structs with methods). Only data, callbacks (func types), and
// *logging.Logger are allowed.
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
		// "runAndCompleteParams": "git.GitOps",
		// "handlePostSignalOpts": "git.GitOps",

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

	if len(forbidden) == 0 {
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
				wantGone, tracked := forbidden[structName]
				if !tracked {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					typStr := typeString(field.Type)
					if strings.Contains(typStr, wantGone) {
						for _, name := range field.Names {
							t.Errorf("%s.%s carries module %s — must be data or callback, not a module reference",
								structName, name.Name, wantGone)
						}
					}
				}
			}
		}
	}
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
