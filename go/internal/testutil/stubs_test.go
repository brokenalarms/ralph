package testutil

import (
	"testing"

	"github.com/brokenalarms/ralph/internal/tasks"
)

// Proves StubBackend, MutableBackend, and TrackingBackend satisfy tasks.Backend.
func TestStubBackend_ImplementsBackend(t *testing.T) {
	var _ tasks.Backend = (*StubBackend)(nil)
	var _ tasks.Backend = (*MutableBackend)(nil)
	var _ tasks.Backend = (*TrackingBackend)(nil)
}
