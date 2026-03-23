package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailStub_StartStop(t *testing.T) {
	ts := NewTailStub("/tmp/test.log")

	if ts.Started() {
		t.Fatal("should not be started before Start()")
	}

	stopCh := ts.Start()
	if !ts.Started() {
		t.Fatal("should be started after Start()")
	}

	ts.Stop()
	if !ts.Stopped() {
		t.Fatal("should be stopped after Stop()")
	}

	select {
	case <-stopCh:
	default:
		t.Fatal("stop channel should be closed after Stop()")
	}

	calls := ts.Calls()
	want := []string{"start", "stop"}
	if len(calls) != len(want) {
		t.Fatalf("Calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("Calls[%d] = %q, want %q", i, calls[i], want[i])
		}
	}
}

func TestTailStub_WriteLine(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "test.log")

	ts := NewTailStub(logFile)
	ts.WriteLine("line one")
	ts.WriteLine("line two")

	lines := ts.Lines()
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0] != "line one" {
		t.Errorf("lines[0] = %q, want %q", lines[0], "line one")
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	if !strings.Contains(string(data), "line one\n") {
		t.Error("log file should contain 'line one'")
	}
	if !strings.Contains(string(data), "line two\n") {
		t.Error("log file should contain 'line two'")
	}
}

func TestTailStub_File(t *testing.T) {
	ts := NewTailStub("/var/log/ralph.log")
	if ts.File() != "/var/log/ralph.log" {
		t.Errorf("File = %q, want %q", ts.File(), "/var/log/ralph.log")
	}
}

func TestTailStub_DoubleStopIsSafe(t *testing.T) {
	ts := NewTailStub("")
	ts.Stop()
	ts.Stop()
	if !ts.Stopped() {
		t.Fatal("should be stopped")
	}
}
