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
// a reference to the git/state/tasks modules — its one exception is calling
// git.TreeHash/git.WorktreeClean, two stateless data-only queries (no
// git.Ops, no module instance) used to key the green-tree test cache.
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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/verify"
)

// HeartbeatInterval is how often a heartbeat line is emitted while tests run.
// Exported so tests can override it.
var HeartbeatInterval = 30 * time.Second

// formatHeartbeatElapsed truncates elapsed to whole seconds for the
// heartbeat log line, so ticker jitter of a few milliseconds past a tick
// (e.g. 30.001s) doesn't leak sub-second noise into the printed duration.
func formatHeartbeatElapsed(elapsed time.Duration) string {
	return elapsed.Truncate(time.Second).String()
}

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
	ProjectDir            string
	ConfigVerify          string // when non-empty, used as the verify command instead of detecting ralph:verify scripts
	VerifyModel           string
	VerifyEscalationModel string
	FixModel              string // model for fix agents, from attempt 1; defaults to ModelOpus
	PromptsDir            string
	RalphDir              string
	Timeouts              claude.Timeouts
	TestTimeout           time.Duration
	CompileCheckTimeout   time.Duration

	// Signals is the set of paths used by fix-agent subprocesses to signal
	// completion. It is data, not a module reference.
	Signals claude.SignalPaths
}

// RunnerFactory produces a fresh fix-agent runner on each call. Verifier
// owns one of these as a submodule — the fix-agent runner is intrinsic to
// what verifier does (same relationship git has to github). Defined as an
// interface, consistent with its sibling submodule Querier, rather than a
// func-typed field.
type RunnerFactory interface {
	New() Runner
}

// RunnerFactoryFunc adapts a plain function to the RunnerFactory interface,
// the same way http.HandlerFunc adapts a function to http.Handler. Tests
// build a RunnerFactory this way without a dedicated named type per stub.
type RunnerFactoryFunc func() Runner

func (f RunnerFactoryFunc) New() Runner { return f() }

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

	// greenCache records the last tree hash a full RunTests/RunPreIterationTests
	// run passed on, keyed by the dir it ran in. A later RunTests call for the
	// same dir whose current tree hash matches AND whose worktree is clean is
	// a cache hit — it returns the recorded pass without invoking the test
	// command. Not persisted across process restarts (out of scope for this
	// in-process cache).
	greenCacheMu   sync.Mutex
	greenCacheDir  string
	greenCacheTree string
}

// New creates a Verifier from a pure-data Config, a logger, an explicit
// fix-agent runner factory, and an explicit LLM querier. When newRunner is
// nil it defaults to agent.New(logger, cfg.ProjectDir). When querier is nil
// it also defaults to agent.New(logger, cfg.ProjectDir), which satisfies
// the Querier interface via its Query method. Tests pass their own stubs —
// these are the ONLY behavioral injection points, and they are explicit
// constructor parameters, not Config fields. cfg.ProjectDir is forwarded
// so every spawned agent enforces the workDir != projectDir invariant.
func New(cfg Config, logger *logging.Logger, newRunner RunnerFactory, querier Querier) *Verifier {
	if newRunner == nil {
		newRunner = RunnerFactoryFunc(func() Runner { return agent.New(logger, cfg.ProjectDir) })
	}
	if querier == nil {
		querier = agent.New(logger, cfg.ProjectDir)
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
	if tree, ok := v.checkGreenCache(dir); ok {
		v.logger.Emit(logging.Opts{Domain: logging.Test}, "Tests cached: tree %s already green", tree)
		return verify.Result{Passed: true, Reason: "cached: tree " + tree + " already green"}, 0
	}

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
					"Tests still running... (%s elapsed)", formatHeartbeatElapsed(time.Since(start)))
			}
		}
	}()

	result := verify.RunTests(ctx, v.cfg.TestTimeout, v.cfg.ConfigVerify, dir)
	elapsed := time.Since(start).Truncate(time.Millisecond)

	switch {
	case result.ScriptMissing:
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "ralph:verify script not found — cannot verify")
	case result.Passed:
		v.logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed (%s)", elapsed)
		v.recordGreenCache(dir)
	default:
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Tests failed (%s): %s", elapsed, result.Reason)
	}

	return result, elapsed
}

// checkGreenCache reports whether dir's current tree hash matches the last
// recorded green run for dir and the worktree is clean. Returns the tree
// hash and true on a cache hit. A dirty worktree, a changed tree, or a dir
// mismatch (including non-git dirs, where TreeHash returns "") is always a
// miss — real work never reads a stale cache.
func (v *Verifier) checkGreenCache(dir string) (string, bool) {
	v.greenCacheMu.Lock()
	cachedDir, cachedTree := v.greenCacheDir, v.greenCacheTree
	v.greenCacheMu.Unlock()

	if cachedTree == "" || cachedDir != dir {
		return "", false
	}
	tree := git.TreeHash(dir)
	if tree == "" || tree != cachedTree {
		return "", false
	}
	if !git.WorktreeClean(dir) {
		return "", false
	}
	return tree, true
}

// recordGreenCache records dir's current tree hash as the last known green
// run. Called only after a passing test run. A hash that cannot be computed
// (non-git dir) simply clears any prior cache for dir rather than recording
// a bad key.
func (v *Verifier) recordGreenCache(dir string) {
	tree := git.TreeHash(dir)
	v.greenCacheMu.Lock()
	defer v.greenCacheMu.Unlock()
	if tree == "" {
		if v.greenCacheDir == dir {
			v.greenCacheDir, v.greenCacheTree = "", ""
		}
		return
	}
	v.greenCacheDir, v.greenCacheTree = dir, tree
}

// SeedGreenCache primes the in-memory cache with a (dir, tree) pair read
// from a prior session's persisted state, so a freshly constructed Verifier
// (a new loop process) can hit the cache on its very first check instead of
// only ever missing until it records its own green run. checkGreenCache
// still re-verifies the seeded tree against the live worktree (current tree
// hash + clean status) before treating it as a hit, so a stale or mismatched
// seed is safely ignored.
func (v *Verifier) SeedGreenCache(dir, tree string) {
	v.greenCacheMu.Lock()
	defer v.greenCacheMu.Unlock()
	v.greenCacheDir, v.greenCacheTree = dir, tree
}

// GreenCache returns the current in-memory cache's (dir, tree) pair, or two
// empty strings if no green run has been recorded yet. Callers (Loop) use
// this to persist the cache to state.json after a green run, so it survives
// a process restart.
func (v *Verifier) GreenCache() (dir, tree string) {
	v.greenCacheMu.Lock()
	defer v.greenCacheMu.Unlock()
	return v.greenCacheDir, v.greenCacheTree
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
		var buildErr error
		prompt, buildErr = verify.BuildReviewPrompt(verify.ReviewPromptInput{
			PromptsDir:  v.cfg.PromptsDir,
			Title:       opts.Title,
			Description: opts.Description,
			Acceptance:  opts.Acceptance,
			Diff:        opts.Diff,
			DiffSource:  opts.DiffSource,
		})
		if buildErr != nil {
			result := verify.Result{Passed: true, Reason: "LLM verification skipped: " + buildErr.Error()}
			v.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: model}, "LLM verification skipped: %v", buildErr)
			return result, model
		}
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

// verifyModel returns the model for the given 1-indexed attempt number.
// Attempt 1 uses VerifyModel (haiku); subsequent attempts escalate to
// VerifyEscalationModel (sonnet).
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
	return model
}

// FixModel returns the model fix agents use, from their first attempt.
// The attempt parameter is retained for signature stability with other
// per-attempt selectors (e.g. verifyModel); fix agents no longer escalate
// across attempts. Exported so callers (e.g. loop pipeline logging) can
// reference the same model the fix agent will use.
func (v *Verifier) FixModel(_ int) string {
	model := v.cfg.FixModel
	if model == "" {
		model = verify.ModelOpus
	}
	return model
}

// FixAgentResult is the result of a fix-agent spawn. It is an alias for
// claude.Result so callers can reference it without importing the claude
// package directly.
type FixAgentResult = claude.Result

// FixAgentInput is the data-only input for SpawnFixAgent. Template is the
// prompt template filename; Vars holds its substitution map (do not include
// {{SIGNAL_COMPLETE}} — SpawnFixAgent injects it from cfg.RalphDir).
// Attempt drives FixModel selection; Description tags log lines (defaults to
// the template name when empty).
type FixAgentInput struct {
	Ctx         context.Context
	Template    string
	Vars        map[string]string
	Attempt     int
	WorkDir     string
	RawLogPath  string
	Description string
}

// SpawnFixAgent loads in.Template, substitutes in.Vars plus
// {{SIGNAL_COMPLETE}} (derived from cfg.RalphDir), and spawns a fix-agent
// subprocess. Model is chosen by FixModel(in.Attempt). Returns the agent
// result so the caller can check SignalDetected.
func (v *Verifier) SpawnFixAgent(in FixAgentInput) FixAgentResult {
	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	vars := make(map[string]string, len(in.Vars)+1)
	for k, val := range in.Vars {
		vars[k] = val
	}
	vars["{{SIGNAL_COMPLETE}}"] = signalPath

	prompt := v.loadVerifyPrompt(in.Template, vars)
	desc := in.Description
	if desc == "" {
		desc = strings.TrimSuffix(in.Template, ".md")
	}
	return v.runFixAgent(in.Ctx, desc, prompt, in.WorkDir, in.RawLogPath, in.Attempt)
}

// PreIterationInput holds inputs for RunPreIterationTests.
type PreIterationInput struct {
	Ctx     context.Context
	WorkDir string // live per-task worktree directory; empty skips pre-iteration tests
}

// PreIterationResult reports the outcome of pre-iteration checks. Loop uses
// Message in the agent prompt and TestPassed / CompilePassed to decide what
// to write to the state store.
type PreIterationResult struct {
	Message        string // human-readable status message appended to agent prompt
	TestResult     verify.Result
	CompileResult  verify.Result
	TestElapsed    time.Duration
	CompileElapsed time.Duration
}

// RunPreIterationTests runs the full test suite and compile check before
// handing off to the agent. Returns a structured result so Loop can both
// log progress and write test_result state without verifier reaching into
// the state module.
func (v *Verifier) RunPreIterationTests(in PreIterationInput) PreIterationResult {
	if in.WorkDir == "" {
		return PreIterationResult{}
	}

	out := PreIterationResult{}

	if tree, ok := v.checkGreenCache(in.WorkDir); ok {
		v.logger.Emit(logging.Opts{Domain: logging.Test}, "Tests cached: tree %s already green", tree)
		out.TestResult = verify.Result{Passed: true, Reason: "cached: tree " + tree + " already green"}
		out.Message += "\n" + v.statusFragment("status-tests-pass.md")
		return v.runPreIterationCompileCheck(in, out)
	}

	tc := verify.DetectTestCommand(v.cfg.ConfigVerify, in.WorkDir)
	if tc != nil {
		source := "config.toml"
		if v.cfg.ConfigVerify == "" {
			if tc.Cmd == "make" {
				source = "Makefile"
			} else {
				source = "package.json"
			}
		}
		command := tc.Cmd + " " + strings.Join(tc.Args, " ")
		v.logger.Emit(logging.Opts{Domain: logging.Test}, "Running test suite: %s (from %s in %s)", command, source, tc.Dir)
	} else {
		v.logger.Emit(logging.Opts{Domain: logging.Test}, "Running pre-iteration test suite...")
	}
	testStart := time.Now()
	result := verify.RunTests(in.Ctx, v.cfg.TestTimeout, v.cfg.ConfigVerify, in.WorkDir)
	out.TestResult = result
	out.TestElapsed = time.Since(testStart).Truncate(10 * time.Millisecond)

	if result.Passed {
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Success}, "Pre-iteration tests: all passing (%s, %s)", result.Command, out.TestElapsed)
		out.Message += "\n" + v.statusFragment("status-tests-pass.md")
		v.recordGreenCache(in.WorkDir)
	} else if result.ScriptMissing {
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "ralph:verify script not found — skipping test suite")
	} else {
		cmdInfo := result.Command
		if cmdInfo == "" {
			cmdInfo = "unknown command"
		}
		v.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Pre-iteration tests: failures detected (%s, %s, %s)", cmdInfo, result.Reason, out.TestElapsed)
		out.Message += "\n" + v.statusFragment("status-tests-failing.md")
		if result.Details != "" {
			details := result.Details
			lines := strings.Split(details, "\n")
			if len(lines) > 20 {
				details = strings.Join(lines[len(lines)-20:], "\n")
			}
			out.Message += "\n  Failure output:\n  " + strings.ReplaceAll(details, "\n", "\n  ")
		}
	}

	return v.runPreIterationCompileCheck(in, out)
}

// runPreIterationCompileCheck runs the compile/build check and appends its
// status to out.Message. Split out of RunPreIterationTests so a test-cache
// hit can skip straight to the compile check without duplicating this tail.
func (v *Verifier) runPreIterationCompileCheck(in PreIterationInput, out PreIterationResult) PreIterationResult {
	compileStart := time.Now()
	compileResult := verify.CompileCheck(in.Ctx, v.cfg.CompileCheckTimeout, in.WorkDir)
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
		out.Message += "\n" + v.statusFragment("status-build-failing.md")
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

	runner := v.newRunner.New()
	result, _ := runner.Run(claude.RunConfig{
		Ctx:          ctx,
		WorkDir:      workDir,
		RalphDir:     v.cfg.RalphDir,
		Prompt:       prompt,
		RawLog:       rawLogPath,
		Signals:      v.cfg.Signals,
		PollInterval: 2 * time.Second,
		Timeouts:     v.cfg.Timeouts,
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

// statusFragment reads a short agent-facing status line from promptsDir,
// trimming the trailing newline. Used to append pre-iteration test/build
// status onto the agent prompt.
func (v *Verifier) statusFragment(filename string) string {
	return strings.TrimRight(v.loadVerifyPrompt(filename, nil), "\n")
}
