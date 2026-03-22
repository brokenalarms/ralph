package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNeedsFallback signals that the bd backend is unavailable and the
// caller should fall back to checklist.
var ErrNeedsFallback = errors.New("bd unavailable, fall back to checklist")

// CommandRunner executes a bd subcommand in a directory and returns
// combined stdout. Stderr is captured separately so callers can
// inspect it on failure.
type CommandRunner func(dir string, args ...string) (stdout string, err error)

// BD implements Backend by shelling out to the bd CLI.
type BD struct {
	ProjectDir string
	PromptsDir string
	RunBD      CommandRunner // injectable for testing; nil uses defaultRunBD
	bdPath     string        // resolved absolute path to the bd binary
}

func (b *BD) runner() CommandRunner {
	if b.RunBD != nil {
		return b.RunBD
	}
	return b.defaultRunBD
}

// resolveBD finds the bd binary on PATH, falling back to common install
// locations (~/.local/bin) that may not be in PATH when invoked from
// non-login shells (e.g. inside tmux).
func (b *BD) resolveBD() error {
	if b.bdPath != "" {
		return nil
	}

	path, err := exec.LookPath("bd")
	if err == nil {
		b.bdPath = path
		return nil
	}

	home, _ := os.UserHomeDir()
	if home != "" {
		candidate := filepath.Join(home, ".local", "bin", "bd")
		if _, statErr := os.Stat(candidate); statErr == nil {
			b.bdPath = candidate
			return nil
		}
	}

	return fmt.Errorf("bd binary not found in PATH or ~/.local/bin: %w", err)
}

func (b *BD) defaultRunBD(dir string, args ...string) (string, error) {
	cmd := exec.Command(b.bdPath, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Init prepares the bd backend: runs bd init if needed, verifies
// health, and manages .gitignore entries. Returns ErrNeedsFallback
// when bd/Dolt is unreachable.
func (b *BD) Init() error {
	// Resolve the bd binary path before any commands run.
	if b.RunBD == nil {
		if err := b.resolveBD(); err != nil {
			return fmt.Errorf("%w: %w", err, ErrNeedsFallback)
		}
	}

	run := b.runner()

	// If .beads doesn't exist, run bd init.
	beadsDir := filepath.Join(b.ProjectDir, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		if _, initErr := run(b.ProjectDir, "init"); initErr != nil {
			return fmt.Errorf("bd init failed: %w: %w", initErr, ErrNeedsFallback)
		}
	}

	// Health check: bd count is lightweight and exercises the DB connection.
	if !b.isHealthy() {
		// Retry init to reconnect a stale server.
		if _, initErr := run(b.ProjectDir, "init"); initErr != nil {
			return fmt.Errorf("bd init retry failed: %w: %w", initErr, ErrNeedsFallback)
		}
		if !b.isHealthy() {
			return fmt.Errorf("server unreachable after retry: %w", ErrNeedsFallback)
		}
	}

	// Ensure .beads and .dolt are in .gitignore.
	if err := b.ensureGitignore(); err != nil {
		// Non-fatal: log but continue.
		_ = err
	}

	return nil
}

func (b *BD) isHealthy() bool {
	_, err := b.runner()(b.ProjectDir, "count")
	return err == nil
}

func (b *BD) ensureGitignore() error {
	gitignore := filepath.Join(b.ProjectDir, ".gitignore")

	var existing string
	if data, err := os.ReadFile(gitignore); err == nil {
		existing = string(data)
	}

	changed := false
	for _, entry := range []string{".beads", ".dolt"} {
		if !gitignoreContains(existing, entry) {
			existing += entry + "\n"
			changed = true
		}
	}

	if !changed {
		return nil
	}

	if err := os.WriteFile(gitignore, []byte(existing), 0644); err != nil {
		return err
	}

	// Auto-commit if inside a git repo.
	gitDir := exec.Command("git", "-C", b.ProjectDir, "rev-parse", "--git-dir")
	if gitDir.Run() == nil {
		add := exec.Command("git", "-C", b.ProjectDir, "add", ".gitignore")
		_ = add.Run()
		commit := exec.Command("git", "-C", b.ProjectDir, "commit", "-m", "Add beads/dolt to .gitignore")
		_ = commit.Run()
	}

	return nil
}

// gitignoreContains checks whether an entry already appears as its own
// line (with optional trailing / or /*).
func gitignoreContains(content, entry string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == entry || trimmed == entry+"/" || trimmed == entry+"/*" {
			return true
		}
	}
	return false
}

func (b *BD) HasRemaining() (bool, error) {
	n, err := b.CountRemaining()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (b *BD) CountCompleted() (int, error) {
	return b.countByStatus("closed")
}

func (b *BD) CountRemaining() (int, error) {
	open, err := b.countByStatus("open")
	if err != nil {
		return 0, err
	}
	inp, err := b.countByStatus("in_progress")
	if err != nil {
		return 0, err
	}
	return open + inp, nil
}

func (b *BD) CountTotal() (int, error) {
	out, err := b.runner()(b.ProjectDir, "count")
	if err != nil {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func (b *BD) countByStatus(status string) (int, error) {
	out, err := b.runner()(b.ProjectDir, "count", "--status", status)
	if err != nil {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, nil
	}
	return n, nil
}

type bdIssue struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Priority *int   `json:"priority,omitempty"`
}

// getNextIssue returns the highest-priority issue across in-progress and
// ready queues. If a ready task has strictly higher priority (lower number)
// than the in-progress task, the in-progress task is reopened and the ready
// task is returned instead.
func (b *BD) getNextIssue() (bdIssue, error) {
	run := b.runner()

	var inProgress, ready bdIssue
	var hasIP, hasReady bool

	out, err := run(b.ProjectDir, "list", "--status", "in_progress", "--flat", "--json", "--limit", "1")
	if err == nil {
		inProgress, hasIP = parseFirstIssue(out)
	}

	out, err = run(b.ProjectDir, "ready", "--limit", "1", "--json")
	if err == nil {
		ready, hasReady = parseFirstIssue(out)
	}

	if hasIP && hasReady {
		ipPri := issuePriority(inProgress)
		rdPri := issuePriority(ready)
		if rdPri < ipPri {
			// Higher-priority ready task preempts; reopen the in-progress task.
			_, _ = run(b.ProjectDir, "update", inProgress.ID, "--status=open")
			return ready, nil
		}
		return inProgress, nil
	}
	if hasIP {
		return inProgress, nil
	}
	if hasReady {
		return ready, nil
	}
	return bdIssue{}, nil
}

// issuePriority returns the numeric priority, defaulting to 2 (medium)
// when unset so that comparisons work for issues without explicit priority.
func issuePriority(issue bdIssue) int {
	if issue.Priority != nil {
		return *issue.Priority
	}
	return 2
}

func parseFirstIssue(jsonStr string) (bdIssue, bool) {
	var issues []bdIssue
	if err := json.Unmarshal([]byte(jsonStr), &issues); err != nil || len(issues) == 0 {
		return bdIssue{}, false
	}
	if issues[0].ID == "" && issues[0].Title == "" {
		return bdIssue{}, false
	}
	return issues[0], true
}

func (b *BD) GetNextTaskInfo() (string, string, error) {
	issue, err := b.getNextIssue()
	if err != nil {
		return "", "", err
	}
	return issue.ID, issue.Title, nil
}

func (b *BD) GetNextTask() (string, error) {
	_, title, err := b.GetNextTaskInfo()
	return title, err
}

func (b *BD) GetNextTaskID() (string, error) {
	id, _, err := b.GetNextTaskInfo()
	return id, err
}

func (b *BD) HasTasks() (bool, error) {
	total, err := b.CountTotal()
	if err != nil {
		return false, err
	}
	return total > 0, nil
}

func (b *BD) NeedsPlanning() (bool, error) {
	has, err := b.HasTasks()
	if err != nil {
		return false, err
	}
	return !has, nil
}

func (b *BD) PlanningSucceeded() (bool, error) {
	return b.HasTasks()
}

func (b *BD) SetState(id, dimension, value, reason string) error {
	if id == "" {
		return nil
	}
	args := []string{"set-state", id, dimension + "=" + value}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	_, err := b.runner()(b.ProjectDir, args...)
	return err
}

func (b *BD) GetState(id, dimension string) (string, error) {
	if id == "" {
		return "", nil
	}
	out, err := b.runner()(b.ProjectDir, "state", id, dimension)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (b *BD) CloseTask(id string, reason string) error {
	if id == "" {
		return nil
	}
	run := b.runner()

	// Prevent closing unless the task has reached phase:verified.
	phase, _ := b.GetState(id, "phase")
	if phase != "verified" {
		return fmt.Errorf("cannot close %s: phase is %q, must be \"verified\"", id, phase)
	}

	if reason == "" {
		reason = "completed by ralph"
	}
	_, err := run(b.ProjectDir, "close", id, "--reason", reason)
	return err
}

func (b *BD) ReopenTask(id string) error {
	if id == "" {
		return nil
	}
	_, err := b.runner()(b.ProjectDir, "update", id, "--status=open")
	return err
}

func (b *BD) SkipTask(id string, reason string) error {
	if id == "" {
		return nil
	}
	if reason == "" {
		reason = "skipped by ralph"
	}
	_, err := b.runner()(b.ProjectDir, "close", id, "--reason", "blocked: "+reason)
	return err
}

func (b *BD) ExecutionInstructions() (string, error) {
	path := b.PromptsDir + "/execution-bd.md"
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading execution instructions: %w", err)
	}
	return string(data), nil
}

func (b *BD) PlanningInstructions() string {
	return "Run `bd prime` to learn the workflow, then create tasks directly in bd with dependencies. Do NOT write a plan.md file — tasks live exclusively in bd."
}

func (b *BD) GetDescription(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	run := b.runner()
	out, err := run(b.ProjectDir, "show", id, "--json")
	if err != nil {
		return "", err
	}
	var items []struct {
		Description string `json:"description"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &items); jsonErr != nil || len(items) == 0 {
		return "", nil
	}
	return items[0].Description, nil
}

func (b *BD) Label() string { return "beads" }
