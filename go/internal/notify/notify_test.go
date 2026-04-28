package notify

import (
	"runtime"
	"strings"
	"testing"
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

	TaskCompleted("ralph-abc", "Fix login bug", "Fixed auth token expiry")

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

	TaskCompleted("ralph-xyz", "Add caching", "")

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

	TaskCompleted("", "", "")

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

	TaskMerged("ralph-abc", "Fix login bug")

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

	TaskMerged("ralph-xyz", "")

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

	TaskMerged("", "")

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
	TaskCompleted("ralph-abc", "Fix bug", summary)

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
	TaskCompleted("ralph-abc", "Fix bug", summary)

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
