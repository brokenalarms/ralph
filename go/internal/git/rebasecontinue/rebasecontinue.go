package rebasecontinue

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options controls the behavior of the rebase-continue loop.
type Options struct {
	Auto       bool
	OursFiles  []string
	RmFiles    []string
	OntoTarget string
	OntoBase   string
}

// Run executes the rebase-continue loop in dir with the given options.
// If OntoTarget is set and no rebase is in progress, it initiates a new rebase.
// Then it loops through conflicts, resolving them per the options, until the
// rebase completes or encounters unresolvable conflicts.
func Run(dir string, opts Options) error {
	// If no rebase is in progress and no --onto target was given, there is nothing to continue.
	if opts.OntoTarget == "" && !rebaseInProgress(dir) {
		return fmt.Errorf("no rebase in progress in %s", dir)
	}

	if opts.OntoTarget != "" && !rebaseInProgress(dir) {
		if err := verifyRef(dir, opts.OntoTarget); err != nil {
			return fmt.Errorf("'%s' is not a valid ref", opts.OntoTarget)
		}

		ontoBase := opts.OntoBase
		if ontoBase == "" {
			var err error
			ontoBase, err = detectOldBase(dir, opts.OntoTarget)
			if err != nil {
				return err
			}
		} else {
			if err := verifyRef(dir, ontoBase); err != nil {
				return fmt.Errorf("'%s' is not a valid ref", ontoBase)
			}
		}

		if ontoBase == "" {
			runGitIgnoreErr(dir, "rebase", "--update-refs", opts.OntoTarget)
		} else {
			runGitIgnoreErr(dir, "rebase", "--update-refs", "--onto", opts.OntoTarget, ontoBase)
		}

		if !rebaseInProgress(dir) {
			return nil
		}
	}

	// Apply --ours for specified files before entering the loop.
	for _, f := range opts.OursFiles {
		if fileExists(filepath.Join(dir, f)) {
			runGitIgnoreErr(dir, "checkout", "--ours", f)
		}
	}

	return continueLoop(dir, opts)
}

// continueLoop stages resolved files, runs git rebase --continue, and repeats
// until the rebase completes or hits unresolvable conflicts.
func continueLoop(dir string, opts Options) error {
	for {
		conflicted := checkConflicts(dir)

		if len(conflicted) > 0 {
			if opts.Auto {
				resolvedAny := false
				for _, f := range conflicted {
					resolved, err := autoResolve(dir, f)
					if err != nil {
						return err
					}
					if resolved {
						resolvedAny = true
					}
				}
				remaining := checkConflicts(dir)
				if len(remaining) > 0 {
					return fmt.Errorf("unresolvable conflicts in: %s", strings.Join(remaining, ", "))
				}
				if resolvedAny {
					continue
				}
			} else {
				// Apply --ours for files that are conflicted at this step.
				resolvedAny := false
				for _, cf := range conflicted {
					for _, of := range opts.OursFiles {
						if cf == of && fileExists(filepath.Join(dir, cf)) {
							runGitIgnoreErr(dir, "checkout", "--ours", cf)
							runGitIgnoreErr(dir, "add", cf)
							resolvedAny = true
						}
					}
				}

				remaining := checkConflicts(dir)
				if len(remaining) > 0 {
					return fmt.Errorf("conflicts in: %s", strings.Join(remaining, ", "))
				}
				if !resolvedAny {
					return fmt.Errorf("conflicts in: %s", strings.Join(conflicted, ", "))
				}
			}
		}

		// Remove files that should stay deleted at each rebase step.
		for _, f := range opts.RmFiles {
			if fileExists(filepath.Join(dir, f)) {
				runGitIgnoreErr(dir, "rm", "-f", f)
			}
		}

		runGitIgnoreErr(dir, "add", "-A")
		result, _ := runGitOutput(dir, "rebase", "--continue")

		if strings.Contains(result, "Successfully rebased") {
			return nil
		}
		if !strings.Contains(result, "could not apply") &&
			!strings.Contains(result, "CONFLICT") &&
			!strings.Contains(result, "error:") {
			return nil
		}
	}
}

// autoResolve attempts to mechanically resolve a conflict in file f:
//   - identical content → take theirs
//   - only one side changed from base → take that side
//   - ours's changes are a subset of theirs → take theirs
//
// Returns (true, nil) if resolved, (false, nil) for genuine divergence.
func autoResolve(dir, f string) (bool, error) {
	baseContent, err := gitShow(dir, ":1:"+f)
	if err != nil {
		baseContent = ""
	}
	oursContent, err := gitShow(dir, ":2:"+f)
	if err != nil {
		return false, nil
	}
	theirsContent, err := gitShow(dir, ":3:"+f)
	if err != nil {
		return false, nil
	}

	if oursContent == theirsContent {
		runGitIgnoreErr(dir, "checkout", "--theirs", f)
		runGitIgnoreErr(dir, "add", f)
		return true, nil
	}

	oursChanged := oursContent != baseContent
	theirsChanged := theirsContent != baseContent

	if !oursChanged {
		runGitIgnoreErr(dir, "checkout", "--theirs", f)
		runGitIgnoreErr(dir, "add", f)
		return true, nil
	}
	if !theirsChanged {
		runGitIgnoreErr(dir, "checkout", "--ours", f)
		runGitIgnoreErr(dir, "add", f)
		return true, nil
	}

	// Both sides changed — check if ours's added lines are a subset of theirs.
	oursAdded := addedLines(baseContent, oursContent)
	if len(oursAdded) > 0 && allLinesIn(oursAdded, theirsContent) {
		runGitIgnoreErr(dir, "checkout", "--theirs", f)
		runGitIgnoreErr(dir, "add", f)
		return true, nil
	}

	return false, nil
}

// detectOldBase finds the last commit in target..HEAD whose cumulative changes
// are already absorbed into target (e.g. via squash merge), using reverse-patch
// application against target's tree.
func detectOldBase(dir, target string) (string, error) {
	headRef, err := gitOutputStr(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	mergeBase, err := gitOutputStr(dir, "merge-base", target, headRef)
	if err != nil {
		return "", err
	}

	commits, err := gitOutputStr(dir, "rev-list", "--reverse", mergeBase+".."+headRef)
	if err != nil || strings.TrimSpace(commits) == "" {
		return "", nil
	}

	oldBase := ""
	for _, commit := range strings.Split(strings.TrimSpace(commits), "\n") {
		commit = strings.TrimSpace(commit)
		if commit == "" {
			continue
		}

		branchFiles, _ := gitOutputStr(dir, "diff", "--name-only", mergeBase, commit)
		if strings.TrimSpace(branchFiles) == "" {
			continue
		}

		if squashMergedInto(dir, target, mergeBase, commit) {
			oldBase = commit
		} else {
			break
		}
	}

	return oldBase, nil
}

// squashMergedInto checks if the patch mergeBase→commit is already absorbed
// into target by reverse-applying it against target's tree.
func squashMergedInto(dir, target, mergeBase, commit string) bool {
	tmpIndex, err := os.CreateTemp("", "grc_squash_check.*")
	if err != nil {
		return false
	}
	tmpIndex.Close()
	defer os.Remove(tmpIndex.Name())

	readTree := exec.Command("git", "read-tree", target)
	readTree.Dir = dir
	readTree.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmpIndex.Name())
	if err := readTree.Run(); err != nil {
		return false
	}

	diffCmd := exec.Command("git", "diff", mergeBase, commit)
	diffCmd.Dir = dir

	applyCmd := exec.Command("git", "apply", "--cached", "--reverse", "--check", "-C0")
	applyCmd.Dir = dir
	applyCmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmpIndex.Name())

	pr, pw := io.Pipe()
	diffCmd.Stdout = pw
	applyCmd.Stdin = pr

	if err := diffCmd.Start(); err != nil {
		return false
	}
	if err := applyCmd.Start(); err != nil {
		pw.Close()
		return false
	}

	diffErr := diffCmd.Wait()
	pw.CloseWithError(diffErr)
	return applyCmd.Wait() == nil
}

func rebaseInProgress(dir string) bool {
	gitDir, err := gitOutputStr(dir, "rev-parse", "--git-dir")
	if err != nil {
		return false
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	_, err1 := os.Stat(filepath.Join(gitDir, "rebase-merge"))
	_, err2 := os.Stat(filepath.Join(gitDir, "rebase-apply"))
	return err1 == nil || err2 == nil
}

func checkConflicts(dir string) []string {
	cmd := exec.Command("git", "diff", "--name-only", "--diff-filter=U")
	cmd.Dir = dir
	out, _ := cmd.Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func gitShow(dir, ref string) (string, error) {
	cmd := exec.Command("git", "show", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func gitOutputStr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func runGitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runGitIgnoreErr(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Run() //nolint:errcheck
}

func verifyRef(dir, ref string) error {
	cmd := exec.Command("git", "rev-parse", "--verify", ref)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ref not found: %s", ref)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// addedLines returns lines present in b but not in a.
func addedLines(a, b string) []string {
	aLines := strings.Split(a, "\n")
	aSet := make(map[string]bool, len(aLines))
	for _, l := range aLines {
		aSet[l] = true
	}
	var added []string
	for _, l := range strings.Split(b, "\n") {
		if l != "" && !aSet[l] {
			added = append(added, l)
		}
	}
	return added
}

// allLinesIn returns true if every line in lines appears in content.
func allLinesIn(lines []string, content string) bool {
	for _, l := range lines {
		if l == "" {
			continue
		}
		if !strings.Contains(content, l) {
			return false
		}
	}
	return true
}
