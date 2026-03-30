package testutil

import (
	"context"
	"errors"
	"testing"
)

var errTest = errors.New("test error")

func TestExecStub_RecordsCalls(t *testing.T) {
	s := NewExecStub()
	s.Run(context.Background(), "gh", "pr", "list")
	s.Run(context.Background(), "tail", "-f", "log.txt")

	calls := s.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Name != "gh" {
		t.Errorf("call[0].Name = %q, want %q", calls[0].Name, "gh")
	}
	if calls[1].Name != "tail" {
		t.Errorf("call[1].Name = %q, want %q", calls[1].Name, "tail")
	}
}

func TestExecStub_CannedResponses(t *testing.T) {
	s := NewExecStub()
	s.On("gh pr list", "42\tFix auth", nil)
	s.On("gh", "default gh output", nil)
	s.On("tail", "", errTest)

	out, err := s.Run(context.Background(), "gh", "pr", "list", "--state", "open")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "42\tFix auth" {
		t.Errorf("output = %q, want %q", out, "42\tFix auth")
	}

	out, err = s.Run(context.Background(), "gh", "api", "repos/...")
	if out != "default gh output" {
		t.Errorf("fallback output = %q, want %q", out, "default gh output")
	}

	_, err = s.Run(context.Background(), "tail", "-f", "file.log")
	if err != errTest {
		t.Errorf("tail err = %v, want errTest", err)
	}
}

func TestExecStub_CalledWith(t *testing.T) {
	s := NewExecStub()
	s.Run(context.Background(), "gh", "pr", "list", "--head", "main")

	if !s.CalledWith("gh", "pr", "list") {
		t.Error("should match prefix 'gh pr list'")
	}
	if !s.CalledWith("gh") {
		t.Error("should match prefix 'gh'")
	}
	if s.CalledWith("tail") {
		t.Error("should not match 'tail'")
	}
	if s.CalledWith("gh", "pr", "create") {
		t.Error("should not match 'gh pr create'")
	}
}

func TestExecStub_CallCount(t *testing.T) {
	s := NewExecStub()
	s.Run(context.Background(), "gh", "pr", "list")
	s.Run(context.Background(), "gh", "pr", "create")
	s.Run(context.Background(), "tail", "-f", "log")

	if n := s.CallCount("gh"); n != 2 {
		t.Errorf("gh call count = %d, want 2", n)
	}
	if n := s.CallCount("tail"); n != 1 {
		t.Errorf("tail call count = %d, want 1", n)
	}
	if n := s.CallCount("bd"); n != 0 {
		t.Errorf("bd call count = %d, want 0", n)
	}
}

func TestExecStub_UnknownCommandReturnsEmpty(t *testing.T) {
	s := NewExecStub()
	out, err := s.Run(context.Background(), "unknown", "cmd")
	if out != "" || err != nil {
		t.Errorf("unknown command: got (%q, %v), want (\"\", nil)", out, err)
	}
}
