package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Proves: maybeRefactor skips when Refactor is false (default),
// ensuring refactoring is opt-in only.
func TestLoop_MaybeRefactor_DisabledByDefault(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := &Loop{
		cfg:    Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, Refactor: false},
		state:  st,
		logger: logging.New(nil),
	}

	err := l.maybeRefactor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Proves: maybeRefactor skips when fewer than 5 tasks have been
// completed in the session, even with --refactor enabled.
func TestLoop_MaybeRefactor_SkipsBelow5Completions(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := &Loop{
		cfg:          Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, Refactor: true},
		state:        st,
		logger:       logging.New(nil),
		sessionTasks: []CompletedTask{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}

	err := l.maybeRefactor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Proves: maybeRefactor calls the LLM when exactly 5 tasks are completed
// and the LLM says NO, no refactoring iteration is spawned.
func TestLoop_MaybeRefactor_LLMSaysNo(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	queryFnCalled := false
	l := &Loop{
		cfg:          Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, Refactor: true},
		state:        st,
		logger:       logging.New(nil),
		git:          &testutil.StubGit{WorkDir: dir, RecentFilesValue: "file.go\nother.go"},
		sessionTasks: make([]CompletedTask, 5),
		refactorQueryFunc: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			queryFnCalled = true
			return "NO\nCode looks fine.", nil
		},
	}

	err := l.maybeRefactor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !queryFnCalled {
		t.Error("expected LLM query to be called at 5 completions")
	}
}

// Proves: maybeRefactor spawns a refactoring iteration when the LLM says YES,
// verifying the full path from LLM decision through runner invocation.
func TestLoop_MaybeRefactor_LLMSaysYes(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	runnerCalled := false
	l := &Loop{
		cfg: Config{
			Dirs: workctx.WorkContext{
				RalphDir:   ralphDir,
				WorkDir:    dir,
				PromptsDir: promptsDir,
			},
			Refactor:     true,
			CallsPerHour: 80,
		},
		state:        st,
		logger:       logging.New(nil),
		git:          &testutil.StubGit{WorkDir: dir, RecentFilesValue: "file.go\nother.go"},
		sessionTasks: make([]CompletedTask, 5),
		limiter:      ratelimit.New(ralphDir, 80),
		signals:      claude.DefaultSignalPaths(ralphDir),
		runner: &stubRunner{
			onRun: func() {
				runnerCalled = true
			},
		},
		refactorQueryFunc: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			return "YES\nThere is significant duplication.", nil
		},
	}

	err := l.maybeRefactor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !runnerCalled {
		t.Error("expected runner to be called when LLM says YES")
	}
}

// Proves: maybeRefactor triggers at every multiple of 5, not just the first.
func TestLoop_MaybeRefactor_TriggersAtMultiplesOf5(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	queryCalls := 0
	base := &Loop{
		cfg:    Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, Refactor: true},
		state:  st,
		logger: logging.New(nil),
		git:    &testutil.StubGit{WorkDir: dir, RecentFilesValue: "file.go\nother.go"},
		refactorQueryFunc: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			queryCalls++
			return "NO\nAll good.", nil
		},
	}

	// 7 completions: should NOT trigger (not a multiple of 5)
	base.sessionTasks = make([]CompletedTask, 7)
	queryCalls = 0
	base.maybeRefactor()
	if queryCalls != 0 {
		t.Errorf("expected 0 LLM calls at 7 completions, got %d", queryCalls)
	}

	// 10 completions: should trigger
	base.sessionTasks = make([]CompletedTask, 10)
	queryCalls = 0
	base.maybeRefactor()
	if queryCalls != 1 {
		t.Errorf("expected 1 LLM call at 10 completions, got %d", queryCalls)
	}
}

// Proves: llmShouldRefactor correctly parses YES/NO responses in various
// formats, including case variations and extra whitespace.
func TestLoop_LLMShouldRefactor_ParsesResponses(t *testing.T) {
	l := &Loop{git: &testutil.StubGit{WorkDir: t.TempDir()}}

	tests := []struct {
		name     string
		response string
		want     bool
	}{
		{"uppercase YES", "YES\nDuplication found.", true},
		{"lowercase yes", "yes\nneeds cleanup", true},
		{"mixed case Yes", "Yes\nsome issues", true},
		{"uppercase NO", "NO\nCode looks fine.", false},
		{"lowercase no", "no\neverything clean", false},
		{"with leading whitespace", "  YES\nfoo", true},
		{"unknown response", "MAYBE\nnot sure", false},
		{"empty response", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l.refactorQueryFunc = func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return tt.response, nil
			}
			got, err := l.llmShouldRefactor(context.Background(), "arch spec", "file1.go\nfile2.go")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("llmShouldRefactor(%q) = %v, want %v", tt.response, got, tt.want)
			}
		})
	}
}
