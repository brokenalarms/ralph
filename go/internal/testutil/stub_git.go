package testutil

import (
	"context"

	"github.com/brokenalarms/ralph/internal/git"
)

// StubGit implements git.GitOps for testing without spawning real git
// subprocesses. Configure fields to control return values; all methods
// have sensible zero-value defaults. Same pattern as StubBackend.
type StubGit struct {
	ProjectDir     string
	WorkDir        string
	WorktreeBranch string
	PrevBranch     string
	BranchRenamed  bool

	HeadRevValue        string
	HasDiffValue        bool
	HasUncommittedValue bool
	ChangedFilesValue   []string
	DiffStatValue       string
	DiffFullValue       string
	LogOnelineValue     string
	ConflictDiffValue   string
	RemoteURLValue      string
	DefaultBranch       string
	RecentFilesValue    string
	CIFailureLogValue   string

	EnsureUpToDateErr    error
	PushErr              error
	PushPRNumber         string
	PushPRErr            error
	MergeRetryResult     bool
	MergeRetryErr        error
	FlushMerged          bool
	FlushErr             error
	FetchBranchErr       error
	DeleteBranchErr      error
	RemoteBranchCommits  bool
	RemoteBranchOnMain   bool
	BranchAheadOfMain    bool
	BranchAncestorOfMain bool

	GitHubStub git.GitHub

	// Call tracking.
	CommitMessages      []string
	TagsCreated         []string
	PrepareForNextCalls int
	ResetCalls          int
	PostMergeUpdateCalls int
	RenameBranchCalls   int
	SetPrevBranchCalls  []string
}

// Compile-time check that StubGit satisfies git.GitOps.
var _ git.GitOps = (*StubGit)(nil)

func (s *StubGit) GetProjectDir() string     { return s.ProjectDir }
func (s *StubGit) GetWorkDir() string         { return s.WorkDir }
func (s *StubGit) GetWorktreeBranch() string  { return s.WorktreeBranch }
func (s *StubGit) GetPrevBranch() string      { return s.PrevBranch }
func (s *StubGit) IsBranchRenamed() bool      { return s.BranchRenamed }
func (s *StubGit) SetBranchRenamed(v bool)    { s.BranchRenamed = v }
func (s *StubGit) SetLocalTestsPassed(v bool) {}

func (s *StubGit) GH() git.GitHub {
	return s.GitHubStub
}

func (s *StubGit) HeadRev() string                                    { return s.HeadRevValue }
func (s *StubGit) HasDiff() bool                                      { return s.HasDiffValue }
func (s *StubGit) HasUncommittedChanges() bool                        { return s.HasUncommittedValue }
func (s *StubGit) ChangedFiles(_, _ string) []string                  { return s.ChangedFilesValue }
func (s *StubGit) DiffStatRange(_, _ string) string                   { return s.DiffStatValue }
func (s *StubGit) DiffFull(_, _ string) string                        { return s.DiffFullValue }
func (s *StubGit) LogOneline(_, _ string) string                      { return s.LogOnelineValue }
func (s *StubGit) ConflictDiff() string                               { return s.ConflictDiffValue }
func (s *StubGit) RemoteURL() string                                  { return s.RemoteURLValue }
func (s *StubGit) DetectDefaultBranch() string {
	if s.DefaultBranch != "" {
		return s.DefaultBranch
	}
	return "main"
}
func (s *StubGit) RecentChangedFiles(_ int) string                    { return s.RecentFilesValue }
func (s *StubGit) GetCIFailureLog(_ string) string                    { return s.CIFailureLogValue }

func (s *StubGit) PrepareForNextTask() {
	s.PrepareForNextCalls++
	s.BranchRenamed = false
}

func (s *StubGit) ResetToDefaultBranch() {
	s.ResetCalls++
	s.BranchRenamed = false
}

func (s *StubGit) RenameBranchForTask(taskDesc, taskID string) {
	if s.BranchRenamed || s.WorktreeBranch == "" || taskDesc == "" {
		return
	}
	s.RenameBranchCalls++
	slug := git.Slugify(taskDesc)
	if slug == "" {
		return
	}
	s.WorktreeBranch = git.BranchName(taskID, slug)
	s.BranchRenamed = true
}

func (s *StubGit) RenameBranchTo(name string) {
	if s.BranchRenamed || s.WorktreeBranch == "" || name == "" {
		return
	}
	if s.WorktreeBranch == name {
		s.BranchRenamed = true
		return
	}
	s.WorktreeBranch = name
	s.BranchRenamed = true
}

func (s *StubGit) SetPrevBranch(branch string) {
	s.PrevBranch = branch
	s.SetPrevBranchCalls = append(s.SetPrevBranchCalls, branch)
}

func (s *StubGit) TagTaskStart(taskID string) {
	s.TagsCreated = append(s.TagsCreated, "task/"+taskID+"/start")
}

func (s *StubGit) TagTaskEnd(taskID string) {
	s.TagsCreated = append(s.TagsCreated, "task/"+taskID+"/end")
}

func (s *StubGit) CommitAll(message string) {
	s.CommitMessages = append(s.CommitMessages, message)
}

func (s *StubGit) EnsureUpToDate(_ context.Context) error  { return s.EnsureUpToDateErr }
func (s *StubGit) Push(_ context.Context) error             { return s.PushErr }

func (s *StubGit) PushAndCreatePR(_ context.Context, _, _, _ string) (string, error) {
	return s.PushPRNumber, s.PushPRErr
}

func (s *StubGit) MergeWithRetry(_ context.Context, _ git.MergeRetryOpts) (bool, error) {
	return s.MergeRetryResult, s.MergeRetryErr
}

func (s *StubGit) FlushUnpushedWork(_ context.Context, _, _ string, _ bool) (bool, error) {
	return s.FlushMerged, s.FlushErr
}

func (s *StubGit) PostMergeUpdateMain() {
	s.PostMergeUpdateCalls++
}

func (s *StubGit) FetchBranch(_ string) error            { return s.FetchBranchErr }
func (s *StubGit) CheckoutRemoteBranch(_ string)         {}
func (s *StubGit) RemoteBranchHasCommits(_ string) bool  { return s.RemoteBranchCommits }
func (s *StubGit) RemoteBranchIsOnMain(_ string) bool    { return s.RemoteBranchOnMain }
func (s *StubGit) BranchIsAheadOfMain(_ string) bool     { return s.BranchAheadOfMain }
func (s *StubGit) BranchIsAncestorOfMain(_ string) bool  { return s.BranchAncestorOfMain }
func (s *StubGit) DeleteRemoteBranchByName(_ string) error { return s.DeleteBranchErr }
