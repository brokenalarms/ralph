package loop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// TestRunDelegatesToNamedStageHelpers proves Loop.Run is a compositional
// orchestrator (per docs/specs/architecture.md) rather than a single
// monolithic function: it must call each of the four named stage helpers
// extracted from its body — setupBranchForTask, resumeCheck,
// dispatchAgentAction, and runAftermath — instead of inlining that logic.
func TestRunDelegatesToNamedStageHelpers(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(dir, "loop.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse loop.go: %v", err)
	}

	var runFunc *ast.FuncDecl
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if ok && fd.Name.Name == "Run" && fd.Recv != nil {
			runFunc = fd
			break
		}
	}
	if runFunc == nil {
		t.Fatal("Loop.Run not found in loop.go")
	}

	wantCalls := map[string]bool{
		"setupBranchForTask":  false,
		"resumeCheck":         false,
		"dispatchAgentAction": false,
		"runAftermath":        false,
	}

	ast.Inspect(runFunc.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if _, tracked := wantCalls[sel.Sel.Name]; tracked {
			wantCalls[sel.Sel.Name] = true
		}
		return true
	})

	for name, called := range wantCalls {
		if !called {
			t.Errorf("Loop.Run does not call %s — expected Run to delegate to this stage helper", name)
		}
	}
}

// TestLoopStateBundlesIterationLocals proves the seven variables Run used to
// carry as loose locals across iterations (runIteration, lastAction,
// lastTaskMerged, sessionTasks, currentTaskID, consecutiveSkipCount,
// worktreeNeedsSetup) are now fields on a single loopState struct.
func TestLoopStateBundlesIterationLocals(t *testing.T) {
	want := []string{
		"runIteration",
		"lastAction",
		"lastTaskMerged",
		"sessionTasks",
		"currentTaskID",
		"consecutiveSkipCount",
		"worktreeNeedsSetup",
	}

	typ := reflect.TypeOf(loopState{})
	got := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got[typ.Field(i).Name] = true
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("loopState is missing field %q", name)
		}
	}
}
