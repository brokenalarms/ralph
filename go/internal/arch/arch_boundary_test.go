// Package arch contains repo-wide architecture boundary tests. It has no
// production code — only tests that parse the rest of the module's AST and
// enforce the Tier 1 rules in docs/specs/architecture.md ("What NOT to Do").
package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// tool describes one externally-invoked CLI covered by the architecture's
// module-boundary rule, and where exec.Command/exec.CommandContext is allowed
// to reference it by its literal name.
type tool struct {
	name       string // literal first argument to exec.Command/exec.CommandContext
	rule       string // the "What NOT to Do" line this tool's boundary enforces
	useInstead string // the module a violation should route through instead
	allowed    func(relPath string) bool
}

var boundedTools = []tool{
	{
		name:       "git",
		rule:       `No exec.Command("git", ...) outside the git package`,
		useInstead: "internal/git",
		allowed: func(relPath string) bool {
			return withinDir(relPath, "internal/git")
		},
	},
	{
		name:       "gh",
		rule:       `No exec.Command("gh", ...) outside git/github.go`,
		useInstead: "internal/git/github.go",
		allowed: func(relPath string) bool {
			return relPath == filepath.Join("internal", "git", "github.go")
		},
	},
	{
		name:       "claude",
		rule:       `No exec.Command("claude", ...) outside the agent and claude packages`,
		useInstead: "internal/agent or internal/claude",
		allowed: func(relPath string) bool {
			return withinDir(relPath, "internal/agent") || withinDir(relPath, "internal/claude")
		},
	},
	{
		name:       "bd",
		rule:       `No exec.Command("bd", ...) outside tasks/bd.go`,
		useInstead: "internal/tasks/bd.go",
		allowed: func(relPath string) bool {
			return relPath == filepath.Join("internal", "tasks", "bd.go")
		},
	},
}

// withinDir reports whether relPath (slash-agnostic) lives under dir
// (given with forward slashes), matching dir itself or any of its subpackages.
func withinDir(relPath, dir string) bool {
	dir = filepath.FromSlash(dir)
	return relPath == dir || strings.HasPrefix(relPath, dir+string(filepath.Separator))
}

// TestExecBoundaries walks every non-test .go file in the module and fails
// on any exec.Command/exec.CommandContext call whose first literal argument
// is one of the CLIs the architecture confines to a specific module — see
// docs/specs/architecture.md ("What NOT to Do"). Each failure names the
// violated rule and the module the call should route through instead.
func TestExecBoundaries(t *testing.T) {
	root := moduleRoot(t)

	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", relPath, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			cmdArg, ok := execCommandArg(call)
			if !ok {
				return true
			}
			lit, ok := stringLiteral(cmdArg)
			if !ok {
				return true
			}
			for _, tl := range boundedTools {
				if lit != tl.name || tl.allowed(relPath) {
					continue
				}
				pos := fset.Position(call.Pos())
				t.Errorf("%s:%d: exec.Command(%q, ...) violates docs/specs/architecture.md 'What NOT to Do': %s — route this through %s instead",
					relPath, pos.Line, lit, tl.rule, tl.useInstead)
			}
			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// execCommandArg returns the AST expression holding the command-name argument
// of an exec.Command(name, ...) or exec.CommandContext(ctx, name, ...) call,
// or ok=false if call isn't one of those two.
func execCommandArg(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return nil, false
	}
	switch sel.Sel.Name {
	case "Command":
		if len(call.Args) < 1 {
			return nil, false
		}
		return call.Args[0], true
	case "CommandContext":
		if len(call.Args) < 2 {
			return nil, false
		}
		return call.Args[1], true
	default:
		return nil, false
	}
}

// stringLiteral returns the unquoted value of expr if it is a string literal.
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// moduleRoot walks up from this file's directory to find the go.mod that
// defines the module under test (go/go.mod).
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller for module root lookup")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above " + thisFile)
		}
		dir = parent
	}
}

// mergePathFiles are the files on the merge/CI decision path whose returned
// errors must be typed so classifyMergeOutcome can branch on them.
var mergePathFiles = []string{
	filepath.Join("internal", "git", "git_merge.go"),
	filepath.Join("internal", "git", "ci.go"),
}

// typedOutcomeRule is the "What NOT to Do" line the merge-path error rule enforces.
const typedOutcomeRule = "No inline `fmt.Errorf`/`errors.New` without `%w` in a return statement on the merge/CI decision path"

// untypedErrorReturn locates one return statement that builds an error inline
// instead of returning a named error type or an errors.Is-able sentinel.
type untypedErrorReturn struct {
	line int
	call string
}

// TestMergePathErrorsAreTyped enforces docs/specs/architecture.md Principle 7
// on the merge/CI decision path: every merge-blocking outcome leaves
// internal/git as a named error type or a package-level sentinel, so
// classifyMergeOutcome (and any future caller) can demux it with
// errors.As/errors.Is rather than by matching on a formatted string.
func TestMergePathErrorsAreTyped(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	for _, relPath := range mergePathFiles {
		f, err := parser.ParseFile(fset, filepath.Join(root, relPath), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", relPath, err)
		}
		for _, v := range untypedErrorReturns(fset, f) {
			t.Errorf("%s:%d: return statement constructs %s inline — violates docs/specs/architecture.md 'What NOT to Do': %s — return a named error type or a package-level sentinel wrapped with %%w instead",
				relPath, v.line, v.call, typedOutcomeRule)
		}
	}
}

// TestUntypedErrorReturnsDetector proves the merge-path detector actually
// fires: an inline fmt.Errorf or errors.New in a return statement is reported,
// while %w wrapping and package-level sentinels are not.
func TestUntypedErrorReturnsDetector(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantLine int
		wantCall string
	}{
		{
			name: "inline fmt.Errorf without %w",
			src: `package p
func f() error {
	return fmt.Errorf("x")
}`,
			wantLine: 3,
			wantCall: "fmt.Errorf",
		},
		{
			name: "inline errors.New",
			src: `package p
func f() error {
	return errors.New("x")
}`,
			wantLine: 3,
			wantCall: "errors.New",
		},
		{
			name: "wrapping and sentinels are allowed",
			src: `package p
var ErrX = errors.New("x")
var ErrY = fmt.Errorf("y")
func f(err error) error {
	if err != nil {
		return fmt.Errorf("x: %w", err)
	}
	return ErrX
}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "snippet.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse snippet: %v", err)
			}
			got := untypedErrorReturns(fset, f)
			if tc.wantCall == "" {
				if len(got) != 0 {
					t.Fatalf("expected no violations, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly 1 violation, got %+v", got)
			}
			if got[0].line != tc.wantLine || got[0].call != tc.wantCall {
				t.Errorf("violation = %+v, want line %d call %s", got[0], tc.wantLine, tc.wantCall)
			}
		})
	}
}

// untypedErrorReturns reports every return statement in f whose results
// construct an error inline: a call to errors.New, or a call to fmt.Errorf
// whose string-literal format does not wrap an underlying error with %w. A
// non-literal format string is not judged — its content is not knowable here.
func untypedErrorReturns(fset *token.FileSet, f *ast.File) []untypedErrorReturn {
	var found []untypedErrorReturn
	ast.Inspect(f, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, result := range ret.Results {
			ast.Inspect(result, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := qualifiedCallName(call)
				if !ok {
					return true
				}
				switch name {
				case "errors.New":
				case "fmt.Errorf":
					format, isLiteral := stringLiteral(firstArg(call))
					if !isLiteral || strings.Contains(format, "%w") {
						return true
					}
				default:
					return true
				}
				found = append(found, untypedErrorReturn{line: fset.Position(call.Pos()).Line, call: name})
				return true
			})
		}
		return true
	})
	return found
}

// qualifiedCallName returns the "pkg.Func" name of a selector call expression.
func qualifiedCallName(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return pkg.Name + "." + sel.Sel.Name, true
}

// firstArg returns call's first argument, or nil when it has none.
func firstArg(call *ast.CallExpr) ast.Expr {
	if len(call.Args) == 0 {
		return nil
	}
	return call.Args[0]
}
