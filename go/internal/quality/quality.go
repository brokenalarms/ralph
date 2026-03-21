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

// DefaultRefactorThreshold is the quality score threshold that triggers
// a refactor iteration, matching REFACTOR_THRESHOLD=20 in ralph.sh.
const DefaultRefactorThreshold = 20

var (
	anyTypeRe    = regexp.MustCompile(`\bany\b`)
	silentCatchRe = regexp.MustCompile(`catch\s*\([^)]*\)\s*\{\s*\}`)
	consoleLogRe  = regexp.MustCompile(`console\.(log|debug|warn|error)\s*\(`)
)

// Assess scans changed files for quality issues and returns the total
// score and a findings report. Matches ralph.sh's assess_quality.
func Assess(workDir, findingsFile string, files ...string) (int, error) {
	totalScore := 0
	var allFindings []string

	for _, relPath := range files {
		absPath := filepath.Join(workDir, relPath)
		data, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		content := string(data)
		lines := strings.Split(content, "\n")

		var fileFindings []string

		// Check for `any` type usage in TypeScript/JavaScript files.
		if isJSOrTS(relPath) {
			count := len(anyTypeRe.FindAllString(content, -1))
			if count > 0 {
				score := count * 3
				totalScore += score
				fileFindings = append(fileFindings,
					fmt.Sprintf("  - %dx untyped `any` usage", count))
			}
		}

		// Check for oversized files (over 500 lines).
		if len(lines) > 500 {
			over := len(lines) - 500
			score := over / 10
			if score < 1 {
				score = 1
			}
			totalScore += score
			fileFindings = append(fileFindings,
				fmt.Sprintf("  - %d lines (%d over 500-line threshold)", len(lines), over))
		}

		// Check for silent catches.
		if isJSOrTS(relPath) {
			matches := silentCatchRe.FindAllString(content, -1)
			if len(matches) > 0 {
				score := len(matches) * 5
				totalScore += score
				fileFindings = append(fileFindings,
					fmt.Sprintf("  - %d silent catch block(s)", len(matches)))
			}
		}

		// Check for console.log in JS/TS files.
		if isJSOrTS(relPath) {
			matches := consoleLogRe.FindAllString(content, -1)
			if len(matches) > 0 {
				score := len(matches) * 2
				totalScore += score
				fileFindings = append(fileFindings,
					fmt.Sprintf("  - %d console.log/debug/warn/error call(s)", len(matches)))
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

func isJSOrTS(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".js" || ext == ".ts" || ext == ".tsx" || ext == ".jsx"
}
