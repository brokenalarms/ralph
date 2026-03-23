package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// Log is the logging interface used by Runner.
type Log interface {
	Log(format string, args ...any)
	Success(format string, args ...any)
	Warn(format string, args ...any)
	Error(format string, args ...any)
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

	// TaskID is the bead/task identifier (e.g. "ralph-xyz") used in
	// completion and status log messages.
	TaskID string
}

// Result describes the outcome of a Claude run.
type Result struct {
	SignalDetected     bool   // true if a completion signal was found
	AllComplete        bool   // true if the all-complete signal was found
	IdleTimeout        bool   // true if the session was killed due to idle timeout
	OnSignalUsed       bool   // true if OnSignal callback was used for verification
	VerificationFailed bool   // true if signal was detected but OnSignal rejected it
	VerificationReason string // reason OnSignal rejected the signal
	TaskDesc           string // task description from the current-task signal
	Summary            string // completion summary from signal file
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
// The orchestrator owns bead close — the agent must not close tasks.
// Git checkout/branch are blocked so sub-agents can't check out ralph's
// branches, which would prevent RecreateFromMain from deleting them.
var IterationDisallowedTools = []string{
	"Bash(bd close*)",
	"Bash(git checkout*)",
	"Bash(git branch*)",
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
			"--add-dir", cfg.WorkDir,
			"--add-dir", cfg.RalphDir,
			"--allowedTools", strings.Join(IterationAllowedTools, ","),
			"--disallowedTools", strings.Join(IterationDisallowedTools, ","),
			"-p", cfg.Prompt,
		}
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
	r.Logger.Log("Claude started (PID: %d)", cmd.Process.Pid)

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

	// Final signal check — Claude may have written a signal just before exiting.
	if !result.SignalDetected {
		if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
			result.SignalDetected = true
			result.AllComplete = hasSignal(cfg.Signals.AllComplete)
			result.Summary = readSignalSummary(cfg.Signals)
			r.Logger.Success("Task completed via signal")
		} else {
			r.Logger.Log("Claude exited (no completion signal)")
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
			// Process exited on its own — return, let caller do final signal check.
			return Result{}

		case <-cfg.Ctx.Done():
			gracefulKill(cmd, processDone)
			return Result{}

		case <-ticker.C:
			// Check if process already exited (channel may not fire instantly).
			if !processAlive(cmd) {
				<-processDone
				return Result{}
			}

			// Track raw log activity for idle detection.
			if info, err := os.Stat(cfg.RawLog); err == nil {
				if info.ModTime().After(lastActivity) {
					lastActivity = info.ModTime()
				}
			}

			// Detect task pickup.
			if !taskLogged && hasSignal(cfg.Signals.CurrentTask) {
				desc := readFirstLine(cfg.Signals.CurrentTask)
				if desc != "" {
					if cfg.TaskID != "" {
						r.Logger.Log("Working on: %s (%s)", desc, cfg.TaskID)
					} else {
						r.Logger.Log("Working on: %s", desc)
					}
					taskLogged = true
					if r.OnTaskDetected != nil {
						r.OnTaskDetected(desc)
					}
				}
			}

			// Detect completion.
			if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
				summary := readSignalSummary(cfg.Signals)
				if summary == "" {
					summary = "task done"
				}
				if cfg.TaskID != "" {
					r.Logger.Success("%s completed: %s", cfg.TaskID, summary)
				} else {
					r.Logger.Success("Completed: %s", summary)
				}

				// If OnSignal is set, let the orchestrator verify before accepting.
				if cfg.OnSignal != nil {
					if !cfg.OnSignal(summary) {
						// Verification failed — clear signal, let agent continue.
						r.Logger.Warn("Verification rejected signal — agent continues")
						clearSignals(cfg.Signals)
						continue
					}
				}

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
					r.Logger.Warn("Idle timeout (%s with no output) — killing session", timeout)
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
		filterStreamJSON(cfg.RawLog, cfg.LogFile, stop)
	}()

	return done
}

// filterStreamJSON tails the raw log file from its current end, extracting
// human-readable content from Claude's stream-json format into logPath.
// It keeps reading until stop is closed, then drains any final output.
func filterStreamJSON(rawLogPath, logPath string, stop <-chan struct{}) {
	logOut, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer logOut.Close()

	f, err := os.Open(rawLogPath)
	if err != nil {
		return
	}
	defer f.Close()

	// Start from end of file (like tail -f -n 0) so we only see new output.
	if _, err := f.Seek(0, 2); err != nil {
		return
	}

	var remainder string
	buf := make([]byte, 64*1024)

	processChunk := func(data string) string {
		for {
			idx := strings.IndexByte(data, '\n')
			if idx < 0 {
				return data
			}
			line := data[:idx]
			data = data[idx+1:]
			if text := extractStreamText(line); text != "" {
				for _, tl := range strings.Split(text, "\n") {
					if tl != "" {
						fmt.Fprintf(logOut, "%s\n", FormatStreamLine("[agent] "+tl))
					}
				}
			}
		}
	}

	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			remainder = processChunk(remainder + string(buf[:n]))
		}

		if readErr != nil || n == 0 {
			select {
			case <-stop:
				// Final drain — read any bytes written after the last Read.
				for {
					n2, _ := f.Read(buf)
					if n2 == 0 {
						break
					}
					remainder = processChunk(remainder + string(buf[:n2]))
				}
				return
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
}

// streamEvent is the top-level envelope for Claude's stream-json output.
type streamEvent struct {
	Type    string       `json:"type"`
	Subtype string       `json:"subtype"`
	Message *streamMsg   `json:"message"`
	Delta   *streamDelta `json:"delta"`
	Error   string       `json:"error"`
}

type streamMsg struct {
	Content []streamContent `json:"content"`
}

type streamContent struct {
	Type  string                 `json:"type"`
	Text  string                 `json:"text"`
	Name  string                 `json:"name"`
	Input map[string]interface{} `json:"input"`
}

type streamDelta struct {
	Text string `json:"text"`
}

// extractStreamText pulls human-readable text from a stream-json line.
// Uses encoding/json to properly parse Claude's nested message format.
func extractStreamText(line string) string {
	if len(line) == 0 || line[0] != '{' {
		return ""
	}

	var ev streamEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}

	switch ev.Type {
	case "assistant":
		return extractAssistantText(ev.Message)

	case "content_block_delta":
		if ev.Delta != nil {
			return ev.Delta.Text
		}

	case "result":
		if ev.Subtype == "error_response" && ev.Error != "" {
			return ev.Error
		}
		return "[done]"
	}

	return ""
}

// extractAssistantText extracts text and tool-use summaries from the
// content array in an assistant message.
func extractAssistantText(msg *streamMsg) string {
	if msg == nil || len(msg.Content) == 0 {
		return ""
	}

	var parts []string
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		case "tool_use":
			parts = append(parts, formatToolUse(c))
		}
	}
	return strings.Join(parts, "\n")
}

// formatToolUse returns a short summary of a tool invocation.
func formatToolUse(c streamContent) string {
	for _, key := range []string{
		"file_path", "command", "pattern", "query", "url",
		"description", "task_id", "skill", "prompt",
	} {
		if v, ok := c.Input[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return "[" + c.Name + "] " + s
			}
		}
	}
	return "[" + c.Name + "]"
}

var mdBoldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

// stripMarkdown removes markdown formatting from text for clean terminal output.
func stripMarkdown(s string) string {
	return mdBoldRe.ReplaceAllString(s, "$1")
}

// colorTag applies ANSI color to a bracketed tag like [agent] or [Read].
func colorTag(tag string) string {
	switch {
	case tag == "[done]":
		return logging.Green + tag + logging.Reset
	case tag == "[agent]":
		return logging.Cyan + tag + logging.Reset
	default:
		return logging.Blue + tag + logging.Reset
	}
}

var tagRe = regexp.MustCompile(`\[([A-Za-z][A-Za-z]*)\]`)

// FormatStreamLine takes raw extracted text from a stream event and returns
// a fully formatted output line with timestamp, ANSI colors, and markdown stripped.
func FormatStreamLine(text string) string {
	text = stripMarkdown(text)
	text = tagRe.ReplaceAllStringFunc(text, colorTag)
	return time.Now().Format("15:04:05") + " " + text
}

// FilterStream tails a raw log file and writes formatted, colored output to
// stdout. Intended for use as the tmux stream pane via `ralph filter-stream`.
// Blocks until the process is killed (tmux manages its lifecycle).
func FilterStream(rawLogPath string) {
	f, err := os.Open(rawLogPath)
	if err != nil {
		return
	}
	defer f.Close()

	if _, err := f.Seek(0, 2); err != nil {
		return
	}

	var remainder string
	buf := make([]byte, 64*1024)

	processChunk := func(data string) string {
		for {
			idx := strings.IndexByte(data, '\n')
			if idx < 0 {
				return data
			}
			line := data[:idx]
			data = data[idx+1:]
			if text := extractStreamText(line); text != "" {
				for _, tl := range strings.Split(text, "\n") {
					if tl != "" {
						fmt.Fprintln(os.Stdout, FormatStreamLine("[agent] "+tl))
					}
				}
			}
		}
	}

	for {
		n, _ := f.Read(buf)
		if n > 0 {
			remainder = processChunk(remainder + string(buf[:n]))
		}
		if n == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// startTailGoroutine follows new data appended to path and writes it to
// stdout, similar to tail -f -n 0. Only forwards lines prefixed with
// "[agent] " — orchestrator messages are already written to stdout directly
// by the logger, so forwarding them here would cause duplication.
// Runs entirely in-process so there are no child processes to orphan.
// Returns a channel that closes when the goroutine exits.
func startTailGoroutine(path string, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()

		// Start from end of file (like tail -f -n 0).
		if _, err := f.Seek(0, 2); err != nil {
			return
		}

		var remainder string
		buf := make([]byte, 64*1024)

		processChunk := func(data string) string {
			for {
				idx := strings.IndexByte(data, '\n')
				if idx < 0 {
					return data
				}
				line := data[:idx]
				data = data[idx+1:]
				if strings.Contains(line, "[agent]") {
					fmt.Fprintln(os.Stdout, line)
				}
			}
		}

		for {
			n, _ := f.Read(buf)
			if n > 0 {
				remainder = processChunk(remainder + string(buf[:n]))
			}
			if n == 0 {
				select {
				case <-stop:
					// Final drain.
					for {
						n2, _ := f.Read(buf)
						if n2 == 0 {
							// Flush any remaining partial line.
							if remainder != "" && strings.Contains(remainder, "[agent]") {
								fmt.Fprintln(os.Stdout, remainder)
							}
							return
						}
						remainder = processChunk(remainder + string(buf[:n2]))
					}
				default:
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}()
	return done
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
