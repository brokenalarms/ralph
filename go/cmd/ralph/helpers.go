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

	"github.com/brokenalarms/ralph/internal/logging"
)

// readLineCtx reads a line from stdin, returning early if ctx is cancelled.
// On cancellation it returns ("", ctx.Err()). The goroutine reading stdin
// may outlive the call but is harmless since the process is exiting.
func readLineCtx(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
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

func evolveRestart(projectDir, scriptPath, baseBranch string, args []string, log *logging.Logger) error {
	ralphDir := filepath.Join(projectDir, ".ralph")

	stopFile := filepath.Join(ralphDir, "stop")
	if _, err := os.Stat(stopFile); err == nil {
		os.Remove(stopFile)
		log.Emit(logging.Opts{}, "Stop signal detected — skipping evolve restart")
		return nil
	}

	// Remove PID file before exec so the new process (which inherits
	// our PID via syscall.Exec) doesn't see itself as a duplicate.
	os.Remove(filepath.Join(ralphDir, "loop.pid"))

	// The binary is already updated — evolve triggers because the on-disk
	// binary hash changed, meaning an external build already ran (post-merge
	// hook, another ralph instance, manual make install). No rebuild needed.
	clearSignalFiles(ralphDir)

	// Kill child processes (tail, stream-filter) before exec — otherwise
	// they become orphans that accumulate across evolve restarts.
	killChildProcesses()

	log.Separator(logging.Magenta, "RALPH EVOLVED")
	execArgs := append([]string{scriptPath, "loop"}, args...)
	return syscall.Exec(scriptPath, execArgs, os.Environ())
}

// extractEmbeddedPrompts writes the embedded prompt templates under
// <ralphDir>/prompts, overwriting whatever a previous process left there.
// The destination is deliberately NOT os.MkdirTemp: macOS's periodic
// maintenance job purges /var/folders temp files not accessed for a few
// days, which deletes the templates out from under a long-running loop
// and fails every subsequent prompt build. Each file is written to a
// .tmp sibling and renamed into place so a concurrent reader (loop and
// task session sharing one project) never observes a partial file.
func extractEmbeddedPrompts(ralphDir string) (string, error) {
	dir := filepath.Join(ralphDir, "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	err := fs.WalkDir(embeddedPrompts, "prompts", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := embeddedPrompts.ReadFile(path)
		if err != nil {
			return err
		}
		// Strip "prompts/" prefix — write directly to dir
		name := strings.TrimPrefix(path, "prompts/")
		tmp := filepath.Join(dir, name+".tmp")
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(dir, name))
	})
	return dir, err
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
	for _, f := range []string{".signal_complete", ".signal_current_task", ".signal_all_complete", ".signal_no_code_needed", ".stream-task", "stop"} {
		os.Remove(filepath.Join(ralphDir, f))
	}
}
