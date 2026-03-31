package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/prompt"
)

const refactorCheckInterval = 5
const refactorLookbackCommits = 10

func (l *Loop) maybeRefactor() error {
	if !l.cfg.Refactor {
		return nil
	}

	completedCount := len(l.sessionTasks)
	if completedCount == 0 || completedCount%refactorCheckInterval != 0 {
		return nil
	}

	recentFiles := l.git.RecentChangedFiles(refactorLookbackCommits)
	if recentFiles == "" {
		return nil
	}

	archSpec := readArchSpec(l.git.GetWorkDir())

	shouldRefactor, err := l.llmShouldRefactor(context.Background(), archSpec, recentFiles)
	if err != nil {
		return fmt.Errorf("refactor check: %w", err)
	}

	if !shouldRefactor {
		l.logger.Log("refactor", "LLM says no refactoring needed — skipping")
		return nil
	}

	l.logger.Phase("--- Adaptive refactor (LLM recommended) ---")

	refactorPrompt, err := prompt.BuildRefactorPrompt(prompt.Vars{
		PromptsDir:       l.cfg.Dirs.PromptsDir,
		WorkDir:          l.git.GetWorkDir(),
		SignalToken:      l.signals.Complete,
		CurrentTaskToken: l.signals.CurrentTask,
		AllCompleteToken: l.signals.AllComplete,
	}, recentFiles)
	if err != nil {
		return fmt.Errorf("building refactor prompt: %w", err)
	}

	if !l.limiter.Allowed() {
		l.logger.Warn("llm", "Rate limit hit before refactor — waiting for reset")
		if err := l.limiter.WaitForReset(context.Background(), func(secs int) {
			l.logger.Log("llm", "Rate limit: %ds until reset", secs)
		}); err != nil {
			return err
		}
	}

	rawLogPath := filepath.Join(l.cfg.Dirs.RalphDir, "raw.log")
	_, err = l.runner.Run(claude.RunConfig{
		WorkDir:      l.git.GetWorkDir(),
		RalphDir:     l.cfg.Dirs.RalphDir,
		Prompt:       refactorPrompt,
		RawLog:       rawLogPath,
		LogFile:      filepath.Join(l.cfg.Dirs.RalphDir, "loop.log"),
		Quiet:        l.cfg.Quiet,
		Verbose:      l.cfg.Verbose,
		Signals:      l.signals,
		PollInterval: 2 * time.Second,
	})
	l.limiter.Increment()

	l.logger.Success("", "Refactor iteration complete")

	return err
}

func readArchSpec(workDir string) string {
	path := filepath.Join(workDir, "docs", "specs", "architecture.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)
	if len(content) > 4000 {
		content = content[:4000]
	}
	return content
}

func (l *Loop) llmShouldRefactor(ctx context.Context, archSpec, recentFiles string) (bool, error) {
	queryFn := l.refactorQueryFunc
	if queryFn == nil && l.agentRunner != nil {
		queryFn = l.agentRunner.Query
	}
	if queryFn == nil {
		return false, fmt.Errorf("no query function available")
	}

	prompt := "You are deciding whether a codebase needs refactoring.\n\n"
	if archSpec != "" {
		prompt += "## Architecture spec\n" + archSpec + "\n\n"
	}
	prompt += "## Recently changed files\n" + recentFiles + "\n\n"
	prompt += "Based on the recently changed files and the architecture spec, does this codebase need refactoring right now?\n"
	prompt += "Consider: code duplication, unclear naming, files growing too large, architectural drift from the spec, dead code.\n"
	prompt += "Reply with exactly YES or NO on the first line, followed by a brief explanation."

	response, err := queryFn(ctx, l.git.GetWorkDir(), prompt, "")
	if err != nil {
		return false, err
	}

	firstLine := strings.SplitN(strings.TrimSpace(response), "\n", 2)[0]
	return strings.EqualFold(strings.TrimSpace(firstLine), "YES"), nil
}
