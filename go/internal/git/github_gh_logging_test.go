package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ghCLI emits a Warn log entry containing the stub's stderr content when
// gh exits non-zero, so operators see what went wrong without parsing Go error strings.
func TestGhCLI_LogsStderrOnNonZeroExit(t *testing.T) {
	bin := t.TempDir()
	ghPath := filepath.Join(bin, "gh")
	script := "#!/bin/sh\nprintf 'auth required' >&2\nexit 4\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	log := &testLog{}
	g := &ghCLI{logger: log}
	_, _ = g.FindOpenPR(context.Background(), "my-branch", "https://github.com/owner/repo.git")

	if !log.contains("auth required") {
		t.Errorf("expected warn log containing 'auth required', got: %v", log.messages)
	}
}

// ghCLI emits a Warn log entry with 'context deadline exceeded' when
// the context is cancelled while gh is running, so operators can distinguish
// timeouts from auth failures or server errors.
func TestGhCLI_LogsContextDeadlineExceeded(t *testing.T) {
	bin := t.TempDir()
	ghPath := filepath.Join(bin, "gh")
	// exec sleep 30 replaces the shell with sleep so SIGKILL hits the process
	// holding the pipe fds — the pipe closes immediately and cmd.Output() returns.
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	log := &testLog{}
	g := &ghCLI{logger: log}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _ = g.FindOpenPR(ctx, "my-branch", "https://github.com/owner/repo.git")

	if !log.contains("context deadline exceeded") {
		t.Errorf("expected warn log containing 'context deadline exceeded', got: %v", log.messages)
	}
}

// deleteBranch emits one concise info line — not the full gh failure dump —
// when the ref-delete call 422s with "Reference does not exist", since that
// means GitHub's "automatically delete head branches" setting already raced
// ralph to remove the branch. This is a benign, expected outcome, not a
// warning-worthy failure.
func TestDeleteBranch_RefAlreadyDeleted_LogsConciseLine(t *testing.T) {
	bin := t.TempDir()
	ghPath := filepath.Join(bin, "gh")
	script := `#!/bin/sh
case "$*" in
  *"--method DELETE"*)
    printf 'gh: Reference does not exist (HTTP 422)\n' >&2
    printf '{"message":"Reference does not exist","documentation_url":"https://docs.github.com/rest/git/refs#delete-a-reference","status":"422"}'
    exit 1
    ;;
  *)
    printf 'ralph/my-task-branch'
    exit 0
    ;;
esac
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	log := &testLog{}
	g := &ghCLI{logger: log}
	g.deleteBranch(context.Background(), "owner/repo", "42")

	if !log.contains("ralph/my-task-branch") || !log.contains("already deleted") {
		t.Errorf("expected concise 'already deleted' log for ralph/my-task-branch, got: %v", log.messages)
	}
	if log.contains(`"message":"Reference does not exist"`) {
		t.Errorf("expected no raw JSON body in log, got: %v", log.messages)
	}
	if log.contains("failed after") {
		t.Errorf("expected no full gh failure dump in log, got: %v", log.messages)
	}
	if log.contains("WARN:") {
		t.Errorf("expected no Warn-level log entry, got: %v", log.messages)
	}
}

// deleteBranch still emits the full gh failure dump for ref-delete failures
// other than the benign 422 "Reference does not exist" race — e.g. a 403
// permission error is a real failure worth a warning.
func TestDeleteBranch_OtherFailure_LogsFullDump(t *testing.T) {
	bin := t.TempDir()
	ghPath := filepath.Join(bin, "gh")
	script := `#!/bin/sh
case "$*" in
  *"--method DELETE"*)
    printf 'gh: Resource not accessible by integration (HTTP 403)\n' >&2
    printf '{"message":"Resource not accessible by integration","status":"403"}'
    exit 1
    ;;
  *)
    printf 'ralph/my-task-branch'
    exit 0
    ;;
esac
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	log := &testLog{}
	g := &ghCLI{logger: log}
	g.deleteBranch(context.Background(), "owner/repo", "42")

	if !log.contains("Resource not accessible by integration") {
		t.Errorf("expected full gh failure dump with stderr content, got: %v", log.messages)
	}
	if !log.contains("failed after") {
		t.Errorf("expected full gh failure dump marker 'failed after', got: %v", log.messages)
	}
	if !log.contains("WARN:") {
		t.Errorf("expected Warn-level log entry, got: %v", log.messages)
	}
}
