package loop

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// compactingBackend records Compact calls so a test can assert whether
// maintenance ran and with what retention.
type compactingBackend struct {
	testutil.StubBackend
	calls   int
	gotDays int
	err     error
}

func (b *compactingBackend) Compact(days int) error {
	b.calls++
	b.gotDays = days
	return b.err
}

func newMaintenanceLoop(t *testing.T, backend tasks.Backend, interval time.Duration) (*Loop, string) {
	t.Helper()
	ralphDir := t.TempDir()
	return &Loop{
		logger:      logging.New(nil),
		taskBackend: backend,
		cfg: Config{
			Dirs:                 workctx.WorkContext{RalphDir: ralphDir},
			StoreCompactInterval: interval,
		},
	}, filepath.Join(ralphDir, storeCompactStampFile)
}

// Verifies a store that has never been compacted is compacted on first idle,
// and that the retention window is the one we intend to keep.
func TestMaybeCompact_RunsWhenNeverCompacted(t *testing.T) {
	backend := &compactingBackend{}
	l, stamp := newMaintenanceLoop(t, backend, time.Hour)

	l.maybeCompactStore()

	if backend.calls != 1 {
		t.Fatalf("Compact calls: got %d, want 1", backend.calls)
	}
	if backend.gotDays != storeCompactRetainDays {
		t.Errorf("retention: got %d days, want %d", backend.gotDays, storeCompactRetainDays)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("expected a compaction stamp to be written: %v", err)
	}
}

// Verifies compaction is skipped while the interval has not elapsed, so an
// idle loop does not re-run branch surgery on every wait.
func TestMaybeCompact_SkipsWhenRecent(t *testing.T) {
	backend := &compactingBackend{}
	l, stamp := newMaintenanceLoop(t, backend, 7*24*time.Hour)
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644); err != nil {
		t.Fatal(err)
	}

	l.maybeCompactStore()

	if backend.calls != 0 {
		t.Errorf("expected no compaction within the interval, got %d calls", backend.calls)
	}
}

// Verifies compaction runs again once the interval has elapsed.
func TestMaybeCompact_RunsWhenStampIsOld(t *testing.T) {
	backend := &compactingBackend{}
	l, stamp := newMaintenanceLoop(t, backend, time.Hour)
	old := time.Now().Add(-2 * time.Hour).Unix()
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(old, 10)), 0o644); err != nil {
		t.Fatal(err)
	}

	l.maybeCompactStore()

	if backend.calls != 1 {
		t.Errorf("expected compaction after the interval elapsed, got %d calls", backend.calls)
	}
}

// Verifies a failed compaction does not stamp, so the next idle retries it
// instead of silently skipping maintenance for another full interval.
func TestMaybeCompact_FailureDoesNotStamp(t *testing.T) {
	backend := &compactingBackend{err: errors.New("dolt gc failed")}
	l, stamp := newMaintenanceLoop(t, backend, time.Hour)

	l.maybeCompactStore()

	if backend.calls != 1 {
		t.Fatalf("Compact calls: got %d, want 1", backend.calls)
	}
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Error("a failed compaction must not write a stamp, or the retry is lost")
	}
}

// Verifies a backend with no Compact method is simply left alone rather than
// panicking on the type assertion.
func TestMaybeCompact_NonCompactingBackendIsNoop(t *testing.T) {
	l, stamp := newMaintenanceLoop(t, &testutil.StubBackend{}, time.Hour)

	l.maybeCompactStore()

	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Error("expected no stamp for a backend that cannot compact")
	}
}

// Verifies a negative interval turns maintenance off entirely.
func TestMaybeCompact_NegativeIntervalDisables(t *testing.T) {
	backend := &compactingBackend{}
	l, _ := newMaintenanceLoop(t, backend, -1)

	l.maybeCompactStore()

	if backend.calls != 0 {
		t.Errorf("expected compaction disabled, got %d calls", backend.calls)
	}
}
