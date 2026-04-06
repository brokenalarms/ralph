package git

import "time"

// Reviewer describes a GitHub App-based automated code reviewer that Ralph
// knows how to detect and poll. Adding a new reviewer is a one-line addition
// to Known.
type Reviewer struct {
	// AppSlug is the GitHub App slug identifying the reviewer.
	AppSlug string
	// BotUsername is the GitHub login of the bot account that posts reviews.
	BotUsername string
	// DefaultTimeout is how long to wait for a review before giving up.
	DefaultTimeout time.Duration
	// ReviewOnPush is only meaningful for Copilot: true if the ruleset gates
	// merging on Copilot review, which warrants the full DefaultTimeout.
	ReviewOnPush bool
}

// Known is the registry of GitHub App-based reviewers Ralph can detect.
// Detection uses per-reviewer probes — only reviewers with a supported probe
// are returned by DetectActiveReviewers.
var Known = []Reviewer{
	{AppSlug: "copilot-code-review", BotUsername: "copilot-pull-request-reviewer", DefaultTimeout: 120 * time.Second},
	{AppSlug: "coderabbitai", BotUsername: "coderabbitai[bot]", DefaultTimeout: 60 * time.Second},
}
