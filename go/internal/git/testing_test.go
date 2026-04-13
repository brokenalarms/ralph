package git

import "testing"

// Proves StubGitHub satisfies the gitHub interface at compile time.
func TestStubGitHub_ImplementsGitHub(t *testing.T) {
	var _ gitHub = (*StubGitHub)(nil)
}

// NewStubGitHub returns a stub with the defaults that represent the overwhelmingly
// common case: available GitHub, open PR #42 against main, merges successfully.
func TestNewStubGitHub_Defaults(t *testing.T) {
	gh := NewStubGitHub()

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
	gh := &StubGitHub{}

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


