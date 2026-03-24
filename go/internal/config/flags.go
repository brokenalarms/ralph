package config

import (
	"fmt"
	"strconv"
	"strings"
)

type FlagKind int

const (
	KindBool FlagKind = iota
	KindInt
	KindString
	KindDuration
	KindStringList
)

type FlagDef struct {
	Short     string
	Long      string
	MetaVar   string
	Help      string
	Default   string
	EnvVar    string
	ConfigKey string
	Kind      FlagKind
	TrackCLI  bool
	Apply     func(cfg *Config, val string) error
}

func parseBoolVal(val string) bool {
	if val == "" {
		return true
	}
	switch strings.ToLower(val) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// Flags is the single source of truth for all CLI flags and config-file keys.
// Adding a flag means adding one entry here — parsing, help text, config file
// support, and defaults display all derive from this slice.
var Flags = []FlagDef{
	{
		Short: "-d", Long: "--dir", MetaVar: "<path>",
		Help: "Project directory", Default: "cwd",
		Kind: KindString,
		Apply: func(cfg *Config, val string) error {
			cfg.ProjectDir = val
			return nil
		},
	},
	{
		Short: "-n", Long: "--max", MetaVar: "<N>",
		Help: "Max iterations", Default: "50",
		EnvVar: "RALPH_MAX_ITERATIONS", ConfigKey: "max_iterations",
		Kind: KindInt, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.MaxIterations = n
			return nil
		},
	},
	{
		Short: "-p", Long: "--prompt", MetaVar: "<text>",
		Help: "Prompt override (otherwise Claude reads repo context)",
		Kind: KindString,
		Apply: func(cfg *Config, val string) error {
			cfg.Prompt = val
			return nil
		},
	},
	{
		Short: "-q", Long: "--quiet",
		Help: "Suppress Claude output streaming (log only)",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.Quiet = true
			return nil
		},
	},
	{
		Long: "--calls-per-hour", MetaVar: "<N>",
		Help: "Max Claude calls per hour", Default: "80",
		ConfigKey: "calls_per_hour",
		Kind: KindInt, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.CallsPerHour = n
			return nil
		},
	},
	{
		Long: "--refactor-every", MetaVar: "<N>",
		Help: "Refactor every N iterations", Default: "0",
		EnvVar: "RALPH_REFACTOR_EVERY", ConfigKey: "refactor_every",
		Kind: KindInt, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.RefactorEvery = n
			return nil
		},
	},
	{
		Long: "--no-refactor",
		Help: "Disable refactoring entirely",
		EnvVar: "RALPH_NO_REFACTOR", ConfigKey: "no_refactor",
		Kind: KindBool, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			if parseBoolVal(val) {
				cfg.NoRefactor = true
			}
			return nil
		},
	},
	{
		Long: "--refactor-threshold", MetaVar: "<N>",
		Help: "Quality score threshold for refactoring", Default: "20",
		EnvVar: "RALPH_REFACTOR_THRESHOLD", ConfigKey: "refactor_threshold",
		Kind: KindInt, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.RefactorThreshold = n
			return nil
		},
	},
	{
		Long: "--disable-check", MetaVar: "<checks>",
		Help: "Disable specific quality checks (comma-separated)",
		ConfigKey: "disabled_checks",
		Kind: KindStringList, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			for _, name := range strings.Split(val, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					cfg.DisabledChecks = append(cfg.DisabledChecks, name)
				}
			}
			return nil
		},
	},
	{
		Long: "--idle-timeout", MetaVar: "<dur>",
		Help: "Kill session after this idle duration", Default: "10m",
		EnvVar: "RALPH_IDLE_TIMEOUT",
		Kind: KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.IdleTimeout = d
			return nil
		},
	},
	{
		Long: "--idle-timeout-progress", MetaVar: "<dur>",
		Help: "Shorter idle timeout when progress detected", Default: "5m",
		EnvVar: "RALPH_IDLE_TIMEOUT_PROGRESS",
		Kind: KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.IdleTimeoutProgress = d
			return nil
		},
	},
	{
		Long: "--tmux",
		Help: "Run in tmux 3-pane layout (status / output / plan)",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.UseTmux = true
			return nil
		},
	},
	{
		Long: "--base-branch", MetaVar: "<name>",
		Help: "Base branch for rebase/merge", Default: "develop",
		EnvVar: "RALPH_BASE_BRANCH", ConfigKey: "base_branch",
		Kind: KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			if val != "" {
				cfg.BaseBranch = val
			}
			return nil
		},
	},
	{
		Long: "--auto-merge",
		Help: "Squash-merge PRs into base branch after task completion",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.AutoMerge = true
			return nil
		},
	},
	{
		Long: "--merge-admin",
		Help: "Use --admin flag on gh pr merge to bypass branch protection (requires --auto-merge)",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.MergeAdmin = true
			return nil
		},
	},
	{
		Long: "--evolve",
		Help: "Self-improving mode: after each merged task, pull main, rebuild, restart (requires --auto-merge)",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.Evolve = true
			return nil
		},
	},
	{
		Long: "--wait",
		Help: "Keep running after all tasks complete, polling for new tasks",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.Wait = true
			return nil
		},
	},
	{
		Long: "--wait-interval", MetaVar: "<dur>",
		Help: "Polling interval for --wait", Default: "5s",
		EnvVar: "RALPH_WAIT_INTERVAL", ConfigKey: "wait_interval",
		Kind: KindDuration, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.WaitInterval = d
			return nil
		},
	},
	{
		Short: "-h", Long: "--help",
		Help: "Show this help",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			return ErrHelp
		},
	},

	// Config-file-only settings (no CLI flag).
	{
		ConfigKey: "watcher_interval", Default: "10",
		Kind: KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.WatcherInterval = n
			return nil
		},
	},
	{
		ConfigKey: "stuck_threshold", Default: "5",
		Kind: KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.StuckThreshold = n
			return nil
		},
	},
	{
		ConfigKey: "stuck_confirmation_threshold", Default: "2",
		Kind: KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.StuckConfirmationThreshold = n
			return nil
		},
	},
	{
		ConfigKey: "stagnation_threshold", Default: "3",
		Kind: KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.StagnationThreshold = n
			return nil
		},
	},
	{
		ConfigKey: "test_saturation_threshold", Default: "3",
		Kind: KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.TestSaturationThreshold = n
			return nil
		},
	},
	{
		ConfigKey: "permission_denial_threshold", Default: "3",
		Kind: KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.PermissionDenialThreshold = n
			return nil
		},
	},
	{
		Long: "--verify-level", MetaVar: "<level>",
		Help:      "Verification level for no-diff completions: fire (trust agent) or hog (spawn verifier)",
		Default:   "fire",
		ConfigKey: "verify_level",
		Kind:      KindString,
		Apply: func(cfg *Config, val string) error {
			switch val {
			case "fire", "hog":
				cfg.VerifyLevel = val
			default:
				return fmt.Errorf("verify-level must be fire or hog, got %q", val)
			}
			return nil
		},
	},
}

var (
	flagMap   map[string]*FlagDef
	configMap map[string]*FlagDef
)

func init() {
	flagMap = make(map[string]*FlagDef, len(Flags)*2)
	configMap = make(map[string]*FlagDef)
	for i := range Flags {
		f := &Flags[i]
		if f.Short != "" {
			flagMap[f.Short] = f
		}
		if f.Long != "" {
			flagMap[f.Long] = f
		}
		if f.ConfigKey != "" {
			configMap[f.ConfigKey] = f
		}
	}
}

const flagHelpColumn = 25

// FlagUsage returns formatted help text for all CLI flags, auto-generated
// from the Flags registry.
func FlagUsage() string {
	var b strings.Builder
	for _, f := range Flags {
		if f.Long == "" && f.Short == "" {
			continue
		}

		var left string
		if f.Short != "" {
			left = fmt.Sprintf("  %s, %s", f.Short, f.Long)
		} else {
			left = fmt.Sprintf("  %s", f.Long)
		}
		if f.MetaVar != "" {
			left += " " + f.MetaVar
		}

		right := f.Help
		var suffixParts []string
		if f.Default != "" {
			suffixParts = append(suffixParts, f.Default)
		}
		if f.EnvVar != "" {
			suffixParts = append(suffixParts, "env "+f.EnvVar)
		}
		if len(suffixParts) > 0 {
			right += " (default: " + strings.Join(suffixParts, ", ") + ")"
		}

		padding := flagHelpColumn - len(left)
		if padding < 2 {
			padding = 2
		}
		fmt.Fprintf(&b, "%s%s%s\n", left, strings.Repeat(" ", padding), right)
	}
	return b.String()
}
