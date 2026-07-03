package loop

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// The production Connectivity default (nil Modules.Connectivity) must
// delegate its GitHub reachability check to the injected git.Ops.PingGitHub
// method rather than shelling out to `gh` itself — proves the loop package
// no longer owns the GitHub connectivity check directly.
func TestLiveConnectivity_CheckGitHub_DelegatesToGitOps(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	sentinel := errors.New("stub ping failure")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		GitHub:     git.StubGitHubConfig{PingErr: sentinel},
	})

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:    st,
		Git:      gm,
		Logger:   logger,
		Verifier: newTestVerifier(t, cfg, logger),
		// Connectivity intentionally left nil to exercise the production
		// liveConnectivity default.
	})

	err := l.connectivity.CheckGitHub(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("expected CheckGitHub to return the git.Ops.PingGitHub error via the injected interface, got %v", err)
	}
}
