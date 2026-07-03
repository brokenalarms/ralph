package component

import "testing"

// Proves: titles mentioning orchestrator concepts (worktree, signal, verification,
// merge, rebase, evolve) are detected as the "loop" component.
func TestDetectComponent_LoopKeywords(t *testing.T) {
	cases := []struct {
		title string
		desc  string
	}{
		{"force-reset worktree after merge", ""},
		{"fix signal file cleanup", "The .signal_complete file persists"},
		{"verification gate timeout", ""},
		{"auto-resolve rebase conflicts", ""},
		{"evolve restart after rebuild", ""},
		{"iteration count off by one", ""},
		{"squash-merge overwrites changes", ""},
	}
	for _, tc := range cases {
		got := DetectComponent(tc.title, tc.desc)
		if got != ComponentLoop {
			t.Errorf("DetectComponent(%q, %q) = %q, want %q", tc.title, tc.desc, got, ComponentLoop)
		}
	}
}

// Proves: titles mentioning task manager concepts (bead, triage, backlog, bd)
// are detected as the "task" component.
func TestDetectComponent_TaskKeywords(t *testing.T) {
	cases := []struct {
		title string
		desc  string
	}{
		{"echo back created beads for review", ""},
		{"triage incoming bugs", ""},
		{"backlog cleanup", ""},
		{"prefix bead titles with component", ""},
		{"task manager prompt update", ""},
	}
	for _, tc := range cases {
		got := DetectComponent(tc.title, tc.desc)
		if got != ComponentTask {
			t.Errorf("DetectComponent(%q, %q) = %q, want %q", tc.title, tc.desc, got, ComponentTask)
		}
	}
}

// Proves: titles mentioning CLI concepts (subcommand, flag, tmux, iTerm)
// are detected as the "command" component.
func TestDetectComponent_CommandKeywords(t *testing.T) {
	cases := []struct {
		title string
		desc  string
	}{
		{"four-pane tmux with loop + task manager", ""},
		{"add --quiet flag", ""},
		{"iTerm notification on task complete", ""},
		{"fix subcommand help text", ""},
		{"CLI entry point refactor", ""},
	}
	for _, tc := range cases {
		got := DetectComponent(tc.title, tc.desc)
		if got != ComponentCommand {
			t.Errorf("DetectComponent(%q, %q) = %q, want %q", tc.title, tc.desc, got, ComponentCommand)
		}
	}
}

// Proves: titles with no recognizable keywords return empty component,
// so we don't force-prefix ambiguous titles.
func TestDetectComponent_Unknown(t *testing.T) {
	got := DetectComponent("fix typo in README", "")
	if got != "" {
		t.Errorf("DetectComponent for unknown area = %q, want empty", got)
	}
}

// Proves: description keywords are used as fallback when the title is ambiguous.
func TestDetectComponent_FallsBackToDescription(t *testing.T) {
	got := DetectComponent("fix off-by-one error", "The iteration counter in the orchestrator loop")
	if got != ComponentLoop {
		t.Errorf("expected loop from description, got %q", got)
	}
}

// Proves: EnsureComponentPrefix adds the detected prefix to an unprefixed title.
func TestEnsureComponentPrefix_AddsPrefix(t *testing.T) {
	got := EnsureComponentPrefix("force-reset worktree after merge", "")
	want := "ralph loop: force-reset worktree after merge"
	if got != want {
		t.Errorf("EnsureComponentPrefix = %q, want %q", got, want)
	}
}

// Proves: already-prefixed titles are returned unchanged to avoid double-prefixing.
func TestEnsureComponentPrefix_AlreadyPrefixed(t *testing.T) {
	cases := []string{
		"ralph loop: force-reset worktree",
		"ralph task: echo back beads",
		"ralph command: four-pane tmux",
	}
	for _, title := range cases {
		got := EnsureComponentPrefix(title, "")
		if got != title {
			t.Errorf("EnsureComponentPrefix(%q) = %q, should be unchanged", title, got)
		}
	}
}

// Proves: StripComponentPrefix removes known prefixes so PR titles stay concise.
func TestStripComponentPrefix(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"ralph loop: force-reset worktree", "force-reset worktree"},
		{"ralph task: echo back beads", "echo back beads"},
		{"ralph command: four-pane tmux", "four-pane tmux"},
		{"Ralph Loop: mixed case prefix", "mixed case prefix"},
		{"fix typo in README", "fix typo in README"},
		{"ralph loop:", "ralph loop:"},
	}
	for _, tc := range cases {
		got := StripComponentPrefix(tc.input)
		if got != tc.want {
			t.Errorf("StripComponentPrefix(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// Proves: titles with unknown component area are returned unchanged.
func TestEnsureComponentPrefix_UnknownComponent(t *testing.T) {
	title := "fix typo in README"
	got := EnsureComponentPrefix(title, "")
	if got != title {
		t.Errorf("EnsureComponentPrefix(%q) = %q, should be unchanged", title, got)
	}
}
