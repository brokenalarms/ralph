package testutil

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// waitPollInterval is how often the WaitFor* helpers re-check their condition.
// It is a polling cadence, not a delay the caller waits out: a helper returns
// as soon as its condition holds.
const waitPollInterval = 5 * time.Millisecond

// WaitFor polls cond until it returns true or timeout elapses, failing the
// test (t.Fatalf, naming desc) if the condition is never met.
//
// Use this instead of a fixed time.Sleep to synchronize a test with an
// asynchronous event. The call returns the instant cond holds, so it is fast
// in the common case; timeout is a bounded upper limit that guards against a
// hang, not a tuned delay that must be "long enough". desc should name the
// observable condition ("signal file to appear", "process N to exit") so a
// timeout failure is self-explanatory.
func WaitFor(t testing.TB, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, desc)
		}
		time.Sleep(waitPollInterval)
	}
}

// WaitForFile waits until path exists and is non-empty, then returns its
// whitespace-trimmed contents. Use it to read a value an external process
// writes asynchronously (e.g. a PID file) without racing the write.
func WaitForFile(t testing.TB, timeout time.Duration, path string) string {
	t.Helper()
	var content string
	WaitFor(t, timeout, "file "+path+" to exist and be non-empty", func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		trimmed := strings.TrimSpace(string(data))
		if trimmed == "" {
			return false
		}
		content = trimmed
		return true
	})
	return content
}

// WaitForSignalFile waits until path exists. Unlike WaitForFile it does not
// require contents — signal files communicate by their presence and may be
// empty.
func WaitForSignalFile(t testing.TB, timeout time.Duration, path string) {
	t.Helper()
	WaitFor(t, timeout, "signal file "+path+" to appear", func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// WaitProcessGone waits until the process identified by pid no longer exists
// (`kill -0` fails). Use it to assert a process was terminated instead of
// sleeping and hoping the kill has propagated.
func WaitProcessGone(t testing.TB, timeout time.Duration, pid string) {
	t.Helper()
	WaitFor(t, timeout, "process "+pid+" to exit", func() bool {
		return exec.Command("kill", "-0", pid).Run() != nil
	})
}
