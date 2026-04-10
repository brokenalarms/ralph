package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Proves: maybeRefactor skips when Refactor is false (default),
// ensuring refactoring is opt-in only.
func TestLoop_MaybeRefactor_DisabledByDefault(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := New(Config{
		Dirs:     workctx.WorkContext{RalphDir: ralphDir},
		Refactor: false,
	}, st, &git.StubRepo{WorkDir: dir})

	err := l.maybeRefactor(context.Background(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Proves: maybeRefactor skips when fewer than 5 tasks have been
// completed in the session, even with --refactor enabled.
func TestLoop_MaybeRefactor_SkipsBelow5Completions(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := New(Config{
		Dirs:     workctx.WorkContext{RalphDir: ralphDir},
		Refactor: true,
	}, st, &git.StubRepo{WorkDir: dir})

	err := l.maybeRefactor(context.Background(), 3)
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
	l := New(Config{
		Dirs:     workctx.WorkContext{RalphDir: ralphDir},
		Refactor: true,
		QueryFn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			queryFnCalled = true
			return "NO\nCode looks fine.", nil
		},
	}, st, &git.StubRepo{WorkDir: dir, RecentFilesValue: "file.go\nother.go"})

	err := l.maybeRefactor(context.Background(), 5)
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
	l := New(Config{
		Dirs: workctx.WorkContext{
			RalphDir:   ralphDir,
			WorkDir:    dir,
			PromptsDir: promptsDir,
		},
		Refactor:     true,
		CallsPerHour: 80,
		QueryFn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			return "YES\nThere is significant duplication.", nil
		},
	}, st, &git.StubRepo{WorkDir: dir, RecentFilesValue: "file.go\nother.go"})
	l.runner = &stubRunner{
		onRun: func() {
			runnerCalled = true
		},
	}

	err := l.maybeRefactor(context.Background(), 5)
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
	l := New(Config{
		Dirs:     workctx.WorkContext{RalphDir: ralphDir},
		Refactor: true,
		QueryFn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			queryCalls++
			return "NO\nAll good.", nil
		},
	}, st, &git.StubRepo{WorkDir: dir, RecentFilesValue: "file.go\nother.go"})

	// 7 completions: should NOT trigger (not a multiple of 5)
	queryCalls = 0
	l.maybeRefactor(context.Background(), 7) //nolint:errcheck
	if queryCalls != 0 {
		t.Errorf("expected 0 LLM calls at 7 completions, got %d", queryCalls)
	}

	// 10 completions: should trigger
	queryCalls = 0
	l.maybeRefactor(context.Background(), 10) //nolint:errcheck
	if queryCalls != 1 {
		t.Errorf("expected 1 LLM call at 10 completions, got %d", queryCalls)
	}
}

// Proves: llmShouldRefactor correctly parses YES/NO responses in various
// formats, including case variations and extra whitespace.
func TestLoop_LLMShouldRefactor_ParsesResponses(t *testing.T) {
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
			queryFn := func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return tt.response, nil
			}
			got, err := llmShouldRefactor(context.Background(), queryFn, t.TempDir(), "arch spec", "file1.go\nfile2.go")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("llmShouldRefactor(%q) = %v, want %v", tt.response, got, tt.want)
			}
		})
	}
}
