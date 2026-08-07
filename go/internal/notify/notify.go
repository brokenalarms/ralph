package notify

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// commandRunner executes an OS command. Tests replace this to capture calls without running them.
var commandRunner = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// dialogRunner executes a blocking dialog command and returns its combined
// output. Unlike commandRunner it needs the output, because the user's answer
// comes back on stdout ("button returned:…, gave up:…") and a cancel comes
// back as a non-zero exit with "-128" on stderr. Tests replace this to drive
// both outcomes without a real dialog.
var dialogRunner = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// stalenessThreshold is the maximum age of an event before its notification is silently dropped.
// Notifications older than this are stale (e.g. queued while the terminal was backgrounded) and
// would arrive confusingly after the fact. Default is 60s per user requirement.
var stalenessThreshold = 60 * time.Second

var (
	terminalNotifierPath string
	terminalNotifierOnce sync.Once
	osascriptPath        string
	osascriptOnce        sync.Once
)

// SetCommandRunner overrides the notification command executor. Returns previous runner for cleanup.
func SetCommandRunner(r func(string, ...string) error) func(string, ...string) error {
	prev := commandRunner
	commandRunner = r
	return prev
}

// SetDialogRunner overrides the blocking-dialog executor. Returns previous runner for cleanup.
func SetDialogRunner(r func(string, ...string) (string, error)) func(string, ...string) (string, error) {
	prev := dialogRunner
	dialogRunner = r
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

// SetOsascriptPath overrides the resolved osascript binary path. Empty string
// means no dialog backend is available. Returns previous value for cleanup.
func SetOsascriptPath(p string) string {
	osascriptOnce.Do(func() {})
	prev := osascriptPath
	osascriptPath = p
	return prev
}

// resolveOsascript returns the osascript binary path, resolving via LookPath
// once. Empty on any platform without it — the dialog backend is unavailable
// there, which callers treat as "no user to ask".
func resolveOsascript() string {
	osascriptOnce.Do(func() {
		p, _ := exec.LookPath("osascript")
		osascriptPath = p
	})
	return osascriptPath
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

// osascriptCancelled reports whether an osascript dialog invocation ended in a
// user cancel. AppleScript signals cancel as error -128 on a non-zero exit;
// some environments instead return the button name on stdout. Anything else —
// including a dialog backend that failed to launch — is not a cancel, so the
// caller's default-run semantics are preserved.
func osascriptCancelled(out string, err error) bool {
	if strings.Contains(out, "button returned:"+dialogCancelButton) {
		return true
	}
	return err != nil && (strings.Contains(out, "-128") || strings.Contains(out, "User canceled"))
}

const (
	dialogCancelButton    = "Cancel"
	dialogRunButton       = "Run now"
	acceptanceDialogTitle = "Ralph acceptance"
)

// AcceptanceCountdown puts up a countdown dialog before the ship-time
// acceptance command runs, and reports whether the user cancelled it.
//
// The default is to RUN: when the countdown expires with no response — or when
// no dialog backend is available at all — this returns false and the caller
// proceeds, so unattended operation is preserved. Only an explicit cancel (the
// Cancel button or Escape) returns true.
func AcceptanceCountdown(command string, countdown time.Duration) bool {
	if resolveOsascript() == "" {
		return false
	}
	seconds := int(countdown.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	body := fmt.Sprintf("Running acceptance in %ds — cancel to skip\n\n%s", seconds, command)
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"%s", "%s"} default button "%s" giving up after %d`,
		escapeForAppleScript(body), escapeForAppleScript(acceptanceDialogTitle),
		dialogCancelButton, dialogRunButton, dialogRunButton, seconds)

	out, err := dialogRunner("osascript", "-e", script)
	if osascriptCancelled(out, err) {
		return true
	}
	if err != nil {
		log.Printf("notify: acceptance countdown dialog failed (%v) — defaulting to run", err)
	}
	return false
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
