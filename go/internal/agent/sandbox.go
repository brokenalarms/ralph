package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Sandbox configures macOS sandbox-exec container isolation. The profile
// uses an allow-then-deny strategy: all file operations are allowed globally,
// then writes are denied under user-controlled paths, then specific write
// holes are punched for the worktree and ralph state dir. This avoids the
// fragile deny-default approach where Node.js crashes because of missing
// operations like file-map-executable or file-ioctl.
type Sandbox struct {
	// DenyWritePaths lists directories where writes are blocked.
	// Typically just "/Users" to prevent writes outside the worktree.
	DenyWritePaths []string
}

// DefaultSandbox returns a Sandbox that denies writes under /Users.
// Per-invocation write access (worktree, ralph state dir) is punched
// through via the writeDirs parameter in Profile/Wrap.
func DefaultSandbox() *Sandbox {
	return &Sandbox{
		DenyWritePaths: []string{"/Users"},
	}
}

// Available checks whether sandbox-exec is present on this system.
func Available() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

// Profile generates a macOS Seatbelt sandbox profile. Strategy:
//  1. Allow all non-file operations (process, network, mach, ipc, etc.)
//  2. Allow all file operations globally (covers file-map-executable, etc.)
//  3. Deny writes under DenyWritePaths (e.g. /Users)
//  4. Punch write holes for writeDirs, /tmp, and /private/tmp
func (s *Sandbox) Profile(writeDirs []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")

	b.WriteString("(allow file*)\n")

	for _, d := range s.DenyWritePaths {
		b.WriteString(fmt.Sprintf("(deny file-write* (subpath %q))\n", d))
	}

	for _, d := range writeDirs {
		b.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n", d))
	}
	b.WriteString("(allow file-write* (subpath \"/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")

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
// read-write access (typically worktree + ralph state dir).
func (s *Sandbox) Wrap(ctx context.Context, writeDirs []string, name string, args ...string) *exec.Cmd {
	profile := s.Profile(writeDirs)

	profileDir := filepath.Join(os.TempDir(), "ralph-sandbox")
	os.MkdirAll(profileDir, 0o755)
	profilePath := filepath.Join(profileDir, fmt.Sprintf("agent-%d.sb", os.Getpid()))
	os.WriteFile(profilePath, []byte(profile), 0o600)

	sandboxArgs := []string{"-f", profilePath, name}
	sandboxArgs = append(sandboxArgs, args...)

	if ctx != nil {
		return exec.CommandContext(ctx, "sandbox-exec", sandboxArgs...)
	}
	return exec.Command("sandbox-exec", sandboxArgs...)
}
