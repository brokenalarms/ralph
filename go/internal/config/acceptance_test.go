package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadAcceptanceConfig(t *testing.T, contents string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg := Defaults()
	if err := cfg.LoadConfigFile(path); err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	return cfg
}

// A project opts into the ship-time acceptance gate with an [acceptance] table:
// the command it names and the countdown it configures both reach the loop.
func TestLoadConfigFile_AcceptanceTable(t *testing.T) {
	cfg := loadAcceptanceConfig(t, `
max_iterations = 20

[acceptance]
command = "npm run test:safari"
countdown_seconds = 30
`)

	if cfg.AcceptanceCommand != "npm run test:safari" {
		t.Errorf("AcceptanceCommand = %q, want the [acceptance] command", cfg.AcceptanceCommand)
	}
	if cfg.AcceptanceCountdown != 30*time.Second {
		t.Errorf("AcceptanceCountdown = %v, want 30s", cfg.AcceptanceCountdown)
	}
	if cfg.MaxIterations != 20 {
		t.Errorf("top-level keys before the table must still apply: MaxIterations = %d, want 20", cfg.MaxIterations)
	}
}

// Omitting countdown_seconds keeps the 10-second default, so a project only has
// to name its command to get a working gate.
func TestLoadConfigFile_AcceptanceCountdownDefaultsToTenSeconds(t *testing.T) {
	cfg := loadAcceptanceConfig(t, "[acceptance]\ncommand = \"make accept\"\n")

	if cfg.AcceptanceCountdown != 10*time.Second {
		t.Errorf("AcceptanceCountdown = %v, want the 10s default", cfg.AcceptanceCountdown)
	}
}

// A project with no [acceptance] table is entirely unaffected: no command means
// the gate is disabled and loop behavior is unchanged.
func TestLoadConfigFile_NoAcceptanceTable_GateDisabled(t *testing.T) {
	cfg := loadAcceptanceConfig(t, "max_iterations = 5\nnotify = true\n")

	if cfg.AcceptanceCommand != "" {
		t.Errorf("AcceptanceCommand = %q, want empty (gate disabled)", cfg.AcceptanceCommand)
	}
}

// An [acceptance] table with an explicitly empty command disables the gate too,
// so a project can keep the table around while turning the gate off.
func TestLoadConfigFile_EmptyAcceptanceCommand_GateDisabled(t *testing.T) {
	cfg := loadAcceptanceConfig(t, "[acceptance]\ncommand = \"\"\ncountdown_seconds = 20\n")

	if cfg.AcceptanceCommand != "" {
		t.Errorf("AcceptanceCommand = %q, want empty (gate disabled)", cfg.AcceptanceCommand)
	}
}

// Keys inside a table are scoped to it: a bare "command" outside any table is
// not an acceptance command, and must not silently enable the gate.
func TestLoadConfigFile_TopLevelCommandKeyDoesNotEnableGate(t *testing.T) {
	cfg := loadAcceptanceConfig(t, "command = \"rm -rf /\"\n")

	if cfg.AcceptanceCommand != "" {
		t.Errorf("AcceptanceCommand = %q, want empty — an unscoped 'command' key is not acceptance.command", cfg.AcceptanceCommand)
	}
}

// The generated config.toml shows the acceptance gate as an [acceptance] table
// with the command commented out, so a project can discover and enable it, and
// re-reading the generated file round-trips the countdown default.
func TestInitConfig_EmitsAcceptanceTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := InitConfig(path); err != nil {
		t.Fatalf("InitConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "[acceptance]") {
		t.Errorf("generated config missing [acceptance] table, got:\n%s", content)
	}
	if !strings.Contains(content, "# command = ") {
		t.Errorf("generated config should offer a commented-out acceptance command, got:\n%s", content)
	}
	if !strings.Contains(content, "countdown_seconds = 10") {
		t.Errorf("generated config should carry the countdown default, got:\n%s", content)
	}

	cfg := Defaults()
	if err := cfg.LoadConfigFile(path); err != nil {
		t.Fatalf("LoadConfigFile on generated file: %v", err)
	}
	if cfg.AcceptanceCommand != "" {
		t.Errorf("generated config must leave the gate disabled, got %q", cfg.AcceptanceCommand)
	}
	if cfg.AcceptanceCountdown != 10*time.Second {
		t.Errorf("generated config must round-trip the countdown, got %v", cfg.AcceptanceCountdown)
	}
}
