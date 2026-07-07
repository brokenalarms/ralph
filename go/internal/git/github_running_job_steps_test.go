package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeGH writes a fake "gh" executable to a temp dir and prepends it to
// PATH for the duration of the test, so ghCLI methods can be exercised
// without a real gh CLI or network access. script is a POSIX shell script body.
func writeFakeGH(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// GetRunningJobSteps resolves the workflow run via the PR's actual head SHA
// (fetched from the pulls endpoint) rather than the latest repo-wide
// pull_request run, so a concurrent run for a different PR can never supply
// the wrong step names. The fake gh script only answers the runs query when
// it is queried with the head SHA resolved from the pulls endpoint — any
// other query (e.g. the "latest repo-wide run" query GetJobStepCount uses)
// falls through to the error branch and fails the test.
func TestGetRunningJobSteps_ResolvesRunByHeadSHA(t *testing.T) {
	writeFakeGH(t, `
args="$*"
case "$args" in
  *"repos/owner/repo/pulls/42"*)
    echo "abc123sha"
    ;;
  *"repos/owner/repo/actions/runs?head_sha=abc123sha&per_page=1"*)
    echo "555"
    ;;
  *"repos/owner/repo/actions/runs/555/jobs"*)
    cat <<'JOBS'
[{"name":"test","steps":[{"name":"Install deps","number":1,"status":"completed"},{"name":"Run Playwright tests","number":5,"status":"in_progress"}]}]
JOBS
    ;;
  *)
    echo "unexpected gh invocation: $args" >&2
    exit 1
    ;;
esac
`)

	g := &ghCLI{logger: &testLog{}}
	steps, err := g.GetRunningJobSteps(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("expected 1 running step, got %d: %+v", len(steps), steps)
	}
	got := steps[0]
	want := JobStepStatus{JobName: "test", StepName: "Run Playwright tests", StepIndex: 5, StepTotal: 2}
	if got != want {
		t.Errorf("GetRunningJobSteps() = %+v, want %+v", got, want)
	}
}

// GetRunningJobSteps returns one JobStepStatus per job that has a step
// currently in_progress, covering the multi-job case (e.g. a matrix build
// with separate lint/test jobs both mid-run).
func TestGetRunningJobSteps_MultiJob(t *testing.T) {
	writeFakeGH(t, `
args="$*"
case "$args" in
  *"repos/owner/repo/pulls/7"*)
    echo "sha7"
    ;;
  *"repos/owner/repo/actions/runs?head_sha=sha7&per_page=1"*)
    echo "900"
    ;;
  *"repos/owner/repo/actions/runs/900/jobs"*)
    cat <<'JOBS'
[
  {"name":"test","steps":[{"name":"Run Playwright tests","number":5,"status":"in_progress"}]},
  {"name":"lint","steps":[{"name":"Run eslint","number":2,"status":"in_progress"},{"name":"Setup","number":1,"status":"completed"}]},
  {"name":"build","steps":[{"name":"Compile","number":1,"status":"completed"}]}
]
JOBS
    ;;
  *)
    echo "unexpected gh invocation: $args" >&2
    exit 1
    ;;
esac
`)

	g := &ghCLI{logger: &testLog{}}
	steps, err := g.GetRunningJobSteps(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 running steps (test, lint — build has none in_progress), got %d: %+v", len(steps), steps)
	}
	byJob := make(map[string]JobStepStatus, len(steps))
	for _, s := range steps {
		byJob[s.JobName] = s
	}
	if got := byJob["test"]; got != (JobStepStatus{JobName: "test", StepName: "Run Playwright tests", StepIndex: 5, StepTotal: 1}) {
		t.Errorf("unexpected test job step: %+v", got)
	}
	if got := byJob["lint"]; got != (JobStepStatus{JobName: "lint", StepName: "Run eslint", StepIndex: 2, StepTotal: 2}) {
		t.Errorf("unexpected lint job step: %+v", got)
	}
	if _, ok := byJob["build"]; ok {
		t.Errorf("expected no entry for build job (no in_progress step), got %+v", byJob["build"])
	}
}

// GetRunningJobSteps returns an error when the gh invocation fails, so
// callers (waitForCI's stepFetch) can fall back to plain check-name
// rendering instead of crashing or silently showing stale step data.
func TestGetRunningJobSteps_ErrorOnGHFailure(t *testing.T) {
	writeFakeGH(t, `echo "boom" >&2; exit 1`)

	g := &ghCLI{logger: &testLog{}}
	_, err := g.GetRunningJobSteps(context.Background(), "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error when gh invocation fails")
	}
	if !strings.Contains(err.Error(), "PR head SHA") {
		t.Errorf("expected error to mention PR head SHA lookup failure, got: %v", err)
	}
}
