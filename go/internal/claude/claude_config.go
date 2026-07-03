package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SignalPaths holds the file paths used for inter-process signaling between
// the ralph loop and the Claude process.
type SignalPaths struct {
	Complete     string // written when current task finishes
	CurrentTask  string // written when Claude picks up a task
	AllComplete  string // written when all tasks are done
	NoCodeNeeded string // written when investigation confirms no code changes are needed
}

// DefaultSignalPaths returns signal file paths under the given ralph dir.
func DefaultSignalPaths(ralphDir string) SignalPaths {
	return SignalPaths{
		Complete:     filepath.Join(ralphDir, ".signal_complete"),
		CurrentTask:  filepath.Join(ralphDir, ".signal_current_task"),
		AllComplete:  filepath.Join(ralphDir, ".signal_all_complete"),
		NoCodeNeeded: filepath.Join(ralphDir, ".signal_no_code_needed"),
	}
}

// Timeouts is the single source of truth for the timing policy applied to an
// agent run. The same struct governs the main implementation agent and the
// verifier's fix agent — there is no per-role divergence; callers pass one
// shared value to both. Role-specific differences (whether a completion signal
// is verified, where output is logged) live on RunConfig, not here.
type Timeouts struct {
	// Idle kills the session if the raw log file hasn't been modified for this
	// duration. Zero disables idle detection.
	Idle time.Duration

	// IdleProgress is the shorter idle timeout used once the agent has produced
	// content output (text, thinking, tool use) in the raw log. Zero falls back
	// to Idle.
	IdleProgress time.Duration

	// MaxRun is the hard wall-clock cap on total agent run time, fired
	// regardless of log activity. Zero disables the wall-clock backstop.
	MaxRun time.Duration

	// Heartbeat emits a liveness line when the agent produces no visible output
	// for this duration. Zero disables.
	Heartbeat time.Duration
}

// RunConfig configures a single Claude invocation.
type RunConfig struct {
	Ctx          context.Context // cancelled on SIGINT to stop the run
	WorkDir      string
	RalphDir     string
	Prompt       string
	RawLog       string // path to raw JSON log file
	LogFile      string // path to human-readable log file
	Signals      SignalPaths
	PollInterval time.Duration

	// Timeouts is the shared idle/progress/wall-clock/heartbeat policy.
	Timeouts Timeouts

	// OnSignal is called when the agent writes a completion signal.
	// If it returns true, the signal is accepted (agent killed, result returned).
	// If false, the signal is rejected (signal file cleared, agent continues).
	// Used by the orchestrator to verify tests before accepting completion.
	// If nil, signals are always accepted (legacy behavior).
	OnSignal func(summary string) bool

	// FeedbackFile is the path where the orchestrator writes feedback for
	// the agent to read. Used to send test failure output back to the agent.
	FeedbackFile string

	// Verbose shows all tool calls in the stream log. When false,
	// low-value tools (Read, Bash, etc.) are hidden.
	Verbose bool

	// Model is the Claude model to use (e.g. "claude-sonnet-4-5-20241022").
	// If empty, the CLI default is used.
	Model string

	// TaskID is the bead/task identifier (e.g. "ralph-xyz") used in
	// completion and status log messages.
	TaskID string
}

// Result describes the outcome of a Claude run.
type Result struct {
	SignalDetected     bool      // true if a completion signal was found
	AllComplete        bool      // true if the all-complete signal was found
	NoCodeNeeded       bool      // true if agent confirmed no code changes required (already fixed / not a bug)
	Compacted          bool      // true if the agent was killed because it triggered context compaction, or exceeded the 200K context limit ("Prompt is too long") with auto-compaction disabled
	IdleTimeout        bool      // true if the session was killed due to idle timeout
	WallClockTimeout   bool      // true if the session was killed due to wall-clock max-run-duration
	FeedbackKill       bool      // true if killed because user feedback arrived
	RateLimited        bool      // true if Claude reported hitting its usage limit
	ResetAt            time.Time // when the rate limit resets (valid when RateLimited is true)
	OnSignalUsed       bool      // true if OnSignal callback was used for verification
	VerificationFailed bool      // true if signal was detected but OnSignal rejected it
	VerificationReason string    // reason OnSignal rejected the signal
	TaskDesc           string    // task description from the current-task signal
	Summary            string    // completion summary from signal file
}

// OnTaskDetected is called when a current-task signal file appears.
// The callback receives the task description read from the signal file.
type OnTaskDetected func(taskDesc string)

// IterationAllowedTools lists the Claude Code tools pre-approved for
// iteration mode. This replaces --dangerously-skip-permissions with
// explicit scoping — only these tools are available, and --add-dir
// restricts which directories they can access.
var IterationAllowedTools = []string{
	"Bash(*)",
	"Read",
	"Edit",
	"Write",
	"Glob",
	"Grep",
	"Agent",
	"Skill",
	"TodoWrite",
	"NotebookEdit",
	"WebFetch",
	"WebSearch",
	"ToolSearch",
}

// IterationDisallowedTools lists tools the agent must not use.
// The orchestrator owns all bd and gh operations — the agent must never
// interact with beads or GitHub directly. Git checkout/branch/push are
// blocked so sub-agents can't interfere with ralph's branch management.
//
// Patterns use leading wildcards (e.g. "Bash(*gh *)") to match commands
// that chain via && or ; after a cd prefix.
var IterationDisallowedTools = []string{
	"Bash(bd *)",
	"Bash(gh *)",
	"Bash(git checkout*)",
	"Bash(git branch*)",
	"Bash(git push*)",
	"Bash(*bd *)",
	"Bash(*gh *)",
	"Bash(*git checkout*)",
	"Bash(*git branch*)",
	"Bash(*git push*)",
	"Bash(rm *.beads*)",
	"Bash(rm *.ralph*)",
	"Bash(*rm *.beads*)",
	"Bash(*rm *.ralph*)",
	"Bash(pkill*dolt*)",
	"Bash(kill*dolt*)",
	"Bash(*pkill*dolt*)",
	"Bash(*kill*dolt*)",

	// Block direct SQL clients that could bypass bd and hit the dolt sql-server
	// via the MySQL protocol on 127.0.0.1. Both bare invocations and those
	// chained after a cd prefix (e.g. "cd /tmp && mysql ...").
	"Bash(mysql *)",
	"Bash(*mysql *)",
	"Bash(sqlite3 *)",
	"Bash(*sqlite3 *)",
	"Bash(psql *)",
	"Bash(*psql *)",

	// Block shell reads of raw .beads/ files (JSONL bead data). Agents must use
	// bd commands exclusively — direct file reads expose internal schema fields
	// the agent should never see.
	"Bash(cat *.beads*)",
	"Bash(*cat *.beads*)",
	"Bash(sed *.beads*)",
	"Bash(*sed *.beads*)",
	"Bash(awk *.beads*)",
	"Bash(*awk *.beads*)",
	"Bash(less *.beads*)",
	"Bash(*less *.beads*)",

	// Block Read-tool access to .beads/ paths. Requires claude CLI to support
	// Read path patterns in --disallowedTools; if not, the agent prompt in
	// execution-bd.md also explicitly forbids .beads/ reads as a fallback.
	"Read(*.beads*)",

	// Block absolute-path cd escapes. The agent's CWD is set to the worktree;
	// cd /abs/path would change it to the project root or another directory,
	// causing tools like tsx to pick up the wrong tsconfig and compile with the
	// wrong JSX factory. Relative cd (cd subdir) remains allowed.
	// The leading-wildcard form catches cd chained after another command prefix.
	"Bash(cd /Users/*)",
	"Bash(*cd /Users/*)",
	"Bash(cd /home/*)",
	"Bash(*cd /home/*)",
	"Bash(cd /tmp/*)",
	"Bash(*cd /tmp/*)",

	// Block orchestrator-owned ralph:* npm scripts. The loop manages the
	// ralph:verify and ralph:post-task lifecycle; agents must only run 'npm test'.
	// The leading-wildcard form catches invocations chained after a prefix.
	"Bash(npm run ralph:*)",
	"Bash(*npm run ralph:*)",

	// claude-mem MCP tools — inherited from the host session but the agent
	// wastes iterations on memory retrieval loops instead of writing code.
	"mcp__plugin_claude-mem_mcp-search__*",

	// claude-mem skills via the Skill tool — the Skill tool can invoke any
	// registered slash command. Blocking at the Skill layer prevents the agent
	// from loading skill instructions that then attempt the blocked MCP tools,
	// wasting the iteration on a doomed memory-retrieval loop.
	"Skill(claude-mem:*)",
}

// CmdFactory builds the exec.Cmd that Run() will start. Receives the
// RunConfig so it can set WorkDir and pipe stdout/stderr to the raw log.
// If nil, Run() spawns "claude" with the standard iteration flags.
type CmdFactory func(cfg RunConfig, rawLog *os.File) *exec.Cmd

// buildAgentEnv returns a hermetic environment for the agent subprocess.
// It always disables Claude's machine-local auto-memory so code agents run on
// the orchestrator's prompt only (context reproducible across machines), and
// prepends the project's .venv/bin to PATH when that directory exists in workDir.
//
// DISABLE_AUTO_COMPACT=1 disables Claude Code's auto-compaction so the agent
// uses the full context window instead of being silently summarized around
// 166K (83% of 200K) — a lossy compaction that ralph previously misread as a
// context leak. With auto-compaction off, a genuinely oversized task fails
// cleanly with "Prompt is too long" at the 200K hard limit instead.
func buildAgentEnv(workDir string) []string {
	env := append(os.Environ(), "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTO_COMPACT=1")
	venvBin := filepath.Join(workDir, ".venv", "bin")
	if info, err := os.Stat(venvBin); err != nil || !info.IsDir() {
		return env
	}
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + venvBin + string(filepath.ListSeparator) + strings.TrimPrefix(entry, "PATH=")
			return env
		}
	}
	env = append(env, "PATH="+venvBin)
	return env
}
