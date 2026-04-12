package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Proves stubGitHub satisfies the gitHub interface at compile time.
func TestStubGitHub_ImplementsGitHub(t *testing.T) {
	var _ gitHub = (*stubGitHub)(nil)
}

// newStubGitHub returns a stub with the defaults that represent the overwhelmingly
// common case: available GitHub, open PR #42 against main, merges successfully.
func TestNewStubGitHub_Defaults(t *testing.T) {
	gh := newStubGitHub()

	if !gh.Available() {
		t.Error("IsAvailable should default to true")
	}
	n, _ := gh.FindOpenPR("branch", "")
	if n != 42 {
		t.Errorf("OpenPR should default to 42, got %d", n)
	}
	pr, _ := gh.GetPR("", 42)
	if pr.BaseRef != "main" {
		t.Errorf("PRBase should default to main, got %q", pr.BaseRef)
	}
	if pr.State != "OPEN" {
		t.Errorf("PRState should default to OPEN, got %q", pr.State)
	}
	result := gh.MergePR(42, "", MergeOpts{})
	if !result.Merged {
		t.Error("MergeResult should default to Merged=true")
	}
}

// GetPR auto-generates a deterministic HeadSHA when HeadSHA is not configured,
// so tests do not need to set it explicitly for non-fast-path scenarios.
func TestStubGitHub_GetPR_AutoGeneratesHeadSHA(t *testing.T) {
	gh := &stubGitHub{}

	pr, _ := gh.GetPR("", 42)
	if pr.HeadSHA != "stub-sha-42" {
		t.Errorf("expected auto-generated HeadSHA %q, got %q", "stub-sha-42", pr.HeadSHA)
	}

	// Explicit HeadSHA takes precedence.
	gh.HeadSHA = "explicit-sha"
	pr, _ = gh.GetPR("", 42)
	if pr.HeadSHA != "explicit-sha" {
		t.Errorf("explicit HeadSHA should not be overridden, got %q", pr.HeadSHA)
	}
}

// TestGitHubIsInternal proves that git.Ops and git.StubRepo are not referenced
// in any .go file outside go/internal/git/. These types were deleted as part of
// collapsing the git abstraction — the compiler enforces it, but this test
// provides a readable failure message if they somehow reappear.
func TestGitHubIsInternal(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	// goDir is go/  (two levels up from go/internal/git/)
	goDir := filepath.Join(filepath.Dir(thisFile), "..", "..")

	forbidden := []string{"git.Ops", "git.StubRepo"}

	err := filepath.Walk(goDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip test files — arch tests legitimately reference these as string
		// literals in pattern guards, not as actual type references.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip files within internal/git/ itself.
		rel, _ := filepath.Rel(goDir, path)
		if strings.HasPrefix(rel, "internal/git/") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(content)
		for _, pattern := range forbidden {
			if strings.Contains(src, pattern) {
				t.Errorf("%s: references %q — GitHub type must stay internal to go/internal/git/", rel, pattern)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk error: %v", err)
	}
}
