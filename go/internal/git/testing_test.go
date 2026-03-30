package git

import "testing"

// Proves StubGitHub satisfies the GitHub interface at compile time.
func TestStubGitHub_ImplementsGitHub(t *testing.T) {
	var _ GitHub = (*StubGitHub)(nil)
}
