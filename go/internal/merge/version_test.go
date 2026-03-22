package merge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Proves: ParseVersion correctly extracts major, minor, patch from a
// well-formed "vX.Y.Z" tag string.
func TestParseVersion_Valid(t *testing.T) {
	cases := []struct {
		tag                        string
		major, minor, patch int
	}{
		{"v0.1.0", 0, 1, 0},
		{"v1.2.3", 1, 2, 3},
		{"v10.20.30", 10, 20, 30},
		{"0.0.0", 0, 0, 0},
	}

	for _, tc := range cases {
		major, minor, patch, err := ParseVersion(tc.tag)
		if err != nil {
			t.Errorf("ParseVersion(%q) error: %v", tc.tag, err)
			continue
		}
		if major != tc.major || minor != tc.minor || patch != tc.patch {
			t.Errorf("ParseVersion(%q) = %d.%d.%d, want %d.%d.%d",
				tc.tag, major, minor, patch, tc.major, tc.minor, tc.patch)
		}
	}
}

// Proves: ParseVersion rejects malformed version strings that don't
// follow the X.Y.Z pattern.
func TestParseVersion_Invalid(t *testing.T) {
	cases := []string{"v1.2", "v1", "abc", "", "v1.2.x"}
	for _, tc := range cases {
		_, _, _, err := ParseVersion(tc)
		if err == nil {
			t.Errorf("ParseVersion(%q) expected error, got nil", tc)
		}
	}
}

// Proves: LatestVersionTag returns "v0.1.0" as the baseline when a repo
// has no version tags, so patch bumping starts from a known default.
func TestLatestVersionTag_NoTags(t *testing.T) {
	dir := initTestRepo(t)

	tag, err := LatestVersionTag(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "v0.1.0" {
		t.Errorf("expected v0.1.0 baseline, got %q", tag)
	}
}

// Proves: LatestVersionTag returns the most recent version tag when
// multiple tags exist in the repository.
func TestLatestVersionTag_WithTags(t *testing.T) {
	dir := initTestRepo(t)

	runGit(t, dir, "tag", "v0.1.0")

	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "second")
	runGit(t, dir, "tag", "v0.1.1")

	tag, err := LatestVersionTag(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "v0.1.1" {
		t.Errorf("expected v0.1.1, got %q", tag)
	}
}

// Proves: BumpPatchTag increments the patch component and creates a new
// tag in a local repo (without requiring a remote to push to).
func TestBumpPatchTag_IncrementsFromExistingTag(t *testing.T) {
	dir := initTestRepo(t)
	runGit(t, dir, "tag", "v0.2.5")

	// Create a bare remote so push succeeds.
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare")
	runGit(t, dir, "remote", "add", "origin", remote)
	runGit(t, dir, "push", "origin", "main")
	runGit(t, dir, "push", "origin", "v0.2.5")

	newTag, err := BumpPatchTag(dir, "")
	if err != nil {
		t.Fatalf("BumpPatchTag error: %v", err)
	}
	if newTag != "v0.2.6" {
		t.Errorf("expected v0.2.6, got %q", newTag)
	}

	// Verify the tag exists locally.
	out, _ := exec.Command("git", "-C", dir, "tag", "-l", "v0.2.6").Output()
	if len(out) == 0 {
		t.Error("tag v0.2.6 was not created locally")
	}
}

// Proves: BumpPatchTag defaults to v0.1.1 when no version tags exist,
// using the v0.1.0 baseline.
func TestBumpPatchTag_DefaultsWhenNoTags(t *testing.T) {
	dir := initTestRepo(t)

	remote := t.TempDir()
	runGit(t, remote, "init", "--bare")
	runGit(t, dir, "remote", "add", "origin", remote)
	runGit(t, dir, "push", "origin", "main")

	newTag, err := BumpPatchTag(dir, "")
	if err != nil {
		t.Fatalf("BumpPatchTag error: %v", err)
	}
	if newTag != "v0.1.1" {
		t.Errorf("expected v0.1.1, got %q", newTag)
	}
}

// Proves: BumpPatchTag tags a specific ref instead of HEAD when a ref is
// provided — used after auto-merge to tag origin/main rather than the
// local worktree branch.
func TestBumpPatchTag_TagsSpecificRef(t *testing.T) {
	dir := initTestRepo(t)
	runGit(t, dir, "tag", "v0.3.0")

	// Create a second commit on a feature branch.
	runGit(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("f"), 0o644)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "feature commit")

	// Create a bare remote so push succeeds.
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare")
	runGit(t, dir, "remote", "add", "origin", remote)
	runGit(t, dir, "push", "origin", "main")
	runGit(t, dir, "push", "origin", "feature")
	runGit(t, dir, "push", "origin", "v0.3.0")

	// Get the SHA of main (first commit) — HEAD is on feature.
	mainSHA, _ := exec.Command("git", "-C", dir, "rev-parse", "main").Output()

	newTag, err := BumpPatchTag(dir, "main")
	if err != nil {
		t.Fatalf("BumpPatchTag error: %v", err)
	}
	if newTag != "v0.3.1" {
		t.Errorf("expected v0.3.1, got %q", newTag)
	}

	// Verify the tag points at main, not at feature (HEAD).
	tagSHA, _ := exec.Command("git", "-C", dir, "rev-parse", "v0.3.1").Output()
	if string(tagSHA) != string(mainSHA) {
		t.Errorf("tag should point at main (%s), got %s",
			strings.TrimSpace(string(mainSHA)),
			strings.TrimSpace(string(tagSHA)))
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %s\n%s", args, err, out)
	}
}
