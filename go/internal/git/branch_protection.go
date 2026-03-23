package git

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// repoNWO extracts the "owner/repo" name-with-owner from a git remote URL.
// Supports both HTTPS and SSH formats:
//   - https://github.com/owner/repo.git
//   - git@github.com:owner/repo.git
func repoNWO(remoteURL string) string {
	// Try gh repo view first — most reliable when gh is authenticated.
	// Falls back to URL parsing if gh isn't available.
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return ""
	}

	// SSH format: git@github.com:owner/repo.git
	if strings.HasPrefix(remoteURL, "git@") {
		re := regexp.MustCompile(`git@[^:]+:(.+?)(?:\.git)?$`)
		if m := re.FindStringSubmatch(remoteURL); len(m) > 1 {
			return m[1]
		}
	}

	// HTTPS format: https://github.com/owner/repo.git
	re := regexp.MustCompile(`github\.com[/:](.+?)(?:\.git)?$`)
	if m := re.FindStringSubmatch(remoteURL); len(m) > 1 {
		return m[1]
	}

	return ""
}

// EnforceAdmins enables the "Include administrators" setting on the base
// branch's protection rules. This prevents admin accounts (including ralph's
// user) from bypassing required status checks when merging PRs.
//
// Requires: gh CLI authenticated with admin access to the repo, and existing
// branch protection rules on the target branch.
func (m *Manager) EnforceAdmins() error {
	dir := m.WorkDir
	if dir == "" {
		dir = m.ProjectDir
	}

	remoteURL := gitOutput(dir, "remote", "get-url", "origin")
	if remoteURL == "" {
		return nil
	}

	nwo := repoNWO(remoteURL)
	if nwo == "" {
		m.Logger.Warn("Could not parse repo owner/name from remote URL — skipping enforce_admins")
		return nil
	}

	branch := detectDefaultBranch(m.ProjectDir, m.BaseBranch)

	// Check current state first to avoid unnecessary API calls.
	enforced, err := isEnforceAdminsEnabled(nwo, branch)
	if err != nil {
		m.Logger.Warn("Could not check enforce_admins status: %v — skipping", err)
		return nil
	}
	if enforced {
		m.Logger.Log("Branch protection: enforce_admins already enabled on %s", branch)
		return nil
	}

	// Enable enforce_admins via GitHub API.
	endpoint := fmt.Sprintf("/repos/%s/branches/%s/protection/enforce_admins", nwo, branch)
	cmd := exec.Command("gh", "api", "-X", "POST", endpoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.TrimSpace(string(out))
		if strings.Contains(output, "Branch not protected") || strings.Contains(output, "Not Found") {
			m.Logger.Warn("No branch protection rules on %s — cannot enable enforce_admins. Configure branch protection in GitHub settings first.", branch)
			return nil
		}
		return fmt.Errorf("failed to enable enforce_admins on %s: %s", branch, output)
	}

	m.Logger.Log("Branch protection: enabled enforce_admins on %s", branch)
	return nil
}

// enforceAdminsResponse is the JSON shape returned by the GitHub API for
// the enforce_admins endpoint.
type enforceAdminsResponse struct {
	Enabled bool `json:"enabled"`
}

// isEnforceAdminsEnabled checks whether enforce_admins is currently enabled
// on a branch's protection rules.
func isEnforceAdminsEnabled(nwo, branch string) (bool, error) {
	endpoint := fmt.Sprintf("/repos/%s/branches/%s/protection/enforce_admins", nwo, branch)
	cmd := exec.Command("gh", "api", endpoint)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("gh api failed: %w", err)
	}

	var resp enforceAdminsResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return false, fmt.Errorf("parsing response: %w", err)
	}
	return resp.Enabled, nil
}
