package git

import (
	"errors"
	"testing"
	"time"
)

// The merge/CI decision path returns typed errors rather than formatted
// strings (docs/specs/architecture.md Principle 7). Converting them must not
// change what the loop logs, so each type renders exactly the message the
// inline fmt.Errorf produced for the same inputs.
func TestMergePathErrorMessages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "auto-merge blocked",
			err:  &AutoMergeBlockedError{PRNumber: 42, Message: "required status check missing"},
			want: "auto-merge blocked for PR #42: required status check missing",
		},
		{
			name: "merge retry failed",
			err:  &MergeRetryFailedError{PRNumber: 42, Message: "not mergeable"},
			want: "merge retry failed for PR #42 after CI passed: not mergeable",
		},
		{
			name: "CI incomplete",
			err:  &CIIncompleteError{PRNumber: 42, Waited: 5 * time.Minute},
			want: "CI checks did not complete within 5m0s",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The pre-push compile check reports the failure reason plus a summary of the
// compiler output. As a typed error it must still render both, so the Build
// log line the loop prints is unchanged.
func TestCompileCheckErrorMessage(t *testing.T) {
	err := &CompileCheckError{Reason: "compile check failed", Details: "./main.go:3:2: undefined: foo"}
	want := "compile check failed\n" + compileCheckSummary("./main.go:3:2: undefined: foo")
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// The base-branch guard is a merge-blocking outcome, so a caller can now
// recognize it with errors.Is instead of matching on the message — while the
// message itself still names the guard and the branches involved.
func TestAssertValidBaseReturnsSentinel(t *testing.T) {
	repo := newRepoForTest(Config{BaseBranch: "main"}, nil)

	err := repo.assertValidBase("some-other-branch")
	if err == nil {
		t.Fatal("expected an error for a base that is neither BaseBranch nor the stack parent")
	}
	if !errors.Is(err, ErrInvalidBaseBranch) {
		t.Errorf("expected errors.Is(err, ErrInvalidBaseBranch), got: %v", err)
	}
	want := `base branch guard: resolved base "some-other-branch" is neither cfg.BaseBranch ("main") nor active stack parent "" — refusing to create/merge PR`
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// A merge that lands on a lineage unreachable from the base branch is the
// guard that keeps a bead open for manual recovery. It is errors.Is-able so a
// caller never has to grep the message for "FAILED".
func TestAssertMergedAncestorReturnsSentinel(t *testing.T) {
	runner := newStubRunner().On("merge-base --is-ancestor", "", errors.New("not an ancestor"))
	repo := newRepoForTest(Config{ProjectDir: "/project", BaseBranch: "main"}, nil, withRunner(runner))

	err := repo.assertMergedAncestor("deadbeef")
	if err == nil {
		t.Fatal("expected an error when the merged SHA is not an ancestor of origin/main")
	}
	if !errors.Is(err, ErrMergedSHANotAncestor) {
		t.Errorf("expected errors.Is(err, ErrMergedSHANotAncestor), got: %v", err)
	}
}
