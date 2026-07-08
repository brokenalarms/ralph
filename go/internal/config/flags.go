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
	Short         string
	Long          string
	MetaVar       string
	Help          string
	Default       string
	EnvVar        string
	ConfigKey     string
	Kind          FlagKind
	CommentInInit bool // include as a commented-out line in generated config.toml even with no default
	Apply         func(cfg *Config, val string) error
	Read          func(cfg *Config) string // returns current value; "" means default/unset for bools
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return ""
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

// adminMergeOnCIInfraFailureFlag is shared between the loop's Flags registry
// (ralph loop --auto-merge) and MergeFlags (ralph merge): the same admin-bypass
// semantics apply whether the infra-failure classification happens during the
// loop's own AutoMerge or a standalone `ralph merge` invocation.
var adminMergeOnCIInfraFailureFlag = FlagDef{
	Long:      "--admin-merge-on-ci-infra-failure",
	Help:      "Admin-bypass branch protection when a CI failure is classified as infrastructure (zero job steps). Has no effect on real test failures.",
	ConfigKey: "admin_merge_on_ci_infra_failure",
	Kind:      KindBool,
	Apply: func(cfg *Config, _ string) error {
		cfg.AdminMergeOnCIInfraFailure = true
		return nil
	},
	Read: func(cfg *Config) string { return boolStr(cfg.AdminMergeOnCIInfraFailure) },
}

// noCIWaitFlag is merge-only: it skips AwaitCI polling in MergeStack, a
// concept the loop's own AutoMerge path has no equivalent for.
var noCIWaitFlag = FlagDef{
	Long: "--no-ci-wait",
	Help: "Skip AwaitCI and rely on infrastructure-failure classification. " +
		"Use when GitHub Actions is known to be down and required checks will never run.",
	Kind: KindBool,
	Apply: func(cfg *Config, _ string) error {
		cfg.SkipCIWait = true
		return nil
	},
	Read: func(cfg *Config) string { return boolStr(cfg.SkipCIWait) },
}

// MergeFlags defines the flags accepted by the `ralph merge` subcommand. It
// is a separate per-subcommand registry built on the same FlagDef mechanism
// as Flags, so merge's help text (FlagUsageFor) and parsing (ParseFlags)
// derive from the same code path as every other subcommand, without adding
// merge-only flags to the loop's own CLI surface.
var MergeFlags = []FlagDef{noCIWaitFlag, adminMergeOnCIInfraFailureFlag}

// Flags is the single source of truth for all CLI flags and config-file keys.
// Adding a flag means adding one entry here — parsing, help text, config file
// support, and defaults display all derive from this slice.
var Flags = []FlagDef{
	{
		Short: "-n", Long: "--max", MetaVar: "<N>",
		Help: "Max iterations", Default: "50",
		EnvVar: "RALPH_MAX_ITERATIONS", ConfigKey: "max_iterations",
		Kind: KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.MaxIterations = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.MaxIterations) },
	},
	{
		Help: "Max Claude calls per hour", Default: "80",
		ConfigKey: "calls_per_hour",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.CallsPerHour = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.CallsPerHour) },
	},
	{
		Help: "Kill session after this idle duration", Default: "10m",
		EnvVar: "RALPH_IDLE_TIMEOUT", ConfigKey: "idle_timeout",
		Kind: KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.IdleTimeout = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.IdleTimeout.String() },
	},
	{
		Help: "Shorter idle timeout when progress detected", Default: "5m",
		EnvVar: "RALPH_IDLE_TIMEOUT_PROGRESS", ConfigKey: "idle_timeout_progress",
		Kind: KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.IdleTimeoutProgress = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.IdleTimeoutProgress.String() },
	},
	{
		Help: "Hard wall-clock cap on agent run time (main and fix agents); kills session if exceeded", Default: "30m",
		EnvVar: "RALPH_MAX_RUN_DURATION", ConfigKey: "max_run_duration",
		Kind: KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.MaxRunDuration = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.MaxRunDuration.String() },
	},
	{
		Long: "--tmux",
		Help: "Run in tmux 3-pane layout (status / output / plan)",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.UseTmux = true
			return nil
		},
		Read: func(cfg *Config) string { return boolStr(cfg.UseTmux) },
	},
	{
		Long: "--base-branch", MetaVar: "<name>",
		Help:   "Remote branch (origin/<name>) used for rebase and PR targets. The loop never touches your local checkout.",
		EnvVar: "RALPH_BASE_BRANCH", ConfigKey: "base_branch",
		Kind: KindString,
		Apply: func(cfg *Config, val string) error {
			if val != "" {
				cfg.BaseBranch = val
			}
			return nil
		},
		Read: func(cfg *Config) string { return cfg.BaseBranch },
	},
	{
		Long: "--auto-merge",
		Help: "Squash-merge PRs into base branch after task completion",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.AutoMerge = true
			return nil
		},
		Read: func(cfg *Config) string { return boolStr(cfg.AutoMerge) },
	},
	adminMergeOnCIInfraFailureFlag,
	{
		Long: "--evolve",
		Help: "Self-improving mode: after each merged task, pull main, rebuild, restart (requires --auto-merge)",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.Evolve = true
			return nil
		},
		Read: func(cfg *Config) string { return boolStr(cfg.Evolve) },
	},
	{
		Long: "--wait",
		Help: "Keep running after all tasks complete, polling for new tasks",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.Wait = true
			return nil
		},
		Read: func(cfg *Config) string { return boolStr(cfg.Wait) },
	},
	{
		Short: "-v", Long: "--verbose",
		Help: "Show all tool calls in stream log (by default low-value tools are hidden)",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.Verbose = true
			return nil
		},
		Read: func(cfg *Config) string { return boolStr(cfg.Verbose) },
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
		ConfigKey: "ci_poll_timeout", Default: "5m",
		Kind: KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.CIPollTimeout = d
			return nil
		},
	},
	{
		ConfigKey: "ci_max_wait", Default: "45m",
		Kind: KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.CIMaxWait = d
			return nil
		},
	},
	{
		ConfigKey: "no_ci_grace_period", Default: "30s",
		Kind: KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.NoCIGracePeriod = d
			return nil
		},
	},
	{
		Long: "--post-task", MetaVar: "<script>",
		Help:      "Run external script after each task completes",
		ConfigKey: "post_task",
		Kind:      KindString,
		Apply: func(cfg *Config, val string) error {
			cfg.PostTask = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.PostTask },
	},
	{
		ConfigKey:     "verify",
		Kind:          KindString,
		CommentInInit: true,
		Apply: func(cfg *Config, val string) error {
			cfg.Verify = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.Verify },
	},
	{
		Long:      "--notify",
		Help:      "Send macOS notification on each task completion",
		ConfigKey: "notify",
		Kind:      KindBool,
		Apply: func(cfg *Config, val string) error {
			cfg.Notify = val == "true"
			return nil
		},
		Read: func(cfg *Config) string { return fmt.Sprintf("%t", cfg.Notify) },
	},
	// Review timeouts
	{
		Help: "Timeout when waiting for a gated Copilot code review", Default: "120s",
		ConfigKey: "copilot_review_timeout",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.ReviewerGatedTimeout = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.ReviewerGatedTimeout.String() },
	},
	{
		Help: "Timeout when waiting for an opportunistic (non-gated) Copilot code review", Default: "90s",
		ConfigKey: "copilot_opportunistic_timeout",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.ReviewerOpportunisticTimeout = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.ReviewerOpportunisticTimeout.String() },
	},
	{
		Help: "Timeout when waiting for a CodeRabbit code review", Default: "60s",
		ConfigKey: "coderabbit_review_timeout",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.CodeRabbitReviewTimeout = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.CodeRabbitReviewTimeout.String() },
	},
	// Stagnation thresholds
	{
		Help: "Max zero-progress idle/wall-clock attempts before parking a task", Default: "2",
		ConfigKey: "max_failed_starts",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.MaxFailedStarts = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.MaxFailedStarts) },
	},
	{
		Help: "Max compaction events before parking a task", Default: "1",
		ConfigKey: "max_compaction_parks",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.MaxCompactionParks = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.MaxCompactionParks) },
	},
	{
		Help: "Max consecutive distinct-task skips before halting the loop", Default: "2",
		ConfigKey: "cascade_skip_limit",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.CascadeSkipLimit = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.CascadeSkipLimit) },
	},
	// Attempt limits
	{
		Help: "Max recent attempts shown in agent prompt context", Default: "3",
		ConfigKey: "max_prompt_attempts",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.MaxPromptAttempts = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.MaxPromptAttempts) },
	},
	{
		Help: "Max consecutive idle timeout failures before skipping a task", Default: "3",
		ConfigKey: "max_idle_timeout_failures",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.MaxIdleTimeoutFailures = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.MaxIdleTimeoutFailures) },
	},
	{
		Help: "Max LLM verification attempts before skipping a task", Default: "3",
		ConfigKey: "max_llm_verify_attempts",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.MaxLLMVerifyAttempts = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.MaxLLMVerifyAttempts) },
	},
	{
		Help: "Max fix agent spawns for failing tests or compile errors", Default: "3",
		ConfigKey: "max_test_fix_attempts",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.MaxTestFixAttempts = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.MaxTestFixAttempts) },
	},
	// Test/compile timeouts
	{
		Help: "Maximum duration for the ralph:verify test suite", Default: "5m",
		ConfigKey: "test_timeout",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.TestTimeout = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.TestTimeout.String() },
	},
	{
		Help: "Maximum duration for pre-push compile check", Default: "60s",
		ConfigKey: "compile_check_timeout",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.CompileCheckTimeout = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.CompileCheckTimeout.String() },
	},
	// Network timeouts
	{
		Help: "Timeout for internet connectivity probe", Default: "3s",
		ConfigKey: "connectivity_check_timeout",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.ConnectivityCheckTimeout = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.ConnectivityCheckTimeout.String() },
	},
	{
		Help: "How often to recheck internet connectivity while waiting for restoration", Default: "30s",
		ConfigKey: "internet_restore_interval",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.InternetRestoreInterval = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.InternetRestoreInterval.String() },
	},
	{
		Help:      "Model for the main working agent",
		Default:   ModelSonnet,
		ConfigKey: "working_model",
		Kind:      KindString,
		Apply: func(cfg *Config, val string) error {
			cfg.WorkingModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.WorkingModel },
	},
	{
		Help:      "Model for LLM verification (first attempt)",
		Default:   ModelHaiku,
		ConfigKey: "verify_model",
		Kind:      KindString,
		Apply: func(cfg *Config, val string) error {
			cfg.VerifyModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.VerifyModel },
	},
	{
		Help:      "Model for LLM verification escalation (subsequent attempts)",
		Default:   ModelSonnet,
		ConfigKey: "verify_escalation_model",
		Kind:      KindString,
		Apply: func(cfg *Config, val string) error {
			cfg.VerifyEscalationModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.VerifyEscalationModel },
	},
	{
		Help:      "Model for fix agents (repair agent, from first attempt)",
		Default:   ModelOpus,
		ConfigKey: "fix_model",
		Kind:      KindString,
		Apply: func(cfg *Config, val string) error {
			cfg.FixModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.FixModel },
	},
	{
		Help:      "Emit an agent-liveness heartbeat at this interval during quiet agent runs (0 disables)",
		Default:   "60s",
		EnvVar:    "RALPH_AGENT_HEARTBEAT_INTERVAL",
		ConfigKey: "agent_heartbeat_interval",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.AgentHeartbeatInterval = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.AgentHeartbeatInterval.String() },
	},
	{
		Help:      "Days to retain session logs in the stable log directory (0 = keep forever)",
		Default:   "30",
		ConfigKey: "log_retention_days",
		Kind:      KindInt,
		Apply: func(cfg *Config, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			cfg.LogRetentionDays = n
			return nil
		},
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.LogRetentionDays) },
	},
}

var configMap map[string]*FlagDef

func init() {
	configMap = make(map[string]*FlagDef)
	for i := range Flags {
		f := &Flags[i]
		if f.ConfigKey != "" {
			configMap[f.ConfigKey] = f
		}
	}
}

const flagHelpColumn = 25

// FlagUsage returns formatted help text for all CLI flags, auto-generated
// from the Flags registry.
func FlagUsage() string {
	return FlagUsageFor(Flags)
}

// FlagUsageFor returns formatted help text for the given flag definitions,
// using the same layout as FlagUsage. Subcommands with their own scoped
// registry (e.g. MergeFlags) call this directly instead of hand-rolling help
// text, so every subcommand's help derives from the same formatting path.
func FlagUsageFor(defs []FlagDef) string {
	var b strings.Builder
	for _, f := range defs {
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

// ParseFlags parses args against defs, applying recognized flags onto cfg.
// It returns any positional (non-flag) arguments in encounter order, and an
// error for unrecognized flags or missing values. Unlike Parse, it does not
// require defs to be the full Flags registry and does not reject positional
// arguments itself — callers with subcommand-specific positional arguments
// (e.g. ralph merge's PR number) pull them from the returned slice.
func ParseFlags(defs []FlagDef, cfg *Config, args []string) ([]string, error) {
	m := make(map[string]*FlagDef, len(defs)*2)
	for i := range defs {
		f := &defs[i]
		if f.Short != "" {
			m[f.Short] = f
		}
		if f.Long != "" {
			m[f.Long] = f
		}
	}

	var positional []string
	i := 0
	for i < len(args) {
		f, ok := m[args[i]]
		if !ok {
			if len(args[i]) > 0 && args[i][0] == '-' {
				return nil, fmt.Errorf("unknown flag: %s", args[i])
			}
			positional = append(positional, args[i])
			i++
			continue
		}

		if f.Kind == KindBool {
			if err := f.Apply(cfg, ""); err != nil {
				return nil, err
			}
			if f.ConfigKey != "" && cfg.cliSet != nil {
				cfg.cliSet[f.ConfigKey] = true
			}
			i++
		} else {
			v, err := requireArg(args, i)
			if err != nil {
				return nil, err
			}
			if err := f.Apply(cfg, v); err != nil {
				return nil, fmt.Errorf("invalid value for %s: %q", args[i], v)
			}
			if f.ConfigKey != "" && cfg.cliSet != nil {
				cfg.cliSet[f.ConfigKey] = true
			}
			i += 2
		}
	}
	return positional, nil
}

// ConfigToState returns a map of config key → value for all CLI flags that
// have both a Long name and a Read function. Bools are stored as "true" when
// set, omitted when false. Values matching the registry default are omitted
// to keep state.json minimal.
func ConfigToState(cfg *Config) map[string]string {
	defaults := Defaults()
	m := make(map[string]string)
	for i := range Flags {
		f := &Flags[i]
		if f.Long == "" || f.Read == nil {
			continue
		}
		key := strings.TrimPrefix(f.Long, "--")
		val := f.Read(cfg)

		if f.Kind == KindBool && val == "" {
			continue
		}

		if val == f.Read(&defaults) {
			continue
		}

		m[key] = val
	}
	return m
}

// ArgsFromState reconstructs CLI args from a state map produced by
// ConfigToState. Only keys that match a current flag definition are included;
// unknown keys (from old binary versions) are silently ignored.
func ArgsFromState(state map[string]string) []string {
	var args []string
	for i := range Flags {
		f := &Flags[i]
		if f.Long == "" {
			continue
		}
		key := strings.TrimPrefix(f.Long, "--")
		val, ok := state[key]
		if !ok || val == "" {
			continue
		}
		// --help is never reconstructed from state.
		if f.Long == "--help" {
			continue
		}
		if f.Kind == KindBool {
			args = append(args, f.Long)
		} else {
			args = append(args, f.Long, val)
		}
	}
	return args
}
