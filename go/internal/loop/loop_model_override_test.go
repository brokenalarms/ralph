package loop

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// A task with metadata model=sonnet must run its iteration agent on sonnet,
// with the log line naming the model and the bead-metadata-override source.
func TestResolveAgentModel_ValidOverride_UsesTaskModel(t *testing.T) {
	backend := &testutil.MutableBackend{
		Metadata: map[string]map[string]string{
			"ralph-abc": {"model": "sonnet"},
		},
	}
	var buf bytes.Buffer
	l := &Loop{
		cfg:         Config{WorkingModel: "opus"},
		taskBackend: backend,
		logger:      logging.NewWithWriter(&buf),
	}

	model, source := l.resolveAgentModel("ralph-abc")

	if model != "sonnet" {
		t.Errorf("expected model %q, got %q", "sonnet", model)
	}
	if source != "set by bead model metadata" {
		t.Errorf("expected source %q, got %q", "set by bead model metadata", source)
	}
}

// A task with an unrecognized model metadata value must log a warning and
// fall back to cfg.WorkingModel rather than passing the bogus value through,
// reporting the fallback's true origin (here, the built-in default since
// WorkingModelSource is unset).
func TestResolveAgentModel_InvalidOverride_FallsBackWithWarning(t *testing.T) {
	backend := &testutil.MutableBackend{
		Metadata: map[string]map[string]string{
			"ralph-abc": {"model": "gpt5"},
		},
	}
	var buf bytes.Buffer
	l := &Loop{
		cfg:         Config{WorkingModel: "opus"},
		taskBackend: backend,
		logger:      logging.NewWithWriter(&buf),
	}

	model, source := l.resolveAgentModel("ralph-abc")

	if model != "opus" {
		t.Errorf("expected fallback to WorkingModel %q, got %q", "opus", model)
	}
	if source != "built-in default" {
		t.Errorf("expected fallback source %q, got %q", "built-in default", source)
	}
	if !strings.Contains(buf.String(), "gpt5") {
		t.Errorf("expected warning to name the unrecognized value, got log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "opus") {
		t.Errorf("expected warning to name the fallback model, got log: %s", buf.String())
	}
}

// A task with an unrecognized model metadata value falls back to
// cfg.WorkingModel, and the reported source must reflect config.toml when
// that is what set WorkingModelSource — not a bare "built-in default".
func TestResolveAgentModel_InvalidOverride_FallsBackReportsConfigTomlSource(t *testing.T) {
	backend := &testutil.MutableBackend{
		Metadata: map[string]map[string]string{
			"ralph-abc": {"model": "gpt5"},
		},
	}
	l := &Loop{
		cfg:         Config{WorkingModel: "opus", WorkingModelSource: "set by working_model in config.toml"},
		taskBackend: backend,
		logger:      logging.New(nil),
	}

	model, source := l.resolveAgentModel("ralph-abc")

	if model != "opus" {
		t.Errorf("expected fallback to WorkingModel %q, got %q", "opus", model)
	}
	if source != "set by working_model in config.toml" {
		t.Errorf("expected fallback source %q, got %q", "set by working_model in config.toml", source)
	}
}

// A task with no model metadata must behave exactly as before: the working
// model is used unchanged, with the source naming the built-in default when
// WorkingModelSource is unset.
func TestResolveAgentModel_NoMetadata_UsesWorkingModelUnchanged(t *testing.T) {
	backend := &testutil.MutableBackend{}
	l := &Loop{
		cfg:         Config{WorkingModel: "opus"},
		taskBackend: backend,
		logger:      logging.New(nil),
	}

	model, source := l.resolveAgentModel("ralph-abc")

	if model != "opus" {
		t.Errorf("expected WorkingModel %q, got %q", "opus", model)
	}
	if source != "built-in default" {
		t.Errorf("expected source %q, got %q", "built-in default", source)
	}
}

// A task with no model metadata and a WorkingModelSource set from
// config.toml must report that source verbatim, not the built-in default.
func TestResolveAgentModel_NoMetadata_ReportsConfigTomlSource(t *testing.T) {
	backend := &testutil.MutableBackend{}
	l := &Loop{
		cfg:         Config{WorkingModel: "sonnet", WorkingModelSource: "set by working_model in config.toml"},
		taskBackend: backend,
		logger:      logging.New(nil),
	}

	model, source := l.resolveAgentModel("ralph-abc")

	if model != "sonnet" {
		t.Errorf("expected WorkingModel %q, got %q", "sonnet", model)
	}
	if source != "set by working_model in config.toml" {
		t.Errorf("expected source %q, got %q", "set by working_model in config.toml", source)
	}
}

// End-to-end: runAgent must actually pass the task-overridden model through
// to the runner's claude.RunConfig.Model, not just resolve it internally.
func TestRunAgent_ModelOverride_ReachesRunner(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	backend := &testutil.MutableBackend{
		Metadata: map[string]map[string]string{
			"ralph-abc": {"model": "sonnet"},
		},
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		WorkingModel:  "opus",
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	var capturedModel string
	l.runner = &stubRunner{onRunCfg: func(cfg claude.RunConfig) {
		capturedModel = cfg.Model
	}}

	l.runAgent(context.Background(), taskContext{id: "ralph-abc", title: "Fix something"}, 0)

	if capturedModel != "sonnet" {
		t.Errorf("expected runner to receive overridden model %q, got %q", "sonnet", capturedModel)
	}
}
