package git

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// A PR merged out-of-band while gateOnCI is waiting for CI is the same
// outcome as the pre-wait already-merged path: the work landed, so the gate
// reports success and never attempts a merge of its own. Attempting one
// would fail against the merged PR and leave the bead open despite the code
// being on main.
func TestGateOnCI_PRMergedDuringWaitSkipsMerge(t *testing.T) {
	stubCISleep(t)

	const prNumber = 7
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: prNumber, Branch: "ralph/task", Base: "main", State: PRStateMerged},
		},
		Checks: map[int][]CICheckResult{
			prNumber: {{Name: "test", State: "PENDING", Bucket: "pending"}},
		},
		// Non-zero so the CI-infrastructure escape hatches stay closed and
		// the test exercises the merged-mid-wait path only.
		JobStepCount: 3,
	})
	log := &testLog{}
	repo := newRepoForTest(Config{
		Logger:        log,
		BaseBranch:    "main",
		CIPollTimeout: 5 * time.Millisecond,
	}, gh)
	repo.worktreeBranch = "ralph/task"

	repoURL := "https://github.com/owner/repo"
	prLink := logging.PRLinkOpt(NWOFromRemote(repoURL), prNumber)

	merged, err := repo.gateOnCI(context.Background(), prNumber, repoURL, prLink, time.Time{})
	if err != nil {
		t.Fatalf("expected the merged-mid-wait PR to resolve cleanly, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true for a PR merged while waiting for CI")
	}

	var announced bool
	for _, msg := range log.messages {
		if strings.Contains(msg, "Auto-merge failed") || strings.Contains(msg, "blocked by branch protection") {
			t.Errorf("expected no merge attempt on an already-merged PR, got: %s", msg)
		}
		if strings.Contains(msg, "merged while waiting for CI") {
			announced = true
			if !strings.Contains(msg, "owner/repo/pull/7") {
				t.Errorf("expected the PR link on the merged-mid-wait line, got: %s", msg)
			}
		}
	}
	if !announced {
		t.Errorf("expected a log line reporting the out-of-band merge, got: %v", log.messages)
	}
}
