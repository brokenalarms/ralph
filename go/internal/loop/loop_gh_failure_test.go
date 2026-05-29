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

// When Ship hits a gh failure, ralph's log captures the stub's stderr content
// so operators can diagnose auth failures, rate limits, and other gh errors
// without grepping the raw process output.
func TestLoop_GhFailure_CapturesStderrInLog(t *testing.T) {
	setup := newGitIntegrationSetup(t)

	bin := t.TempDir()
	ghPath := filepath.Join(bin, "gh")
	script := "#!/bin/sh\nprintf 'stub error: rate limit exceeded' >&2\nexit 1\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	gm := git.New(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir,
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	})

	// PRNumber: 1 routes Ship through GetPRState → gh api, which hits the stub
	// binary. The stub writes to stderr and exits 1, triggering logGHFailure.
	_, _ = gm.Ship(context.Background(), git.ShipOpts{PRNumber: 1})

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "stub error: rate limit exceeded") {
		t.Errorf("expected log to contain stub stderr content, got: %s", logOutput)
	}
}
