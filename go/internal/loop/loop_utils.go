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
	"github.com/brokenalarms/ralph/internal/verify"
)

// isNewTask returns true when the next task differs from the last one stored
// in state. Prefers task ID comparison (stable across description edits);
// falls back to description when no ID is available.
func isNewTask(lastID, lastTask, taskID, taskDesc string) bool {
	if taskID != "" {
		return lastID != taskID
	}
	return lastTask != taskDesc
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

// isTransientGitHubError returns true for HTTP errors that are safe to retry:
// 401 Unauthorized (token briefly invalid) and 5xx server errors.
func isTransientGitHubError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{"401", "500", "502", "503", "504"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isOnline checks internet connectivity with a quick TCP dial.
func isOnline(timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", "api.anthropic.com:443", timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitForInternet blocks until internet connectivity is restored.
// Shows a single updating line in the terminal log, writes one summary
// line to the log file when restored. Returns false if context is cancelled.
// The logger parameter is the cross-module exception — see Loop.logger.
func waitForInternet(ctx context.Context, logger *logging.Logger, interval, checkTimeout time.Duration) bool {
	if isOnline(checkTimeout) {
		return true
	}

	start := time.Now()
	logger.Emit(logging.Opts{Level: logging.Warn}, "Internet unreachable — waiting for connectivity...")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if isOnline(checkTimeout) {
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
	workDir     string
	projectDir  string
	testTimeout time.Duration
	logger      *logging.Logger
}

// runVerifyBuild executes the verify-build script if configured. Uses the
// verify_build config/CLI value first; falls back to detecting a
// ralph:verify-build npm script or ralph-verify-build Makefile target
// (worktree then project root). Runs in the worktree (the task's working
// directory) with a timeout matching the test suite timeout. Returns empty
// string if the script passes or is not configured. Returns a build failure
// message (stdout+stderr) if the script exits non-zero.
func runVerifyBuild(ctx context.Context, p runVerifyBuildParams) string {
	if ctx.Err() != nil {
		return ""
	}

	var script string
	if p.verifyBuild != "" {
		script = p.verifyBuild
		p.logger.Emit(logging.Opts{Domain: "build"}, "Using verify_build config: %s", script)
	} else {
		tc := verify.DetectVerifyBuild(p.workDir, p.projectDir)
		if tc != nil {
			script = tc.Cmd + " " + strings.Join(tc.Args, " ")
			p.logger.Emit(logging.Opts{Domain: "build"}, "Detected ralph:verify-build script: %s (in %s)", script, tc.Dir)
		}
	}
	if script == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, p.testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = p.workDir
	p.logger.Emit(logging.Opts{Domain: "build"}, "Running verify-build: %s", script)
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
	postTask   string
	workDir    string
	projectDir string
	logger     *logging.Logger
}

// runPostTask executes the post-task script if configured. Uses the post_task
// config/CLI value first; falls back to detecting a ralph:post-task npm script
// or ralph-post-task Makefile target (worktree then project root). Runs in the
// worktree directory with RALPH_TASK_ID, RALPH_PR_NUMBER, and RALPH_MERGED env vars.
// Non-zero exit warns and continues.
func runPostTask(ctx context.Context, p runPostTaskParams, taskID string, prNumber int, merged bool) {
	var script string
	if p.postTask != "" {
		script = p.postTask
		p.logger.Emit(logging.Opts{Domain: "post-task"}, "Using post_task config: %s (in %s)", script, p.workDir)
	} else {
		tc := verify.DetectPostTask(p.workDir, p.projectDir)
		if tc != nil {
			script = tc.Cmd + " " + strings.Join(tc.Args, " ")
			p.logger.Emit(logging.Opts{Domain: "post-task"}, "Detected ralph:post-task script: %s (in %s)", script, tc.Dir)
		}
	}
	if script == "" {
		p.logger.Emit(logging.Opts{Domain: "post-task"}, "No post_task config and no ralph:post-task npm script or ralph-post-task Makefile target found — skipping post-task")
		return
	}
	prStr := strconv.Itoa(prNumber)
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = p.workDir
	cmd.Env = append(os.Environ(),
		"RALPH_TASK_ID="+taskID,
		"RALPH_PR_NUMBER="+prStr,
		"RALPH_MERGED="+fmt.Sprintf("%t", merged),
		"RALPH_PROJECT_DIR="+p.projectDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	p.logger.Emit(logging.Opts{Domain: "post-task"}, "Running %s (task=%s pr=%d merged=%t)", script, taskID, prNumber, merged)
	if err := cmd.Run(); err != nil {
		p.logger.Emit(logging.Opts{Domain: "post-task", Level: logging.Warn}, "Script exited with error: %v", err)
	}
}
