package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
)

func safeRemoveRalphDir(dir string) error {
	base := filepath.Base(dir)
	if base == ".beads" || base == ".dolt" {
		return fmt.Errorf("refusing to remove %s: protected directory", base)
	}
	return os.RemoveAll(dir)
}

func touchFile(path string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		f.Close()
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasUncommittedChanges(dir string) bool {
	cmd1 := exec.Command("git", "-C", dir, "diff", "--quiet")
	cmd2 := exec.Command("git", "-C", dir, "diff", "--cached", "--quiet")
	return cmd1.Run() != nil || cmd2.Run() != nil
}

func validatePlanFile(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("plan file not found: %s", path)
	} else if err != nil {
		return fmt.Errorf("plan file error: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading plan file: %w", err)
	}

	if !strings.Contains(string(data), "- [") {
		return fmt.Errorf("plan file is not in Ralph format (must contain markdown checkboxes): %s", path)
	}

	return nil
}

func ensureGitignored(projectDir, entry string) {
	gitignorePath := fmt.Sprintf("%s/.gitignore", projectDir)
	existing := ""
	if data, err := os.ReadFile(gitignorePath); err == nil {
		existing = string(data)
	}

	found := false
	for _, line := range strings.Split(existing, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == entry || trimmed == entry+"/" || trimmed == entry+"/*" {
			found = true
			break
		}
	}
	if found {
		return
	}

	existing += entry + "\n"
	os.WriteFile(gitignorePath, []byte(existing), 0o644)

	if git.IsGitRepo(projectDir) {
		exec.Command("git", "-C", projectDir, "add", ".gitignore").Run()
		exec.Command("git", "-C", projectDir, "commit", "-m", "Add "+entry+" to .gitignore").Run()
	}
}
