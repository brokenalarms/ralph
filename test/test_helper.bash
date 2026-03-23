#!/usr/bin/env bash
# Test helper: sources ralph.sh functions into a temp git repo environment.

RALPH_SH="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/ralph.sh"

# --- Source functions from ralph.sh (call before setup_test_repo) ---
source_ralph_functions() {
  RALPH_SOURCED=true
  source "$RALPH_SH"
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
  STOP_FILE="$RALPH_DIR/stop"
  LOG_FILE="$RALPH_DIR/loop.log"
  RAW_LOG="$RALPH_DIR/raw.log"
  RESUME_SCRIPT="$RALPH_DIR/resume.sh"
  touch "$LOG_FILE" "$RAW_LOG"

  WORK_DIR="$PROJECT_DIR"
  WORKTREE_BRANCH=""
  PROJECT_NAME="project"
  _TASK_SEQ=0
  _BRANCH_RENAMED=false

  SIGNAL_COMPLETE_FILE="$RALPH_DIR/.signal_complete"
  SIGNAL_TASK_FILE="$RALPH_DIR/.signal_current_task"
  SIGNAL_ALL_COMPLETE_FILE="$RALPH_DIR/.signal_all_complete"

  TASK_BACKEND="bd"
  RESUME=false
  USE_WORKTREE=true
  CALLS_PER_HOUR=80
  REFACTOR_THRESHOLD=0
  QUIET=false
  AUTO_MERGE=false

  PROMPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/go/cmd/ralph/prompts"
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
}

setup_rebase_env() {
  local origin_dir="$TEST_TMPDIR/origin"
  local default_branch
  default_branch=$(git -C "$PROJECT_DIR" branch --show-current)

  git clone --bare "$PROJECT_DIR" "$origin_dir" 2>/dev/null
  git -C "$PROJECT_DIR" remote add origin "$origin_dir" 2>/dev/null || true
  git -C "$PROJECT_DIR" fetch origin 2>/dev/null
  git -C "$origin_dir" symbolic-ref HEAD "refs/heads/$default_branch" 2>/dev/null || true

  init_ralph_dir
  setup_worktree

  git -C "$WORK_DIR" remote set-url origin "$origin_dir" 2>/dev/null || true
  git -C "$WORK_DIR" fetch origin 2>/dev/null

  TEST_DEFAULT_BRANCH="$default_branch"
  TEST_ORIGIN_DIR="$origin_dir"
}

push_to_origin() {
  git -C "$PROJECT_DIR" push origin "$TEST_DEFAULT_BRANCH" -q 2>/dev/null
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
