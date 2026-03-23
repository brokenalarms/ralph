package git

import (
	"fmt"
	"strings"
	"testing"
)

// repoNWO correctly extracts owner/repo from HTTPS remote URLs,
// which is the standard format for GitHub repositories.
func TestRepoNWO_HTTPS(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/owner/repo.git", "owner/repo"},
		{"https://github.com/owner/repo", "owner/repo"},
		{"https://github.com/org/some-repo.git", "org/some-repo"},
	}
	for _, tt := range tests {
		got := repoNWO(tt.url)
		if got != tt.want {
			t.Errorf("repoNWO(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// repoNWO correctly extracts owner/repo from SSH remote URLs,
// which are common for authenticated git operations.
func TestRepoNWO_SSH(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"git@github.com:owner/repo.git", "owner/repo"},
		{"git@github.com:owner/repo", "owner/repo"},
		{"git@github.com:org/some-repo.git", "org/some-repo"},
	}
	for _, tt := range tests {
		got := repoNWO(tt.url)
		if got != tt.want {
			t.Errorf("repoNWO(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// repoNWO returns empty string for non-GitHub URLs and empty input,
// preventing invalid API calls against non-GitHub remotes.
func TestRepoNWO_NonGitHub(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"", ""},
		{"https://gitlab.com/owner/repo.git", ""},
		{"not-a-url", ""},
	}
	for _, tt := range tests {
		got := repoNWO(tt.url)
		if got != tt.want {
			t.Errorf("repoNWO(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// EnforceAdmins returns nil without making API calls when there is no
// remote URL, ensuring ralph works in repos without a GitHub remote.
func TestEnforceAdmins_NoRemote(t *testing.T) {
	tmp := t.TempDir()
	run(t, "git", "init", tmp)

	log := &testLog{}
	mgr := &Manager{
		ProjectDir: tmp,
		WorkDir:    tmp,
		Logger:     log,
	}

	err := mgr.EnforceAdmins()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func withMockAPI(t *testing.T, check func(string, string) (bool, error), post func(string, string) (string, error)) {
	t.Helper()
	origCheck := checkEnforceAdmins
	origPost := postEnforceAdmins
	checkEnforceAdmins = check
	postEnforceAdmins = post
	t.Cleanup(func() {
		checkEnforceAdmins = origCheck
		postEnforceAdmins = origPost
	})
}

func enforceAdminsManager(t *testing.T) (*Manager, *testLog) {
	t.Helper()
	project, _ := initBareRepo(t)
	run(t, "git", "-C", project, "remote", "set-url", "origin", "https://github.com/owner/repo.git")

	log := &testLog{}
	mgr := &Manager{
		ProjectDir: project,
		WorkDir:    project,
		Logger:     log,
	}
	return mgr, log
}

func TestEnforceAdmins_EnablesSuccessfully(t *testing.T) {
	mgr, log := enforceAdminsManager(t)
	var postCalled bool
	withMockAPI(t,
		func(nwo, branch string) (bool, error) { return false, nil },
		func(nwo, branch string) (string, error) {
			postCalled = true
			return `{"enabled":true}`, nil
		},
	)

	err := mgr.EnforceAdmins()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !postCalled {
		t.Fatal("expected POST to be called")
	}
	if !log.contains("enabled enforce_admins") {
		t.Fatalf("expected success log, got: %v", log.messages)
	}
}

func TestEnforceAdmins_AlreadyEnabled(t *testing.T) {
	mgr, log := enforceAdminsManager(t)
	var postCalled bool
	withMockAPI(t,
		func(nwo, branch string) (bool, error) { return true, nil },
		func(nwo, branch string) (string, error) {
			postCalled = true
			return "", nil
		},
	)

	err := mgr.EnforceAdmins()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if postCalled {
		t.Fatal("POST should not be called when already enabled")
	}
	if !log.contains("already enabled") {
		t.Fatalf("expected already-enabled log, got: %v", log.messages)
	}
}

func TestEnforceAdmins_BranchNotProtected(t *testing.T) {
	mgr, log := enforceAdminsManager(t)
	withMockAPI(t,
		func(nwo, branch string) (bool, error) { return false, nil },
		func(nwo, branch string) (string, error) {
			return "Branch not protected", fmt.Errorf("exit status 1")
		},
	)

	err := mgr.EnforceAdmins()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !log.contains("No branch protection rules") {
		t.Fatalf("expected branch-not-protected warning, got: %v", log.messages)
	}
}

func TestEnforceAdmins_CheckAPIError(t *testing.T) {
	mgr, log := enforceAdminsManager(t)
	withMockAPI(t,
		func(nwo, branch string) (bool, error) {
			return false, fmt.Errorf("network timeout")
		},
		func(nwo, branch string) (string, error) {
			t.Fatal("POST should not be called when check fails")
			return "", nil
		},
	)

	err := mgr.EnforceAdmins()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !log.contains("Could not check enforce_admins status") {
		t.Fatalf("expected check-error warning, got: %v", log.messages)
	}
}

func TestEnforceAdmins_PostAPIError(t *testing.T) {
	mgr, _ := enforceAdminsManager(t)
	withMockAPI(t,
		func(nwo, branch string) (bool, error) { return false, nil },
		func(nwo, branch string) (string, error) {
			return "internal server error", fmt.Errorf("exit status 1")
		},
	)

	err := mgr.EnforceAdmins()
	if err == nil {
		t.Fatal("expected error for non-branch-protection API failure")
	}
	if expected := "failed to enable enforce_admins"; !strings.Contains(err.Error(), expected) {
		t.Fatalf("expected error containing %q, got: %v", expected, err)
	}
}
