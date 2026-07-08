package main

import (
	"context"
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

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, &out, strings.NewReader(""), log)

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
	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", false, &out, strings.NewReader(""), log)

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

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, &out, strings.NewReader("y\n"), log)

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

	adopt, ok := checkLeftoverRalphPRs(context.Background(), gm, "/project", true, &out, strings.NewReader("n\n"), log)

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

	checkLeftoverRalphPRs(context.Background(), gm, "/project", true, &out, strings.NewReader("n\n"), log)

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

	adopt, ok := checkLeftoverRalphPRs(ctx, gm, "/project", true, &out, strings.NewReader("y\n"), log)

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

		checkLeftoverRalphPRs(context.Background(), gm, "/project", true, &out, strings.NewReader(answer), log)

		inspector := gm.(git.StubInspector)
		if got := inspector.GetBranchForTaskCalls(); got != 0 {
			t.Errorf("answer %q: expected 0 BranchForTask calls, got %d", answer, got)
		}
		if got := inspector.GetRemoveWorktreeCalls(); got != 0 {
			t.Errorf("answer %q: expected 0 RemoveWorktree calls, got %d", answer, got)
		}
	}
}
