package notify

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// commandRunner executes an OS command. Tests replace this to capture calls without running them.
var commandRunner = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// stalenessThreshold is the maximum age of an event before its notification is silently dropped.
// Notifications older than this are stale (e.g. queued while the terminal was backgrounded) and
// would arrive confusingly after the fact. Default is 60s per user requirement.
var stalenessThreshold = 60 * time.Second

var (
	terminalNotifierPath string
	terminalNotifierOnce sync.Once
)

// SetCommandRunner overrides the notification command executor. Returns previous runner for cleanup.
func SetCommandRunner(r func(string, ...string) error) func(string, ...string) error {
	prev := commandRunner
	commandRunner = r
	return prev
}

// SetStalenessThreshold overrides the staleness gate duration. Returns previous value for cleanup.
func SetStalenessThreshold(d time.Duration) time.Duration {
	prev := stalenessThreshold
	stalenessThreshold = d
	return prev
}

// SetTerminalNotifierPath overrides the resolved terminal-notifier binary path.
// Empty string means not available; falls back to osascript. Returns previous value for cleanup.
func SetTerminalNotifierPath(p string) string {
	// Consume the Once so that resolveTerminalNotifier won't overwrite the test value.
	terminalNotifierOnce.Do(func() {})
	prev := terminalNotifierPath
	terminalNotifierPath = p
	return prev
}

// resolveTerminalNotifier returns the terminal-notifier binary path, resolving via LookPath once.
func resolveTerminalNotifier() string {
	terminalNotifierOnce.Do(func() {
		p, _ := exec.LookPath("terminal-notifier")
		terminalNotifierPath = p
	})
	return terminalNotifierPath
}

// terminalNotifierSender maps $TERM_PROGRAM to the terminal app's bundle ID for -sender.
// Returns empty string when TERM_PROGRAM is unknown — omit -sender entirely in that case.
func terminalNotifierSender() string {
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app":
		return "com.googlecode.iterm2"
	case "Apple_Terminal":
		return "com.apple.Terminal"
	default:
		return ""
	}
}

// escapeForAppleScript produces a value safe to embed inside AppleScript double-quoted strings.
// Order matters: backslash must be escaped before double-quote to avoid double-escaping.
func escapeForAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// sendNotification delivers the notification if eventAt is within the staleness threshold.
// Notifications older than stalenessThreshold are dropped — they arrived too late to be useful.
func sendNotification(title, body string, eventAt time.Time) {
	if time.Since(eventAt) > stalenessThreshold {
		return
	}
	switch runtime.GOOS {
	case "darwin":
		if tn := resolveTerminalNotifier(); tn != "" {
			args := []string{"-title", title, "-message", body}
			if bundle := terminalNotifierSender(); bundle != "" {
				args = append(args, "-sender", bundle)
			}
			if err := commandRunner("terminal-notifier", args...); err != nil {
				log.Printf("notify: terminal-notifier failed: %v", err)
			}
		} else {
			script := `display notification "` + escapeForAppleScript(body) + `" with title "` + escapeForAppleScript(title) + `"`
			if err := commandRunner("osascript", "-e", script); err != nil {
				log.Printf("notify: osascript failed: %v", err)
			}
		}
	case "linux":
		if err := commandRunner("notify-send", title, body); err != nil {
			log.Printf("notify: notify-send failed: %v", err)
		}
	default:
		log.Printf("notify: no notification backend available for %s", runtime.GOOS)
	}
}

func TaskCompleted(taskID, title, summary string, eventAt time.Time) {
	notifTitle := "Task done"
	if taskID != "" {
		notifTitle += ": [" + taskID + "]"
	}
	if title != "" {
		notifTitle += " " + title
	}
	sendNotification(notifTitle, summary, eventAt)
}

// BackendUnavailable reports that the task backend has failed several polls in
// a row. Sent once per outage, not once per retry: an unreachable backend
// otherwise produces an alert every few seconds for as long as it stays down.
func BackendUnavailable(projectDir string, failures int, cause error, eventAt time.Time) {
	title := "Task backend unavailable"
	if projectDir != "" {
		title += ": " + filepath.Base(projectDir)
	}
	body := fmt.Sprintf("%d consecutive task-poll failures", failures)
	if cause != nil {
		body += " — " + cause.Error()
	}
	sendNotification(title, body, eventAt)
}

func TaskMerged(taskID, title string, eventAt time.Time) {
	notifTitle := "Task merged"
	if taskID != "" {
		notifTitle += ": [" + taskID + "]"
	}
	if title != "" {
		notifTitle += " " + title
	}
	sendNotification(notifTitle, "", eventAt)
}
