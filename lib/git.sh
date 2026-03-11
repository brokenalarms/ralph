#!/usr/bin/env bash
# Git worktree and branch management

temp_branch() { echo "ralph/$PROJECT_NAME/next"; }

setup_worktree() {
  WORK_DIR="$PROJECT_DIR"

  if [[ "$USE_WORKTREE" == false ]]; then
    return
  fi

  if ! git -C "$PROJECT_DIR" rev-parse --git-dir &>/dev/null; then
    log_error "Not a git repo — ralph requires git. Use --no-worktree to run without git isolation."
    exit 1
  fi


  # On resume, reuse existing worktree if stored in state
  if [[ "$RESUME" == true ]]; then
    local stored_worktree
    stored_worktree=$(read_state "worktree_dir")
    if [[ -n "$stored_worktree" && "$stored_worktree" != "null" && -d "$stored_worktree" ]]; then
      WORK_DIR="$stored_worktree"
      WORKTREE_BRANCH=$(read_state "worktree_branch")
      PROJECT_NAME=$(basename "$PROJECT_DIR")
      local named_branches
      named_branches=$(git -C "$PROJECT_DIR" branch --list "ralph/$PROJECT_NAME/*" 2>/dev/null | wc -l | tr -d ' ')
      _TASK_SEQ=$((named_branches))
      SIGNAL_FILE="$WORK_DIR/.ralph-signal"
      log "Resuming in worktree: $WORK_DIR (branch: $WORKTREE_BRANCH)"
      return
    fi
  fi

  PROJECT_NAME=$(basename "$PROJECT_DIR")

  local today
  today=$(date +%Y%m%d)
  local run_seq=1
  if [[ -d "$RALPH_DIR/worktrees" ]]; then
    local existing_today
    existing_today=$(find "$RALPH_DIR/worktrees" -maxdepth 1 -name "ralph-${today}-*" -type d 2>/dev/null | wc -l | tr -d ' ')
    run_seq=$((existing_today + 1))
  fi

  WORKTREE_BRANCH=$(temp_branch)
  WORK_DIR="$RALPH_DIR/worktrees/ralph-${today}-$(printf "%02d" $run_seq)"

  mkdir -p "$RALPH_DIR/worktrees"

  git -C "$PROJECT_DIR" worktree prune 2>/dev/null || true
  if git -C "$PROJECT_DIR" rev-parse --verify "$WORKTREE_BRANCH" &>/dev/null; then
    if ! git -C "$PROJECT_DIR" branch -D "$WORKTREE_BRANCH" 2>/dev/null; then
      local existing_wt
      existing_wt=$(git -C "$PROJECT_DIR" worktree list --porcelain 2>/dev/null | grep -B2 "branch refs/heads/$WORKTREE_BRANCH" | grep "^worktree " | sed 's/^worktree //')
      if [[ -n "$existing_wt" && "$existing_wt" == */.ralph/worktrees/* ]]; then
        log_warn "Removing stale ralph worktree: $existing_wt"
        git -C "$PROJECT_DIR" worktree remove --force "$existing_wt" 2>/dev/null || true
        git -C "$PROJECT_DIR" branch -D "$WORKTREE_BRANCH" 2>/dev/null || true
      else
        log_error "Cannot delete branch '$WORKTREE_BRANCH' — it is checked out in a non-ralph worktree: ${existing_wt:-unknown}"
        exit 1
      fi
    fi
  fi

  local default_branch
  default_branch=$(git -C "$PROJECT_DIR" symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's|refs/remotes/origin/||') || true
  default_branch=${default_branch:-main}

  git -C "$PROJECT_DIR" fetch origin "$default_branch" 2>/dev/null || true
  git -C "$PROJECT_DIR" worktree add -b "$WORKTREE_BRANCH" "$WORK_DIR" "origin/$default_branch" 2>/dev/null \
    || git -C "$PROJECT_DIR" worktree add -b "$WORKTREE_BRANCH" "$WORK_DIR" HEAD
  git -C "$WORK_DIR" config rebase.updateRefs true
  log "Worktree: $WORK_DIR (branch: $WORKTREE_BRANCH)"

  write_state "worktree_dir" "$WORK_DIR"
  write_state "worktree_branch" "$WORKTREE_BRANCH"

  SIGNAL_FILE="$WORK_DIR/.ralph-signal"
}

rename_branch_for_task() {
  local task_desc="$1"
  if [[ "$_BRANCH_RENAMED" == true || -z "$WORKTREE_BRANCH" || -z "$task_desc" ]]; then
    return
  fi
  if [[ "$WORK_DIR" == "$PROJECT_DIR" ]]; then
    return
  fi

  local slug
  slug=$(slugify "$task_desc")
  if [[ -z "$slug" ]]; then
    return
  fi

  _TASK_SEQ=$((_TASK_SEQ + 1))
  local new_branch="ralph/$PROJECT_NAME/$(printf "%02d" $_TASK_SEQ)-${slug}"
  if git -C "$WORK_DIR" branch -m "$WORKTREE_BRANCH" "$new_branch" 2>/dev/null; then
    WORKTREE_BRANCH="$new_branch"
    write_state "worktree_branch" "$WORKTREE_BRANCH"
    _BRANCH_RENAMED=true
  fi
}

rotate_branch() {
  if [[ -z "$WORKTREE_BRANCH" || "$WORK_DIR" == "$PROJECT_DIR" ]]; then
    return
  fi

  WORKTREE_BRANCH=$(temp_branch)
  git -C "$WORK_DIR" branch -D "$WORKTREE_BRANCH" 2>/dev/null || true
  if git -C "$WORK_DIR" checkout -b "$WORKTREE_BRANCH" 2>/dev/null; then
    write_state "worktree_branch" "$WORKTREE_BRANCH"
    _BRANCH_RENAMED=false
    log "Branch: $WORKTREE_BRANCH (from previous iteration)"
  else
    log_warn "Branch rotation failed, continuing on $WORKTREE_BRANCH"
  fi
}

rebase_onto_default_branch() {
  local default_branch
  default_branch=$(git -C "$PROJECT_DIR" symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's|refs/remotes/origin/||') || true
  default_branch=${default_branch:-main}
  git -C "$WORK_DIR" fetch origin "$default_branch" 2>/dev/null || true

  if git -C "$WORK_DIR" rebase --update-refs "origin/$default_branch" 2>/dev/null; then
    log "Rebased onto origin/$default_branch"
    return 0
  fi

  git -C "$WORK_DIR" rebase --abort 2>/dev/null || true
  log_warn "Rebase failed, checking for squash-merged branches..."

  local last_merged=""
  local branch
  while IFS= read -r branch; do
    branch="${branch#"${branch%%[![:space:]]*}"}"
    branch="${branch#\* }"
    [[ "$branch" == */next ]] && continue
    local merge_base
    merge_base=$(git -C "$WORK_DIR" merge-base "origin/$default_branch" "$branch" 2>/dev/null) || continue
    local branch_files
    branch_files=$(git -C "$WORK_DIR" diff --name-only "$merge_base" "$branch" 2>/dev/null)
    if [[ -z "$branch_files" ]]; then
      continue
    fi
    if git -C "$WORK_DIR" diff --quiet "$branch" "origin/$default_branch" -- $branch_files 2>/dev/null; then
      last_merged="$branch"
    fi
  done < <(git -C "$PROJECT_DIR" branch --list "ralph/$PROJECT_NAME/*" --sort=refname 2>/dev/null)

  if [[ -z "$last_merged" ]]; then
    log_error "Rebase onto $default_branch failed with real conflicts — halting"
    return 1
  fi

  log "Detected squash-merged branch: $last_merged"

  if ! git -C "$WORK_DIR" rebase --update-refs --onto "origin/$default_branch" "$last_merged" HEAD 2>/dev/null; then
    git -C "$WORK_DIR" rebase --abort 2>/dev/null || true
    log_error "Rebase onto $default_branch past squash-merged branches failed — halting"
    return 1
  fi

  log "Rebased onto origin/$default_branch (skipped squash-merged branches)"

  local remaining_seq=0
  while IFS= read -r branch; do
    branch="${branch#"${branch%%[![:space:]]*}"}"
    branch="${branch#\* }"
    [[ "$branch" == */next ]] && continue
    if git -C "$WORK_DIR" rev-parse --verify "$branch" &>/dev/null; then
      local seq_num
      seq_num=$(echo "$branch" | grep -oE '/[0-9]+-' | head -1 | tr -dc '0-9')
      if [[ -n "$seq_num" && "$seq_num" -gt "$remaining_seq" ]]; then
        remaining_seq=$seq_num
      fi
    fi
  done < <(git -C "$PROJECT_DIR" branch --list "ralph/$PROJECT_NAME/*" --sort=refname 2>/dev/null)

  git -C "$PROJECT_DIR" branch -D "$last_merged" 2>/dev/null || true
  _TASK_SEQ=$remaining_seq
  return 0
}
