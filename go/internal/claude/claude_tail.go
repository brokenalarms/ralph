package claude

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// filterStreamJSON tails the raw log file from its current end, extracting
// human-readable content from Claude's stream-json format into logPath.
// It keeps reading until stop is closed, then drains any final output.
func filterStreamJSON(rawLogPath, logPath, workDir string, verbose bool, stop <-chan struct{}) {
	logOut, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer logOut.Close()

	f, err := os.Open(rawLogPath)
	if err != nil {
		return
	}
	defer f.Close()

	// Start from end of file (like tail -f -n 0) so we only see new output.
	if _, err := f.Seek(0, 2); err != nil {
		return
	}

	var remainder string
	buf := make([]byte, 64*1024)

	batcher := NewToolBatcher(5*time.Second, workDir)
	batcher.SetVerbose(verbose)

	emitLines := func(lines []string) {
		for _, out := range lines {
			fmt.Fprintf(logOut, "%s\n", out)
		}
	}

	processChunk := func(data string) string {
		for {
			idx := strings.IndexByte(data, '\n')
			if idx < 0 {
				return data
			}
			line := data[:idx]
			data = data[idx+1:]
			if text := extractStreamText(line); text != "" {
				for _, tl := range strings.Split(text, "\n") {
					if tl != "" {
						emitLines(batcher.ProcessLine(tl))
					}
				}
			}
		}
	}

	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			remainder = processChunk(remainder + string(buf[:n]))
		}

		if readErr != nil || n == 0 {
			emitLines(batcher.FlushIfExpired())
			select {
			case <-stop:
				for {
					n2, _ := f.Read(buf)
					if n2 == 0 {
						break
					}
					remainder = processChunk(remainder + string(buf[:n2]))
				}
				emitLines(batcher.Flush())
				return
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
}

// FilterStream tails a raw log file and writes formatted, colored output to
// stdout. Intended for use as the tmux stream pane via `ralph filter-stream`.
// Blocks until the process is killed (tmux manages its lifecycle).
func FilterStream(rawLogPath, workDir string, verbose bool) {
	f, err := os.Open(rawLogPath)
	if err != nil {
		return
	}
	defer f.Close()

	if _, err := f.Seek(0, 2); err != nil {
		return
	}

	var remainder string
	buf := make([]byte, 64*1024)
	batcher := NewToolBatcher(5*time.Second, workDir)
	batcher.SetVerbose(verbose)

	emitLines := func(lines []string) {
		for _, out := range lines {
			fmt.Fprintln(os.Stdout, out)
		}
	}

	processChunk := func(data string) string {
		for {
			idx := strings.IndexByte(data, '\n')
			if idx < 0 {
				return data
			}
			line := data[:idx]
			data = data[idx+1:]
			if text := extractStreamText(line); text != "" {
				for _, tl := range strings.Split(text, "\n") {
					if tl != "" {
						emitLines(batcher.ProcessLine(tl))
					}
				}
			}
		}
	}

	for {
		n, _ := f.Read(buf)
		if n > 0 {
			remainder = processChunk(remainder + string(buf[:n]))
		}
		if n == 0 {
			emitLines(batcher.FlushIfExpired())
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// startTailGoroutine follows new data appended to path and writes it to
// stdout, similar to tail -f -n 0. Only forwards lines prefixed with
// "[r] " — orchestrator messages are already written to stdout directly
// by the logger, so forwarding them here would cause duplication.
// Runs entirely in-process so there are no child processes to orphan.
// Returns a channel that closes when the goroutine exits.
func startTailGoroutine(path string, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()

		// Start from end of file (like tail -f -n 0).
		if _, err := f.Seek(0, 2); err != nil {
			return
		}

		var remainder string
		buf := make([]byte, 64*1024)

		processChunk := func(data string) string {
			for {
				idx := strings.IndexByte(data, '\n')
				if idx < 0 {
					return data
				}
				line := data[:idx]
				data = data[idx+1:]
				if strings.Contains(line, "[r]") || strings.Contains(line, "[signal]") {
					fmt.Fprintln(os.Stdout, line)
				}
			}
		}

		for {
			n, _ := f.Read(buf)
			if n > 0 {
				remainder = processChunk(remainder + string(buf[:n]))
			}
			if n == 0 {
				select {
				case <-stop:
					// Final drain.
					for {
						n2, _ := f.Read(buf)
						if n2 == 0 {
							// Flush any remaining partial line.
							if remainder != "" && (strings.Contains(remainder, "[r]") || strings.Contains(remainder, "[signal]")) {
								fmt.Fprintln(os.Stdout, remainder)
							}
							return
						}
						remainder = processChunk(remainder + string(buf[:n2]))
					}
				default:
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}()
	return done
}
