package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
)

// Proves: a resumed task session is launched with --resume and never with
// --session-id. The two flags are mutually exclusive — --session-id asks the
// CLI to create a session with that id, which would discard the transcript
// the user asked to come back to. The EnterWorktree ban is kept in both
// modes, so a resumed session stays pinned to its worktree.
func TestTaskResumeExtraArgs_UsesResumeNotSessionID(t *testing.T) {
	args := taskResumeExtraArgs("test-session-id")

	want := []string{"--resume", "test-session-id", "--disallowedTools", "EnterWorktree"}
	if len(args) != len(want) {
		t.Fatalf("taskResumeExtraArgs() = %q, want %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("taskResumeExtraArgs()[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

// Proves: `ralph task --resume <id>` is recognised as a resume request and
// the flag is consumed, so it never reaches the session as a stray argument.
// Both the spaced and the =-joined spellings work, and a value-less --resume
// is rejected rather than silently starting a fresh session.
func TestParseTaskResumeFlag(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    string
		rest    []string
		wantErr bool
	}{
		{name: "spaced", args: []string{"--resume", "abc-123"}, want: "abc-123", rest: []string{}},
		{name: "joined", args: []string{"--resume=abc-123"}, want: "abc-123", rest: []string{}},
		{name: "keeps other args", args: []string{"--model", "opus", "--resume", "abc"}, want: "abc", rest: []string{"--model", "opus"}},
		{name: "absent", args: []string{"--model", "opus"}, want: "", rest: []string{"--model", "opus"}},
		{name: "missing value", args: []string{"--resume"}, wantErr: true},
		{name: "empty joined value", args: []string{"--resume="}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, err := parseTaskResumeFlag(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTaskResumeFlag(%q) should error", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTaskResumeFlag(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("session id = %q, want %q", got, tc.want)
			}
			if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
				t.Errorf("remaining args = %q, want %q", rest, tc.rest)
			}
		})
	}
}

// Proves: the worktree a task session runs in survives the session, so a
// later `ralph task --resume` can re-anchor there instead of creating a new
// worktree and stranding the resumed session away from its work.
func TestTaskSession_RoundTrip(t *testing.T) {
	ralphDir := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sessionID = "12345678-1234-4567-89ab-123456789abc"

	if err := writeTaskSession(ralphDir, sessionID, taskSession{WorkDir: workDir, Branch: "ralph/task/20260701-01"}); err != nil {
		t.Fatalf("writeTaskSession: %v", err)
	}

	got, err := readTaskSession(ralphDir, sessionID)
	if err != nil {
		t.Fatalf("readTaskSession: %v", err)
	}
	if got.WorkDir != workDir {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, workDir)
	}
	if got.Branch != "ralph/task/20260701-01" {
		t.Errorf("Branch = %q, want %q", got.Branch, "ralph/task/20260701-01")
	}
}

// Proves: resuming a session ralph never launched fails with a clear error
// instead of falling through to fresh-worktree creation.
func TestReadTaskSession_UnknownSessionErrors(t *testing.T) {
	_, err := readTaskSession(t.TempDir(), "12345678-1234-4567-89ab-123456789abc")
	if err == nil {
		t.Fatal("readTaskSession should error when no record exists")
	}
	if !strings.Contains(err.Error(), "no worktree recorded") {
		t.Errorf("error should explain that no worktree was recorded, got: %v", err)
	}
}

// Proves: when the user answered "n" to "Keep this task worktree for
// resume?", a later resume of that session reports the missing worktree
// rather than silently starting over somewhere else.
func TestReadTaskSession_MissingWorktreeErrors(t *testing.T) {
	ralphDir := t.TempDir()
	const sessionID = "12345678-1234-4567-89ab-123456789abc"
	gone := filepath.Join(t.TempDir(), "removed-worktree")

	if err := writeTaskSession(ralphDir, sessionID, taskSession{WorkDir: gone}); err != nil {
		t.Fatalf("writeTaskSession: %v", err)
	}

	_, err := readTaskSession(ralphDir, sessionID)
	if err == nil {
		t.Fatal("readTaskSession should error when the recorded worktree is gone")
	}
	if !strings.Contains(err.Error(), gone) {
		t.Errorf("error should name the missing worktree %q, got: %v", gone, err)
	}
}

// Proves: a session id from the command line cannot escape the session
// record directory — it is used as a file name, so anything but a UUID-shaped
// value is refused.
func TestTaskSessionPath_RejectsPathTraversal(t *testing.T) {
	for _, id := range []string{"", "../../etc/passwd", "abc/def", "not a uuid"} {
		if _, err := taskSessionPath(t.TempDir(), id); err == nil {
			t.Errorf("taskSessionPath should reject session id %q", id)
		}
	}
}

// Proves the whole `ralph task --resume <session-id>` launch: it reuses the
// worktree recorded at launch, rebuilds the task-manager system prompt from
// current bead state (the transcript restores no system prompt, which is why
// a raw `claude --resume` came back as a plain Claude session), and hands the
// CLI --resume plus bypassPermissions — never --session-id.
func TestHandleTask_ResumeLaunchesInRecordedWorktreeWithRebuiltPrompt(t *testing.T) {
	projectDir := t.TempDir()
	runCmd(t, "git", "-C", projectDir, "init")
	runCmd(t, "git", "-C", projectDir, "commit", "--allow-empty", "-m", "init")

	ralphDir := filepath.Join(projectDir, ".ralph")
	workDir := filepath.Join(ralphDir, "worktrees", "ralph-task-20260701-01")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const sessionID = "12345678-1234-4567-89ab-123456789abc"
	if err := writeTaskSession(ralphDir, sessionID, taskSession{WorkDir: workDir, Branch: "ralph/task/20260701-01"}); err != nil {
		t.Fatal(err)
	}

	// The bd shim's output is what preloadTaskContext collects, so finding
	// the marker in the system prompt proves the preload ran at resume time.
	const beadMarker = "BEAD-STATE-AT-RESUME-TIME"
	bin := t.TempDir()
	invocation := filepath.Join(bin, "claude-invocation.txt")
	writeShim(t, filepath.Join(bin, "bd"), `#!/bin/sh
case "$*" in
  *--json*) echo "[]" ;;
  list*) echo "`+beadMarker+`" ;;
  *) echo "" ;;
esac
`)
	writeShim(t, filepath.Join(bin, "claude"), `#!/bin/sh
{
  echo "CWD=$PWD"
  for a in "$@"; do echo "ARG=$a"; done
} > `+invocation+`
`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	code := handleTask(config.Subcommand{Name: "task", Dir: projectDir, Args: []string{"--resume", sessionID}}, logging.New(nil))
	if code != 0 {
		t.Fatalf("handleTask --resume exit code = %d, want 0", code)
	}

	data, err := os.ReadFile(invocation)
	if err != nil {
		t.Fatalf("claude was never invoked: %v", err)
	}
	out := string(data)

	for _, want := range []string{
		"ARG=--resume",
		"ARG=" + sessionID,
		"ARG=--permission-mode",
		"ARG=bypassPermissions",
		"ARG=--system-prompt",
		"ARG=--disallowedTools",
		"ARG=EnterWorktree",
		beadMarker,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("resumed launch should include %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ARG=--session-id") {
		t.Errorf("resumed launch must not pass --session-id, got:\n%s", out)
	}
	if !strings.Contains(out, "CWD=") || !strings.Contains(out, filepath.Base(workDir)) {
		t.Errorf("resumed session should run in the recorded worktree %q, got:\n%s", workDir, out)
	}
}

// Proves: resuming a session whose worktree was cleaned up fails instead of
// creating a fresh worktree — the user is told the session is gone rather
// than silently getting an empty one.
func TestHandleTask_ResumeWithMissingWorktreeFails(t *testing.T) {
	projectDir := t.TempDir()
	runCmd(t, "git", "-C", projectDir, "init")
	runCmd(t, "git", "-C", projectDir, "commit", "--allow-empty", "-m", "init")

	ralphDir := filepath.Join(projectDir, ".ralph")
	const sessionID = "12345678-1234-4567-89ab-123456789abc"
	gone := filepath.Join(ralphDir, "worktrees", "removed")
	if err := writeTaskSession(ralphDir, sessionID, taskSession{WorkDir: gone}); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	writeShim(t, filepath.Join(bin, "claude"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	var logBuf strings.Builder
	code := handleTask(config.Subcommand{Name: "task", Dir: projectDir, Args: []string{"--resume", sessionID}}, logging.New(&logBuf))

	if code != 1 {
		t.Errorf("exit code = %d, want 1 for a missing worktree", code)
	}
	if !strings.Contains(logBuf.String(), "no longer exists") {
		t.Errorf("error should explain the worktree is gone, got:\n%s", logBuf.String())
	}
	if _, err := os.Stat(filepath.Join(ralphDir, "worktrees")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(ralphDir, "worktrees"))
		if len(entries) > 0 {
			t.Errorf("no worktree should have been created, found %d entries", len(entries))
		}
	}
}

func writeShim(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
