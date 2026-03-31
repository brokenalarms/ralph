package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// Log is the logging interface used by Runner.
type Log interface {
	Emit(o logging.Opts, format string, args ...any)
	AgentLog(domain logging.Domain, format string, args ...any)
}

// SignalPaths holds the file paths used for inter-process signaling between
// the ralph loop and the Claude process.
type SignalPaths struct {
	Complete    string // written when current task finishes
	CurrentTask string // written when Claude picks up a task
	AllComplete string // written when all tasks are done
}

// DefaultSignalPaths returns signal file paths under the given ralph dir.
func DefaultSignalPaths(ralphDir string) SignalPaths {
	return SignalPaths{
		Complete:    filepath.Join(ralphDir, ".signal_complete"),
		CurrentTask: filepath.Join(ralphDir, ".signal_current_task"),
		AllComplete: filepath.Join(ralphDir, ".signal_all_complete"),
	}
}

// RunConfig configures a single Claude invocation.
type RunConfig struct {
	Ctx       context.Context // cancelled on SIGINT to stop the run
	WorkDir   string
	RalphDir  string
	Prompt    string
	RawLog    string // path to raw JSON log file
	LogFile   string // path to human-readable log file
	Quiet     bool   // suppress terminal streaming
	Signals   SignalPaths
	PollInterval time.Duration

	// IdleTimeout kills the session if the raw log file hasn't been modified
	// for this duration. Zero disables idle detection.
	IdleTimeout time.Duration

	// IdleTimeoutProgress is the shorter idle timeout used when the
	// HasProgress callback reports that work was already done in this
	// iteration (e.g. git diff exists). Zero falls back to IdleTimeout.
	IdleTimeoutProgress time.Duration

	// HasProgress returns true when the current iteration has already
	// produced observable work (new commits, unstaged changes). Used to
	// select the shorter IdleTimeoutProgress. May be nil.
	HasProgress func() bool

	// OnSignal is called when the agent writes a completion signal.
	// If it returns true, the signal is accepted (agent killed, result returned).
	// If false, the signal is rejected (signal file cleared, agent continues).
	// Used by the orchestrator to verify tests before accepting completion.
	// If nil, signals are always accepted (legacy behavior).
	OnSignal func(summary string) bool

	// FeedbackFile is the path where the orchestrator writes feedback for
	// the agent to read. Used to send test failure output back to the agent.
	FeedbackFile string

	// Verbose shows all tool calls in the stream log. When false,
	// low-value tools (Read, Bash, etc.) are hidden.
	Verbose bool

	// Model is the Claude model to use (e.g. "claude-sonnet-4-5-20241022").
	// If empty, the CLI default is used.
	Model string

	// TaskID is the bead/task identifier (e.g. "ralph-xyz") used in
	// completion and status log messages.
	TaskID string
}

// Result describes the outcome of a Claude run.
type Result struct {
	SignalDetected     bool      // true if a completion signal was found
	AllComplete        bool      // true if the all-complete signal was found
	IdleTimeout        bool      // true if the session was killed due to idle timeout
	FeedbackKill       bool      // true if killed because user feedback arrived
	RateLimited        bool      // true if Claude reported hitting its usage limit
	ResetAt            time.Time // when the rate limit resets (valid when RateLimited is true)
	OnSignalUsed       bool      // true if OnSignal callback was used for verification
	VerificationFailed bool      // true if signal was detected but OnSignal rejected it
	VerificationReason string    // reason OnSignal rejected the signal
	TaskDesc           string    // task description from the current-task signal
	Summary            string    // completion summary from signal file
}

// OnTaskDetected is called when a current-task signal file appears.
// The callback receives the task description read from the signal file.
type OnTaskDetected func(taskDesc string)

// IterationAllowedTools lists the Claude Code tools pre-approved for
// iteration mode. This replaces --dangerously-skip-permissions with
// explicit scoping — only these tools are available, and --add-dir
// restricts which directories they can access.
var IterationAllowedTools = []string{
	"Bash(*)",
	"Read",
	"Edit",
	"Write",
	"Glob",
	"Grep",
	"Agent",
	"Skill",
	"TodoWrite",
	"NotebookEdit",
	"WebFetch",
	"WebSearch",
	"ToolSearch",
}

// IterationDisallowedTools lists tools the agent must not use.
// The orchestrator owns bead lifecycle (bd close) and all remote git
// operations (push, PR creation). Git checkout/branch are blocked so
// sub-agents can't interfere with ralph's branch management.
var IterationDisallowedTools = []string{
	"Bash(bd close*)",
	"Bash(git checkout*)",
	"Bash(git branch*)",
	"Bash(git push*)",
	"Bash(gh pr create*)",
}

// CmdFactory builds the exec.Cmd that Run() will start. Receives the
// RunConfig so it can set WorkDir and pipe stdout/stderr to the raw log.
// If nil, Run() spawns "claude" with the standard iteration flags.
type CmdFactory func(cfg RunConfig, rawLog *os.File) *exec.Cmd

// Runner manages Claude process lifecycle: spawning, signal polling, and cleanup.
// It tracks active streaming goroutines (filter and tail) so they can be
// explicitly stopped before a new Run() call to prevent goroutine accumulation.
type Runner struct {
	Logger         Log
	OnTaskDetected OnTaskDetected
	CmdFactory     CmdFactory

	mu         sync.Mutex
	stdinPipe  io.WriteCloser
	filterStop chan struct{}
	filterDone <-chan struct{}
	tailStop   chan struct{}
	tailDone   <-chan struct{}
}

// StopStreaming stops and drains any active filter/tail goroutines. Safe to
// call multiple times or when no goroutines are active. Must be called before
// spawning a new Run() that shares the same raw log or log file to prevent
// duplicate writers.
func (r *Runner) StopStreaming() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.filterStop != nil {
		close(r.filterStop)
		<-r.filterDone
		r.filterStop = nil
		r.filterDone = nil
	}
	if r.tailStop != nil {
		close(r.tailStop)
		if r.tailDone != nil {
			<-r.tailDone
		}
		r.tailStop = nil
		r.tailDone = nil
	}
}

// InjectMessage writes a user message to the running agent's stdin pipe using
// the stream-json input format. Returns an error if no pipe is available (agent
// not running) or the write fails (pipe broken — agent exited).
func (r *Runner) InjectMessage(msg string) error {
	r.mu.Lock()
	pipe := r.stdinPipe
	r.mu.Unlock()
	if pipe == nil {
		return fmt.Errorf("no stdin pipe available")
	}
	payload := UserInputMessage(msg)
	_, err := fmt.Fprintln(pipe, payload)
	return err
}

// UserInputMessage builds a stream-json user message for injection into a
// running Claude process via stdin. The content is JSON-escaped to handle
// multi-line test output, error messages, etc.
func UserInputMessage(content string) string {
	escaped, _ := json.Marshal(content)
	return fmt.Sprintf(`{"type":"user_input_text","content":%s}`, string(escaped))
}

// Run spawns a Claude process, polls for signal files, and returns when the
// process exits or a completion signal is detected. Mirrors ralph.sh run_claude.
func (r *Runner) Run(cfg RunConfig) (Result, error) {
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

	var cmd *exec.Cmd
	if r.CmdFactory != nil {
		cmd = r.CmdFactory(cfg, rawLog)
	} else {
		args := []string{
			"--print", "--verbose",
			"--output-format", "stream-json",
			"--permission-mode", "bypassPermissions",
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
		cmd.Stdin = nil
		cmd.Stdout = rawLog
		cmd.Stderr = rawLog
		// Start in its own process group so we can signal it cleanly.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting claude: %w", err)
	}
	r.Logger.Emit(logging.Opts{Domain: logging.LLM}, "Claude started (PID: %d)", cmd.Process.Pid)

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
	if !cfg.Quiet && cfg.LogFile != "" {
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
		if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
			result.SignalDetected = true
			result.AllComplete = hasSignal(cfg.Signals.AllComplete)
			result.Summary = readSignalSummary(cfg.Signals)
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success}, "Task completed via signal")
		} else {
			r.Logger.Emit(logging.Opts{Domain: logging.LLM}, "Claude exited (no completion signal)")
		}
	}

	// Check for Claude rate limit in the raw log output.
	if !result.SignalDetected && !result.IdleTimeout {
		if logData, err := os.ReadFile(cfg.RawLog); err == nil {
			if resetAt, found := ScanRawLogForRateLimit(string(logData), time.Now()); found {
				result.RateLimited = true
				result.ResetAt = resetAt
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn}, "Claude rate limit detected — resets at %s", resetAt.Format("3:04pm"))
			}
		}
	}

	return result, nil
}

// poll checks for process exit and signal files on a ticker.
func (r *Runner) poll(cmd *exec.Cmd, cfg RunConfig) Result {
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	taskLogged := false
	lastActivity := time.Now()
	processDone := make(chan struct{})

	// Watch for process exit in a goroutine so we don't block on ticker.
	go func() {
		_ = cmd.Wait()
		close(processDone)
	}()

	for {
		select {
		case <-processDone:
			// Process exited — check for feedback signal before returning.
			if cfg.FeedbackFile != "" {
				if _, err := os.Stat(cfg.FeedbackFile); err == nil {
					os.Remove(cfg.FeedbackFile)
					r.Logger.Emit(logging.Opts{Domain: logging.LLM}, "Feedback signal detected — restarting agent")
					return Result{FeedbackKill: true}
				}
			}
			return Result{}

		case <-cfg.Ctx.Done():
			gracefulKill(cmd, processDone)
			return Result{}

		case <-ticker.C:
			// Check if process already exited (channel may not fire instantly).
			if !processAlive(cmd) {
				<-processDone
				if cfg.FeedbackFile != "" {
					if _, err := os.Stat(cfg.FeedbackFile); err == nil {
						os.Remove(cfg.FeedbackFile)
						r.Logger.Emit(logging.Opts{Domain: logging.LLM}, "Feedback signal detected — restarting agent")
						return Result{FeedbackKill: true}
					}
				}
				return Result{}
			}

			// Track raw log activity for idle detection.
			if info, err := os.Stat(cfg.RawLog); err == nil {
				if info.ModTime().After(lastActivity) {
					lastActivity = info.ModTime()
				}
			}

			// Detect task pickup — the stream formatter already emits
			// "[signal] current_task: ..." in real-time, so we only set the
			// flag and fire the callback here, no duplicate log line.
			if !taskLogged && hasSignal(cfg.Signals.CurrentTask) {
				desc := readFirstLine(cfg.Signals.CurrentTask)
				if desc != "" {
					taskLogged = true
					if r.OnTaskDetected != nil {
						r.OnTaskDetected(desc)
					}
				}
			}

			// Check for user feedback signal FIRST — it's a deliberate
			// human override and must take priority over completion signals.
			// Content is already on the bead via bd update --append-notes.
			if cfg.FeedbackFile != "" {
				if _, err := os.Stat(cfg.FeedbackFile); err == nil {
					os.Remove(cfg.FeedbackFile)
					r.Logger.Emit(logging.Opts{Domain: logging.LLM}, "Feedback signal detected — restarting agent")
					gracefulKill(cmd, processDone)
					return Result{FeedbackKill: true}
				}
			}

			// Detect completion.
			if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
				summary := readSignalSummary(cfg.Signals)
				if summary == "" {
					summary = "task done"
				}
				if cfg.TaskID != "" {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success}, "%s completed: %s", cfg.TaskID, summary)
				} else {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success}, "Completed: %s", summary)
				}

				// If OnSignal is set, let the orchestrator verify before accepting.
				// Run in a goroutine so we can still detect feedback during
				// long-running verification (tests, LLM review, fix agents).
				if cfg.OnSignal != nil {
					accepted := r.runOnSignalWithFeedbackWatch(cfg, cmd, processDone, summary)
					if accepted == feedbackInterrupt {
						return Result{FeedbackKill: true}
					}
					if accepted == signalRejected {
						r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn}, "Verification rejected signal — agent continues")
						clearSignals(cfg.Signals)
						continue
					}
				}

				// Wait for the agent to finish its final output before killing.
				// The signal file is often written before the agent's completion
				// message appears in the log; killing immediately truncates it.
				waitForOutputSettle(cfg.RawLog, processDone)

				gracefulKill(cmd, processDone)

				return Result{
					SignalDetected: true,
					AllComplete:    hasSignal(cfg.Signals.AllComplete),
					OnSignalUsed:   cfg.OnSignal != nil,
					TaskDesc:       readFirstLine(cfg.Signals.CurrentTask),
					Summary:        summary,
				}
			}

			// Check idle timeout.
			if cfg.IdleTimeout > 0 {
				timeout := cfg.IdleTimeout
				if cfg.IdleTimeoutProgress > 0 && cfg.HasProgress != nil && cfg.HasProgress() {
					timeout = cfg.IdleTimeoutProgress
				}
				idle := time.Since(lastActivity)
				if idle >= timeout {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn}, "Idle timeout (%s with no output) — killing session", timeout)
					gracefulKill(cmd, processDone)
					return Result{IdleTimeout: true}
				}
			}
		}
	}
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

// onSignalOutcome describes the result of running OnSignal with feedback monitoring.
type onSignalOutcome int

const (
	signalAccepted    onSignalOutcome = iota // OnSignal returned true
	signalRejected                           // OnSignal returned false
	feedbackInterrupt                        // feedback file appeared during OnSignal
)

// runOnSignalWithFeedbackWatch runs the OnSignal callback in a goroutine while
// polling for feedback. If a feedback file appears while OnSignal is executing
// (e.g. during test suite, LLM verification, or fix agent execution), the
// agent is killed immediately and feedbackInterrupt is returned.
func (r *Runner) runOnSignalWithFeedbackWatch(cfg RunConfig, cmd *exec.Cmd, processDone <-chan struct{}, summary string) onSignalOutcome {
	if cfg.FeedbackFile == "" {
		if cfg.OnSignal(summary) {
			return signalAccepted
		}
		return signalRejected
	}

	done := make(chan bool, 1)
	go func() {
		done <- cfg.OnSignal(summary)
	}()

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case accepted := <-done:
			if accepted {
				return signalAccepted
			}
			return signalRejected
		case <-ticker.C:
			if _, err := os.Stat(cfg.FeedbackFile); err == nil {
				os.Remove(cfg.FeedbackFile)
				r.Logger.Emit(logging.Opts{Domain: logging.LLM}, "Feedback signal detected — restarting agent")
				gracefulKill(cmd, processDone)
				return feedbackInterrupt
			}
		}
	}
}

// --- Signal file helpers ---

// ClearSignals removes all signal files. Exported for use by the main loop.
func ClearSignals(s SignalPaths) {
	clearSignals(s)
}

func clearSignals(s SignalPaths) {
	os.Remove(s.Complete)
	os.Remove(s.CurrentTask)
	os.Remove(s.AllComplete)
}

func hasSignal(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readFirstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		return stripJSONFragment(line)
	}
	return ""
}

// stripJSONFragment removes JSON bleed-through from signal file content.
// Signal summaries are plain text; anything starting with '{' is garbage,
// and trailing '{...' fragments are trimmed.
func stripJSONFragment(s string) string {
	if s == "" || s[0] == '{' {
		return ""
	}
	if idx := strings.IndexByte(s, '{'); idx > 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

func readSignalSummary(s SignalPaths) string {
	if hasSignal(s.AllComplete) {
		if v := readFirstLine(s.AllComplete); v != "" {
			return v
		}
	}
	if hasSignal(s.Complete) {
		return readFirstLine(s.Complete)
	}
	return ""
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
