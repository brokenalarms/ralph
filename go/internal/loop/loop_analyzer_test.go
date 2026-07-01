package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// errorLogLine returns a single JSON-lines log entry that the analyzer's error
// fingerprinter recognises. Three lines with the same message cross the
// repeated-error threshold of 3.
func errorLogLine(msg string) string {
	return `{"type":"assistant","message":{"content":[{"type":"text","text":"` + msg + `"}]}}`
}

// writeErrors writes n copies of an error line to path (truncating any prior
// content). Used to seed the raw log so analyzeIteration sees repeated errors.
func writeErrors(t *testing.T, path, msg string, n int) {
	t.Helper()
	var sb strings.Builder
	line := errorLogLine(msg)
	for i := 0; i < n; i++ {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("writeErrors: %v", err)
	}
}

// appendErrors appends n copies of an error line to path. Used for the second
// iteration in the same-task consecutive tests where the file already exists.
func appendErrors(t *testing.T, path, msg string, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("appendErrors: %v", err)
	}
	defer f.Close()
	line := errorLogLine(msg)
	for i := 0; i < n; i++ {
		f.WriteString(line + "\n")
	}
}

// appendBenignLines appends n copies of a plain assistant-text log line to
// path — matches neither the stuck-phrase nor error-fingerprint detectors,
// so it only feeds the analyzer's stagnation counter.
func appendBenignLines(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("appendBenignLines: %v", err)
	}
	defer f.Close()
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"still working on it"}]}}`
	for i := 0; i < n; i++ {
		f.WriteString(line + "\n")
	}
}

// Proves: the first time a task triggers repeated_error, the loop skips the
// task and continues — it does NOT immediately halt (AC6).
func TestLoop_RepeatedError_FirstIterationSkipsTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix login",
				NextID:    "ralph-abc",
			},
		},
	}

	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			// readLogFrom(path, 0) skips the first line (off-by-one when logStart=0
			// and file was empty before the run). Write 4 lines so 3 are seen after
			// the skip — enough to reach the repeated-error threshold of 3.
			writeErrors(t, cfg.RawLog, "Error: cannot find module 'foo'", 4)
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	status, _ := st.Read("status")
	if strings.HasPrefix(status, "halted_repeated_error") {
		t.Errorf("first repeated_error detection should skip, not halt; got status=%q", status)
	}
	if status == "error" {
		t.Errorf("unexpected error status: %q", status)
	}

	// Task must have been skipped, not completed.
	backend.SkipMu.Lock()
	skipped := append([]string(nil), backend.SkippedIDs...)
	backend.SkipMu.Unlock()
	if len(skipped) == 0 {
		t.Error("expected SkipTask to be called for the task, but it was not")
	} else if skipped[0] != "ralph-abc" {
		t.Errorf("expected SkipTask(ralph-abc), got %q", skipped[0])
	}
}

// Proves: when the same skip reason (repeated_error) recurs on a second,
// different task, the loop halts immediately with app_wide:<reason> instead
// of letting the streak continue to the numeric cascade limit — the second
// same-reason failure is conclusive (ralph-qlmy AC2, AC4 non-retryable case).
func TestLoop_SameReasonSkipRecurs_HaltsAppWide(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// seqTaskBackend (defined in loop_branch_test.go) serves tasks in order;
	// SkipTask advances the cursor so the next call returns the next task.
	backend := &seqTaskBackend{
		TrackingBackend: &testutil.TrackingBackend{
			MutableBackend: testutil.MutableBackend{
				StubBackend: testutil.StubBackend{Total: 2},
			},
		},
		queue: []tasks.TaskInfo{
			{ID: "ralph-a1", Title: "Task one"},
			{ID: "ralph-a2", Title: "Task two"},
		},
	}

	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			// Each task appends 4 identical error lines. readLogFrom skips the first
			// line when logStart=0 (off-by-one), so 3 are seen for task 1 — enough
			// to reach the threshold. Task 2 has logStart>0 and sees all 4.
			appendErrors(t, cfg.RawLog, "Error: cannot find module 'foo'", 4)
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	// Non-retryable-style run-count guarantee: repeated_error skips on the
	// very first iteration of each task, so this is 1 run on task 1 + 1 run
	// on task 2 (2 total) before halting — no third task is ever selected.
	status, _ := st.Read("status")
	if status != "halted_app_wide:analyzer" {
		t.Errorf("expected status halted_app_wide:analyzer, got %q", status)
	}

	backend.SkipMu.Lock()
	skippedCount := len(backend.SkippedIDs)
	backend.SkipMu.Unlock()
	if skippedCount != 2 {
		t.Errorf("expected exactly 2 skipped tasks (halt on 2nd, no 3rd run), got %d", skippedCount)
	}
}

// Proves: when the same task triggers repeated_error in two consecutive
// iterations, the loop halts with repeated_error_recurring (AC8).
func TestLoop_RepeatedError_TwoConsecutiveIterationsHalt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// StubBackend.SetSkippedIDs is a no-op, so after the first skip the same
	// task is returned again — simulating a session where the user re-enabled
	// the task or the skip list was reset between runs.
	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix login",
				NextID:    "ralph-abc",
			},
		},
	}

	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			// Always appends 4 identical error lines per invocation.
			// Iteration 1 (logStart=0): readLogFrom skips 1, sees 3 → count=3 → Skip.
			// Iteration 2 (logStart=4): readLogFrom sees 4 new lines; errorHashes
			// already has count=3, so the first new line (count=4 >= 3) returns true
			// immediately → repeatedErrorIterations becomes 2 → Halt.
			appendErrors(t, cfg.RawLog, "Error: cannot find module 'foo'", 4)
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	status, _ := st.Read("status")
	if status != "halted_repeated_error_recurring" {
		t.Errorf("expected halted_repeated_error_recurring after two consecutive repeated_error iterations, got %q", status)
	}
}

// Proves: CascadeSkipLimit=1 in config halts the loop after a single
// distinct-task skip instead of waiting for the default limit of 2 — the
// numeric fallback (which fires only when the reason-aware app-wide check
// does not match) still honors the configured value.
func TestCascadeSkipLimit_ConfigOverridesDefault(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix login",
				NextID:    "ralph-c1",
			},
		},
	}

	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			appendErrors(t, cfg.RawLog, "Error: cannot find module 'foo'", 4)
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations:    10,
		CallsPerHour:     80,
		CascadeSkipLimit: 1,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	status, _ := st.Read("status")
	if status != "halted_cascade_skipped:1" {
		t.Errorf("expected halted_cascade_skipped:1 with CascadeSkipLimit=1, got %q", status)
	}
}

// Proves: a single task's stagnation (3 consecutive no-change iterations)
// skips that task and the loop proceeds to the next ready task rather than
// halting the whole loop (ralph-qlmy AC1) — proven here by observing that a
// second task is actually attempted afterward. When the SAME reason
// (stagnation) recurs on that second task, the loop halts app-wide instead
// of letting it burn its own 3-iteration stagnation bracket a second time
// (AC2/AC7).
func TestLoop_StagnationSkipsTask_ThenSameReasonRecurrenceHalts(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &seqTaskBackend{
		TrackingBackend: &testutil.TrackingBackend{
			MutableBackend: testutil.MutableBackend{
				StubBackend: testutil.StubBackend{Total: 2},
			},
		},
		queue: []tasks.TaskInfo{
			{ID: "ralph-s1", Title: "Task one"},
			{ID: "ralph-s2", Title: "Task two"},
		},
	}

	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			appendBenignLines(t, cfg.RawLog, 2)
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	backend.SkipMu.Lock()
	skippedIDs := append([]string(nil), backend.SkippedIDs...)
	skipReasons := append([]string(nil), backend.SkipReasons...)
	backend.SkipMu.Unlock()

	if len(skippedIDs) < 1 || skippedIDs[0] != "ralph-s1" {
		t.Fatalf("expected ralph-s1 to be skipped first, got %v", skippedIDs)
	}
	if skipReasons[0] != string(tasks.SkipStagnation) {
		t.Errorf("expected skip reason %q for the first task, got %q", tasks.SkipStagnation, skipReasons[0])
	}

	if len(skippedIDs) != 2 || skippedIDs[1] != "ralph-s2" {
		t.Fatalf("expected ralph-s2 to also be attempted and skipped (proving the loop proceeded past ralph-s1's stagnation instead of halting), got %v", skippedIDs)
	}

	status, _ := st.Read("status")
	if status != "halted_app_wide:stagnation" {
		t.Errorf("expected status halted_app_wide:stagnation, got %q", status)
	}
}

// Proves: when the analyzer would skip a task that has open dependents, the
// task is parked (SkipTask IS called) — open dependents are no longer a
// reason to refuse or escalate to halt. Dependent re-pointing is a triage
// concern for the task manager, not a loop invariant.
func TestLoop_SkipWithOpenDependents_ParksNotHalts(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:      1,
				Total:          1,
				NextTask:       "Fix login",
				NextID:         "ralph-abc",
				OpenDependents: []string{"ralph-dep1"},
			},
		},
	}

	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			// Remove the task from the loop's inbox so the loop exits cleanly after
			// the first skip; otherwise the stub re-selects it indefinitely.
			backend.Lock()
			backend.Remaining = 0
			backend.Unlock()
			// 4 lines so 3 survive the readLogFrom off-by-one when logStart=0.
			writeErrors(t, cfg.RawLog, "Error: cannot find module 'foo'", 4)
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	// The task MUST be parked regardless of open dependents.
	backend.SkipMu.Lock()
	skippedIDs := append([]string(nil), backend.SkippedIDs...)
	backend.SkipMu.Unlock()
	if len(skippedIDs) == 0 {
		t.Error("expected SkipTask to be called for task with open dependents, but it was not")
	} else if skippedIDs[0] != "ralph-abc" {
		t.Errorf("expected SkipTask(ralph-abc), got %q", skippedIDs[0])
	}

	// The loop must NOT halt with a strand-dependents error.
	status, _ := st.Read("status")
	if strings.HasPrefix(status, "halted_skip_would_strand_dependents") {
		t.Errorf("loop must not halt with strand error; got %q", status)
	}
}
