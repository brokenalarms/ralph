package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fnRunner is a Runner whose behavior is supplied per-test as a function,
// useful for injecting side effects around specific git calls.
type fnRunner struct {
	RunFn func(ctx context.Context, dir string, args ...string) (string, error)
}

func (f *fnRunner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	return f.RunFn(ctx, dir, args...)
}

// initPruneTestRepo creates a plain (non-bare) local repo with one commit,
// suitable as the projectDir for PruneOrphanedWorktrees tests.
func initPruneTestRepo(t *testing.T) string {
	t.Helper()
	project := filepath.Join(t.TempDir(), "project")
	run(t, "git", "init", "-q", "-b", "main", project)
	run(t, "git", "-C", project, "config", "user.name", "test")
	run(t, "git", "-C", project, "config", "user.email", "test@test")
	run(t, "git", "-C", project, "commit", "-q", "--allow-empty", "-m", "init")
	return project
}

// A worktree that becomes registered via `git worktree add` after
// PruneOrphanedWorktrees has already scanned the worktree root directory,
// but before it decides whether the directory is orphaned, must survive.
// This reproduces the incident where a concurrent `ralph task` worktree was
// deleted because it was absent from a stale `git worktree list` snapshot.
func TestPruneOrphanedWorktrees_RaceRegisteredAfterScanNotRemoved(t *testing.T) {
	project := initPruneTestRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktreeRoot: %v", err)
	}

	// racingDir exists as an empty directory when PruneOrphanedWorktrees reads
	// worktreeRoot, but isn't yet a registered worktree — mirroring the window
	// in which a concurrent process's `git worktree add` is still landing.
	racingDir := filepath.Join(worktreeRoot, "racing-worktree")
	if err := os.MkdirAll(racingDir, 0o755); err != nil {
		t.Fatalf("mkdir racingDir: %v", err)
	}

	real := &execRunner{}
	injected := false
	runner := &fnRunner{RunFn: func(ctx context.Context, dir string, args ...string) (string, error) {
		if !injected && len(args) >= 3 && args[0] == "worktree" && args[1] == "list" && args[2] == "--porcelain" {
			injected = true
			// Capture the snapshot as it looked at the instant this call was
			// issued (racingDir absent), THEN let the concurrent process's
			// registration land — reproducing a snapshot that goes stale
			// between being read and being acted on, regardless of how fast
			// the removal decision that consumes it runs afterward.
			staleOutput, staleErr := real.Run(ctx, dir, args...)
			if _, err := real.Run(context.Background(), dir, "worktree", "add", racingDir, "-b", "ralph/racing-task"); err != nil {
				t.Fatalf("simulate concurrent worktree add: %v", err)
			}
			return staleOutput, staleErr
		}
		return real.Run(ctx, dir, args...)
	}}

	r := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, Logger: &testLog{}},
		nil,
		withRunner(runner),
	)

	r.PruneOrphanedWorktrees()

	if _, err := os.Stat(racingDir); err != nil {
		t.Fatalf("expected racing worktree directory to survive, got: %v", err)
	}
}

// A directory with a valid `.git` gitdir link must be treated as live and
// skipped even when a `git worktree list` query fails to report it — the
// gitdir link is the authoritative, race-safe check, not the list output.
func TestPruneOrphanedWorktrees_ValidGitdirLinkSkippedEvenWhenListOmitsIt(t *testing.T) {
	project := initPruneTestRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktreeRoot: %v", err)
	}

	liveDir := filepath.Join(worktreeRoot, "live-worktree")
	run(t, "git", "-C", project, "worktree", "add", liveDir, "-b", "ralph/live-task")

	real := &execRunner{}
	runner := &fnRunner{RunFn: func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "worktree" && args[1] == "list" && args[2] == "--porcelain" {
			return "", nil
		}
		return real.Run(ctx, dir, args...)
	}}

	r := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, Logger: &testLog{}},
		nil,
		withRunner(runner),
	)

	r.PruneOrphanedWorktrees()

	if _, err := os.Stat(liveDir); err != nil {
		t.Fatalf("expected live worktree directory to survive, got: %v", err)
	}
}

// A directory with no worktree registration and no valid .git gitdir link is
// genuinely orphaned and must still be removed — the race fix must not
// disable orphan cleanup entirely.
func TestPruneOrphanedWorktrees_GenuinelyOrphanedDirRemoved(t *testing.T) {
	project := initPruneTestRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	orphanDir := filepath.Join(worktreeRoot, "orphan")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphanDir: %v", err)
	}

	r := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)

	r.PruneOrphanedWorktrees()

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("expected orphaned directory to be removed, stat err: %v", err)
	}
}
