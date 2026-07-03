package claude

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestClaudeGo_SplitByResponsibility verifies claude.go was split into
// claude_poll.go (poll/watchdog/rate-limit engine), claude_signals.go
// (signal-file helpers), claude_process.go (process lifecycle), and
// claude_config.go (static config) — matching the grouping described in
// ralph-xfxi. Prevents regression back into one monolithic claude.go.
func TestClaudeGo_SplitByResponsibility(t *testing.T) {
	wantFile := map[string]string{
		// process lifecycle
		"Run":                 "claude_process.go",
		"gracefulKill":        "claude_process.go",
		"stopProcessGroup":    "claude_process.go",
		"processAlive":        "claude_process.go",
		"waitForOutputSettle": "claude_process.go",

		// poll/watchdog/rate-limit engine
		"poll":                   "claude_poll.go",
		"scanNewLines":           "claude_poll.go",
		"isContentActivity":      "claude_poll.go",
		"parseSystemStatusEvent": "claude_poll.go",

		// signal-file helpers
		"clearSignals":      "claude_signals.go",
		"hasSignal":         "claude_signals.go",
		"readFirstLine":     "claude_signals.go",
		"stripJSONFragment": "claude_signals.go",
		"readSignalSummary": "claude_signals.go",

		// static config
		"IterationAllowedTools":    "claude_config.go",
		"IterationDisallowedTools": "claude_config.go",
		"Timeouts":                 "claude_config.go",
		"RunConfig":                "claude_config.go",
		"buildAgentEnv":            "claude_config.go",

		// Runner struct itself is cohesive and stays in claude.go, along with
		// its stdin-injection API.
		"Runner":           "claude.go",
		"InjectMessage":    "claude.go",
		"UserInputMessage": "claude.go",
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	gotFile := map[string]string{}
	for _, pkg := range pkgs {
		for filename, f := range pkg.Files {
			base := filepath.Base(filename)
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					name := d.Name.Name
					// Methods (e.g. func (r *Runner) poll(...)) are recorded
					// under their bare method name — the receiver type isn't
					// part of the responsibility split being checked here.
					gotFile[name] = base
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							gotFile[s.Name.Name] = base
						case *ast.ValueSpec:
							for _, n := range s.Names {
								gotFile[n.Name] = base
							}
						}
					}
				}
			}
		}
	}

	for name, want := range wantFile {
		got, ok := gotFile[name]
		if !ok {
			t.Errorf("%s: not found in package", name)
			continue
		}
		if got != want {
			t.Errorf("%s: expected in %s, found in %s", name, want, got)
		}
	}
}
