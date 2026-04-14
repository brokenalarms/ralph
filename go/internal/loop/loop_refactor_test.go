package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Proves: maybeRefactor skips when Refactor is false (default),
// ensuring refactoring is opt-in only.
func TestLoop_MaybeRefactor_DisabledByDefault(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	cfg := Config{
		Dirs:     workctx.WorkContext{RalphDir: ralphDir},
		Refactor: false,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{WorkDir: dir}),
		TaskBackend: nil,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

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

	cfg := Config{
		Dirs:     workctx.WorkContext{RalphDir: ralphDir},
		Refactor: true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{WorkDir: dir}),
		TaskBackend: nil,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

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
	cfg := Config{
		Dirs:     workctx.WorkContext{RalphDir: ralphDir},
		Refactor: true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{WorkDir: dir, RecentChangedFiles: "file.go\nother.go"}),
		TaskBackend: nil,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.querier = &stubQuerier{
		fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			queryFnCalled = true
			return "NO\nCode looks fine.", nil
		},
	}

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
	cfg := Config{
		Dirs: workctx.WorkContext{
			RalphDir:   ralphDir,
			WorkDir:    dir,
			PromptsDir: promptsDir,
		},
		Refactor:     true,
		CallsPerHour: 80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{WorkDir: dir, RecentChangedFiles: "file.go\nother.go"}),
		TaskBackend: nil,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.runner = &stubRunner{
		onRun: func() {
			runnerCalled = true
		},
	}
	l.querier = &stubQuerier{
		fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			return "YES\nThere is significant duplication.", nil
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
	cfg := Config{
		Dirs:     workctx.WorkContext{RalphDir: ralphDir},
		Refactor: true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{WorkDir: dir, RecentChangedFiles: "file.go\nother.go"}),
		TaskBackend: nil,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.querier = &stubQuerier{
		fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			queryCalls++
			return "NO\nAll good.", nil
		},
	}

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
			resp := tt.response
			dir, st := setupTestDir(t)
			ralphDir := filepath.Join(dir, ".ralph")
			cfg := Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}}
			logger := logging.New(nil)
			l := New(cfg, Modules{
				State: st,
				Git:   git.NewStub(git.StubRepoConfig{WorkDir: dir}),
				Querier: &stubQuerier{
					fn: func(_ context.Context, _, _, _ string) (string, error) {
						return resp, nil
					},
				},
				Logger:   logger,
				Verifier: newTestVerifier(t, cfg, logger),
			})
			got, err := l.llmShouldRefactor(context.Background(), "arch spec", "file1.go\nfile2.go")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("llmShouldRefactor(%q) = %v, want %v", resp, got, tt.want)
			}
		})
	}
}
