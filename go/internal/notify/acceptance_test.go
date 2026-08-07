package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// captureDialog installs a fake dialog backend returning the given output and
// error, and records the argv it was called with. It also pins a non-empty
// osascript path so the dialog path is exercised on any platform.
func captureDialog(t *testing.T, out string, err error) *[]notifyCall {
	t.Helper()
	calls := &[]notifyCall{}
	prev := SetDialogRunner(func(name string, args ...string) (string, error) {
		*calls = append(*calls, notifyCall{name: name, args: args})
		return out, err
	})
	t.Cleanup(func() { SetDialogRunner(prev) })
	prevPath := SetOsascriptPath("/usr/bin/osascript")
	t.Cleanup(func() { SetOsascriptPath(prevPath) })
	return calls
}

// The countdown expiring with nobody at the keyboard must NOT skip the
// acceptance run — unattended operation is the default, so the gate runs.
// osascript reports this as a clean exit with "gave up:true".
func TestAcceptanceCountdown_Expiry_Runs(t *testing.T) {
	captureDialog(t, "button returned:, gave up:true\n", nil)

	if AcceptanceCountdown("npm run test:safari", 10*time.Second) {
		t.Error("countdown expiry must not cancel — the acceptance gate defaults to running")
	}
}

// A user clicking Cancel (or pressing Escape) skips the acceptance run.
// osascript reports this as a non-zero exit carrying error -128.
func TestAcceptanceCountdown_UserCancel_Skips(t *testing.T) {
	captureDialog(t, "0:35: execution error: User canceled. (-128)\n", fmt.Errorf("exit status 1"))

	if !AcceptanceCountdown("npm run test:safari", 10*time.Second) {
		t.Error("an explicit user cancel must skip the acceptance run")
	}
}

// A user clicking the run button proceeds immediately rather than waiting out
// the countdown.
func TestAcceptanceCountdown_RunButton_Runs(t *testing.T) {
	captureDialog(t, "button returned:Run now, gave up:false\n", nil)

	if AcceptanceCountdown("npm run test:safari", 10*time.Second) {
		t.Error("choosing the run button must not be treated as a cancel")
	}
}

// A broken dialog backend (osascript missing, permission error) must not be
// mistaken for a cancel — an unattended loop still runs its acceptance suite.
func TestAcceptanceCountdown_DialogFailure_Runs(t *testing.T) {
	captureDialog(t, "", fmt.Errorf("fork/exec: no such file or directory"))

	if AcceptanceCountdown("npm run test:safari", 10*time.Second) {
		t.Error("a dialog backend failure must default to running, not skipping")
	}
}

// With no dialog backend available at all (non-macOS host), the gate runs
// without attempting to show anything.
func TestAcceptanceCountdown_NoDialogBackend_RunsWithoutPrompting(t *testing.T) {
	calls := captureDialog(t, "button returned:, gave up:true\n", nil)
	prevPath := SetOsascriptPath("")
	t.Cleanup(func() { SetOsascriptPath(prevPath) })

	if AcceptanceCountdown("npm run test:safari", 10*time.Second) {
		t.Error("missing dialog backend must default to running")
	}
	if len(*calls) != 0 {
		t.Errorf("expected no dialog invocation without a backend, got %v", *calls)
	}
}

// The dialog the user sees names the command about to run and auto-dismisses
// after the configured countdown, so a present user knows what they are vetoing
// and how long they have.
func TestAcceptanceCountdown_DialogCarriesCommandAndCountdown(t *testing.T) {
	calls := captureDialog(t, "button returned:, gave up:true\n", nil)

	AcceptanceCountdown("npm run test:safari", 25*time.Second)

	if len(*calls) != 1 {
		t.Fatalf("expected 1 dialog invocation, got %d", len(*calls))
	}
	script := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(script, "giving up after 25") {
		t.Errorf("dialog must auto-dismiss after the configured countdown, got %q", script)
	}
	if !strings.Contains(script, "npm run test:safari") {
		t.Errorf("dialog must name the acceptance command, got %q", script)
	}
	if !strings.Contains(script, "cancel to skip") {
		t.Errorf("dialog must tell the user cancelling skips the run, got %q", script)
	}
}

// A sub-second countdown still produces a valid AppleScript delay: "giving up
// after 0" would dismiss the dialog before it renders, giving the user no
// chance to cancel.
func TestAcceptanceCountdown_SubSecondCountdown_ClampsToOneSecond(t *testing.T) {
	calls := captureDialog(t, "button returned:, gave up:true\n", nil)

	AcceptanceCountdown("make accept", 100*time.Millisecond)

	if len(*calls) != 1 {
		t.Fatalf("expected 1 dialog invocation, got %d", len(*calls))
	}
	script := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(script, "giving up after 1") {
		t.Errorf("sub-second countdown must clamp to 1s, got %q", script)
	}
}

// A multi-line acceptance command must not break the single-line AppleScript
// string literal the dialog is built from.
func TestAcceptanceCountdown_CommandNewlinesDoNotBreakScript(t *testing.T) {
	calls := captureDialog(t, "button returned:, gave up:true\n", nil)

	AcceptanceCountdown("make build\nmake accept", 10*time.Second)

	if len(*calls) != 1 {
		t.Fatalf("expected 1 dialog invocation, got %d", len(*calls))
	}
	var script string
	for i, a := range (*calls)[0].args {
		if a == "-e" && i+1 < len((*calls)[0].args) {
			script = (*calls)[0].args[i+1]
		}
	}
	if strings.Contains(script, "\n") {
		t.Errorf("raw newline leaked into the AppleScript string: %q", script)
	}
	if !strings.Contains(script, "make build make accept") {
		t.Errorf("expected newline replaced with a space, got %q", script)
	}
}
