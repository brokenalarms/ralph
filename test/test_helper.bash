#!/usr/bin/env bash
# Test helper: sources ralph.sh functions into a temp git repo environment.

RALPH_SH="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/ralph.sh"

# --- Source functions from ralph.sh (call before setup_test_repo) ---
source_ralph_functions() {
  RALPH_SOURCED=true
  source "$RALPH_SH"
  # Remove the cleanup trap that ralph.sh installs
  trap - EXIT
}

# --- Setup temp git repo (call after source_ralph_functions) ---
setup_test_repo() {
  TEST_TMPDIR="$(mktemp -d)"
  PROJECT_DIR="$TEST_TMPDIR/project"
  mkdir -p "$PROJECT_DIR"
  git -C "$PROJECT_DIR" init -q
  git -C "$PROJECT_DIR" config user.name "test"
  git -C "$PROJECT_DIR" config user.email "test@test"
  git -C "$PROJECT_DIR" commit --allow-empty -m "init" -q

  RALPH_DIR="$PROJECT_DIR/.ralph"
  mkdir -p "$RALPH_DIR"

  PLAN_FILE="$RALPH_DIR/plan.md"
  STATE_FILE="$RALPH_DIR/state.json"
  SIGNAL_FILE="$RALPH_DIR/signal"
  STOP_FILE="$RALPH_DIR/stop"
  LOG_FILE="$RALPH_DIR/loop.log"
  touch "$LOG_FILE"

  WORK_DIR="$PROJECT_DIR"
  WORKTREE_BRANCH=""
  PROJECT_NAME="project"
  _TASK_SEQ=0
  _BRANCH_RENAMED=false

  SIGNAL_TOKEN="###RALPH_TASK_COMPLETE###"
  CURRENT_TASK_TOKEN="###RALPH_CURRENT_TASK###"
  ALL_COMPLETE_TOKEN="###RALPH_ALL_COMPLETE###"

  PLAN_FILE_ARG=""
  RESUME=false
  USE_WORKTREE=true
  CALLS_PER_HOUR=80
  QUIET=false

  PROMPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/prompts"
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
}

teardown_test_repo() {
  if [[ -n "${TEST_TMPDIR:-}" && -d "$TEST_TMPDIR" ]]; then
    # Remove any worktrees before deleting
    if git -C "$PROJECT_DIR" rev-parse --git-dir &>/dev/null 2>/dev/null; then
      git -C "$PROJECT_DIR" worktree list --porcelain 2>/dev/null | \
        grep '^worktree ' | sed 's/^worktree //' | while read -r wt; do
          [[ "$wt" == "$PROJECT_DIR" ]] && continue
          git -C "$PROJECT_DIR" worktree remove --force "$wt" 2>/dev/null || true
        done
    fi
    rm -rf "$TEST_TMPDIR"
  fi
}
