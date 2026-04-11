package loop

// orchestrator_arch_test.go enforces the rules in
// docs/specs/orchestrator-modules.md mechanically. These tests walk every
// .go file under go/internal/ and fail on violations of the orchestrator/
// module-boundary rules.
//
// As of the spec landing, these tests are EXPECTED TO FAIL on the current
// code. The failures ARE the punch list for the refactor — each failure
// names the file, line, struct/function, and what needs to change. Tests
// will turn green commit-by-commit as the refactor lands.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// forbiddenModuleTypes is the list of types that may not appear as struct
// fields (outside the Loop struct) or as function/method parameters
// (anywhere outside Loop's own private methods and constructors).
//
// Each entry is the type as it appears in source after import resolution:
// "git.Ops" for the interface, "*git.Repo" for the pointer to the struct,
// etc. The check is on the rendered type string from typeString().
var forbiddenModuleTypes = map[string]bool{
	"git.Ops":             true,
	"*git.Repo":           true,
	"git.Repo":            true,
	"git.Manager":         true,
	"*git.Manager":        true,
	"git.GitOps":          true,
	"*state.Store":        true,
	"state.Store":         true,
	"*attempts.Tracker":   true,
	"attempts.Tracker":    true,
	"*ratelimit.Limiter":  true,
	"ratelimit.Limiter":   true,
	"*analyzer.Analyzer":  true,
	"analyzer.Analyzer":   true,
	"tasks.Backend":       true,
	"*agent.Agent":        true,
	"agent.Agent":         true,
	"*verifier.Verifier":  true,
	"verifier.Verifier":   true,
	"claudeRunner":        true,
}

// allowedNonLoopStructHolders names structs that are allowed to hold
// module-typed fields. This is the whitelist for rule 1 in the spec.
//
// As the refactor progresses this list should never grow. The permanent
// entries are:
//
//   - "Loop" — the orchestrator itself; this is Rule 1.
//   - "Modules" — the struct form of loop.New's module-reference
//     parameter list. Modules is owned by nobody after construction:
//     cmd/ralph/main.go builds one, hands it to loop.New, and loop.New
//     copies each field onto Loop's private fields and discards the
//     struct. It IS the constructor's parameter list, just packaged as
//     a struct. Rule A's carve-out for loop.New's parameters applies
//     to Modules the same way it applies to positional parameters.
//     See docs/specs/orchestrator-modules.md.
var allowedModuleHolders = map[string]bool{
	"Loop":    true,
	"Modules": true,
}

// internalDir returns the absolute path to go/internal/ from any test
// file's runtime location.
func internalDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile is .../go/internal/loop/orchestrator_arch_test.go
	// We want .../go/internal/
	return filepath.Dir(filepath.Dir(thisFile))
}

// walkGoFiles walks every non-test .go file under root and calls fn for
// each parsed file. Test files (_test.go), generated files, and the
// stub*/testutil packages are skipped — they exist to construct stubs
// and may legitimately reference module types.
func walkGoFiles(t *testing.T, root string, fn func(path string, f *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "testutil" || strings.HasPrefix(base, "stub") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Logf("parse %s: %v", path, perr)
			return nil
		}
		fn(path, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// reportViolations prints up to head violations and fails the test with
// the total count. Avoids overwhelming output when there are many.
func reportViolations(t *testing.T, label string, violations []string) {
	t.Helper()
	if len(violations) == 0 {
		return
	}
	sort.Strings(violations)
	const head = 30
	for i, v := range violations {
		if i >= head {
			break
		}
		t.Errorf("%s: %s", label, v)
	}
	if len(violations) > head {
		t.Errorf("%s: %d more violations not shown (total %d)", label, len(violations)-head, len(violations))
	}
}

// TestNoModulesInNonLoopStructs walks every struct type in go/internal/
// and fails when a non-Loop struct holds a field whose type is a known
// module. See rule 1 in docs/specs/orchestrator-modules.md.
func TestNoModulesInNonLoopStructs(t *testing.T) {
	root := internalDir(t)
	var violations []string

	walkGoFiles(t, root, func(path string, f *ast.File) {
		rel, _ := filepath.Rel(root, path)
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
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				structName := ts.Name.Name
				if allowedModuleHolders[structName] {
					continue
				}
				for _, field := range st.Fields.List {
					tn := typeString(field.Type)
					if !forbiddenModuleTypes[tn] && !forbiddenModuleTypes[strings.TrimPrefix(tn, "*")] {
						continue
					}
					names := []string{"_"}
					if len(field.Names) > 0 {
						names = nil
						for _, n := range field.Names {
							names = append(names, n.Name)
						}
					}
					for _, name := range names {
						violations = append(violations, formatViolation(rel, structName, name, tn))
					}
				}
			}
		}
	})
	reportViolations(t, "non-Loop struct holds module field", violations)
}

func formatViolation(file, structName, fieldName, typeName string) string {
	return file + ": " + structName + "." + fieldName + " has type " + typeName
}

// TestNoModulesInFunctionParams walks every function and method declaration
// in go/internal/ and fails when a parameter type is a known module.
// Constructors (func names starting with "New") are exempt because they
// are the entry point for dependency injection.
//
// See rule 2 in docs/specs/orchestrator-modules.md.
func TestNoModulesInFunctionParams(t *testing.T) {
	root := internalDir(t)
	var violations []string

	walkGoFiles(t, root, func(path string, f *ast.File) {
		rel, _ := filepath.Rel(root, path)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Type.Params == nil {
				continue
			}
			if strings.HasPrefix(fd.Name.Name, "New") {
				continue
			}
			for _, field := range fd.Type.Params.List {
				tn := typeString(field.Type)
				if !forbiddenModuleTypes[tn] && !forbiddenModuleTypes[strings.TrimPrefix(tn, "*")] {
					continue
				}
				names := []string{"_"}
				if len(field.Names) > 0 {
					names = nil
					for _, n := range field.Names {
						names = append(names, n.Name)
					}
				}
				for _, name := range names {
					violations = append(violations, rel+": "+fd.Name.Name+" parameter "+name+" has type "+tn)
				}
			}
		}
	})
	reportViolations(t, "function/method parameter is a module type", violations)
}

// *logging.Logger is the single named cross-module exception to the
// "no module objects passed through" rule. Logging is genuinely
// cross-cutting — every package needs to log — and package-level state
// would leak across parallel tests. So *logging.Logger is allowed as a
// struct field and as a function parameter, by name, with no further
// exemptions.
//
// See rule 5 in docs/specs/orchestrator-modules.md.
//
// The two arch tests below (TestNoLoggerAsField, TestNoLoggerInFunctionParams)
// are intentionally no-ops. They exist as named placeholders so that any
// future change to the rule lands here. If the rule changes back to
// "no logger threading", these tests can be re-implemented.

// TestNoImplementationLeakInExportedNames walks exported type, field, and
// function names in go/internal/ and fails when a name contains an
// implementation prefix outside the module that owns that implementation.
// "Bead*" outside the tasks package leaks beads-as-backend through the
// API surface; "Github*" outside the git package does the same; etc.
//
// See rule 6 in docs/specs/orchestrator-modules.md.
func TestNoImplementationLeakInExportedNames(t *testing.T) {
	root := internalDir(t)
	var violations []string

	type leak struct {
		prefix    string
		ownerPath string // package path that may legitimately use this prefix
	}
	leaks := []leak{
		{"Bead", "tasks"},
		{"Github", "git"},
		{"Copilot", "git"},
	}

	walkGoFiles(t, root, func(path string, f *ast.File) {
		rel, _ := filepath.Rel(root, path)
		// rel looks like "loop/loop.go" or "tasks/beads/beads.go"
		// The owning package is the first directory component.
		parts := strings.Split(rel, string(filepath.Separator))
		owningDir := ""
		if len(parts) > 0 {
			owningDir = parts[0]
		}

		check := func(name string) {
			if !ast.IsExported(name) {
				return
			}
			for _, l := range leaks {
				if !strings.HasPrefix(name, l.prefix) {
					continue
				}
				if owningDir == l.ownerPath {
					return
				}
				violations = append(violations, rel+": exported name "+name+" leaks \""+l.prefix+"\" prefix outside "+l.ownerPath+"/")
				return
			}
		}

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					check(ts.Name.Name)
					if st, ok := ts.Type.(*ast.StructType); ok {
						for _, field := range st.Fields.List {
							for _, name := range field.Names {
								check(name.Name)
							}
						}
					}
					if it, ok := ts.Type.(*ast.InterfaceType); ok {
						for _, method := range it.Methods.List {
							for _, name := range method.Names {
								check(name.Name)
							}
						}
					}
				}
			case *ast.FuncDecl:
				check(d.Name.Name)
			}
		}
	})
	reportViolations(t, "exported name leaks implementation prefix", violations)
}
