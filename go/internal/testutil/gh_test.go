package testutil

import (
	"context"
	"testing"
)

func TestGHStub_PRList(t *testing.T) {
	s := NewGHStub()
	s.PRListOutput = "42\tFix authentication"
	run := s.Runner()

	out, err := run(context.Background(), "gh", "pr", "list", "--head", "feature-branch")
	if err != nil {
		t.Fatalf("pr list: %v", err)
	}
	if out != "42\tFix authentication" {
		t.Errorf("output = %q, want %q", out, "42\tFix authentication")
	}
}

func TestGHStub_PRCreate(t *testing.T) {
	s := NewGHStub()
	s.PRCreateErr = errTest
	run := s.Runner()

	_, err := run(context.Background(), "gh", "pr", "create", "--title", "Fix bug")
	if err != errTest {
		t.Errorf("pr create err = %v, want errTest", err)
	}
}

func TestGHStub_PRMerge(t *testing.T) {
	s := NewGHStub()
	s.PRMergeOutput = "merged"
	run := s.Runner()

	out, err := run(context.Background(), "gh", "pr", "merge", "42", "--squash")
	if err != nil {
		t.Fatalf("pr merge: %v", err)
	}
	if out != "merged" {
		t.Errorf("output = %q, want %q", out, "merged")
	}
}

func TestGHStub_RecordsCalls(t *testing.T) {
	s := NewGHStub()
	run := s.Runner()

	run(context.Background(), "gh", "pr", "list")
	run(context.Background(), "gh", "pr", "checks", "42")

	calls := s.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}

	if !s.CalledWith("pr list") {
		t.Error("should match 'pr list'")
	}
	if !s.CalledWith("pr checks") {
		t.Error("should match 'pr checks'")
	}
	if s.CalledWith("pr create") {
		t.Error("should not match 'pr create'")
	}
}

func TestGHStub_UnknownSubcommand(t *testing.T) {
	s := NewGHStub()
	run := s.Runner()

	out, err := run(context.Background(), "gh", "unknown")
	if out != "" || err != nil {
		t.Errorf("unknown: got (%q, %v), want (\"\", nil)", out, err)
	}
}
