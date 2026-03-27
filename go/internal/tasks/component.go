package tasks

import "strings"

const (
	ComponentLoop    = "ralph loop"
	ComponentTask    = "ralph task"
	ComponentCommand = "ralph command"
)

var componentKeywords = []struct {
	component string
	keywords  []string
}{
	{ComponentLoop, []string{
		"orchestrat", "iteration", "worktree", "signal",
		"verification", "verif", "merge", "rebase",
		"evolve", "git", "branch", "squash",
		"push", "pull", "loop",
	}},
	{ComponentTask, []string{
		"bead", "triage", "backlog", "task manager",
		"bd ", "bd\t", "task prompt", "tracking",
	}},
	{ComponentCommand, []string{
		"tmux", "flag", "iterm", "subcommand",
		"cli", "pane", "command",
	}},
}

var knownPrefixes = []string{
	ComponentLoop + ":",
	ComponentTask + ":",
	ComponentCommand + ":",
}

// StripComponentPrefix removes a known component prefix ("ralph loop:",
// "ralph task:", "ralph command:") from the title, if present.
func StripComponentPrefix(title string) string {
	lower := strings.ToLower(title)
	for _, prefix := range knownPrefixes {
		if strings.HasPrefix(lower, prefix) {
			stripped := strings.TrimSpace(title[len(prefix):])
			if stripped != "" {
				return stripped
			}
			return title
		}
	}
	return title
}

// DetectComponent returns the ralph component (loop, task, command) that best
// matches the given title and description keywords. Returns empty string if
// no component can be determined.
func DetectComponent(title, description string) string {
	combined := strings.ToLower(title + " " + description)

	type match struct {
		component string
		count     int
	}
	var best match

	for _, entry := range componentKeywords {
		count := 0
		for _, kw := range entry.keywords {
			if strings.Contains(combined, kw) {
				count++
			}
		}
		if count > best.count {
			best = match{entry.component, count}
		}
	}

	return best.component
}

// EnsureComponentPrefix detects the target component from the title and
// description, then prepends the component prefix if the title doesn't
// already have one. Returns the title unchanged if it's already prefixed
// or if no component can be detected.
func EnsureComponentPrefix(title, description string) string {
	lower := strings.ToLower(title)
	for _, prefix := range knownPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return title
		}
	}

	comp := DetectComponent(title, description)
	if comp == "" {
		return title
	}

	return comp + ": " + title
}
