package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var Version = "0.1.0-dev"

// SourceDir is the root of Ralph's source repository, set at build time via
// ldflags. Used by evolve to locate build-go.sh regardless of which project
// ralph loop is running against.
var SourceDir string

// ResolveSourceDir returns Ralph's source directory. It prefers the build-time
// SourceDir, then falls back to resolving the real path of the running binary
// and walking up to find the repo root (identified by scripts/build-go.sh).
func ResolveSourceDir() string {
	if SourceDir != "" {
		return SourceDir
	}

	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}

	// Walk up from the binary's real location looking for the repo root.
	dir := filepath.Dir(real)
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "build-go.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// Config holds all CLI configuration matching ralph.sh's flag interface.
type Config struct {
	ProjectDir                 string
	MaxIterations              int
	Prompt                     string
	Quiet                      bool
	CallsPerHour               int
	Refactor                   bool
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
	BaseBranch              string
	Wait                    bool
	Verbose                 bool
	VerifyLevel             string
	VerifyModel             string
	VerifyEscalationModel   string


	cliSet map[string]bool
}

// Defaults returns a Config with default values derived from the Flags
// registry (single source of truth). Environment variables listed in each
// FlagDef.EnvVar override the registry defaults.
func Defaults() Config {
	cfg := Config{
		ProjectDir: ".",
	}
	for i := range Flags {
		f := &Flags[i]
		if f.Default != "" {
			_ = f.Apply(&cfg, f.Default)
		}
	}
	for i := range Flags {
		f := &Flags[i]
		if f.EnvVar != "" {
			if v := os.Getenv(f.EnvVar); v != "" {
				_ = f.Apply(&cfg, v)
			}
		}
	}
	return cfg
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
		return Subcommand{Name: "stop", Dir: ".", Args: args[1:]}, true
	case "feedback":
		return parseSubcommandWithDir(args, "feedback"), true
	case "command", "commander":
		return parseSubcommandWithDir(args, "command"), true
	case "task":
		return parseSubcommandWithDir(args, "task"), true
	case "loop":
		return parseSubcommandWithDir(args, "loop"), true
	case "review":
		return parseSubcommandWithDir(args, "review"), true
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
			return cfg, fmt.Errorf("unknown argument: %s", args[i])
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
