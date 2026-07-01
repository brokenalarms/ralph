package analyzer

import (
	"fmt"
	"testing"
)

// Verifies that an iteration with no log output is treated as benign,
// so empty/quiet iterations don't trip any detection heuristics.
func TestEmptyLogContinues(t *testing.T) {
	a := New()
	r := a.Analyze(IterationState{})
	if r.Action != Continue {
		t.Errorf("empty log: got action %d, want Continue", r.Action)
	}
}

// Verifies that an explicit "I'm blocked" phrase triggers a stuck warning
// on the first occurrence, giving the loop a chance to recover before halting.
func TestStuckPhraseWarnsFirst(t *testing.T) {
	a := New()
	log := assistantTextMsg("I tried but I'm blocked on this task")
	r := a.Analyze(IterationState{IterationLog: log})
	if r.Action != Warn || r.Reason != "stuck_indicators_detected" {
		t.Errorf("first stuck: got %+v, want Warn/stuck_indicators_detected", r)
	}
}

// Verifies that two consecutive stuck-detected iterations escalate to halt,
// matching the 2-strike threshold from ralph.sh.
func TestStuckLoopHaltsAfterTwoConsecutive(t *testing.T) {
	a := New()
	stuckLog := assistantTextMsg("I cannot proceed with this task")

	r := a.Analyze(IterationState{IterationLog: stuckLog})
	if r.Action != Warn {
		t.Fatalf("first stuck iteration: got %+v, want Warn", r)
	}

	r = a.Analyze(IterationState{IterationLog: stuckLog})
	if r.Action != Halt || r.Reason != "stuck_loop" {
		t.Errorf("second stuck iteration: got %+v, want Halt/stuck_loop", r)
	}
}

// Verifies that a non-stuck iteration between two stuck ones resets the
// counter, so occasional hiccups don't accumulate into a false halt.
func TestStuckCounterResetsOnCleanIteration(t *testing.T) {
	a := New()
	stuckLog := assistantTextMsg("unable to complete this step")
	cleanLog := assistantTextMsg("making progress on the task")

	a.Analyze(IterationState{IterationLog: stuckLog})
	a.Analyze(IterationState{IterationLog: cleanLog, HasDiff: true, ChangedFiles: []string{"main.go"}})
	r := a.Analyze(IterationState{IterationLog: stuckLog})

	if r.Action != Warn {
		t.Errorf("stuck after reset: got %+v, want Warn (not Halt)", r)
	}
}

// Verifies that repeated tool calls alone do NOT trigger stuck detection.
// This heuristic was removed — only explicit stuck phrases trigger it now.
func TestRepeatedToolCallsNotStuck(t *testing.T) {
	a := New()
	log := assistantToolUseMsg("Bash", "/usr/bin/test") + "\n" +
		assistantToolUseMsg("Bash", "/usr/bin/test") + "\n" +
		assistantToolUseMsg("Bash", "/usr/bin/test") + "\n" +
		assistantToolUseMsg("Bash", "/usr/bin/test") + "\n" +
		assistantToolUseMsg("Bash", "/usr/bin/test") + "\n"

	r := a.Analyze(IterationState{IterationLog: log})
	if r.Action != Continue {
		t.Errorf("repeated tool calls should not trigger stuck: got %+v, want Continue", r)
	}
}

// Verifies that 3 consecutive iterations with no git changes and no signal
// skip the task (not halt the whole loop) — a single stagnant task should
// not prevent the loop from moving on to other ready work.
func TestStagnationSkipsAfterThree(t *testing.T) {
	a := New()
	noChange := IterationState{IterationLog: "thinking about things\n"}

	for i := 0; i < 2; i++ {
		r := a.Analyze(noChange)
		if r.Action != Continue {
			t.Fatalf("stagnation iteration %d: got %+v, want Continue", i+1, r)
		}
	}

	r := a.Analyze(noChange)
	if r.Action != Skip || r.Reason != "stagnation" {
		t.Errorf("3rd stagnant iteration: got %+v, want Skip/stagnation", r)
	}
}

// Verifies that any actual change (diff, commit, or signal) resets the
// stagnation counter, so productive work prevents false stagnation halts.
func TestStagnationResetsOnChange(t *testing.T) {
	a := New()
	noChange := IterationState{IterationLog: "no progress\n"}
	withChange := IterationState{IterationLog: "made changes\n", HasDiff: true, ChangedFiles: []string{"app.go"}}

	a.Analyze(noChange)
	a.Analyze(noChange)
	a.Analyze(withChange) // resets counter

	r := a.Analyze(noChange)
	if r.Action == Halt {
		t.Error("stagnation should have reset after a change")
	}
}

// Verifies that a signal (task completion) counts as progress and resets
// the stagnation counter even when there are no file changes.
func TestStagnationResetsOnSignal(t *testing.T) {
	a := New()
	noChange := IterationState{IterationLog: "idle\n"}
	withSignal := IterationState{IterationLog: "signaled\n", HasSignal: true}

	a.Analyze(noChange)
	a.Analyze(noChange)
	a.Analyze(withSignal)

	r := a.Analyze(noChange)
	if r.Action == Halt {
		t.Error("stagnation should have reset after a signal")
	}
}

// Verifies that 3 consecutive iterations where ALL changed files are test
// files triggers a test_saturation halt, catching agents stuck in a
// write-tests-only loop without advancing the actual feature.
func TestTestSaturationHaltsAfterThree(t *testing.T) {
	a := New()
	testOnly := IterationState{
		IterationLog: "wrote some tests\n",
		HasDiff:      true,
		ChangedFiles: []string{"pkg/handler_test.go", "tests/integration.py"},
	}

	for i := 0; i < 2; i++ {
		r := a.Analyze(testOnly)
		if r.Action != Continue {
			t.Fatalf("test-only iteration %d: got %+v, want Continue", i+1, r)
		}
	}

	r := a.Analyze(testOnly)
	if r.Action != Halt || r.Reason != "test_saturation" {
		t.Errorf("3rd test-only iteration: got %+v, want Halt/test_saturation", r)
	}
}

// Verifies that a non-test file among test files resets the test saturation
// counter, since the agent is making substantive code changes.
func TestTestSaturationResetsWithNonTestFile(t *testing.T) {
	a := New()
	testOnly := IterationState{
		IterationLog: "tests\n",
		HasDiff:      true,
		ChangedFiles: []string{"handler_test.go"},
	}
	mixed := IterationState{
		IterationLog: "feature + tests\n",
		HasDiff:      true,
		ChangedFiles: []string{"handler.go", "handler_test.go"},
	}

	a.Analyze(testOnly)
	a.Analyze(testOnly)
	a.Analyze(mixed) // resets counter

	r := a.Analyze(testOnly)
	if r.Action == Halt {
		t.Error("test saturation should have reset after mixed change")
	}
}

// Verifies isTestFile correctly classifies various test file patterns,
// matching ralph.sh's basename regex and top-dir checks.
func TestIsTestFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"pkg/handler_test.go", true},
		{"tests/e2e.py", true},
		{"__tests__/App.test.js", true},
		{"spec/models/user_spec.rb", true},
		{"src/handler.go", false},
		{"lib/utils.py", false},
		{"README.md", false},
		{"test_helpers.py", true},
		{"", false},
		{"tests/helpers/setup.js", true},
		{"musicXmusicTests/SetupHelper.swift", true},
		{"musicXmusicUITests/UIFixture.swift", true},
	}

	for _, tc := range tests {
		got := isTestFile(tc.path)
		if got != tc.want {
			t.Errorf("isTestFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Proves: no false positives on normal progress — real source file changes
// reset both the stagnant and test-only counters to 0.
func TestNormalProgressResetsCounters(t *testing.T) {
	a := New()
	a.stagnantCount = 2
	a.testOnlyCount = 2

	realChange := IterationState{
		IterationLog: assistantTextMsg("implementing feature"),
		HasDiff:      true,
		ChangedFiles: []string{"src.js"},
	}
	r := a.Analyze(realChange)
	if r.Action != Continue {
		t.Errorf("real change: got %+v, want Continue", r)
	}
	if a.stagnantCount != 0 {
		t.Errorf("stagnantCount = %d, want 0", a.stagnantCount)
	}
	if a.testOnlyCount != 0 {
		t.Errorf("testOnlyCount = %d, want 0", a.testOnlyCount)
	}
}

// Proves: a source file among test files resets the test-only counter,
// since the agent is making substantive code changes alongside tests.
func TestMixedTestAndSourceResetsTestOnlyCount(t *testing.T) {
	a := New()
	a.testOnlyCount = 2

	mixed := IterationState{
		IterationLog: assistantTextMsg("add source and tests"),
		HasDiff:      true,
		ChangedFiles: []string{"AppTests/HTTPClientTests.swift", "src/HTTPClient.swift"},
	}
	r := a.Analyze(mixed)
	if r.Action != Continue {
		t.Errorf("mixed change: got %+v, want Continue", r)
	}
	if a.testOnlyCount != 0 {
		t.Errorf("testOnlyCount = %d, want 0", a.testOnlyCount)
	}
}

// Proves: files under a top-level test directory (tests/helpers/setup.js)
// are counted as test files, incrementing the test-only counter.
func TestFilesUnderTopLevelTestDirCountAsTestFiles(t *testing.T) {
	a := New()

	testHelper := IterationState{
		IterationLog: assistantTextMsg("adding test helper"),
		HasDiff:      true,
		ChangedFiles: []string{"tests/helpers/setup.js"},
	}
	r := a.Analyze(testHelper)
	if r.Action != Continue {
		t.Errorf("test helper: got %+v, want Continue", r)
	}
	if a.testOnlyCount != 1 {
		t.Errorf("testOnlyCount = %d, want 1", a.testOnlyCount)
	}
}

// Proves: files under suffixed test directories (e.g. musicXmusicTests,
// musicXmusicUITests) are counted as test files.
func TestFilesUnderSuffixedTestDirsCountAsTestFiles(t *testing.T) {
	a := New()

	suffixedTests := IterationState{
		IterationLog: assistantTextMsg("adding test helpers"),
		HasDiff:      true,
		ChangedFiles: []string{"musicXmusicTests/SetupHelper.swift", "musicXmusicUITests/UIFixture.swift"},
	}
	r := a.Analyze(suffixedTests)
	if r.Action != Continue {
		t.Errorf("suffixed test dirs: got %+v, want Continue", r)
	}
	if a.testOnlyCount != 1 {
		t.Errorf("testOnlyCount = %d, want 1", a.testOnlyCount)
	}
}

// Proves: ralph detects repeated identical errors and returns Skip on the first
// iteration that triggers the threshold (3 occurrences accumulated across
// iterations for the same task). The second consecutive iteration that triggers
// the threshold escalates to Halt with reason repeated_error_recurring.
func TestRepeatedErrorFingerprintTriggersHalt(t *testing.T) {
	a := New()
	log := assistantTextMsg("Error: cannot find module 'foo'\nsome other output")

	state := IterationState{IterationLog: log, TaskKey: "test-task-1"}

	r := a.Analyze(state)
	if r.Action == Skip && r.Reason == "repeated_error" {
		t.Fatal("first call should not trigger repeated_error (count only 1)")
	}

	r = a.Analyze(state)
	if r.Action == Skip && r.Reason == "repeated_error" {
		t.Fatal("second call should not trigger repeated_error (count only 2)")
	}

	// Third call: count hits 3 — first detection, returns Skip (not Halt yet).
	r = a.Analyze(state)
	if r.Action != Skip || r.Reason != "repeated_error" {
		t.Errorf("third call: got %+v, want Skip/repeated_error", r)
	}

	// Fourth call: same task, second consecutive iteration with repeated_error — escalates to Halt.
	r = a.Analyze(state)
	if r.Action != Halt || r.Reason != "repeated_error_recurring" {
		t.Errorf("fourth call: got %+v, want Halt/repeated_error_recurring", r)
	}
}

// Proves: errors with different volatile parts (timestamps, UUIDs) are
// treated as the same error after normalization.
func TestErrorNormalizationCollapsesTimestampsAndUUIDs(t *testing.T) {
	a := New()

	text1 := assistantTextMsg("Error: 2026-03-20T10:00:00Z request a1b2c3d4-e5f6-7890-abcd-ef1234567890 failed")
	text2 := assistantTextMsg("Error: 2026-03-21T15:30:00Z request 11111111-2222-3333-4444-555555555555 failed")
	text3 := assistantTextMsg("Error: 2026-03-22T09:15:00Z request deadbeef-cafe-babe-dead-beefcafebabe failed")

	a.Analyze(IterationState{IterationLog: text1, TaskKey: "test-task-2"})
	a.Analyze(IterationState{IterationLog: text2, TaskKey: "test-task-2"})

	// Third call: count hits 3 — first detection, returns Skip (not Halt yet).
	r := a.Analyze(IterationState{IterationLog: text3, TaskKey: "test-task-2"})
	if r.Action != Skip || r.Reason != "repeated_error" {
		t.Errorf("third call: got %+v, want Skip/repeated_error", r)
	}

	// Fourth call: same task, second consecutive — escalates to Halt.
	text4 := assistantTextMsg("Error: 2026-03-23T12:00:00Z request aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee failed")
	r = a.Analyze(IterationState{IterationLog: text4, TaskKey: "test-task-2"})
	if r.Action != Halt || r.Reason != "repeated_error_recurring" {
		t.Errorf("fourth call: got %+v, want Halt/repeated_error_recurring", r)
	}
}

// Proves: different errors don't accumulate toward the repeated error
// threshold, even across many iterations.
func TestDifferentErrorsDoNotTriggerRepeatedErrorHalt(t *testing.T) {
	a := New()

	text1 := assistantTextMsg("Error: module 'foo' not found")
	text2 := assistantTextMsg("Error: syntax error in bar.js")
	text3 := assistantTextMsg("Error: timeout connecting to database")

	a.Analyze(IterationState{IterationLog: text1, TaskKey: "test-task-3"})
	a.Analyze(IterationState{IterationLog: text2, TaskKey: "test-task-3"})
	r := a.Analyze(IterationState{IterationLog: text3, TaskKey: "test-task-3"})

	if r.Action == Halt && r.Reason == "repeated_error" {
		t.Error("different errors should not trigger repeated_error halt")
	}
}

// Proves: clearing error hashes for a task resets the counter, so a new
// attempt starts fresh without inheriting previous error history.
func TestErrorHashesClearedOnTaskChange(t *testing.T) {
	a := New()
	log := assistantTextMsg("Error: cannot find module 'foo'")

	a.Analyze(IterationState{IterationLog: log, TaskKey: "task-a"})
	a.Analyze(IterationState{IterationLog: log, TaskKey: "task-a"})
	a.ClearErrorHashes("task-a")

	r := a.Analyze(IterationState{IterationLog: log, TaskKey: "task-a"})
	if r.Action == Halt && r.Reason == "repeated_error" {
		t.Error("error hashes should have been cleared, but repeated_error still triggered")
	}
}

// Proves: permission phrases in assistant text (agent prose about code) do NOT
// trigger halts — only tool_result content counts as real permission errors.
// Proves: an empty task key means error fingerprinting is skipped entirely,
// so repeated errors without a task context don't cause halts.
func TestNoTaskKeySkipsErrorFingerprinting(t *testing.T) {
	a := New()
	log := assistantTextMsg("Error: something broke")

	a.Analyze(IterationState{IterationLog: log, TaskKey: ""})
	a.Analyze(IterationState{IterationLog: log, TaskKey: ""})
	r := a.Analyze(IterationState{IterationLog: log, TaskKey: ""})

	if r.Action == Halt && r.Reason == "repeated_error" {
		t.Error("empty task key should skip error fingerprinting")
	}
}

// Proves: 3 consecutive test-only iterations across different task keys do NOT
// trigger test_saturation — the counter resets between tasks via ResetForNewTask.
func TestTestSaturationResetsAcrossTaskBoundary(t *testing.T) {
	a := New()
	testOnly := IterationState{
		IterationLog: "wrote some tests\n",
		HasDiff:      true,
		ChangedFiles: []string{"pkg/handler_test.go"},
	}

	a.ResetForNewTask()
	r := a.Analyze(IterationState{IterationLog: testOnly.IterationLog, HasDiff: true, ChangedFiles: testOnly.ChangedFiles, TaskKey: "task-1"})
	if r.Action == Halt {
		t.Fatal("task-1 iteration 1: should not halt")
	}

	a.ResetForNewTask()
	r = a.Analyze(IterationState{IterationLog: testOnly.IterationLog, HasDiff: true, ChangedFiles: testOnly.ChangedFiles, TaskKey: "task-2"})
	if r.Action == Halt {
		t.Fatal("task-2 iteration 1: should not halt after reset")
	}

	a.ResetForNewTask()
	r = a.Analyze(IterationState{IterationLog: testOnly.IterationLog, HasDiff: true, ChangedFiles: testOnly.ChangedFiles, TaskKey: "task-3"})
	if r.Action == Halt && r.Reason == "test_saturation" {
		t.Error("3 test-only iterations across 3 tasks must NOT trigger test_saturation after per-task reset")
	}
}

// Proves: 3 consecutive test-only iterations within the SAME task still
// trigger test_saturation — the counter only resets on task boundaries.
func TestTestSaturationStillFiresWithinSameTask(t *testing.T) {
	a := New()
	testOnly := IterationState{
		IterationLog: "wrote some tests\n",
		HasDiff:      true,
		ChangedFiles: []string{"pkg/handler_test.go"},
		TaskKey:      "same-task",
	}

	a.ResetForNewTask()
	for i := 0; i < 2; i++ {
		r := a.Analyze(testOnly)
		if r.Action == Halt {
			t.Fatalf("iteration %d within same task: should not halt yet", i+1)
		}
	}

	r := a.Analyze(testOnly)
	if r.Action != Halt || r.Reason != "test_saturation" {
		t.Errorf("3rd test-only iteration within same task: got %+v, want Halt/test_saturation", r)
	}
}

// Proves: after a Skip on iteration 1, a second iteration with the same task
// and same error returns Halt/repeated_error_recurring (not Skip again).
func TestRepeatedError_SecondConsecutiveHaltsWithRecurring(t *testing.T) {
	a := New()
	errorLog := assistantTextMsg("Error: cannot find module 'foo'")
	state := IterationState{IterationLog: errorLog, TaskKey: "task-consec"}

	// Build up 3 occurrences of the same error (threshold).
	a.Analyze(state)
	a.Analyze(state)

	// Third call: first detection → Skip.
	r := a.Analyze(state)
	if r.Action != Skip || r.Reason != "repeated_error" {
		t.Fatalf("third call: got %+v, want Skip/repeated_error", r)
	}

	// Fourth call: same task, second consecutive repeated_error → Halt.
	r = a.Analyze(state)
	if r.Action != Halt || r.Reason != "repeated_error_recurring" {
		t.Errorf("fourth call: got %+v, want Halt/repeated_error_recurring", r)
	}
}

// Proves: a clean iteration between two Skip-triggering iterations resets the
// consecutive counter, so the next error detection is Skip again, not Halt.
func TestRepeatedError_CleanIterationResetsConsecutiveCounter(t *testing.T) {
	a := New()
	errorLog := assistantTextMsg("Error: cannot find module 'foo'")
	cleanLog := assistantTextMsg("task completed successfully")
	state := IterationState{IterationLog: errorLog, TaskKey: "task-reset"}

	// Build to threshold and get first Skip.
	a.Analyze(state)
	a.Analyze(state)
	r := a.Analyze(state)
	if r.Action != Skip || r.Reason != "repeated_error" {
		t.Fatalf("pre-reset: got %+v, want Skip/repeated_error", r)
	}

	// A clean iteration (no error lines) resets the consecutive counter to 0.
	cleanState := IterationState{IterationLog: cleanLog, TaskKey: "task-reset", HasSignal: true}
	r = a.Analyze(cleanState)
	if r.Action == Halt {
		t.Fatalf("clean iteration: should not halt, got %+v", r)
	}

	// errorHashes still has count >= 3, so the very next error iteration triggers
	// repeated_error again. But because the consecutive counter was reset, it
	// returns Skip (repeatedErrorIterations=1), not Halt.
	r = a.Analyze(state)
	if r.Action != Skip || r.Reason != "repeated_error" {
		t.Errorf("first error after reset: got %+v, want Skip/repeated_error (not Halt)", r)
	}
}

func assistantTextMsg(text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, text)
}

func assistantToolUseMsg(name, command string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":%q,"input":{"command":%q}}]}}`, name, command)
}

func toolResultMsg(content string) string {
	return fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_read","content":%q}]}}`, content)
}

func bashToolUse(id, command string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q}}]}}`, id, command)
}

func bashResultMsg(id, content string) string {
	return fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`, id, content)
}
