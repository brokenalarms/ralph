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
	Read      func(cfg *Config) string // returns current value; "" means default/unset for bools
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

// Flags is the single source of truth for all CLI flags and config-file keys.
// Adding a flag means adding one entry here — parsing, help text, config file
// support, and defaults display all derive from this slice.
var Flags = []FlagDef{
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
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.MaxIterations) },
	},
	{
		Short: "-p", Long: "--prompt", MetaVar: "<text>",
		Help: "Prompt override (otherwise Claude reads repo context)",
		Kind: KindString,
		Apply: func(cfg *Config, val string) error {
			cfg.Prompt = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.Prompt },
	},
	{
		Short: "-q", Long: "--quiet",
		Help: "Suppress Claude output streaming (log only)",
		Kind: KindBool,
		Apply: func(cfg *Config, _ string) error {
			cfg.Quiet = true
			return nil
		},
		Read: func(cfg *Config) string { return boolStr(cfg.Quiet) },
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
		Read: func(cfg *Config) string { return strconv.Itoa(cfg.CallsPerHour) },
	},
	{
		Long: "--refactor",
		Help: "Enable LLM-based adaptive refactoring (checks every 5 task completions)",
		ConfigKey: "refactor",
		Kind: KindBool, TrackCLI: true,
		Apply: func(cfg *Config, _ string) error {
			cfg.Refactor = true
			return nil
		},
		Read: func(cfg *Config) string { return boolStr(cfg.Refactor) },
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
		Read: func(cfg *Config) string { return cfg.IdleTimeout.String() },
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
		Read: func(cfg *Config) string { return cfg.IdleTimeoutProgress.String() },
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
		Help: "Base branch for rebase/merge", Default: "develop",
		EnvVar: "RALPH_BASE_BRANCH", ConfigKey: "base_branch",
		Kind: KindString, TrackCLI: true,
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
	{
		Long:      "--admin-merge-on-ci-infra-failure",
		Help:      "Admin-bypass branch protection when AutoMerge detects a CI infrastructure failure (zero job steps). Has no effect on real test failures.",
		ConfigKey: "admin_merge_on_ci_infra_failure",
		Kind:      KindBool, TrackCLI: true,
		Apply: func(cfg *Config, _ string) error {
			cfg.AdminMergeOnCIInfraFailure = true
			return nil
		},
		Read: func(cfg *Config) string { return boolStr(cfg.AdminMergeOnCIInfraFailure) },
	},
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
		Long: "--post-task", MetaVar: "<script>",
		Help:      "Run external script after each task completes",
		ConfigKey: "post_task",
		Kind:      KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			cfg.PostTask = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.PostTask },
	},
	{
		Long: "--verify-build", MetaVar: "<script>",
		Help:      "Run external script before pre-iteration tests to check project-level build health",
		ConfigKey: "verify_build",
		Kind:      KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			cfg.VerifyBuild = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.VerifyBuild },
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
	{
		Long: "--post-signal-timeout", MetaVar: "<dur>",
		Help:      "Timeout for post-signal operations (verification, push, merge)", Default: "15m",
		EnvVar:    "RALPH_POST_SIGNAL_TIMEOUT",
		ConfigKey: "post_signal_timeout",
		Kind:      KindDuration,
		Apply: func(cfg *Config, val string) error {
			d, err := parseDuration(val)
			if err != nil {
				return err
			}
			cfg.PostSignalTimeout = d
			return nil
		},
		Read: func(cfg *Config) string { return cfg.PostSignalTimeout.String() },
	},
	{
		Long: "--model", MetaVar: "<model>",
		Help:      "Model ceiling for all LLM interactions (loop agent, fix agents, verification, task, review). When set, no LLM call may use a higher-tier model.",
		Default:   ModelSonnet,
		ConfigKey: "model",
		Kind:      KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			cfg.Model = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.Model },
	},
	// Review timeouts
	{
		Long: "--copilot-review-timeout", MetaVar: "<dur>",
		Help: "Timeout when waiting for a gated Copilot code review", Default: "120s",
		ConfigKey: "copilot_review_timeout",
		Kind:      KindDuration, TrackCLI: true,
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
		Long: "--copilot-opportunistic-timeout", MetaVar: "<dur>",
		Help: "Timeout when waiting for an opportunistic (non-gated) Copilot code review", Default: "90s",
		ConfigKey: "copilot_opportunistic_timeout",
		Kind:      KindDuration, TrackCLI: true,
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
		Long: "--coderabbit-review-timeout", MetaVar: "<dur>",
		Help: "Timeout when waiting for a CodeRabbit code review", Default: "60s",
		ConfigKey: "coderabbit_review_timeout",
		Kind:      KindDuration, TrackCLI: true,
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
	// Attempt limits
	{
		Long: "--max-prompt-attempts", MetaVar: "<N>",
		Help: "Max recent attempts shown in agent prompt context", Default: "3",
		ConfigKey: "max_prompt_attempts",
		Kind:      KindInt, TrackCLI: true,
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
		Long: "--max-idle-timeout-failures", MetaVar: "<N>",
		Help: "Max consecutive idle timeout failures before skipping a task", Default: "3",
		ConfigKey: "max_idle_timeout_failures",
		Kind:      KindInt, TrackCLI: true,
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
		Long: "--max-llm-verify-attempts", MetaVar: "<N>",
		Help: "Max LLM verification attempts before skipping a task", Default: "3",
		ConfigKey: "max_llm_verify_attempts",
		Kind:      KindInt, TrackCLI: true,
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
		Long: "--max-test-fix-attempts", MetaVar: "<N>",
		Help: "Max fix agent spawns for failing tests or compile errors", Default: "3",
		ConfigKey: "max_test_fix_attempts",
		Kind:      KindInt, TrackCLI: true,
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
		Long: "--test-timeout", MetaVar: "<dur>",
		Help: "Maximum duration for the ralph:verify test suite", Default: "5m",
		ConfigKey: "test_timeout",
		Kind:      KindDuration, TrackCLI: true,
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
		Long: "--compile-check-timeout", MetaVar: "<dur>",
		Help: "Maximum duration for pre-push compile check", Default: "60s",
		ConfigKey: "compile_check_timeout",
		Kind:      KindDuration, TrackCLI: true,
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
		Long: "--connectivity-check-timeout", MetaVar: "<dur>",
		Help: "Timeout for internet connectivity probe", Default: "3s",
		ConfigKey: "connectivity_check_timeout",
		Kind:      KindDuration, TrackCLI: true,
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
		Long: "--internet-restore-interval", MetaVar: "<dur>",
		Help: "How often to recheck internet connectivity while waiting for restoration", Default: "30s",
		ConfigKey: "internet_restore_interval",
		Kind:      KindDuration, TrackCLI: true,
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
		Long: "--agent-escalation-model", MetaVar: "<model>",
		Help:      "Model for agent on retry attempts (subsequent attempts after first)",
		Default:   ModelOpus,
		ConfigKey: "agent_escalation_model",
		Kind:      KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			cfg.AgentEscalationModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.AgentEscalationModel },
	},
	{
		Long: "--verify-model", MetaVar: "<model>",
		Help:      "Model for LLM verification (first attempt)",
		Default:   ModelHaiku,
		ConfigKey: "verify_model",
		Kind:      KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			cfg.VerifyModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.VerifyModel },
	},
	{
		Long: "--verify-escalation-model", MetaVar: "<model>",
		Help:      "Model for LLM verification escalation (subsequent attempts)",
		Default:   ModelSonnet,
		ConfigKey: "verify_escalation_model",
		Kind:      KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			cfg.VerifyEscalationModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.VerifyEscalationModel },
	},
	{
		Long: "--fix-model", MetaVar: "<model>",
		Help:      "Model for fix agents on first attempt",
		Default:   ModelSonnet,
		ConfigKey: "fix_model",
		Kind:      KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			cfg.FixModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.FixModel },
	},
	{
		Long: "--fix-escalation-model", MetaVar: "<model>",
		Help:      "Model for fix agents on retry attempts (subsequent attempts after first)",
		Default:   ModelOpus,
		ConfigKey: "fix_escalation_model",
		Kind:      KindString, TrackCLI: true,
		Apply: func(cfg *Config, val string) error {
			cfg.FixEscalationModel = val
			return nil
		},
		Read: func(cfg *Config) string { return cfg.FixEscalationModel },
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
