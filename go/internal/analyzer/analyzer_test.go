package analyzer

import (
	"fmt"
	"strings"
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

// Verifies that 3+ permission-denial phrases in assistant messages cause an
// immediate halt, protecting the loop from burning iterations against a
// sandbox or filesystem restriction.
func TestPermissionDenialHaltsAt3(t *testing.T) {
	a := New()
	log := assistantTextMsg("Error: permission denied. Failed: cannot write to /foo. blocked by sandbox rule.")
	r := a.Analyze(IterationState{IterationLog: log})
	if r.Action != Halt || r.Reason != "permission_denied" {
		t.Errorf("3 permission lines: got %+v, want Halt/permission_denied", r)
	}
}

// Verifies that fewer than 3 permission phrases do NOT halt — isolated
// permission errors may be transient and shouldn't abort the whole loop.
func TestPermissionDenialBelowThreshold(t *testing.T) {
	a := New()
	log := assistantTextMsg("permission denied. cannot write.")
	r := a.Analyze(IterationState{IterationLog: log})
	if r.Action == Halt && r.Reason == "permission_denied" {
		t.Errorf("2 permission lines should not halt, got %+v", r)
	}
}

// Verifies that permission phrases inside tool results (file contents, command
// output) do NOT trigger the permission denial detector. This prevents false
// positives when Claude reads/writes code containing these phrases as data.
func TestPermissionDenialIgnoresToolResults(t *testing.T) {
	a := New()
	log := toolResultMsg(`PERMISSION_DENIAL_THRESHOLD=3\npermission denied error handling\ncannot write guard\nblocked by sandbox check\nnot allowed to delete`) +
		"\n" + assistantTextMsg("I've updated the permission checks.")
	r := a.Analyze(IterationState{IterationLog: log})
	if r.Action == Halt && r.Reason == "permission_denied" {
		t.Errorf("permission phrases in tool results should not halt, got %+v", r)
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

// Verifies that 5+ identical tool calls in a single iteration are detected
// as a stuck loop, catching agents that retry the same failing command.
func TestRepeatedToolCallsDetectedAsStuck(t *testing.T) {
	a := New()
	log := assistantToolUseMsg("Bash", "/usr/bin/test") + "\n" +
		assistantToolUseMsg("Bash", "/usr/bin/test") + "\n" +
		assistantToolUseMsg("Bash", "/usr/bin/test") + "\n" +
		assistantToolUseMsg("Bash", "/usr/bin/test") + "\n" +
		assistantToolUseMsg("Bash", "/usr/bin/test") + "\n"

	r := a.Analyze(IterationState{IterationLog: log})
	if r.Action != Warn || r.Reason != "stuck_indicators_detected" {
		t.Errorf("repeated tool calls: got %+v, want Warn/stuck_indicators_detected", r)
	}
}

// Verifies that 3 consecutive iterations with no git changes and no signal
// trigger a stagnation halt, preventing the loop from idling indefinitely.
func TestStagnationHaltsAfterThree(t *testing.T) {
	a := New()
	noChange := IterationState{IterationLog: "thinking about things\n"}

	for i := 0; i < 2; i++ {
		r := a.Analyze(noChange)
		if r.Action != Continue {
			t.Fatalf("stagnation iteration %d: got %+v, want Continue", i+1, r)
		}
	}

	r := a.Analyze(noChange)
	if r.Action != Halt || r.Reason != "stagnation" {
		t.Errorf("3rd stagnant iteration: got %+v, want Halt/stagnation", r)
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

// Verifies that permission denial detection is case-insensitive,
// matching ralph.sh's grep -i flag.
func TestPermissionDenialCaseInsensitive(t *testing.T) {
	a := New()
	log := assistantTextMsg("PERMISSION DENIED. Cannot Write to disk. BLOCKED BY SANDBOX.")
	r := a.Analyze(IterationState{IterationLog: log})
	if r.Action != Halt || r.Reason != "permission_denied" {
		t.Errorf("case-insensitive permission: got %+v, want Halt/permission_denied", r)
	}
}

// Verifies that permission denial detail is capped at 5 lines,
// matching ralph.sh's `head -5` on the matches.
func TestPermissionDenialDetailCapped(t *testing.T) {
	a := New()
	phrases := make([]string, 10)
	for i := range phrases {
		phrases[i] = "permission denied"
	}
	log := assistantTextMsg(strings.Join(phrases, ". "))

	r := a.Analyze(IterationState{IterationLog: log})
	if r.Action != Halt {
		t.Fatalf("expected halt, got %+v", r)
	}
	detailLines := strings.Split(r.Detail, "\n")
	if len(detailLines) > 5 {
		t.Errorf("detail has %d lines, want <= 5", len(detailLines))
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

// Proves: ralph detects repeated identical errors across iterations and
// halts after 3 occurrences of the same error for the same task key.
func TestRepeatedErrorFingerprintTriggersHalt(t *testing.T) {
	a := New()
	log := assistantTextMsg("Error: cannot find module 'foo'\nsome other output")

	state := IterationState{IterationLog: log, TaskKey: "test-task-1"}

	r := a.Analyze(state)
	if r.Action == Halt && r.Reason == "repeated_error" {
		t.Fatal("first call should not trigger repeated_error")
	}

	r = a.Analyze(state)
	if r.Action == Halt && r.Reason == "repeated_error" {
		t.Fatal("second call should not trigger repeated_error")
	}

	r = a.Analyze(state)
	if r.Action != Halt || r.Reason != "repeated_error" {
		t.Errorf("third call: got %+v, want Halt/repeated_error", r)
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
	r := a.Analyze(IterationState{IterationLog: text3, TaskKey: "test-task-2"})

	if r.Action != Halt || r.Reason != "repeated_error" {
		t.Errorf("normalized errors: got %+v, want Halt/repeated_error", r)
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

func assistantTextMsg(text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, text)
}

func assistantToolUseMsg(name, command string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":%q,"input":{"command":%q}}]}}`, name, command)
}

func toolResultMsg(content string) string {
	return fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","content":%q}]}}`, content)
}
