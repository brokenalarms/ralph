package analyzer

import (
	"encoding/json"
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
)

// IterationState captures the git/signal state observed after an iteration,
// provided by the caller (the execution loop).
type IterationState struct {
	HasDiff       bool     // unstaged or staged changes in the worktree
	NewCommits    bool     // HEAD moved since iteration start
	HasSignal     bool     // task-complete or all-complete signal detected
	ChangedFiles  []string // paths of all changed files (diff + commits combined)
	IterationLog  string   // raw log output from this iteration
}

// Analyzer tracks multi-iteration patterns and decides whether to continue,
// warn, or halt the execution loop. Matches ralph.sh's analyze_iteration.
type Analyzer struct {
	stagnantCount int
	testOnlyCount int
	stuckCount    int
}

// New creates an Analyzer with zeroed counters, matching ralph.sh's
// counter reset at the start of each execution phase.
func New() *Analyzer {
	return &Analyzer{}
}

var permissionRe = regexp.MustCompile(`(?i)(permission denied|cannot write|blocked by sandbox|not allowed)`)
var stuckPhraseRe = regexp.MustCompile(`(?i)(I'm blocked|I cannot proceed|unable to complete)`)

// Analyze inspects an iteration's output and state, updating internal counters
// and returning a Result. The caller provides IterationState from git and
// signal checks; the analyzer handles log content analysis and counter logic.
func (a *Analyzer) Analyze(state IterationState) Result {
	if state.IterationLog == "" {
		return Result{Action: Continue}
	}

	// Parse the JSON-lines log once, extracting assistant text and tool call
	// signatures. All text-based detectors run against assistantText to avoid
	// false positives from file contents in tool results.
	parsed := parseLog(state.IterationLog)

	// --- Permission denial: 3+ matches in assistant messages only → halt ---
	permMatches := permissionRe.FindAllString(parsed.AssistantText, -1)
	if len(permMatches) >= 3 {
		return Result{
			Action: Halt,
			Reason: "permission_denied",
			Detail: strings.Join(firstN(permMatches, 5), "\n"),
		}
	}

	// --- Stuck loop: explicit phrases or repeated tool calls ---
	stuckDetected := stuckPhraseRe.MatchString(parsed.AssistantText)

	if !stuckDetected {
		maxRepeats := maxToolCallRepeats(parsed.ToolCalls)
		if maxRepeats >= 3 {
			stuckDetected = true
		}
	}

	if stuckDetected {
		a.stuckCount++
		if a.stuckCount >= 2 {
			return Result{Action: Halt, Reason: "stuck_loop"}
		}
		return Result{Action: Warn, Reason: "stuck_indicators_detected"}
	}
	a.stuckCount = 0

	// --- Stagnation: 3 consecutive no-change iterations → halt ---
	hasChanges := state.HasDiff || state.NewCommits
	if !hasChanges && !state.HasSignal && !state.NewCommits {
		a.stagnantCount++
		if a.stagnantCount >= 3 {
			return Result{Action: Halt, Reason: "stagnation"}
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

// maxToolCallRepeats counts the most-repeated tool call signature,
// matching ralph.sh's grep + sort + uniq -c approach.
func maxToolCallRepeats(toolCalls []string) int {
	if len(toolCalls) == 0 {
		return 0
	}
	counts := make(map[string]int)
	for _, key := range toolCalls {
		counts[key]++
	}
	max := 0
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	return max
}

var testFileBaseRe = regexp.MustCompile(`(?i)(test|spec|_test\.|test_)`)
var testDirRe = regexp.MustCompile(`(?i)^(tests?|specs?|__tests__)(/|$)`)

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
	AssistantText string   // text and thinking blocks from assistant messages
	ToolCalls     []string // "toolName:target" signatures from tool_use blocks
}

// parseLog walks the JSON-lines log once, extracting assistant text/thinking
// and tool call signatures. All text-based detectors should run against
// AssistantText to avoid false positives from file contents in tool results.
func parseLog(log string) parsedLog {
	var text strings.Builder
	var calls []string

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
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Type != "assistant" {
			continue
		}
		var blocks []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		}
		if json.Unmarshal(msg.Message.Content, &blocks) != nil {
			continue
		}
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
			case "tool_use":
				target := extractToolTarget(b.Input)
				calls = append(calls, b.Name+":"+target)
			}
		}
	}
	return parsedLog{AssistantText: text.String(), ToolCalls: calls}
}

// extractToolTarget pulls the most identifying field from a tool_use input.
func extractToolTarget(raw json.RawMessage) string {
	var input struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Pattern  string `json:"pattern"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return ""
	}
	if input.Command != "" {
		return input.Command
	}
	if input.FilePath != "" {
		return input.FilePath
	}
	return input.Pattern
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
