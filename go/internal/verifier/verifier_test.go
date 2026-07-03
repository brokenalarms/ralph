package verifier

import (
	"context"
	"os"
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
