package notify

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func platformCommand() string {
	if runtime.GOOS == "darwin" {
		return "osascript"
	}
	return "notify-send"
}

type notifyCall struct {
	name string
	args []string
}

func captureRunner(t *testing.T) *[]notifyCall {
	t.Helper()
	calls := &[]notifyCall{}
	prev := SetCommandRunner(func(name string, args ...string) error {
		*calls = append(*calls, notifyCall{name: name, args: args})
		return nil
	})
	t.Cleanup(func() { SetCommandRunner(prev) })
	prevTN := SetTerminalNotifierPath("")
	t.Cleanup(func() { SetTerminalNotifierPath(prevTN) })
	return calls
}

func argsJoined(calls []notifyCall) string {
	var sb strings.Builder
	for _, c := range calls {
		sb.WriteString(c.name)
		for _, a := range c.args {
			sb.WriteByte(' ')
			sb.WriteString(a)
		}
	}
	return sb.String()
}

// TaskCompleted uses osascript and includes bead ID and task title in the notification title.
func TestTaskCompleted_UsesOsascript(t *testing.T) {
	calls := captureRunner(t)

	TaskCompleted("ralph-abc", "Fix login bug", "Fixed auth token expiry", time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	got := (*calls)[0]
	if got.name != platformCommand() {
		t.Errorf("expected %s, got %q", platformCommand(), got.name)
	}
	script := strings.Join(got.args, " ")
	if !strings.Contains(script, "Task done: [ralph-abc] Fix login bug") {
		t.Errorf("notification title missing bead ID and task title, got %q", script)
	}
	if !strings.Contains(script, "Fixed auth token expiry") {
		t.Errorf("notification body missing summary, got %q", script)
	}
}

// TaskCompleted with no summary still fires with bead ID and title.
func TestTaskCompleted_NoSummary(t *testing.T) {
	calls := captureRunner(t)

	TaskCompleted("ralph-xyz", "Add caching", "", time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	script := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(script, "Task done: [ralph-xyz] Add caching") {
		t.Errorf("notification missing bead ID and title, got %q", script)
	}
}

// TaskCompleted with all empty fields still fires a notification.
func TestTaskCompleted_Empty(t *testing.T) {
	calls := captureRunner(t)

	TaskCompleted("", "", "", time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	script := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(script, "Task done") {
		t.Errorf("notification missing base text, got %q", script)
	}
}

// TaskMerged uses osascript and includes bead ID and task title.
func TestTaskMerged_UsesOsascript(t *testing.T) {
	calls := captureRunner(t)

	TaskMerged("ralph-abc", "Fix login bug", time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	got := (*calls)[0]
	if got.name != platformCommand() {
		t.Errorf("expected %s, got %q", platformCommand(), got.name)
	}
	script := strings.Join(got.args, " ")
	if !strings.Contains(script, "Task merged: [ralph-abc] Fix login bug") {
		t.Errorf("notification missing bead ID and task title, got %q", script)
	}
}

// TaskMerged with only a task ID (no title) still fires.
func TestTaskMerged_IDOnly(t *testing.T) {
	calls := captureRunner(t)

	TaskMerged("ralph-xyz", "", time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	script := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(script, "Task merged: [ralph-xyz]") {
		t.Errorf("notification missing bead ID, got %q", script)
	}
}

// TaskMerged with no ID or title still fires a notification.
func TestTaskMerged_Empty(t *testing.T) {
	calls := captureRunner(t)

	TaskMerged("", "", time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	script := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(script, "Task merged") {
		t.Errorf("notification missing base text, got %q", script)
	}
}

// TaskCompleted with a summary containing special chars produces a correctly-escaped AppleScript.
// Newlines become spaces, backslashes become \\, double-quotes become \", non-ASCII is passed through.
func TestTaskCompleted_SpecialCharsEscapedForAppleScript(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("AppleScript escaping only applies on darwin")
	}
	calls := captureRunner(t)

	summary := "line1\npath\\to\\file\"done\"café"
	TaskCompleted("ralph-abc", "Fix bug", summary, time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	script := strings.Join((*calls)[0].args, " ")

	// Newline must not appear as Go-style \n two-char sequence — it should be replaced with a space.
	if strings.Contains(script, `\n`) {
		t.Errorf("script contains Go-style \\n escape; want newline replaced with space, got %q", script)
	}
	// Newline replaced with space — surrounding words should be adjacent via space.
	if !strings.Contains(script, "line1 path") {
		t.Errorf("script missing newline-replaced-with-space sequence, got %q", script)
	}
	// Backslash must be escaped as \\.
	if !strings.Contains(script, `\\`) {
		t.Errorf("script missing escaped backslash, got %q", script)
	}
	// Double-quote must be escaped as \".
	if !strings.Contains(script, `\"`) {
		t.Errorf("script missing escaped double-quote, got %q", script)
	}
	// Non-ASCII must pass through as-is, not as \uNNNN or \xNN.
	if !strings.Contains(script, "café") {
		t.Errorf("script missing non-ASCII chars (want café), got %q", script)
	}
}

// TaskCompleted with special chars produces no Go-style escape sequences (\xNN, \uNNNN, bare \n).
func TestTaskCompleted_NoGoStyleEscapesInScript(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("AppleScript escaping only applies on darwin")
	}
	calls := captureRunner(t)

	summary := "newline:\n unicode:é hex:\x41 path:\\dir"
	TaskCompleted("ralph-abc", "Fix bug", summary, time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	// Inspect only the osascript -e argument (the script string itself).
	var scriptArg string
	for i, a := range (*calls)[0].args {
		if a == "-e" && i+1 < len((*calls)[0].args) {
			scriptArg = (*calls)[0].args[i+1]
		}
	}
	if scriptArg == "" {
		t.Fatal("could not find -e argument in osascript call")
	}
	// No Go-style \uNNNN unicode escapes.
	for i := 0; i < len(scriptArg)-5; i++ {
		if scriptArg[i] == '\\' && scriptArg[i+1] == 'u' {
			t.Errorf("script contains Go-style \\uNNNN escape at pos %d: %q", i, scriptArg[i:i+6])
		}
	}
	// No Go-style \xNN hex escapes.
	for i := 0; i < len(scriptArg)-3; i++ {
		if scriptArg[i] == '\\' && scriptArg[i+1] == 'x' {
			t.Errorf("script contains Go-style \\xNN escape at pos %d: %q", i, scriptArg[i:i+4])
		}
	}
	// No bare \n (Go newline escape) — only valid escapes are \\ and \".
	if strings.Contains(scriptArg, `\n`) {
		t.Errorf("script contains bare \\n Go escape sequence, got %q", scriptArg)
	}
}

// A stale event timestamp causes sendNotification to drop the notification without calling
// the command runner. This is the timeliness gate: notifications that arrive long after
// their event (e.g. deferred by macOS when the terminal was backgrounded) are silently dropped.
func TestSendNotification_StaleTimestamp_Dropped(t *testing.T) {
	calls := captureRunner(t)

	prev := SetStalenessThreshold(1 * time.Second)
	t.Cleanup(func() { SetStalenessThreshold(prev) })

	staleAt := time.Now().Add(-2 * time.Second)
	TaskCompleted("ralph-abc", "Fix bug", "summary", staleAt)

	if len(*calls) != 0 {
		t.Errorf("expected 0 command calls for stale notification, got %d: %v", len(*calls), *calls)
	}
}

// A fresh event timestamp causes sendNotification to fire — the notification arrives promptly
// and is delivered to the user.
func TestSendNotification_FreshTimestamp_Fires(t *testing.T) {
	calls := captureRunner(t)

	prev := SetStalenessThreshold(60 * time.Second)
	t.Cleanup(func() { SetStalenessThreshold(prev) })

	TaskCompleted("ralph-abc", "Fix bug", "summary", time.Now())

	if len(*calls) != 1 {
		t.Errorf("expected 1 command call for fresh notification, got %d", len(*calls))
	}
}

// When terminal-notifier is available, sendNotification uses it with -title/-message argv
// instead of routing through osascript/Script Editor.
func TestSendNotification_TerminalNotifier_Used(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("terminal-notifier path is darwin-only")
	}
	calls := captureRunner(t)
	prev := SetTerminalNotifierPath("/usr/local/bin/terminal-notifier")
	t.Cleanup(func() { SetTerminalNotifierPath(prev) })
	t.Setenv("TERM_PROGRAM", "")

	TaskCompleted("ralph-abc", "Fix login bug", "Fixed auth token expiry", time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	got := (*calls)[0]
	if got.name != "terminal-notifier" {
		t.Errorf("expected terminal-notifier, got %q", got.name)
	}
	argv := strings.Join(got.args, " ")
	if !strings.Contains(argv, "-title") {
		t.Errorf("missing -title flag, got %q", argv)
	}
	if !strings.Contains(argv, "Task done: [ralph-abc] Fix login bug") {
		t.Errorf("missing expected title value, got %q", argv)
	}
	if !strings.Contains(argv, "-message") {
		t.Errorf("missing -message flag, got %q", argv)
	}
	if !strings.Contains(argv, "Fixed auth token expiry") {
		t.Errorf("missing expected message value, got %q", argv)
	}
}

// The -sender arg is included only when TERM_PROGRAM maps to a known bundle ID;
// unknown values (including empty) cause -sender to be omitted entirely.
func TestSendNotification_TerminalNotifier_SenderResolution(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("terminal-notifier sender resolution is darwin-only")
	}
	cases := []struct {
		termProgram    string
		wantSender     bool
		expectedBundle string
	}{
		{"iTerm.app", true, "com.googlecode.iterm2"},
		{"Apple_Terminal", true, "com.apple.Terminal"},
		{"vscode", false, ""},
		{"", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.termProgram, func(t *testing.T) {
			calls := captureRunner(t)
			prev := SetTerminalNotifierPath("/usr/local/bin/terminal-notifier")
			t.Cleanup(func() { SetTerminalNotifierPath(prev) })
			t.Setenv("TERM_PROGRAM", tc.termProgram)

			TaskCompleted("ralph-abc", "title", "body", time.Now())

			if len(*calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(*calls))
			}
			argv := strings.Join((*calls)[0].args, " ")
			hasSender := strings.Contains(argv, "-sender")
			if hasSender != tc.wantSender {
				t.Errorf("TERM_PROGRAM=%q: wantSender=%v but hasSender=%v, argv=%q",
					tc.termProgram, tc.wantSender, hasSender, argv)
			}
			if tc.wantSender && !strings.Contains(argv, tc.expectedBundle) {
				t.Errorf("TERM_PROGRAM=%q: expected bundle %q in argv %q",
					tc.termProgram, tc.expectedBundle, argv)
			}
		})
	}
}

// When terminal-notifier is not available (empty path), sendNotification falls back to
// the existing osascript invocation unchanged.
func TestSendNotification_NoTerminalNotifier_FallsBackToOsascript(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("osascript fallback is darwin-only")
	}
	calls := captureRunner(t)
	// captureRunner already sets terminal-notifier to ""; this confirms the fallback path.
	prev := SetTerminalNotifierPath("")
	t.Cleanup(func() { SetTerminalNotifierPath(prev) })

	TaskCompleted("ralph-abc", "Fix bug", "Summary text", time.Now())

	if len(*calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(*calls))
	}
	if (*calls)[0].name != "osascript" {
		t.Errorf("expected osascript fallback, got %q", (*calls)[0].name)
	}
}
