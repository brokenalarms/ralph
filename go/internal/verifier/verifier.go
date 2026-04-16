// Package verifier provides the post-signal verification operations used by
// the execution loop. It is a peer module of internal/loop: constructed once
// in cmd/ralph/main.go, passed into loop.Modules, and called by the loop via
// its stateless public API.
//
// Verifier exposes individual operations (run tests, compile check, LLM
// verify, spawn fix-agent subprocesses) rather than an all-in-one OnSignal
// entry point. The loop orchestrates these operations: it fetches fresh
// HEAD/diff from its own git module between calls, tracks retry counters as
// local variables, and writes state-store side effects. Verifier never holds
// or reaches into git / state / tasks modules.
//
// Verifier owns two submodules at construction time:
//
//   - newRunner: a factory that produces fix-agent subprocess runners. In
//     production it returns *agent.Runner; in tests it returns a stub.
//   - querier: the one-shot LLM query function used for LLM verification. In
//     production it shells out to `claude --print`; in tests it returns
//     canned YES/NO responses.
//
// Both submodules are explicit constructor parameters (not Config function
// fields). See docs/specs/orchestrator-modules.md for the peer-module rule.
package verifier

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/verify"
)

// HeartbeatInterval is how often a heartbeat line is emitted while tests run.
// Exported so tests can override it.
var HeartbeatInterval = 30 * time.Second

// Runner is the minimal subprocess interface verifier uses for fix agents.
// Verifier only calls Run on fix-agent runners — it never calls StopStreaming
// or InjectMessage on them, so this is narrower than the loop's claudeRunner
// interface. The default factory returns a *agent.Runner.
type Runner interface {
	Run(cfg claude.RunConfig) (claude.Result, error)
}

// Config holds the verifier's configuration as pure data. No function
// fields, no module references. Per-call data (diffs, task metadata, etc.)
// flows in through method arguments, not through Config.
type Config struct {
	VerifyDir             string
	ProjectDir            string // project root used as fallback when ralph:verify is absent from VerifyDir
	VerifyModel           string
	VerifyEscalationModel string
	FixModel              string // model for fix agents on attempt 1; defaults to ModelSonnet
	FixEscalationModel    string // model for fix agents on attempt 2+; defaults to ModelOpus
	ModelCap              string // maximum model tier ceiling from --model flag; empty means no cap
	PromptsDir            string
	RalphDir              string
	IdleTimeout           time.Duration
	TestTimeout           time.Duration
	CompileCheckTimeout   time.Duration

	// Signals is the set of paths used by fix-agent subprocesses to signal
	// completion. It is data, not a module reference.
	Signals claude.SignalPaths
}

// RunnerFactory produces a fresh fix-agent runner on each call. Verifier
// owns one of these as a submodule — the fix-agent runner is intrinsic to
// what verifier does (same relationship git has to github).
type RunnerFactory func() Runner

// Querier is verifier's local interface for one-shot LLM queries. Defined
// here (not imported from elsewhere) so verifier holds no peer-module
// references — same dependency-inversion pattern git uses with StateStore.
// In production *agent.Runner satisfies this interface via its Query
// method; tests provide stubs that return canned YES/NO responses.
type Querier interface {
	Query(ctx context.Context, workDir, prompt, model string, allowedTools []string) (string, error)
}

// Verifier owns the individual verification operations. It holds its
// configuration, a logger, and two submodules: a fix-agent runner factory
// and an LLM querier. It holds no peer-module references and no function
// hooks for behavior injection — the submodules are explicit constructor
// parameters. Verifier never reaches into git, state, or taskBackend.
type Verifier struct {
	cfg       Config
	logger    *logging.Logger
	newRunner RunnerFactory
	querier   Querier
}

// New creates a Verifier from a pure-data Config, a logger, an explicit
// fix-agent runner factory, and an explicit LLM querier. When newRunner is
// nil it defaults to agent.New(logger). When querier is nil it also
// defaults to agent.New(logger), which satisfies the Querier interface via
// its Query method. Tests pass their own stubs — these are the ONLY
// behavioral injection points, and they are explicit constructor
// parameters, not Config fields.
func New(cfg Config, logger *logging.Logger, newRunner RunnerFactory, querier Querier) *Verifier {
	if newRunner == nil {
		newRunner = func() Runner { return agent.New(logger) }
	}
	if querier == nil {
		querier = agent.New(logger)
	}
	if cfg.TestTimeout == 0 {
		cfg.TestTimeout = 5 * time.Minute
	}
	if cfg.CompileCheckTimeout == 0 {
		cfg.CompileCheckTimeout = 60 * time.Second
	}
	return &Verifier{cfg: cfg, logger: logger, newRunner: newRunner, querier: querier}
}

// RunTests runs the ralph:verify test suite in dir with a heartbeat log line
// every HeartbeatInterval. Verifier owns the start/result narrative for its
// own operation; callers (Loop) only log orchestration concerns like retry
// counters. Returns the test result and elapsed duration.
func (v *Verifier) RunTests(ctx context.Context, dir string) (verify.Result, time.Duration) {
	v.logger.Emit(logging.Opts{Domain: logging.Test}, "Running test suite...")

	start := time.Now()
	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				v.logger.Emit(logging.Opts{Domain: logging.Test},
					"Tests still running... (%s elapsed)", time.Since(start).Truncate(time.Millisecond))
			}
		}
	}()

	result := verify.RunTests(ctx, v.cfg.TestTimeout, dir, v.cfg.ProjectDir)
	elapsed := time.Since(start).Truncate(time.Millisecond)

	switch {
	case result.ScriptMissing:
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "ralph:verify script not found — cannot verify")
	case result.Passed:
		v.logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed (%s)", elapsed)
	default:
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Tests failed (%s): %s", elapsed, result.Reason)
	}

	return result, elapsed
}

// CompileCheck runs the build/type check (go build / tsc --noEmit) in dir.
// Verifier owns its own start/result narrative; callers only log
// orchestration concerns.
func (v *Verifier) CompileCheck(ctx context.Context, dir string) verify.Result {
	v.logger.Emit(logging.Opts{Domain: logging.Build}, "Running compile check...")
	result := verify.CompileCheck(ctx, v.cfg.CompileCheckTimeout, dir)
	if result.Passed {
		v.logger.Emit(logging.Opts{Domain: logging.Build}, "Compile check passed")
	} else {
		v.logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Warn}, "Compile check failed: %s", result.Reason)
	}
	return result
}

// LLMVerifyOpts carries per-call inputs to LLMVerify. Pure data — no
// callbacks, no module references. The diff is pre-fetched by Loop and
// passed in as a string.
type LLMVerifyOpts struct {
	Ctx         context.Context
	WorkDir     string
	TaskID      string
	Title       string
	Description string
	Acceptance  string
	Diff        string
	DiffSource  string
	// Attempt is the 1-indexed attempt number within a single verification
	// flow; attempt 1 uses the base verify model, subsequent attempts
	// escalate to the escalation model.
	Attempt int
	// NoCodeNeeded is true when the agent claimed no code changes are
	// required. When set with an empty Diff, the verifier spawns a
	// tool-using query to read the codebase and confirm the acceptance
	// criteria are already met, instead of auto-passing.
	NoCodeNeeded bool
	// AgentSummary is the agent's explanation of why no code is needed.
	AgentSummary string
}

// LLMVerify runs LLM verification with the given opts. It selects the
// appropriate model, builds the prompt via verify.BuildReviewPrompt (pure
// helper), calls v.querier (verifier's submodule) to actually run the LLM,
// then parses the response via verify.ParseReviewResponse (pure helper).
// Verifier owns the start/result narrative — callers (Loop) only log
// orchestration concerns like retry counters. The returned Result reports
// whether the LLM approved the diff. The selected model is returned
// alongside so callers can include it in orchestration logs.
//
// An empty Diff short-circuits to NoDiff: the agent confirmed completion
// but no PR/diff exists to verify. An LLM call error is treated as a
// pass-through (verification skipped) so transient LLM failures do not
// block task completion.
func (v *Verifier) LLMVerify(opts LLMVerifyOpts) (verify.Result, string) {
	model := v.verifyModel(opts.Attempt)

	if opts.Diff == "" && !opts.NoCodeNeeded {
		return verify.Result{Passed: true, NoDiff: true, Reason: "no PR found and no new commits — agent confirms task complete"}, model
	}

	var prompt string
	var allowedTools []string
	if opts.NoCodeNeeded {
		// Agent claimed no code changes needed. Spawn a tool-using
		// verifier that can read the codebase to confirm the acceptance
		// criteria are already met.
		prompt = v.loadVerifyPrompt("verify-no-code-needed.md", map[string]string{
			"{{TASK_TITLE}}":          opts.Title,
			"{{TASK_DESCRIPTION}}":    opts.Description,
			"{{ACCEPTANCE_CRITERIA}}": opts.Acceptance,
			"{{AGENT_SUMMARY}}":       opts.AgentSummary,
		})
		allowedTools = []string{"Read", "Grep", "Glob"}
		v.logger.Emit(logging.Opts{Domain: logging.LLM, Model: model}, "Verifying no-code-needed claim...")
	} else {
		prompt = verify.BuildReviewPrompt(verify.ReviewPromptInput{
			PromptsDir:  v.cfg.PromptsDir,
			Title:       opts.Title,
			Description: opts.Description,
			Acceptance:  opts.Acceptance,
			Diff:        opts.Diff,
			DiffSource:  opts.DiffSource,
		})
		v.logger.Emit(logging.Opts{Domain: logging.LLM, Model: model}, "Running LLM verification...")
	}

	response, err := v.querier.Query(opts.Ctx, opts.WorkDir, prompt, model, allowedTools)
	if err != nil {
		result := verify.Result{Passed: true, Reason: "LLM verification skipped: " + err.Error()}
		v.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: model}, "LLM verification skipped: %v", err)
		return result, model
	}

	result := verify.ParseReviewResponse(response)
	if result.Passed {
		v.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: model}, "LLM verified: %s", result.Reason)
	} else {
		v.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Error, Model: model}, "LLM verification rejected: %s", result.Details)
	}

	return result, model
}

// verifyModel returns the model for the given 1-indexed attempt number,
// capped by ModelCap when set. Attempt 1 uses VerifyModel (haiku);
// subsequent attempts escalate to VerifyEscalationModel (sonnet).
func (v *Verifier) verifyModel(attempt int) string {
	var model string
	if attempt <= 1 {
		model = v.cfg.VerifyModel
		if model == "" {
			model = verify.ModelHaiku
		}
	} else {
		model = v.cfg.VerifyEscalationModel
		if model == "" {
			model = verify.ModelSonnet
		}
	}
	return verify.CapModel(v.cfg.ModelCap, model)
}

// FixModel returns the model for the given 1-indexed attempt number, capped
// by ModelCap when set. Attempt 1 uses FixModel (sonnet); subsequent attempts
// escalate to FixEscalationModel (opus). Exported so callers (e.g. loop
// pipeline logging) can reference the same model the fix agent will use.
func (v *Verifier) FixModel(attempt int) string {
	var model string
	if attempt <= 1 {
		model = v.cfg.FixModel
		if model == "" {
			model = verify.ModelSonnet
		}
	} else {
		model = v.cfg.FixEscalationModel
		if model == "" {
			model = verify.ModelOpus
		}
	}
	return verify.CapModel(v.cfg.ModelCap, model)
}

// FixAgentResult is the result of a fix-agent spawn. It is an alias for
// claude.Result so callers can reference it without importing the claude
// package directly.
type FixAgentResult = claude.Result

// FixAgentSpawn carries inputs shared by the various SpawnXFixAgent helpers.
type FixAgentSpawn struct {
	Ctx        context.Context
	TaskTitle  string
	WorkDir    string
	RawLogPath string
}

// SpawnTestFixAgent spawns a fix agent to address test-suite failures.
// Returns the claude result so Loop can check SignalDetected.
func (v *Verifier) SpawnTestFixAgent(in FixAgentSpawn, taskAcceptance, testOutput string, attempt, maxAttempts int) claude.Result {
	v.logger.Emit(logging.Opts{Domain: logging.Test}, "Spawning fix agent for test failures (attempt %d/%d)", attempt, maxAttempts)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-tests.md", map[string]string{
		"{{TASK_TITLE}}":       in.TaskTitle,
		"{{TASK_DESCRIPTION}}": fmt.Sprintf("Tests failed after completion. Fix the failures.\n\nAcceptance criteria:\n%s", taskAcceptance),
		"{{TEST_OUTPUT}}":      testOutput,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	return v.runFixAgent(in.Ctx, "test failures", fixPrompt, in.WorkDir, in.RawLogPath, attempt)
}

// SpawnCompileFixAgent spawns a fix agent to address compile/type errors.
func (v *Verifier) SpawnCompileFixAgent(in FixAgentSpawn, taskAcceptance, compileOutput string, attempt, maxAttempts int) claude.Result {
	v.logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Warn}, "Compile check failed — spawning fix agent (attempt %d/%d)", attempt, maxAttempts)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-tests.md", map[string]string{
		"{{TASK_TITLE}}":       in.TaskTitle,
		"{{TASK_DESCRIPTION}}": fmt.Sprintf("Build/type check failed after completion. Fix the compile errors.\n\nAcceptance criteria:\n%s", taskAcceptance),
		"{{TEST_OUTPUT}}":      compileOutput,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	return v.runFixAgent(in.Ctx, "build errors", fixPrompt, in.WorkDir, in.RawLogPath, attempt)
}

// SpawnVerifyFixAgent spawns a fix agent to address LLM verification rejection.
func (v *Verifier) SpawnVerifyFixAgent(in FixAgentSpawn, taskDesc, taskAcceptance, rejectionDetails string, attempt, maxAttempts int) claude.Result {
	v.logger.Emit(logging.Opts{Domain: logging.LLM, Model: v.FixModel(attempt)}, "Spawning fix agent for verification rejection (attempt %d/%d)", attempt, maxAttempts)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-fix.md", map[string]string{
		"{{TASK_TITLE}}":          in.TaskTitle,
		"{{TASK_DESCRIPTION}}":    taskDesc,
		"{{ACCEPTANCE_CRITERIA}}": taskAcceptance,
		"{{REJECTION_REASON}}":    rejectionDetails,
		"{{SIGNAL_COMPLETE}}":     signalPath,
	})

	return v.runFixAgent(in.Ctx, "verification rejection", fixPrompt, in.WorkDir, in.RawLogPath, attempt)
}

// CIFixInput is the per-call input for SpawnCIFixAgent. RequiredFailures is
// pre-filtered by the caller (Loop) using git.RequiredFailedChecks so
// verifier doesn't need to know about git types.
type CIFixInput struct {
	Spawn            FixAgentSpawn
	CILog            string
	PRNumber         int
	RequiredFailures []string // names of failed required checks
	OptionalFailures []string // names of failed optional checks (for logging)
}

// SpawnCIFixAgent spawns a fix agent to address CI failures. Returns empty
// claude.Result (SignalDetected=false) without spawning if RequiredFailures
// is empty — only optional/deploy checks failed.
func (v *Verifier) SpawnCIFixAgent(in CIFixInput) claude.Result {
	if len(in.OptionalFailures) > 0 {
		v.logger.Emit(logging.Opts{Domain: logging.CI}, "Ignoring optional/deploy check failures: %s", strings.Join(in.OptionalFailures, ", "))
	}
	if len(in.RequiredFailures) == 0 {
		v.logger.Emit(logging.Opts{Domain: logging.CI}, "Only optional checks failed on PR #%d — skipping fix agent", in.PRNumber)
		return claude.Result{}
	}

	v.logger.Emit(logging.Opts{Domain: logging.CI}, "CI failed on PR #%d — spawning fix agent for required checks", in.PRNumber)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-ci.md", map[string]string{
		"{{TASK_TITLE}}":      in.Spawn.TaskTitle,
		"{{FAILED_CHECKS}}":   strings.Join(in.RequiredFailures, ", "),
		"{{CI_LOG}}":          in.CILog,
		"{{SIGNAL_COMPLETE}}": signalPath,
	})

	return v.runFixAgent(in.Spawn.Ctx, "CI failures", fixPrompt, in.Spawn.WorkDir, in.Spawn.RawLogPath, 1)
}

// SpawnCopilotFixAgent spawns a fix agent to address actionable Copilot review
// comments. reviewContext is the pre-formatted comment block including file
// paths and line numbers.
func (v *Verifier) SpawnCopilotFixAgent(in FixAgentSpawn, reviewContext string) claude.Result {
	v.logger.Emit(logging.Opts{Domain: logging.Git}, "Spawning Copilot review fix agent")

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-copilot-review.md", map[string]string{
		"{{TASK_TITLE}}":      in.TaskTitle,
		"{{REVIEW_FEEDBACK}}": reviewContext,
		"{{SIGNAL_COMPLETE}}": signalPath,
	})

	return v.runFixAgent(in.Ctx, "Copilot review feedback", fixPrompt, in.WorkDir, in.RawLogPath, 1)
}

// SpawnConflictFixAgent spawns a fix agent to resolve merge conflicts that
// the automatic rebase could not handle.
func (v *Verifier) SpawnConflictFixAgent(in FixAgentSpawn, conflictDiff, beadDesc string) claude.Result {
	v.logger.Emit(logging.Opts{Domain: logging.Git}, "Unresolvable merge conflict — spawning conflict resolution agent")

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("resolve-conflict.md", map[string]string{
		"{{TASK_TITLE}}":       in.TaskTitle,
		"{{TASK_DESCRIPTION}}": beadDesc,
		"{{CONFLICT_DIFF}}":    conflictDiff,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	return v.runFixAgent(in.Ctx, "conflict resolution", fixPrompt, in.WorkDir, in.RawLogPath, 1)
}

// PreIterationInput holds inputs for RunPreIterationTests.
type PreIterationInput struct {
	Ctx context.Context
}

// PreIterationResult reports the outcome of pre-iteration checks. Loop uses
// Message in the agent prompt and TestPassed / CompilePassed to decide what
// to write to the state store.
type PreIterationResult struct {
	Message       string // human-readable status message appended to agent prompt
	TestResult    verify.Result
	CompileResult verify.Result
	TestElapsed   time.Duration
	CompileElapsed time.Duration
}

// RunPreIterationTests runs the full test suite and compile check before
// handing off to the agent. Returns a structured result so Loop can both
// log progress and write test_result state without verifier reaching into
// the state module.
func (v *Verifier) RunPreIterationTests(in PreIterationInput) PreIterationResult {
	if v.cfg.VerifyDir == "" {
		return PreIterationResult{}
	}

	out := PreIterationResult{}

	v.logger.Emit(logging.Opts{Domain: logging.Test}, "Running pre-iteration test suite...")
	testStart := time.Now()
	result := verify.RunTests(in.Ctx, v.cfg.TestTimeout, v.cfg.VerifyDir, v.cfg.ProjectDir)
	out.TestResult = result
	out.TestElapsed = time.Since(testStart).Truncate(10 * time.Millisecond)

	if result.Command != "" {
		v.logger.Emit(logging.Opts{Domain: logging.Test}, "Using: %s (in %s)", result.Command, result.Dir)
	}

	if result.Passed {
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Success}, "Pre-iteration tests: all passing (%s, %s)", result.Command, out.TestElapsed)
		out.Message += "\n- Test suite status: all tests passing as of start."
	} else if result.ScriptMissing {
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "ralph:verify script not found — skipping test suite")
	} else {
		cmdInfo := result.Command
		if cmdInfo == "" {
			cmdInfo = "unknown command"
		}
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Pre-iteration tests: failures detected (%s, %s, %s)", cmdInfo, result.Reason, out.TestElapsed)
		out.Message += "\n- Test suite status: some tests are FAILING. Fix them before your task. If the tests pass when you run them, they were fixed externally — proceed with your task."
		if result.Details != "" {
			details := result.Details
			lines := strings.Split(details, "\n")
			if len(lines) > 20 {
				details = strings.Join(lines[len(lines)-20:], "\n")
			}
			out.Message += "\n  Failure output:\n  " + strings.ReplaceAll(details, "\n", "\n  ")
		}
	}

	compileStart := time.Now()
	compileResult := verify.CompileCheck(in.Ctx, v.cfg.CompileCheckTimeout, v.cfg.VerifyDir)
	out.CompileResult = compileResult
	out.CompileElapsed = time.Since(compileStart).Truncate(10 * time.Millisecond)

	if compileResult.Command != "" {
		v.logger.Emit(logging.Opts{Domain: logging.Build}, "Using: %s (in %s)", compileResult.Command, compileResult.Dir)
	}

	if compileResult.Passed {
		cmdInfo := compileResult.Command
		if cmdInfo == "" {
			cmdInfo = "skipped"
		}
		v.logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Success}, "Pre-iteration compile check: passing (%s, %s)", cmdInfo, out.CompileElapsed)
	} else {
		cmdInfo := compileResult.Command
		if cmdInfo == "" {
			cmdInfo = "unknown command"
		}
		v.logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Warn}, "Pre-iteration compile check: failures detected (%s, %s, %s)", cmdInfo, compileResult.Reason, out.CompileElapsed)
		out.Message += "\n- Build status: compile check is FAILING. Fix the build errors before your task."
		details := compileResult.Details
		if details == "" {
			details = compileResult.Reason
		}
		if details != "" {
			lines := strings.Split(details, "\n")
			if len(lines) > 20 {
				details = strings.Join(lines[len(lines)-20:], "\n")
			}
			out.Message += "\n  Compile errors:\n  " + strings.ReplaceAll(details, "\n", "\n  ")
		}
	}

	return out
}

// runFixAgent is verifier's single entry point for spawning a fix-agent
// subprocess. It constructs a runner via the configured factory (production:
// agent.New(logger); tests: stub), runs the given prompt, and logs the
// outcome. Verifier does NOT touch the main loop's runner — Loop stops its
// own runner before calling any Spawn*FixAgent method.
func (v *Verifier) runFixAgent(ctx context.Context, description, prompt, workDir, rawLogPath string, attempt int) claude.Result {
	model := v.FixModel(attempt)
	v.logger.Emit(logging.Opts{Domain: logging.LLM, Model: model}, "Spawning fix agent: %s", description)

	runner := v.newRunner()
	result, _ := runner.Run(claude.RunConfig{
		Ctx:          ctx,
		WorkDir:      workDir,
		RalphDir:     v.cfg.RalphDir,
		Prompt:       prompt,
		RawLog:       rawLogPath,
		Quiet:        true,
		Signals:      v.cfg.Signals,
		PollInterval: 2 * time.Second,
		IdleTimeout:  v.cfg.IdleTimeout,
		Model:        model,
	})

	if !result.SignalDetected {
		v.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: model}, "Fix agent exited without signal (%s)", description)
	} else if result.Summary != "" {
		v.logger.Emit(logging.Opts{Domain: logging.LLM, Model: model}, "Fix agent (%s): %s", description, result.Summary)
	}

	return result
}

// loadVerifyPrompt reads a prompt template and performs variable
// substitution. When the template file is missing (e.g. in tests), it
// returns a key:value dump so the fix agent still gets the variables.
func (v *Verifier) loadVerifyPrompt(filename string, vars map[string]string) string {
	path := filepath.Join(v.cfg.PromptsDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		result := ""
		for k, val := range vars {
			result += k + ": " + val + "\n"
		}
		return result
	}
	s := string(data)
	for k, val := range vars {
		s = strings.ReplaceAll(s, k, val)
	}
	return s
}
