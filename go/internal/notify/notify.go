package notify

import (
	"fmt"
	"io"
	"os"
)

// writer is the output target for notifications. Defaults to os.Stdout.
// Tests override this to capture output without writing to the terminal.
var writer io.Writer = os.Stdout

// TaskMerged sends an iTerm2 OSC 9 terminal notification for a merged task.
// The notification appears as a macOS notification when the terminal is not
// focused, letting the user monitor progress passively.
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
