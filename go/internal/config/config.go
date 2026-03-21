package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const Version = "0.1.0"

// Config holds all CLI configuration matching ralph.sh's flag interface.
type Config struct {
	ProjectDir          string
	MaxIterations       int
	Prompt              string
	PlanFile            string
	PlanOnly            bool
	SkipPlanning        bool
	Quiet               bool
	UseWorktree         bool
	CallsPerHour        int
	RefactorEvery       int
	UseTmux             bool
	AutoMerge           bool
	IdleTimeout         time.Duration
	IdleTimeoutProgress time.Duration
}

// Defaults returns a Config with ralph.sh default values.
// MaxIterations and RefactorEvery read from RALPH_MAX_ITERATIONS and
// RALPH_REFACTOR_EVERY env vars, falling back to shell defaults (50 and 0).
func Defaults() Config {
	return Config{
		ProjectDir:          ".",
		MaxIterations:       envInt("RALPH_MAX_ITERATIONS", 50),
		UseWorktree:         true,
		CallsPerHour:        80,
		RefactorEvery:       envInt("RALPH_REFACTOR_EVERY", 0),
		IdleTimeout:         envDuration("RALPH_IDLE_TIMEOUT", 10*time.Minute),
		IdleTimeoutProgress: envDuration("RALPH_IDLE_TIMEOUT_PROGRESS", 30*time.Second),
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
			// Positional argument = project directory
			cfg.ProjectDir = args[i]
			i++
		}
	}

	return cfg, nil
}

// ErrHelp is returned when -h/--help is passed.
var ErrHelp = fmt.Errorf("help requested")

// ValidatePlanFile checks that a --plan-file argument points to an
// existing file containing markdown checkboxes. Returns nil if valid,
// or a descriptive error suitable for display.
func ValidatePlanFile(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("plan file not found: %s", path)
	} else if err != nil {
		return fmt.Errorf("checking plan file: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading plan file: %w", err)
	}

	content := string(data)
	if !strings.Contains(content, "- [ ]") && !strings.Contains(content, "- [x]") {
		return fmt.Errorf("plan file is not in Ralph format (needs markdown checkboxes: - [ ] task)")
	}

	return nil
}

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
