package testutil

import (
	"testing"
	"time"
)

func TestProcessStub_StartKillWait(t *testing.T) {
	p := NewProcessStub(42)

	if p.Started() {
		t.Fatal("should not be started before Start()")
	}

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !p.Started() {
		t.Fatal("should be started after Start()")
	}
	if p.Pid() != 42 {
		t.Errorf("Pid = %d, want 42", p.Pid())
	}

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !p.Killed() {
		t.Fatal("should be killed after Kill()")
	}

	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !p.Waited() {
		t.Fatal("should be waited after Wait()")
	}

	got := p.Calls()
	want := []string{"start", "kill", "wait"}
	if len(got) != len(want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Calls[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProcessStub_WaitBlocksUntilRelease(t *testing.T) {
	p := NewProcessStub(1)

	done := make(chan struct{})
	go func() {
		p.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Wait should block until Release")
	case <-time.After(50 * time.Millisecond):
	}

	p.Release()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait should unblock after Release")
	}
}

func TestProcessStub_WaitBlocksUntilKill(t *testing.T) {
	p := NewProcessStub(2)

	done := make(chan struct{})
	go func() {
		p.Wait()
		close(done)
	}()

	p.Kill()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait should unblock after Kill")
	}
}

func TestProcessStub_ErrorInjection(t *testing.T) {
	p := NewProcessStub(3)
	p.StartErr = errTest
	p.KillErr = errTest
	p.WaitErr = errTest

	if err := p.Start(); err != errTest {
		t.Errorf("Start err = %v, want errTest", err)
	}
	if err := p.Kill(); err != errTest {
		t.Errorf("Kill err = %v, want errTest", err)
	}
	if err := p.Wait(); err != errTest {
		t.Errorf("Wait err = %v, want errTest", err)
	}
}
