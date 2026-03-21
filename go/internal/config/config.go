package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const Version = "0.1.0"

// Config holds all CLI configuration matching ralph.sh's flag interface.
type Config struct {
	ProjectDir                 string
	MaxIterations              int
	Prompt                     string
	PlanFile                   string
	PlanOnly                   bool
	SkipPlanning               bool
	Quiet                      bool
	UseWorktree                bool
	CallsPerHour               int
	UseTmux                    bool
	AutoMerge                  bool
	IdleTimeout                time.Duration
	IdleTimeoutProgress        time.Duration
	WatcherInterval            int
	StuckThreshold             int
	StuckConfirmationThreshold int
	StagnationThreshold        int
	TestSaturationThreshold    int
	PermissionDenialThreshold  int

	cliSet map[string]bool
}

// Defaults returns a Config with ralph.sh default values.
// MaxIterations reads from RALPH_MAX_ITERATIONS env var, falling back to 50.
func Defaults() Config {
	return Config{
		ProjectDir:                 ".",
		MaxIterations:              envInt("RALPH_MAX_ITERATIONS", 50),
		UseWorktree:                true,
		CallsPerHour:               80,
		IdleTimeout:                envDuration("RALPH_IDLE_TIMEOUT", 10*time.Minute),
		IdleTimeoutProgress:        envDuration("RALPH_IDLE_TIMEOUT_PROGRESS", 30*time.Second),
		WatcherInterval:            10,
		StuckThreshold:             5,
		StuckConfirmationThreshold: 2,
		StagnationThreshold:        3,
		TestSaturationThreshold:    3,
		PermissionDenialThreshold:  3,
	}
}

// envInt reads an integer from an environment variable, returning fallback
// if unset or unparseable.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// envDuration reads a duration from an environment variable, returning
// fallback if unset or unparseable. Accepts Go duration strings (e.g. "5m",
// "30s") or bare integers interpreted as seconds.
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		if n, err2 := strconv.Atoi(v); err2 == nil {
			return time.Duration(n) * time.Second
		}
		return fallback
	}
	return d
}

// Subcommand represents a ralph subcommand (stop, feedback) parsed before flags.
type Subcommand struct {
	Name string   // "stop", "feedback", or "" for main loop
	Dir  string   // target directory
	Args []string // remaining arguments (e.g. feedback message parts)
}

// ParseSubcommand checks if the first argument is a subcommand.
// Returns the subcommand info and whether one was found.
func ParseSubcommand(args []string) (Subcommand, bool) {
	if len(args) == 0 {
		return Subcommand{}, false
	}

	switch args[0] {
	case "stop":
		return parseSubcommandWithDir(args, "stop"), true
	case "feedback":
		return parseSubcommandWithDir(args, "feedback"), true
	default:
		return Subcommand{}, false
	}
}

func parseSubcommandWithDir(args []string, name string) Subcommand {
	sub := Subcommand{Name: name, Dir: "."}
	rest := args[1:]

	// If the next arg exists, doesn't start with '-', and is a directory, use it.
	if len(rest) > 0 && len(rest[0]) > 0 && rest[0][0] != '-' {
		if info, err := os.Stat(rest[0]); err == nil && info.IsDir() {
			sub.Dir = rest[0]
			rest = rest[1:]
		}
	}

	sub.Args = rest
	return sub
}

// Parse processes CLI arguments into a Config. Returns an error for unknown
// flags or missing values.
func Parse(args []string) (Config, error) {
	cfg := Defaults()
	cfg.cliSet = make(map[string]bool)
	i := 0

	for i < len(args) {
		switch args[i] {
		case "-d", "--dir":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			cfg.ProjectDir = v
			i += 2

		case "-n", "--max":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			cfg.MaxIterations = n
			cfg.cliSet["max_iterations"] = true
			i += 2

		case "-p", "--prompt":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			cfg.Prompt = v
			i += 2

		case "--plan-file":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			cfg.PlanFile = v
			i += 2

		case "--plan":
			cfg.PlanOnly = true
			i++

		case "--skip-planning":
			cfg.SkipPlanning = true
			i++

		case "-q", "--quiet":
			cfg.Quiet = true
			i++

		case "--no-worktree":
			cfg.UseWorktree = false
			i++

		case "--calls-per-hour":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			cfg.CallsPerHour = n
			cfg.cliSet["calls_per_hour"] = true
			i += 2

		case "--tmux":
			cfg.UseTmux = true
			i++

		case "--auto-merge":
			cfg.AutoMerge = true
			i++

		case "--idle-timeout":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			d, err := parseDuration(v)
			if err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			cfg.IdleTimeout = d
			i += 2

		case "--idle-timeout-progress":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			d, err := parseDuration(v)
			if err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			cfg.IdleTimeoutProgress = d
			i += 2

		case "-h", "--help":
			return cfg, ErrHelp

		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				return cfg, fmt.Errorf("unknown option: %s", args[i])
			}
			cfg.ProjectDir = args[i]
			i++
		}
	}

	return cfg, nil
}

// CLISet reports whether a config key was explicitly set via CLI flags.
func (c *Config) CLISet(key string) bool {
	if c.cliSet == nil {
		return false
	}
	return c.cliSet[key]
}

// ErrHelp is returned when -h/--help is passed.
var ErrHelp = fmt.Errorf("help requested")

func requireArg(args []string, i int) (string, error) {
	if i+1 >= len(args) {
		return "", fmt.Errorf("option %s requires an argument", args[i])
	}
	return args[i+1], nil
}

// parseDuration parses a duration string. Accepts Go duration format (e.g.
// "10m", "30s") or bare integers interpreted as seconds.
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		n, err2 := strconv.Atoi(s)
		if err2 != nil {
			return 0, fmt.Errorf("must be a duration (e.g. 10m, 30s) or seconds: %s", s)
		}
		return time.Duration(n) * time.Second, nil
	}
	return d, nil
}

// LoadConfigFile reads a TOML-like config file (key = value per line) and
// applies values to the Config. CLI-set values (tracked via cliSet) take
// precedence and are not overwritten.
func (c *Config) LoadConfigFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			continue
		}

		key := strings.TrimSpace(line[:eqIdx])
		value := strings.TrimSpace(line[eqIdx+1:])

		if commentIdx := strings.Index(value, "#"); commentIdx >= 0 {
			value = strings.TrimSpace(value[:commentIdx])
		}

		value = strings.Trim(value, `"'`)

		if c.cliSet != nil && c.cliSet[key] {
			continue
		}

		n, err := strconv.Atoi(value)
		if err != nil {
			continue
		}

		switch key {
		case "max_iterations":
			c.MaxIterations = n
		case "calls_per_hour":
			c.CallsPerHour = n
		case "refactor_every":
			c.RefactorEvery = n
		case "watcher_interval":
			c.WatcherInterval = n
		case "stuck_threshold":
			c.StuckThreshold = n
		case "stuck_confirmation_threshold":
			c.StuckConfirmationThreshold = n
		case "stagnation_threshold":
			c.StagnationThreshold = n
		case "test_saturation_threshold":
			c.TestSaturationThreshold = n
		case "permission_denial_threshold":
			c.PermissionDenialThreshold = n
		}
	}
	return scanner.Err()
}

// configKeys lists all supported ralph.toml keys in display order.
var configKeys = []struct {
	Key     string
	Default int
}{
	{"max_iterations", 50},
	{"calls_per_hour", 80},
	{"refactor_every", 0},
	{"watcher_interval", 10},
	{"stuck_threshold", 5},
	{"stuck_confirmation_threshold", 2},
	{"stagnation_threshold", 3},
	{"test_saturation_threshold", 3},
	{"permission_denial_threshold", 3},
}

// InitConfig generates a ralph.toml file at the given path with default values.
// Returns an error if the file already exists.
func InitConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config file already exists: %s", path)
	}

	var b strings.Builder
	for _, k := range configKeys {
		fmt.Fprintf(&b, "%s = %d\n", k.Key, k.Default)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
