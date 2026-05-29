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
