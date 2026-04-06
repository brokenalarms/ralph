package loop

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verify"
)

// isNewTask returns true when the next task differs from the last one stored
// in state. Prefers task ID comparison (stable across description edits);
// falls back to description when no ID is available.
func isNewTask(st *state.Store, taskID, taskDesc string) bool {
	if taskID != "" {
		lastID, _ := st.Read("last_task_id")
		return lastID != taskID
	}
	lastTask, _ := st.Read("last_task")
	return lastTask != taskDesc
}

// persistCompletedTask writes a completed task entry to state.json so
// ralph-task can verify tasks weren't falsely closed and setStackHead can
// find unmerged branches for stacking.
func persistCompletedTask(st *state.Store, logger *logging.Logger, taskID string, merged bool) {
	if taskID == "" {
		return
	}
	if err := st.AddCompletedTask(taskID, merged); err != nil {
		logger.Emit(logging.Opts{Domain: "state", Level: logging.Warn}, "AddCompletedTask: %v", err)
	}
}

func fileLineCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

func readLogFrom(path string, startLine int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := 0
	offset := 0
	for i, b := range data {
		if b == '\n' {
			lines++
			if lines >= startLine {
				offset = i + 1
				break
			}
		}
	}
	if offset >= len(data) {
		return ""
	}
	return string(data[offset:])
}

// skipTask sets the task back to open in bd, records the reason as a
// comment, and adds the ID to both the backend's in-memory skip set
// and the state.json skipped_tasks list so it stays excluded from
// future selection.
func skipTask(backend tasks.Backend, st *state.Store, logger *logging.Logger, id, reason string) {
	if id == "" {
		return
	}
	logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Skipping task %s: %s", id, reason)
	if err := backend.SkipTask(id, reason); err != nil {
		logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to skip task %s in backend: %v", id, err)
	}
	if st != nil {
		if err := st.AddSkippedTask(id); err != nil {
			logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to persist skip for %s: %v", id, err)
		}
		skipped, _ := st.GetSkippedTasks()
		backend.SetSkippedIDs(skipped)
	}
}

// isOnline checks internet connectivity with a quick DNS lookup.
func isOnline() bool {
	conn, err := net.DialTimeout("tcp", "api.anthropic.com:443", 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitForInternet blocks until internet connectivity is restored.
// Shows a single updating line in the terminal log, writes one summary
// line to the log file when restored. Returns false if context is cancelled.
func waitForInternet(ctx context.Context, logger *logging.Logger) bool {
	if isOnline() {
		return true
	}

	start := time.Now()
	logger.Emit(logging.Opts{Level: logging.Warn}, "Internet unreachable — waiting for connectivity...")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if isOnline() {
				elapsed := time.Since(start).Truncate(time.Second)
				logger.Emit(logging.Opts{Level: logging.Success}, "Internet restored after %s", elapsed)
				return true
			}
			elapsed := time.Since(start).Truncate(time.Second)
			logger.Emit(logging.Opts{}, "Internet still unreachable (%s elapsed)", elapsed)
		}
	}
}

// checkGitHubConnectivity verifies that GitHub is reachable via gh api /rate_limit.
// Uses a 10-second context timeout to avoid hanging on blocked connections.
func checkGitHubConnectivity(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "/rate_limit")
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("GitHub connectivity check timed out after 10s")
		}
		return fmt.Errorf("gh api /rate_limit: %w", err)
	}
	return nil
}

type runVerifyBuildParams struct {
	verifyBuild string
	projectDir  string
	logger      *logging.Logger
}

// runVerifyBuild executes the --verify-build script if configured. Runs in
// the project directory with a timeout matching the test suite timeout.
// Returns empty string if the script passes or is not configured.
// Returns a build failure message (stdout+stderr) if the script exits non-zero.
func runVerifyBuild(ctx context.Context, p runVerifyBuildParams) string {
	if p.verifyBuild == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, verify.TestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", p.verifyBuild)
	cmd.Dir = p.projectDir
	p.logger.Emit(logging.Opts{Domain: "build"}, "Running verify-build: %s", p.verifyBuild)
	out, err := cmd.CombinedOutput()
	if err == nil {
		p.logger.Emit(logging.Opts{Domain: "build", Level: logging.Success}, "Build health check passed")
		return ""
	}
	output := strings.TrimSpace(string(out))
	p.logger.Emit(logging.Opts{Domain: "build", Level: logging.Warn}, "Build health check failed: %v", err)
	msg := "\n- BUILD IS BROKEN. Fix the build before working on your task. Do not start the task until the build is healthy."
	if output != "" {
		msg += "\n  Build failure output:\n  " + strings.ReplaceAll(output, "\n", "\n  ")
	}
	return msg
}

type runPostTaskParams struct {
	postTask    string
	worktreeDir string
	projectDir  string
	logger      *logging.Logger
}

// runPostTask executes the post-task script if configured. Checks for a
// ralph:post-task npm script or ralph-post-task Makefile target first (worktree
// then project root); falls back to the --post-task CLI flag. Runs in the
// project directory with RALPH_TASK_ID, RALPH_PR_NUMBER, and RALPH_MERGED env vars.
// Non-zero exit warns and continues.
func runPostTask(ctx context.Context, p runPostTaskParams, taskID string, prNumber int, merged bool) {
	tc := verify.DetectPostTask(p.worktreeDir, p.projectDir)
	var script string
	var scriptDir string
	if tc != nil {
		script = tc.Cmd + " " + strings.Join(tc.Args, " ")
		scriptDir = tc.Dir
		p.logger.Emit(logging.Opts{Domain: "post-task"}, "Detected ralph:post-task script: %s (in %s)", script, scriptDir)
	} else if p.postTask != "" {
		script = p.postTask
		scriptDir = p.projectDir
		p.logger.Emit(logging.Opts{Domain: "post-task"}, "Using --post-task CLI flag: %s (in %s)", script, scriptDir)
	} else {
		p.logger.Emit(logging.Opts{Domain: "post-task"}, "No ralph:post-task script found in package.json and no --post-task CLI flag — skipping post-task")
		return
	}
	prStr := strconv.Itoa(prNumber)
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = p.projectDir
	cmd.Env = append(os.Environ(),
		"RALPH_TASK_ID="+taskID,
		"RALPH_PR_NUMBER="+prStr,
		"RALPH_MERGED="+fmt.Sprintf("%t", merged),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	p.logger.Emit(logging.Opts{Domain: "post-task"}, "Running %s (task=%s pr=%d merged=%t)", script, taskID, prNumber, merged)
	if err := cmd.Run(); err != nil {
		p.logger.Emit(logging.Opts{Domain: "post-task", Level: logging.Warn}, "Script exited with error: %v", err)
	}
}
