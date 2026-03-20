#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
  init_ralph_dir
  REFACTOR_THRESHOLD=20
}

teardown() {
  teardown_test_repo
}

# Proves: refactor prompt includes shared standards (Boy Scout Rule) and refactor-specific instructions.
@test "build_refactor_prompt includes shared standards and refactor instructions" {
  local prompt
  prompt=$(build_refactor_prompt "ralph.sh server.js")
  [[ "$prompt" == *"Boy Scout Rule"* ]]
  [[ "$prompt" == *"refactor-only iteration"* ]]
  [[ "$prompt" == *"ralph.sh server.js"* ]]
}

# Proves: refactor prompt resolves template variables for work dir and signal.
@test "build_refactor_prompt resolves template variables" {
  local prompt
  prompt=$(build_refactor_prompt "lib/tasks.sh")
  [[ "$prompt" == *"$WORK_DIR"* ]]
  [[ "$prompt" == *".signal_complete"* ]]
  [[ "$prompt" != *"{{WORK_DIR}}"* ]]
  [[ "$prompt" != *"{{RALPH_DIR}}"* ]]
  [[ "$prompt" != *"{{RECENT_FILES}}"* ]]
  [[ "$prompt" != *"{{QUALITY_FINDINGS}}"* ]]
}

# Proves: refactor prompt injects quality findings so Claude knows what triggered the refactor.
@test "build_refactor_prompt includes quality findings" {
  local prompt
  prompt=$(build_refactor_prompt "src/auth.ts" "src/auth.ts:
  - 3x untyped \`any\` usage
  - 550 lines (50 over 500-line threshold)")
  [[ "$prompt" == *"3x untyped"* ]]
  [[ "$prompt" == *"550 lines"* ]]
  [[ "$prompt" == *"Quality signals detected"* ]]
}

# Proves: refactor prompt injects the recent files list so Claude knows what to review.
@test "build_refactor_prompt includes recent files in output" {
  local prompt
  prompt=$(build_refactor_prompt "src/auth.sh
src/utils.sh
test/auth.bats")
  [[ "$prompt" == *"src/auth.sh"* ]]
  [[ "$prompt" == *"src/utils.sh"* ]]
  [[ "$prompt" == *"test/auth.bats"* ]]
}

# Proves: quality_score state defaults to 0 when initialized.
@test "quality_score defaults to 0 in initial state" {
  write_state "quality_score" "0"
  local val
  val=$(read_state "quality_score")
  [[ "$val" == "0" ]]
}

# Proves: quality_score state can be written and read back.
@test "quality_score tracks across writes" {
  write_state "quality_score" "25"
  local val
  val=$(read_state "quality_score")
  [[ "$val" == "25" ]]
}

# Proves: REFACTOR_THRESHOLD defaults to 20 for adaptive quality-based refactoring.
@test "REFACTOR_THRESHOLD defaults to 20" {
  [[ "$REFACTOR_THRESHOLD" == "20" ]]
}

# Proves: assess_quality scores files with `any` type usage.
@test "assess_quality detects any type usage in TypeScript" {
  local test_file="$WORK_DIR/src/test.ts"
  mkdir -p "$WORK_DIR/src"
  cat > "$test_file" << 'EOF'
function parse(data: any): any {
  return data as any;
}
EOF
  local findings_file="$RALPH_DIR/.quality-findings"
  assess_quality "$WORK_DIR" "$findings_file" "src/test.ts"
  (( QUALITY_SCORE >= 9 ))
  [[ -s "$findings_file" ]]
  grep -q "untyped" "$findings_file"
}

# Proves: assess_quality scores files over 500 lines.
@test "assess_quality detects oversized files" {
  local test_file="$WORK_DIR/big.sh"
  for i in $(seq 1 550); do
    echo "echo 'line $i'" >> "$test_file"
  done
  local findings_file="$RALPH_DIR/.quality-findings"
  assess_quality "$WORK_DIR" "$findings_file" "big.sh"
  (( QUALITY_SCORE > 0 ))
  grep -q "over 500-line" "$findings_file"
}

# Proves: assess_quality detects silent catches.
@test "assess_quality detects silent catches" {
  local test_file="$WORK_DIR/src/handler.ts"
  mkdir -p "$WORK_DIR/src"
  cat > "$test_file" << 'EOF'
try {
  doSomething();
} catch (e) {}
EOF
  local findings_file="$RALPH_DIR/.quality-findings"
  assess_quality "$WORK_DIR" "$findings_file" "src/handler.ts"
  (( QUALITY_SCORE >= 5 ))
  grep -q "silent catch" "$findings_file"
}

# Proves: assess_quality returns 0 for clean files with no quality issues.
@test "assess_quality returns 0 for clean files" {
  local test_file="$WORK_DIR/clean.sh"
  echo '#!/bin/bash' > "$test_file"
  echo 'echo "hello"' >> "$test_file"
  local findings_file="$RALPH_DIR/.quality-findings"
  assess_quality "$WORK_DIR" "$findings_file" "clean.sh"
  (( QUALITY_SCORE == 0 ))
  [[ ! -s "$findings_file" ]]
}

# Proves: assess_quality detects console.log ghosts in JS/TS files.
@test "assess_quality detects console.log ghosts" {
  local test_file="$WORK_DIR/src/debug.js"
  mkdir -p "$WORK_DIR/src"
  cat > "$test_file" << 'EOF'
console.log('here');
console.debug('test');
console.warn('????');
EOF
  local findings_file="$RALPH_DIR/.quality-findings"
  assess_quality "$WORK_DIR" "$findings_file" "src/debug.js"
  (( QUALITY_SCORE >= 6 ))
  grep -q "console.log" "$findings_file"
}

# Proves: planning prompt includes debt assessment with balanced guidance (not just a checklist).
@test "planning prompt includes debt assessment section" {
  local planning_prompt
  planning_prompt=$(<"$PROMPTS_DIR/planning.md")
  [[ "$planning_prompt" == *"Debt assessment"* ]]
  [[ "$planning_prompt" == *"Dead code"* ]]
  [[ "$planning_prompt" == *"500 lines"* ]]
  [[ "$planning_prompt" == *"don't add refactor tasks just because you can"* ]]
}

# Proves: shared prompt includes Boy Scout Rule as a reminder, not a mandate.
@test "shared prompt includes Boy Scout Rule" {
  local shared
  shared=$(<"$PROMPTS_DIR/shared.md")
  [[ "$shared" == *"Boy Scout Rule"* ]]
  [[ "$shared" == *"Dead code"* ]]
  [[ "$shared" == *"leave it alone"* ]]
}

# Proves: refactor prompt enforces behavior preservation — refactoring must not change functionality.
@test "refactor prompt enforces no behavior change" {
  local prompt
  prompt=$(build_refactor_prompt "file.sh")
  [[ "$prompt" == *"Do NOT add new features or change behavior"* ]]
}

# Proves: refactor prompt requires refactor: commit prefix for clear git history.
@test "refactor prompt requires refactor: commit prefix" {
  local prompt
  prompt=$(build_refactor_prompt "file.sh")
  [[ "$prompt" == *'refactor:'* ]]
}

# Proves: refactor prompt allows skipping when no meaningful debt exists (no forced busywork).
@test "refactor prompt allows no-op when nothing meaningful found" {
  local prompt
  prompt=$(build_refactor_prompt "file.sh")
  [[ "$prompt" == *"signal completion without making changes"* ]]
}

# Proves: refactor prompt warns against premature abstractions and one-line utility extraction.
@test "refactor prompt discourages premature abstractions" {
  local prompt
  prompt=$(build_refactor_prompt "file.sh")
  [[ "$prompt" == *"utility functions"* || "$prompt" == *"one-time operations"* ]]
}

# Proves: refactor prompt uses 500 lines as the split signal per Church of Clean Code thresholds.
@test "refactor prompt references 500 line threshold" {
  local prompt
  prompt=$(build_refactor_prompt "file.sh")
  [[ "$prompt" == *"500"* ]]
}
