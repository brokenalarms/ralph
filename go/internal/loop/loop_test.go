package loop

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

type stubRunner struct {
	onRun  func()
	result claude.Result
}

func (s *stubRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if s.onRun != nil {
		s.onRun()
	}
	return s.result, nil
}

func (s *stubRunner) StopStreaming() {}

func (s *stubRunner) InjectMessage(_ string) error { return nil }

// stubBackend implements tasks.Backend for testing without shelling out to
// bd or reading plan files. Lets us control exactly how many tasks remain
// and what the next task is.

type stubBackend struct {
	remaining    int
	completed    int
	total        int
	nextTask     string
	nextID       string
	nextPriority *int
	label        string
	description  string
	acceptance   string
	fullContext  string
	skippedTask  string
	skipReason   string
}

// mutableBackend is like stubBackend but allows changing the next task
// mid-run to simulate task transitions.

type mutableBackend struct {
	mu           sync.Mutex
	remaining    int
	completed    int
	total        int
	nextTask     string
	nextID       string
	nextPriority *int
	label        string
	description  string
	metadata     map[string]map[string]string // id -> key -> value
	externalRefs map[string]string            // id -> external ref (e.g. PR URL)
}

func (m *mutableBackend) Init() error { return nil }
func (m *mutableBackend) HasRemaining() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remaining > 0, nil
}
func (m *mutableBackend) CountCompleted() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.completed, nil
}
func (m *mutableBackend) CountRemaining() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remaining, nil
}
func (m *mutableBackend) CountTotal() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total, nil
}
func (m *mutableBackend) GetNextTask() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextTask, nil
}
func (m *mutableBackend) GetNextTaskID() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextID, nil
}
func (m *mutableBackend) GetNextTaskInfo() (tasks.TaskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return tasks.TaskInfo{ID: m.nextID, Title: m.nextTask, Priority: m.nextPriority}, nil
}
func (m *mutableBackend) HasTasks() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total > 0, nil
}
func (m *mutableBackend) CloseTask(string, string) error         { return nil }
func (m *mutableBackend) SkipTask(string, string) error          { return nil }
func (m *mutableBackend) SetSkippedIDs(_ []string)               {}
func (m *mutableBackend) ReopenTask(string) error                { return nil }
func (m *mutableBackend) SetState(_, _, _, _ string) error       { return nil }
func (m *mutableBackend) GetState(_, _ string) (string, error)   { return "", nil }
func (m *mutableBackend) ExecutionInstructions() (string, error) { return "", nil }
func (m *mutableBackend) GetDescription(_ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.description, nil
}
func (m *mutableBackend) GetAcceptance(_ string) (string, error)  { return "", nil }
func (m *mutableBackend) GetFullContext(_ string) (string, error) { return "", nil }
func (m *mutableBackend) ProjectContext() (string, error)         { return "", nil }
func (m *mutableBackend) GetExternalRef(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.externalRefs != nil {
		return m.externalRefs[id], nil
	}
	return "", nil
}
func (m *mutableBackend) SetExternalRef(_, _ string) error { return nil }
func (m *mutableBackend) AppendNotes(_, _ string) error    { return nil }
func (m *mutableBackend) SetMetadata(_, _, _ string) error { return nil }
func (m *mutableBackend) GetMetadata(id, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.metadata != nil {
		if keys, ok := m.metadata[id]; ok {
			return keys[key], nil
		}
	}
	return "", nil
}
func (m *mutableBackend) Label() string {
	if m.label != "" {
		return m.label
	}
	return "beads"
}

func (s *stubBackend) Init() error                    { return nil }
func (s *stubBackend) HasRemaining() (bool, error)    { return s.remaining > 0, nil }
func (s *stubBackend) CountCompleted() (int, error)   { return s.completed, nil }
func (s *stubBackend) CountRemaining() (int, error)   { return s.remaining, nil }
func (s *stubBackend) CountTotal() (int, error)       { return s.total, nil }
func (s *stubBackend) GetNextTask() (string, error)   { return s.nextTask, nil }
func (s *stubBackend) GetNextTaskID() (string, error) { return s.nextID, nil }
func (s *stubBackend) GetNextTaskInfo() (tasks.TaskInfo, error) {
	return tasks.TaskInfo{ID: s.nextID, Title: s.nextTask, Priority: s.nextPriority}, nil
}
func (s *stubBackend) HasTasks() (bool, error)        { return s.total > 0, nil }
func (s *stubBackend) CloseTask(string, string) error { return nil }
func (s *stubBackend) SkipTask(id, reason string) error {
	s.skippedTask = id
	s.skipReason = reason
	return nil
}
func (s *stubBackend) SetSkippedIDs(_ []string)                {}
func (s *stubBackend) ReopenTask(string) error                 { return nil }
func (s *stubBackend) SetState(_, _, _, _ string) error        { return nil }
func (s *stubBackend) GetState(_, _ string) (string, error)    { return "", nil }
func (s *stubBackend) ExecutionInstructions() (string, error)  { return "", nil }
func (s *stubBackend) GetDescription(_ string) (string, error) { return s.description, nil }
func (s *stubBackend) GetAcceptance(_ string) (string, error)  { return s.acceptance, nil }
func (s *stubBackend) GetFullContext(_ string) (string, error) { return s.fullContext, nil }
func (s *stubBackend) ProjectContext() (string, error)         { return "", nil }
func (s *stubBackend) GetExternalRef(_ string) (string, error) { return "", nil }
func (s *stubBackend) SetExternalRef(_, _ string) error        { return nil }
func (s *stubBackend) AppendNotes(_, _ string) error           { return nil }
func (s *stubBackend) SetMetadata(_, _, _ string) error        { return nil }
func (s *stubBackend) GetMetadata(_, _ string) (string, error) { return "", nil }
func (s *stubBackend) Label() string {
	if s.label != "" {
		return s.label
	}
	return "beads"
}

func setupTestDir(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	return dir, st
}

type trackingBackend struct {
	mutableBackend
	closedIDs    []string
	closeReasons []string
	closeMu      sync.Mutex
	skippedIDs   []string
	skipReasons  []string
	skipMu       sync.Mutex
}

func (t *trackingBackend) CloseTask(id string, reason string) error {
	t.closeMu.Lock()
	t.closedIDs = append(t.closedIDs, id)
	t.closeReasons = append(t.closeReasons, reason)
	t.closeMu.Unlock()
	return nil
}

func (t *trackingBackend) SkipTask(id string, reason string) error {
	t.skipMu.Lock()
	t.skippedIDs = append(t.skippedIDs, id)
	t.skipReasons = append(t.skipReasons, reason)
	t.skipMu.Unlock()
	return nil
}

// Verifies the orchestrator closes the assigned task after signal detection
// and verification pass, preventing agents from needing to call bd close
// directly (which could close tasks they aren't assigned to).

// --- test helpers ---

// createPromptTemplates creates minimal prompt template files so the loop
// can build prompts without errors.
func createPromptTemplates(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	for _, name := range []string{"shared.md", "internal.md", "reflection.md", "signal.md", "feedback.md", "refactor.md", "refactor-style.md", "execution-bd.md"} {
		os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644)
	}
}

func initBareRepoWithOrigin(t *testing.T) (projectDir string, bareDir string) {
	t.Helper()
	tmp := t.TempDir()

	bare := filepath.Join(tmp, "bare.git")
	run(t, "git", "init", "--bare", "-b", "main", bare)

	project := filepath.Join(tmp, "project")
	run(t, "git", "clone", bare, project)
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "init")
	run(t, "git", "-C", project, "push", "-u", "origin", "main")
	run(t, "git", "-C", project, "remote", "set-head", "origin", "main")

	return project, bare
}

func pushToOrigin(t *testing.T, projectDir string) {
	t.Helper()
	run(t, "git", "-C", projectDir, "push", "origin", "main", "-q")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	run(t, "git", "-C", dir, "add", name)
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

type stubGitHub struct {
	available      bool
	prState        string
	prBase         string
	prHead         string
	openPRBranches []string
}

func (s *stubGitHub) Available() bool                                      { return s.available }
func (s *stubGitHub) FindOpenPR(_, _ string) (string, error)               { return "", nil }
func (s *stubGitHub) CreatePR(_ git.CreatePROpts) error                    { return nil }
func (s *stubGitHub) MergePR(_, _ string, _ git.MergeOpts) (string, error) { return "", nil }
func (s *stubGitHub) UpdateBranch(_, _, _ string) (bool, error)            { return false, nil }
func (s *stubGitHub) ListChecks(_, _ string) ([]git.CICheckResult, error)  { return nil, nil }
func (s *stubGitHub) EditPR(_, _, _, _ string) error                       { return nil }
func (s *stubGitHub) GetRunLog(_, _ string) string                         { return "" }
func (s *stubGitHub) CheckEnforceAdmins(_, _ string) (bool, error)         { return false, nil }
func (s *stubGitHub) PostEnforceAdmins(_, _ string) (string, error)        { return "", nil }
func (s *stubGitHub) FindPR(_, _ string) (string, string, string, error)   { return "", "", "", nil }
func (s *stubGitHub) SearchPR(_, _ string) (string, error)                 { return "", nil }
func (s *stubGitHub) PRDiff(_, _ string) (string, error)                   { return "", nil }
func (s *stubGitHub) GetPRState(_, _ string) (string, error)               { return s.prState, nil }
func (s *stubGitHub) GetPRBase(_, _ string) (string, error)                { return s.prBase, nil }
func (s *stubGitHub) GetPRHead(_, _ string) (string, error)                { return s.prHead, nil }
func (s *stubGitHub) GetPRHeadSHA(_, _ string) (string, error)             { return "", nil }
func (s *stubGitHub) ListOpenPRBranches(_ string) ([]string, error)        { return s.openPRBranches, nil }

// getPRBase takes only a GitHub interface and workDir — no Loop needed.
