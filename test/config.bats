#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: config file values override hardcoded defaults.
@test "load_config sets variables from ralph.toml" {
  cat > "$PROJECT_DIR/ralph.toml" <<'EOF'
max_iterations = 25
calls_per_hour = 40
stuck_threshold = 10
stagnation_threshold = 5
EOF
  _CLI_MAX_ITERATIONS=""
  _CLI_CALLS_PER_HOUR=""

  load_config "$PROJECT_DIR/ralph.toml"
  [[ "$MAX_ITERATIONS" -eq 25 ]]
  [[ "$CALLS_PER_HOUR" -eq 40 ]]
  [[ "$STUCK_THRESHOLD" -eq 10 ]]
  [[ "$STAGNATION_THRESHOLD" -eq 5 ]]
}

# Proves: CLI args take precedence over config file values.
@test "CLI args override config file values" {
  cat > "$PROJECT_DIR/ralph.toml" <<'EOF'
max_iterations = 25
calls_per_hour = 40
EOF
  MAX_ITERATIONS=99
  _CLI_MAX_ITERATIONS="99"
  _CLI_CALLS_PER_HOUR=""

  load_config "$PROJECT_DIR/ralph.toml"
  [[ "$MAX_ITERATIONS" -eq 99 ]]
  [[ "$CALLS_PER_HOUR" -eq 40 ]]
}

# Proves: missing config file is silently ignored.
@test "load_config is a no-op when file does not exist" {
  MAX_ITERATIONS=50
  STUCK_THRESHOLD=5
  _CLI_MAX_ITERATIONS=""
  _CLI_CALLS_PER_HOUR=""

  load_config "$PROJECT_DIR/nonexistent.toml"
  [[ "$MAX_ITERATIONS" -eq 50 ]]
  [[ "$STUCK_THRESHOLD" -eq 5 ]]
}

# Proves: comments and blank lines are ignored.
@test "load_config ignores comments and blank lines" {
  cat > "$PROJECT_DIR/ralph.toml" <<'EOF'
# This is a comment
max_iterations = 30

  # indented comment
calls_per_hour = 60
EOF
  _CLI_MAX_ITERATIONS=""
  _CLI_CALLS_PER_HOUR=""

  load_config "$PROJECT_DIR/ralph.toml"
  [[ "$MAX_ITERATIONS" -eq 30 ]]
  [[ "$CALLS_PER_HOUR" -eq 60 ]]
}

# Proves: inline comments after values are stripped.
@test "load_config strips inline comments" {
  cat > "$PROJECT_DIR/ralph.toml" <<'EOF'
max_iterations = 15 # keep it short
EOF
  _CLI_MAX_ITERATIONS=""
  _CLI_CALLS_PER_HOUR=""

  load_config "$PROJECT_DIR/ralph.toml"
  [[ "$MAX_ITERATIONS" -eq 15 ]]
}

# Proves: quoted string values are unquoted.
@test "load_config handles quoted values" {
  cat > "$PROJECT_DIR/ralph.toml" <<'EOF'
max_iterations = "20"
EOF
  _CLI_MAX_ITERATIONS=""
  _CLI_CALLS_PER_HOUR=""

  load_config "$PROJECT_DIR/ralph.toml"
  [[ "$MAX_ITERATIONS" -eq 20 ]]
}

# Proves: all supported config keys are recognized.
@test "load_config handles all config keys" {
  cat > "$PROJECT_DIR/ralph.toml" <<'EOF'
max_iterations = 10
calls_per_hour = 20
watcher_interval = 5
stuck_threshold = 8
stuck_confirmation_threshold = 4
stagnation_threshold = 6
test_saturation_threshold = 7
permission_denial_threshold = 9
EOF
  _CLI_MAX_ITERATIONS=""
  _CLI_CALLS_PER_HOUR=""

  load_config "$PROJECT_DIR/ralph.toml"
  [[ "$MAX_ITERATIONS" -eq 10 ]]
  [[ "$CALLS_PER_HOUR" -eq 20 ]]
  [[ "$WATCHER_INTERVAL" -eq 5 ]]
  [[ "$STUCK_THRESHOLD" -eq 8 ]]
  [[ "$STUCK_CONFIRMATION_THRESHOLD" -eq 4 ]]
  [[ "$STAGNATION_THRESHOLD" -eq 6 ]]
  [[ "$TEST_SATURATION_THRESHOLD" -eq 7 ]]
  [[ "$PERMISSION_DENIAL_THRESHOLD" -eq 9 ]]
}

# Proves: analyze_iteration respects configurable thresholds.
@test "analyze_iteration uses configurable stagnation threshold" {
  STAGNATION_THRESHOLD=2
  ANALYSIS_RESULT="continue"
  _stagnant_count=0
  _test_only_count=0
  _stuck_count=0
  local logfile="$RALPH_DIR/test_iter.log"
  echo "some output" > "$logfile"
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD)

  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "continue" ]]
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "halt:stagnation" ]]
}

# Proves: --init-config generates a valid config file.
@test "init-config generates ralph.toml" {
  run bash "$RALPH_SH" --init-config "$PROJECT_DIR"
  [[ "$status" -eq 0 ]]
  [[ -f "$PROJECT_DIR/ralph.toml" ]]
  grep -q "max_iterations" "$PROJECT_DIR/ralph.toml"
  grep -q "stuck_threshold" "$PROJECT_DIR/ralph.toml"
}

# Proves: --init-config refuses to overwrite existing config.
@test "init-config refuses to overwrite existing config" {
  echo "existing" > "$PROJECT_DIR/ralph.toml"
  run bash "$RALPH_SH" --init-config "$PROJECT_DIR"
  [[ "$status" -eq 1 ]]
  [[ "$(cat "$PROJECT_DIR/ralph.toml")" == "existing" ]]
}
