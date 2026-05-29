package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// TestGHCLI_LogsStderrOnFailure proves that when gh exits non-zero, the warn
// log entry is emitted containing the stderr content.
func TestGHCLI_LogsStderrOnFailure(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'auth required' >&2\nexit 4\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	var logBuf bytes.Buffer
	g := &ghCLI{logger: logging.NewWithWriter(&logBuf)}
	_, _ = g.FindOpenPR(context.Background(), "some-branch", "https://github.com/owner/repo.git")

	if !strings.Contains(logBuf.String(), "auth required") {
		t.Errorf("expected stderr content 'auth required' in log, got:\n%s", logBuf.String())
	}
}

// TestGHCLI_LogsContextDeadlineExceeded proves that when the context times out
// while gh is running, the warn log entry is emitted with err containing
// "context deadline exceeded".
func TestGHCLI_LogsContextDeadlineExceeded(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'connecting' >&2\nexec sleep 30\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	var logBuf bytes.Buffer
	g := &ghCLI{logger: logging.NewWithWriter(&logBuf)}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _ = g.FindOpenPR(ctx, "some-branch", "https://github.com/owner/repo.git")

	if !strings.Contains(logBuf.String(), "context deadline exceeded") {
		t.Errorf("expected 'context deadline exceeded' in log, got:\n%s", logBuf.String())
	}
}
