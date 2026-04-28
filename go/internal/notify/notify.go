package notify

import (
	"log"
	"os/exec"
	"runtime"
	"strings"
)

// commandRunner executes an OS command. Tests replace this to capture calls without running them.
var commandRunner = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// SetCommandRunner overrides the notification command executor. Returns previous runner for cleanup.
func SetCommandRunner(r func(string, ...string) error) func(string, ...string) error {
	prev := commandRunner
	commandRunner = r
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

func sendNotification(title, body string) {
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

func TaskCompleted(taskID, title, summary string) {
	notifTitle := "Task done"
	if taskID != "" {
		notifTitle += ": [" + taskID + "]"
	}
	if title != "" {
		notifTitle += " " + title
	}
	sendNotification(notifTitle, summary)
}

func TaskMerged(taskID, title string) {
	notifTitle := "Task merged"
	if taskID != "" {
		notifTitle += ": [" + taskID + "]"
	}
	if title != "" {
		notifTitle += " " + title
	}
	sendNotification(notifTitle, "")
}
