package server

import (
	"os"
	"path/filepath"
	"testing"
)

// Proves: appendNotesDefault routes note-append through the tasks backend
// (tasks.BD.AppendNotes) rather than shelling out to bd itself — it invokes
// `bd update <id> --append-notes <msg>` against a bd binary resolved from
// PATH, with no direct exec.Command in this package.
func TestAppendNotesDefault_InvokesBDUpdateAppendNotes(t *testing.T) {
	bin := t.TempDir()
	logFile := filepath.Join(bin, "bd.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logFile + "\n"
	if err := os.WriteFile(filepath.Join(bin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	s := &Server{}
	projectDir := t.TempDir()
	if err := s.appendNotesDefault(projectDir, "ralph-abc", "please fix the tests"); err != nil {
		t.Fatalf("appendNotesDefault returned error: %v", err)
	}

	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected bd to be invoked, log file missing: %v", err)
	}
	got := string(logged)
	if got != "update ralph-abc --append-notes please fix the tests\n" {
		t.Errorf("bd invocation = %q, want %q", got, "update ralph-abc --append-notes please fix the tests\n")
	}
}
