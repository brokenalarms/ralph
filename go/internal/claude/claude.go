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
	Complete      string // written when current task finishes
	CurrentTask   string // written when Claude picks up a task
	AllComplete   string // written when all tasks are done
	NoCodeNeeded  string // written when investigation confirms no code changes are needed
}

// DefaultSignalPaths returns signal file paths under the given ralph dir.
func DefaultSignalPaths(ralphDir string) SignalPaths {
	return SignalPaths{
		Complete:     filepath.Join(ralphDir, ".signal_complete"),
		CurrentTask:  filepath.Join(ralphDir, ".signal_current_task"),
		AllComplete:  filepath.Join(ralphDir, ".signal_all_complete"),
		NoCodeNeeded: filepath.Join(ralphDir, ".signal_no_code_needed"),
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
	Signals   SignalPaths
	PollInterval time.Duration

	// IdleTimeout kills the session if the raw log file hasn't been modified
	// for this duration. Zero disables idle detection.
	IdleTimeout time.Duration

	// MaxRunDuration is the hard wall-clock cap on total agent run time.
	// Zero disables the wall-clock backstop.
	MaxRunDuration time.Duration

	// IdleTimeoutProgress is the shorter idle timeout used once the agent has
	// produced content output (text, thinking, tool use) in the raw log.
	// Zero falls back to IdleTimeout.
	IdleTimeoutProgress time.Duration

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
	NoCodeNeeded       bool      // true if agent confirmed no code changes required (already fixed / not a bug)
	Compacted          bool      // true if the agent was killed because it triggered context compaction
	IdleTimeout        bool      // true if the session was killed due to idle timeout
	WallClockTimeout   bool      // true if the session was killed due to wall-clock max-run-duration
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
// The orchestrator owns all bd and gh operations — the agent must never
// interact with beads or GitHub directly. Git checkout/branch/push are
// blocked so sub-agents can't interfere with ralph's branch management.
//
// Patterns use leading wildcards (e.g. "Bash(*gh *)") to match commands
// that chain via && or ; after a cd prefix.
var IterationDisallowedTools = []string{
	"Bash(bd *)",
	"Bash(gh *)",
	"Bash(git checkout*)",
	"Bash(git branch*)",
	"Bash(git push*)",
	"Bash(*bd *)",
	"Bash(*gh *)",
	"Bash(*git checkout*)",
	"Bash(*git branch*)",
	"Bash(*git push*)",
	"Bash(rm *.beads*)",
	"Bash(rm *.ralph*)",
	"Bash(*rm *.beads*)",
	"Bash(*rm *.ralph*)",
	"Bash(pkill*dolt*)",
	"Bash(kill*dolt*)",
	"Bash(*pkill*dolt*)",
	"Bash(*kill*dolt*)",

	// Block direct SQL clients that could bypass bd and hit the dolt sql-server
	// via the MySQL protocol on 127.0.0.1. Both bare invocations and those
	// chained after a cd prefix (e.g. "cd /tmp && mysql ...").
	"Bash(mysql *)",
	"Bash(*mysql *)",
	"Bash(sqlite3 *)",
	"Bash(*sqlite3 *)",
	"Bash(psql *)",
	"Bash(*psql *)",

	// Block shell reads of raw .beads/ files (JSONL bead data). Agents must use
	// bd commands exclusively — direct file reads expose internal schema fields
	// the agent should never see.
	"Bash(cat *.beads*)",
	"Bash(*cat *.beads*)",
	"Bash(sed *.beads*)",
	"Bash(*sed *.beads*)",
	"Bash(awk *.beads*)",
	"Bash(*awk *.beads*)",
	"Bash(less *.beads*)",
	"Bash(*less *.beads*)",

	// Block Read-tool access to .beads/ paths. Requires claude CLI to support
	// Read path patterns in --disallowedTools; if not, the agent prompt in
	// execution-bd.md also explicitly forbids .beads/ reads as a fallback.
	"Read(*.beads*)",

	// Block absolute-path cd escapes. The agent's CWD is set to the worktree;
	// cd /abs/path would change it to the project root or another directory,
	// causing tools like tsx to pick up the wrong tsconfig and compile with the
	// wrong JSX factory. Relative cd (cd subdir) remains allowed.
	// The leading-wildcard form catches cd chained after another command prefix.
	"Bash(cd /Users/*)",
	"Bash(*cd /Users/*)",
	"Bash(cd /home/*)",
	"Bash(*cd /home/*)",
	"Bash(cd /tmp/*)",
	"Bash(*cd /tmp/*)",

	// Block orchestrator-owned ralph:* npm scripts. The loop manages the
	// ralph:verify and ralph:post-task lifecycle; agents must only run 'npm test'.
	// The leading-wildcard form catches invocations chained after a prefix.
	"Bash(npm run ralph:*)",
	"Bash(*npm run ralph:*)",

	// claude-mem MCP tools — inherited from the host session but the agent
	// wastes iterations on memory retrieval loops instead of writing code.
	"mcp__plugin_claude-mem_mcp-search__*",

	// claude-mem skills via the Skill tool — the Skill tool can invoke any
	// registered slash command. Blocking at the Skill layer prevents the agent
	// from loading skill instructions that then attempt the blocked MCP tools,
	// wasting the iteration on a doomed memory-retrieval loop.
	"Skill(claude-mem:*)",
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

	// ProjectDir is the repository root that agent spawns must NEVER chdir
	// into. Run() rejects RunConfig.WorkDir values equal to ProjectDir or
	// empty — the structural defense against "worktree leaked into main"
	// failures where a misconfigured workDir falls back to the project root.
	//
	// May be empty in tests and in direct claude.Runner construction; in
	// production it is set by agent.New() so all paths into Run() are guarded.
	ProjectDir string

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

// buildAgentEnv returns an environment for the agent subprocess with the
// project's .venv/bin prepended to PATH if that directory exists in workDir.
// Returns nil (inherit parent environment unchanged) when no .venv/bin is found.
func buildAgentEnv(workDir string) []string {
	venvBin := filepath.Join(workDir, ".venv", "bin")
	if info, err := os.Stat(venvBin); err != nil || !info.IsDir() {
		return nil
	}
	env := os.Environ()
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + venvBin + string(filepath.ListSeparator) + strings.TrimPrefix(entry, "PATH=")
			return env
		}
	}
	env = append(env, "PATH="+venvBin)
	return env
}

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

	// Check for Claude rate limit in the raw log output.
	if !result.SignalDetected && !result.IdleTimeout {
		if logData, err := os.ReadFile(cfg.RawLog); err == nil {
			if resetAt, found := ScanRawLogForRateLimit(string(logData), time.Now()); found {
				result.RateLimited = true
				result.ResetAt = resetAt
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Claude rate limit detected — resets at %s", resetAt.Format("3:04pm"))
			}
		}
	}

	return result, nil
}

// isContentActivity returns true if a raw-log line represents real assistant
// work (text, thinking, tool-use) that should reset the idle watchdog.
// Infrastructure events (rate_limit_event, system, ping, result, error, user
// message echoes) return false. Unparseable/plaintext lines return true so we
// err on the side of keeping the session alive.
func isContentActivity(line string) bool {
	if len(line) == 0 {
		return false
	}
	if line[0] != '{' {
		// Plaintext/stderr — conservative: treat as content activity.
		return true
	}

	var ev struct {
		Type  string `json:"type"`
		Delta *struct {
			Type       string  `json:"type"`
			Text       string  `json:"text"`
			Thinking   string  `json:"thinking"`
			StopReason *string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock *struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Message *struct {
			Role string `json:"role"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		// Unparseable JSON — conservative: treat as content activity.
		return true
	}

	switch ev.Type {
	case "content_block_delta":
		if ev.Delta == nil {
			return false
		}
		// text_delta and thinking_delta are content; input_json_delta is not.
		return ev.Delta.Type == "text_delta" || ev.Delta.Type == "thinking_delta" ||
			ev.Delta.Text != "" || ev.Delta.Thinking != ""
	case "content_block_start":
		if ev.ContentBlock == nil {
			return false
		}
		t := ev.ContentBlock.Type
		return t == "text" || t == "thinking"
	case "message_start":
		return ev.Message != nil && ev.Message.Role == "assistant"
	case "message_delta":
		return ev.Delta != nil && ev.Delta.StopReason != nil && *ev.Delta.StopReason != ""
	case "rate_limit_event", "system", "ping", "result", "error", "user":
		return false
	default:
		// Unknown event types — conservative: treat as content activity.
		return true
	}
}

// parseSystemStatusEvent returns the status value if the line is a system event
// with subtype=status (e.g. compacting, throttled). Returns "" for all other lines.
func parseSystemStatusEvent(line string) string {
	if len(line) == 0 || line[0] != '{' {
		return ""
	}
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}
	if ev.Type == "system" && ev.Subtype == "status" && ev.Status != "" {
		return ev.Status
	}
	return ""
}

// newLinesScan holds results from scanning newly appended raw log lines.
type newLinesScan struct {
	hasActivity             bool
	isCompacting            bool
	rlThrottled             bool
	rlResetAt               time.Time
	rlWarning               bool
	rlWarningIsUsingOverage bool
	rlType                  string
	rlUtilization           float64
	// Populated when a non-throttled, non-warning rate_limit_event is seen (status=allowed).
	rlAllowed               bool
	rlAllowedResetAt        time.Time
	rlAllowedIsUsingOverage bool
	rlAllowedUtilization    float64
	// True when any rate_limit_event in this scan had isUsingOverage=true.
	rlFirstOverage   bool
	rlOverageResetAt time.Time
	// System status events (type=system, subtype=status) detected in this scan.
	statusEvents []string
}

// scanNewLines reads new output appended to rawLog since *lastOffset.
// Activity is detected by file growth — immune to scanner buffer limits.
// Rate-limit events are detected by scanning the new lines with a 1MB
// scanner buffer; if an oversized line (e.g. a full file read as a single
// JSON line) exceeds the buffer, the scanner skips to EOF — rate-limit
// events are always small so the skip is safe.
func scanNewLines(rawLog string, lastOffset *int64) newLinesScan {
	info, err := os.Stat(rawLog)
	if err != nil {
		return newLinesScan{}
	}
	size := info.Size()

	grew := size > *lastOffset
	if !grew {
		return newLinesScan{}
	}

	f, err := os.Open(rawLog)
	if err != nil {
		// Can't open but file grew — report activity conservatively.
		*lastOffset = size
		return newLinesScan{hasActivity: true}
	}
	defer f.Close()

	if _, err := f.Seek(*lastOffset, 0); err != nil {
		*lastOffset = size
		return newLinesScan{hasActivity: true}
	}

	var result newLinesScan
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		*lastOffset += int64(len(line)) + 1 // +1 for '\n'
		if !result.hasActivity && isContentActivity(line) {
			result.hasActivity = true
		}
		resetAt, throttled, warning, isUsingOverage, rateLimitType, utilization, ok := ParseRateLimitEvent(line)
		if ok {
			if throttled && !result.rlThrottled {
				result.rlThrottled = true
				result.rlResetAt = resetAt
				result.rlType = rateLimitType
				result.rlUtilization = utilization
			}
			if warning && !result.rlWarning && !result.rlThrottled {
				result.rlWarning = true
				result.rlResetAt = resetAt
				result.rlType = rateLimitType
				result.rlUtilization = utilization
				result.rlWarningIsUsingOverage = isUsingOverage
			}
			if !throttled && !warning && !result.rlAllowed {
				result.rlAllowed = true
				result.rlAllowedResetAt = resetAt
				result.rlAllowedIsUsingOverage = isUsingOverage
				result.rlAllowedUtilization = utilization
			}
			if isUsingOverage && !result.rlFirstOverage {
				result.rlFirstOverage = true
				result.rlOverageResetAt = resetAt
			}
		}
		if status := parseSystemStatusEvent(line); status != "" {
			result.statusEvents = append(result.statusEvents, status)
			if status == "compacting" {
				result.isCompacting = true
			}
		}
	}
	// Scanner errored on oversized line — skip to EOF so we don't re-scan
	// it every tick. Conservatively report activity since we can't parse it.
	if scanner.Err() != nil {
		*lastOffset = size
		result.hasActivity = true
	}
	return result
}


// poll checks for process exit and signal files on a ticker.
func (r *Runner) poll(cmd *exec.Cmd, cfg RunConfig) Result {
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	taskLogged := false
	warningLogged := false
	activitySeen := false
	var warningResetsAt time.Time
	var warningOverage bool
	allowedTransitionLogged := false
	nowOverageLogged := false
	lastActivity := time.Now()
	runStart := time.Now()
	processDone := make(chan struct{})

	// Seed the log offset so we only scan lines written during this session.
	var logOffset int64
	if cfg.RawLog != "" {
		if info, err := os.Stat(cfg.RawLog); err == nil {
			logOffset = info.Size()
		}
	}

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
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Feedback signal detected — restarting agent")
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
						r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Feedback signal detected — restarting agent")
						return Result{FeedbackKill: true}
					}
				}
				return Result{}
			}

			// Scan new log lines for content activity and rate limit events.
			if cfg.RawLog != "" {
				scan := scanNewLines(cfg.RawLog, &logOffset)
				if scan.hasActivity {
					lastActivity = time.Now()
					activitySeen = true
				}
				for _, status := range scan.statusEvents {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model},
						"Claude system status: %s at %s", status, time.Since(runStart).Round(time.Millisecond))
				}
				if scan.isCompacting {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model},
						"Compaction detected — killing agent (context leak)")
					gracefulKill(cmd, processDone)
					return Result{Compacted: true}
				}
				if scan.rlThrottled {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model},
						"Claude rate limit throttled (%s %.0f%%) — resets at %s",
						scan.rlType, scan.rlUtilization*100, scan.rlResetAt.Format("3:04pm"))
					gracefulKill(cmd, processDone)
					return Result{RateLimited: true, ResetAt: scan.rlResetAt}
				}
				if scan.rlWarning && !warningLogged {
					warningLogged = true
					warningResetsAt = scan.rlResetAt
					warningOverage = scan.rlWarningIsUsingOverage
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Info, Model: cfg.Model},
						"rate limit warning: %s at %.0f%%, resets at %s",
						scan.rlType, scan.rlUtilization*100, scan.rlResetAt.Format("3:04pm"))
				}
				if warningLogged {
					if scan.rlAllowed && !allowedTransitionLogged {
						allowedTransitionLogged = true
						if scan.rlAllowedResetAt.Equal(warningResetsAt) {
							r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Info, Model: cfg.Model},
								"rate limit: allowed back in at %.0f%%, original reset confirmed",
								scan.rlAllowedUtilization*100)
						} else {
							r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model},
								"rate limit: consuming extended usage — original reset %s has advanced to %s",
								warningResetsAt.Format("3:04pm"), scan.rlAllowedResetAt.Format("3:04pm"))
						}
					}
					if scan.rlFirstOverage && !warningOverage && !nowOverageLogged {
						nowOverageLogged = true
						r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model},
							"rate limit: now using overage capacity (overage resets at %s)",
							scan.rlOverageResetAt.Format("3:04pm"))
					}
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
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Feedback signal detected — restarting agent")
					gracefulKill(cmd, processDone)
					return Result{FeedbackKill: true}
				}
			}

			// Detect no-code-needed signal: agent confirmed no code changes
			// required (bug already fixed, not a bug, etc.). Bypasses
			// OnSignal verification — no commit check applies.
			if cfg.Signals.NoCodeNeeded != "" && hasSignal(cfg.Signals.NoCodeNeeded) {
				summary := readFirstLine(cfg.Signals.NoCodeNeeded)
				if summary == "" {
					summary = "confirmed no code changes needed"
				}
				if cfg.TaskID != "" {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "%s: %s", cfg.TaskID, summary)
				} else {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "%s", summary)
				}
				waitForOutputSettle(cfg.RawLog, processDone)
				gracefulKill(cmd, processDone)
				return Result{
					SignalDetected: true,
					NoCodeNeeded:   true,
					TaskDesc:       readFirstLine(cfg.Signals.CurrentTask),
					Summary:        summary,
				}
			}

			// Detect completion.
			if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
				summary := readSignalSummary(cfg.Signals)
				if summary == "" {
					summary = "task done"
				}
				if cfg.TaskID != "" {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "%s completed: %s", cfg.TaskID, summary)
				} else {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "Completed: %s", summary)
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
						r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Verification rejected signal — agent continues")
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
				if cfg.IdleTimeoutProgress > 0 && activitySeen {
					timeout = cfg.IdleTimeoutProgress
				}
				idle := time.Since(lastActivity)
				if idle >= timeout {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Idle timeout (%s with no output) — killing session", timeout)
					gracefulKill(cmd, processDone)
					return Result{IdleTimeout: true}
				}
			}

			// Check wall-clock timeout — fires regardless of log activity.
			if cfg.MaxRunDuration > 0 && time.Since(runStart) >= cfg.MaxRunDuration {
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Wall-clock timeout (%s max run duration) — killing session", cfg.MaxRunDuration)
				gracefulKill(cmd, processDone)
				return Result{WallClockTimeout: true}
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
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Feedback signal detected — restarting agent")
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
	os.Remove(s.NoCodeNeeded)
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
