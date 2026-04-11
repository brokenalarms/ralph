package main

type StackPR = stackPR
type StackResult = stackResult

var CollectStack = collectStack
var RunMerge = runMerge

func NewStackPR(number int, head string) StackPR {
	return stackPR{number: number, head: head}
}

func StackResultPRs(r StackResult) []StackPR     { return r.prs }
func StackResultBaseBranch(r StackResult) string { return r.baseBranch }
func StackPRNumber(p StackPR) int                { return p.number }
func StackPRHead(p StackPR) string               { return p.head }

