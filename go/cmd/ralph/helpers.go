package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
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

func promptRebaseRecovery(err error) git.RebaseRecovery {
	fmt.Printf("\n%sRebase conflict:%s %v\n\n", logging.Red, logging.Reset, err)
	fmt.Printf("  %s1)%s Create fresh worktree from main (recommended — completed work is already merged)\n", logging.Bold, logging.Reset)
	fmt.Printf("  %s2)%s Abort — exit so you can resolve conflicts manually\n", logging.Bold, logging.Reset)
	fmt.Printf("  %s3)%s Skip — continue without rebasing (may cause issues)\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sChoice [1/2/3]:%s ", logging.Yellow, logging.Reset)

	var answer string
	fmt.Scanln(&answer)

	switch strings.TrimSpace(answer) {
	case "1", "":
		return git.RebaseFreshWorktree
	case "2":
		return git.RebaseManualResolve
	default:
		return git.RebaseAbort
	}
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
