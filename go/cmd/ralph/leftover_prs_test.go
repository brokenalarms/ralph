package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

// No leftover open ralph PRs: checkLeftoverRalphPRs returns immediately with
// no prompt output and no state mutation — startup is unchanged (AC #6).
func TestCheckLeftoverRalphPRs_NoLeftover_NoPromptNoChange(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{})
	var out strings.Builder
	log := logging.NewWithWriter(&out)

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader(""), log)

	if !ok {
		t.Fatal("expected ok=true when no leftover PRs")
	}
	if adopt != "" {
		t.Errorf("expected no branch adopted, got %q", adopt)
	}
	if out.Len() != 0 {
		t.Errorf("expected no prompt output, got: %q", out.String())
	}
	if got := gm.(git.StubInspector).GetAdoptedStackBranch(); got != "" {
		t.Errorf("expected no adopted branch set on git.Ops, got %q", got)
	}
}

// Non-interactive stdin (no TTY) never blocks on the prompt: it defaults to
// fresh-from-main and logs the leftover PRs as a warning (AC #5).
func TestCheckLeftoverRalphPRs_NonInteractive_DefaultsFreshWithWarning(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", State: git.PRStateOpen}},
		},
	})
	var out strings.Builder
	var logBuf strings.Builder
	log := logging.NewWithWriter(&logBuf)

	// interactive=false — must not read r at all (nil-safe reader unused).
	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", false, false, &out, strings.NewReader(""), log)

	if !ok {
		t.Fatal("expected ok=true (non-interactive never blocks)")
	}
	if adopt != "" {
		t.Errorf("expected fresh-from-main (no adopt), got %q", adopt)
	}
	if !strings.Contains(logBuf.String(), "ralph/tabi-uael") {
		t.Errorf("expected leftover PR branch logged as a warning, got: %q", logBuf.String())
	}
	if got := gm.(git.StubInspector).GetAdoptedStackBranch(); got != "" {
		t.Errorf("expected no adopted branch set, got %q", got)
	}
}

// Answering "y" adopts the newest (highest-numbered) leftover PR's branch as
// the stack head (AC #2) — verifies the incident regression: a leftover PR
// branch diverged from main must still be selectable.
func TestCheckLeftoverRalphPRs_AnswerY_AdoptsNewestBranch(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs: []git.StubPR{
				{Number: 1200, Branch: "ralph/older-task", State: git.PRStateOpen},
				{Number: 1241, Branch: "ralph/tabi-uael", State: git.PRStateOpen},
			},
		},
	})
	var out strings.Builder
	log := logging.NewWithWriter(&strings.Builder{})

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader("y\n"), log)

	if !ok {
		t.Fatal("expected ok=true")
	}
	if adopt != "ralph/tabi-uael" {
		t.Errorf("expected newest leftover branch ralph/tabi-uael adopted, got %q", adopt)
	}
	if got := gm.(git.StubInspector).GetAdoptedStackBranch(); got != "ralph/tabi-uael" {
		t.Errorf("expected git.Ops.SetAdoptedStackBranch called with ralph/tabi-uael, got %q", got)
	}
	if !strings.Contains(out.String(), "#1241") || !strings.Contains(out.String(), "#1200") {
		t.Errorf("expected both leftover PRs listed in the prompt, got: %q", out.String())
	}
}

// Answering "n" proceeds from origin/main exactly as today — no branch is
// adopted (AC #3).
func TestCheckLeftoverRalphPRs_AnswerN_FreshFromMain(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", State: git.PRStateOpen}},
		},
	})
	var out strings.Builder
	var logBuf strings.Builder
	log := logging.NewWithWriter(&logBuf)

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader("n\n"), log)

	if !ok {
		t.Fatal("expected ok=true")
	}
	if adopt != "" {
		t.Errorf("expected no branch adopted on 'n', got %q", adopt)
	}
	if got := gm.(git.StubInspector).GetAdoptedStackBranch(); got != "" {
		t.Errorf("expected git.Ops.SetAdoptedStackBranch not called, got %q", got)
	}
	if !strings.Contains(logBuf.String(), "remain open and unmerged") {
		t.Errorf("expected a log noting the leftover PRs remain open, got: %q", logBuf.String())
	}
}

// The prompt states the consequence of both answers before asking (y/n) so
// the user can make an informed choice — the leftover-PR fate ('n' leaves
// #1241 open and stale) must not be revealed only after answering.
func TestCheckLeftoverRalphPRs_PromptStatesBothOutcomesBeforeAsking(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", State: git.PRStateOpen}},
		},
	})
	var out strings.Builder
	log := logging.NewWithWriter(&strings.Builder{})

	checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader("n\n"), log)

	prompt := out.String()
	if !strings.Contains(prompt, "y — continue the stack on top of #1241") {
		t.Errorf("expected prompt to state the 'y' outcome, got: %q", prompt)
	}
	if !strings.Contains(prompt, "n — start fresh from origin/main") || !strings.Contains(prompt, "#1241 stays open and may go stale as main advances") {
		t.Errorf("expected prompt to state the 'n' outcome (leftover PR stays open and may go stale) before asking, got: %q", prompt)
	}
	if !strings.Contains(prompt, "Ctrl-C — quit; run 'ralph merge 1241' to drain the stack first") {
		t.Errorf("expected prompt to state the Ctrl-C escape, got: %q", prompt)
	}
	if !strings.Contains(prompt, "Continue the stack? (y/n)") {
		t.Errorf("expected the final question to ask 'Continue the stack? (y/n)', got: %q", prompt)
	}
	// The outcome lines must appear before the question is asked.
	yIdx := strings.Index(prompt, "y — continue the stack")
	qIdx := strings.Index(prompt, "Continue the stack? (y/n)")
	if yIdx == -1 || qIdx == -1 || yIdx > qIdx {
		t.Errorf("expected outcome lines before the (y/n) question, got: %q", prompt)
	}
}

// Ctrl-C (context cancelled while the prompt is pending) exits cleanly: no
// branch is adopted and the caller is signaled to stop before any branch
// setup (AC #4).
func TestCheckLeftoverRalphPRs_CtrlC_ExitsCleanly(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", State: git.PRStateOpen}},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	log := logging.NewWithWriter(&strings.Builder{})

	adopt, ok := checkLeftoverRalphPRs(ctx, gm, "/project", true, false, &out, strings.NewReader("y\n"), log)

	if ok {
		t.Fatal("expected ok=false on Ctrl-C")
	}
	if adopt != "" {
		t.Errorf("expected no branch adopted on Ctrl-C, got %q", adopt)
	}
	if got := gm.(git.StubInspector).GetAdoptedStackBranch(); got != "" {
		t.Errorf("expected no state mutation on Ctrl-C, got adopted branch %q", got)
	}
	if got := gm.(git.StubInspector).GetBranchForTaskCalls(); got != 0 {
		t.Errorf("expected no branch setup on Ctrl-C, got %d BranchForTask calls", got)
	}
}

// Regardless of outcome, checkLeftoverRalphPRs never performs branch setup
// itself — it only ever reads PRs and, on "y", records the adopted branch.
// This is the structural guarantee behind "no branch setup happens before
// the answer" (AC #1): the caller in runMain invokes this before gm.Init,
// and this function cannot have created a branch or worktree by the time it
// returns.
func TestCheckLeftoverRalphPRs_NeverPerformsBranchSetup(t *testing.T) {
	for _, answer := range []string{"y\n", "n\n"} {
		gm := git.NewStub(git.StubRepoConfig{
			GitHub: git.StubGitHubConfig{
				Available: true,
				PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", State: git.PRStateOpen}},
			},
		})
		var out strings.Builder
		log := logging.NewWithWriter(&strings.Builder{})

		checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader(answer), log)

		inspector := gm.(git.StubInspector)
		if got := inspector.GetBranchForTaskCalls(); got != 0 {
			t.Errorf("answer %q: expected 0 BranchForTask calls, got %d", answer, got)
		}
		if got := inspector.GetRemoveWorktreeCalls(); got != 0 {
			t.Errorf("answer %q: expected 0 RemoveWorktree calls, got %d", answer, got)
		}
	}
}

// A lone leftover ralph PR based directly on the default branch is one the
// loop already committed to merging — the bead was closed "verified, merge
// pending" and only a transient failure (e.g. a GitHub 503) left the PR
// open. Startup retries that merge before offering adoption: the PR lands,
// main is updated, no prompt is shown, and the run starts fresh from main.
// The merge is attempted before any branch setup, so the retry can never
// race a worktree built on the unmerged branch.
func TestCheckLeftoverRalphPRs_LoneMergeablePR_MergedBeforeAnyPrompt(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		BaseBranch:       "main",
		MergeStackResult: git.MergeStackResult{MergedCount: 1, TotalPRs: 1},
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", Base: "main", State: git.PRStateOpen}},
		},
	})
	var out strings.Builder
	log := logging.NewWithWriter(&strings.Builder{})

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader("y\n"), log)

	if !ok {
		t.Fatal("expected ok=true after a successful leftover merge")
	}
	if adopt != "" {
		t.Errorf("expected fresh-from-main after the leftover merged, got adopt=%q", adopt)
	}
	if out.Len() != 0 {
		t.Errorf("expected no adoption prompt after a successful merge, got: %q", out.String())
	}
	inspector := gm.(git.StubInspector)
	if got := inspector.GetMergeStackCalls(); got != 1 {
		t.Errorf("expected 1 merge attempt for the lone leftover PR, got %d", got)
	}
	if got := inspector.GetMergeStackTopPR(); got != "1241" {
		t.Errorf("expected the leftover PR #1241 to be merged, got TopPR %q", got)
	}
	if got := inspector.GetAdoptedStackBranch(); got != "" {
		t.Errorf("expected no branch adopted after the leftover merged, got %q", got)
	}
	if got := inspector.GetBranchForTaskCalls(); got != 0 {
		t.Errorf("expected the merge to precede all branch setup, got %d BranchForTask calls", got)
	}
	if got := inspector.GetRemoveWorktreeCalls(); got != 0 {
		t.Errorf("expected the merge to precede all worktree setup, got %d RemoveWorktree calls", got)
	}
}

// When the retried merge fails (CI red, branch protection, another transient
// GitHub error), the user still gets the existing adopt-or-fresh prompt —
// the retry is a best-effort shortcut, never a new failure mode.
func TestCheckLeftoverRalphPRs_LoneMergeFails_FallsBackToPrompt(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		BaseBranch:    "main",
		MergeStackErr: errors.New("CI failed on PR #1241"),
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", Base: "main", State: git.PRStateOpen}},
		},
	})
	var out strings.Builder
	log := logging.NewWithWriter(&strings.Builder{})

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader("y\n"), log)

	if !ok {
		t.Fatal("expected ok=true")
	}
	if got := gm.(git.StubInspector).GetMergeStackCalls(); got != 1 {
		t.Errorf("expected the merge to have been attempted once, got %d", got)
	}
	if adopt != "ralph/tabi-uael" {
		t.Errorf("expected the prompt's 'y' answer to still adopt the branch, got %q", adopt)
	}
	if !strings.Contains(out.String(), "Continue the stack? (y/n)") {
		t.Errorf("expected the adoption prompt after a failed merge, got: %q", out.String())
	}
}

// A merge call that reports zero PRs merged (no error, nothing landed) is a
// failure, not a success — the PR is still open, so the prompt must appear.
func TestCheckLeftoverRalphPRs_LoneMergeMergedNothing_FallsBackToPrompt(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		BaseBranch:       "main",
		MergeStackResult: git.MergeStackResult{MergedCount: 0, TotalPRs: 1},
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", Base: "main", State: git.PRStateOpen}},
		},
	})
	var out strings.Builder
	log := logging.NewWithWriter(&strings.Builder{})

	adopt, _ := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader("n\n"), log)

	if adopt != "" {
		t.Errorf("expected 'n' to start fresh, got %q", adopt)
	}
	if !strings.Contains(out.String(), "Continue the stack? (y/n)") {
		t.Errorf("expected the adoption prompt when nothing merged, got: %q", out.String())
	}
}

// Two or more leftover PRs form a stack: merging the bottom one here would
// strand the rest (the tabi #669 cascade). No merge is attempted — the
// prompt, and 'ralph merge', remain the only drain path.
func TestCheckLeftoverRalphPRs_StackOfTwo_NoMergeAttempted(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		BaseBranch:       "main",
		MergeStackResult: git.MergeStackResult{MergedCount: 2, TotalPRs: 2},
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs: []git.StubPR{
				{Number: 1240, Branch: "ralph/older-task", Base: "main", State: git.PRStateOpen},
				{Number: 1241, Branch: "ralph/tabi-uael", Base: "ralph/older-task", State: git.PRStateOpen},
			},
		},
	})
	var out strings.Builder
	log := logging.NewWithWriter(&strings.Builder{})

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader("y\n"), log)

	if !ok {
		t.Fatal("expected ok=true")
	}
	if got := gm.(git.StubInspector).GetMergeStackCalls(); got != 0 {
		t.Errorf("expected no merge attempt for a stack of 2, got %d", got)
	}
	if adopt != "ralph/tabi-uael" {
		t.Errorf("expected the prompt to drive adoption unchanged, got %q", adopt)
	}
	if !strings.Contains(out.String(), "ralph merge 1241") {
		t.Errorf("expected the prompt to still point at 'ralph merge' for the stack, got: %q", out.String())
	}
}

// A lone leftover PR whose base is another branch is the top of a stack
// whose bottom has already gone (branch deleted, PR closed) — merging it
// would land work onto a base the loop never verified. Only PRs based
// directly on the default branch are retried.
func TestCheckLeftoverRalphPRs_LonePRNotBasedOnDefaultBranch_NoMergeAttempted(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		BaseBranch:       "main",
		MergeStackResult: git.MergeStackResult{MergedCount: 1, TotalPRs: 1},
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", Base: "ralph/gone-task", State: git.PRStateOpen}},
		},
	})
	var out strings.Builder
	log := logging.NewWithWriter(&strings.Builder{})

	checkLeftoverRalphPRs(context.Background(), gm, "/project", true, false, &out, strings.NewReader("n\n"), log)

	if got := gm.(git.StubInspector).GetMergeStackCalls(); got != 0 {
		t.Errorf("expected no merge attempt for a PR based off a non-default branch, got %d", got)
	}
	if !strings.Contains(out.String(), "Continue the stack? (y/n)") {
		t.Errorf("expected the adoption prompt, got: %q", out.String())
	}
}

// Non-interactive startup (no TTY) also retries the lone leftover merge —
// the supervisor case is exactly where a stranded PR would otherwise sit
// unmerged forever, since no one is there to answer the prompt.
func TestCheckLeftoverRalphPRs_NonInteractive_LoneMergeablePRMerged(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		BaseBranch:       "main",
		MergeStackResult: git.MergeStackResult{MergedCount: 1, TotalPRs: 1},
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 1241, Branch: "ralph/tabi-uael", Base: "main", State: git.PRStateOpen}},
		},
	})
	var out strings.Builder
	var logBuf strings.Builder
	log := logging.NewWithWriter(&logBuf)

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", false, false, &out, strings.NewReader(""), log)

	if !ok || adopt != "" {
		t.Fatalf("expected fresh-from-main after merge, got adopt=%q ok=%v", adopt, ok)
	}
	if got := gm.(git.StubInspector).GetMergeStackCalls(); got != 1 {
		t.Errorf("expected the merge to be retried without a TTY, got %d attempts", got)
	}
	if strings.Contains(logBuf.String(), "remain open and unmerged") {
		t.Errorf("expected no leftover warning after the PR merged, got: %q", logBuf.String())
	}
}
