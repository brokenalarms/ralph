package workctx

import (
	"path/filepath"
)

// WorkContext bundles the directory paths that modules need to locate
// project files, agent working directories, ralph state, and prompt
// templates. Passing a single struct instead of separate strings
// prevents projectDir/workDir mixups across the codebase.
type WorkContext struct {
	ProjectDir string // git project root — never the worktree
	WorkDir    string // where the agent works (worktree or project dir)
	RalphDir   string // .ralph state directory
	PromptsDir string // prompt template directory
}

// New creates a WorkContext from the project root and prompts directory.
// WorkDir defaults to ProjectDir; callers update it after worktree setup.
// RalphDir is derived as ProjectDir/.ralph and is where all logs live.
func New(projectDir, promptsDir string) WorkContext {
	return WorkContext{
		ProjectDir: projectDir,
		WorkDir:    projectDir,
		RalphDir:   filepath.Join(projectDir, ".ralph"),
		PromptsDir: promptsDir,
	}
}
