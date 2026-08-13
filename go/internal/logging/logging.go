package logging

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/brokenalarms/ralph/internal/config"
)

// Hyperlink returns an OSC 8 terminal hyperlink that renders visible as
// the given text but links to url when clicked. Terminals that don't
// support OSC 8 show the visible text unaltered.
func Hyperlink(url, visible string) string {
	return fmt.Sprintf("\033]8;;%s\033\\%s\033]8;;\033\\", url, visible)
}

// PRLink returns "PR #N" formatted as a clickable OSC 8 hyperlink
// pointing to the GitHub PR URL. If nwo (owner/repo) is empty, returns
// plain "PR #N" without a link.
func PRLink(nwo string, prNumber int) string {
	if nwo == "" || prNumber == 0 {
		if prNumber != 0 {
			return fmt.Sprintf("PR #%d", prNumber)
		}
		return ""
	}
	url := fmt.Sprintf("https://github.com/%s/pull/%d", nwo, prNumber)
	return Hyperlink(url, fmt.Sprintf("PR #%d", prNumber))
}

// PRLinkOpt returns a *Link for use in Opts.Link, pointing to the GitHub
// PR URL. If either argument is zero/empty, returns nil.
func PRLinkOpt(nwo string, prNumber int) *Link {
	if nwo == "" || prNumber == 0 {
		return nil
	}
	return &Link{
		Text: fmt.Sprintf("PR #%d", prNumber),
		URL:  fmt.Sprintf("https://github.com/%s/pull/%d", nwo, prNumber),
	}
}

// ANSI color codes.
const (
	Red        = "\033[0;31m"
	Green      = "\033[0;32m"
	Yellow     = "\033[0;33m"
	Blue       = "\033[0;34m"
	Magenta    = "\033[0;35m"
	Cyan       = "\033[0;36m"
	BrightBlue = "\033[0;94m"
	Orange     = "\033[0;38;5;208m"
	Dim        = "\033[2;37m"
	Bold       = "\033[1m"
	Reset      = "\033[0m"
)

// Actor identifies the source of a log message.
type Actor string

const (
	Orch       Actor = "o" // orchestrator
	AgentActor Actor = "r" // ralph/agent
)

// Domain categorizes what a log message is about.
type Domain string

const (
	Git      Domain = "git"
	CI       Domain = "ci"
	Beads    Domain = "beads"
	Test     Domain = "test"
	Build    Domain = "build"
	LLM      Domain = "llm"
	Shell    Domain = "bash"
	Analyzer Domain = "analyzer"
	Loop     Domain = "loop"
	PostTask Domain = "post-task"
	Review   Domain = "review"
	State    Domain = "state"
)

// Tag formats [actor][domain] with ANSI color. If domain is empty,
// only [actor] is emitted. Greppable by actor or domain independently.
func Tag(color string, actor Actor, domain Domain) string {
	if domain == "" {
		return fmt.Sprintf("%s[%s]%s", color, actor, Reset)
	}
	return fmt.Sprintf("%s[%s][%s]%s", color, actor, domain, Reset)
}

// BranchTag formats a branch name as a colored tag for log messages,
// e.g. "[main]" in green.
func BranchTag(branch string) string {
	return Green + "[" + branch + "]" + Reset
}

// Level controls the severity and color of a log message.
type Level int

const (
	Info    Level = iota // cyan
	Success              // green
	Warn                 // yellow
	Error                // red
	Debug                // dim — low-priority detail, not surfaced by default callers
)

func (lv Level) color() string {
	switch lv {
	case Success:
		return Green
	case Warn:
		return Yellow
	case Error:
		return Red
	case Debug:
		return Dim
	default:
		return Cyan
	}
}

// Link is a clickable reference appended to a log line.
type Link struct {
	Text string // visible text, e.g. "PR #42"
	URL  string // click target, e.g. "https://github.com/owner/repo/pull/42"
}

// Opts is the structured parameter for Emit. All log context is a field.
type Opts struct {
	Domain Domain
	Level  Level
	Link   *Link  // clickable reference appended at end of line
	Branch string // appended as colored tag
	Model  string // when set, appended as a color-coded [model] sub-tag after the domain tag
	Append bool   // when true, omit trailing newline
}

// ModelTag returns a color-coded [model-short-name] sub-tag for the given model ID.
// "claude-haiku-…" → bright-blue [haiku], "claude-sonnet-…" → yellow [sonnet],
// "claude-opus-…" → magenta [opus].
func ModelTag(model string) string {
	short := modelShortName(model)
	var color string
	switch short {
	case "haiku":
		color = BrightBlue
	case "sonnet":
		color = Yellow
	case "opus":
		color = Magenta
	case "fable":
		color = Green
	default:
		color = Cyan
	}
	return fmt.Sprintf("%s[%s]%s", color, short, Reset)
}

func modelShortName(model string) string {
	return config.ModelShortName(model)
}

// Logger provides colored logging with trailing timestamps that appear
// only when the second changes from the previous line.
type Logger struct {
	out       io.Writer
	logFile   io.Writer
	streaming bool
	Fmt       LineFormatter
}

// New creates a Logger that writes to stdout and the given log file writer.
// If logFile is nil, file logging is disabled.
func New(logFile io.Writer) *Logger {
	if logFile == nil {
		logFile = io.Discard
	}
	return &Logger{
		out:     os.Stdout,
		logFile: logFile,
	}
}

// NewWithWriter creates a Logger that writes to the given writer instead of
// stdout, useful for capturing output in tests.
func NewWithWriter(w io.Writer) *Logger {
	return &Logger{
		out:     w,
		logFile: io.Discard,
	}
}

// Emit writes a log message with structured options. When Append is true,
// the trailing newline is omitted — use this for partial-line assembly.
func (l *Logger) Emit(o Opts, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if o.Link != nil {
		if o.Link.URL != "" {
			msg += "  (" + Hyperlink(o.Link.URL, o.Link.Text) + ")"
		} else if o.Link.Text != "" {
			msg += "  (" + o.Link.Text + ")"
		}
	}
	if o.Branch != "" {
		msg += "  " + BranchTag(o.Branch)
	}

	var content string
	if o.Domain == "" && o.Level == 0 && o.Model == "" {
		content = msg
	} else {
		tag := Tag(o.Level.color(), Orch, o.Domain)
		if o.Model != "" {
			tag += ModelTag(o.Model)
		}
		content = fmt.Sprintf("%s %s", tag, msg)
	}

	if o.Append {
		l.write(content)
	} else {
		l.write(l.Fmt.Format(content) + "\n")
	}
}

// SetStreaming enables or disables streaming mode. In streaming mode, the
// logger writes only to the log file — stdout is handled by a single tail
// goroutine to prevent duplicate output.
func (l *Logger) SetStreaming(on bool) {
	l.streaming = on
}

// write outputs a pre-formatted string to both stdout and logFile,
// respecting streaming mode. All log output flows through this method.
func (l *Logger) write(s string) {
	if !l.streaming {
		fmt.Fprint(l.out, s)
	}
	fmt.Fprint(l.logFile, s)
}

// EmitInPlace delegates to Emit with Append: true.
func (l *Logger) EmitInPlace(o Opts, format string, args ...any) {
	o.Append = true
	l.Emit(o, format, args...)
}

// EmitAppend delegates to Emit with Append: true and no tag fields.
func (l *Logger) EmitAppend(format string, args ...any) {
	l.Emit(Opts{Append: true}, format, args...)
}

// EmitFinalInPlace writes a trailing newline.
func (l *Logger) EmitFinalInPlace() {
	l.write("\n")
}

// AgentLog writes an info-level message with [r] actor prefix.
func (l *Logger) AgentLog(domain Domain, format string, args ...any) {
	tag := Tag(Cyan, AgentActor, domain)
	content := fmt.Sprintf("%s %s", tag, fmt.Sprintf(format, args...))
	l.write(l.Fmt.Format(content) + "\n")
}

// Phase writes a bold blue phase header.
func (l *Logger) Phase(format string, args ...any) {
	l.PhaseColor(Blue, format, args...)
}

// PhaseColor writes a bold phase header in the given ANSI color.
func (l *Logger) PhaseColor(color string, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	content := fmt.Sprintf("%s%s[o]%s %s%s%s", Bold, color, Reset, Bold, msg, Reset)
	l.write(l.Fmt.FormatLine(content) + "\n")
}

// PriorityColor returns the ANSI color for a given priority level.
func PriorityColor(priority int) string {
	switch priority {
	case 0:
		return Red
	case 1:
		return Orange
	case 2:
		return Yellow
	case 3:
		return Green
	case 4:
		return Dim
	default:
		return Reset
	}
}

// PriorityTag returns a colored "[P0]"-style tag for the given priority.
// Returns empty string when priority is nil (unset).
func PriorityTag(priority *int) string {
	if priority == nil {
		return ""
	}
	return fmt.Sprintf("%s[P%d]%s", PriorityColor(*priority), *priority, Reset)
}

// TaskBanner writes a bold magenta separator with the task ID and title centered,
// shown once when a new task begins. When priority is non-nil, a colored
// priority tag is included after the separator.
func (l *Logger) TaskBanner(taskID, title string, priority *int) {
	label := taskID
	if title != "" {
		label = taskID + ": " + title
	}
	l.Separator(Magenta, label)
	if priority != nil {
		l.Emit(Opts{}, "%s %s", PriorityTag(priority), title)
	}
}

// descMaxLines is the maximum number of description lines shown in the stream log.
const descMaxLines = 3

// descTagPad is the number of spaces to indent description continuation lines,
// aligning them with the text start after the [o][beads] tag and its space.
// "[o][beads] " = 10 visible tag chars + 1 space separator = 11.
const descTagPad = 11

// EmitDescription writes a bead description to the log with per-output truncation:
// stdout shows the first descMaxLines lines followed by a dim "… (N more lines)"
// indicator when the description is longer; the log file receives the full text.
// All lines are formatted with timestamp and markdown stripping. Continuation
// lines are indented to align with the text column past the [o][beads] tag.
func (l *Logger) EmitDescription(description string) {
	if description == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(description, "\n"), "\n")
	tag := Tag(Info.color(), Orch, Beads)
	pad := strings.Repeat(" ", descTagPad)

	for i, line := range lines {
		var content string
		if i == 0 {
			content = fmt.Sprintf("%s %s", tag, FormatContent(line))
		} else {
			content = pad + FormatContent(line)
		}
		formatted := l.Fmt.FormatLine(content) + "\n"
		fmt.Fprint(l.logFile, formatted)
		if i < descMaxLines && !l.streaming {
			fmt.Fprint(l.out, formatted)
		}
	}

	if len(lines) > descMaxLines && !l.streaming {
		remaining := len(lines) - descMaxLines
		indicator := fmt.Sprintf("%s%s… (%d more lines)%s", pad, Dim, remaining, Reset)
		fmt.Fprint(l.out, l.Fmt.FormatLine(indicator)+"\n")
	}
}

// BannerOpts holds the data for an iteration banner.
type BannerOpts struct {
	RunIteration int
	MaxIteration int
	DoneThisRun  int
	Remaining    int
	TaskID       string
	TaskTitle    string
	TaskChanged  bool
	Priority     *int
	Version      string
	BaseBranch   string
	WarnPhase    bool
	Description  string
}

// IterationBanner renders the task banner and phase line between iterations.
func (l *Logger) IterationBanner(o BannerOpts) {
	if o.TaskID != "" && o.TaskChanged {
		l.TaskBanner(o.TaskID, o.TaskTitle, o.Priority)
	}

	phaseColor := Green
	if o.WarnPhase {
		phaseColor = Yellow
	}
	versionTag := ""
	if o.Version != "" {
		versionTag = fmt.Sprintf(" | Ralph v%s", o.Version)
	}
	baseTag := ""
	if o.BaseBranch != "" {
		baseTag = fmt.Sprintf(" | base: %s", o.BaseBranch)
	}
	maxTag := fmt.Sprintf("%d", o.MaxIteration)
	if o.MaxIteration <= 0 {
		maxTag = "unlimited"
	}
	l.PhaseColor(phaseColor, "--- Run iteration %d/%s | %d done this run · %d remaining%s%s ---",
		o.RunIteration, maxTag, o.DoneThisRun, o.Remaining, versionTag, baseTag)

	if o.Description != "" {
		l.EmitDescription(o.Description)
	}
}

// DashedSeparator writes a bold, colored full-width dashed line using ─ characters.
func (l *Logger) DashedSeparator(color string) {
	const totalWidth = 72
	content := fmt.Sprintf("%s%s%s%s", Bold, color, strings.Repeat("─", totalWidth), Reset)
	l.write("\n")
	l.write(l.Fmt.FormatLine(content) + "\n")
	l.write("\n")
}

// Separator writes a bold, colored full-width separator with a centered label.
func (l *Logger) Separator(color, label string) {
	const totalWidth = 72
	pad := totalWidth - len(label) - 2 // 2 for spaces around label
	if pad < 4 {
		pad = 4
	}
	left := pad / 2
	right := pad - left
	content := fmt.Sprintf("%s%s%s %s %s%s",
		Bold, color,
		strings.Repeat("═", left), label, strings.Repeat("═", right),
		Reset)
	l.write("\n")
	l.write(l.Fmt.FormatLine(content) + "\n")
	l.write("\n")
}
