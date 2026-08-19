package git

import (
	"context"
	"strings"
	"testing"
)

// CheckBranchNamespaceSquat returns nil when origin has no ref at the bare
// ralph/ namespace root — the common case, proving the check does not
// false-positive on a normal repo where every ralph branch is properly
// prefixed (ralph/<task>).
func TestCheckBranchNamespaceSquat_NoSquattingRef_ReturnsNil(t *testing.T) {
	r := newStubRunner()
	r.On("ls-remote origin refs/heads/ralph", "", nil)

	repo := newRepoForTest(Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}}, nil, withRunner(r))

	if err := repo.CheckBranchNamespaceSquat(context.Background()); err != nil {
		t.Errorf("expected nil error when no squatting ref exists, got %v", err)
	}
}

// CheckBranchNamespaceSquat returns an error naming the ref and its SHA when
// origin has a bare branch named "ralph" — this is the fossil ref that
// blocks every ralph/<beadID>-<slug> push with a "directory file conflict"
// rejection, since git refs are path-like.
func TestCheckBranchNamespaceSquat_SquattingRefPresent_ReturnsNamedError(t *testing.T) {
	r := newStubRunner()
	r.On("ls-remote origin refs/heads/ralph", "1234567890abcdef1234567890abcdef12345678\trefs/heads/ralph", nil)

	repo := newRepoForTest(Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}}, nil, withRunner(r))

	err := repo.CheckBranchNamespaceSquat(context.Background())
	if err == nil {
		t.Fatal("expected error when a squatting ref exists on origin")
	}
	if !strings.Contains(err.Error(), "refs/heads/ralph") {
		t.Errorf("expected error to name the ref refs/heads/ralph, got: %v", err)
	}
	if !strings.Contains(err.Error(), "1234567890abcdef1234567890abcdef12345678") {
		t.Errorf("expected error to name the SHA, got: %v", err)
	}
}
