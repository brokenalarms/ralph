package loop

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/retry"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/verify"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Modules is the struct form of loop.New's module-reference parameter
// list. It is the single composition point where modules from many
// packages meet: cmd/ralph/main.go constructs a Modules literal and
// passes it to loop.New, which copies each field onto Loop's private
// fields and discards the struct. After loop.New returns, the Modules
// struct no longer exists anywhere.
//
// This is the ONLY exported struct in the codebase permitted to hold
// module references. It is distinct in kind from the forbidden *Deps/
// *Opts/*Params antipattern because:
//
//   - It is owned by nobody after construction — not held on any
//     module, not stored as a field anywhere.
//   - It IS loop.New's parameter list, just packaged as a struct for
//     self-documentation and partial-override ergonomics in tests.
//   - Rule A's carve-out for loop.New's parameters applies to Modules
//     just as it does to positional parameters.
//
// Connectivity bundles the network-reachability checks the loop runs at
// startup and between iterations. Defined locally in the loop package
// (same dependency-inversion pattern as git.StateStore) so the loop
// holds no peer-module reference and no function fields. When the
// caller passes a nil Modules.Connectivity, loop.New defaults it to Loop
// itself (which delegates to git.Ops.PingGitHub and the git package's
// connectivity helpers — see Loop's CheckGitHub/IsOnline/WaitForInternet
// methods). Tests pass non-nil stub implementations to override the live
// network behavior.
type Connectivity interface {
	// CheckGitHub returns nil when GitHub is reachable; an error otherwise.
	// Run once at startup before any task execution.
	CheckGitHub(ctx context.Context) error
	// IsOnline returns whether the local network is currently reachable.
	IsOnline() bool
	// WaitForInternet blocks until the network is reachable or the context
	// is cancelled. Returns true on success, false on cancellation.
	WaitForInternet(ctx context.Context, logger *logging.Logger) bool
}

// IterationHook fires once at the start of every loop iteration. Production
// uses it to regenerate the resume script so the user can resume from the
// most recent state. Tests can omit it (nil = no-op).
type IterationHook interface {
	OnIterationStart()
}

// PostTaskHook fires after each task completes. Production has its own
// runPostTask path (driven by cfg.PostTask script + RALPH_TASK_ID env);
// this interface exists to let tests observe completions or to let
// alternative orchestrators short-circuit the script path.
type PostTaskHook interface {
	OnPostTask(ctx context.Context, taskID string, prNumber int, merged bool)
}

// WaitHook fires when the loop enters its wait-for-tasks state. Tests
// use it to detect that the wait path was reached. Production uses nil.
type WaitHook interface {
	OnWait()
}

// VerifyHook is the legacy fallback verification path used when the
// runner did not run verification via OnSignal. Production uses
// runSimpleVerifyCompletion when nil. Tests can stub this to bypass
// the test/LLM verification entirely.
type VerifyHook interface {
	Verify(ctx context.Context, dir, headBefore string) (bool, string)
}

// BinaryHasher computes the SHA-256 of the running binary on demand.
// Production uses liveBinaryHasher (reads os.Executable()); tests may
// inject a stub that returns different hashes to simulate a binary swap.
type BinaryHasher interface {
	Hash() ([]byte, error)
}

// liveBinaryHasher is the production BinaryHasher. It resolves symlinks so
// that wrapper scripts pointing at the real binary don't mask a swap.
type liveBinaryHasher struct{}

func (h *liveBinaryHasher) Hash() ([]byte, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return sum[:], nil
}

// See docs/specs/orchestrator-modules.md for the full rationale on the
// Modules carve-out. Modules holds the construction-time dependencies
// the loop composes; loop.New copies each field onto Loop's private
// fields and discards the struct. The interfaces below all default to
// production implementations when nil — tests pass non-nil stubs to
// override behavior.
type Modules struct {
	State         *state.Store
	Git           git.Ops
	TaskBackend   tasks.Backend
	Logger        *logging.Logger
	Verifier      *verifier.Verifier
	Runner        claudeRunner  // nil → agent.New(logger)
	Connectivity  Connectivity  // nil → live gh CLI / net checks
	IterationHook IterationHook // nil → no-op
	PostTaskHook  PostTaskHook  // nil → fallback to runPostTask script path
	WaitHook      WaitHook      // nil → no-op
	VerifyHook    VerifyHook    // nil → fallback to runSimpleVerifyCompletion
	BinaryHasher  BinaryHasher  // nil → reads os.Executable() SHA-256
}

// Config holds all parameters needed by the execution loop. Pure data —
// no function fields, no module references. The behavioral injection
// points (Connectivity, hooks, runner) live on Modules.
type Config struct {
	Dirs                  workctx.WorkContext
	PlanFile              string
	MaxIterations         int
	AutoMerge             bool
	Evolve                bool
	CallsPerHour          int
	Timeouts              claude.Timeouts
	PostTask              string
	VerifyBuild           string
	Notify                bool
	Wait                  bool
	Verbose               bool
	WorkingModel          string
	Version               string
	Verify                string // when non-empty, used as the verify command instead of detecting ralph:verify scripts
	VerifyModel           string // model for the first LLM verification attempt; defaults to haiku
	VerifyEscalationModel string // model for subsequent LLM verification attempts; defaults to sonnet
	FixModel              string // model for fix agents, from attempt 1; defaults to opus

	// Attempt limits — overrides package defaults when set.
	MaxPromptAttempts      int
	MaxIdleTimeoutFailures int
	MaxLLMVerifyAttempts   int
	MaxTestFixAttempts     int

	// Stagnation thresholds — overrides package defaults when set.
	MaxFailedStarts    int
	MaxCompactionParks int
	CascadeSkipLimit   int

	// Test/compile timeouts.
	TestTimeout         time.Duration
	CompileCheckTimeout time.Duration

	// Network timeouts.
	ConnectivityCheckTimeout time.Duration
	InternetRestoreInterval  time.Duration

	// ShipRetryBackoffs overrides the default retry delays for transient GitHub
	// errors (default: 5s, 15s, 30s). Set to zero-duration slices in tests to
	// avoid sleeping.
	ShipRetryBackoffs []time.Duration

	// InfraRetryBackoffs overrides the backoff delays between infrastructure CI
	// retries (default: 1min, 2min, 4min). Set to zero-duration slices in tests
	// to avoid sleeping. The slice length defines the configured infra-retry
	// backoff schedule, but actual infra retries may also be limited by the
	// overall ship retry budget.
	InfraRetryBackoffs []time.Duration
}

// Loop is the production Connectivity default, used when Modules.Connectivity
// is nil. Loop is the one struct permitted to hold module references (see
// docs/specs/orchestrator-modules.md), so the live implementation lives here
// instead of on a separate helper struct that would have to hold git.Ops
// itself. CheckGitHub delegates GitHub reachability to the injected git.Ops
// interface; IsOnline delegates local network probing to the git package's
// connectivity helpers.
//
// IsOnline and WaitForInternet honor cfg.ConnectivityCheckTimeout (typically
// 3 seconds — these are fast local-network reachability pings). CheckGitHub
// uses git.Ops.PingGitHub, which applies its own hardcoded 10-second
// timeout, because the gh api round-trip is a different kind of check (auth
// + GitHub server latency) and the same 3s budget would produce false
// negatives. The two timeouts measure different things and should not share
// a value.
var _ Connectivity = (*Loop)(nil)

func (l *Loop) CheckGitHub(ctx context.Context) error {
	return l.git.PingGitHub(ctx)
}
func (l *Loop) IsOnline() bool { return git.IsOnline(l.cfg.ConnectivityCheckTimeout) }
func (l *Loop) WaitForInternet(ctx context.Context, logger *logging.Logger) bool {
	return waitForInternet(ctx, logger, l.cfg.InternetRestoreInterval, l.cfg.ConnectivityCheckTimeout)
}

// claudeRunner abstracts the streaming-agent session for testability.
type claudeRunner interface {
	Run(cfg claude.RunConfig) (claude.Result, error)
	StopStreaming()
	InjectMessage(msg string) error
}

// CompletedTask holds summary info for a task completed during this session.
type CompletedTask struct {
	ID      string
	Title   string
	Summary string
	PRNum   int
	PRTitle string
	PRURL   string
}

// Loop orchestrates the execution phase: task selection, prompt building,
// rate limiting, branch rotation, Claude invocation, and response analysis.
//
// The logger field holds the single cross-module exception to the
// "no module objects passed through" rule. Logging is genuinely
// cross-cutting — every package needs to log — and package-level state
// would leak across parallel tests. The logger is constructed once in
// cmd/ralph/main.go and threaded into Loop and other modules at
// construction time. This is the only such exception in the codebase.
type Loop struct {
	cfg                     Config
	state                   *state.Store
	git                     git.Ops
	taskBackend             tasks.Backend
	limiter                 *ratelimit.Limiter
	runner                  claudeRunner
	verifier                *verifier.Verifier
	analyzer                *analyzer.Analyzer
	taskAttempts            []AttemptEvent // in-memory attempt events for the current task, reset per task
	taskIdleTimeouts        int            // consecutive idle timeout count for the current task
	currentTaskID           string         // tracks which task's attempts are in taskAttempts
	consecutiveNoAgentIters int            // incremented when an iteration ends without invoking the agent; reset on agent invocation
	logger                  *logging.Logger
	signals                 claude.SignalPaths
	connectivity            Connectivity
	iterationHook           IterationHook
	postTaskHook            PostTaskHook
	waitHook                WaitHook
	verifyHook              VerifyHook
	binaryHasher            BinaryHasher
	startupBinaryHash       []byte
	completedTasks          []CompletedTask
	sessionSkippedIDs       map[string]bool // in-memory skip set for this session; no state.json persistence
	activeReviewers         []git.Reviewer
	reviewersDetected       bool
	skipStreakReason        tasks.SkipReason // reason of the most recent skip in the current unbroken run of consecutive skips; reset once a task succeeds
}

// New creates an execution loop from the given configuration and module
// references. mods is the single composition point where modules from
// many packages meet — see the Modules type comment for the rule that
// carves it out from the "no module types in exported struct fields"
// prohibition. Nil interface fields on mods default to production
// implementations.
func New(cfg Config, mods Modules) *Loop {
	st := mods.State
	gm := mods.Git
	logger := mods.Logger
	taskBackend := mods.TaskBackend

	signals := claude.DefaultSignalPaths(cfg.Dirs.RalphDir)

	limiter := ratelimit.New(cfg.Dirs.RalphDir, cfg.CallsPerHour)

	if cfg.ConnectivityCheckTimeout == 0 {
		cfg.ConnectivityCheckTimeout = 3 * time.Second
	}
	if cfg.InternetRestoreInterval == 0 {
		cfg.InternetRestoreInterval = 30 * time.Second
	}
	if cfg.TestTimeout == 0 {
		cfg.TestTimeout = 5 * time.Minute
	}
	if cfg.CompileCheckTimeout == 0 {
		cfg.CompileCheckTimeout = 60 * time.Second
	}

	runner := mods.Runner
	if runner == nil {
		runner = agent.New(logger, cfg.Dirs.ProjectDir)
	}

	bh := mods.BinaryHasher
	if bh == nil {
		bh = &liveBinaryHasher{}
	}

	l := &Loop{
		cfg:           cfg,
		state:         st,
		git:           gm,
		taskBackend:   taskBackend,
		limiter:       limiter,
		runner:        runner,
		verifier:      mods.Verifier,
		analyzer:      analyzer.New(),
		logger:        logger,
		signals:       signals,
		iterationHook: mods.IterationHook,
		postTaskHook:  mods.PostTaskHook,
		waitHook:      mods.WaitHook,
		verifyHook:    mods.VerifyHook,
		binaryHasher:  bh,
	}
	if mods.Connectivity != nil {
		l.connectivity = mods.Connectivity
	} else {
		l.connectivity = l
	}
	return l
}

// SessionTasks returns the tasks completed during this session.
func (l *Loop) SessionTasks() []CompletedTask {
	return l.completedTasks
}

// emitTaskSummary prints a clean single-task completion block immediately
// when a task finishes, so each task's outcome is visible as it happens
// rather than accumulated into a session dump.
func emitTaskSummary(ct CompletedTask, log *logging.Logger) {
	fmt.Println()
	log.Phase("=== TASK COMPLETE ===")
	log.Emit(logging.Opts{}, "Task:  %s", ct.ID)
	if ct.Title != "" {
		log.Emit(logging.Opts{}, "Issue: %s", ct.Title)
	}
	if ct.Summary != "" {
		log.Emit(logging.Opts{}, "Fix:   %s", ct.Summary)
	}
	if ct.PRNum != 0 || ct.PRURL != "" {
		pr := fmt.Sprintf("PR #%d", ct.PRNum)
		if ct.PRURL != "" {
			pr = ct.PRURL
		}
		log.Emit(logging.Opts{}, "PR:    %s", pr)
	}
}

// skipTask reassigns the bead to ralph-task (config.TaskAssignee), records
// the reason as a comment, and tracks the ID in-memory for the lifetime of
// this session. The bead leaves the loop's bd ready inbox by assignment, so
// no separate skip filter is needed. Always parks — open dependents are a
// triage concern for the task manager, not a reason to refuse.
func (l *Loop) skipTask(id string, reason tasks.SkipReason, detail string) {
	if id == "" {
		return
	}
	msg := string(reason)
	if detail != "" {
		msg += ": " + detail
	}
	l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Skipping task %s: %s", id, msg)
	if err := l.taskBackend.SkipTask(id, reason, detail); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to skip task %s in backend: %v", id, err)
	}
	l.state.ClearCurrentTask()
	if l.sessionSkippedIDs == nil {
		l.sessionSkippedIDs = make(map[string]bool)
	}
	l.sessionSkippedIDs[id] = true
}

// appWideRecurrence reports whether reason already caused a skip earlier in
// the current unbroken run of consecutive skips. A reason that has already
// exhausted one task's retries is conclusive — a second task hitting the
// same failure category should halt the loop immediately rather than burn
// its own in-task retry bracket. Empty reasons never recur.
func (l *Loop) appWideRecurrence(reason tasks.SkipReason) bool {
	return reason != "" && reason == l.skipStreakReason
}

// recordSkipStreak marks reason as having caused a skip in the current
// streak, so a later task hitting the same reason halts on first failure.
func (l *Loop) recordSkipStreak(reason tasks.SkipReason) {
	if reason != "" {
		l.skipStreakReason = reason
	}
}

// resetSkipStreak clears the streak reason once a task succeeds, breaking
// the chain of consecutive skips.
func (l *Loop) resetSkipStreak() {
	l.skipStreakReason = ""
}

// haltAppWide skips the task and halts the whole loop because reason
// already caused a skip earlier in this streak — the recurrence across
// tasks is what makes it app-wide rather than a single task's problem.
func (l *Loop) haltAppWide(taskID string, reason tasks.SkipReason, detail string) {
	l.skipTask(taskID, reason, detail)
	haltReason := "app_wide:" + string(reason)
	l.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Error}, "Halting: %s", haltReason)
	l.state.Write("status", "halted_"+haltReason)
	l.git.TagTaskEnd(taskID)
}

// ensureActiveReviewers populates l.activeReviewers on first call. Subsequent
// calls are no-ops. The loop is single-threaded so no synchronization is needed.
func (l *Loop) ensureActiveReviewers(ctx context.Context) {
	if l.reviewersDetected {
		return
	}
	l.reviewersDetected = true
	reviewers, err := l.git.DetectActiveReviewers(ctx)
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Could not detect active reviewers: %v", err)
		return
	}
	l.activeReviewers = reviewers
	for _, r := range reviewers {
		if r.AppSlug == "copilot-code-review" {
			if r.ReviewOnPush {
				l.logger.Emit(logging.Opts{Domain: logging.Git}, "Copilot code review is enabled (review_on_push=true)")
			} else {
				l.logger.Emit(logging.Opts{Domain: logging.Git}, "Copilot code review is enabled (review_on_push=false, opportunistic)")
			}
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Git}, "%s code review is enabled", r.AppSlug)
		}
	}
}

// agentRunResult carries the outcome of a single agent invocation.
type agentRunResult struct {
	prep         iterationPrompt
	result       claude.Result
	iterAction   analyzer.Action
	diffStat     string
	action       loopAction // actionDone or actionRetry if short-circuited; actionProceed otherwise
	agentInvoked bool       // true when runner.Run was actually called this iteration
}

// runAgent prepares the prompt, invokes Claude, handles run errors, and analyzes
// the outcome. If action != actionProceed, Run() should use that action directly.
// waitForRate returns true immediately if the rate limit allows the call.
// If the limit is exceeded, it waits for the reset window and returns true
// once the limit clears, or false if the context is cancelled.
func (l *Loop) waitForRate(ctx context.Context) bool {
	if l.limiter.Allowed() {
		return true
	}

	l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn}, "Rate limit reached (%d/%d calls this hour)", l.limiter.Count(), l.cfg.CallsPerHour)

	err := l.limiter.WaitForReset(ctx, func(secs int) {
		l.logger.Emit(logging.Opts{Domain: logging.LLM}, "Rate limit: %ds until reset", secs)
	})
	return err == nil
}

func (l *Loop) runAgent(ctx context.Context, task taskContext, runIteration int) agentRunResult {
	if ctx.Err() != nil {
		return agentRunResult{action: actionDone}
	}

	taskKey := task.id
	if taskKey == "" {
		taskKey = task.title
	}
	if taskKey != l.currentTaskID {
		l.taskAttempts = nil
		l.taskIdleTimeouts = 0
		l.currentTaskID = taskKey
		l.analyzer.ResetForNewTask()
	}

	prep, ok := l.prepareAndBuildPrompt(ctx, task.id, task.title)
	if !ok {
		return agentRunResult{action: actionDone}
	}

	taskStart := time.Now()
	agentModel := l.cfg.WorkingModel
	l.logger.Emit(logging.Opts{Domain: logging.LLM, Model: agentModel}, "Agent model: %s", agentModel)
	result, runErr := l.runner.Run(claude.RunConfig{
		Ctx:          ctx,
		WorkDir:      prep.workDir,
		RalphDir:     l.cfg.Dirs.RalphDir,
		Prompt:       prep.fullPrompt,
		TaskID:       task.id,
		RawLog:       prep.rawLogPath,
		LogFile:      logging.ActiveLogPath(l.cfg.Dirs.RalphDir, "loop"),
		Verbose:      l.cfg.Verbose,
		Model:        agentModel,
		Signals:      l.signals,
		PollInterval: 2 * time.Second,
		Timeouts:     l.cfg.Timeouts,
		OnSignal: func(summary string) bool {
			// Stop the main runner before spawning fix-agent subprocesses
			// inside runVerifyPipeline. The main agent session is wrapping
			// up anyway (it just signaled completion).
			l.runner.StopStreaming()
			// Capture HEAD before any subsequent git operations so the
			// verify pipeline can assert this commit is still reachable.
			signalTimeHead := l.git.HeadRev()
			verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
				ctx:            ctx,
				headBefore:     prep.headBefore,
				signalTimeHead: signalTimeHead,
				workDir:        prep.workDir,
				rawLogPath:     prep.rawLogPath,
				taskID:         task.id,
				nextTask:       task.title,
			})
			if skipReason != "" {
				l.skipTask(task.id, tasks.SkipVerificationRejected, skipReason)
			}
			return verified
		},
		FeedbackFile: filepath.Join(l.cfg.Dirs.RalphDir, "feedback"),
	})

	runAction := l.handleRunResult(ctx, result, runErr, task.id, task.title, prep.headBefore, runIteration)
	if runAction != actionProceed {
		return agentRunResult{action: runAction, prep: prep, result: result, agentInvoked: true}
	}
	elapsed := time.Since(taskStart)
	l.limiter.Increment()

	headAfter := l.git.HeadRev()
	iterState := analyzeIteration(analyzeIterationParams{
		hasDiff:      l.git.HasDiff(),
		changedFiles: l.git.ChangedFiles(prep.headBefore, headAfter),
		signals:      l.signals,
	}, prep.rawLogPath, prep.logStart, prep.headBefore, headAfter, task.id)
	analysisResult := l.analyzer.Analyze(iterState)

	diffStat, halt, iterAction := l.processRunOutcome(result, elapsed, runIteration, prep, task.id, task.title, analysisResult, headAfter)
	if halt {
		return agentRunResult{action: actionDone, iterAction: iterAction, agentInvoked: true}
	}
	if iterAction == analyzer.Skip {
		return agentRunResult{action: actionSkip, iterAction: iterAction, agentInvoked: true}
	}

	return agentRunResult{
		prep:         prep,
		result:       result,
		iterAction:   iterAction,
		diffStat:     diffStat,
		action:       actionProceed,
		agentInvoked: true,
	}
}

// Run executes the full iteration loop. Returns nil on normal completion
// (all tasks done, max iterations reached, or stopped). Returns an error
// for unrecoverable failures.
func (l *Loop) Run(ctx context.Context) error {
	if verify.DetectTestCommand(l.cfg.Verify, l.cfg.Dirs.ProjectDir) == nil {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "No ralph:verify script found in %s — add a \"ralph:verify\" script to package.json (or a make ralph-verify target) for test-based verification. Continuing with LLM verification only.", l.cfg.Dirs.ProjectDir)
	}

	if err := l.connectivity.CheckGitHub(ctx); err != nil {
		if ctx.Err() != nil {
			l.state.Write("status", "stopped")
			return nil
		}
		return fmt.Errorf("Cannot reach GitHub (%v).\nPossible causes: VPN blocking GitHub, no internet, gh auth expired.\nFixes: disconnect VPN, check internet, or run \"gh auth login\" to refresh credentials.", err)
	}

	if err := l.limiter.Init(); err != nil {
		return fmt.Errorf("rate limiter init: %w", err)
	}
	if err := l.initialize(ctx); err != nil {
		return err
	}

	// Recover any loop-owned in_progress beads stranded by a prior session
	// (crash, kill, or an abandon that did not release the claim). Without this
	// an orphan with no open dependents is invisible to `bd ready` and never
	// reaches detectStuckInProgress, so it stays in_progress forever.
	l.reconcileOrphanedClaims()

	if l.cfg.Evolve {
		if h, err := l.binaryHasher.Hash(); err == nil {
			l.startupBinaryHash = h
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Loop, Level: logging.Warn}, "Evolve: startup binary hash failed: %v — evolve hash guard disabled", err)
		}
	}

	st := &loopState{}
	loaded, _ := l.state.Load()
	iteration := loaded.Iteration

iterLoop:
	for {
		// ── Task selection ──
		completedIDs := make(map[string]bool, len(st.sessionTasks))
		for _, ct := range st.sessionTasks {
			completedIDs[ct.ID] = true
		}
		task, action, waited := l.selectNextTask(ctx, selectNextTaskParams{
			runIteration:   st.runIteration,
			maxIterations:  l.cfg.MaxIterations,
			wait:           l.cfg.Wait,
			completedIDs:   completedIDs,
			lastTaskMerged: st.lastTaskMerged,
		})
		if action == actionDone {
			break
		}

		// ── End-of-wait: sync + evolve after idle wait ──
		// Fires when the loop just exited waitForTasks with a new task. Catches
		// any binary rebuild that occurred while the loop was idle between tasks.
		if waited && ctx.Err() == nil {
			if res := l.postTaskAndMaybeEvolve(ctx, task.id, 0, false); res == signalEvolve {
				break iterLoop
			}
		}

		st.runIteration++
		iteration++
		st.lastTaskMerged = false
		st.currentTaskID = task.id

		// Retry counters are local variables inside runVerifyPipeline, so
		// they are naturally scoped per-iteration — no reset needed.

		// ── Worktree setup: recreate after task teardown ──
		if st.worktreeNeedsSetup {
			if err := l.git.SetupWorktree(ctx); err != nil {
				if ctx.Err() != nil {
					l.state.Write("status", "stopped")
					break
				}
				l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error}, "SetupWorktree failed: %v", err)
				l.state.Write("status", "error")
				break
			}
			st.worktreeNeedsSetup = false
		}

		// ── Branch setup ──
		switch l.setupBranchForTask(ctx, task) {
		case branchHalt:
			break iterLoop
		case branchContinue:
			continue iterLoop
		}

		if l.iterationHook != nil {
			l.iterationHook.OnIterationStart()
		}

		l.logIterationBanner(logIterationBannerParams{
			version: l.cfg.Version,
		}, st.runIteration, l.state.ReadMaxIterations(l.cfg.MaxIterations), task, st.lastAction)
		l.beginIteration(task, iteration)

		// ── Resume check: does a PR already exist for this task? ──
		decision := l.resumeCheck(ctx, task)
		switch decision {
		case resumeHalt:
			break iterLoop
		case resumeHandled:
			continue
		}

		// ── Produce a completeTaskOut from one of two entry points, then run
		// the shared aftermath. An existing open PR ships through the single
		// shipAndFinalize path (no agent); otherwise the agent runs and
		// completeTask funnels into the same path. Routing the resume open-PR
		// case through shipAndFinalize is what fixes the "CI fixed but never
		// merged" stall: doShip awaits CI, runs the fix agent on failure, and
		// merges inside its retry loop — it never returns to task selection
		// (which would strand the in_progress task on an empty bd ready). ──
		var out completeTaskOut
		haveOut := false

		if decision == resumeShipExisting {
			_ = l.taskBackend.ClaimTask(task.id)
			out = l.shipAndFinalize(ctx, completeTaskParams{
				taskID:     task.id,
				nextTask:   task.title,
				workDir:    l.git.GetWorkDir(),
				rawLogPath: logging.ActiveLogPath(l.cfg.Dirs.RalphDir, "raw"),
				notify:     l.cfg.Notify,
				ralphDir:   l.cfg.Dirs.RalphDir,
			})
			haveOut = true
		} else {
			// ── Run agent ──
			agentRun := l.RunIteration(ctx, task, st.runIteration)
			st.lastAction = agentRun.iterAction
			if agentRun.agentInvoked {
				l.consecutiveNoAgentIters = 0
			} else {
				l.consecutiveNoAgentIters++
			}
			skipSignalCheck := false
			if agentRun.action != actionProceed {
				dr := l.dispatchAgentAction(ctx, task, agentRun, st)
				switch dr.action {
				case dispatchContinue:
					continue iterLoop
				case dispatchBreak:
					break iterLoop
				}
				haveOut = dr.haveOut
				out = dr.out
				skipSignalCheck = dr.skipSignalCheck
			}
			st.consecutiveSkipCount = 0

			if skipSignalCheck {
				// completeTask already ran for actionCompactionShip — skip the
				// signal-detection gate to avoid reopening the just-closed task.
			} else if agentRun.result.SignalDetected {
				out = l.completeTask(ctx, completeTaskParams{
					result:     agentRun.result,
					headBefore: agentRun.prep.headBefore,
					workDir:    agentRun.prep.workDir,
					rawLogPath: agentRun.prep.rawLogPath,
					diffStat:   agentRun.diffStat,
					taskID:     task.id,
					nextTask:   task.title,
					notify:     l.cfg.Notify,
					ralphDir:   l.cfg.Dirs.RalphDir,
				})
				haveOut = true
			} else {
				// No completion signal detected after a clean agent run: the
				// iteration ended without shipping (generic Claude failure or
				// end_turn with no signal file). ClaimTask already set
				// status=in_progress at iteration start — release it so the
				// bead returns to bd ready and any local commits remain resumable.
				l.setPhaseInterrupted(task.id)
			}
		}

		// ── Shared aftermath: process the completeTaskOut from either path ──
		switch l.runAftermath(ctx, task, haveOut, out, st) {
		case aftermathContinue:
			continue
		case aftermathBreak:
			break iterLoop
		}
	}

	// Catch-all: if the loop exited due to Ctrl-C and a task was in-flight,
	// set phase=interrupted so the task manager sees it as safe to update.
	if ctx.Err() != nil && st.currentTaskID != "" {
		l.setPhaseInterrupted(st.currentTaskID)
	}

	l.completedTasks = st.sessionTasks
	return nil
}

// doShip executes the full two-phase ship pipeline: push + PR creation (Phase 1),
// then reviewer polling and merge (Phase 2, only when AutoMerge is enabled).
// Retries up to 5 times on review fix requests or CI failures.
//
// pushedBranch is set to the worktree branch name if Phase 1's push succeeded
// (regardless of whether CreatePR then succeeded or failed). An empty
// pushedBranch means nothing was pushed to the remote. shipErr carries the
// Phase 1 push error when push was attempted and failed — callers use this to
// distinguish "push failed" (skip, work in local worktree) from "no commits"
// (safe to close). The pr_creation_failed path is separate: push succeeded
// (pushedBranch != "") but CreatePR returned an error.
// shipOutcome bundles doShip's result. Zero-value fields mean "nothing
// happened on that front" (e.g. prNumber == 0 means no PR was created;
// merged == false with shipErr == nil means a PR is open but not yet merged).
type shipOutcome struct {
	prNumber       int
	prResultURL    string
	merged         bool
	ciFailure      bool
	ciInfraFailure bool
	stacked        bool
	pushedBranch   string
	shipErr        error
}

// callShip invokes git.Ship and layers connectivity and transient-GitHub-error
// retries on top: a dead connection waits for the internet to return before
// retrying once, while a transient GitHub error retries on the configured
// backoff schedule via retry.Retry.
func (l *Loop) callShip(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
	result, err := l.git.Ship(ctx, opts)
	if err != nil {
		if !l.connectivity.IsOnline() {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship failed — internet appears down")
			l.connectivity.WaitForInternet(ctx, l.logger)
			result, err = l.git.Ship(ctx, opts)
		} else if git.IsTransientGitHubError(err) {
			backoffs := l.cfg.ShipRetryBackoffs
			if backoffs == nil {
				backoffs = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}
			}
			// attempted gates the first call: the initial Ship failure
			// that got us into this branch already happened above, so
			// Retry's first fn() call is a passthrough that hands back
			// that known error rather than shipping again immediately.
			attempted := false
			retryErr := retry.Retry(ctx, retry.BackoffOpts{
				Schedule: backoffs,
				OnRetry: func(_ int, delay time.Duration) {
					l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship failed with transient error (%v) — retrying in %s", err, delay)
				},
			}, git.IsTransientGitHubError, func() (bool, error) {
				if !attempted {
					attempted = true
					return false, err
				}
				result, err = l.git.Ship(ctx, opts)
				return err == nil, err
			})
			if retryErr != nil && ctx.Err() != nil {
				return result, err
			}
		}
		if err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship: %v", err)
		}
	}
	return result, err
}

// shipPhase1 pushes the branch and creates the PR (no merge yet), so the push
// lands before reviewer detection runs. It links the task to the PR as soon
// as one exists and kicks off lazy reviewer detection regardless of the
// AutoMerge setting.
func (l *Loop) shipPhase1(ctx context.Context, taskID, title, taskDesc, taskAccept, summary string) (git.ShipResult, error) {
	result, err := l.callShip(ctx, git.ShipOpts{
		TaskID:      taskID,
		TaskTitle:   title,
		Description: taskDesc,
		Acceptance:  taskAccept,
		Summary:     summary,
	})
	if err != nil {
		return result, err
	}

	// Link task to PR as soon as PR is available.
	if result.PRNumber != 0 && taskID != "" {
		ref := result.PRURL
		if ref == "" {
			ref = prURL(l.git.RemoteURL(), result.PRNumber)
		}
		if ref != "" {
			l.logger.Emit(logging.Opts{Domain: logging.Git}, "Linking task %s to %s (branch: %s)", taskID, ref, l.git.GetWorktreeBranch())
			if refErr := l.taskBackend.SetExternalRef(taskID, ref); refErr != nil {
				l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetExternalRef: %v", refErr)
			}
		}
	}

	// Reviewer detection happens after push (lazy, deferred until post-push
	// context is established) regardless of AutoMerge setting.
	l.ensureActiveReviewers(ctx)

	return result, nil
}

// shipPhase2Merge runs the CI/merge retry state machine: it ships with merge
// enabled and, on ReviewFixNeeded, CI failure, or a merge conflict, spawns the
// matching fix agent and retries — up to maxShipRetries — before giving up.
func (l *Loop) shipPhase2Merge(ctx context.Context, taskID, title, workDir, rawLogPath string, phase1 git.ShipResult) shipOutcome {
	prResultNum := phase1.PRNumber
	prResultURL := phase1.PRURL
	pushedBranch := phase1.PushedBranch

	// Seed review-addressed state from persistent store.
	reviewAddressed := make(map[string]bool)
	for _, reviewer := range l.activeReviewers {
		key := "review_addressed:" + reviewer.BotUsername + ":" + taskID
		if v, err := l.state.Read(key); err == nil && v == "true" {
			reviewAddressed[reviewer.BotUsername] = true
		}
	}

	// Retry loop: Ship may return ReviewFixNeeded or CIFailure; fix and retry.
	const maxShipRetries = 5
	infraRetryBackoffs := l.cfg.InfraRetryBackoffs
	if infraRetryBackoffs == nil {
		infraRetryBackoffs = []time.Duration{1 * time.Minute, 2 * time.Minute, 4 * time.Minute}
	}
	infraRetries := 0
	var mergeResult git.ShipResult
	for attempt := 0; attempt < maxShipRetries; attempt++ {
		var mergeErr error
		mergeResult, mergeErr = l.callShip(ctx, git.ShipOpts{
			PRNumber:        prResultNum,
			AutoMerge:       true,
			Reviewers:       l.activeReviewers,
			ReviewAddressed: reviewAddressed,
		})
		if mergeErr != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship (merge): %v", mergeErr)
			return shipOutcome{prNumber: prResultNum, prResultURL: prResultURL, pushedBranch: pushedBranch}
		}
		if mergeResult.ReviewFixNeeded {
			// Review fix needed: spawn fix agent, mark addressed, retry.
			l.tryFixReviewComments(ctx, mergeResult.PendingReviewer, mergeResult.PendingReview, prResultNum, title, workDir, rawLogPath)
			l.markReviewAddressed(taskID, mergeResult.PendingReviewer)
			reviewAddressed[mergeResult.PendingReviewer] = true
			continue
		}
		if mergeResult.CIFailure && mergeResult.CIFailureDetail != nil {
			// Infrastructure failure (zero job steps): CI never actually ran —
			// billing, runner allocation, or a broken workflow file. The work is
			// already verified locally by pre-iteration tests + the pre-push
			// compile check, so there is nothing for a fix agent to do.
			// Close the bead, leave the PR open, and move on — it will merge
			// when CI infrastructure recovers. Spawning an expensive fix agent
			// here wastes tokens and produces a spurious "fix agent" log line
			// that misreads as "tests are failing".
			if mergeResult.InfrastructureFailure {
				l.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn},
					"CI infrastructure failure (zero job steps) — closing bead, PR open for merge when CI recovers")
				return shipOutcome{prNumber: prResultNum, prResultURL: prResultURL, ciFailure: true, ciInfraFailure: true, pushedBranch: pushedBranch}
			}
			// Real CI failure: spawn fix agent; if it pushed new commits, retry merge.
			fixResult := l.tryFixCI(ctx, mergeResult.CIFailureDetail, title, workDir, rawLogPath)
			if fixResult == git.CIFixApplied {
				continue
			}
			// Transient CI failure — re-trigger CI and retry with backoff.
			if fixResult == git.CIFixNoCommits && infraRetries < len(infraRetryBackoffs) {
				delay := infraRetryBackoffs[infraRetries]
				infraRetries++
				l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
					"Transient CI failure — re-triggering and retrying in %s (%d/%d)",
					delay, infraRetries, len(infraRetryBackoffs))
				l.git.EmptyCommit("trigger CI re-run")
				if pushErr := l.git.Push(ctx); pushErr != nil {
					l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
						"Push for CI re-trigger failed: %v", pushErr)
				}
				if retry.Wait(ctx, delay, nil) != nil {
					return shipOutcome{prNumber: prResultNum, prResultURL: prResultURL, pushedBranch: pushedBranch}
				}
				continue
			}
		}
		if mergeResult.ConflictDetail != nil {
			// Conflict fix: spawn fix agent; if it resolved and pushed, retry merge.
			if l.tryFixConflict(ctx, taskID, title, workDir, rawLogPath) {
				continue
			}
			// Agent could not resolve — give up.
			return shipOutcome{prNumber: prResultNum, prResultURL: prResultURL, pushedBranch: pushedBranch}
		}
		return shipOutcome{prNumber: prResultNum, prResultURL: prResultURL, merged: mergeResult.Merged, ciFailure: mergeResult.CIFailure, stacked: mergeResult.Stacked, pushedBranch: pushedBranch}
	}
	return shipOutcome{prNumber: prResultNum, prResultURL: prResultURL, merged: mergeResult.Merged, ciFailure: mergeResult.CIFailure, stacked: mergeResult.Stacked, pushedBranch: pushedBranch}
}

func (l *Loop) doShip(ctx context.Context, taskID, title, summary, rawLogPath, workDir string) shipOutcome {
	// Pre-fetch task description and acceptance criteria so git/github can
	// build the PR body internally. The orchestrator owns the data, the
	// git package owns the markdown formatting.
	var taskDesc, taskAccept string
	if taskID != "" && l.taskBackend != nil {
		taskDesc, _ = l.taskBackend.GetDescription(taskID)
		taskAccept, _ = l.taskBackend.GetAcceptance(taskID)
	}

	result, err := l.shipPhase1(ctx, taskID, title, taskDesc, taskAccept, summary)
	// result.PushedBranch is populated by shipPR when the push leg succeeded,
	// even if CreatePR then returned an error. Thread it through so the
	// caller can distinguish "nothing was pushed" from "push landed on
	// remote but PR creation failed".
	if err != nil {
		return shipOutcome{prNumber: result.PRNumber, prResultURL: result.PRURL, pushedBranch: result.PushedBranch, shipErr: err}
	}

	if !l.cfg.AutoMerge || result.PRNumber == 0 {
		return shipOutcome{prNumber: result.PRNumber, prResultURL: result.PRURL, pushedBranch: result.PushedBranch}
	}

	// Phase 2: Ship with merge enabled. Pass the PR number so Ship
	// skips push+PR and proceeds directly to reviewer poll + merge.
	return l.shipPhase2Merge(ctx, taskID, title, workDir, rawLogPath, result)
}
