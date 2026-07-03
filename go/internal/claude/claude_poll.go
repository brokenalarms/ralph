package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// isContentActivity returns true if a raw-log line represents real assistant
// work (text, thinking, tool-use) that should reset the idle watchdog.
// Infrastructure events (rate_limit_event, system, ping, result, error, user
// message echoes) return false. Unparseable/plaintext lines return true so we
// err on the side of keeping the session alive.
func isContentActivity(line string) bool {
	if len(line) == 0 {
		return false
	}
	if line[0] != '{' {
		// Plaintext/stderr — conservative: treat as content activity.
		return true
	}

	var ev struct {
		Type  string `json:"type"`
		Delta *struct {
			Type       string  `json:"type"`
			Text       string  `json:"text"`
			Thinking   string  `json:"thinking"`
			StopReason *string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock *struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Message *struct {
			Role string `json:"role"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		// Unparseable JSON — conservative: treat as content activity.
		return true
	}

	switch ev.Type {
	case "content_block_delta":
		if ev.Delta == nil {
			return false
		}
		// text_delta and thinking_delta are content; input_json_delta is not.
		return ev.Delta.Type == "text_delta" || ev.Delta.Type == "thinking_delta" ||
			ev.Delta.Text != "" || ev.Delta.Thinking != ""
	case "content_block_start":
		if ev.ContentBlock == nil {
			return false
		}
		t := ev.ContentBlock.Type
		return t == "text" || t == "thinking"
	case "message_start":
		return ev.Message != nil && ev.Message.Role == "assistant"
	case "message_delta":
		return ev.Delta != nil && ev.Delta.StopReason != nil && *ev.Delta.StopReason != ""
	case "rate_limit_event", "system", "ping", "result", "error", "user":
		return false
	default:
		// Unknown event types — conservative: treat as content activity.
		return true
	}
}

// parseSystemStatusEvent returns the status value if the line is a system event
// with subtype=status (e.g. compacting, throttled). Returns "" for all other lines.
func parseSystemStatusEvent(line string) string {
	if len(line) == 0 || line[0] != '{' {
		return ""
	}
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}
	if ev.Type == "system" && ev.Subtype == "status" && ev.Status != "" {
		return ev.Status
	}
	return ""
}

// newLinesScan holds results from scanning newly appended raw log lines.
type newLinesScan struct {
	hasActivity      bool
	lastActivityText string // last extracted line of ANY tool/text (incl. verbose tools) — for the liveness heartbeat
	isCompacting     bool
	rateLimit        rateLimitScan
	// System status events (type=system, subtype=status) detected in this scan.
	statusEvents []string
}

// rateLimitEvent is a single parsed rate_limit_event, as returned by
// ParseRateLimitEvent.
type rateLimitEvent struct {
	resetAt        time.Time
	throttled      bool
	warning        bool
	isUsingOverage bool
	rateLimitType  string
	utilization    float64
}

// rateLimitScan accumulates rate_limit_event observations across a single
// scanNewLines call — at most one transition of each kind is kept, first
// occurrence wins.
type rateLimitScan struct {
	throttled             bool
	resetAt               time.Time
	warning               bool
	warningIsUsingOverage bool
	rateLimitType         string
	utilization           float64
	// Populated when a non-throttled, non-warning rate_limit_event is seen (status=allowed).
	allowed               bool
	allowedResetAt        time.Time
	allowedIsUsingOverage bool
	allowedUtilization    float64
	// True when any rate_limit_event in this scan had isUsingOverage=true.
	firstOverage   bool
	overageResetAt time.Time
}

// absorb folds one parsed rate_limit_event into the scan, keeping the
// first occurrence of each transition kind (throttled/warning/allowed/overage).
func (s *rateLimitScan) absorb(ev rateLimitEvent) {
	if ev.throttled && !s.throttled {
		s.throttled = true
		s.resetAt = ev.resetAt
		s.rateLimitType = ev.rateLimitType
		s.utilization = ev.utilization
	}
	if ev.warning && !s.warning && !s.throttled {
		s.warning = true
		s.resetAt = ev.resetAt
		s.rateLimitType = ev.rateLimitType
		s.utilization = ev.utilization
		s.warningIsUsingOverage = ev.isUsingOverage
	}
	if !ev.throttled && !ev.warning && !s.allowed {
		s.allowed = true
		s.allowedResetAt = ev.resetAt
		s.allowedIsUsingOverage = ev.isUsingOverage
		s.allowedUtilization = ev.utilization
	}
	if ev.isUsingOverage && !s.firstOverage {
		s.firstOverage = true
		s.overageResetAt = ev.resetAt
	}
}

// scanNewLines reads new output appended to rawLog since *lastOffset.
// Activity is detected by file growth — immune to scanner buffer limits.
// Rate-limit events are detected by scanning the new lines with a 1MB
// scanner buffer; if an oversized line (e.g. a full file read as a single
// JSON line) exceeds the buffer, the scanner skips to EOF — rate-limit
// events are always small so the skip is safe.
func scanNewLines(rawLog string, lastOffset *int64) newLinesScan {
	info, err := os.Stat(rawLog)
	if err != nil {
		return newLinesScan{}
	}
	size := info.Size()

	grew := size > *lastOffset
	if !grew {
		return newLinesScan{}
	}

	f, err := os.Open(rawLog)
	if err != nil {
		// Can't open but file grew — report activity conservatively.
		*lastOffset = size
		return newLinesScan{hasActivity: true}
	}
	defer f.Close()

	if _, err := f.Seek(*lastOffset, 0); err != nil {
		*lastOffset = size
		return newLinesScan{hasActivity: true}
	}

	var result newLinesScan
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		*lastOffset += int64(len(line)) + 1 // +1 for '\n'
		if !result.hasActivity && isContentActivity(line) {
			result.hasActivity = true
		}
		if text := extractStreamText(line); text != "" {
			result.lastActivityText = firstLine(text)
		}
		resetAt, throttled, warning, isUsingOverage, rateLimitType, utilization, ok := ParseRateLimitEvent(line)
		if ok {
			result.rateLimit.absorb(rateLimitEvent{
				resetAt:        resetAt,
				throttled:      throttled,
				warning:        warning,
				isUsingOverage: isUsingOverage,
				rateLimitType:  rateLimitType,
				utilization:    utilization,
			})
		}
		if status := parseSystemStatusEvent(line); status != "" {
			result.statusEvents = append(result.statusEvents, status)
			if status == "compacting" {
				result.isCompacting = true
			}
		}
	}
	// Scanner errored on oversized line — skip to EOF so we don't re-scan
	// it every tick. Conservatively report activity since we can't parse it.
	if scanner.Err() != nil {
		*lastOffset = size
		result.hasActivity = true
	}
	return result
}

// rateLimitLogger tracks rate-limit warning/allowed/overage transitions
// across poll ticks and emits log lines exactly once per transition.
type rateLimitLogger struct {
	warningLogged           bool
	warningResetsAt         time.Time
	warningOverage          bool
	allowedTransitionLogged bool
	nowOverageLogged        bool
}

// observe logs any rate-limit transitions found in scan. It returns
// (resetAt, true) when scan reports a hard throttle — the caller must kill
// the agent and return Result{RateLimited: true, ResetAt: resetAt}.
func (rl *rateLimitLogger) observe(r *Runner, cfg RunConfig, scan rateLimitScan) (resetAt time.Time, throttled bool) {
	if scan.throttled {
		r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model},
			"Claude rate limit throttled (%s %.0f%%) — resets at %s",
			scan.rateLimitType, scan.utilization*100, scan.resetAt.Format("3:04pm"))
		return scan.resetAt, true
	}
	if scan.warning && !rl.warningLogged {
		rl.warningLogged = true
		rl.warningResetsAt = scan.resetAt
		rl.warningOverage = scan.warningIsUsingOverage
		r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Info, Model: cfg.Model},
			"rate limit warning: %s at %.0f%%, resets at %s",
			scan.rateLimitType, scan.utilization*100, scan.resetAt.Format("3:04pm"))
	}
	if rl.warningLogged {
		if scan.allowed && !rl.allowedTransitionLogged {
			rl.allowedTransitionLogged = true
			if scan.allowedResetAt.Equal(rl.warningResetsAt) {
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Info, Model: cfg.Model},
					"rate limit: allowed back in at %.0f%%, original reset confirmed",
					scan.allowedUtilization*100)
			} else {
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model},
					"rate limit: consuming extended usage — original reset %s has advanced to %s",
					rl.warningResetsAt.Format("3:04pm"), scan.allowedResetAt.Format("3:04pm"))
			}
		}
		if scan.firstOverage && !rl.warningOverage && !rl.nowOverageLogged {
			rl.nowOverageLogged = true
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model},
				"rate limit: now using overage capacity (overage resets at %s)",
				scan.overageResetAt.Format("3:04pm"))
		}
	}
	return time.Time{}, false
}

// clampHeartbeatInterval shortens the configured heartbeat interval so a
// heartbeat always precedes any idle-timeout kill.
func clampHeartbeatInterval(cfg RunConfig) time.Duration {
	heartbeatInterval := cfg.Timeouts.Heartbeat
	if heartbeatInterval > 0 && cfg.Timeouts.Idle > 0 {
		minIdleTimeout := cfg.Timeouts.Idle
		if cfg.Timeouts.IdleProgress > 0 && cfg.Timeouts.IdleProgress < minIdleTimeout {
			minIdleTimeout = cfg.Timeouts.IdleProgress
		}
		if heartbeatInterval >= minIdleTimeout {
			heartbeatInterval = minIdleTimeout * 3 / 4
			if heartbeatInterval < cfg.PollInterval {
				heartbeatInterval = cfg.PollInterval
			}
		}
	}
	return heartbeatInterval
}

// checkSignals inspects signal files for one poll tick: task pickup, user
// feedback (checked first — a deliberate human override), no-code-needed,
// and completion. Any signal that ends the run performs the graceful kill
// itself. Returns the terminal Result and done=true when poll should
// return immediately. When done is false, retryTick=true means a
// completion signal was rejected by OnSignal verification (already
// cleared) and poll should skip straight to the next tick without running
// heartbeat/timeout checks this iteration — matching the rest of the
// signal-detection block, which never reaches those checks either.
func (r *Runner) checkSignals(cfg RunConfig, cmd *exec.Cmd, processDone chan struct{}, taskLogged *bool) (result Result, done bool, retryTick bool) {
	// Detect task pickup — the stream formatter already emits
	// "[signal] current_task: ..." in real-time, so we only set the
	// flag and fire the callback here, no duplicate log line.
	if !*taskLogged && hasSignal(cfg.Signals.CurrentTask) {
		desc := readFirstLine(cfg.Signals.CurrentTask)
		if desc != "" {
			*taskLogged = true
			if r.OnTaskDetected != nil {
				r.OnTaskDetected(desc)
			}
		}
	}

	// Check for user feedback signal FIRST — it's a deliberate
	// human override and must take priority over completion signals.
	// Content is already on the bead via bd update --append-notes.
	if cfg.FeedbackFile != "" {
		if _, err := os.Stat(cfg.FeedbackFile); err == nil {
			os.Remove(cfg.FeedbackFile)
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Feedback signal detected — restarting agent")
			gracefulKill(cmd, processDone)
			return Result{FeedbackKill: true}, true, false
		}
	}

	// Detect no-code-needed signal: agent confirmed no code changes
	// required (bug already fixed, not a bug, etc.). Bypasses
	// OnSignal verification — no commit check applies.
	if cfg.Signals.NoCodeNeeded != "" && hasSignal(cfg.Signals.NoCodeNeeded) {
		summary := readFirstLine(cfg.Signals.NoCodeNeeded)
		if summary == "" {
			summary = "confirmed no code changes needed"
		}
		if cfg.TaskID != "" {
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "%s: %s", cfg.TaskID, summary)
		} else {
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "%s", summary)
		}
		waitForOutputSettle(cfg.RawLog, processDone)
		gracefulKill(cmd, processDone)
		return Result{
			SignalDetected: true,
			NoCodeNeeded:   true,
			TaskDesc:       readFirstLine(cfg.Signals.CurrentTask),
			Summary:        summary,
		}, true, false
	}

	// Detect completion.
	if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
		summary := readSignalSummary(cfg.Signals)
		if summary == "" {
			summary = "task done"
		}
		if cfg.TaskID != "" {
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "%s completed: %s", cfg.TaskID, summary)
		} else {
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: cfg.Model}, "Completed: %s", summary)
		}

		// If OnSignal is set, let the orchestrator verify before accepting.
		// Run in a goroutine so we can still detect feedback during
		// long-running verification (tests, LLM review, fix agents).
		if cfg.OnSignal != nil {
			accepted := r.runOnSignalWithFeedbackWatch(cfg, cmd, processDone, summary)
			if accepted == feedbackInterrupt {
				return Result{FeedbackKill: true}, true, false
			}
			if accepted == signalRejected {
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Verification rejected signal — agent continues")
				clearSignals(cfg.Signals)
				return Result{}, false, true
			}
		}

		// Wait for the agent to finish its final output before killing.
		// The signal file is often written before the agent's completion
		// message appears in the log; killing immediately truncates it.
		waitForOutputSettle(cfg.RawLog, processDone)

		gracefulKill(cmd, processDone)

		return Result{
			SignalDetected: true,
			AllComplete:    hasSignal(cfg.Signals.AllComplete),
			OnSignalUsed:   cfg.OnSignal != nil,
			TaskDesc:       readFirstLine(cfg.Signals.CurrentTask),
			Summary:        summary,
		}, true, false
	}

	return Result{}, false, false
}

// emitHeartbeat logs a liveness line when the agent has been quiet longer
// than heartbeatInterval, then resets *lastEmit. Reports only known facts —
// the process is alive, how long the raw log has been silent, how long
// until the idle-kill fires, and the last observed activity — never that
// the agent is making useful progress: silence looks identical whether the
// agent is thinking or hung. checkTimeouts is what adjudicates a genuinely
// stuck agent.
func (r *Runner) emitHeartbeat(cfg RunConfig, heartbeatInterval time.Duration, lastActivity time.Time, lastEmit *time.Time, activitySeen bool, latestActivity string) {
	if heartbeatInterval <= 0 || time.Since(*lastEmit) < heartbeatInterval {
		return
	}
	quiet := time.Since(lastActivity).Truncate(time.Second)
	msg := fmt.Sprintf("Agent alive, no output for %s", quiet)
	if cfg.Timeouts.Idle > 0 {
		idleThreshold := cfg.Timeouts.Idle
		if cfg.Timeouts.IdleProgress > 0 && activitySeen {
			idleThreshold = cfg.Timeouts.IdleProgress
		}
		if killIn := (idleThreshold - time.Since(lastActivity)).Truncate(time.Second); killIn > 0 {
			msg += fmt.Sprintf(" (idle-kill in %s)", killIn)
		}
	}
	if latestActivity != "" {
		msg += " — last: " + latestActivity
	}
	r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "%s", msg)
	*lastEmit = time.Now()
}

// checkTimeouts evaluates the idle and wall-clock timeouts for one poll
// tick, killing the agent and returning (Result, true) when either fires.
// Returns (Result{}, false) when neither timeout has elapsed.
func (r *Runner) checkTimeouts(cfg RunConfig, cmd *exec.Cmd, processDone chan struct{}, runStart, lastActivity time.Time, activitySeen bool) (Result, bool) {
	if cfg.Timeouts.Idle > 0 {
		timeout := cfg.Timeouts.Idle
		if cfg.Timeouts.IdleProgress > 0 && activitySeen {
			timeout = cfg.Timeouts.IdleProgress
		}
		idle := time.Since(lastActivity)
		if idle >= timeout {
			r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Idle timeout (%s with no output) — killing session", timeout)
			gracefulKill(cmd, processDone)
			return Result{IdleTimeout: true}, true
		}
	}

	if cfg.Timeouts.MaxRun > 0 && time.Since(runStart) >= cfg.Timeouts.MaxRun {
		r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model}, "Wall-clock timeout (%s max run duration) — killing session", cfg.Timeouts.MaxRun)
		gracefulKill(cmd, processDone)
		return Result{WallClockTimeout: true}, true
	}

	return Result{}, false
}

// poll checks for process exit and signal files on a ticker.
func (r *Runner) poll(cmd *exec.Cmd, cfg RunConfig) Result {
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	taskLogged := false
	activitySeen := false
	lastActivity := time.Now()
	lastEmit := time.Now()
	var latestActivity string
	runStart := time.Now()
	processDone := make(chan struct{})
	var rlLogger rateLimitLogger

	heartbeatInterval := clampHeartbeatInterval(cfg)

	// Seed the log offset so we only scan lines written during this session.
	var logOffset int64
	if cfg.RawLog != "" {
		if info, err := os.Stat(cfg.RawLog); err == nil {
			logOffset = info.Size()
		}
	}

	// Watch for process exit in a goroutine so we don't block on ticker.
	go func() {
		_ = cmd.Wait()
		close(processDone)
	}()

	for {
		select {
		case <-processDone:
			// Process exited — check for feedback signal before returning.
			if cfg.FeedbackFile != "" {
				if _, err := os.Stat(cfg.FeedbackFile); err == nil {
					os.Remove(cfg.FeedbackFile)
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Feedback signal detected — restarting agent")
					return Result{FeedbackKill: true}
				}
			}
			return Result{}

		case <-cfg.Ctx.Done():
			gracefulKill(cmd, processDone)
			return Result{}

		case <-ticker.C:
			// Check if process already exited (channel may not fire instantly).
			if !processAlive(cmd) {
				<-processDone
				if cfg.FeedbackFile != "" {
					if _, err := os.Stat(cfg.FeedbackFile); err == nil {
						os.Remove(cfg.FeedbackFile)
						r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Feedback signal detected — restarting agent")
						return Result{FeedbackKill: true}
					}
				}
				return Result{}
			}

			// Scan new log lines for content activity and rate limit events.
			if cfg.RawLog != "" {
				scan := scanNewLines(cfg.RawLog, &logOffset)
				if scan.hasActivity {
					lastActivity = time.Now()
					lastEmit = time.Now()
					activitySeen = true
				}
				if scan.lastActivityText != "" {
					latestActivity = scan.lastActivityText
				}
				for _, status := range scan.statusEvents {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model},
						"Claude system status: %s at %s", status, time.Since(runStart).Round(time.Millisecond))
				}
				if scan.isCompacting {
					r.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: cfg.Model},
						"Compaction event detected despite DISABLE_AUTO_COMPACT — killing agent (context limit exceeded, task too big)")
					gracefulKill(cmd, processDone)
					return Result{Compacted: true}
				}
				if resetAt, throttled := rlLogger.observe(r, cfg, scan.rateLimit); throttled {
					gracefulKill(cmd, processDone)
					return Result{RateLimited: true, ResetAt: resetAt}
				}
			}

			if res, done, retryTick := r.checkSignals(cfg, cmd, processDone, &taskLogged); done {
				return res
			} else if retryTick {
				continue
			}

			r.emitHeartbeat(cfg, heartbeatInterval, lastActivity, &lastEmit, activitySeen, latestActivity)

			if res, handled := r.checkTimeouts(cfg, cmd, processDone, runStart, lastActivity, activitySeen); handled {
				return res
			}
		}
	}
}

// onSignalOutcome describes the result of running OnSignal with feedback monitoring.
type onSignalOutcome int

const (
	signalAccepted    onSignalOutcome = iota // OnSignal returned true
	signalRejected                           // OnSignal returned false
	feedbackInterrupt                        // feedback file appeared during OnSignal
)

// runOnSignalWithFeedbackWatch runs the OnSignal callback in a goroutine while
// polling for feedback. If a feedback file appears while OnSignal is executing
// (e.g. during test suite, LLM verification, or fix agent execution), the
// agent is killed immediately and feedbackInterrupt is returned.
func (r *Runner) runOnSignalWithFeedbackWatch(cfg RunConfig, cmd *exec.Cmd, processDone <-chan struct{}, summary string) onSignalOutcome {
	if cfg.FeedbackFile == "" {
		if cfg.OnSignal(summary) {
			return signalAccepted
		}
		return signalRejected
	}

	done := make(chan bool, 1)
	go func() {
		done <- cfg.OnSignal(summary)
	}()

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case accepted := <-done:
			if accepted {
				return signalAccepted
			}
			return signalRejected
		case <-ticker.C:
			if _, err := os.Stat(cfg.FeedbackFile); err == nil {
				os.Remove(cfg.FeedbackFile)
				r.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: cfg.Model}, "Feedback signal detected — restarting agent")
				gracefulKill(cmd, processDone)
				return feedbackInterrupt
			}
		}
	}
}
