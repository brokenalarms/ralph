package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNeedsFallback signals that the bd backend is unavailable.
var ErrNeedsFallback = errors.New("bd unavailable")

// CommandRunner executes a bd subcommand in a directory and returns
// combined stdout. Stderr is captured separately so callers can
// inspect it on failure.
type CommandRunner func(ctx context.Context, dir string, args ...string) (stdout string, err error)

// BD implements Backend by shelling out to the bd CLI.
type BD struct {
	Ctx        context.Context
	ProjectDir string
	PromptsDir string
	RalphDir   string        // .ralph state directory for invocation logging; empty disables logging
	RunBD      CommandRunner // injectable for testing; nil uses defaultRunBD
	bdPath     string        // resolved absolute path to the bd binary
	skippedIDs map[string]bool
	resumeTaskID string
}

// bdCallRecord is one JSONL entry in .ralph/bd-calls.log.
type bdCallRecord struct {
	Time              string   `json:"time"`
	Args              []string `json:"args"`
	Dir               string   `json:"dir"`
	DurationMs        int64    `json:"durationMs"`
	ExitCode          int      `json:"exitCode"`
	Signal            string   `json:"signal,omitempty"`
	KilledByCtxCancel bool     `json:"killedByCtxCancel"`
	CtxErr            string   `json:"ctxErr,omitempty"`
	StderrTail        string   `json:"stderrTail,omitempty"`
}

func (b *BD) SetSkippedIDs(ids []string) {
	b.skippedIDs = make(map[string]bool, len(ids))
	for _, id := range ids {
		b.skippedIDs[id] = true
	}
}

func (b *BD) SetResumeTaskID(id string) {
	b.resumeTaskID = id
}

func (b *BD) ctx() context.Context {
	if b.Ctx != nil {
		return b.Ctx
	}
	return context.Background()
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

func (b *BD) defaultRunBD(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, b.bdPath, args...)
	cmd.Dir = dir
	start := time.Now()
	out, err := cmd.Output()

	var stderrBytes []byte
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		stderrBytes = exitErr.Stderr
	}

	b.logBDCall(ctx, dir, args, start, cmd.ProcessState, stderrBytes)

	if err != nil && len(stderrBytes) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderrBytes)))
	}
	return strings.TrimSpace(string(out)), err
}

// logBDCall appends one JSONL record to .ralph/bd-calls.log.
// No log rotation or size cap — the file is append-only; the user manages it.
// Best-effort: any failure to open or write the log is silently ignored and
// never affects the bd call's return value.
func (b *BD) logBDCall(ctx context.Context, dir string, args []string, start time.Time, ps *os.ProcessState, stderr []byte) {
	if b.RalphDir == "" {
		return
	}

	rec := bdCallRecord{
		Time:       start.UTC().Format(time.RFC3339Nano),
		Args:       args,
		Dir:        dir,
		DurationMs: time.Since(start).Milliseconds(),
	}

	if ps != nil {
		rec.ExitCode = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			rec.Signal = ws.Signal().String()
		}
	} else {
		rec.ExitCode = -1
	}

	if ctx.Err() != nil {
		rec.KilledByCtxCancel = true
		rec.CtxErr = ctx.Err().Error()
	}

	if len(stderr) > 0 {
		s := strings.TrimSpace(string(stderr))
		if len(s) > 500 {
			s = s[len(s)-500:]
		}
		rec.StderrTail = s
	}

	logPath := filepath.Join(b.RalphDir, "bd-calls.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(rec)
}

// Init prepares the bd backend: verifies health and manages .gitignore
// entries. If .beads doesn't exist, returns an error requiring the user
// to run `bd init` manually — ralph never auto-initializes a beads
// database to avoid accidentally reinitializing (and wiping) an
// existing one.
func (b *BD) Init() error {
	// Resolve the bd binary path before any commands run.
	if b.RunBD == nil {
		if err := b.resolveBD(); err != nil {
			return fmt.Errorf("%w: %w", err, ErrNeedsFallback)
		}
	}

	// Require .beads to already exist — never auto-init.
	beadsDir := filepath.Join(b.ProjectDir, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return fmt.Errorf("no .beads directory in %s — run `bd init` manually to initialize: %w", b.ProjectDir, ErrNeedsFallback)
	}

	// Health check: bd count is lightweight and exercises the DB connection.
	if !b.isHealthy() {
		return fmt.Errorf("bd unhealthy (bd count failed) — run `bd doctor` to diagnose: %w", ErrNeedsFallback)
	}

	// Ensure .beads and .dolt are in .gitignore.
	if err := b.ensureGitignore(); err != nil {
		// Non-fatal: log but continue.
		_ = err
	}

	return nil
}

func (b *BD) isHealthy() bool {
	_, err := b.runner()(b.ctx(), b.ProjectDir, "count")
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
	issue, err := b.getNextIssue()
	if err != nil {
		return false, err
	}
	return issue.ID != "" || issue.Title != "", nil
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
	remaining, err := b.CountRemaining()
	if err != nil {
		return 0, nil
	}
	completed, err := b.CountCompleted()
	if err != nil {
		return 0, nil
	}
	return remaining + completed, nil
}

func (b *BD) countByStatus(status string) (int, error) {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "count", "--status", status)
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
	Type     string `json:"type,omitempty"`
	Status   string `json:"status,omitempty"`
}

// getNextIssue returns the highest-priority issue to work on. If a resume
// task ID is set and that task is still open/in_progress, it is returned
// directly. Otherwise falls through to bd ready.
func (b *BD) getNextIssue() (bdIssue, error) {
	if issue, ok := b.resumeTask(); ok {
		return issue, nil
	}

	out, err := b.runner()(b.ctx(), b.ProjectDir, "ready", "--json")
	if err != nil {
		return bdIssue{}, nil
	}
	issue, ok := bestIssue(out, b.skippedIDs)
	if !ok {
		return bdIssue{}, nil
	}
	return issue, nil
}

// resumeTask checks whether the resumeTaskID points to a task that is
// still open or in_progress. Returns the issue and true if so.
func (b *BD) resumeTask() (bdIssue, bool) {
	if b.resumeTaskID == "" || b.skippedIDs[b.resumeTaskID] {
		return bdIssue{}, false
	}
	out, err := b.runner()(b.ctx(), b.ProjectDir, "show", b.resumeTaskID, "--json")
	if err != nil {
		return bdIssue{}, false
	}
	var items []bdIssue
	if jsonErr := json.Unmarshal([]byte(out), &items); jsonErr != nil || len(items) == 0 {
		return bdIssue{}, false
	}
	issue := items[0]
	if issue.Status == "open" || issue.Status == "in_progress" {
		return issue, true
	}
	return bdIssue{}, false
}

// issuePriority returns the numeric priority, defaulting to 2 (medium)
// when unset so that comparisons work for issues without explicit priority.
func issuePriority(issue bdIssue) int {
	if issue.Priority != nil {
		return *issue.Priority
	}
	return 2
}

// issueTypeRank maps issue type to a sort order within the same priority.
// Bugs are fixed first, then tasks, then features/enhancements.
func issueTypeRank(issue bdIssue) int {
	switch issue.Type {
	case "bug":
		return 0
	case "task":
		return 1
	default:
		return 2
	}
}

// bestIssue parses all issues from JSON and returns the one with the
// highest priority (lowest number), breaking ties by type rank
// (bug < task < feature/enhancement), then by status (in_progress before
// open). Issues whose ID appears in skip are excluded from selection.
func bestIssue(jsonStr string, skip map[string]bool) (bdIssue, bool) {
	var issues []bdIssue
	if err := json.Unmarshal([]byte(jsonStr), &issues); err != nil || len(issues) == 0 {
		return bdIssue{}, false
	}

	best := -1
	for i, issue := range issues {
		if issue.ID == "" && issue.Title == "" {
			continue
		}
		if skip[issue.ID] {
			continue
		}
		if best == -1 {
			best = i
			continue
		}
		bp, ip := issuePriority(issues[best]), issuePriority(issue)
		bt, it := issueTypeRank(issues[best]), issueTypeRank(issue)
		switch {
		case ip < bp:
			best = i
		case ip == bp && it < bt:
			best = i
		case ip == bp && it == bt && issue.Status == "in_progress" && issues[best].Status != "in_progress":
			best = i
		}
	}
	if best == -1 {
		return bdIssue{}, false
	}
	return issues[best], true
}

func (b *BD) GetNextTaskInfo() (TaskInfo, error) {
	issue, err := b.getNextIssue()
	if err != nil {
		return TaskInfo{}, err
	}
	return TaskInfo{
		ID:       issue.ID,
		Title:    EnsureComponentPrefix(issue.Title, ""),
		Priority: issue.Priority,
	}, nil
}

func (b *BD) GetNextTask() (string, error) {
	info, err := b.GetNextTaskInfo()
	return info.Title, err
}

func (b *BD) GetNextTaskID() (string, error) {
	info, err := b.GetNextTaskInfo()
	return info.ID, err
}

func (b *BD) HasTasks() (bool, error) {
	total, err := b.CountTotal()
	if err != nil {
		return false, err
	}
	return total > 0, nil
}

func (b *BD) SetState(id, dimension, value, reason string) error {
	if id == "" {
		return nil
	}
	args := []string{"set-state", id, dimension + "=" + value}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	_, err := b.runner()(b.ctx(), b.ProjectDir, args...)
	if err == nil {
		}
	return err
}

func (b *BD) GetState(id, dimension string) (string, error) {
	if id == "" {
		return "", nil
	}
	out, err := b.runner()(b.ctx(), b.ProjectDir, "state", id, dimension)
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
	phase, err := b.GetState(id, "phase")
	if err != nil {
		return fmt.Errorf("cannot close %s: GetState failed: %w", id, err)
	}
	if phase != "verified" {
		return fmt.Errorf("cannot close %s: phase is %q, must be \"verified\"", id, phase)
	}

	if reason == "" {
		reason = "completed by ralph"
	}
	_, err = run(b.ctx(), b.ProjectDir, "close", id, "--reason", reason)
	return err
}

// depBlockRe matches the bd close error format:
//   "cannot close <id>: blocked by open issues [id1 id2] (use --force to override)"
var depBlockRe = regexp.MustCompile(`blocked by open issues \[([^\]]+)\]`)

// ParseDependencyBlock checks whether an error from CloseTask indicates a
// dependency block. Returns the blocking task IDs if so.
func ParseDependencyBlock(err error) []string {
	if err == nil {
		return nil
	}
	m := depBlockRe.FindStringSubmatch(err.Error())
	if m == nil {
		return nil
	}
	return strings.Fields(m[1])
}

func (b *BD) ReopenTask(id string) error {
	if id == "" {
		return nil
	}
	_, err := b.runner()(b.ctx(), b.ProjectDir, "update", id, "--status=open")
	return err
}

func (b *BD) SkipTask(id string, reason string) error {
	if id == "" {
		return nil
	}
	if _, err := b.runner()(b.ctx(), b.ProjectDir, "update", id, "--status=open"); err != nil {
		return err
	}
	if reason != "" {
		_, err := b.runner()(b.ctx(), b.ProjectDir, "comments", "add", id, "skipped: "+reason)
		return err
	}
	return nil
}

func (b *BD) ExecutionInstructions() (string, error) {
	path := b.PromptsDir + "/execution-bd.md"
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading execution instructions: %w", err)
	}
	return string(data), nil
}

func (b *BD) GetDescription(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	run := b.runner()
	out, err := run(b.ctx(), b.ProjectDir, "show", id, "--json")
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

func (b *BD) GetAcceptance(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	run := b.runner()
	out, err := run(b.ctx(), b.ProjectDir, "show", id, "--json")
	if err != nil {
		return "", err
	}
	var items []struct {
		Acceptance string `json:"acceptance_criteria"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &items); jsonErr != nil || len(items) == 0 {
		return "", nil
	}
	return items[0].Acceptance, nil
}

func (b *BD) GetFullContext(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	out, err := b.runner()(b.ctx(), b.ProjectDir, "show", id)
	if err != nil {
		return "", err
	}
	comments, _ := b.runner()(b.ctx(), b.ProjectDir, "comments", id)
	if comments != "" {
		out += "\n\nCOMMENTS\n" + comments
	}
	return out, nil
}

func (b *BD) ProjectContext() (string, error) {
	ctx := b.ctx()
	run := b.runner()

	var sections []string

	sections = append(sections, fmt.Sprintf("Project directory: %s", b.ProjectDir))

	if configData, err := os.ReadFile(filepath.Join(b.ProjectDir, ".ralph", "config.toml")); err == nil {
		sections = append(sections, "## Ralph config (config.toml)\n```\n"+strings.TrimSpace(string(configData))+"\n```")
	}

	if out, err := run(ctx, b.ProjectDir, "list", "--flat"); err == nil && out != "" {
		sections = append(sections, "## Open beads\n```\n"+out+"\n```")
	}

	if out, err := run(ctx, b.ProjectDir, "list", "--status", "closed", "--limit", "20"); err == nil && out != "" {
		sections = append(sections, "## Recently closed beads\n```\n"+out+"\n```")
	}

	if out, err := run(ctx, b.ProjectDir, "prime"); err == nil && out != "" {
		sections = append(sections, "## bd workflow context\n"+out)
	}

	return strings.Join(sections, "\n\n"), nil
}

func (b *BD) GetExternalRef(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	out, err := b.runner()(b.ctx(), b.ProjectDir, "show", id, "--json")
	if err != nil {
		return "", err
	}
	var items []struct {
		ExternalRef string `json:"external_ref"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &items); jsonErr != nil || len(items) == 0 {
		return "", nil
	}
	return items[0].ExternalRef, nil
}

func (b *BD) SetExternalRef(id, ref string) error {
	if id == "" {
		return nil
	}
	_, err := b.runner()(b.ctx(), b.ProjectDir, "update", id, "--external-ref", ref)
	return err
}

func (b *BD) AppendNotes(id, msg string) error {
	if id == "" || msg == "" {
		return nil
	}
	_, err := b.runner()(b.ctx(), b.ProjectDir, "update", id, "--append-notes", msg)
	return err
}

func (b *BD) SetMetadata(id, key, value string) error {
	if id == "" || key == "" {
		return nil
	}
	_, err := b.runner()(b.ctx(), b.ProjectDir, "update", id, "--set-metadata", key+"="+value)
	return err
}

func (b *BD) GetMetadata(id, key string) (string, error) {
	if id == "" || key == "" {
		return "", nil
	}
	out, err := b.runner()(b.ctx(), b.ProjectDir, "show", id, "--json")
	if err != nil {
		return "", err
	}
	var items []struct {
		Metadata map[string]string `json:"metadata"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &items); jsonErr != nil || len(items) == 0 {
		return "", nil
	}
	if items[0].Metadata == nil {
		return "", nil
	}
	return items[0].Metadata[key], nil
}

func (b *BD) Label() string { return "beads" }
