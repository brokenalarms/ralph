package git

import (
	"context"
	"testing"
)

// GetFailedJobAnnotations resolves the workflow run via the PR's actual head
// SHA (fetched from the pulls endpoint) — not the latest repo-wide
// pull_request run, which belongs to whichever PR pushed most recently — and
// reports the failure-level check-run annotations of each failed job, keyed by
// job id. This is what lets the loop tell a step timeout ("has timed out
// after") from a real test failure: both report the step's conclusion as
// "failure", only the timeout carries the annotation. The fake gh script
// answers the runs query only when queried with the head SHA; any other query
// falls through to the error branch and fails the test.
func TestGetFailedJobAnnotations_ResolvesRunByHeadSHAAndReadsAnnotations(t *testing.T) {
	writeFakeGH(t, `
args="$*"
case "$args" in
  *"repos/owner/repo/pulls/149"*)
    echo "headsha149"
    ;;
  *"repos/owner/repo/actions/runs?head_sha=headsha149&per_page=1"*)
    echo "32277887688"
    ;;
  *"repos/owner/repo/actions/runs/32277887688/jobs"*)
    cat <<'JOBS'
[{"id":98765,"name":"test"}]
JOBS
    ;;
  *"repos/owner/repo/check-runs/98765/annotations"*)
    cat <<'ANN'
["The action 'Install system tools' has timed out after 5 minutes."]
ANN
    ;;
  *)
    echo "unexpected gh invocation: $args" >&2
    exit 1
    ;;
esac
`)

	g := &ghCLI{logger: &testLog{}}
	jobs, err := g.GetFailedJobAnnotations(context.Background(), "owner/repo", 149)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected annotations for 1 failed job, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].JobName != "test" {
		t.Errorf("JobName = %q, want %q", jobs[0].JobName, "test")
	}
	if len(jobs[0].Messages) != 1 || !hasStepTimeoutAnnotation(jobs[0].Messages) {
		t.Errorf("Messages = %q, want the step-timeout annotation", jobs[0].Messages)
	}
}

// A run whose failed job produced a real assertion failure carries no
// timeout annotation, so hasStepTimeoutAnnotation rejects it and the loop
// keeps routing that failure to the fix agent.
func TestGetFailedJobAnnotations_RealFailureHasNoTimeoutAnnotation(t *testing.T) {
	writeFakeGH(t, `
args="$*"
case "$args" in
  *"repos/owner/repo/pulls/7"*)
    echo "sha7"
    ;;
  *"repos/owner/repo/actions/runs?head_sha=sha7&per_page=1"*)
    echo "42"
    ;;
  *"repos/owner/repo/actions/runs/42/jobs"*)
    cat <<'JOBS'
[{"id":11,"name":"test"}]
JOBS
    ;;
  *"repos/owner/repo/check-runs/11/annotations"*)
    cat <<'ANN'
["Process completed with exit code 1."]
ANN
    ;;
  *)
    echo "unexpected gh invocation: $args" >&2
    exit 1
    ;;
esac
`)

	g := &ghCLI{logger: &testLog{}}
	jobs, err := g.GetFailedJobAnnotations(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected annotations for 1 failed job, got %d: %+v", len(jobs), jobs)
	}
	if hasStepTimeoutAnnotation(jobs[0].Messages) {
		t.Errorf("Messages = %q must not classify as a step timeout", jobs[0].Messages)
	}
}
