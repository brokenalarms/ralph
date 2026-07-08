package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// Run spawns a Claude process, polls for signal files, and returns when the
// process exits or a completion signal is detected. Mirrors ralph.sh run_claude.
func (r *Runner) Run(cfg RunConfig) (Result, error) {
	// Worktree invariant: refuse to spawn an agent in the project root or
	// with an empty cwd. See Runner.ProjectDir for rationale.
	if cfg.WorkDir == "" {
		return Result{}, fmt.Errorf("agent spawn refused: WorkDir is empty (worktree setup must have failed)")
	}
	if r.ProjectDir != "" && cfg.WorkDir == r.ProjectDir {
		return Result{}, fmt.Errorf("agent spawn refused: WorkDir == ProjectDir (%s) — worktree setup must have failed", cfg.WorkDir)
	}
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}

	clearSignals(cfg.Signals)

	rawLog, err := os.OpenFile(cfg.RawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Result{}, fmt.Errorf("opening raw log: %w", err)
	}
	defer rawLog.Close()

	r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model},
		"Invoking claude with prompt size %dB, workDir %s, model %s", len(cfg.Prompt), cfg.WorkDir, cfg.Model)

	var cmd *exec.Cmd
	if r.CmdFactory != nil {
		cmd = r.CmdFactory(cfg, rawLog)
	} else {
		args := []string{
			"--print", "--verbose",
			"--output-format", "stream-json",
			"--permission-mode", "bypassPermissions",
			// Hermetic context: load only repo-committed project settings/CLAUDE.md,
			// never the user-global ~/.claude config. Combined with the
			// CLAUDE_CODE_DISABLE_AUTO_MEMORY env in buildAgentEnv, the agent sees
			// only the orchestrator's prompt + project repo context.
			"--setting-sources", "project",
			"--add-dir", cfg.WorkDir,
			"--add-dir", cfg.RalphDir,
			"--allowedTools", strings.Join(IterationAllowedTools, ","),
			"--disallowedTools", strings.Join(IterationDisallowedTools, ","),
		}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		args = append(args, "-p", cfg.Prompt)
		cmd = exec.Command("claude", args...)
		cmd.Dir = cfg.WorkDir
		cmd.Env = buildAgentEnv(cfg.WorkDir)
		cmd.Stdin = nil
		cmd.Stdout = rawLog
		cmd.Stderr = rawLog
		// Start in its own process group so we can signal it cleanly.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	versionCtx, versionCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer versionCancel()
	if out, err := exec.CommandContext(versionCtx, "claude", "--version").Output(); err != nil {
		r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "claude --version failed: %v", err)
	} else {
		r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Using claude binary version %s", strings.TrimSpace(string(out)))
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting claude: %w", err)
	}
	r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Claude started (PID: %d)", cmd.Process.Pid)

	// Record stream start time for the filter.
	_ = os.WriteFile(filepath.Join(cfg.RalphDir, ".stream-start"),
		[]byte(fmt.Sprintf("%d", time.Now().Unix())), 0o644)

	// Stop any goroutines left from a previous Run() to prevent accumulation.
	r.StopStreaming()

	// Start stream filter BEFORE Claude begins writing so it catches all
	// output (mirrors the bash fix for the tail -f race).
	filterStop := make(chan struct{})
	filterDone := r.startStreamFilter(cfg, filterStop)

	// Start optional terminal tail (pure Go — no external process to orphan).
	tailStop := make(chan struct{})
	var tailDone <-chan struct{}
	if cfg.LogFile != "" {
		tailDone = startTailGoroutine(cfg.LogFile, tailStop)
	}

	// Track goroutines at the Runner level so StopStreaming() can drain them
	// if a nested Run() (e.g. verification agent inside OnSignal) needs to
	// start before this Run() returns.
	r.mu.Lock()
	r.filterStop = filterStop
	r.filterDone = filterDone
	r.tailStop = tailStop
	r.tailDone = tailDone
	r.mu.Unlock()

	// Poll for signals or process exit.
	result := r.poll(cmd, cfg)

	// Stop streaming goroutines and drain remaining output.
	r.StopStreaming()

	// Close stdin pipe so the agent sees EOF. Safe to call after process exit.
	r.mu.Lock()
	if r.stdinPipe != nil {
		r.stdinPipe.Close()
		r.stdinPipe = nil
	}
	r.mu.Unlock()

	// Final signal check — Claude may have written a signal just before exiting.
	// Skip if feedback was detected — the agent should restart, not complete.
	if !result.SignalDetected && !result.FeedbackKill {
		if cfg.Signals.NoCodeNeeded != "" && hasSignal(cfg.Signals.NoCodeNeeded) {
			result.SignalDetected = true
			result.NoCodeNeeded = true
			result.Summary = readFirstLine(cfg.Signals.NoCodeNeeded)
			if result.Summary == "" {
				result.Summary = "confirmed no code changes needed"
			}
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "Task completed via no-code-needed signal")
		} else if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
			result.SignalDetected = true
			result.AllComplete = hasSignal(cfg.Signals.AllComplete)
			result.Summary = readSignalSummary(cfg.Signals)
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "Task completed via signal")
		} else {
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Claude exited (no completion signal)")
		}
	}

	// Check for Claude rate limit in the raw log output. Runs even when the
	// session was classified as an idle timeout — a server-side throttle can
	// stall output long enough for the idle watchdog to fire before the
	// throttle evidence (JSON rate_limit_event or plaintext "hit your limit")
	// is scanned in real time, so this fallback reclassifies the result
	// instead of letting the throttle masquerade as an idle timeout.
	if !result.SignalDetected {
		if logData, err := os.ReadFile(cfg.RawLog); err == nil {
			if resetAt, found := ScanRawLogForRateLimit(string(logData), time.Now()); found {
				result.RateLimited = true
				result.ResetAt = resetAt
				result.IdleTimeout = false
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Claude rate limit detected — resets at %s", resetAt.Format("3:04pm"))
			}
		}
	}

	// With auto-compaction disabled, an oversized task fails hard at the 200K
	// context window instead of being silently summarized. Route that failure
	// through the same too-big handling as a real-time "compacting" event.
	if !result.SignalDetected && !result.Compacted && !result.IdleTimeout && !result.RateLimited {
		if logData, err := os.ReadFile(cfg.RawLog); err == nil {
			if containsPromptTooLong(string(logData)) {
				result.Compacted = true
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Context limit (200K) exceeded — task too big")
			}
		}
	}

	return result, nil
}

// containsPromptTooLong reports whether the raw agent output contains
// Claude Code's hard-limit failure message. This fires only when
// auto-compaction is disabled (DISABLE_AUTO_COMPACT=1) and the agent's
// context exceeds the 200K model window — with compaction enabled, the
// agent is summarized instead and this string never appears.
func containsPromptTooLong(logContent string) bool {
	return strings.Contains(strings.ToLower(logContent), "prompt is too long")
}

// startStreamFilter launches a goroutine that tails the raw log and writes
// filtered human-readable output to LogFile. Uses a pure Go implementation
// (no external processes) to avoid orphaned tail/jq/perl/sed processes that
// accumulated across iterations. The goroutine keeps reading until stop is
// closed, then drains any remaining content. Returns a channel that closes
// when the filter is done.
func (r *Runner) startStreamFilter(cfg RunConfig, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	if cfg.LogFile == "" {
		close(done)
		return done
	}

	go func() {
		defer close(done)
		filterStreamJSON(cfg.RawLog, cfg.LogFile, cfg.WorkDir, cfg.Verbose, stop)
	}()

	return done
}

// --- Process lifecycle helpers ---

func processAlive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	// Signal 0 checks if process exists without sending a signal.
	return cmd.Process.Signal(syscall.Signal(0)) == nil
}

// waitForOutputSettle waits until the raw log file has not been modified for
// a settle period, indicating the agent has finished writing. Caps the total
// wait to prevent hanging if the agent keeps writing indefinitely.
func waitForOutputSettle(rawLogPath string, processDone <-chan struct{}) {
	const (
		settleTime = 2 * time.Second
		maxWait    = 10 * time.Second
		checkEvery = 250 * time.Millisecond
	)

	deadline := time.Now().Add(maxWait)
	lastMod := time.Now()

	// Snapshot current mtime as the baseline.
	if info, err := os.Stat(rawLogPath); err == nil {
		lastMod = info.ModTime()
	}

	for time.Now().Before(deadline) {
		select {
		case <-processDone:
			return
		case <-time.After(checkEvery):
		}

		if info, err := os.Stat(rawLogPath); err == nil {
			if info.ModTime().After(lastMod) {
				lastMod = info.ModTime()
			}
		}

		if time.Since(lastMod) >= settleTime {
			return
		}
	}
}

// gracefulKill sends SIGTERM, waits briefly, then SIGKILL if needed.
// processDone should be a channel that closes when cmd.Wait() completes
// (from the poll goroutine) to avoid concurrent Wait() calls.
func gracefulKill(cmd *exec.Cmd, processDone <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-processDone:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// stopProcessGroup kills cmd and all its descendants. Uses multiple
// mechanisms for reliability: pkill -P (by parent PID, matching bash
// ralph's cleanup pattern), process group kill, and direct kill.
func stopProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid

	// Kill children by parent PID first (matches bash ralph's pkill -P
	// pattern). This is more reliable on macOS than process group killing
	// alone for bash pipeline children (tail, jq, perl, sed).
	_ = exec.Command("pkill", "-9", "-P", fmt.Sprintf("%d", pid)).Run()

	// Kill the entire process group (negative PID). Catches any processes
	// still in the group that pkill -P may have missed.
	_ = syscall.Kill(-pid, syscall.SIGKILL)

	// Direct kill as final fallback.
	_ = cmd.Process.Kill()

	_ = cmd.Wait()
}
