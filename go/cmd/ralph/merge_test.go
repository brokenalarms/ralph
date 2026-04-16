package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
)

// Proves ralph merge -h includes a FLAGS section listing --no-ci-wait with a
// help string that tells users when to use it (Actions down, checks won't run).
func TestMergeHelp_NoCIWaitFlagDocumented(t *testing.T) {
	out := captureMergeHelp(t)

	if !strings.Contains(out, "FLAGS:") {
		t.Error("merge help should contain a FLAGS: section")
	}
	if !strings.Contains(out, "--no-ci-wait") {
		t.Error("merge help should list --no-ci-wait flag")
	}
	if !strings.Contains(out, "GitHub Actions") {
		t.Error("--no-ci-wait help should mention GitHub Actions")
	}
	if !strings.Contains(out, "never run") {
		t.Error("--no-ci-wait help should mention that checks will never run")
	}
}

// Proves ralph merge -h explains that infra-only CI failures (zero job steps)
// are detected and passed automatically — the user does not need --no-ci-wait
// for this case. This is the key behavior added by ralph-pdpt.
func TestMergeHelp_InfraFailureFallthroughDocumented(t *testing.T) {
	out := captureMergeHelp(t)

	if !strings.Contains(out, "zero job steps") {
		t.Error("merge help should mention zero job steps as the infra-failure signal")
	}
	if !strings.Contains(out, "automatically") {
		t.Error("merge help should state that infra failures proceed automatically")
	}
	// Must make clear this happens without requiring the flag.
	if !strings.Contains(out, "no flag") && !strings.Contains(out, "without --no-ci-wait") && !strings.Contains(out, "does not require") {
		t.Error("merge help should clarify that infra-failure fallthrough requires no flag")
	}
}

// Proves ralph merge -h documents --admin-merge-on-ci-infra-failure and its scope:
// branch-protection bypass only when CI failure is classified as infra (zero job steps).
func TestMergeHelp_AdminMergeOnCIInfraFailureFlagDocumented(t *testing.T) {
	out := captureMergeHelp(t)

	if !strings.Contains(out, "--admin-merge-on-ci-infra-failure") {
		t.Error("merge help should list --admin-merge-on-ci-infra-failure flag")
	}
	if !strings.Contains(out, "zero job steps") {
		t.Error("--admin-merge-on-ci-infra-failure help should mention zero job steps as the classification signal")
	}
	if !strings.Contains(out, "real test failures") {
		t.Error("--admin-merge-on-ci-infra-failure help should clarify it has no effect on real test failures")
	}
}

// Passing an old flag name (--admin-on-infra-failure) that was renamed must
// return an unknown-flag error, not be silently ignored.
func TestMergeUnknownFlag_ReturnsError(t *testing.T) {
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	log := logging.New(os.Stderr)

	sub := config.Subcommand{Name: "merge", Dir: t.TempDir(), Args: []string{"321", "--admin-on-infra-failure"}}
	rc := handleMerge(sub, log)

	w.Close()
	os.Stderr = old
	data, _ := io.ReadAll(r)
	out := string(data)

	if rc == 0 {
		t.Error("expected non-zero return code for unknown flag --admin-on-infra-failure")
	}
	if !strings.Contains(out, "unknown flag") {
		t.Errorf("expected 'unknown flag' error message in stderr, got: %q", out)
	}
}

func captureMergeHelp(t *testing.T) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	printMergeUsage()
	w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return string(data)
}
