package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
)

// failingBackend reports an error from every poll, counting the attempts so a
// test can assert how often the loop retried.
type failingBackend struct {
	testutil.StubBackend
	polls int
}

func (b *failingBackend) HasRemaining() (bool, error) {
	b.polls++
	return false, errors.New("checksum error")
}

// Verifies that a failing poll does not reset to the base interval but walks
// the backoff schedule, so a backend that cannot answer is retried at a falling
// rate rather than every 5s forever. This is the whole point of the change:
// against a wedged Dolt server each poll strands a ~3MB temp file, so the retry
// rate is what bounds the damage.
func TestPollBackoff_WalksScheduleThenHolds(t *testing.T) {
	l := &Loop{
		cfg: Config{PollFailureBackoffs: []time.Duration{
			time.Second, 2 * time.Second, 3 * time.Second,
		}},
	}

	if got := l.pollBackoff(); got != waitPollInterval {
		t.Errorf("healthy poll: got %s, want base interval %s", got, waitPollInterval)
	}

	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second}
	for i, w := range want {
		l.consecutivePollFailures = i + 1
		if got := l.pollBackoff(); got != w {
			t.Errorf("after %d failures: got %s, want %s", i+1, got, w)
		}
	}
}

// Verifies that a successful poll clears the failure run, so a backend that
// comes back returns to the base interval immediately rather than staying
// throttled for the rest of the session.
func TestPollBackoff_ResetsOnSuccess(t *testing.T) {
	_, st := setupTestDir(t)
	l := &Loop{
		state:                   st,
		logger:                  logging.New(nil),
		taskBackend:             &testutil.StubBackend{Remaining: 1},
		consecutivePollFailures: 4,
		firstPollFailureAt:      time.Now().Add(-time.Minute),
		pollFailureNotified:     true,
	}

	found, done := l.pollForTasks()
	if !found || done {
		t.Fatalf("expected a healthy backend to report found: found=%v done=%v", found, done)
	}
	if l.consecutivePollFailures != 0 {
		t.Errorf("failure count not cleared: got %d, want 0", l.consecutivePollFailures)
	}
	if !l.firstPollFailureAt.IsZero() {
		t.Error("outage start not cleared after recovery")
	}
	if l.pollFailureNotified {
		t.Error("notification latch not cleared after recovery, so a later outage would be silent")
	}
	if got := l.pollBackoff(); got != waitPollInterval {
		t.Errorf("after recovery: got %s, want base interval %s", got, waitPollInterval)
	}
}

// Verifies that a short outage does not halt the loop — a backend restart must
// be survivable, or every beads upgrade would stop every loop on the machine.
func TestPollFailures_ShortOutageDoesNotHalt(t *testing.T) {
	_, st := setupTestDir(t)
	backend := &failingBackend{}
	l := &Loop{
		state:       st,
		logger:      logging.New(nil),
		taskBackend: backend,
		cfg:         Config{PollFailureHaltAfter: time.Hour},
	}

	found, done := l.pollForTasks()
	if found {
		t.Error("expected found=false when the backend errors")
	}
	if done {
		t.Error("expected a single failure not to halt the loop")
	}
	if l.consecutivePollFailures != 1 {
		t.Errorf("failure count: got %d, want 1", l.consecutivePollFailures)
	}
}

// Verifies the loop gives up once the backend has been failing longer than
// PollFailureHaltAfter, and records the stop in state. Without this the loop
// retries forever: the real incident ran 3.5 days and 212,135 failed polls.
func TestPollFailures_HaltsAfterSustainedOutage(t *testing.T) {
	_, st := setupTestDir(t)
	backend := &failingBackend{}
	l := &Loop{
		state:       st,
		logger:      logging.New(nil),
		taskBackend: backend,
		cfg:         Config{PollFailureHaltAfter: 30 * time.Minute},
	}

	// First failure opens the outage window.
	if _, done := l.pollForTasks(); done {
		t.Fatal("expected the first failure not to halt")
	}
	// Backdate the window past the limit rather than sleeping for it.
	l.firstPollFailureAt = time.Now().Add(-31 * time.Minute)

	_, done := l.pollForTasks()
	if !done {
		t.Fatal("expected the loop to halt once the outage exceeded PollFailureHaltAfter")
	}
	if got, _ := st.Read("status"); got != "stopped" {
		t.Errorf("status: got %q, want %q", got, "stopped")
	}
}

// Verifies waitForTasks actually stops rather than spinning when the backend
// stays down. The context timeout is the failsafe: if the loop ignored the halt
// signal the test would hang until it fired and the poll count would be far
// higher.
func TestWaitForTasks_StopsOnSustainedFailure(t *testing.T) {
	_, st := setupTestDir(t)
	backend := &failingBackend{}
	l := &Loop{
		state:       st,
		logger:      logging.New(nil),
		taskBackend: backend,
		cfg: Config{
			// Zero delays keep the test fast; the schedule's shape is covered
			// by TestPollBackoff_WalksScheduleThenHolds.
			PollFailureBackoffs:  []time.Duration{0},
			PollFailureHaltAfter: time.Millisecond,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	found := l.waitForTasks(ctx)

	if found {
		t.Error("expected found=false when the backend never answers")
	}
	if ctx.Err() != nil {
		t.Fatal("waitForTasks ran until the context expired instead of halting on its own")
	}
	if got, _ := st.Read("status"); got != "stopped" {
		t.Errorf("status: got %q, want %q", got, "stopped")
	}
	if backend.polls < 1 {
		t.Errorf("expected at least one poll, got %d", backend.polls)
	}
}
