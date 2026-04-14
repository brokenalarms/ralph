package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoExportedFieldsOnStubs walks testing.go and fails on:
//   - any exported field on stubGitHub or stubRepo (the unexported fakes
//     must keep all state internal so tests observe behavior only through
//     interface methods),
//   - any func-typed field on StubGitHubConfig or StubRepoConfig (callback
//     fields are the forbidden transition-style pattern — the stub API is
//     data-in, static-world-out).
func TestNoExportedFieldsOnStubs(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "testing.go", nil, 0)
	if err != nil {
		t.Fatalf("parse testing.go: %v", err)
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
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				continue
			}
			switch ts.Name.Name {
			case "stubGitHub", "stubRepo":
				for _, field := range st.Fields.List {
					for _, n := range field.Names {
						if n.IsExported() {
							t.Errorf("%s.%s is exported — fakes must keep state unexported (tests observe via interface methods, not field reads)", ts.Name.Name, n.Name)
						}
					}
				}
			case "StubGitHubConfig", "StubRepoConfig":
				for _, field := range st.Fields.List {
					if _, isFunc := field.Type.(*ast.FuncType); isFunc {
						t.Errorf("%s.%s is a func-typed field — callback fields are forbidden (spec: 'Every callback-based test is transition-style … split into two static-world tests or delete')", ts.Name.Name, fieldNames(field))
					}
				}
			}
		}
	}
}

// TestNoSequencedResponseSlices walks StubGitHubConfig and StubRepoConfig
// fields and fails on any slice-of-result-types. The previous anti-pattern
// was `MergeResults []MergeResult` and `Responses [][]MergeResult` — each
// returning the Nth entry on the Nth call, which programs transition-style
// behavior through data instead of callbacks. The new stub is static: one
// value per method, no sequencing.
func TestNoSequencedResponseSlices(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "testing.go", nil, 0)
	if err != nil {
		t.Fatalf("parse testing.go: %v", err)
	}

	// Result-shaped type names (single-value returns from gitHub / Ops
	// methods). If any slice of these appears in a Config struct, it's
	// the sequenced-response pattern.
	resultShaped := map[string]bool{
		"MergeResult":    true,
		"CICheckResult":  true,
		"ShipResult":     true,
		"ResumeTaskResult": true,
		"PRDetail":       true,
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
			if ts.Name.Name != "StubGitHubConfig" && ts.Name.Name != "StubRepoConfig" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				continue
			}
			for _, field := range st.Fields.List {
				arr, isArr := field.Type.(*ast.ArrayType)
				if !isArr {
					continue
				}
				// Slice-of-slice is always sequenced.
				if _, nested := arr.Elt.(*ast.ArrayType); nested {
					t.Errorf("%s.%s is a slice-of-slice — sequenced-response pattern forbidden", ts.Name.Name, fieldNames(field))
					continue
				}
				elt := typeStr(arr.Elt)
				if resultShaped[elt] {
					t.Errorf("%s.%s is []%s — sequenced-response pattern forbidden (one value per method, not a per-call sequence)", ts.Name.Name, fieldNames(field), elt)
				}
			}
		}
	}
}

// TestStubConstructorsReturnInterfaces verifies that the exported
// constructors of the git package — New and NewStub — return Ops (the
// interface), not a concrete type. If NewForTest is added later for
// real-git integration tests (Phase D), it will be covered here too.
func TestStubConstructorsReturnInterfaces(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse dir: %v", err)
	}

	constructors := map[string]bool{
		"New":        true,
		"NewStub":    true,
		"NewForTest": true, // Phase D (optional: only checked if present)
	}
	seen := map[string]bool{}

	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv != nil {
					continue
				}
				if !constructors[fd.Name.Name] {
					continue
				}
				seen[fd.Name.Name] = true
				if fd.Type.Results == nil || len(fd.Type.Results.List) == 0 {
					t.Errorf("%s has no return type", fd.Name.Name)
					continue
				}
				ret := typeStr(fd.Type.Results.List[0].Type)
				if ret != "Ops" {
					t.Errorf("%s returns %s — must return Ops (the interface)", fd.Name.Name, ret)
				}
			}
		}
	}

	// New and NewStub are required to exist; NewForTest is optional (Phase D).
	for _, required := range []string{"New", "NewStub"} {
		if !seen[required] {
			t.Errorf("expected exported constructor %s to exist in git package", required)
		}
	}
}

// TestRepoIsUnexported enforces Phase E: the git module's concrete struct
// must be `repo` (unexported). External callers see only Ops.
func TestRepoIsUnexported(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse dir: %v", err)
	}
	for _, pkg := range pkgs {
		for filename, f := range pkg.Files {
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
					if ts.Name.Name == "Repo" {
						t.Errorf("%s: type Repo is exported — must be lowercase repo (Phase E)", filename)
					}
				}
			}
		}
	}
}

// TestNoRepoStructLiterals walks all .go files in the git package and
// fails on any `&repo{` or `&stubRepo{` composite literal outside the
// three approved construction seams:
//
//   - git.go         → New() constructor for production *repo
//   - testing.go     → NewStub() constructor for stubRepo
//   - test_helpers_test.go → newRepoForTest() helper for package-internal
//     tests that need a real *repo with injected dependencies
//
// Any other file — production or test — must go through one of the three
// constructors. External packages cannot construct either type (both are
// unexported) so this test defends the package-internal boundary.
func TestNoRepoStructLiterals(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	testFiles, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob tests: %v", err)
	}
	files = append(files, testFiles...)

	exempt := map[string]bool{
		"git.go":               true, // New constructor
		"testing.go":           true, // NewStub constructor + stubRepo helpers
		"test_helpers_test.go": true, // newRepoForTest test helper
	}
	repoLit := regexp.MustCompile(`&repo\{|&stubRepo\{`)
	commentLine := regexp.MustCompile(`^\s*//`)
	seen := map[string]bool{}
	for _, path := range files {
		base := filepath.Base(path)
		if exempt[base] || seen[path] {
			continue
		}
		seen[path] = true
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if commentLine.MatchString(line) {
				continue
			}
			if repoLit.MatchString(line) {
				t.Errorf("%s:%d: repo/stubRepo struct literal — construct via git.New, git.NewStub, or newRepoForTest: %s", path, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestNoLoopStubRepoMutation walks internal/loop/*_test.go and fails on
// post-construction assignments to stub fields. The new stubRepo is
// unexported so this would be a compile error, but the check also catches
// assignments on the local interface variable (gm) that would be possible
// if an exported helper re-introduced mutability.
func TestNoLoopStubRepoMutation(t *testing.T) {
	loopDir := filepath.Join("..", "loop")
	files, err := filepath.Glob(filepath.Join(loopDir, "*_test.go"))
	if err != nil {
		t.Fatalf("glob loop tests: %v", err)
	}
	// Line-comment-aware match: catches `gm.X = y`, `stub.X = y`, `repo.X = y`
	// at the start of a line (after optional indent). Excludes `==`, `:=`,
	// and comments that begin with `//`.
	mutPattern := regexp.MustCompile(`^\s*(gm|stub|repo)\.[A-Z][A-Za-z0-9_]*\s*=\s*[^=]`)
	commentPattern := regexp.MustCompile(`^\s*//`)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if commentPattern.MatchString(line) {
				continue
			}
			if mutPattern.MatchString(line) {
				t.Errorf("%s:%d: post-construction stub field mutation — config must be set at construction: %s", path, i+1, strings.TrimSpace(line))
			}
		}
	}
}
