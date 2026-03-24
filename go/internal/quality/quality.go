package quality

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding represents a quality issue detected in a file.
type Finding struct {
	File    string
	Message string
	Score   int
}

const DefaultRefactorThreshold = 200

const (
	CheckAnyType      = "any-type"
	CheckOversizedFile = "oversized-file"
	CheckSilentCatch  = "silent-catch"
	CheckConsoleLog   = "console-log"
	CheckDebugPrint   = "debug-print"
	CheckTodoCount    = "todo-count"
)

// AllChecks lists every quality check name.
var AllChecks = []string{
	CheckAnyType,
	CheckOversizedFile,
	CheckSilentCatch,
	CheckConsoleLog,
	CheckDebugPrint,
	CheckTodoCount,
}

// Options controls which quality checks run during Assess.
type Options struct {
	DisabledChecks map[string]bool
}

func (o *Options) enabled(name string) bool {
	if o == nil || o.DisabledChecks == nil {
		return true
	}
	return !o.DisabledChecks[name]
}

var (
	anyTypeRe     = regexp.MustCompile(`\bany\b`)
	silentCatchRe = regexp.MustCompile(`catch\s*\([^)]*\)\s*\{\s*\}`)
	consoleLogRe  = regexp.MustCompile(`console\.(log|debug|warn|error)\s*\(`)
	debugPrintRe  = regexp.MustCompile(`fmt\.(Println|Printf|Print)\s*\(`)
	todoRe        = regexp.MustCompile(`(?i)\bTODO\b`)
)

// Assess scans changed files for quality issues and returns the total
// score and a findings report. Pass nil opts to run all checks.
func Assess(workDir, findingsFile string, opts *Options, files ...string) (int, error) {
	totalScore := 0
	var allFindings []string

	for _, relPath := range files {
		if shouldExclude(relPath) {
			continue
		}
		absPath := filepath.Join(workDir, relPath)
		data, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		content := string(data)
		lines := strings.Split(content, "\n")

		var fileFindings []string

		if opts.enabled(CheckAnyType) && isJSOrTS(relPath) {
			count := len(anyTypeRe.FindAllString(content, -1))
			if count > 0 {
				score := count * 3
				totalScore += score
				fileFindings = append(fileFindings,
					fmt.Sprintf("  - %dx untyped `any` usage", count))
			}
		}

		if opts.enabled(CheckOversizedFile) && len(lines) > 500 {
			over := len(lines) - 500
			score := over / 10
			if score < 1 {
				score = 1
			}
			totalScore += score
			fileFindings = append(fileFindings,
				fmt.Sprintf("  - %d lines (%d over 500-line threshold)", len(lines), over))
		}

		if opts.enabled(CheckSilentCatch) && isJSOrTS(relPath) {
			matches := silentCatchRe.FindAllString(content, -1)
			if len(matches) > 0 {
				score := len(matches) * 5
				totalScore += score
				fileFindings = append(fileFindings,
					fmt.Sprintf("  - %d silent catch block(s)", len(matches)))
			}
		}

		if opts.enabled(CheckConsoleLog) && isJSOrTS(relPath) {
			matches := consoleLogRe.FindAllString(content, -1)
			if len(matches) > 0 {
				score := len(matches) * 2
				totalScore += score
				fileFindings = append(fileFindings,
					fmt.Sprintf("  - %d console.log/debug/warn/error call(s)", len(matches)))
			}
		}

		if opts.enabled(CheckDebugPrint) && isGo(relPath) {
			matches := debugPrintRe.FindAllString(content, -1)
			if len(matches) > 0 {
				score := len(matches) * 2
				totalScore += score
				fileFindings = append(fileFindings,
					fmt.Sprintf("  - %d fmt.Print/Println/Printf call(s)", len(matches)))
			}
		}

		if opts.enabled(CheckTodoCount) {
			matches := todoRe.FindAllString(content, -1)
			if len(matches) >= 3 {
				score := len(matches)
				totalScore += score
				fileFindings = append(fileFindings,
					fmt.Sprintf("  - %d TODO comments", len(matches)))
			}
		}

		if len(fileFindings) > 0 {
			allFindings = append(allFindings, relPath+":")
			allFindings = append(allFindings, fileFindings...)
		}
	}

	if findingsFile != "" {
		if len(allFindings) > 0 {
			os.WriteFile(findingsFile, []byte(strings.Join(allFindings, "\n")+"\n"), 0o644)
		} else {
			os.WriteFile(findingsFile, nil, 0o644)
		}
	}

	return totalScore, nil
}

// FormatFindings reads a findings file and returns its content as a string
// suitable for inclusion in a refactor prompt.
func FormatFindings(findingsFile string) string {
	data, err := os.ReadFile(findingsFile)
	if err != nil || len(data) == 0 {
		return ""
	}
	return string(data)
}

func isJSOrTS(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".js" || ext == ".ts" || ext == ".tsx" || ext == ".jsx"
}

func isGo(path string) bool {
	return strings.ToLower(filepath.Ext(path)) == ".go"
}

var legacyScripts = map[string]bool{
	"ralph.sh": true,
}

func shouldExclude(relPath string) bool {
	base := filepath.Base(relPath)

	if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".bats") {
		return true
	}

	if strings.HasPrefix(relPath, "test/") || strings.HasPrefix(relPath, "test"+string(filepath.Separator)) {
		return true
	}

	if legacyScripts[base] {
		return true
	}

	if base == "main.go" && strings.Contains(relPath, "cmd/") {
		return true
	}

	return false
}
