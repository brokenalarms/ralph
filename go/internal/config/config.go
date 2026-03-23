package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

var Version = "0.1.0-dev"

// Config holds all CLI configuration matching ralph.sh's flag interface.
type Config struct {
	ProjectDir                 string
	MaxIterations              int
	Prompt                     string
	Quiet                      bool
	UseWorktree                bool
	CallsPerHour               int
	RefactorEvery              int
	NoRefactor                 bool
	RefactorThreshold          int
	DisabledChecks             []string
	UseTmux                    bool
	AutoMerge                  bool
	MergeAdmin                 bool
	Evolve                     bool
	IdleTimeout                time.Duration
	IdleTimeoutProgress        time.Duration
	WatcherInterval            int
	StuckThreshold             int
	StuckConfirmationThreshold int
	StagnationThreshold        int
	TestSaturationThreshold    int
	PermissionDenialThreshold  int
	BaseBranch string
	Wait       bool
	WaitInterval               time.Duration

	cliSet map[string]bool
}

// Defaults returns a Config with ralph.sh default values.
// MaxIterations and RefactorEvery read from RALPH_MAX_ITERATIONS and
// RALPH_REFACTOR_EVERY env vars, falling back to shell defaults (50 and 0).
func Defaults() Config {
	return Config{
		ProjectDir:                 ".",
		MaxIterations:              envInt("RALPH_MAX_ITERATIONS", 50),
		UseWorktree:                true,
		CallsPerHour:               80,
		RefactorEvery:              envInt("RALPH_REFACTOR_EVERY", 0),
		NoRefactor:                 envBool("RALPH_NO_REFACTOR", false),
		RefactorThreshold:          envInt("RALPH_REFACTOR_THRESHOLD", 20),
		IdleTimeout:                envDuration("RALPH_IDLE_TIMEOUT", 10*time.Minute),
		IdleTimeoutProgress:        envDuration("RALPH_IDLE_TIMEOUT_PROGRESS", 5*time.Minute),
		WatcherInterval:            10,
		StuckThreshold:             5,
		StuckConfirmationThreshold: 2,
		StagnationThreshold:        3,
		TestSaturationThreshold:    3,
		PermissionDenialThreshold:  3,
		BaseBranch:   envString("RALPH_BASE_BRANCH", "develop"),
		WaitInterval: envDuration("RALPH_WAIT_INTERVAL", 5*time.Second),
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

// envString reads a string from an environment variable, returning fallback
// if unset or empty.
func envString(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

// envBool reads a boolean from an environment variable, returning fallback
// if unset. Accepts "1", "true", "yes" as true; anything else is false.
func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
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
	case "commander":
		return parseSubcommandWithDir(args, "commander"), true
	case "task":
		return parseSubcommandWithDir(args, "task"), true
	case "filter-stream":
		return parseFilterStream(args), true
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

// parseFilterStream handles `ralph filter-stream <rawlog>` where the argument
// is a file path, not a directory.
func parseFilterStream(args []string) Subcommand {
	sub := Subcommand{Name: "filter-stream"}
	if len(args) > 1 {
		sub.Args = args[1:]
	}
	return sub
}

// Parse processes CLI arguments into a Config using the Flags registry.
// Returns an error for unknown flags or missing values.
func Parse(args []string) (Config, error) {
	cfg := Defaults()
	cfg.cliSet = make(map[string]bool)
	i := 0

	for i < len(args) {
		f, ok := flagMap[args[i]]
		if !ok {
			if len(args[i]) > 0 && args[i][0] == '-' {
				return cfg, fmt.Errorf("unknown option: %s", args[i])
			}
			return cfg, fmt.Errorf("unknown argument: %s (use --dir to specify a project directory)", args[i])
		}

		if f.Kind == KindBool {
			if err := f.Apply(&cfg, ""); err != nil {
				return cfg, err
			}
			if f.TrackCLI && f.ConfigKey != "" {
				cfg.cliSet[f.ConfigKey] = true
			}
			i++
		} else {
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			if err := f.Apply(&cfg, v); err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			if f.TrackCLI && f.ConfigKey != "" {
				cfg.cliSet[f.ConfigKey] = true
			}
			i += 2
		}
	}

	return cfg, nil
}

// Validate checks for invalid flag combinations.
func (c *Config) Validate() error {
	if c.Evolve {
		if !c.AutoMerge {
			return fmt.Errorf("--evolve requires --auto-merge")
		}
		if c.UseTmux {
			return fmt.Errorf("--evolve is incompatible with --tmux")
		}
	}
	if c.MergeAdmin && !c.AutoMerge {
		return fmt.Errorf("--merge-admin requires --auto-merge")
	}
	return nil
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
// applies values to the Config using the Flags registry. CLI-set values
// (tracked via cliSet) take precedence and are not overwritten.
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

		fd := configMap[key]
		if fd == nil {
			continue
		}
		_ = fd.Apply(c, value)
	}
	return scanner.Err()
}

// InitConfig generates a ralph.toml file at the given path with default values
// derived from the Flags registry. Returns an error if the file already exists.
func InitConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config file already exists: %s", path)
	}

	var b strings.Builder
	for _, f := range Flags {
		if f.ConfigKey == "" {
			continue
		}
		switch f.Kind {
		case KindBool:
			fmt.Fprintf(&b, "%s = false\n", f.ConfigKey)
		default:
			fmt.Fprintf(&b, "%s = %s\n", f.ConfigKey, f.Default)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
