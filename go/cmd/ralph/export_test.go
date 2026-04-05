package main

import (
	"context"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

type StackPR = stackPR
type StackResult = stackResult

var CollectStack = collectStack
var RebaseStackAndPush = rebaseStackAndPush
var RunMerge = runMerge
var CmdOutputDir = cmdOutputDir

func NewStackPR(number int, head string) StackPR {
	return stackPR{number: number, head: head}
}

func StackResultPRs(r StackResult) []StackPR    { return r.prs }
func StackResultBaseBranch(r StackResult) string { return r.baseBranch }
func StackPRNumber(p StackPR) int               { return p.number }
func StackPRHead(p StackPR) string               { return p.head }

// RunMergeForTest wraps runMerge for external tests.
func RunMergeForTest(ctx context.Context, prs []stackPR, projectDir, defaultBranch string, gm *git.Manager, log *logging.Logger) int {
	return runMerge(ctx, prs, projectDir, defaultBranch, gm, log)
}
