package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
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

// Proves: preloadTaskContext appends a "$ audit-window" block listing
// unaudited ralph-loop closures from the last 72 hours, computed
// deterministically in Go rather than left to the session to recompute.
func TestPreloadTaskContext_AppendsAuditWindowWhenUnauditedClosuresExist(t *testing.T) {
	now := time.Now()
	backend := &testutil.StubBackend{
		ClosedTasks: []tasks.ClosedTaskInfo{
			{ID: "ralph-abc", Title: "Fix the thing", Assignee: config.LoopAssignee, ClosedAt: now.Add(-1 * time.Hour)},
			{ID: "ralph-old", Title: "Too old", Assignee: config.LoopAssignee, ClosedAt: now.Add(-73 * time.Hour)},
			{ID: "ralph-self", Title: "Self work", Assignee: config.TaskAssignee, ClosedAt: now.Add(-1 * time.Hour)},
		},
	}
	log := logging.New(nil)

	got := preloadTaskContext(backend, log)

	if !strings.Contains(got, "$ audit-window") {
		t.Fatalf("expected an audit-window section, got %q", got)
	}
	if !strings.Contains(got, "ralph-abc") {
		t.Errorf("expected ralph-abc in audit-window section, got %q", got)
	}
	if strings.Contains(got, "ralph-old") || strings.Contains(got, "ralph-self") {
		t.Errorf("expected stale/self-work closures excluded, got %q", got)
	}
}

// Proves: preloadTaskContext omits the "$ audit-window" block entirely when
// no unaudited closures exist, so the task-manager prompt stays silent.
func TestPreloadTaskContext_OmitsAuditWindowWhenEmpty(t *testing.T) {
	backend := &testutil.StubBackend{}
	log := logging.New(nil)

	got := preloadTaskContext(backend, log)

	if strings.Contains(got, "audit-window") {
		t.Errorf("expected no audit-window section, got %q", got)
	}
}
