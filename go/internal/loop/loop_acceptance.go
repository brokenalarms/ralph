package loop

import (
	"context"
	"strconv"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verify"
)

// defaultAcceptanceCountdown matches config's acceptance.countdown_seconds
// default. loop.New falls back to it when a caller (a test, or an embedder
// setting AcceptanceCommand directly) leaves the countdown unset.
const defaultAcceptanceCountdown = 10 * time.Second

// runAcceptanceGate runs the project's ship-time acceptance suite once,
// immediately before push/PR creation. It is the high-fidelity opportunistic
// ceiling above per-iteration verify: machine-seizing suites that can never run
// per iteration run here instead, on every unattended ship.
//
// The countdown defaults to RUN, so an unattended loop always runs the gate. A
// present user can cancel, which records acceptance_skipped metadata on the
// bead and ships anyway.
//
// Returns nil when the ship should proceed (gate disabled, cancelled, or
// passed) and the failure Result when the acceptance command failed — the
// caller routes that into the standard verification-failure path.
func (l *Loop) runAcceptanceGate(ctx context.Context, taskID string) *verify.Result {
	if l.cfg.AcceptanceCommand == "" {
		return nil
	}

	countdown := l.cfg.AcceptanceCountdown
	l.logger.Emit(logging.Opts{Domain: logging.Test}, "Acceptance gate: %s (cancel within %s to skip)", l.cfg.AcceptanceCommand, countdown)

	if notify.AcceptanceCountdown(l.cfg.AcceptanceCommand, countdown) {
		l.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Acceptance cancelled by user — shipping unverified by acceptance")
		if taskID != "" {
			skippedAt := strconv.FormatInt(time.Now().Unix(), 10)
			if err := l.taskBackend.SetMetadata(taskID, tasks.MetadataAcceptanceSkipped, skippedAt); err != nil {
				l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetMetadata(%s): %v", tasks.MetadataAcceptanceSkipped, err)
			}
		}
		return nil
	}

	result := verify.RunAcceptance(ctx, l.cfg.AcceptanceCommand, l.git.GetWorkDir(), l.cfg.TestTimeout)
	if !result.Passed {
		return &result
	}
	l.logger.Emit(logging.Opts{Domain: logging.Test}, "Acceptance passed")
	return nil
}
