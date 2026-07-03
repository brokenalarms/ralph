package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brokenalarms/ralph/internal/component"
	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
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

	// Disable bd's git-push backup: since .beads is gitignored, the backup's
	// git add always fails. The gitignore decision and this disable are the same decision.
	if err := b.ensureBackupGitPushDisabled(); err != nil {
		log.Printf("bd: ensureBackupGitPushDisabled: %v", err)
	}

	// Disable export git-add: the export path lives under .beads (gitignored),
	// so every git add attempt fails. Dolt is the real sync backend.
	if err := b.ensureExportGitAddDisabled(); err != nil {
		log.Printf("bd: ensureExportGitAddDisabled: %v", err)
	}

	// Pin dolt.port and export.path so they survive reboots and export tasks.
	if err := b.ensureDoltPort(); err != nil {
		log.Printf("bd: ensureDoltPort: %v", err)
	}
	if err := b.ensureTasksExport(); err != nil {
		log.Printf("bd: ensureTasksExport: %v", err)
	}

	return nil
}

func (b *BD) isHealthy() bool {
	_, err := b.runner()(b.ctx(), b.ProjectDir, "count")
	return err == nil
}

func (b *BD) ensureGitignore() error {
	return git.EnsureIgnored(b.ProjectDir, "Add beads/dolt to .gitignore", ".beads", ".dolt")
}

func (b *BD) ensureDoltPort() error {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "config", "get", "dolt.port")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "not set in config.yaml") {
		return nil
	}
	absDir, err := filepath.Abs(b.ProjectDir)
	if err != nil {
		return err
	}
	h := fnv.New32a()
	h.Write([]byte(absDir))
	port := 49152 + int(h.Sum32()%16384)
	_, err = b.runner()(b.ctx(), b.ProjectDir, "config", "set", "dolt.port", strconv.Itoa(port))
	return err
}

func (b *BD) ensureTasksExport() error {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "config", "get", "export.path")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "not set in config.yaml") {
		return nil
	}
	_, err = b.runner()(b.ctx(), b.ProjectDir, "config", "set", "export.path", "../beads-tasks.jsonl")
	return err
}

func (b *BD) ensureBackupGitPushDisabled() error {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "config", "get", "backup.git-push")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "not set in config.yaml") {
		return nil
	}
	_, err = b.runner()(b.ctx(), b.ProjectDir, "config", "set", "backup.git-push", "false")
	return err
}

func (b *BD) ensureExportGitAddDisabled() error {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "config", "get", "export.git-add")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "false" {
		return nil
	}
	_, err = b.runner()(b.ctx(), b.ProjectDir, "config", "set", "export.git-add", "false")
	return err
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
	Assignee string `json:"assignee,omitempty"`
}

// nonExecutableBDTypes lists bd issue types that are containers or metadata
// rather than agent work. The loop must never select these: an agent has no
// concrete change to make on an epic, decision, merge-request, molecule, or
// convoy. Surfacing them causes the orchestrator to "complete" the container
// without closing it (epics can't close while children are open), after which
// every subsequent selection re-picks the same container, the in-session
// dedup triggers, skipTask escalates because the container has open dependents,
// and the selector spins until it hits maxSelectionAttempts.
//
// Kept as a blacklist (not an allowlist of executable types) so that new bd
// types added in the future default to executable. If a new container type is
// introduced and reproduces this failure mode, add it here.
var nonExecutableBDTypes = []string{"epic", "decision", "merge-request", "molecule", "convoy"}

// nonExecutableBDTypesSet is the lookup form of nonExecutableBDTypes, used by
// resumeTask to reject a resume target whose type is a container.
var nonExecutableBDTypesSet = func() map[string]bool {
	m := make(map[string]bool, len(nonExecutableBDTypes))
	for _, t := range nonExecutableBDTypes {
		m[t] = true
	}
	return m
}()

// bdReadyExcludeTypeArg is the --exclude-type value passed to every bd ready
// invocation so container/meta types never reach the selector.
var bdReadyExcludeTypeArg = "--exclude-type=" + strings.Join(nonExecutableBDTypes, ",")

// getNextIssue returns the highest-priority issue to work on. It delegates
// to Stage 1 (resumeCandidate) to decide whether in-flight work should
// continue, falling through to Stage 2 (nextReadyIssue) whenever Stage 1
// has no candidate to resume.
func (b *BD) getNextIssue() (bdIssue, error) {
	if resume, ok := b.resumeCandidate(); ok {
		return resume, nil
	}
	return b.nextReadyIssue()
}

// resumeCandidate implements Stage 1 (resume): it returns the interrupted
// in-flight task (resumeTask) to continue, unless a strictly-higher-priority
// ready task exists to preempt it. This preemption criterion is
// priority-based regardless of bd ready's sort policy — an older ready bead
// is never on its own a reason to abandon started work. Equal or lower
// priority ready work defers to the in-flight task so WIP is not abandoned.
func (b *BD) resumeCandidate() (bdIssue, bool) {
	resume, hasResume := b.resumeTask()
	if !hasResume {
		return bdIssue{}, false
	}
	if b.higherPriorityReadyExists(resume) {
		return bdIssue{}, false
	}
	return resume, true
}

// higherPriorityReadyExists reports whether bd ready has a not-in-progress
// task with a strictly better (lower-numbered) priority than resume. It
// queries bd ready sorted purely by priority and limited to a single row,
// independent of the hybrid-sorted query nextReadyIssue uses for selection —
// this keeps the preemption decision from depending on hybrid's age-aware
// ordering, and performs no Go-side re-sort of its own.
func (b *BD) higherPriorityReadyExists(resume bdIssue) bool {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "ready", "--json", bdReadyExcludeTypeArg, "--sort=priority", "--limit=1", "--assignee="+config.LoopAssignee)
	if err != nil {
		return false
	}
	top, hasTop := firstReadyIssue(out)
	if !hasTop {
		return false
	}
	return issuePriority(top) < issuePriority(resume)
}

// nextReadyIssue implements Stage 2 (pick next): it returns row 0 of bd
// ready's hybrid-sorted output verbatim. bd ready is invoked with
// --sort=hybrid so bd's own hybrid ordering policy (priority-aware for
// issues younger than its cutoff, strictly oldest-first beyond it) picks the
// candidate — no re-ranking happens in Go.
func (b *BD) nextReadyIssue() (bdIssue, error) {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "ready", "--json", bdReadyExcludeTypeArg, "--sort=hybrid", "--assignee="+config.LoopAssignee)
	if err != nil {
		return bdIssue{}, nil
	}
	if ready, ok := firstReadyIssue(out); ok {
		return ready, nil
	}
	return bdIssue{}, nil
}

// resumeTask checks whether the resumeTaskID points to a task that is
// still open or in_progress, assigned to the loop (not skipped/reassigned),
// and has an executable type. Container/meta types (epic, decision,
// merge-request, molecule, convoy) are rejected so a stale last_task_id
// pointing at a container can't bypass the type filter applied to bd ready.
func (b *BD) resumeTask() (bdIssue, bool) {
	if b.resumeTaskID == "" {
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
	if nonExecutableBDTypesSet[issue.Type] {
		return bdIssue{}, false
	}
	// Reject tasks that were skipped (reassigned to ralph-task). A non-empty
	// assignee that isn't the loop means the bead has left the loop's inbox.
	if issue.Assignee != "" && issue.Assignee != config.LoopAssignee {
		return bdIssue{}, false
	}
	if issue.Status == "open" || issue.Status == "in_progress" {
		// Do not resume a task whose dependencies are no longer satisfied. A
		// stale resumeTaskID can point at a task that was started, then had a
		// blocker reopened — e.g. a prerequisite's PR failed CI and its bead
		// was reopened, while this dependent task remained current_task_id.
		// Resuming here would bypass the dependency graph and work the
		// dependent task ahead of its prerequisite. Fall through to bd ready
		// instead, which excludes blocked tasks, so the prerequisite is
		// selected first. Only block on a definitive not-ready answer; a bd
		// failure (err != nil) preserves the prior resume behavior.
		if ready, err := b.IsReady(b.resumeTaskID); err == nil && !ready {
			return bdIssue{}, false
		}
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

// firstReadyIssue returns the first issue in bd ready's ordered JSON output.
// Ordering is bd's responsibility (--sort=hybrid on the invocation); this
// function performs no ranking of its own.
func firstReadyIssue(jsonStr string) (bdIssue, bool) {
	var issues []bdIssue
	if err := json.Unmarshal([]byte(jsonStr), &issues); err != nil || len(issues) == 0 {
		return bdIssue{}, false
	}
	return issues[0], true
}

func (b *BD) GetNextTaskInfo() (TaskInfo, error) {
	issue, err := b.getNextIssue()
	if err != nil {
		return TaskInfo{}, err
	}
	return TaskInfo{
		ID:       issue.ID,
		Title:    component.EnsureComponentPrefix(issue.Title, ""),
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

	if reason == "" {
		reason = "completed by ralph"
	}
	_, err := run(b.ctx(), b.ProjectDir, "close", id, "--reason", reason)
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

func (b *BD) ClaimTask(id string) error {
	if id == "" {
		return nil
	}
	// Mark in-progress and pin ownership explicitly at the call site — no
	// reliance on BEADS_ACTOR or `--claim`'s implicit "assign to you". Setting
	// --assignee=ralph-loop keeps the bead in the loop's own inbox so a later
	// skip→reopen stays selectable by `bd ready --assignee=ralph-loop`.
	_, err := b.runner()(b.ctx(), b.ProjectDir, "update", id, "--assignee="+config.LoopAssignee, "--status=in_progress")
	return err
}

func (b *BD) ReopenTask(id string) error {
	if id == "" {
		return nil
	}
	_, err := b.runner()(b.ctx(), b.ProjectDir, "update", id, "--status=open")
	return err
}

func (b *BD) SkipTask(id string, reason SkipReason, detail string) error {
	if id == "" {
		return nil
	}
	if _, err := b.runner()(b.ctx(), b.ProjectDir, "update", id, "--status=open", "--assignee="+config.TaskAssignee, "--add-label=skipped"); err != nil {
		return err
	}
	if err := b.SetMetadata(id, "skip_reason", string(reason)); err != nil {
		return err
	}
	if detail != "" {
		if err := b.SetMetadata(id, "skip_detail", detail); err != nil {
			return err
		}
	}
	comment := string(reason)
	if detail != "" {
		comment += ": " + detail
	}
	_, err := b.runner()(b.ctx(), b.ProjectDir, "comments", "add", id, "skipped: "+comment)
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
	out, err := b.runner()(b.ctx(), b.ProjectDir, "show", id, "--json")
	if err != nil {
		return "", err
	}
	var items []struct {
		Title        string `json:"title"`
		Description  string `json:"description"`
		Acceptance   string `json:"acceptance_criteria"`
		Dependencies []struct {
			ID             string `json:"id"`
			Title          string `json:"title"`
			Status         string `json:"status"`
			DependencyType string `json:"dependency_type"`
		} `json:"dependencies"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &items); jsonErr != nil || len(items) == 0 {
		return "", nil
	}
	bead := items[0]
	var parts []string
	if bead.Title != "" {
		parts = append(parts, "TITLE: "+bead.Title)
	}
	if bead.Description != "" {
		parts = append(parts, "DESCRIPTION\n"+bead.Description)
	}
	if bead.Acceptance != "" {
		parts = append(parts, "ACCEPTANCE CRITERIA\n"+bead.Acceptance)
	}
	var openDeps []string
	for _, dep := range bead.Dependencies {
		if dep.DependencyType == "blocks" && dep.Status != "closed" && dep.ID != "" {
			openDeps = append(openDeps, "- "+dep.ID+": "+dep.Title)
		}
	}
	if len(openDeps) > 0 {
		parts = append(parts, "OPEN DEPENDENCIES\n"+strings.Join(openDeps, "\n"))
	}
	return strings.Join(parts, "\n\n"), nil
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

// GetOpenDependents returns the IDs of open issues that depend on the given task.
// It calls bd show <id> --json and parses the dependents array, keeping only
// non-closed entries. Returns nil on any error or when no open dependents exist.
func (b *BD) GetOpenDependents(id string) ([]string, error) {
	if id == "" {
		return nil, nil
	}
	out, err := b.runner()(b.ctx(), b.ProjectDir, "show", id, "--json")
	if err != nil {
		return nil, err
	}
	var items []struct {
		Dependents []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"dependents"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &items); jsonErr != nil || len(items) == 0 {
		return nil, nil
	}
	var openIDs []string
	for _, dep := range items[0].Dependents {
		if dep.Status != "closed" && dep.ID != "" {
			openIDs = append(openIDs, dep.ID)
		}
	}
	return openIDs, nil
}

// ListInProgressByAssignee returns TaskInfo for all in_progress tasks assigned
// to the given assignee by calling bd list --status=in_progress --assignee=<assignee> --json.
// Returns nil (not an error) when the bd call fails or the output is unparseable,
// since this is used for opportunistic stall detection.
func (b *BD) ListInProgressByAssignee(assignee string) ([]TaskInfo, error) {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "list", "--status=in_progress", "--assignee="+assignee, "--json")
	if err != nil {
		return nil, nil
	}
	return parseInProgressJSON(out), nil
}

// ListAllInProgress returns TaskInfo (with Assignee populated) for all
// in_progress tasks across all assignees. Returns nil (not an error) on bd
// call failure, since this is used for opportunistic stall detection.
func (b *BD) ListAllInProgress() ([]TaskInfo, error) {
	out, err := b.runner()(b.ctx(), b.ProjectDir, "list", "--status=in_progress", "--json")
	if err != nil {
		return nil, nil
	}
	return parseInProgressJSON(out), nil
}

// parseInProgressJSON parses bd list --json output into TaskInfo slices,
// populating the Assignee field from the issue JSON.
func parseInProgressJSON(out string) []TaskInfo {
	var issues []bdIssue
	if jsonErr := json.Unmarshal([]byte(out), &issues); jsonErr != nil {
		return nil
	}
	var result []TaskInfo
	for _, issue := range issues {
		if issue.ID == "" {
			continue
		}
		result = append(result, TaskInfo{ID: issue.ID, Title: issue.Title, Priority: issue.Priority, Assignee: issue.Assignee})
	}
	return result
}

// IsReady reports whether task id is ready to work on. It calls bd blocked
// --json (status-agnostic) and returns false if id appears in that set,
// true otherwise. Returns an error only when the bd call itself fails.
func (b *BD) IsReady(id string) (bool, error) {
	if id == "" {
		return true, nil
	}
	out, err := b.runner()(b.ctx(), b.ProjectDir, "blocked", "--json")
	if err != nil {
		return false, err
	}
	var items []struct {
		ID string `json:"id"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &items); jsonErr != nil {
		return true, nil
	}
	for _, item := range items {
		if item.ID == id {
			return false, nil
		}
	}
	return true, nil
}

func (b *BD) Label() string { return "beads" }
