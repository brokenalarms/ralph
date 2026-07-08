package verifier

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
)

// stubQuerier is a minimal Querier double that records the last prompt it
// received and plays back pre-configured responses in call order.
type stubQuerier struct {
	responses  []string
	calls      int
	lastPrompt string
}

func newStubQuerier(responses ...string) *stubQuerier {
	return &stubQuerier{responses: responses}
}

func (s *stubQuerier) Query(_ context.Context, _, prompt, _ string, _ []string) (string, error) {
	s.lastPrompt = prompt
	idx := s.calls
	s.calls++
	if idx < len(s.responses) {
		return s.responses[idx], nil
	}
	return "YES: default", nil
}

func (s *stubQuerier) LastPrompt() string { return s.lastPrompt }
func (s *stubQuerier) Calls() int         { return s.calls }

func newTestVerifierWithPrompts(t *testing.T, promptsDir string, q Querier) *Verifier {
	t.Helper()
	return New(Config{
		PromptsDir: promptsDir,
	}, logging.New(nil), nil, q)
}

// RunPreIterationTests appends the passing-tests status line from the
// status-tests-pass.md prompt template — not a hardcoded Go string — so a
// distinctive marker written into the template must appear in the message.
func TestVerifier_RunPreIterationTests_PassingTests_UsesTemplate(t *testing.T) {
	dir := t.TempDir()
	promptsDir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)
	os.WriteFile(filepath.Join(promptsDir, "status-tests-pass.md"), []byte("- CUSTOM PASS MARKER"), 0o644)

	v := newTestVerifierWithPrompts(t, promptsDir, nil)
	result := v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})

	if !strings.Contains(result.Message, "CUSTOM PASS MARKER") {
		t.Errorf("expected message sourced from status-tests-pass.md template, got: %q", result.Message)
	}
}

// RunPreIterationTests appends the failing-tests status line from the
// status-tests-failing.md prompt template.
func TestVerifier_RunPreIterationTests_FailingTests_UsesTemplate(t *testing.T) {
	dir := t.TempDir()
	promptsDir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\t@echo 'boom' && exit 1\n"), 0o644)
	os.WriteFile(filepath.Join(promptsDir, "status-tests-failing.md"), []byte("- CUSTOM FAIL MARKER"), 0o644)

	v := newTestVerifierWithPrompts(t, promptsDir, nil)
	result := v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})

	if !strings.Contains(result.Message, "CUSTOM FAIL MARKER") {
		t.Errorf("expected message sourced from status-tests-failing.md template, got: %q", result.Message)
	}
	if !strings.Contains(result.Message, "boom") {
		t.Errorf("expected failure output included, got: %q", result.Message)
	}
}

// RunPreIterationTests appends the compile-failing status line from the
// status-build-failing.md prompt template.
func TestVerifier_RunPreIterationTests_CompileFailing_UsesTemplate(t *testing.T) {
	dir := t.TempDir()
	promptsDir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module brokenpkg\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package brokenpkg\n\nfunc Foo() int {\n\treturn \"not an int\"\n}\n"), 0o644)
	os.WriteFile(filepath.Join(promptsDir, "status-build-failing.md"), []byte("- CUSTOM COMPILE FAIL MARKER"), 0o644)

	v := newTestVerifierWithPrompts(t, promptsDir, nil)
	result := v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})

	if !strings.Contains(result.Message, "CUSTOM COMPILE FAIL MARKER") {
		t.Errorf("expected message sourced from status-build-failing.md template, got: %q", result.Message)
	}
}

// LLMVerify treats a missing verify-review.md template as an infrastructure
// fault: it skips verification (Passed=true) rather than falling back to a
// divergent inline prompt, and never calls the querier.
func TestVerifier_LLMVerify_MissingReviewTemplate_SkipsVerification(t *testing.T) {
	promptsDir := t.TempDir() // no verify-review.md written

	q := newStubQuerier("NO: should never be reached")
	v := newTestVerifierWithPrompts(t, promptsDir, q)

	result, _ := v.LLMVerify(LLMVerifyOpts{
		Ctx:  context.Background(),
		Diff: "+ some diff",
	})

	if !result.Passed {
		t.Errorf("expected verification to be skipped (Passed=true) when template is missing, got Passed=false, reason=%q", result.Reason)
	}
	if !strings.Contains(result.Reason, "skipped") {
		t.Errorf("expected skip reason, got: %q", result.Reason)
	}
	if q.Calls() != 0 {
		t.Errorf("expected querier not to be called when template is missing, got %d calls", q.Calls())
	}
}

// LLMVerify builds its prompt from the verify-review.md template — a
// distinctive marker written into the template must reach the querier
// alongside the substituted task title.
func TestVerifier_LLMVerify_UsesReviewTemplate(t *testing.T) {
	promptsDir := t.TempDir()
	os.WriteFile(filepath.Join(promptsDir, "verify-review.md"),
		[]byte("CUSTOM REVIEW MARKER\nTITLE: {{TASK_TITLE}}\nDIFF: {{DIFF}}"), 0o644)

	q := newStubQuerier("NO: rejected for testing")
	v := newTestVerifierWithPrompts(t, promptsDir, q)

	result, _ := v.LLMVerify(LLMVerifyOpts{
		Ctx:   context.Background(),
		Title: "Fix the widget",
		Diff:  "+ some diff",
	})

	if !strings.Contains(q.LastPrompt(), "CUSTOM REVIEW MARKER") {
		t.Errorf("expected prompt sourced from verify-review.md template, got: %q", q.LastPrompt())
	}
	if !strings.Contains(q.LastPrompt(), "Fix the widget") {
		t.Errorf("expected task title substituted into prompt, got: %q", q.LastPrompt())
	}
	if result.Passed {
		t.Error("expected rejection to be reported (Passed=false)")
	}
}

// gitRun runs a git command in dir, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initGitRepoForCache creates a real git repository at dir with one commit,
// so verify.TreeHash / verify.WorktreeClean (the green-tree cache's data
// source) have real git state to operate on. A local identity is configured
// so the commit succeeds regardless of the environment's global git config.
func initGitRepoForCache(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "init")
}

// writeCountingMakefile writes a ralph-verify Makefile target to dir that
// appends one line to counterFile (kept outside dir, so invocations don't
// dirty the git worktree under test) every time it runs.
func writeCountingMakefile(t *testing.T, dir, counterFile string) {
	t.Helper()
	makefile := fmt.Sprintf("ralph-verify:\n\t@echo run >> %s\n", counterFile)
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
}

// countLines returns the number of lines in path, or 0 if it does not exist.
// Used to count real test-command invocations recorded by
// writeCountingMakefile's counter file.
func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read counter file: %v", err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// headSHA returns the commit SHA at HEAD in dir.
func headSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// RunTests caches a green run keyed by tree hash: a second call on the same
// clean, unchanged tree returns a pass without invoking the test command
// again.
func TestVerifier_RunTests_CachesGreenTreeHash(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)

	result1, _ := v.RunTests(context.Background(), dir)
	if !result1.Passed {
		t.Fatalf("expected first run to pass, got: %+v", result1)
	}
	result2, _ := v.RunTests(context.Background(), dir)
	if !result2.Passed {
		t.Fatalf("expected second (cached) run to pass, got: %+v", result2)
	}

	if got := countLines(t, counterFile); got != 1 {
		t.Fatalf("expected the test command to run exactly once across both calls, got %d invocations", got)
	}
}

// A cache hit emits a distinct "Tests cached: tree <hash> already green" log
// line so log-based timing audits can distinguish hits from real runs.
func TestVerifier_RunTests_CacheHit_EmitsDistinctLogLine(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	var buf bytes.Buffer
	v := New(Config{}, logging.NewWithWriter(&buf), nil, nil)

	v.RunTests(context.Background(), dir)
	buf.Reset()
	v.RunTests(context.Background(), dir)

	if !strings.Contains(buf.String(), "Tests cached: tree") {
		t.Errorf("expected a distinct cache-hit log line, got: %s", buf.String())
	}
}

// A commit that changes the tree between calls invalidates the cache — the
// test command runs for real again.
func TestVerifier_RunTests_TreeChanged_RunsAgain(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)
	v.RunTests(context.Background(), dir)

	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write extra.txt: %v", err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "second commit")

	v.RunTests(context.Background(), dir)

	if got := countLines(t, counterFile); got != 2 {
		t.Fatalf("expected 2 real invocations after the tree changed, got %d", got)
	}
}

// An uncommitted change (dirty worktree) invalidates the cache even though
// HEAD^{tree} is unchanged — the test command runs for real again.
func TestVerifier_RunTests_DirtyWorktree_RunsAgain(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)
	v.RunTests(context.Background(), dir)

	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write untracked.txt: %v", err)
	}

	v.RunTests(context.Background(), dir)

	if got := countLines(t, counterFile); got != 2 {
		t.Fatalf("expected 2 real invocations when the worktree is dirty, got %d", got)
	}
}

// A failing run must not populate the cache — the immediately following
// RunTests call executes the suite for real, not a cached failure.
func TestVerifier_RunTests_FailingRun_DoesNotCache(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	makefile := fmt.Sprintf("ralph-verify:\n\t@echo run >> %s\n\t@exit 1\n", counterFile)
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)

	result1, _ := v.RunTests(context.Background(), dir)
	if result1.Passed {
		t.Fatal("expected first run to fail")
	}
	result2, _ := v.RunTests(context.Background(), dir)
	if result2.Passed {
		t.Fatal("expected second run to fail for real too — a failing run must not populate the cache")
	}

	if got := countLines(t, counterFile); got != 2 {
		t.Fatalf("expected 2 real invocations — a failing run must not populate the cache, got %d", got)
	}
}

// The cache key is the tree hash, not the commit SHA: an empty commit
// (new SHA, identical tree) still hits the cache.
func TestVerifier_RunTests_CacheKeyIsTreeHashNotCommitSHA(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)
	v.RunTests(context.Background(), dir)

	firstSHA := headSHA(t, dir)
	gitRun(t, dir, "commit", "--allow-empty", "-m", "empty commit, same tree")
	secondSHA := headSHA(t, dir)
	if firstSHA == secondSHA {
		t.Fatal("expected the empty commit to produce a new commit SHA")
	}

	result2, _ := v.RunTests(context.Background(), dir)
	if !result2.Passed {
		t.Fatalf("expected a cache hit despite the new commit SHA (same tree), got: %+v", result2)
	}
	if got := countLines(t, counterFile); got != 1 {
		t.Fatalf("expected exactly 1 real invocation — tree hash unchanged despite the new commit SHA, got %d", got)
	}
}

// RunPreIterationTests writes the green-tree cache on a passing run, so a
// subsequent RunTests call on the same unchanged, clean tree is a cache hit.
func TestVerifier_RunPreIterationTests_GreenRun_WritesCache(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)
	v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})

	result, _ := v.RunTests(context.Background(), dir)
	if !result.Passed {
		t.Fatalf("expected a cache hit after a green RunPreIterationTests run, got: %+v", result)
	}
	if got := countLines(t, counterFile); got != 1 {
		t.Fatalf("expected exactly 1 real invocation (from RunPreIterationTests) — RunTests should have hit the cache, got %d", got)
	}
}

// RunPreIterationTests itself must check the green-tree cache on entry, not
// just write it: a prior green run (via RunTests or RunPreIterationTests) on
// the same unchanged, clean tree means the next RunPreIterationTests call is
// a cache hit — the pre-iteration test command must not run again.
func TestVerifier_RunPreIterationTests_CacheHit_SkipsRealRun(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)

	first := v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})
	if !first.TestResult.Passed {
		t.Fatalf("expected first run to pass, got: %+v", first.TestResult)
	}

	second := v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})
	if !second.TestResult.Passed {
		t.Fatalf("expected second (cached) run to report passed, got: %+v", second.TestResult)
	}

	if got := countLines(t, counterFile); got != 1 {
		t.Fatalf("expected exactly 1 real invocation across both RunPreIterationTests calls, got %d", got)
	}
}

// A commit that changes the tree between two RunPreIterationTests calls
// invalidates the cache — e.g. the base branch moved between loop
// iterations — so the pre-iteration test command runs for real again rather
// than trusting a stale cache entry.
func TestVerifier_RunPreIterationTests_TreeChanged_RunsAgain(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)
	v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})

	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write extra.txt: %v", err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "second commit")

	second := v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})
	if !second.TestResult.Passed {
		t.Fatalf("expected second run to pass, got: %+v", second.TestResult)
	}

	if got := countLines(t, counterFile); got != 2 {
		t.Fatalf("expected 2 real invocations after the tree changed, got %d", got)
	}
}

// A cache hit inside RunPreIterationTests emits the same distinct "Tests
// cached: tree <hash> already green" log line RunTests uses, so log-based
// timing audits can identify a skipped pre-iteration run the same way.
func TestVerifier_RunPreIterationTests_CacheHit_EmitsDistinctLogLine(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	var buf bytes.Buffer
	v := New(Config{}, logging.NewWithWriter(&buf), nil, nil)

	v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})
	buf.Reset()
	v.RunPreIterationTests(PreIterationInput{Ctx: context.Background(), WorkDir: dir})

	if !strings.Contains(buf.String(), "Tests cached: tree") {
		t.Errorf("expected a distinct cache-hit log line, got: %s", buf.String())
	}
}

// SeedGreenCache lets a freshly constructed Verifier (simulating a new loop
// process) start with a cache hit when the persisted dir/tree from a prior
// session's state.json matches the current worktree — without any real run
// happening in this process instance.
func TestVerifier_SeedGreenCache_EnablesHitWithoutPriorRunInThisInstance(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	// Prime the tree hash using a throwaway verifier instance — this stands
	// in for a value read out of state.json.
	tree := treeHashForTest(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)
	v.SeedGreenCache(dir, tree)

	result, _ := v.RunTests(context.Background(), dir)
	if !result.Passed {
		t.Fatalf("expected a cache hit from the seeded cache, got: %+v", result)
	}
	if got := countLines(t, counterFile); got != 0 {
		t.Fatalf("expected zero real invocations — the seeded cache should have been used, got %d", got)
	}
}

// GreenCache exposes the current in-memory cache (dir, tree) after a green
// run so the loop can persist it to state.json.
func TestVerifier_GreenCache_ReturnsDirAndTreeAfterGreenRun(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	writeCountingMakefile(t, dir, counterFile)
	initGitRepoForCache(t, dir)

	v := New(Config{}, logging.New(nil), nil, nil)

	if gotDir, gotTree := v.GreenCache(); gotDir != "" || gotTree != "" {
		t.Fatalf("expected empty cache before any run, got dir=%q tree=%q", gotDir, gotTree)
	}

	v.RunTests(context.Background(), dir)

	gotDir, gotTree := v.GreenCache()
	if gotDir != dir {
		t.Errorf("GreenCache dir = %q, want %q", gotDir, dir)
	}
	wantTree := treeHashForTest(t, dir)
	if gotTree != wantTree {
		t.Errorf("GreenCache tree = %q, want %q", gotTree, wantTree)
	}
}

// treeHashForTest returns the current HEAD^{tree} hash for dir, used to
// build expectations without importing internal/git (would cycle).
func treeHashForTest(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD^{tree}")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD^{tree}: %v", err)
	}
	return strings.TrimSpace(string(out))
}
