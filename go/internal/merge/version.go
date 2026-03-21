package merge

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// BumpPatchTag reads the latest vX.Y.Z tag from the repo, increments the
// patch component, creates the new tag, and pushes it. The ref parameter
// specifies which commit to tag (e.g. "origin/main"); empty string tags HEAD.
// Returns the new tag string (e.g. "v0.1.1") or an error. If no version tag
// exists, starts from v0.1.0 and bumps to v0.1.1.
func BumpPatchTag(projectDir, ref string) (string, error) {
	latest, err := LatestVersionTag(projectDir)
	if err != nil {
		return "", err
	}

	major, minor, patch, err := ParseVersion(latest)
	if err != nil {
		return "", err
	}

	newTag := fmt.Sprintf("v%d.%d.%d", major, minor, patch+1)

	if err := createAndPushTag(newTag, projectDir, ref); err != nil {
		return "", err
	}

	return newTag, nil
}

// LatestVersionTag returns the most recent vX.Y.Z tag in the repository,
// sorted by semantic version. If no version tags exist, returns "v0.1.0"
// as the baseline.
func LatestVersionTag(projectDir string) (string, error) {
	cmd := exec.Command("git", "tag", "--list", "v[0-9]*.[0-9]*.[0-9]*", "--sort=-v:refname")
	cmd.Dir = projectDir
	out, err := cmd.Output()
	if err != nil {
		return "v0.1.0", nil
	}
	first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if first == "" {
		return "v0.1.0", nil
	}
	return first, nil
}

// ParseVersion extracts major, minor, patch from a "vX.Y.Z" string.
func ParseVersion(tag string) (major, minor, patch int, err error) {
	tag = strings.TrimPrefix(tag, "v")
	parts := strings.SplitN(tag, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid version tag: %q", tag)
	}

	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid major version in %q: %w", tag, err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid minor version in %q: %w", tag, err)
	}
	patch, err = strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid patch version in %q: %w", tag, err)
	}

	return major, minor, patch, nil
}

func createAndPushTag(tag, projectDir, ref string) error {
	args := []string{"tag", tag}
	if ref != "" {
		args = append(args, ref)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = projectDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create tag %s: %s", tag, strings.TrimSpace(string(out)))
	}

	cmd = exec.Command("git", "push", "origin", tag)
	cmd.Dir = projectDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to push tag %s: %s", tag, strings.TrimSpace(string(out)))
	}

	return nil
}
