package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

// stdinIsTTY reports whether os.Stdin is an interactive terminal. A prompt
// must never block on a non-TTY stdin (redirected from /dev/null, a pipe, or
// a supervisor with no attached terminal) — see checkLeftoverRalphPRs.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readLineFromReader reads a line from r, returning early with ctx.Err() if
// ctx is cancelled (e.g. Ctrl-C) before or during the read. Mirrors
// readLineCtx but takes an explicit io.Reader so callers can inject a
// non-stdin source in tests.
func readLineFromReader(ctx context.Context, r io.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ch := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(r).ReadString('\n')
		ch <- line
	}()
	select {
	case line := <-ch:
		return line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// checkLeftoverRalphPRs detects ralph-authored PRs left open by prior runs
// and, when any exist, resolves whether the new run should continue the
// stack on top of the newest one or start fresh from origin/main. A lone
// leftover PR based on the default branch is merged first (see
// retryLoneLeftoverMerge), leaving nothing to resolve.
//
// Runs exactly once, at loop startup, before any git branch setup
// (gm.Init/SetupWorktree) — the caller must invoke this before gm.Init so
// the "no branch setup happens before the answer" guarantee holds. This
// function never creates the loop's task branch or worktree: it calls the
// read-only gm.ListOpenPRs, the lone-PR gm.MergeStack retry (which works in
// its own temp worktree), and, on an explicit "y" answer,
// gm.SetAdoptedStackBranch.
//
// Returns adopt = the branch to continue the stack on ("" for fresh-from-main)
// and ok = false when the user Ctrl-C'd the prompt, signaling the caller to
// exit cleanly without any further branch setup or state mutation.
//
// interactive selects whether to prompt (true only for a real TTY) or
// silently default to fresh-from-main, logging the leftover PRs as a
// warning, when stdin is not interactive — this also covers the common case
// of ralph running under an orchestrator/supervisor with no attached
// terminal.
func checkLeftoverRalphPRs(ctx context.Context, gm git.Ops, projectDir string, interactive, adminMergeOnCIInfraFailure bool, w io.Writer, r io.Reader, log *logging.Logger) (adopt string, ok bool) {
	openPRs, err := gm.ListOpenPRs(ctx, projectDir)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "Leftover PR check: failed to list PRs: %v", err)
		return "", true
	}
	leftover := git.LeftoverRalphPRs(openPRs)
	if len(leftover) == 0 {
		return "", true
	}

	if retryLoneLeftoverMerge(ctx, gm, leftover, adminMergeOnCIInfraFailure, log) {
		return "", true
	}

	if !interactive {
		log.Emit(logging.Opts{Level: logging.Warn}, "%d ralph PR(s) left open from a prior run — non-interactive stdin, starting fresh from main:", len(leftover))
		for _, pr := range leftover {
			log.Emit(logging.Opts{Level: logging.Warn}, "  #%d %s", pr.Number, pr.Head)
		}
		return "", true
	}

	top := leftover[0]
	fmt.Fprintf(w, "\n%s[ralph v%s (go)]%s %d ralph PR(s) left open from a prior run:\n", logging.Yellow, config.Version, logging.Reset, len(leftover))
	for _, pr := range leftover {
		fmt.Fprintf(w, "  #%d %s\n", pr.Number, pr.Head)
	}
	fmt.Fprintf(w, "  y — continue the stack on top of #%d (new work branches from it)\n", top.Number)
	fmt.Fprintf(w, "  n — start fresh from origin/main (#%d stays open and may go stale as main advances)\n", top.Number)
	fmt.Fprintf(w, "  Ctrl-C — quit; run 'ralph merge %d' to drain the stack first\n", top.Number)
	fmt.Fprintf(w, "Continue the stack? (y/n) ")

	answer, err := readLineFromReader(ctx, r)
	if err != nil {
		return "", false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "y" || answer == "yes" {
		gm.SetAdoptedStackBranch(top.Head)
		return top.Head, true
	}
	log.Emit(logging.Opts{Level: logging.Warn}, "Starting fresh from origin/main — %d leftover ralph PR(s) remain open and unmerged", len(leftover))
	return "", true
}

// retryLoneLeftoverMerge re-attempts the merge of a single leftover PR based
// directly on the default branch, returning true when it lands.
//
// Such a PR is one the loop already committed to merging: completeTask closes
// the bead as "verified — merge pending" on the promise the PR will land, and
// only a transient failure (a GitHub 503, say) leaves it open. Nothing else
// ever revisits it — the bead is closed, so the loop never re-selects it — and
// the stack then grows on an unmerged bottom until a human runs 'ralph merge'.
// Retrying here re-runs the sanctioned CI-aware squash-merge rather than
// adding a new merge path.
//
// Chains of two or more are deliberately left alone: merging the bottom of a
// stack outside MergeStack's bottom-up walk strands its descendants, so the
// adopt prompt and 'ralph merge' stay the only drain path for stacks.
func retryLoneLeftoverMerge(ctx context.Context, gm git.Ops, leftover []git.PRInfo, adminMergeOnCIInfraFailure bool, log *logging.Logger) bool {
	if len(leftover) != 1 {
		return false
	}
	pr := leftover[0]
	baseBranch := gm.DetectDefaultBranch()
	if pr.Base != baseBranch {
		return false
	}

	log.Emit(logging.Opts{}, "Leftover PR #%d (%s) is based on %s and was left unmerged by a prior run — retrying the merge", pr.Number, pr.Head, baseBranch)
	result, err := gm.MergeStack(ctx, git.MergeStackOpts{
		TopPR:                      strconv.Itoa(pr.Number),
		AdminMergeOnCIInfraFailure: adminMergeOnCIInfraFailure,
	})
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "Leftover PR #%d merge retry failed: %v", pr.Number, err)
		return false
	}
	if result.MergedCount == 0 {
		log.Emit(logging.Opts{Level: logging.Warn}, "Leftover PR #%d was not merged", pr.Number)
		return false
	}
	log.Emit(logging.Opts{Level: logging.Success}, "Leftover PR #%d merged — starting fresh from %s", pr.Number, baseBranch)
	return true
}
