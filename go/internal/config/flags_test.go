package config

import (
	"os"
	"strings"
	"testing"
)

// Verifies that help text generated from the flag registry includes every
// CLI flag's long name, proving flags and help text share a single source.
func TestFlagUsageContainsAllFlags(t *testing.T) {
	usage := FlagUsage()
	for _, f := range Flags {
		if f.Long == "" {
			continue
		}
		if !strings.Contains(usage, f.Long) {
			t.Errorf("FlagUsage() missing flag %s", f.Long)
		}
	}
}

// Verifies that help text includes the default value from each FlagDef,
// catching stale hardcoded defaults (e.g. --wait-interval showing "30s"
// when the registry says "5s").
func TestFlagUsageContainsDefaults(t *testing.T) {
	usage := FlagUsage()
	for _, f := range Flags {
		if f.Default == "" || f.Long == "" {
			continue
		}
		expected := "default: " + f.Default
		if !strings.Contains(usage, expected) {
			t.Errorf("FlagUsage() for %s missing %q", f.Long, expected)
		}
	}
}

// Verifies that help text includes env var names for flags that define them,
// preventing env var documentation from drifting out of sync.
func TestFlagUsageContainsEnvVars(t *testing.T) {
	usage := FlagUsage()
	for _, f := range Flags {
		if f.EnvVar == "" {
			continue
		}
		if !strings.Contains(usage, f.EnvVar) {
			t.Errorf("FlagUsage() missing env var %s for flag %s", f.EnvVar, f.Long)
		}
	}
}

// Verifies that every CLI flag in the registry is recognized by Parse(),
// ensuring the registry and parser are in sync.
func TestAllRegisteredFlagsAreParseable(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")
	t.Setenv("RALPH_BASE_BRANCH", "")
	t.Setenv("RALPH_WAIT_INTERVAL", "")

	for _, f := range Flags {
		if f.Long == "" && f.Short == "" {
			continue
		}
		if f.Long == "--help" {
			continue
		}

		name := f.Long
		if name == "" {
			name = f.Short
		}

		var args []string
		args = append(args, name)
		if f.Kind != KindBool {
			switch f.Kind {
			case KindInt:
				args = append(args, "42")
			case KindString:
				args = append(args, "test")
			case KindDuration:
				args = append(args, "5m")
			case KindStringList:
				args = append(args, "a,b")
			}
		}

		_, err := Parse(args)
		if err != nil && strings.Contains(err.Error(), "unknown") {
			t.Errorf("Flag %s defined in registry but not recognized by Parse()", name)
		}
	}
}

// Verifies that every flag with a ConfigKey appears in the config map used
// by LoadConfigFile, ensuring config file support stays in sync.
func TestAllConfigKeysInConfigMap(t *testing.T) {
	for _, f := range Flags {
		if f.ConfigKey == "" {
			continue
		}
		if configMap[f.ConfigKey] == nil {
			t.Errorf("ConfigKey %q defined in Flags but missing from configMap", f.ConfigKey)
		}
	}
}

// Verifies that Defaults() produces values matching every FlagDef.Default,
// proving runtime defaults derive from the Flags registry (single source of truth)
// rather than being independently hardcoded.
func TestDefaultsDeriveFromFlagRegistry(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")
	t.Setenv("RALPH_BASE_BRANCH", "")
	t.Setenv("RALPH_WAIT_INTERVAL", "")

	cfg := Defaults()

	// Build a second config by applying FlagDef defaults directly.
	check := Config{ProjectDir: "."}
	for i := range Flags {
		f := &Flags[i]
		if f.Default != "" && f.Default != "cwd" {
			if err := f.Apply(&check, f.Default); err != nil {
				t.Fatalf("applying default %q for %s: %v", f.Default, f.Long, err)
			}
		}
	}

	if cfg.MaxIterations != check.MaxIterations {
		t.Errorf("MaxIterations: Defaults()=%d, registry=%d", cfg.MaxIterations, check.MaxIterations)
	}
	if cfg.CallsPerHour != check.CallsPerHour {
		t.Errorf("CallsPerHour: Defaults()=%d, registry=%d", cfg.CallsPerHour, check.CallsPerHour)
	}
	if cfg.IdleTimeout != check.IdleTimeout {
		t.Errorf("IdleTimeout: Defaults()=%v, registry=%v", cfg.IdleTimeout, check.IdleTimeout)
	}
	if cfg.IdleTimeoutProgress != check.IdleTimeoutProgress {
		t.Errorf("IdleTimeoutProgress: Defaults()=%v, registry=%v", cfg.IdleTimeoutProgress, check.IdleTimeoutProgress)
	}
	if cfg.BaseBranch != check.BaseBranch {
		t.Errorf("BaseBranch: Defaults()=%q, registry=%q", cfg.BaseBranch, check.BaseBranch)
	}
	if cfg.WaitInterval != check.WaitInterval {
		t.Errorf("WaitInterval: Defaults()=%v, registry=%v", cfg.WaitInterval, check.WaitInterval)
	}
	if cfg.WatcherInterval != check.WatcherInterval {
		t.Errorf("WatcherInterval: Defaults()=%d, registry=%d", cfg.WatcherInterval, check.WatcherInterval)
	}
	if cfg.StuckThreshold != check.StuckThreshold {
		t.Errorf("StuckThreshold: Defaults()=%d, registry=%d", cfg.StuckThreshold, check.StuckThreshold)
	}
}

// Verifies that InitConfig generates entries for all flags with ConfigKeys.
func TestInitConfigUsesRegistry(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ralph.toml"

	if err := InitConfig(path); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	data, _ := readFile(path)
	content := string(data)

	for _, f := range Flags {
		if f.ConfigKey == "" {
			continue
		}
		if !strings.Contains(content, f.ConfigKey) {
			t.Errorf("InitConfig output missing key %q", f.ConfigKey)
		}
	}
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// Verifies that the --dir flag has been removed from the flag registry:
// it should not appear in help text and should not be parseable.
func TestDirFlagRemoved(t *testing.T) {
	usage := FlagUsage()
	if strings.Contains(usage, "--dir") {
		t.Error("--dir should not appear in FlagUsage()")
	}

	_, err := Parse([]string{"--dir", "/tmp"})
	if err == nil {
		t.Error("Parse should reject --dir flag")
	}

	_, err = Parse([]string{"-d", "/tmp"})
	if err == nil {
		t.Error("Parse should reject -d flag")
	}
}
