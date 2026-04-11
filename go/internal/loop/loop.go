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
// See docs/specs/orchestrator-modules.md for the full rationale.
type Modules struct {
	State       *state.Store
	Git         git.Ops
	TaskBackend tasks.Backend
	Logger      *logging.Logger
	Verifier    *verifier.Verifier
}

// Config holds all parameters needed by the execution loop.
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
	OnRebaseConflict      func(err error) git.RebaseRecovery
	Version               string
	VerifyDir             string // project root where tests are run; empty disables verification
	VerifyModel           string // model for the first LLM verification attempt; defaults to haiku
	VerifyEscalationModel string // model for subsequent LLM verification attempts; defaults to sonnet
	OnIterationStart      func() // called at the start of each iteration (e.g. to regenerate resume script)

	// Attempt limits — overrides package defaults when set.
	MaxPromptAttempts      int
	MaxMergeFailures       int
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

	// hooks for test injection; nil uses the real implementation
	CheckGitHub     func(ctx context.Context) error // startup GitHub reachability check; nil uses real implementation
	OnVerify        func(ctx context.Context, dir, headBefore string) (bool, string)
	OnPostTask      func(ctx context.Context, taskID string, prNumber int, merged bool) // called instead of runPostTask when set
	IsOnline        func() bool
	WaitForInternet func(ctx context.Context, logger *logging.Logger) bool
	OnWait          func()
	NewRunner       func() claudeRunner
	QueryFn         func(ctx context.Context, workDir, prompt, model string) (string, error)
}

// claudeRunner abstracts the Claude execution interface for testability.
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
	attempts          *attempts.Tracker
	logger            *logging.Logger
	signals           claude.SignalPaths
	completedTasks    []CompletedTask
	activeReviewers   []git.Reviewer
	reviewersDetected bool
}

// New creates an execution loop from the given configuration and module
// references. mods is the single composition point where modules from
// many packages meet — see the Modules type comment for the rule that
// carves it out from the "no module types in exported struct fields"
// prohibition.
func New(cfg Config, mods Modules) *Loop {
	st := mods.State
	gm := mods.Git
	logger := mods.Logger
	taskBackend := mods.TaskBackend

	signals := claude.DefaultSignalPaths(cfg.Dirs.RalphDir)

	limiter := ratelimit.New(cfg.Dirs.RalphDir, cfg.CallsPerHour)
	limiter.StopFile = filepath.Join(cfg.Dirs.RalphDir, "stop")

	agentRunner := agent.New(logger)

	if cfg.CheckGitHub == nil {
		cfg.CheckGitHub = checkGitHubConnectivity
	}
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
	if cfg.IsOnline == nil {
		checkTimeout := cfg.ConnectivityCheckTimeout
		cfg.IsOnline = func() bool { return isOnline(checkTimeout) }
	}
	if cfg.WaitForInternet == nil {
		interval := cfg.InternetRestoreInterval
		checkTimeout := cfg.ConnectivityCheckTimeout
		cfg.WaitForInternet = func(ctx context.Context, logger *logging.Logger) bool {
			return waitForInternet(ctx, logger, interval, checkTimeout)
		}
	}
	if cfg.NewRunner == nil {
		cfg.NewRunner = func() claudeRunner { return agent.New(logger) }
	}
	if cfg.QueryFn == nil {
		cfg.QueryFn = agentRunner.Query
	}

	at := attempts.New(cfg.Dirs.RalphDir)
	if cfg.MaxPromptAttempts > 0 {
		at.MaxPromptAttempts = cfg.MaxPromptAttempts
	}
	if cfg.MaxMergeFailures > 0 {
		at.MaxMergeFailures = cfg.MaxMergeFailures
	}
	if cfg.MaxIdleTimeoutFailures > 0 {
		at.MaxIdleTimeoutFailures = cfg.MaxIdleTimeoutFailures
	}
	l := &Loop{
		cfg:         cfg,
		state:       st,
		git:         gm,
		taskBackend: taskBackend,
		limiter:     limiter,
		runner:      agentRunner,
		verifier:    mods.Verifier,
		analyzer:    analyzer.New(),
		attempts:    at,
		logger:      logger,
		signals:     signals,
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

	if err := l.cfg.CheckGitHub(ctx); err != nil {
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

		if l.cfg.OnIterationStart != nil {
			l.cfg.OnIterationStart()
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
	prBody := l.prBody(taskID, summary)

	callShip := func(opts git.ShipOpts) (git.ShipResult, error) {
		result, err := l.git.Ship(ctx, opts)
		if err != nil {
			if !l.cfg.IsOnline() {
				l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship failed — internet appears down")
				l.cfg.WaitForInternet(ctx, l.logger)
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
	result, err := callShip(git.ShipOpts{TaskID: taskID, TaskTitle: title, Body: prBody})
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
		}
		return prResultNum, prResultURL, mergeResult.Merged, mergeResult.CIFailure, mergeResult.Stacked
	}
	return prResultNum, prResultURL, mergeResult.Merged, mergeResult.CIFailure, mergeResult.Stacked
}
