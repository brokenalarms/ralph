package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Sandbox configures macOS sandbox-exec container isolation. Each sandboxed
// agent process gets filesystem write access restricted to specified
// directories (typically the worktree and ralph state dir). Read access
// to system paths is allowed for tool execution (git, go, npm, etc.).
type Sandbox struct {
	// ReadPaths lists directories with read-only access beyond the write dirs.
	ReadPaths []string
}

// DefaultSandbox returns a Sandbox configured for typical macOS development.
// Write access is granted per-invocation via the writeDirs parameter.
// Read access includes standard system and tool paths.
func DefaultSandbox() *Sandbox {
	home, _ := os.UserHomeDir()
	readPaths := []string{
		"/usr",
		"/bin",
		"/sbin",
		"/Library",
		"/System",
		"/private/etc",
		"/opt",
		"/Applications",
	}
	if home != "" {
		readPaths = append(readPaths, filepath.Join(home, ".claude"))
		readPaths = append(readPaths, filepath.Join(home, ".gitconfig"))
		readPaths = append(readPaths, filepath.Join(home, ".config"))
		readPaths = append(readPaths, filepath.Join(home, "go"))
		readPaths = append(readPaths, filepath.Join(home, ".npm"))
		readPaths = append(readPaths, filepath.Join(home, ".cargo"))
	}
	return &Sandbox{ReadPaths: readPaths}
}

// Available checks whether sandbox-exec is present on this system.
func Available() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

// Profile generates a macOS Seatbelt sandbox profile that restricts
// filesystem write access to writeDirs and /tmp.
func (s *Sandbox) Profile(writeDirs []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")

	for _, d := range writeDirs {
		b.WriteString(fmt.Sprintf("(allow file-read* file-write* (subpath %q))\n", d))
	}

	b.WriteString("(allow file-read* file-write* (subpath \"/tmp\"))\n")
	b.WriteString("(allow file-read* file-write* (subpath \"/private/tmp\"))\n")

	for _, p := range s.ReadPaths {
		b.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", p))
	}

	// Allow file metadata operations needed by tools
	b.WriteString("(allow file-read-metadata)\n")

	b.WriteString("(allow process*)\n")
	b.WriteString("(allow sysctl-read)\n")
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
