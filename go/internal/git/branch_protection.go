package git

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
)

// repoNWO extracts the "owner/repo" name-with-owner from a git remote URL.
// Supports both HTTPS and SSH formats:
//   - https://github.com/owner/repo.git
//   - git@github.com:owner/repo.git
func repoNWO(remoteURL string) string {
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
func (r *Repo) EnforceAdmins() error {
	dir := r.workDir
	if dir == "" {
		dir = r.projectDir
	}

	remoteURL := r.gitOutput(dir, "remote", "get-url", "origin")
	if remoteURL == "" {
		return nil
	}

	nwo := repoNWO(remoteURL)
	if nwo == "" {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Could not parse repo owner/name from remote URL — skipping enforce_admins")
		return nil
	}

	branch := r.detectDefaultBranch()
	gh := r.github

	enforced, err := gh.CheckEnforceAdmins(nwo, branch)
	if err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Could not check enforce_admins status: %v — skipping", err)
		return nil
	}
	if enforced {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Branch protection: enforce_admins already enabled on %s", branch)
		return nil
	}

	output, err := gh.PostEnforceAdmins(nwo, branch)
	if err != nil {
		if strings.Contains(output, "Branch not protected") || strings.Contains(output, "Not Found") {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No branch protection rules on %s — cannot enable enforce_admins. Configure branch protection in GitHub settings first.", branch)
			return nil
		}
		return fmt.Errorf("failed to enable enforce_admins on %s: %s", branch, output)
	}

	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Branch protection: enabled enforce_admins on %s", branch)
	return nil
}
