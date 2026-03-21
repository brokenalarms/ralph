package merge

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// BumpPatchTag reads the latest vX.Y.Z tag from the repo, increments the
// patch component, creates the new tag on HEAD, and pushes it. Returns the
// new tag string (e.g. "v0.1.1") or an error. If no version tag exists,
// starts from v0.1.0 and bumps to v0.1.1.
func BumpPatchTag(projectDir string) (string, error) {
	latest, err := LatestVersionTag(projectDir)
	if err != nil {
		return "", err
	}

	major, minor, patch, err := ParseVersion(latest)
	if err != nil {
		return "", err
	}

	newTag := fmt.Sprintf("v%d.%d.%d", major, minor, patch+1)

	if err := createAndPushTag(newTag, projectDir); err != nil {
		return "", err
	}

	return newTag, nil
}

// LatestVersionTag returns the most recent vX.Y.Z tag reachable from HEAD.
// If no version tags exist, returns "v0.1.0" as the baseline.
func LatestVersionTag(projectDir string) (string, error) {
	cmd := exec.Command("git", "describe", "--tags", "--match", "v[0-9]*.[0-9]*.[0-9]*", "--abbrev=0")
	cmd.Dir = projectDir
	out, err := cmd.Output()
	if err != nil {
		return "v0.1.0", nil
	}
	return strings.TrimSpace(string(out)), nil
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

func createAndPushTag(tag, projectDir string) error {
	cmd := exec.Command("git", "tag", tag)
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
