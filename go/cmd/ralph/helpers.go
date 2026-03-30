package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

// readLineCtx reads a line from stdin, returning early if ctx is cancelled.
// On cancellation it returns ("", ctx.Err()). The goroutine reading stdin
// may outlive the call but is harmless since the process is exiting.
func readLineCtx(ctx context.Context) (string, error) {
	ch := make(chan string, 1)
	go func() {
		var s string
		fmt.Scanln(&s)
		ch <- s
	}()
	select {
	case s := <-ch:
		return s, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

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

func autoRebaseRecovery() func(err error) git.RebaseRecovery {
	return func(err error) git.RebaseRecovery {
		fmt.Printf("\n%sRebase conflict:%s %v\n", logging.Red, logging.Reset, err)
		fmt.Printf("Recreating worktree from main\n")
		return git.RebaseFreshWorktree
	}
}

func evolveRestart(projectDir, scriptPath string, args []string, log *logging.Logger) error {
	ralphDir := filepath.Join(projectDir, ".ralph")

	stopFile := filepath.Join(ralphDir, "stop")
	if _, err := os.Stat(stopFile); err == nil {
		os.Remove(stopFile)
		log.Log("", "Stop signal detected — skipping evolve restart")
		return nil
	}

	clearSignalFiles(ralphDir)

	// Kill child processes (tail, stream-filter) before exec — otherwise
	// they become orphans that accumulate across evolve restarts.
	killChildProcesses()

	log.Separator(logging.Magenta, "RALPH EVOLVED")
	execArgs := append([]string{scriptPath, "loop"}, args...)
	return syscall.Exec(scriptPath, execArgs, os.Environ())
}

func extractEmbeddedPrompts() (string, error) {
	tmpDir, err := os.MkdirTemp("", "ralph-prompts-*")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(embeddedPrompts, "prompts", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := embeddedPrompts.ReadFile(path)
		if err != nil {
			return err
		}
		// Strip "prompts/" prefix — write directly to tmpDir
		name := strings.TrimPrefix(path, "prompts/")
		return os.WriteFile(filepath.Join(tmpDir, name), data, 0o644)
	})
	return tmpDir, err
}

// killChildProcesses kills all descendant processes before exec to prevent
// orphan accumulation across evolve restarts. Kills grandchildren first
// (bash pipeline processes like tail, jq, perl, sed), then direct children.
func killChildProcesses() {
	pidStr := fmt.Sprintf("%d", os.Getpid())

	// Find direct children so we can kill their children (grandchildren) first.
	out, _ := exec.Command("pgrep", "-P", pidStr).Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Kill grandchildren (pipeline processes inside bash stream filter).
		exec.Command("pkill", "-9", "-P", line).Run()
		// Kill the process group rooted at this child (catches any
		// processes that setpgid into the child's group).
		if childPid, err := strconv.Atoi(line); err == nil {
			syscall.Kill(-childPid, syscall.SIGKILL)
		}
	}

	// Kill remaining direct children with SIGKILL.
	exec.Command("pkill", "-9", "-P", pidStr).Run()
}

func clearSignalFiles(ralphDir string) {
	for _, f := range []string{".signal_complete", ".signal_current_task", ".signal_all_complete", ".stream-task", "stop"} {
		os.Remove(filepath.Join(ralphDir, f))
	}
}

