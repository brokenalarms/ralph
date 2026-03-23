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
	t.Setenv("RALPH_REFACTOR_EVERY", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")
	t.Setenv("RALPH_BASE_BRANCH", "")
	t.Setenv("RALPH_NO_REFACTOR", "")
	t.Setenv("RALPH_REFACTOR_THRESHOLD", "")
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
