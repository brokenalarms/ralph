package notify

import (
	"log"
	"os/exec"
	"runtime"
	"strings"
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
		script := `display notification "` + escapeForAppleScript(body) + `" with title "` + escapeForAppleScript(title) + `"`
		if err := commandRunner("osascript", "-e", script); err != nil {
			log.Printf("notify: osascript failed: %v", err)
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
