package loop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

// TestGHFailure_LogsStderrToRalphLog proves that when the loop passes its
// logger to git.New() and gh fails, the loop's logger captures the gh stderr
// content.
func TestGHFailure_LogsStderrToRalphLog(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'gh: auth required' >&2\nexit 1\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	setup := newGitIntegrationSetup(t)

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	g := git.New(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir,
		BaseBranch: "main",
		Logger:     logger,
	})

	_, _ = g.FindOpenPRForBranch(context.Background(), "some-branch")

	if !strings.Contains(logBuf.String(), "gh: auth required") {
		t.Errorf("expected 'gh: auth required' in log output, got:\n%s", logBuf.String())
	}
}
