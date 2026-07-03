package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
)

var errBDUnavailable = errors.New("bd unavailable")

// Proves: the `ralph feedback` subcommand routes note-append through the
// tasks backend (tasks.BD.AppendNotes) — it invokes `bd update <id>
// --append-notes <msg>` against a bd binary resolved from PATH, with no
// direct exec.Command call in this package.
func TestHandleSubcommand_Feedback_InvokesBDUpdateAppendNotes(t *testing.T) {
	bin := t.TempDir()
	logFile := filepath.Join(bin, "bd.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logFile + "\n"
	if err := os.WriteFile(filepath.Join(bin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	if err := os.MkdirAll(ralphDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateJSON := `{"last_task_id": "ralph-abc"}`
	if err := os.WriteFile(filepath.Join(ralphDir, "state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	log := logging.New(nil)
	sub := config.Subcommand{Name: "feedback", Dir: dir, Args: []string{"please", "fix", "the", "tests"}}

	code := handleSubcommand(sub, log)
	if code != 0 {
		t.Fatalf("handleSubcommand(feedback) = %d, want 0", code)
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

// Proves: preloadTaskContext formats the backend's bd list and bd ready
// output as "$ bd list\n<output>" / "$ bd ready\n<output>" sections joined
// by a blank line, matching the format the task-manager prompt expects —
// unchanged from when this shelled out to bd directly.
func TestPreloadTaskContext_FormatsListAndReadyOutput(t *testing.T) {
	backend := &testutil.StubBackend{
		OpenList:  "ralph-abc: fix the thing",
		ReadyList: "ralph-xyz: ready task",
	}
	log := logging.New(nil)

	got := preloadTaskContext(backend, log)

	want := "$ bd list\nralph-abc: fix the thing\n\n$ bd ready\nralph-xyz: ready task"
	if got != want {
		t.Errorf("preloadTaskContext() = %q, want %q", got, want)
	}
}

// Proves: preloadTaskContext returns an empty string (no startup context)
// when the backend is unavailable, rather than propagating an error into
// the prompt — mirroring the old "bd not found, skip preload" behavior.
func TestPreloadTaskContext_ReturnsEmptyWhenBackendUnavailable(t *testing.T) {
	backend := &testutil.StubBackend{
		OpenListErr:  errBDUnavailable,
		ReadyListErr: errBDUnavailable,
	}
	log := logging.New(nil)

	got := preloadTaskContext(backend, log)

	if got != "" {
		t.Errorf("preloadTaskContext() = %q, want empty string", got)
	}
}

// Proves: preloadTaskContext skips a section whose backend call errored or
// returned empty output, but still includes the other section.
func TestPreloadTaskContext_SkipsFailedSection(t *testing.T) {
	backend := &testutil.StubBackend{
		OpenListErr: errBDUnavailable,
		ReadyList:   "ralph-xyz: ready task",
	}
	log := logging.New(nil)

	got := preloadTaskContext(backend, log)

	if strings.Contains(got, "bd list") {
		t.Errorf("expected no bd list section, got %q", got)
	}
	if !strings.Contains(got, "$ bd ready\nralph-xyz: ready task") {
		t.Errorf("expected bd ready section, got %q", got)
	}
}
