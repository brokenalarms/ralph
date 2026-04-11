package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
)

const refactorCheckInterval = 5
const refactorLookbackCommits = 10

// maybeRefactor runs an adaptive refactoring iteration if enough tasks have
// been completed and the LLM recommends it. The caller is responsible for
// ensuring the rate limit allows the call before invoking this method.
func (l *Loop) maybeRefactor(ctx context.Context, sessionCount int) error {
	if !l.cfg.Refactor {
		return nil
	}

	if sessionCount == 0 || sessionCount%refactorCheckInterval != 0 {
		return nil
	}

	recentFiles := l.git.RecentChangedFiles(refactorLookbackCommits)
	if recentFiles == "" {
		return nil
	}

	archSpec := readArchSpec(l.git.GetWorkDir())

	shouldRefactor, err := llmShouldRefactor(ctx, l.runner.Query, l.git.GetWorkDir(), archSpec, recentFiles)
	if err != nil {
		return fmt.Errorf("refactor check: %w", err)
	}

	if !shouldRefactor {
		l.logger.Emit(logging.Opts{Domain: "refactor"}, "LLM says no refactoring needed — skipping")
		return nil
	}

	l.logger.Phase("--- Adaptive refactor (LLM recommended) ---")

	ralphDir := l.cfg.Dirs.RalphDir
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

	if !l.waitForRate(ctx) {
		return nil
	}

	rawLogPath := filepath.Join(ralphDir, "raw.log")
	_, err = l.runner.Run(claude.RunConfig{
		WorkDir:      l.git.GetWorkDir(),
		RalphDir:     ralphDir,
		Prompt:       refactorPrompt,
		RawLog:       rawLogPath,
		LogFile:      filepath.Join(ralphDir, "loop.log"),
		Quiet:        l.cfg.Quiet,
		Verbose:      l.cfg.Verbose,
		Signals:      l.signals,
		PollInterval: 2 * time.Second,
	})
	l.limiter.Increment()

	l.logger.Emit(logging.Opts{Level: logging.Success}, "Refactor iteration complete")

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

func llmShouldRefactor(ctx context.Context, queryFn func(context.Context, string, string, string) (string, error), workDir string, archSpec, recentFiles string) (bool, error) {
	if queryFn == nil {
		return false, fmt.Errorf("no query function available")
	}

	refactorPrompt := "You are deciding whether a codebase needs refactoring.\n\n"
	if archSpec != "" {
		refactorPrompt += "## Architecture spec\n" + archSpec + "\n\n"
	}
	refactorPrompt += "## Recently changed files\n" + recentFiles + "\n\n"
	refactorPrompt += "Based on the recently changed files and the architecture spec, does this codebase need refactoring right now?\n"
	refactorPrompt += "Consider: code duplication, unclear naming, files growing too large, architectural drift from the spec, dead code.\n"
	refactorPrompt += "Reply with exactly YES or NO on the first line, followed by a brief explanation."

	response, err := queryFn(ctx, workDir, refactorPrompt, "")
	if err != nil {
		return false, err
	}

	firstLine := strings.SplitN(strings.TrimSpace(response), "\n", 2)[0]
	return strings.EqualFold(strings.TrimSpace(firstLine), "YES"), nil
}
