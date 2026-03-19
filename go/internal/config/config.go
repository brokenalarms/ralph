package config

import (
	"fmt"
	"os"
	"strconv"
)

const Version = "0.1.0"

// Config holds all CLI configuration matching ralph.sh's flag interface.
type Config struct {
	ProjectDir    string
	MaxIterations int
	Prompt        string
	PlanFile      string
	PlanOnly      bool
	SkipPlanning  bool
	Quiet         bool
	UseWorktree   bool
	CallsPerHour  int
	RefactorEvery int
	UseTmux       bool
}

// Defaults returns a Config with ralph.sh default values.
func Defaults() Config {
	return Config{
		ProjectDir:    ".",
		MaxIterations: 20,
		UseWorktree:   true,
		CallsPerHour:  80,
		RefactorEvery: 5,
	}
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

func requireArg(args []string, i int) (string, error) {
	if i+1 >= len(args) {
		return "", fmt.Errorf("option %s requires an argument", args[i])
	}
	return args[i+1], nil
}
