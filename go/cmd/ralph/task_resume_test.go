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

// Proves the whole `ralph task --resume <session-id>` launch: it creates a
// fresh worktree (transcript lookup is not cwd-bound, so the resumed session
// needs a worktree to stay out of the main checkout, not the original one),
// rebuilds the task-manager system prompt from current bead state (the
// transcript restores no system prompt, which is why a raw `claude --resume`
// came back as a plain Claude session), and hands the CLI --resume plus
// bypassPermissions — never --session-id.
func TestHandleTask_ResumeLaunchesInFreshWorktreeWithRebuiltPrompt(t *testing.T) {
	projectDir := newRepoWithOrigin(t)
	ralphDir := filepath.Join(projectDir, ".ralph")
	const sessionID = "12345678-1234-4567-89ab-123456789abc"

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

	cwd := launchCWD(t, out)
	worktreesDir := resolved(t, filepath.Join(ralphDir, "worktrees"))
	if !strings.HasPrefix(cwd, worktreesDir+string(filepath.Separator)) {
		t.Errorf("resumed session should run in a worktree under %s, ran in %s", worktreesDir, cwd)
	}
	if cwd == resolved(t, projectDir) {
		t.Errorf("resumed session must never run in the main checkout %s", projectDir)
	}
}

// launchCWD extracts the working directory the claude shim was launched in.
func launchCWD(t *testing.T, invocation string) string {
	t.Helper()
	for _, line := range strings.Split(invocation, "\n") {
		if after, ok := strings.CutPrefix(line, "CWD="); ok {
			return after
		}
	}
	t.Fatalf("no CWD recorded in claude invocation:\n%s", invocation)
	return ""
}

// resolved reports path with symlinks expanded, matching the physical path a
// launched process reports as its working directory (macOS temp dirs are
// reached through a symlink).
func resolved(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// newRepoWithOrigin builds a git repo with an origin remote carrying main,
// which is what worktree creation validates against before it will run.
func newRepoWithOrigin(t *testing.T) string {
	t.Helper()
	originDir := t.TempDir()
	runCmd(t, "git", "init", "--bare", "--initial-branch=main", originDir)

	projectDir := t.TempDir()
	runCmd(t, "git", "init", "--initial-branch=main", projectDir)
	runCmd(t, "git", "-C", projectDir, "commit", "--allow-empty", "-m", "init")
	runCmd(t, "git", "-C", projectDir, "remote", "add", "origin", originDir)
	runCmd(t, "git", "-C", projectDir, "push", "-u", "origin", "main")
	return projectDir
}

func writeShim(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
