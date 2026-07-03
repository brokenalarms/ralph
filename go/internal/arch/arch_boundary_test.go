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
