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
