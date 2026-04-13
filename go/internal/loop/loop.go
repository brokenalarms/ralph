package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/attempts"
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
	Querier      querier        // nil → agent.New(logger); one-shot LLM queries
	Connectivity Connectivity   // nil → live gh CLI / net checks
	IterationHook IterationHook // nil → no-op
	PostTaskHook PostTaskHook   // nil → fallback to runPostTask script path
	WaitHook     WaitHook       // nil → no-op
	VerifyHook   VerifyHook     // nil → fallback to runSimpleVerifyCompletion
}

// Config holds all parameters needed by the execution loop. Pure data —
// no function fields, no module references. The behavioral injection
// points (Connectivity, hooks, runner) live on Modules.
type Config struct {
	Dirs                  workctx.WorkContext
	PlanFile              string
	MaxIterations         int
	Refactor              bool
	Quiet                 bool
	AutoMerge             bool
	Evolve                bool
	CallsPerHour          int
	IdleTimeout           time.Duration
	IdleTimeoutProgress   time.Duration
	PostSignalTimeout     time.Duration
	PostTask              string
	VerifyBuild           string
	Notify                bool
	Wait                  bool
	Verbose               bool
	Model                 string
	AgentEscalationModel  string // model for agent on retry attempts; defaults to opus
	ModelCap              string // maximum model tier for all LLM calls; empty means no cap
	Version               string
	VerifyDir             string // project root where tests are run; empty disables verification
	VerifyModel           string // model for the first LLM verification attempt; defaults to haiku
	VerifyEscalationModel string // model for subsequent LLM verification attempts; defaults to sonnet

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
// Run/StopStreaming/InjectMessage drive the interactive agent — this
// interface does NOT include one-shot queries, which go through querier.
type claudeRunner interface {
	Run(cfg claude.RunConfig) (claude.Result, error)
	StopStreaming()
	InjectMessage(msg string) error
}

// querier abstracts one-shot LLM queries (no streaming, no signal polling).
// Used by the refactor-decision helper. The verifier defines its own
// identical Querier interface — in production both are satisfied by
// *agent.Runner, but they are injected separately.
type querier interface {
	Query(ctx context.Context, workDir, prompt, model string, allowedTools []string) (string, error)
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
	querier           querier
	verifier          *verifier.Verifier
	analyzer          *analyzer.Analyzer
	attempts          *attempts.Tracker
	logger            *logging.Logger
	signals           claude.SignalPaths
	connectivity      Connectivity
	iterationHook     IterationHook
	postTaskHook      PostTaskHook
	waitHook          WaitHook
	verifyHook        VerifyHook
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
		runner = agent.New(logger)
	}

	q := mods.Querier
	if q == nil {
		q = agent.New(logger)
	}

	at := attempts.New(attempts.Config{
		RalphDir:               cfg.Dirs.RalphDir,
		MaxPromptAttempts:      cfg.MaxPromptAttempts,
		MaxIdleTimeoutFailures: cfg.MaxIdleTimeoutFailures,
	})
	l := &Loop{
		cfg:           cfg,
		state:         st,
		git:           gm,
		taskBackend:   taskBackend,
		limiter:       limiter,
		runner:        runner,
		querier:       q,
		verifier:      mods.Verifier,
		analyzer:      analyzer.New(),
		attempts:      at,
		logger:        logger,
		signals:       signals,
		connectivity:  connectivity,
		iterationHook: mods.IterationHook,
		postTaskHook:  mods.PostTaskHook,
		waitHook:      mods.WaitHook,
		verifyHook:    mods.VerifyHook,
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
func (l *Loop) skipTask(id, reason string) {
	if id == "" {
		return
	}
	l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Skipping task %s: %s", id, reason)
	if err := l.taskBackend.SkipTask(id, reason); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to skip task %s in backend: %v", id, err)
	}
	if err := l.state.AddSkippedTask(id); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to persist skip for %s: %v", id, err)
	}
	skipped, _ := l.state.GetSkippedTasks()
	l.taskBackend.SetSkippedIDs(skipped)
}

// ensureActiveReviewers populates l.activeReviewers on first call. Subsequent
// calls are no-ops. The loop is single-threaded so no synchronization is needed.
func (l *Loop) ensureActiveReviewers() {
	if l.reviewersDetected {
		return
	}
	l.reviewersDetected = true
	reviewers, err := l.git.DetectActiveReviewers()
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
	prep       iterationPrompt
	result     claude.Result
	iterAction analyzer.Action
	diffStat   string
	action     loopAction // actionDone or actionRetry if short-circuited; actionProceed otherwise
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
	prep, ok := l.prepareAndBuildPrompt(ctx, task.id, task.title)
	if !ok {
		return agentRunResult{action: actionDone}
	}

	taskStart := time.Now()
	agentModel := l.cfg.Model
	if l.attempts.Count(task.id, task.title) > 0 {
		agentModel = l.cfg.AgentEscalationModel
	}
	agentModel = verify.CapModel(l.cfg.ModelCap, agentModel)
	l.logger.Emit(logging.Opts{Domain: logging.LLM, Model: agentModel}, "Agent model: %s", agentModel)
	result, runErr := l.runner.Run(claude.RunConfig{
		Ctx:                 ctx,
		WorkDir:             prep.workDir,
		RalphDir:            l.cfg.Dirs.RalphDir,
		Prompt:              prep.fullPrompt,
		TaskID:              task.id,
		RawLog:              prep.rawLogPath,
		LogFile:             filepath.Join(l.cfg.Dirs.RalphDir, "loop.log"),
		Quiet:               l.cfg.Quiet,
		Verbose:             l.cfg.Verbose,
		Model:               agentModel,
		Signals:             l.signals,
		PollInterval:        2 * time.Second,
		IdleTimeout:         l.cfg.IdleTimeout,
		IdleTimeoutProgress: l.cfg.IdleTimeoutProgress,
		HasProgress: func() bool {
			if l.git.HeadRev() != prep.headBefore {
				return true
			}
			if prep.diffBefore {
				return false
			}
			return l.git.HasDiff()
		},
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
		return agentRunResult{action: runAction}
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
		return agentRunResult{action: actionDone, iterAction: iterAction}
	}

	return agentRunResult{
		prep:       prep,
		result:     result,
		iterAction: iterAction,
		diffStat:   diffStat,
		action:     actionProceed,
	}
}

// Run executes the full iteration loop. Returns nil on normal completion
// (all tasks done, max iterations reached, or stopped). Returns an error
// for unrecoverable failures.
func (l *Loop) Run(ctx context.Context) error {
	if l.cfg.VerifyDir != "" && verify.DetectTestCommand(l.cfg.VerifyDir, l.cfg.Dirs.ProjectDir) == nil {
		return fmt.Errorf("no ralph:verify script found in %s — add a \"ralph:verify\" script to package.json (or a make ralph-verify target) so the loop can verify task completion", l.cfg.Dirs.ProjectDir)
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

	var runIteration int
	var lastAction analyzer.Action
	var lastTaskMerged bool
	var sessionTasks []CompletedTask
	st, _ := l.state.Load()
	iteration := st.Iteration

iterLoop:
	for {
		// ── Task selection ──
		completedIDs := make(map[string]bool, len(sessionTasks))
		for _, ct := range sessionTasks {
			completedIDs[ct.ID] = true
		}
		task, action := l.selectNextTask(ctx, selectNextTaskParams{
			runIteration:   runIteration,
			maxIterations:  l.cfg.MaxIterations,
			wait:           l.cfg.Wait,
			completedIDs:   completedIDs,
			lastTaskMerged: lastTaskMerged,
		})
		if action == actionDone {
			break
		}

		runIteration++
		iteration++
		lastTaskMerged = false

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
				} else {
					l.state.Write("status", "error")
				}
				break
			}
			l.state.WriteRunBranch(branch)
			if task.id != "" && branch != "" && strings.Contains(branch, task.id) {
				_ = l.taskBackend.SetMetadata(task.id, "branch", branch)
			}
		}

		if err := l.maybeRefactor(ctx, len(sessionTasks)); err != nil {
			l.logger.Emit(logging.Opts{Level: logging.Warn}, "Refactor iteration error: %v", err)
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
			l.ensureActiveReviewers()
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
		if resumeResult.Handled {
			l.onResumeDone(ctx, task.id, task.title, resumeResult)
			l.git.TagTaskEnd(task.id)
			continue
		}

		// ── Run agent ──
		agentRun := l.runAgent(ctx, task, runIteration)
		lastAction = agentRun.iterAction
		if agentRun.action != actionProceed {
			if agentRun.action == actionRetry {
				continue
			}
			break
		}

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
				evolve:            l.cfg.Evolve,
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
			case signalRetry, signalSkipped:
				continue
			case signalEvolve:
				break iterLoop
			}
			// signalComplete: fall through to tagTaskEnd
		}
		l.git.TagTaskEnd(task.id)
	}

	l.completedTasks = sessionTasks
	return nil
}

// doShip executes the full two-phase ship pipeline: push + PR creation (Phase 1),
// then reviewer polling and merge (Phase 2, only when AutoMerge is enabled).
// Retries up to 5 times on review fix requests or CI failures.
func (l *Loop) doShip(ctx context.Context, taskID, title, summary, rawLogPath, workDir string) (prNumber int, prResultURL string, merged bool, ciFailure bool, stacked bool) {
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
	if err != nil {
		return result.PRNumber, result.PRURL, false, false, false
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
	l.ensureActiveReviewers()

	if !l.cfg.AutoMerge || result.PRNumber == 0 {
		return result.PRNumber, result.PRURL, false, false, false
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
			return prResultNum, prResultURL, false, false, false
		}
		if mergeResult.ReviewFixNeeded {
			// Review fix needed: spawn fix agent, mark addressed, retry.
			l.tryFixReviewComments(ctx, mergeResult.PendingReviewer, mergeResult.PendingReview, prResultNum, title, workDir, rawLogPath)
			l.markReviewAddressed(taskID, mergeResult.PendingReviewer)
			reviewAddressed[mergeResult.PendingReviewer] = true
			continue
		}
		if mergeResult.CIFailure && mergeResult.CIFailureDetail != nil {
			// CI fix: spawn fix agent; if it pushed new commits, retry merge.
			fixResult := l.tryFixCI(ctx, mergeResult.CIFailureDetail, title, workDir, rawLogPath)
			if fixResult == git.CIFixApplied {
				continue
			}
			// Transient infrastructure failure — re-trigger CI and retry with backoff.
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
					return prResultNum, prResultURL, false, false, false
				case <-time.After(delay):
				}
				continue
			}
		}
		return prResultNum, prResultURL, mergeResult.Merged, mergeResult.CIFailure, mergeResult.Stacked
	}
	return prResultNum, prResultURL, mergeResult.Merged, mergeResult.CIFailure, mergeResult.Stacked
}
