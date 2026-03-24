package health

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
)

// Collect returns process counts, directory size, state fields, signal files,
// and branch status — a between-iteration snapshot of orchestrator health.
func TestCollect_PopulatesAllFields(t *testing.T) {
	dir := t.TempDir()

	// State file with known field count
	state := map[string]any{"iteration": 5, "status": "running", "last_task": "fix bug"}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644)

	// Signal files
	os.WriteFile(filepath.Join(dir, ".signal_current_task"), []byte("test"), 0o644)
	os.WriteFile(filepath.Join(dir, ".signal_complete"), []byte("done"), 0o644)

	// Non-signal file should be ignored
	os.WriteFile(filepath.Join(dir, "stop"), []byte(""), 0o644)

	snap := Collect(dir, dir)

	if snap.RalphDirSizeMB <= 0 {
		t.Error("expected positive directory size")
	}

	if snap.StateFields != 3 {
		t.Errorf("expected 3 state fields, got %d", snap.StateFields)
	}

	if len(snap.SignalFiles) != 2 {
		t.Errorf("expected 2 signal files, got %v", snap.SignalFiles)
	}

	for _, name := range snap.SignalFiles {
		if !strings.HasPrefix(name, ".signal_") {
			t.Errorf("unexpected signal file: %s", name)
		}
	}
}

// Log formats the snapshot as a single compact line with process counts,
// directory size, state field count, signal files, and branch name.
func TestLog_FormatsCompactLine(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.NewWithWriter(&buf)

	snap := Snapshot{
		TailProcesses:   2,
		FilterProcesses: 1,
		RalphDirSizeMB:  1.5,
		StateFields:     8,
		SignalFiles:     []string{".signal_current_task"},
		WorktreeBranch:  "ralph/fix-bug",
	}

	Log(logger, snap)
	output := buf.String()

	if !strings.Contains(output, "tail=2") {
		t.Errorf("expected tail=2 in output: %s", output)
	}
	if !strings.Contains(output, "filter=1") {
		t.Errorf("expected filter=1 in output: %s", output)
	}
	if !strings.Contains(output, "1.5MB") {
		t.Errorf("expected 1.5MB in output: %s", output)
	}
	if !strings.Contains(output, "8 state fields") {
		t.Errorf("expected '8 state fields' in output: %s", output)
	}
	if !strings.Contains(output, ".signal_current_task") {
		t.Errorf("expected signal file name in output: %s", output)
	}
	if !strings.Contains(output, "ralph/fix-bug") {
		t.Errorf("expected branch name in output: %s", output)
	}
	if !strings.Contains(output, "[health]") {
		t.Errorf("expected [health] domain tag in output: %s", output)
	}
}

// When no signal files exist, "none" is shown instead of an empty list.
func TestLog_NoSignals(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.NewWithWriter(&buf)

	snap := Snapshot{SignalFiles: nil, WorktreeBranch: "main"}
	Log(logger, snap)

	if !strings.Contains(buf.String(), "signals: none") {
		t.Errorf("expected 'signals: none' in output: %s", buf.String())
	}
}

func TestFindSignalFiles_OnlyMatchesDotSignalPrefix(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".signal_complete"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, ".signal_all_complete"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "signal_bogus"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "stop"), []byte(""), 0o644)

	found := findSignalFiles(dir)
	if len(found) != 2 {
		t.Errorf("expected 2 signal files, got %v", found)
	}
}

func TestStateFieldCount_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte("{}"), 0o644)

	if count := stateFieldCount(path); count != 0 {
		t.Errorf("expected 0 fields for empty JSON, got %d", count)
	}
}

func TestStateFieldCount_MissingFile(t *testing.T) {
	if count := stateFieldCount("/nonexistent/state.json"); count != 0 {
		t.Errorf("expected 0 for missing file, got %d", count)
	}
}
