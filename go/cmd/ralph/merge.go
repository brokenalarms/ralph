package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

func handleMerge(sub config.Subcommand, log *logging.Logger) int {
	if hasHelpFlag(sub.Args) || len(sub.Args) == 0 {
		printMergeUsage()
		return 0
	}

	var prNumber string
	for _, arg := range sub.Args {
		if !strings.HasPrefix(arg, "-") && prNumber == "" {
			prNumber = arg
		}
	}
	if prNumber == "" {
		log.Emit(logging.Opts{Level: logging.Error}, "Usage: ralph merge <top-pr-number>")
		return 1
	}

	projectDir, _ := filepath.Abs(sub.Dir)
	if !git.IsGitRepo(projectDir) {
		log.Emit(logging.Opts{Level: logging.Error}, "Not a git repository: %s", projectDir)
		return 1
	}

	ralphDir := filepath.Join(projectDir, ".ralph")
	gm := git.New(git.Config{
		WorkDir:  projectDir,
		RalphDir: ralphDir,
		Logger:   log,
	})
	if !gm.GitHubAvailable() {
		log.Emit(logging.Opts{Level: logging.Error}, "gh CLI not available")
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	result, err := gm.MergeStack(ctx, git.MergeStackOpts{TopPR: prNumber})
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}
	_ = result
	return 0
}

func printMergeUsage() {
	fmt.Println(`Usage: ralph merge <top-pr-number>

Companion for ralph loop when --auto-merge is off. Give it any PR
in the stack — it finds the bottom, rebases the entire chain onto
main using --update-refs, force-pushes all branches, then merges
bottom-up waiting for CI between each merge.

Uses rebasecontinue.Run --auto for mechanical conflict resolution.

Examples:
  ralph merge 321  Merge the stack from bottom to PR #321
  ralph merge 314  Merge just PR #314 if it's the only open one`)
}
