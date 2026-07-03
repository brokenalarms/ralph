package git

import (
	"context"
	"strings"
	"testing"
)

// setupLeafTestRepo creates a real (non-stubbed) git repo with a linear
// history: base commit, then a descendant commit one step ahead of it.
// Returns the repo dir and both commit SHAs.
func setupLeafTestRepo(t *testing.T) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	run(t, "git", "-C", dir, "init", "-q", "-b", "main")
	run(t, "git", "-C", dir, "config", "user.name", "test")
	run(t, "git", "-C", dir, "config", "user.email", "test@test")
	run(t, "git", "-C", dir, "commit", "--allow-empty", "-q", "-m", "base")
	baseSHA = strings.TrimSpace(cmdOutput(t, "git", "-C", dir, "rev-parse", "HEAD"))
	run(t, "git", "-C", dir, "commit", "--allow-empty", "-q", "-m", "head")
	headSHA = strings.TrimSpace(cmdOutput(t, "git", "-C", dir, "rev-parse", "HEAD"))
	return dir, baseSHA, headSHA
}

// isAncestor must report true when the first commit is an ancestor of the
// second, and false when the relationship is reversed — the boolean form of
// `git merge-base --is-ancestor` that EnsureUpToDate/RemoteBranchIsOnMain/etc
// rely on to decide whether a rebase or reset is needed.
func TestIsAncestor(t *testing.T) {
	dir, baseSHA, headSHA := setupLeafTestRepo(t)
	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: dir, BaseBranch: "main", Logger: discardLog{}}, nil, withRunner(&execRunner{}))

	if !mgr.isAncestor(dir, baseSHA, headSHA) {
		t.Error("expected base to be an ancestor of head")
	}
	if mgr.isAncestor(dir, headSHA, baseSHA) {
		t.Error("expected head to NOT be an ancestor of base")
	}
}

// isAncestorCtx is the context-aware variant used by RebaseStack to check a
// worktree's stale-ness; it must agree with the plain isAncestor result.
func TestIsAncestorCtx(t *testing.T) {
	dir, baseSHA, headSHA := setupLeafTestRepo(t)
	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: dir, BaseBranch: "main", Logger: discardLog{}}, nil, withRunner(&execRunner{}))

	if !mgr.isAncestorCtx(context.Background(), dir, baseSHA, headSHA) {
		t.Error("expected base to be an ancestor of head")
	}
}

// revCount must parse `git rev-list --count` output into an int, and map
// empty output (e.g. an invalid range from a stubbed/failed command) to zero
// instead of leaving callers to hand-parse the string themselves.
func TestRevCount(t *testing.T) {
	dir, baseSHA, headSHA := setupLeafTestRepo(t)
	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: dir, BaseBranch: "main", Logger: discardLog{}}, nil, withRunner(&execRunner{}))

	if got := mgr.revCount(dir, baseSHA, headSHA); got != 1 {
		t.Errorf("expected 1 commit between base and head, got %d", got)
	}
	if got := mgr.revCount(dir, headSHA, headSHA); got != 0 {
		t.Errorf("expected 0 commits for an empty range, got %d", got)
	}
	if got := mgr.revCount(dir, "not-a-real-ref", headSHA); got != 0 {
		t.Errorf("expected empty/error output to map to 0, got %d", got)
	}
}

// hasCommitsAhead must report true when ref has commits not on base, and
// false when they're equal — used by BranchHasUnmergedWork/LocalBranchHasCommits
// to decide whether there's unpushed/unmerged work to flush.
func TestHasCommitsAhead(t *testing.T) {
	dir, baseSHA, headSHA := setupLeafTestRepo(t)
	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: dir, BaseBranch: "main", Logger: discardLog{}}, nil, withRunner(&execRunner{}))

	if !mgr.hasCommitsAhead(baseSHA, headSHA) {
		t.Error("expected head to have commits ahead of base")
	}
	if mgr.hasCommitsAhead(headSHA, headSHA) {
		t.Error("expected no commits ahead when base == ref")
	}
}
