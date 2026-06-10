package config

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var Version = "0.1.0-dev"

// Model aliases passed to `claude --model`. The CLI resolves each alias to
// the latest released model in that family, so Ralph automatically picks up
// new model versions without code changes. Use full version strings (e.g.
// "claude-opus-4-6") only when reproducibility against a specific release
// is required, via --model-ceiling, fix_escalation_model, etc.
const (
	ModelHaiku  = "haiku"
	ModelSonnet = "sonnet"
	ModelOpus   = "opus"
)

// Bead assignee identities — the single source of truth for bead ownership.
// The autonomous loop works ONLY beads assigned to LoopAssignee (its inbox) and
// claims them explicitly as such. The task manager creates beads owned by
// TaskAssignee (hidden from the loop) and releases them to the loop with
// `bd update <id> --assignee=ralph-loop` once settled. The task-manager prompt
// must use these same literal strings.
const (
	LoopAssignee = "ralph-loop"
	TaskAssignee = "ralph-task"
)

// Config holds all CLI configuration matching ralph.sh's flag interface.
type Config struct {
	ProjectDir                 string
	MaxIterations              int
	Prompt                     string
	CallsPerHour               int
	UseTmux                    bool
	AutoMerge                  bool
	Evolve                     bool
	IdleTimeout                time.Duration
	IdleTimeoutProgress        time.Duration
	MaxRunDuration             time.Duration
	WatcherInterval            int
	StuckThreshold             int
	StuckConfirmationThreshold int
	StagnationThreshold        int
	TestSaturationThreshold    int
	PermissionDenialThreshold  int
	BaseBranch                 string
	Wait                       bool
	Verbose                    bool
	Model                      string
	AgentEscalationModel       string // deprecated: no effect; cross-iteration escalation removed
	VerifyModel                string
	VerifyEscalationModel      string
	FixModel                   string
	FixEscalationModel         string
	PostTask                   string
	VerifyBuild                string
	Verify                     string
	Notify                     bool
	CIPollTimeout              time.Duration
	NoCIGracePeriod            time.Duration

	// Review timeouts — how long to wait for automated code reviewers.
	// Field names are neutral: the config package doesn't know about
	// specific reviewer implementations (Copilot, CodeRabbit). Those are
	// internal details of the git module.
	ReviewerGatedTimeout         time.Duration
	ReviewerOpportunisticTimeout time.Duration
	CodeRabbitReviewTimeout      time.Duration

	// Attempt limits — maximum retries before giving up or skipping.
	MaxPromptAttempts      int
	MaxIdleTimeoutFailures int
	MaxLLMVerifyAttempts   int
	MaxTestFixAttempts     int

	// Test/compile timeouts.
	TestTimeout         time.Duration
	CompileCheckTimeout time.Duration

	// Network timeouts for connectivity checks.
	ConnectivityCheckTimeout time.Duration
	InternetRestoreInterval  time.Duration

	// AgentHeartbeatInterval emits an agent-liveness heartbeat line when the
	// agent produces no visible output for this duration. Zero disables.
	AgentHeartbeatInterval time.Duration

	// AdminMergeOnCIInfraFailure enables admin-bypass of branch protection when
	// CI failure is classified as infrastructure-only (zero job steps). Has no
	// effect on real test failures.
	AdminMergeOnCIInfraFailure bool

	// LogRetentionDays is the number of days to keep loop logs in the stable
	// log directory. Zero disables pruning.
	LogRetentionDays int

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
		return Subcommand{Name: "feedback", Dir: ".", Args: args[1:]}, true
	case "attach":
		return Subcommand{Name: "attach", Dir: ".", Args: args[1:]}, true
	case "task":
		return Subcommand{Name: "task", Dir: ".", Args: args[1:]}, true
	case "loop":
		return Subcommand{Name: "loop", Dir: ".", Args: args[1:]}, true
	case "review":
		return Subcommand{Name: "review", Dir: ".", Args: args[1:]}, true
	case "merge":
		return Subcommand{Name: "merge", Dir: ".", Args: args[1:]}, true
	case "filter-stream":
		return parseFilterStream(args), true
	default:
		return Subcommand{}, false
	}
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
			if f.ConfigKey != "" {
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
			if f.ConfigKey != "" {
				cfg.cliSet[f.ConfigKey] = true
			}
			i += 2
		}
	}

	return cfg, nil
}

// Validate checks for invalid flag combinations and required configuration.
// Call this after LoadConfigFile so that base_branch set in config.toml is visible.
func (c *Config) Validate() error {
	if c.BaseBranch == "" {
		return fmt.Errorf("base branch not set: provide --base-branch, RALPH_BASE_BRANCH env, or base_branch in config.toml")
	}
	if c.Evolve {
		if !c.AutoMerge {
			return fmt.Errorf("--evolve requires --auto-merge")
		}
		if c.UseTmux {
			return fmt.Errorf("--evolve is incompatible with --tmux")
		}
	}
	if c.AdminMergeOnCIInfraFailure && !c.AutoMerge {
		return fmt.Errorf("--admin-merge-on-ci-infra-failure requires --auto-merge")
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

// InitConfig generates a config.toml file at the given path with default values
// derived from the Flags registry. Returns an error if the file already exists.
func InitConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config file already exists: %s", path)
	}

	type entry struct {
		key string
		val string
	}
	var regular, commented []entry
	for _, f := range Flags {
		if f.ConfigKey == "" {
			continue
		}
		if f.Kind == KindBool {
			regular = append(regular, entry{f.ConfigKey, "false"})
		} else if f.Default != "" {
			regular = append(regular, entry{f.ConfigKey, f.Default})
		} else if f.CommentInInit {
			commented = append(commented, entry{f.ConfigKey, ""})
		}
	}
	sort.Slice(regular, func(i, j int) bool { return regular[i].key < regular[j].key })
	sort.Slice(commented, func(i, j int) bool { return commented[i].key < commented[j].key })

	var b strings.Builder
	for _, e := range regular {
		fmt.Fprintf(&b, "%s = %s\n", e.key, e.val)
	}
	for _, e := range commented {
		fmt.Fprintf(&b, "# %s = \n", e.key)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
