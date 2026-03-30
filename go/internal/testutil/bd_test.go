package testutil

import (
	"context"
	"testing"
)

func TestBDStub_DefaultCounts(t *testing.T) {
	s := NewBDStub()
	run := s.Runner()

	out, err := run(context.Background(), "/tmp", "count")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if out != "5" {
		t.Errorf("total = %q, want %q", out, "5")
	}

	out, _ = run(context.Background(), "/tmp", "count", "--status", "open")
	if out != "3" {
		t.Errorf("open = %q, want %q", out, "3")
	}

	out, _ = run(context.Background(), "/tmp", "count", "--status", "closed")
	if out != "2" {
		t.Errorf("closed = %q, want %q", out, "2")
	}
}

func TestBDStub_ReadyReturnsJSON(t *testing.T) {
	s := NewBDStub()
	run := s.Runner()

	out, err := run(context.Background(), "/tmp", "ready", "--json")
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if out != s.Ready {
		t.Errorf("ready output = %q, want %q", out, s.Ready)
	}
}

func TestBDStub_RecordsCalls(t *testing.T) {
	s := NewBDStub()
	run := s.Runner()

	run(context.Background(), "/tmp", "count")
	run(context.Background(), "/tmp", "ready", "--json")
	run(context.Background(), "/tmp", "close", "abc123", "--reason", "done")

	if !s.CalledWith("count") {
		t.Error("should record count call")
	}
	if !s.CalledWith("ready", "--json") {
		t.Error("should record ready --json call")
	}
	if !s.CalledWith("close", "abc123") {
		t.Error("should record close call")
	}
	if s.CalledWith("init") {
		t.Error("should not match init (not called)")
	}
}

func TestBDStub_SetAndGetState(t *testing.T) {
	s := NewBDStub()
	run := s.Runner()

	run(context.Background(), "/tmp", "set-state", "abc123", "phase=verified")
	out, _ := run(context.Background(), "/tmp", "state", "abc123", "phase")
	if out != "verified" {
		t.Errorf("state = %q, want %q", out, "verified")
	}
}

func TestBDStub_UnhealthyReturnsError(t *testing.T) {
	s := NewBDStub()
	s.Healthy = false
	run := s.Runner()

	_, err := run(context.Background(), "/tmp", "count")
	if err == nil {
		t.Error("unhealthy stub should return error for count")
	}
}

func TestBDStub_InitError(t *testing.T) {
	s := NewBDStub()
	s.InitErr = errTest
	run := s.Runner()

	_, err := run(context.Background(), "/tmp", "init")
	if err != errTest {
		t.Errorf("init err = %v, want errTest", err)
	}
}

func TestBDStub_CustomResponses(t *testing.T) {
	s := NewBDStub()
	s.ShowText = "Bug: login fails on Safari"
	s.Comments = "Confirmed on Safari 17"
	s.Prime = "workflow context here"
	run := s.Runner()

	out, _ := run(context.Background(), "/tmp", "show", "abc123")
	if out != "Bug: login fails on Safari" {
		t.Errorf("show = %q, want %q", out, "Bug: login fails on Safari")
	}

	out, _ = run(context.Background(), "/tmp", "comments", "abc123")
	if out != "Confirmed on Safari 17" {
		t.Errorf("comments = %q, want %q", out, "Confirmed on Safari 17")
	}

	out, _ = run(context.Background(), "/tmp", "prime")
	if out != "workflow context here" {
		t.Errorf("prime = %q, want %q", out, "workflow context here")
	}
}
