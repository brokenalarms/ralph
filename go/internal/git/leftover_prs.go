package git

import (
	"sort"
	"strings"
)

// LeftoverRalphPRs filters allPRs to open, ralph-authored PRs (branchPrefix
// "ralph/"), sorted by PR number descending — newest first. Used at loop
// startup to surface prior-run PRs a fresh run would otherwise silently
// build past.
func LeftoverRalphPRs(allPRs []PRInfo) []PRInfo {
	var out []PRInfo
	for _, pr := range allPRs {
		if pr.State == PRStateOpen && strings.HasPrefix(pr.Head, branchPrefix) {
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out
}
