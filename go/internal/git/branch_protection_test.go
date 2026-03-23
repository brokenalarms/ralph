package git

import (
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
