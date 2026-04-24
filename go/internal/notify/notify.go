package notify

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
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

func sendNotification(title, body string) {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification %q with title %q`, body, title)
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
