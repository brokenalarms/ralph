package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/verifier"
)

// checkOutcome is the data-only result of one fixCheck evaluation.
type checkOutcome struct {
	Passed  bool
	Failure string
	// Abort marks an infrastructure/tooling fault the check found while
	// validating its own inputs (e.g. an empty diff on a branch that should
	// have one) — not a normal pass/fail verdict on the work itself.
	// runFixLoop returns immediately without consuming an attempt or
	// spawning a fix agent, the same as the signalTimeHead movement guard.
	Abort bool
}

// fixCheck is runFixLoop's local interface seam: a single retryable
// condition (tests passing, compile succeeding, ...). Concrete
// implementations are constructed with their dependencies at DI time —
// no func fields, no callbacks.
type fixCheck interface {
	name() string
	evaluate(ctx context.Context) checkOutcome
}

// fixPlan is the pure-data description of one fix-and-retry cycle: which
// checks must pass, what fix-agent template/vars to spawn on failure, and
// how many attempts to allow before giving up. Sequencing is owned by
// runFixLoop — fixPlan carries only data.
type fixPlan struct {
	checks           []fixCheck
	spawnTemplate    string
	spawnVars        map[string]string
	spawnDescription string
	maxAttempts      int
	// exhaustedFormat is a fmt string taking (maxAttempts, joined check
	// failures), used both for the "giving up" log line and the returned
	// skip reason.
	exhaustedFormat string
	workDir         string
	rawLogPath      string
	// signalTimeHead, when non-empty, is asserted an ancestor of HEAD at
	// the start of every attempt (see runFixLoop's movement guard).
	signalTimeHead string
	logDomain      logging.Domain
}

// runFixLoop is the generic "evaluate checks -> spawn fix agent ->
// re-evaluate" retry cycle shared by every fix-and-retry flow. Sequencing
// (attempt counters, exhaustion, the fix-agent spawn) is owned here;
// plan.checks supply only pass/fail data via their evaluate method.
//
// Before consuming each attempt, runFixLoop asserts plan.signalTimeHead
// (when set) is still an ancestor of HEAD — a worktree reset between
// attempts is an infrastructure failure, not a check rejection, and
// aborts without consuming an attempt.
//
// A check can also report Abort on its outcome — an infrastructure/tooling
// fault found while validating its own inputs, not a rejection of the work.
// Like the movement guard, this returns immediately without consuming an
// attempt or spawning a fix agent.
//
// Returns (true, "") once every check passes, (false, "") when the
// movement guard trips, a check aborts, or a fix agent fails to signal, and
// (false, skipReason) — skipReason formatted from plan.exhaustedFormat —
// once maxAttempts is exhausted.
func (l *Loop) runFixLoop(ctx context.Context, plan fixPlan) (bool, string) {
	attempts := 0
	for {
		if plan.signalTimeHead != "" && !l.git.IsCommitAncestorOf(plan.signalTimeHead, "HEAD") {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
				"Infrastructure error: signal-time commit %s is not an ancestor of HEAD — worktree may have been reset; aborting fix loop without consuming an attempt",
				plan.signalTimeHead)
			return false, ""
		}

		var failures []string
		for _, c := range plan.checks {
			outcome := c.evaluate(ctx)
			if outcome.Abort {
				return false, ""
			}
			if !outcome.Passed {
				failures = append(failures, outcome.Failure)
			}
		}
		if len(failures) == 0 {
			return true, ""
		}

		attempts++
		if attempts > plan.maxAttempts {
			msg := fmt.Sprintf(plan.exhaustedFormat, plan.maxAttempts, strings.Join(failures, "\n\n"))
			l.logger.Emit(logging.Opts{Domain: plan.logDomain, Level: logging.Error}, "%s — giving up", msg)
			return false, msg
		}
		l.logger.Emit(logging.Opts{Domain: plan.logDomain, Level: logging.Warn}, "%s (attempt %d/%d)", plan.spawnDescription, attempts, plan.maxAttempts)

		vars := make(map[string]string, len(plan.spawnVars)+1)
		for k, v := range plan.spawnVars {
			vars[k] = v
		}
		vars["{{TEST_OUTPUT}}"] = strings.Join(failures, "\n\n")

		result := l.verifier.SpawnFixAgent(verifier.FixAgentInput{
			Ctx:         ctx,
			Template:    plan.spawnTemplate,
			Vars:        vars,
			Attempt:     attempts,
			WorkDir:     plan.workDir,
			RawLogPath:  plan.rawLogPath,
			Description: plan.spawnDescription,
		})
		if !result.SignalDetected {
			return false, ""
		}
	}
}
