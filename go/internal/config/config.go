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
	PlanFile                   string
	PlanOnly                   bool
	SkipPlanning               bool
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
	CIWaitTimeout              time.Duration
	Evolve                     bool
	IdleTimeout                time.Duration
	IdleTimeoutProgress        time.Duration
	WatcherInterval            int
	StuckThreshold             int
	StuckConfirmationThreshold int
	StagnationThreshold        int
	TestSaturationThreshold    int
	PermissionDenialThreshold  int
	BranchStrategy             string
	Wait                       bool
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
		BranchStrategy:             "single",
		WaitInterval:               envDuration("RALPH_WAIT_INTERVAL", 30*time.Second),
		CIWaitTimeout:              envDuration("RALPH_CI_WAIT_TIMEOUT", 10*time.Minute),
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

		case "--refactor-every":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			cfg.RefactorEvery = n
			cfg.cliSet["refactor_every"] = true
			i += 2

		case "--no-refactor":
			cfg.NoRefactor = true
			cfg.cliSet["no_refactor"] = true
			i++

		case "--refactor-threshold":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			cfg.RefactorThreshold = n
			cfg.cliSet["refactor_threshold"] = true
			i += 2

		case "--disable-check":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			for _, name := range strings.Split(v, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					cfg.DisabledChecks = append(cfg.DisabledChecks, name)
				}
			}
			cfg.cliSet["disabled_checks"] = true
			i += 2

		case "--tmux":
			cfg.UseTmux = true
			i++

		case "--wait":
			cfg.Wait = true
			i++

		case "--wait-interval":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			d, err := parseDuration(v)
			if err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			cfg.WaitInterval = d
			cfg.cliSet["wait_interval"] = true
			i += 2

		case "--auto-merge":
			cfg.AutoMerge = true
			i++

		case "--merge-admin":
			cfg.MergeAdmin = true
			i++

		case "--ci-wait-timeout":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			d, err := parseDuration(v)
			if err != nil {
				return cfg, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			cfg.CIWaitTimeout = d
			cfg.cliSet["ci_wait_timeout"] = true
			i += 2

		case "--evolve":
			cfg.Evolve = true
			i++

		case "--branch-strategy":
			v, err := requireArg(args, i)
			if err != nil {
				return cfg, err
			}
			if v != "single" && v != "stacked" {
				return cfg, fmt.Errorf("invalid value for %s: %q (must be \"single\" or \"stacked\")", args[i], v)
			}
			cfg.BranchStrategy = v
			cfg.cliSet["branch_strategy"] = true
			i += 2

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
			cfg.cliSet["idle_timeout"] = true
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
			cfg.cliSet["idle_timeout_progress"] = true
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

		switch key {
		case "branch_strategy":
			if value == "single" || value == "stacked" {
				c.BranchStrategy = value
			}
			continue
		case "no_refactor":
			switch strings.ToLower(value) {
			case "1", "true", "yes":
				c.NoRefactor = true
			}
			continue
		case "disabled_checks":
			for _, name := range strings.Split(value, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					c.DisabledChecks = append(c.DisabledChecks, name)
				}
			}
			continue
		case "merge_admin":
			switch strings.ToLower(value) {
			case "1", "true", "yes":
				c.MergeAdmin = true
			}
			continue
		case "idle_timeout", "idle_timeout_progress", "wait_interval", "ci_wait_timeout":
			d, err := parseDuration(value)
			if err != nil {
				continue
			}
			switch key {
			case "idle_timeout":
				c.IdleTimeout = d
			case "idle_timeout_progress":
				c.IdleTimeoutProgress = d
			case "wait_interval":
				c.WaitInterval = d
			case "ci_wait_timeout":
				c.CIWaitTimeout = d
			}
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
		case "refactor_threshold":
			c.RefactorThreshold = n
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

// InitConfig generates a ralph.toml file at the given path with default values.
// Returns an error if the file already exists.
func InitConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config file already exists: %s", path)
	}
	return writeDefaultConfig(path)
}

// EnsureConfigFile creates a ralph.toml at the given path with defaults if it
// does not already exist. Returns true if a new file was created.
func EnsureConfigFile(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := writeDefaultConfig(path); err != nil {
		return false, err
	}
	return true, nil
}

func writeDefaultConfig(path string) error {
	d := Defaults()
	var b strings.Builder
	fmt.Fprintf(&b, "branch_strategy = %s\n", d.BranchStrategy)
	fmt.Fprintf(&b, "no_refactor = %v\n", d.NoRefactor)
	fmt.Fprintf(&b, "max_iterations = %d\n", d.MaxIterations)
	fmt.Fprintf(&b, "calls_per_hour = %d\n", d.CallsPerHour)
	fmt.Fprintf(&b, "refactor_every = %d\n", d.RefactorEvery)
	fmt.Fprintf(&b, "refactor_threshold = %d\n", d.RefactorThreshold)
	fmt.Fprintf(&b, "idle_timeout = %s\n", formatDuration(d.IdleTimeout))
	fmt.Fprintf(&b, "idle_timeout_progress = %s\n", formatDuration(d.IdleTimeoutProgress))
	fmt.Fprintf(&b, "wait_interval = %s\n", formatDuration(d.WaitInterval))
	fmt.Fprintf(&b, "watcher_interval = %d\n", d.WatcherInterval)
	fmt.Fprintf(&b, "stuck_threshold = %d\n", d.StuckThreshold)
	fmt.Fprintf(&b, "stuck_confirmation_threshold = %d\n", d.StuckConfirmationThreshold)
	fmt.Fprintf(&b, "stagnation_threshold = %d\n", d.StagnationThreshold)
	fmt.Fprintf(&b, "test_saturation_threshold = %d\n", d.TestSaturationThreshold)
	fmt.Fprintf(&b, "permission_denial_threshold = %d\n", d.PermissionDenialThreshold)
	fmt.Fprintf(&b, "merge_admin = %v\n", d.MergeAdmin)
	fmt.Fprintf(&b, "ci_wait_timeout = %s\n", formatDuration(d.CIWaitTimeout))
	fmt.Fprintf(&b, "disabled_checks =\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// formatDuration renders a duration as a human-friendly string for config files.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
