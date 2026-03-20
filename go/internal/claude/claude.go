package claude

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Log is the logging interface used by Runner.
type Log interface {
	Log(format string, args ...any)
	Task(format string, args ...any)
	TaskSuccess(format string, args ...any)
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
}

// Result describes the outcome of a Claude run.
type Result struct {
	SignalDetected bool   // true if a completion signal was found
	AllComplete    bool   // true if the all-complete signal was found
	IdleTimeout    bool   // true if the session was killed due to idle timeout
	TaskDesc       string // task description from the current-task signal
	Summary        string // completion summary from signal file
}

// OnTaskDetected is called when a current-task signal file appears.
// The callback receives the task description read from the signal file.
type OnTaskDetected func(taskDesc string)

// Runner manages Claude process lifecycle: spawning, signal polling, and cleanup.
type Runner struct {
	Logger         Log
	OnTaskDetected OnTaskDetected
}

// Run spawns a Claude process, polls for signal files, and returns when the
// process exits or a completion signal is detected. Mirrors ralph.sh run_claude.
func (r *Runner) Run(cfg RunConfig) (Result, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}

	clearSignals(cfg.Signals)

	rawLog, err := os.OpenFile(cfg.RawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Result{}, fmt.Errorf("opening raw log: %w", err)
	}
	defer rawLog.Close()

	cmd := exec.Command("claude",
		"--print", "--verbose",
		"--output-format", "stream-json",
		"--add-dir", cfg.WorkDir,
		"--add-dir", cfg.RalphDir,
		"--dangerously-skip-permissions",
		"-p", cfg.Prompt,
	)
	cmd.Dir = cfg.WorkDir
	cmd.Stdin = nil
	cmd.Stdout = rawLog
	cmd.Stderr = rawLog
	// Start in its own process group so we can signal it cleanly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting claude: %w", err)
	}
	r.Logger.Log("Claude started (PID: %d)", cmd.Process.Pid)

	// Record stream start time for the filter.
	_ = os.WriteFile(filepath.Join(cfg.RalphDir, ".stream-start"),
		[]byte(fmt.Sprintf("%d", time.Now().Unix())), 0o644)

	// Start stream filter BEFORE Claude begins writing so it catches all
	// output (mirrors the bash fix for the tail -f race).
	filterStop := make(chan struct{})
	filterDone := r.startStreamFilter(cfg, filterStop)

	// Start optional terminal tail.
	var tailCmd *exec.Cmd
	if !cfg.Quiet && cfg.LogFile != "" {
		tailCmd = r.startTail(cfg.LogFile)
	}

	// Poll for signals or process exit.
	result := r.poll(cmd, cfg)

	// Reap the Claude process.
	_ = cmd.Wait()

	// Signal the filter to drain remaining output and exit.
	close(filterStop)
	<-filterDone
	stopProcess(tailCmd)

	// Final signal check — Claude may have written a signal just before exiting.
	if !result.SignalDetected {
		if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
			result.SignalDetected = true
			result.AllComplete = hasSignal(cfg.Signals.AllComplete)
			result.Summary = readSignalSummary(cfg.Signals)
			r.Logger.TaskSuccess("Task completed via signal")
		} else {
			r.Logger.Log("Claude exited (no completion signal)")
		}
	}

	return result, nil
}

// poll checks for process exit and signal files on a ticker.
func (r *Runner) poll(cmd *exec.Cmd, cfg RunConfig) Result {
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

		case <-ticker.C:
			// Check if process already exited (channel may not fire instantly).
			if !processAlive(cmd) {
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
					r.Logger.Task("Working on: %s", desc)
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
				r.Logger.TaskSuccess("Completed: %s", summary)

				gracefulKill(cmd)

				return Result{
					SignalDetected: true,
					AllComplete:    hasSignal(cfg.Signals.AllComplete),
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
					gracefulKill(cmd)
					return Result{IdleTimeout: true}
				}
			}
		}
	}
}

// startStreamFilter launches a goroutine that tails the raw log and writes
// filtered human-readable output to LogFile. The goroutine keeps reading
// until stop is closed, then drains any remaining content. Returns a channel
// that closes when the filter is done.
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
				fmt.Fprintln(logOut, text)
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

// extractStreamText pulls human-readable text from a stream-json line.
// Rather than pulling in a JSON library, we do minimal string extraction
// for the content_block_delta and result message types that contain text.
func extractStreamText(line string) string {
	// Quick rejection: must look like JSON.
	if len(line) == 0 || line[0] != '{' {
		return ""
	}

	// assistant message with content text
	if strings.Contains(line, `"type":"assistant"`) || strings.Contains(line, `"type": "assistant"`) {
		return extractJSONString(line, "content")
	}

	// content_block_delta with text delta
	if strings.Contains(line, `"content_block_delta"`) {
		return extractJSONString(line, "text")
	}

	// result message
	if strings.Contains(line, `"type":"result"`) || strings.Contains(line, `"type": "result"`) {
		if strings.Contains(line, `"subtype":"error_response"`) || strings.Contains(line, `"subtype": "error_response"`) {
			return extractJSONString(line, "error")
		}
	}

	return ""
}

// extractJSONString does a best-effort extraction of a string value for the
// given key from a JSON line. This avoids importing encoding/json for this
// hot path. Returns empty string if not found.
func extractJSONString(line, key string) string {
	needle := `"` + key + `"`
	idx := strings.Index(line, needle)
	if idx < 0 {
		return ""
	}
	// Skip past key, colon, optional whitespace, and opening quote.
	rest := line[idx+len(needle):]
	rest = strings.TrimLeft(rest, " \t:")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]

	// Read until unescaped closing quote.
	var b strings.Builder
	for i := 0; i < len(rest); i++ {
		if rest[i] == '\\' && i+1 < len(rest) {
			switch rest[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(rest[i+1])
			}
			i++
			continue
		}
		if rest[i] == '"' {
			break
		}
		b.WriteByte(rest[i])
	}
	return b.String()
}

// startTail launches tail -f on the given file for terminal display.
func (r *Runner) startTail(path string) *exec.Cmd {
	cmd := exec.Command("tail", "-f", "-n", "0", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		r.Logger.Warn("Could not start tail: %v", err)
		return nil
	}
	return cmd
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
func gracefulKill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	// Give Claude 2 seconds to clean up.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
	}
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
