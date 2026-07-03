package analyzer

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Result represents the outcome of analyzing a single iteration.
type Result struct {
	Action Action
	Reason string
	Detail string
}

// Action is the type of response from the analyzer.
type Action int

const (
	Continue Action = iota
	Warn
	Halt
	Skip // first repeated_error in an iteration; escalates to Halt on second consecutive
)

// IterationState captures the git/signal state observed after an iteration,
// provided by the caller (the execution loop).
type IterationState struct {
	HasDiff       bool     // unstaged or staged changes in the worktree
	NewCommits    bool     // HEAD moved since iteration start
	HasSignal     bool     // task-complete or all-complete signal detected
	ChangedFiles  []string // paths of all changed files (diff + commits combined)
	IterationLog  string   // raw log output from this iteration
	TaskKey       string   // task identifier for error fingerprinting (empty = skip)
}

// Analyzer tracks multi-iteration patterns and decides whether to continue,
// warn, or halt the execution loop. Matches ralph.sh's analyze_iteration.
type Analyzer struct {
	stagnantCount            int
	testOnlyCount            int
	stuckCount               int
	errorHashes              map[string]map[string]int // task_key → error_hash → count
	repeatedErrorIterations  map[string]int            // task_key → consecutive repeated_error iteration count
}

// New creates an Analyzer with zeroed counters, matching ralph.sh's
// counter reset at the start of each execution phase.
func New() *Analyzer {
	return &Analyzer{}
}

var stuckPhraseRe = regexp.MustCompile(`(?i)(I'm blocked|I cannot proceed|unable to complete)`)

// Analyze inspects an iteration's output and state, updating internal counters
// and returning a Result. The caller provides IterationState from git and
// signal checks; the analyzer handles log content analysis and counter logic.
func (a *Analyzer) Analyze(state IterationState) Result {
	if state.IterationLog == "" {
		return Result{Action: Continue}
	}

	parsed := parseLog(state.IterationLog)

	// --- Stuck loop: skip if task completed via signal ---
	if state.HasSignal {
		a.stuckCount = 0
	}

	stuckDetected := false
	if !state.HasSignal {
		// Only check for explicit stuck phrases — repeated tool call counting
		// removed as it produces false positives during normal development.
		stuckDetected = stuckPhraseRe.MatchString(parsed.AssistantText)
	}

	if stuckDetected {
		a.stuckCount++
		if a.stuckCount >= 2 {
			return Result{Action: Halt, Reason: "stuck_loop"}
		}
		return Result{Action: Warn, Reason: "stuck_indicators_detected"}
	}
	a.stuckCount = 0

	// --- Repeated error detection: same error fingerprint 3x in one iteration ---
	// First detection → Skip (give the agent one chance to recover after task skip).
	// Second consecutive iteration with the same task triggering repeated_error → Halt.
	if state.TaskKey != "" {
		if a.checkRepeatedErrors(parsed.AssistantText, state.TaskKey) {
			if a.repeatedErrorIterations == nil {
				a.repeatedErrorIterations = make(map[string]int)
			}
			a.repeatedErrorIterations[state.TaskKey]++
			if a.repeatedErrorIterations[state.TaskKey] >= 2 {
				return Result{Action: Halt, Reason: "repeated_error_recurring",
					Detail: fmt.Sprintf("consecutive iteration %d", a.repeatedErrorIterations[state.TaskKey])}
			}
			return Result{Action: Skip, Reason: "repeated_error"}
		}
		if a.repeatedErrorIterations != nil {
			a.repeatedErrorIterations[state.TaskKey] = 0
		}
	}

	// --- Stagnation: 3 consecutive no-change iterations → skip this task ---
	// A single task idling does not mean the whole loop is stuck — other
	// ready tasks may make progress fine, so this only skips the task
	// (routing through Loop's skip path) rather than halting everything.
	hasChanges := state.HasDiff || state.NewCommits
	if !hasChanges && !state.HasSignal && !state.NewCommits {
		a.stagnantCount++
		if a.stagnantCount >= 3 {
			return Result{Action: Skip, Reason: "stagnation"}
		}
		return Result{Action: Continue}
	}
	a.stagnantCount = 0

	// --- Test saturation: 3 consecutive test-only change iterations → halt ---
	if hasChanges && len(state.ChangedFiles) > 0 {
		allTestFiles := true
		for _, f := range state.ChangedFiles {
			if !isTestFile(f) {
				allTestFiles = false
				break
			}
		}
		if allTestFiles {
			a.testOnlyCount++
			if a.testOnlyCount >= 3 {
				return Result{Action: Halt, Reason: "test_saturation"}
			}
		} else {
			a.testOnlyCount = 0
		}
	}

	return Result{Action: Continue}
}

var testFileBaseRe = regexp.MustCompile(`(?i)(test|spec|_test\.|test_)`)
var testDirRe = regexp.MustCompile(`(?i)(tests?|specs?|__tests__)$`)

// isTestFile returns true if a file path looks like a test file, matching
// ralph.sh's basename and top-directory checks.
func isTestFile(path string) bool {
	if path == "" {
		return false
	}

	// Check basename
	parts := strings.Split(path, "/")
	base := parts[len(parts)-1]
	if testFileBaseRe.MatchString(base) {
		return true
	}

	// Check top-level directory
	if len(parts) > 1 && testDirRe.MatchString(parts[0]) {
		return true
	}

	return false
}

// parsedLog holds the extracted content from a JSON-lines iteration log,
// split by source so detectors can target the right signal.
type parsedLog struct {
	AssistantText string // text and thinking blocks from assistant messages
}

// parseLog walks the JSON-lines log once, extracting assistant text and
// thinking blocks. All text-based detectors should run against
// AssistantText to avoid false positives from file contents in tool results.
func parseLog(log string) parsedLog {
	var text strings.Builder

	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}

		var blocks []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		}
		if json.Unmarshal(msg.Message.Content, &blocks) != nil {
			continue
		}

		switch msg.Type {
		case "assistant":
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						text.WriteString(b.Text)
						text.WriteByte('\n')
					}
				case "thinking":
					if b.Thinking != "" {
						text.WriteString(b.Thinking)
						text.WriteByte('\n')
					}
				}
			}
		}
	}
	return parsedLog{
		AssistantText: text.String(),
	}
}

// --- Error fingerprinting ---

var errorLineRe = regexp.MustCompile(`(?im)(^(Error|Failed|Exception|panic|FATAL|TypeError|SyntaxError|ReferenceError|RuntimeError|ImportError|ValueError):.*|exited with (code|status) [1-9].*|non-zero exit code.*|command failed.*|build failed.*|compilation failed.*|test failed.*)`)

var (
	timestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[^ ]*`)
	uuidRe      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	lineNumRe   = regexp.MustCompile(`\bline \d+`)
	colNumRe    = regexp.MustCompile(`:\d+:\d+`)
	tmpPathRe   = regexp.MustCompile(`/tmp/[^ ]*`)
	varFolderRe = regexp.MustCompile(`/var/folders/[^ ]*`)
	multiSpace  = regexp.MustCompile(`\s+`)
)

func extractErrors(text string) []string {
	return errorLineRe.FindAllString(text, -1)
}

func normalizeError(line string) string {
	line = timestampRe.ReplaceAllString(line, "TIMESTAMP")
	line = uuidRe.ReplaceAllString(line, "UUID")
	line = lineNumRe.ReplaceAllString(line, "line N")
	line = colNumRe.ReplaceAllString(line, ":N:N")
	line = tmpPathRe.ReplaceAllString(line, "/tmp/TMPPATH")
	line = varFolderRe.ReplaceAllString(line, "/tmp/TMPPATH")
	line = multiSpace.ReplaceAllString(line, " ")
	return strings.TrimSpace(line)
}

func fingerprintError(normalized string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(normalized)))
}

func (a *Analyzer) checkRepeatedErrors(text, taskKey string) bool {
	if taskKey == "" {
		return false
	}
	errors := extractErrors(text)
	if len(errors) == 0 {
		return false
	}
	if a.errorHashes == nil {
		a.errorHashes = make(map[string]map[string]int)
	}
	if a.errorHashes[taskKey] == nil {
		a.errorHashes[taskKey] = make(map[string]int)
	}
	for _, errLine := range errors {
		normalized := normalizeError(errLine)
		hash := fingerprintError(normalized)
		a.errorHashes[taskKey][hash]++
		if a.errorHashes[taskKey][hash] >= 3 {
			return true
		}
	}
	return false
}

// ResetForNewTask zeros the per-task iteration counters when the loop
// transitions to a new task. errorHashes is preserved — it is keyed per task
// and accumulates across sessions intentionally.
func (a *Analyzer) ResetForNewTask() {
	a.testOnlyCount = 0
	a.stagnantCount = 0
	a.stuckCount = 0
	a.repeatedErrorIterations = nil
}

// ClearErrorHashes removes all recorded error hashes for a given task key,
// resetting the repeated-error counter for that task.
func (a *Analyzer) ClearErrorHashes(taskKey string) {
	if a.errorHashes != nil {
		delete(a.errorHashes, taskKey)
	}
}
