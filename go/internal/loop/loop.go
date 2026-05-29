package loop

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/ratelimit"
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
// caller passes a nil Modules.Connectivity, loop.New defaults it to
// liveConnectivity (which uses the package-level checkGitHubConnectivity
// / isOnline / waitForInternet helpers). Tests pass non-nil stub
// implementations to override the live network behavior.
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
	State        *state.Store
	Git          git.Ops
	TaskBackend  tasks.Backend
	Logger       *logging.Logger
	Verifier     *verifier.Verifier
	Runner       claudeRunner   // nil → agent.New(logger)
	Connectivity  Connectivity   // nil → live gh CLI / net checks
	IterationHook IterationHook  // nil → no-op
	PostTaskHook  PostTaskHook   // nil → fallback to runPostTask script path
	WaitHook      WaitHook       // nil → no-op
	VerifyHook    VerifyHook     // nil → fallback to runSimpleVerifyCompletion
	BinaryHasher  BinaryHasher   // nil → reads os.Executable() SHA-256
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
	IdleTimeout           time.Duration
	IdleTimeoutProgress   time.Duration
	MaxRunDuration        time.Duration
	PostSignalTimeout     time.Duration
	PostTask              string
	VerifyBuild           string
	Notify                bool
	Wait                  bool
	Verbose               bool
	Model                 string
	AgentEscalationModel  string // deprecated: no effect; cross-iteration escalation was removed in ralph-pg95
	ModelCap              string // maximum model tier for all LLM calls; empty means no cap
	Version               string
	VerifyDir             string // project root where tests are run; empty disables verification
	Verify                string // when non-empty, used as the verify command instead of detecting ralph:verify scripts
	VerifyModel           string // model for the first LLM verification attempt; defaults to haiku
	VerifyEscalationModel string // model for subsequent LLM verification attempts; defaults to sonnet
	FixModel              string // model for fix agents on attempt 1; defaults to sonnet
	FixEscalationModel    string // model for fix agents on attempt 2+; defaults to opus

	// Attempt limits — overrides package defaults when set.
	MaxPromptAttempts      int
	MaxIdleTimeoutFailures int
	MaxLLMVerifyAttempts   int
	MaxTestFixAttempts     int

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

// liveConnectivity is the production Connectivity implementation. It
// delegates to the package-level helpers in loop_utils.go.
//
// IsOnline and WaitForInternet honor the loop-level cfg.ConnectivityCheckTimeout
// (typically 3 seconds — these are fast local-network reachability pings).
// CheckGitHub uses its own hardcoded 10-second timeout inside
// checkGitHubConnectivity, because the gh api round-trip is a different
// kind of check (auth + GitHub server latency) and the same 3s budget
// would produce false negatives. The two timeouts measure different
// things and should not share a value.
type liveConnectivity struct {
	checkTimeout    time.Duration
	restoreInterval time.Duration
}

func (c *liveConnectivity) CheckGitHub(ctx context.Context) error {
	return checkGitHubConnectivity(ctx)
}
func (c *liveConnectivity) IsOnline() bool { return isOnline(c.checkTimeout) }
func (c *liveConnectivity) WaitForInternet(ctx context.Context, logger *logging.Logger) bool {
	return waitForInternet(ctx, logger, c.restoreInterval, c.checkTimeout)
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
	cfg               Config
	state             *state.Store
	git               git.Ops
	taskBackend       tasks.Backend
	limiter           *ratelimit.Limiter
	runner            claudeRunner
	verifier          *verifier.Verifier
	analyzer          *analyzer.Analyzer
	taskAttempts             []AttemptEvent // in-memory attempt events for the current task, reset per task
	taskIdleTimeouts         int            // consecutive idle timeout count for the current task
	currentTaskID            string         // tracks which task's attempts are in taskAttempts
	consecutiveNoAgentIters  int            // incremented when an iteration ends without invoking the agent; reset on agent invocation
	logger            *logging.Logger
	signals           claude.SignalPaths
	connectivity      Connectivity
	iterationHook     IterationHook
	postTaskHook      PostTaskHook
	waitHook          WaitHook
	verifyHook        VerifyHook
	binaryHasher      BinaryHasher
	startupBinaryHash []byte
	completedTasks    []CompletedTask
	activeReviewers   []git.Reviewer
	reviewersDetected bool
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

	connectivity := mods.Connectivity
	if connectivity == nil {
		connectivity = &liveConnectivity{
			checkTimeout:    cfg.ConnectivityCheckTimeout,
			restoreInterval: cfg.InternetRestoreInterval,
		}
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
		connectivity:  connectivity,
		iterationHook: mods.IterationHook,
		postTaskHook:  mods.PostTaskHook,
		waitHook:      mods.WaitHook,
		verifyHook:    mods.VerifyHook,
		binaryHasher:  bh,
	}
	return l
}

// SessionTasks returns the tasks completed during this session.
func (l *Loop) SessionTasks() []CompletedTask {
	return l.completedTasks
}

// skipTask sets the task back to open in bd, records the reason as a comment,
// and adds the ID to both the backend's in-memory skip set and the state.json
// skipped_tasks list so it stays excluded from future selection.
// Returns (true, haltReason) when the skip is escalated because the task has
// open dependents that would be stranded; (false, "") on a normal successful skip.
func (l *Loop) skipTask(id, reason string) (bool, string) {
	if id == "" {
		return false, ""
	}
	if l.taskBackend != nil {
		deps, err := l.taskBackend.GetOpenDependents(id)
		if err == nil && len(deps) > 0 {
			return true, "skip_would_strand_dependents:" + deps[0]
		}
	}
	l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Skipping task %s: %s", id, reason)
	if err := l.taskBackend.SkipTask(id, reason); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to skip task %s in backend: %v", id, err)
	}
	l.state.ClearCurrentTask()
	if err := l.state.AddSkippedTask(id); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to persist skip for %s: %v", id, err)
	}
	skipped, _ := l.state.GetSkippedTasks()
	l.taskBackend.SetSkippedIDs(skipped)
	return false, ""
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
	agentModel := verify.CapModel(l.cfg.ModelCap, l.cfg.Model)
	l.logger.Emit(logging.Opts{Domain: logging.LLM, Model: agentModel}, "Agent model: %s", agentModel)
	result, runErr := l.runner.Run(claude.RunConfig{
		Ctx:                 ctx,
		WorkDir:             prep.workDir,
		RalphDir:            l.cfg.Dirs.RalphDir,
		Prompt:              prep.fullPrompt,
		TaskID:              task.id,
		RawLog:              prep.rawLogPath,
		LogFile:             filepath.Join(l.cfg.Dirs.EffectiveLogDir(), "loop.log"),
		Verbose:             l.cfg.Verbose,
		Model:               agentModel,
		Signals:             l.signals,
		PollInterval:        2 * time.Second,
		IdleTimeout:         l.cfg.IdleTimeout,
		IdleTimeoutProgress: l.cfg.IdleTimeoutProgress,
		MaxRunDuration:      l.cfg.MaxRunDuration,
		OnSignal: func(summary string) bool {
			// Stop the main runner before spawning fix-agent subprocesses
			// inside runVerifyPipeline. The main agent session is wrapping
			// up anyway (it just signaled completion).
			l.runner.StopStreaming()
			verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
				ctx:        ctx,
				headBefore: prep.headBefore,
				workDir:    prep.workDir,
				rawLogPath: prep.rawLogPath,
				taskID:     task.id,
				nextTask:   task.title,
			})
			if skipReason != "" {
				l.skipTask(task.id, skipReason)
			}
			return verified
		},
		FeedbackFile: filepath.Join(l.cfg.Dirs.RalphDir, "feedback"),
	})

	runAction := l.handleRunResult(ctx, result, runErr, task.id, task.title, prep.headBefore, runIteration)
	if runAction != actionProceed {
		return agentRunResult{action: runAction, agentInvoked: true}
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
	if l.cfg.VerifyDir != "" && verify.DetectTestCommand(l.cfg.Verify, l.cfg.VerifyDir, l.cfg.Dirs.ProjectDir) == nil {
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

	if l.cfg.Evolve {
		if h, err := l.binaryHasher.Hash(); err == nil {
			l.startupBinaryHash = h
		} else {
			l.logger.Emit(logging.Opts{Domain: "loop", Level: logging.Warn}, "Evolve: startup binary hash failed: %v — evolve hash guard disabled", err)
		}
	}

	var runIteration int
	var lastAction analyzer.Action
	var lastTaskMerged bool
	var sessionTasks []CompletedTask
	var currentTaskID string
	var consecutiveSkipCount int
	st, _ := l.state.Load()
	iteration := st.Iteration

iterLoop:
	for {
		// ── Task selection ──
		completedIDs := make(map[string]bool, len(sessionTasks))
		for _, ct := range sessionTasks {
			completedIDs[ct.ID] = true
		}
		task, action, waited := l.selectNextTask(ctx, selectNextTaskParams{
			runIteration:   runIteration,
			maxIterations:  l.cfg.MaxIterations,
			wait:           l.cfg.Wait,
			completedIDs:   completedIDs,
			lastTaskMerged: lastTaskMerged,
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

		runIteration++
		iteration++
		lastTaskMerged = false
		currentTaskID = task.id

		// Retry counters are local variables inside runVerifyPipeline, so
		// they are naturally scoped per-iteration — no reset needed.

		// ── Branch setup ──
		if task.changed || !l.git.IsBranchRenamed() {
			storedBranch, _ := l.taskBackend.GetMetadata(task.id, "branch")
			storedExternalRef, _ := l.taskBackend.GetExternalRef(task.id)
			completedBranches := l.completedBranches()
			branch, err := l.git.BranchForTask(ctx, task.id, task.title, git.BranchTaskMeta{
				Branch:            storedBranch,
				ExternalRef:       storedExternalRef,
				CompletedBranches: completedBranches,
			})
			if err != nil {
				if ctx.Err() != nil {
					l.state.Write("status", "stopped")
					l.setPhaseInterrupted(task.id)
					break
				}
				var transportErr *git.TransportError
				if errors.As(err, &transportErr) {
					skipReason := "transport_error:" + transportErr.Op
					l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
						"Branch setup failed (transient transport error) — skipping task %s: %v", task.id, err)
					l.skipTask(task.id, skipReason)
					l.consecutiveNoAgentIters++
					continue iterLoop
				}
				l.state.Write("status", "error")
				break
			}
			l.state.WriteRunBranch(branch)
			if task.id != "" && branch != "" && strings.Contains(branch, task.id) {
				_ = l.taskBackend.SetMetadata(task.id, "branch", branch)
			}
		}

		if l.iterationHook != nil {
			l.iterationHook.OnIterationStart()
		}

		l.logIterationBanner(logIterationBannerParams{
			version: l.cfg.Version,
		}, runIteration, l.state.ReadMaxIterations(l.cfg.MaxIterations), iteration, task, lastAction)
		l.beginIteration(task, iteration)

		// ── Resume check: does a PR already exist for this task? ──
		branch, _ := l.taskBackend.GetMetadata(task.id, "branch")
		if branch != "" && !strings.Contains(branch, task.id) {
			branch = ""
		}
		externalRef, _ := l.taskBackend.GetExternalRef(task.id)
		if externalRef != "" {
			l.ensureActiveReviewers(ctx)
		}
		resumeResult, resumeErr := l.git.ResumeTask(ctx, git.ResumeTaskMeta{
			TaskID:      task.id,
			TaskTitle:   task.title,
			Branch:      branch,
			ExternalRef: externalRef,
		}, git.ResumeTaskOpts{
			AutoMerge:       l.cfg.AutoMerge,
			Reviewers:       l.activeReviewers,
			ReviewAddressed: l.reviewAddressedForTask(task.id, l.activeReviewers),
		})
		if resumeErr != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "ResumeTask: %v", resumeErr)
		}
		if resumeResult.PRURLToStore != "" && task.id != "" {
			_ = l.taskBackend.SetExternalRef(task.id, resumeResult.PRURLToStore)
		}
		if resumeResult.ClearMetadata && task.id != "" {
			_ = l.taskBackend.SetExternalRef(task.id, "")
			if resumeResult.NewBranch != "" {
				_ = l.taskBackend.SetMetadata(task.id, "branch", resumeResult.NewBranch)
			} else {
				_ = l.taskBackend.SetMetadata(task.id, "branch", "")
			}
		}
		if resumeResult.ShipFailedAfterPush {
			shipErrStr := "unknown Ship error"
			if resumeResult.ShipErr != nil {
				shipErrStr = resumeResult.ShipErr.Error()
			}
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
				"Task %s: Ship failed after pushed commits on branch %s: %s — Manual recovery: retry Ship manually after the gh issue resolves, or close the branch's PR if one was created",
				task.id, branch, shipErrStr)
			l.state.Write("status", "halted_ship_failed_with_pushed_work")
			break iterLoop
		}
		if resumeResult.Handled {
			l.onResumeDone(ctx, task.id, task.title, resumeResult)
			l.git.TagTaskEnd(task.id)
			l.state.WriteRunBranch("")
			l.consecutiveNoAgentIters++
			continue
		}

		// ── Run agent ──
		agentRun := l.RunIteration(ctx, task, runIteration)
		lastAction = agentRun.iterAction
		if agentRun.agentInvoked {
			l.consecutiveNoAgentIters = 0
		} else {
			l.consecutiveNoAgentIters++
		}
		if agentRun.action != actionProceed {
			if agentRun.action == actionRetry {
				continue
			}
			if agentRun.action == actionSkip {
				// Task skipped by analyzer. Track consecutive skips for cascade detection.
				consecutiveSkipCount++
				if consecutiveSkipCount >= 3 {
					haltReason := fmt.Sprintf("cascade_skipped:%d", consecutiveSkipCount)
					l.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Error}, "Halting: %s", haltReason)
					l.state.Write("status", "halted_"+haltReason)
					break iterLoop
				}
				l.state.WriteRunBranch("")
				currentTaskID = ""
				continue iterLoop
			}
			break
		}
		consecutiveSkipCount = 0

		// ── Complete task (post-signal pipeline) ──
		if agentRun.result.SignalDetected {
			out := l.completeTask(ctx, completeTaskParams{
				result:            agentRun.result,
				headBefore:        agentRun.prep.headBefore,
				workDir:           agentRun.prep.workDir,
				rawLogPath:        agentRun.prep.rawLogPath,
				diffStat:          agentRun.diffStat,
				taskID:            task.id,
				nextTask:          task.title,
				postSignalTimeout: l.cfg.PostSignalTimeout,
				notify:            l.cfg.Notify,
				ralphDir:          l.cfg.Dirs.RalphDir,
			})
			if out.ct != nil {
				sessionTasks = append(sessionTasks, *out.ct)
			}
			if out.merged {
				lastTaskMerged = true
			}
			switch out.action {
			case signalRetry:
				continue
			}
			// ── End-of-task: sync + evolve after task completes ──
			// signalSkipped or signalComplete: fire post-task hook and check for
			// binary rebuild. On signalRetry the task is not yet done — skip.
			if ctx.Err() == nil {
				if res := l.postTaskAndMaybeEvolve(ctx, task.id, out.prNumber, out.merged); res == signalEvolve {
					break iterLoop
				}
			}
			if out.action == signalSkipped {
				l.state.WriteRunBranch("")
				continue
			}
			// signalComplete: fall through to tagTaskEnd
		}
		l.git.TagTaskEnd(task.id)
		l.state.WriteRunBranch("")
		currentTaskID = ""
	}

	// Catch-all: if the loop exited due to Ctrl-C and a task was in-flight,
	// set phase=interrupted so the task manager sees it as safe to update.
	if ctx.Err() != nil && currentTaskID != "" {
		l.setPhaseInterrupted(currentTaskID)
	}

	l.completedTasks = sessionTasks
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
func (l *Loop) doShip(ctx context.Context, taskID, title, summary, rawLogPath, workDir string) (prNumber int, prResultURL string, merged bool, ciFailure bool, ciInfraFailure bool, stacked bool, pushedBranch string, shipErr error) {
	// Pre-fetch task description and acceptance criteria so git/github can
	// build the PR body internally. The orchestrator owns the data, the
	// git package owns the markdown formatting.
	var taskDesc, taskAccept string
	if taskID != "" && l.taskBackend != nil {
		taskDesc, _ = l.taskBackend.GetDescription(taskID)
		taskAccept, _ = l.taskBackend.GetAcceptance(taskID)
	}

	callShip := func(opts git.ShipOpts) (git.ShipResult, error) {
		result, err := l.git.Ship(ctx, opts)
		if err != nil {
			if !l.connectivity.IsOnline() {
				l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship failed — internet appears down")
				l.connectivity.WaitForInternet(ctx, l.logger)
				result, err = l.git.Ship(ctx, opts)
			} else if isTransientGitHubError(err) {
				backoffs := l.cfg.ShipRetryBackoffs
				if backoffs == nil {
					backoffs = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}
				}
				for _, delay := range backoffs {
					if err == nil {
						break
					}
					l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship failed with transient error (%v) — retrying in %s", err, delay)
					select {
					case <-ctx.Done():
						return result, err
					case <-time.After(delay):
					}
					result, err = l.git.Ship(ctx, opts)
				}
			}
			if err != nil {
				l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship: %v", err)
			}
		}
		return result, err
	}

	// Phase 1: push + PR (no merge yet) so push happens before reviewer detection.
	result, err := callShip(git.ShipOpts{
		TaskID:      taskID,
		TaskTitle:   title,
		Description: taskDesc,
		Acceptance:  taskAccept,
		Summary:     summary,
	})
	// result.PushedBranch is populated by shipPR when the push leg succeeded,
	// even if CreatePR then returned an error. Thread it through so the
	// caller can distinguish "nothing was pushed" from "push landed on
	// remote but PR creation failed".
	pushedBranch = result.PushedBranch
	if err != nil {
		return result.PRNumber, result.PRURL, false, false, false, false, pushedBranch, err
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

	if !l.cfg.AutoMerge || result.PRNumber == 0 {
		return result.PRNumber, result.PRURL, false, false, false, false, pushedBranch, nil
	}

	// Phase 2: Ship with merge enabled. Pass the PR number so Ship
	// skips push+PR and proceeds directly to reviewer poll + merge.
	prResultNum := result.PRNumber
	prResultURL = result.PRURL

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
		mergeResult, mergeErr = callShip(git.ShipOpts{
			PRNumber:        prResultNum,
			AutoMerge:       true,
			Reviewers:       l.activeReviewers,
			ReviewAddressed: reviewAddressed,
		})
		if mergeErr != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship (merge): %v", mergeErr)
			return prResultNum, prResultURL, false, false, false, false, pushedBranch, nil
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
				return prResultNum, prResultURL, false, true, true, false, pushedBranch, nil
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
				select {
				case <-ctx.Done():
					return prResultNum, prResultURL, false, false, false, false, pushedBranch, nil
				case <-time.After(delay):
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
			return prResultNum, prResultURL, false, false, false, false, pushedBranch, nil
		}
		return prResultNum, prResultURL, mergeResult.Merged, mergeResult.CIFailure, false, mergeResult.Stacked, pushedBranch, nil
	}
	return prResultNum, prResultURL, mergeResult.Merged, mergeResult.CIFailure, false, mergeResult.Stacked, pushedBranch, nil
}
