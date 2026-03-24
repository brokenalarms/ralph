package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var spawnCounter atomic.Uint64

// Sandbox configures macOS sandbox-exec container isolation. The profile
// uses deny-default: all operations are denied unless explicitly allowed.
// Reads are granted globally (agents need system libs, tools, git config).
// Writes are scoped to the worktree, ralph state dir, and /tmp only.
type Sandbox struct{}

// DefaultSandbox returns a Sandbox with the standard deny-default profile.
func DefaultSandbox() *Sandbox {
	return &Sandbox{}
}

// Available returns false — sandbox-exec is disabled until the profile
// can be validated against all agent operations without silent failures.
// See ralph-djn for the re-enablement plan.
func Available() bool {
	return false
}

// Profile generates a macOS Seatbelt sandbox profile. Strategy:
//  1. (deny default) — block everything
//  2. (allow file-read* (subpath "/")) — global read for system libs/tools
//  3. (allow file-write* ...) — scoped writes to writeDirs and /tmp only
//  4. (allow file-map-executable) etc. — Node.js requires these globally
//  5. Non-file operations (process, network, mach, ipc) allowed
func (s *Sandbox) Profile(writeDirs []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")

	b.WriteString("(allow file-read* (subpath \"/\"))\n")
	b.WriteString("(allow file-read-metadata)\n")
	b.WriteString("(allow file-map-executable)\n")
	b.WriteString("(allow file-ioctl)\n")
	b.WriteString("(allow file-test-existence)\n")

	for _, d := range writeDirs {
		b.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n", d))
	}
	b.WriteString("(allow file-write* (subpath \"/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")
	b.WriteString("(allow file-write* (literal \"/dev/null\"))\n")
	b.WriteString("(allow file-write* (literal \"/dev/zero\"))\n")
	b.WriteString("(allow file-write* (subpath \"/dev/fd\"))\n")
	b.WriteString("(allow file-write* (regex #\"^/dev/ttys[0-9]+$\"))\n")

	b.WriteString("(allow process*)\n")
	b.WriteString("(allow sysctl*)\n")
	b.WriteString("(allow mach*)\n")
	b.WriteString("(allow network*)\n")
	b.WriteString("(allow system*)\n")
	b.WriteString("(allow signal)\n")
	b.WriteString("(allow iokit*)\n")
	b.WriteString("(allow ipc*)\n")

	return b.String()
}

// Wrap creates an exec.Cmd that runs the given command inside a
// sandbox-exec container. writeDirs specifies directories that get
// write access (typically worktree + ralph state dir).
func (s *Sandbox) Wrap(ctx context.Context, writeDirs []string, name string, args ...string) *exec.Cmd {
	profile := s.Profile(writeDirs)

	profileDir := "/tmp/ralph-sandbox"
	os.MkdirAll(profileDir, 0o755)
	seq := spawnCounter.Add(1)
	profilePath := filepath.Join(profileDir, fmt.Sprintf("agent-%d-%d.sb", os.Getpid(), seq))
	os.WriteFile(profilePath, []byte(profile), 0o600)

	sandboxArgs := []string{"-f", profilePath, name}
	sandboxArgs = append(sandboxArgs, args...)

	if ctx != nil {
		return exec.CommandContext(ctx, "sandbox-exec", sandboxArgs...)
	}
	return exec.Command("sandbox-exec", sandboxArgs...)
}
