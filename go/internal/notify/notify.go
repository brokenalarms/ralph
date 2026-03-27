package notify

import (
	"fmt"
	"io"
	"os"
)

// writer is the output target for notifications. Defaults to os.Stdout.
// Tests override this to capture output without writing to the terminal.
var writer io.Writer = os.Stdout

// SetWriter overrides the notification output target. Returns the previous
// writer so callers can restore it.
func SetWriter(w io.Writer) io.Writer {
	prev := writer
	writer = w
	return prev
}

// TaskMerged sends an iTerm2 OSC 9 terminal notification for a merged task.
// The notification appears as a macOS notification when the terminal is not
// focused, letting the user monitor progress passively.
// TaskCompleted sends an iTerm2 OSC 9 terminal notification for a completed
// task iteration. Includes bead ID, title, and the agent's completion summary.
func TaskCompleted(taskID, title, summary string) {
	msg := "Task done"
	if taskID != "" {
		msg += ": [" + taskID + "]"
	}
	if title != "" {
		msg += " " + title
	}
	if summary != "" {
		msg += " — " + summary
	}
	fmt.Fprintf(writer, "\033]9;%s\007", msg)
}

func TaskMerged(taskID, title string) {
	msg := "Task merged"
	if taskID != "" {
		msg += ": [" + taskID + "]"
	}
	if title != "" {
		msg += " " + title
	}
	fmt.Fprintf(writer, "\033]9;%s\007", msg)
}
