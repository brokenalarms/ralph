package main

import (
	"io"
	"os"
	"strings"
	"testing"
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
