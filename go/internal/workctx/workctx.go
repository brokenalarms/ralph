package workctx

import (
	"path/filepath"

	"github.com/brokenalarms/ralph/internal/logging"
)

// WorkContext bundles the directory paths that modules need to locate
// project files, agent working directories, ralph state, and prompt
// templates. Passing a single struct instead of separate strings
// prevents projectDir/workDir mixups across the codebase.
type WorkContext struct {
	ProjectDir string // git project root — never the worktree
	WorkDir    string // where the agent works (worktree or project dir)
	RalphDir   string // .ralph state directory
	LogDir     string // stable per-project log directory (outside worktree)
	PromptsDir string // prompt template directory
}

// New creates a WorkContext from the project root and prompts directory.
// WorkDir defaults to ProjectDir; callers update it after worktree setup.
// RalphDir is derived as ProjectDir/.ralph. LogDir is a stable per-project
// path under ~/.ralph/logs/ that survives worktree recreation; falls back
// to RalphDir if the home directory is unavailable.
func New(projectDir, promptsDir string) WorkContext {
	ralphDir := filepath.Join(projectDir, ".ralph")
	logDir, err := logging.StableLogDir(projectDir)
	if err != nil {
		logDir = ralphDir
	}
	return WorkContext{
		ProjectDir: projectDir,
		WorkDir:    projectDir,
		RalphDir:   ralphDir,
		LogDir:     logDir,
		PromptsDir: promptsDir,
	}
}

// EffectiveLogDir returns LogDir when set, falling back to RalphDir.
// This lets code that constructs WorkContext directly (e.g. tests) work
// without setting LogDir explicitly.
func (w WorkContext) EffectiveLogDir() string {
	if w.LogDir != "" {
		return w.LogDir
	}
	return w.RalphDir
}
