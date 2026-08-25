package git

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// --- Stub API (per docs/specs/stub-interface-rewrite.md) ---
//
// stubGitHub is a true in-memory fake of the gitHub interface. Its behavior
// is fixed: every test runs against the same model. What varies per test is
// the initial state of the world (PRs, Checks, static return values) passed
// in through StubGitHubConfig.
//
// Construction is the one and only configuration seam. The stub's internal
// state is reachable only through gitHub interface methods — the same methods
// production uses — so tests observe "what the SUT did" by querying the
// world, not by reading stub fields.

// StubPR describes a pull request that exists in the fake's world.
// Unset fields are filled in with deterministic defaults at construction:
// URL → https://github.com/owner/repo/pull/<Number>, HeadSHA → stub-sha-<Number>,
// Base → "main", State → PRStateOpen.
//
// Conflicted and Blocked describe world-state causes, not prescribed
// outcomes — the fake's MergePR derives its return value from these
// properties the same way real GitHub derives its response from the actual
// world. A PR with Conflicted=true cannot be merged until the conflict is
// resolved; a PR with Blocked=true is gated by branch protection.
type StubPR struct {
	Number     int
	Title      string
	URL        string
	Branch     string
	Base       string
	HeadSHA    string
	State      PRState
	Conflicted bool
	Blocked    bool
}

// StubGitHubConfig declares the starting state of the fake's world and any
// static return values. All fields are plain data; none programs per-call
// behavior. For fault injection, set the *Err field for the method that
// should fail — a single value per field, no sequencing.
type StubGitHubConfig struct {
	Available bool

	// The world.
	PRs    []StubPR
	Checks map[int][]CICheckResult

	// Static return values.
	RunLog           string
	Reviewers        []Reviewer
	RequiredChecks   []string
	JobStepCount     int
	RunningJobSteps  []JobStepStatus
	PollReviewResult *AutoReview
	FetchThreadIDs   map[int]string
	PRDiffOutput     string

	// FailedJobAnnotations is the failure-level check-run annotation set
	// GetFailedJobAnnotations reports per failed job of the PR's current run.
	FailedJobAnnotations []JobAnnotations

	// Fault injection — single error per method.
	CreatePRErr             error
	CreatePRViaAPIErr       error
	EditPRErr               error
	EditPRBaseErr           error
	ReopenPRErr             error
	ListChecksErr           error
	FindOpenPRErr           error
	FindPRErr               error
	GetPRErr                error
	ListAllPRsErr           error
	ListOpenPRBranchesErr   error
	PRDiffErr               error
	GetJobStepCountErr      error
	GetRunningJobStepsErr   error
	FailedJobAnnotationsErr error
	DetectReviewersErr      error
	PollReviewErr           error
	RequiredChecksErr       error
	ReplyToReviewErr        error
	FetchThreadIDsErr       error
	ResolveThreadErr        error
	PingErr                 error
}

// stubGitHub is the unexported fake. Tests never see this type directly;
// they receive it only through the gitHub interface value returned by
// newStubGitHub.
type stubGitHub struct {
	cfg          StubGitHubConfig
	prs          map[int]*StubPR
	nextPRNumber int
}

// Compile-time check that stubGitHub implements the full gitHub interface.
var _ gitHub = (*stubGitHub)(nil)

// newStubGitHub returns a stubGitHub initialized with cfg's world, typed
// as the gitHub interface so callers cannot reach into its internal state.
// Package-private: external packages configure GitHub behavior through
// StubRepoConfig.GitHub when they need it; internal/git tests use this
// constructor directly.
func newStubGitHub(cfg StubGitHubConfig) gitHub {
	s := &stubGitHub{
		cfg: cfg,
		prs: make(map[int]*StubPR, len(cfg.PRs)),
	}
	maxNum := 0
	for i := range cfg.PRs {
		pr := cfg.PRs[i] // copy
		normalizeStubPR(&pr)
		s.prs[pr.Number] = &pr
		if pr.Number > maxNum {
			maxNum = pr.Number
		}
	}
	s.nextPRNumber = maxNum + 1
	if s.nextPRNumber == 1 {
		// No pre-loaded PRs: start numbering at something that can't collide
		// with a default test assumption.
		s.nextPRNumber = 100
	}
	return s
}

// normalizeStubPR fills in deterministic defaults for unset fields.
func normalizeStubPR(pr *StubPR) {
	if pr.Base == "" {
		pr.Base = "main"
	}
	if pr.State == "" {
		pr.State = PRStateOpen
	}
	if pr.HeadSHA == "" {
		pr.HeadSHA = fmt.Sprintf("stub-sha-%d", pr.Number)
	}
	if pr.URL == "" {
		pr.URL = fmt.Sprintf("https://github.com/owner/repo/pull/%d", pr.Number)
	}
}

func (s *stubGitHub) Available() bool { return s.cfg.Available }

func (s *stubGitHub) FindOpenPR(_ context.Context, branch, repoURL string) (int, error) {
	if s.cfg.FindOpenPRErr != nil {
		return 0, s.cfg.FindOpenPRErr
	}
	for _, pr := range s.prs {
		if pr.State == PRStateOpen && pr.Branch == branch {
			return pr.Number, nil
		}
	}
	return 0, nil
}

func (s *stubGitHub) CreatePR(_ context.Context, opts CreatePROpts) (int, error) {
	if s.cfg.CreatePRErr != nil {
		return 0, s.cfg.CreatePRErr
	}
	num := s.nextPRNumber
	s.nextPRNumber++
	pr := &StubPR{
		Number: num,
		Title:  opts.Title,
		Branch: opts.Head,
		Base:   opts.Base,
		State:  PRStateOpen,
	}
	normalizeStubPR(pr)
	s.prs[num] = pr
	return num, nil
}

func (s *stubGitHub) MergePR(_ context.Context, prNumber int, _ string, opts MergeOpts) MergeResult {
	pr, ok := s.prs[prNumber]
	if !ok {
		return MergeResult{Merged: false, Message: fmt.Sprintf("PR %d not found", prNumber)}
	}
	if pr.State != PRStateOpen {
		return MergeResult{Merged: false, Message: fmt.Sprintf("PR %d not open (state=%s)", prNumber, pr.State)}
	}
	if pr.Conflicted {
		return MergeResult{Conflict: true, Message: fmt.Sprintf("PR %d has merge conflicts", prNumber)}
	}
	if pr.Blocked && !opts.Admin {
		return MergeResult{Blocked: true, Message: fmt.Sprintf("PR %d blocked by branch protection", prNumber)}
	}
	pr.State = PRStateMerged
	return MergeResult{Merged: true, MergedSHA: pr.HeadSHA}
}

func (s *stubGitHub) ListChecks(_ context.Context, prNumber int, _ string) ([]CICheckResult, error) {
	return s.cfg.Checks[prNumber], s.cfg.ListChecksErr
}

func (s *stubGitHub) EditPR(_ context.Context, prNumber int, _, title, _ string) error {
	if s.cfg.EditPRErr != nil {
		return s.cfg.EditPRErr
	}
	if pr, ok := s.prs[prNumber]; ok {
		pr.Title = title
	}
	return nil
}

func (s *stubGitHub) EditPRBase(_ context.Context, prNumber int, _, base string) error {
	if s.cfg.EditPRBaseErr != nil {
		return s.cfg.EditPRBaseErr
	}
	if pr, ok := s.prs[prNumber]; ok {
		pr.Base = base
	}
	return nil
}

func (s *stubGitHub) GetRunLog(_ context.Context, _ int, _ string) string {
	return s.cfg.RunLog
}

func (s *stubGitHub) FindPR(_ context.Context, branch, _ string) (int, string, string, error) {
	if s.cfg.FindPRErr != nil {
		return 0, "", "", s.cfg.FindPRErr
	}
	for _, pr := range s.prs {
		if pr.Branch == branch {
			return pr.Number, pr.Title, pr.URL, nil
		}
	}
	return 0, "", "", nil
}

func (s *stubGitHub) PRDiff(_ context.Context, _ string, _ int) (string, error) {
	return s.cfg.PRDiffOutput, s.cfg.PRDiffErr
}

func (s *stubGitHub) GetPR(_ context.Context, _ string, prNumber int) (*PRDetail, error) {
	if s.cfg.GetPRErr != nil {
		return nil, s.cfg.GetPRErr
	}
	pr, ok := s.prs[prNumber]
	if !ok {
		return nil, nil
	}
	return &PRDetail{
		State:   pr.State,
		BaseRef: pr.Base,
		HeadRef: pr.Branch,
		HeadSHA: pr.HeadSHA,
	}, nil
}

func (s *stubGitHub) ListOpenPRBranches(_ context.Context, _ string) ([]string, error) {
	if s.cfg.ListOpenPRBranchesErr != nil {
		return nil, s.cfg.ListOpenPRBranchesErr
	}
	// Collect open PR branches in deterministic (number-ascending) order so
	// tests don't depend on map iteration order.
	nums := make([]int, 0, len(s.prs))
	for n := range s.prs {
		nums = append(nums, n)
	}
	sortInts(nums)
	var branches []string
	for _, n := range nums {
		if s.prs[n].State == PRStateOpen {
			branches = append(branches, s.prs[n].Branch)
		}
	}
	return branches, nil
}

func (s *stubGitHub) ReopenPR(_ context.Context, prNumber int, _ string) error {
	if s.cfg.ReopenPRErr != nil {
		return s.cfg.ReopenPRErr
	}
	if pr, ok := s.prs[prNumber]; ok {
		pr.State = PRStateOpen
	}
	return nil
}

func (s *stubGitHub) CreatePRViaAPI(_ context.Context, _ string, opts CreatePROpts) (int, error) {
	if s.cfg.CreatePRViaAPIErr != nil {
		return 0, s.cfg.CreatePRViaAPIErr
	}
	return s.CreatePR(context.Background(), opts)
}

func (s *stubGitHub) GetJobStepCount(_ context.Context, _ string, _ int) (int, error) {
	return s.cfg.JobStepCount, s.cfg.GetJobStepCountErr
}

func (s *stubGitHub) GetRunningJobSteps(_ context.Context, _ string, _ int) ([]JobStepStatus, error) {
	return s.cfg.RunningJobSteps, s.cfg.GetRunningJobStepsErr
}

func (s *stubGitHub) GetFailedJobAnnotations(_ context.Context, _ string, _ int) ([]JobAnnotations, error) {
	return s.cfg.FailedJobAnnotations, s.cfg.FailedJobAnnotationsErr
}

func (s *stubGitHub) ListAllPRs(_ context.Context, _ string) ([]PRInfo, error) {
	if s.cfg.ListAllPRsErr != nil {
		return nil, s.cfg.ListAllPRsErr
	}
	nums := make([]int, 0, len(s.prs))
	for n := range s.prs {
		nums = append(nums, n)
	}
	sortInts(nums)
	out := make([]PRInfo, 0, len(nums))
	for _, n := range nums {
		pr := s.prs[n]
		out = append(out, PRInfo{Number: pr.Number, Head: pr.Branch, Base: pr.Base, State: pr.State})
	}
	return out, nil
}

func (s *stubGitHub) ListOpenPRs(ctx context.Context, workDir string) ([]PRInfo, error) {
	all, err := s.ListAllPRs(ctx, workDir)
	if err != nil {
		return nil, err
	}
	out := make([]PRInfo, 0, len(all))
	for _, pr := range all {
		if pr.State == PRStateOpen {
			out = append(out, pr)
		}
	}
	return out, nil
}

func (s *stubGitHub) DetectActiveReviewers(_ context.Context, _ string) ([]Reviewer, error) {
	return s.cfg.Reviewers, s.cfg.DetectReviewersErr
}

func (s *stubGitHub) PollReview(_ context.Context, _, _ string, _ int, _ time.Duration) (*AutoReview, error) {
	return s.cfg.PollReviewResult, s.cfg.PollReviewErr
}

func (s *stubGitHub) GetRequiredChecks(_ context.Context, _, _ string) ([]string, error) {
	return s.cfg.RequiredChecks, s.cfg.RequiredChecksErr
}

func (s *stubGitHub) ReplyToReviewComment(_ context.Context, _ string, _, _ int, _ string) error {
	return s.cfg.ReplyToReviewErr
}

func (s *stubGitHub) FetchReviewThreadIDs(_ context.Context, _ string, _ int, _ []int) (map[int]string, error) {
	return s.cfg.FetchThreadIDs, s.cfg.FetchThreadIDsErr
}

func (s *stubGitHub) ResolveReviewThread(_ context.Context, _ string) error {
	return s.cfg.ResolveThreadErr
}

func (s *stubGitHub) Ping(_ context.Context) error {
	return s.cfg.PingErr
}

// sortInts sorts a slice in ascending order. Local helper to avoid importing
// sort in this file purely for one call site.
func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// --- stubRepo: true in-memory fake of Ops for external-package unit tests ---
//
// StubRepoConfig declares the starting world state and prescribed outcomes for
// a stubRepo. All fields are plain data; no callbacks, no sequenced slices.
// Construction is the one and only configuration seam — state mutations after
// construction happen only through Ops interface methods driven by the SUT.
//
// PR-related methods delegate to an inner stubGitHub constructed from
// cfg.GitHub. Git-module-local methods read from cfg/mutable state.
type StubRepoConfig struct {
	// World state — the git-module view of the repo.
	ProjectDir     string
	WorkDir        string
	RalphDir       string
	BaseBranch     string // default "main" if empty
	WorktreeBranch string
	PrevBranch     string
	BranchRenamed  bool
	RemoteURL      string
	HeadRev        string // returned by HeadRev()
	DefaultBranch  string // returned by DetectDefaultBranch(); defaults to BaseBranch
	HasUncommitted bool
	HasDiff        bool
	KnownPRNumber  int

	// Inner GitHub fake config. NewStub builds a stubGitHub from this and
	// delegates PR-related methods to it.
	GitHub StubGitHubConfig

	// Static returns for read-only queries.
	ChangedFiles       []string
	DiffStat           string
	DiffFullResult     string
	DiffFromBaseResult string
	LogOnelineResult   string
	ConflictDiff       string
	RecentChangedFiles string
	CIFailureLog       string

	// Ship / merge results — prescribed outcomes where deriving from state
	// would require an unreasonable modeling burden.
	Ship               ShipResult
	ShipErr            error
	PushAndCreatePRNum int
	PushAndCreatePRErr error
	MergeRetrySucceeds bool
	MergeRetryErr      error
	FlushMerged        bool
	FlushErr           error
	MergeStackResult   MergeStackResult
	MergeStackErr      error
	ResumeTaskResult   ResumeTaskResult
	ResumeTaskErr      error

	// Per-method fault injection for simple methods.
	InitErr                      error
	CheckBranchNamespaceSquatErr error
	SyncWorktreeBaseErr          error
	BranchForTaskResult          string // override for BranchForTask; default derived from taskID
	BranchForTaskErr             error
	EnsureUpToDateErr            error
	PushErr                      error
	FetchBranchErr               error
	DeleteRemoteBranchByNameErr  error
	ReplyToAndResolveCommentsErr error
	RenameBranchForTaskErr       error

	// Remote branch state — map of branch name → property. Missing entries
	// return zero values.
	RemoteBranchHasCommits map[string]bool
	RemoteBranchIsOnMain   map[string]bool
	BranchIsAheadOfMain    map[string]bool
	BranchHasUnmergedWork  map[string]bool
	BranchIsAncestorOfMain map[string]bool
	// CommitDroppedFromBranch simulates a worktree reset that removed the
	// agent's commit. When true, IsCommitAncestorOf always returns false.
	CommitDroppedFromBranch bool

	// PRChainHealthy overrides PRChainIsHealthy output. When PRChainHealthyMsg
	// is non-empty, PRChainIsHealthy returns (PRChainHealthy, PRChainHealthyMsg).
	// Otherwise returns (true, "").
	PRChainHealthy    bool
	PRChainHealthyMsg string

	// PRDiffForRefResult is returned by PRDiffForRef.
	PRDiffForRefResult string
}

// StubInspector exposes internal call counters on the test stub so loop-level
// tests can assert ordering invariants (e.g. that evolve fires before
// BranchForTask). Only *stubRepo implements this; access it via type assertion:
//
//	gm.(git.StubInspector).GetBranchForTaskCalls()
type StubInspector interface {
	GetBranchForTaskCalls() int
	GetRemoveWorktreeCalls() int
	GetRemoveWorktreeForBranchCalls() int
	GetRemovedWorktreeForBranch() string
	GetFlushUnpushedWorkCalls() int
	GetReplyToAndResolveCommentsCalls() int
	GetAdoptedStackBranch() string
	GetMergeStackCalls() int
	GetMergeStackTopPR() string
	GetBranchForTaskStackParents() []string
}

// stubRepo is an unexported fake of Ops. Tests receive it only through the
// Ops interface via NewStub. All mutable state is unexported.
type stubRepo struct {
	cfg StubRepoConfig
	gh  gitHub

	// headMu guards the head fields, which a test may mutate from a goroutine
	// (simulating a commit landing mid-run) while the SUT reads HeadRev
	// concurrently.
	headMu sync.Mutex

	// Mutable state. Initialized from cfg, mutated by SUT-driven Ops methods
	// so subsequent reads reflect what the SUT did.
	worktreeBranch                 string
	prevBranch                     string
	branchRenamed                  bool
	headRev                        string
	knownPRNumber                  int
	commitSeq                      int
	branchForTaskCalls             int
	removeWorktreeCalls            int
	removeWorktreeForBranchCalls   int
	removedWorktreeForBranch       string
	flushUnpushedWorkCalls         int
	replyToAndResolveCommentsCalls int
	adoptedStackBranch             string
	mergeStackCalls                int
	mergeStackTopPR                string
	branchForTaskStackParents      []string
}

// GetBranchForTaskCalls returns the number of times BranchForTask has been
// called. Used by tests to assert that evolve fired before branch setup.
func (s *stubRepo) GetBranchForTaskCalls() int { return s.branchForTaskCalls }

// GetBranchForTaskStackParents returns the CompletedBranches the last
// BranchForTask call was given — the candidate stack parents for the task
// being set up. Used by tests to assert which branches an iteration is
// willing to stack on.
func (s *stubRepo) GetBranchForTaskStackParents() []string { return s.branchForTaskStackParents }

// GetRemoveWorktreeCalls returns the number of times RemoveWorktree has been
// called. Used by tests to assert that a skipped task tears down its worktree.
func (s *stubRepo) GetRemoveWorktreeCalls() int { return s.removeWorktreeCalls }

// GetRemoveWorktreeForBranchCalls returns the number of times
// RemoveWorktreeForBranch has been called. Used by tests to assert an
// explicit-branch cleanup path ran (e.g. `ralph task` exit cleanup).
func (s *stubRepo) GetRemoveWorktreeForBranchCalls() int { return s.removeWorktreeForBranchCalls }

// GetRemovedWorktreeForBranch returns the branch name passed to the most
// recent RemoveWorktreeForBranch call.
func (s *stubRepo) GetRemovedWorktreeForBranch() string { return s.removedWorktreeForBranch }

// GetFlushUnpushedWorkCalls returns the number of times FlushUnpushedWork has
// been called. Used by tests to assert that a skipped task's branch is not
// flushed/auto-merged by the wait-mode safety-net.
func (s *stubRepo) GetFlushUnpushedWorkCalls() int { return s.flushUnpushedWorkCalls }

// GetReplyToAndResolveCommentsCalls returns the number of times
// ReplyToAndResolveComments has been called. Used by tests to assert that a
// review fix's post-push reply-and-resolve step fires after a successful push.
func (s *stubRepo) GetReplyToAndResolveCommentsCalls() int {
	return s.replyToAndResolveCommentsCalls
}

// GetAdoptedStackBranch returns the branch passed to the most recent
// SetAdoptedStackBranch call. Used by tests to assert the leftover-PR
// startup prompt adopted the correct branch.
func (s *stubRepo) GetAdoptedStackBranch() string { return s.adoptedStackBranch }

// GetMergeStackCalls returns the number of MergeStack calls, and
// GetMergeStackTopPR the TopPR of the most recent one. Used by tests to
// assert which PR a merge targeted — and, for a leftover stack of two or
// more, that no merge was attempted at all.
func (s *stubRepo) GetMergeStackCalls() int    { return s.mergeStackCalls }
func (s *stubRepo) GetMergeStackTopPR() string { return s.mergeStackTopPR }

// Compile-time check that *stubRepo satisfies Ops.
var _ Ops = (*stubRepo)(nil)

// Compile-time check that *stubRepo satisfies StubInspector.
var _ StubInspector = (*stubRepo)(nil)

// NewForTest returns a real *repo (as Ops) configured for integration
// testing: real execRunner, real state store (file-backed via cfg.RalphDir
// or nil when unset), real logger — but with gitHub swapped for a
// stubGitHub built from ghCfg. External integration tests construct a real
// bare repo on disk, pass its path in cfg.ProjectDir/WorkDir, and assert
// on the observable git state (branches, commits, origin/main) after the
// SUT runs.
//
// This is the third and final construction seam (alongside New and NewStub).
// Production code uses New; loop-module unit tests use NewStub; loop-module
// integration tests that need real git state transitions use NewForTest.
func NewForTest(cfg Config, ghCfg StubGitHubConfig) Ops {
	projectDir := cfg.ProjectDir
	if projectDir == "" {
		projectDir = cfg.WorkDir
	}
	return &repo{
		projectDir:                  projectDir,
		workDir:                     cfg.WorkDir,
		ralphDir:                    cfg.RalphDir,
		baseBranch:                  cfg.BaseBranch,
		resume:                      cfg.Resume,
		logger:                      cfg.Logger,
		compileCheckTimeout:         cfg.CompileCheckTimeout,
		ciPollTimeout:               cfg.CIPollTimeout,
		noCIGracePeriod:             cfg.NoCIGracePeriod,
		copilotGatedTimeout:         cfg.CopilotGatedTimeout,
		copilotOpportunisticTimeout: cfg.CopilotOpportunisticTimeout,
		codeRabbitTimeout:           cfg.CodeRabbitTimeout,
		configVerify:                cfg.ConfigVerify,
		testTimeout:                 cfg.TestTimeout,
		github:                      newStubGitHub(ghCfg),
		state:                       newStateStore(cfg.RalphDir),
	}
}

// NewStub returns a stubRepo as the Ops interface. External packages use
// this to isolate unit tests from the git module.
func NewStub(cfg StubRepoConfig) Ops {
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = "main"
	}
	if cfg.DefaultBranch == "" {
		cfg.DefaultBranch = cfg.BaseBranch
	}
	return &stubRepo{
		cfg:            cfg,
		gh:             newStubGitHub(cfg.GitHub),
		worktreeBranch: cfg.WorktreeBranch,
		prevBranch:     cfg.PrevBranch,
		branchRenamed:  cfg.BranchRenamed,
		headRev:        cfg.HeadRev,
		knownPRNumber:  cfg.KnownPRNumber,
	}
}

// --- State accessors ---

func (s *stubRepo) GetProjectDir() string     { return s.cfg.ProjectDir }
func (s *stubRepo) GetWorkDir() string        { return s.cfg.WorkDir }
func (s *stubRepo) GetWorktreeBranch() string { return s.worktreeBranch }
func (s *stubRepo) GetPrevBranch() string     { return s.prevBranch }
func (s *stubRepo) IsBranchRenamed() bool     { return s.branchRenamed }
func (s *stubRepo) SetBranchRenamed(v bool)   { s.branchRenamed = v }
func (s *stubRepo) SetKnownPRNumber(n int)    { s.knownPRNumber = n }

// --- PR operations: delegate to inner gh, mirroring *repo's delegation ---

func (s *stubRepo) FindOpenPRForBranch(ctx context.Context, branch string) (int, error) {
	if !s.gh.Available() {
		return 0, nil
	}
	return s.gh.FindOpenPR(ctx, branch, s.cfg.RemoteURL)
}

func (s *stubRepo) GetPRState(ctx context.Context, prNumber int) (PRState, error) {
	if !s.gh.Available() {
		return "", nil
	}
	pr, err := s.gh.GetPR(ctx, NWOFromRemote(s.cfg.RemoteURL), prNumber)
	if err != nil {
		return "", err
	}
	if pr == nil {
		return "", nil
	}
	return pr.State, nil
}

func (s *stubRepo) ListOpenPRBranches(ctx context.Context) ([]string, error) {
	if s.cfg.RemoteURL == "" || !s.gh.Available() {
		return nil, nil
	}
	return s.gh.ListOpenPRBranches(ctx, s.cfg.RemoteURL)
}

func (s *stubRepo) GetPRBase(ctx context.Context, prNumber int) string {
	if !s.gh.Available() {
		return ""
	}
	pr, err := s.gh.GetPR(ctx, NWOFromRemote(s.cfg.RemoteURL), prNumber)
	if err != nil || pr == nil {
		return ""
	}
	return pr.BaseRef
}

func (s *stubRepo) FindPRForBranch(ctx context.Context, branch string) (int, string, string, error) {
	if !s.gh.Available() {
		return 0, "", "", nil
	}
	return s.gh.FindPR(ctx, branch, s.cfg.RemoteURL)
}

func (s *stubRepo) PRChainIsHealthy(_ context.Context, prNumber int) (bool, string) {
	if s.cfg.PRChainHealthyMsg != "" {
		return s.cfg.PRChainHealthy, s.cfg.PRChainHealthyMsg
	}
	return true, ""
}

func (s *stubRepo) PRDiffForRef(ctx context.Context, externalRef string) string {
	return s.cfg.PRDiffForRefResult
}

// --- Diff and status queries ---

func (s *stubRepo) HeadRev() string {
	s.headMu.Lock()
	defer s.headMu.Unlock()
	return s.headRev
}
func (s *stubRepo) HasDiff() bool                                   { return s.cfg.HasDiff }
func (s *stubRepo) HasUncommittedChanges() bool                     { return s.cfg.HasUncommitted }
func (s *stubRepo) ChangedFiles(_, _ string) []string               { return s.cfg.ChangedFiles }
func (s *stubRepo) DiffStatRange(_, _ string) string                { return s.cfg.DiffStat }
func (s *stubRepo) DiffFull(_, _ string) string                     { return s.cfg.DiffFullResult }
func (s *stubRepo) DiffFromBase() string                            { return s.cfg.DiffFromBaseResult }
func (s *stubRepo) LogOneline(_, _ string) string                   { return s.cfg.LogOnelineResult }
func (s *stubRepo) ConflictDiff() string                            { return s.cfg.ConflictDiff }
func (s *stubRepo) RemoteURL() string                               { return s.cfg.RemoteURL }
func (s *stubRepo) DetectDefaultBranch() string                     { return s.cfg.DefaultBranch }
func (s *stubRepo) RecentChangedFiles(_ int) string                 { return s.cfg.RecentChangedFiles }
func (s *stubRepo) GetCIFailureLog(_ context.Context, _ int) string { return s.cfg.CIFailureLog }

// --- Branch lifecycle ---

func (s *stubRepo) SyncWorktreeBase(_ context.Context, _ []string) error {
	return s.cfg.SyncWorktreeBaseErr
}

// BranchForTask mirrors production's contract: prepare-for-next-task (resets
// branchRenamed), then rename the branch for the task. Returns the resulting
// branch name.
func (s *stubRepo) BranchForTask(_ context.Context, taskID, title string, meta BranchTaskMeta) (string, error) {
	s.branchForTaskCalls++
	s.branchForTaskStackParents = meta.CompletedBranches
	s.PrepareForNextTask(taskID, "")
	if s.cfg.BranchForTaskErr != nil {
		return "", s.cfg.BranchForTaskErr
	}
	if s.cfg.BranchForTaskResult != "" {
		s.worktreeBranch = s.cfg.BranchForTaskResult
		s.branchRenamed = true
		return s.worktreeBranch, nil
	}
	if meta.Branch != "" {
		s.worktreeBranch = meta.Branch
		s.branchRenamed = true
		return s.worktreeBranch, nil
	}
	if err := s.RenameBranchForTask(title, taskID); err != nil {
		return "", err
	}
	if s.worktreeBranch == "" {
		// Fall back to a deterministic derived name so callers always get
		// something back.
		s.worktreeBranch = "ralph/" + taskID
		s.branchRenamed = true
	}
	return s.worktreeBranch, nil
}

func (s *stubRepo) PrepareForNextTask(_, _ string) {
	s.branchRenamed = false
	s.prevBranch = s.worktreeBranch
}

func (s *stubRepo) ResetToDefaultBranch() {
	s.branchRenamed = false
}

func (s *stubRepo) RenameBranchForTask(taskDesc, taskID string) error {
	if s.cfg.RenameBranchForTaskErr != nil {
		return s.cfg.RenameBranchForTaskErr
	}
	if s.branchRenamed || s.worktreeBranch == "" || taskDesc == "" {
		return nil
	}
	slug := Slugify(taskDesc)
	if slug == "" {
		return nil
	}
	s.worktreeBranch = BranchName(taskID, slug)
	s.branchRenamed = true
	return nil
}

func (s *stubRepo) RenameBranchTo(name string) {
	if s.branchRenamed || s.worktreeBranch == "" || name == "" {
		return
	}
	s.worktreeBranch = name
	s.branchRenamed = true
}

func (s *stubRepo) SetPrevBranch(branch string) {
	s.prevBranch = branch
}

func (s *stubRepo) SetAdoptedStackBranch(branch string) {
	s.adoptedStackBranch = branch
}

// --- Tag operations (no-ops in the stub; tags have no observable effect
// outside real git) ---

func (s *stubRepo) TagTaskStart(_ string) {}
func (s *stubRepo) TagTaskEnd(_ string)   {}

// --- Commit operations ---

// CommitAll advances the fake head so subsequent HeadRev calls reflect that
// a commit was made.
func (s *stubRepo) CommitAll(_ string) {
	s.headMu.Lock()
	defer s.headMu.Unlock()
	s.commitSeq++
	s.headRev = fmt.Sprintf("stub-head-%d", s.commitSeq)
}

func (s *stubRepo) EmptyCommit(_ string) {
	s.headMu.Lock()
	defer s.headMu.Unlock()
	s.commitSeq++
	s.headRev = fmt.Sprintf("stub-head-%d", s.commitSeq)
}

// --- Sync / push operations ---

func (s *stubRepo) EnsureUpToDate(_ context.Context) error { return s.cfg.EnsureUpToDateErr }
func (s *stubRepo) Push(_ context.Context) error           { return s.cfg.PushErr }

// Ship returns the prescribed outcome. Tests that want observable post-Ship
// state (e.g. a findable PR) configure cfg.GitHub.PRs accordingly.
func (s *stubRepo) Ship(_ context.Context, _ ShipOpts) (ShipResult, error) {
	return s.cfg.Ship, s.cfg.ShipErr
}

func (s *stubRepo) PushAndCreatePR(_ context.Context, _, _, _ string) (int, error) {
	return s.cfg.PushAndCreatePRNum, s.cfg.PushAndCreatePRErr
}

// --- Reviewer / review operations: delegate to gh where possible ---

func (s *stubRepo) DetectActiveReviewers(ctx context.Context) ([]Reviewer, error) {
	nwo := NWOFromRemote(s.cfg.RemoteURL)
	if nwo == "" {
		return nil, nil
	}
	return s.gh.DetectActiveReviewers(ctx, nwo)
}

func (s *stubRepo) PollReview(ctx context.Context, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	nwo := NWOFromRemote(s.cfg.RemoteURL)
	if nwo == "" {
		return nil, nil
	}
	return s.gh.PollReview(ctx, nwo, botUsername, prNumber, timeout)
}

func (s *stubRepo) ReplyToAndResolveComments(_ context.Context, _ int, _ []ReviewComment) error {
	s.replyToAndResolveCommentsCalls++
	return s.cfg.ReplyToAndResolveCommentsErr
}

// --- Resume / merge operations ---

func (s *stubRepo) ResumeTask(_ context.Context, _ ResumeTaskMeta, _ ResumeTaskOpts) (ResumeTaskResult, error) {
	return s.cfg.ResumeTaskResult, s.cfg.ResumeTaskErr
}

func (s *stubRepo) MergeWithRetry(_ context.Context, _ MergeRetryOpts) (bool, error) {
	return s.cfg.MergeRetrySucceeds, s.cfg.MergeRetryErr
}

func (s *stubRepo) FlushUnpushedWork(_ context.Context, _, _ string, _ bool) (bool, error) {
	s.flushUnpushedWorkCalls++
	return s.cfg.FlushMerged, s.cfg.FlushErr
}

// PostMergeUpdateMain is a no-op in the stub: production fast-forwards the
// local main branch, but the stub has no local refs to update.
func (s *stubRepo) PostMergeUpdateMain() {}

func (s *stubRepo) MergeStack(_ context.Context, opts MergeStackOpts) (MergeStackResult, error) {
	s.mergeStackCalls++
	s.mergeStackTopPR = opts.TopPR
	return s.cfg.MergeStackResult, s.cfg.MergeStackErr
}

// --- Remote branch operations ---

func (s *stubRepo) FetchBranch(_ string) error    { return s.cfg.FetchBranchErr }
func (s *stubRepo) CheckoutRemoteBranch(_ string) {}

func (s *stubRepo) RemoteBranchHasCommits(branch string) bool {
	return s.cfg.RemoteBranchHasCommits[branch]
}

func (s *stubRepo) RemoteBranchIsOnMain(branch string) bool {
	return s.cfg.RemoteBranchIsOnMain[branch]
}

func (s *stubRepo) BranchIsAheadOfMain(branch string) bool {
	return s.cfg.BranchIsAheadOfMain[branch]
}

func (s *stubRepo) BranchHasUnmergedWork(branch string) bool {
	return s.cfg.BranchHasUnmergedWork[branch]
}

func (s *stubRepo) BranchIsAncestorOfMain(branch string) bool {
	return s.cfg.BranchIsAncestorOfMain[branch]
}

func (s *stubRepo) IsCommitAncestorOf(_, _ string) bool {
	return !s.cfg.CommitDroppedFromBranch
}

func (s *stubRepo) DeleteRemoteBranchByName(_ string) error {
	return s.cfg.DeleteRemoteBranchByNameErr
}

// --- Init / worktree lifecycle ---

func (s *stubRepo) Init(_ context.Context) error          { return s.cfg.InitErr }
func (s *stubRepo) InitTask(_ context.Context) error      { return s.cfg.InitErr }
func (s *stubRepo) SetupWorktree(_ context.Context) error { return nil }

func (s *stubRepo) CheckBranchNamespaceSquat(_ context.Context) error {
	return s.cfg.CheckBranchNamespaceSquatErr
}

// RemoveWorktree records the call; there is no real worktree to remove.
func (s *stubRepo) RemoveWorktree() { s.removeWorktreeCalls++ }

func (s *stubRepo) RemoveWorktreeForBranch(branch string) {
	s.removeWorktreeForBranchCalls++
	s.removedWorktreeForBranch = branch
}

// --- GitHub availability and PR listing ---

func (s *stubRepo) GitHubAvailable() bool {
	return s.gh.Available()
}

func (s *stubRepo) ListAllPRs(ctx context.Context, workDir string) ([]PRInfo, error) {
	return s.gh.ListAllPRs(ctx, workDir)
}

func (s *stubRepo) ListOpenPRs(ctx context.Context, workDir string) ([]PRInfo, error) {
	return s.gh.ListOpenPRs(ctx, workDir)
}

func (s *stubRepo) PingGitHub(ctx context.Context) error {
	return s.gh.Ping(ctx)
}
