package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
	"github.com/brokenalarms/ralph/internal/ratelimit"
)

const refactorCheckInterval = 5
const refactorLookbackCommits = 10

type maybeRefactorParams struct {
	cfg          Config
	git          git.GitOps
	limiter      *ratelimit.Limiter
	runner       claudeRunner
	logger       *logging.Logger
	signals      claude.SignalPaths
	queryFn      func(ctx context.Context, workDir, prompt, model string) (string, error)
	sessionCount int
}

func maybeRefactor(ctx context.Context, p maybeRefactorParams) error {
	if !p.cfg.Refactor {
		return nil
	}

	if p.sessionCount == 0 || p.sessionCount%refactorCheckInterval != 0 {
		return nil
	}

	recentFiles := p.git.RecentChangedFiles(refactorLookbackCommits)
	if recentFiles == "" {
		return nil
	}

	archSpec := readArchSpec(p.git.GetWorkDir())

	shouldRefactor, err := llmShouldRefactor(ctx, llmShouldRefactorParams{
		queryFn: p.queryFn,
		workDir: p.git.GetWorkDir(),
	}, archSpec, recentFiles)
	if err != nil {
		return fmt.Errorf("refactor check: %w", err)
	}

	if !shouldRefactor {
		p.logger.Emit(logging.Opts{Domain: "refactor"}, "LLM says no refactoring needed — skipping")
		return nil
	}

	p.logger.Phase("--- Adaptive refactor (LLM recommended) ---")

	refactorPrompt, err := prompt.BuildRefactorPrompt(prompt.Vars{
		PromptsDir:       p.cfg.Dirs.PromptsDir,
		WorkDir:          p.git.GetWorkDir(),
		SignalToken:      p.signals.Complete,
		CurrentTaskToken: p.signals.CurrentTask,
		AllCompleteToken: p.signals.AllComplete,
	}, recentFiles)
	if err != nil {
		return fmt.Errorf("building refactor prompt: %w", err)
	}

	if !p.limiter.Allowed() {
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn}, "Rate limit hit before refactor — waiting for reset")
		if err := p.limiter.WaitForReset(ctx, func(secs int) {
			p.logger.Emit(logging.Opts{Domain: logging.LLM}, "Rate limit: %ds until reset", secs)
		}); err != nil {
			return err
		}
	}

	rawLogPath := filepath.Join(p.cfg.Dirs.RalphDir, "raw.log")
	_, err = p.runner.Run(claude.RunConfig{
		WorkDir:      p.git.GetWorkDir(),
		RalphDir:     p.cfg.Dirs.RalphDir,
		Prompt:       refactorPrompt,
		RawLog:       rawLogPath,
		LogFile:      filepath.Join(p.cfg.Dirs.RalphDir, "loop.log"),
		Quiet:        p.cfg.Quiet,
		Verbose:      p.cfg.Verbose,
		Signals:      p.signals,
		PollInterval: 2 * time.Second,
	})
	p.limiter.Increment()

	p.logger.Emit(logging.Opts{Level: logging.Success}, "Refactor iteration complete")

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

type llmShouldRefactorParams struct {
	queryFn func(ctx context.Context, workDir, prompt, model string) (string, error)
	workDir string
}

func llmShouldRefactor(ctx context.Context, p llmShouldRefactorParams, archSpec, recentFiles string) (bool, error) {
	if p.queryFn == nil {
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

	response, err := p.queryFn(ctx, p.workDir, refactorPrompt, "")
	if err != nil {
		return false, err
	}

	firstLine := strings.SplitN(strings.TrimSpace(response), "\n", 2)[0]
	return strings.EqualFold(strings.TrimSpace(firstLine), "YES"), nil
}
