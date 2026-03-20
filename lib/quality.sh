#!/usr/bin/env bash
# Quality signal assessment for adaptive refactoring.
# Scans recently changed files and produces a numeric pain score
# based on code quality signals from the refactor-style commandments.

# Assess quality of changed files and output a pain score.
# Sets QUALITY_SCORE (int) and writes findings to QUALITY_FINDINGS_FILE.
# Args: $1 = work_dir, $2 = findings_file, $3... = files to assess
assess_quality() {
  local work_dir="$1"
  local findings_file="$2"
  shift 2
  local files=("$@")

  QUALITY_SCORE=0
  local findings=""

  for file in "${files[@]}"; do
    local full_path="$work_dir/$file"
    [[ -f "$full_path" ]] || continue

    local file_findings=""

    # I. Types — `any` usage in TypeScript/JavaScript files
    if [[ "$file" =~ \.(ts|tsx|js|jsx)$ ]]; then
      local any_count
      any_count=$(grep -cE '\bany\b' "$full_path" 2>/dev/null || echo 0)
      if (( any_count > 0 )); then
        QUALITY_SCORE=$(( QUALITY_SCORE + any_count * 3 ))
        file_findings+="  - ${any_count}x untyped \`any\` usage"$'\n'
      fi
    fi

    # II. Error handling — silent catches
    local silent_catch_count
    silent_catch_count=$(grep -cE 'catch\s*\([^)]*\)\s*\{\s*\}' "$full_path" 2>/dev/null || echo 0)
    if (( silent_catch_count > 0 )); then
      QUALITY_SCORE=$(( QUALITY_SCORE + silent_catch_count * 5 ))
      file_findings+="  - ${silent_catch_count}x silent catch (swallowed error)"$'\n'
    fi

    # VI. Size — files over 500 lines
    local line_count
    line_count=$(wc -l < "$full_path" 2>/dev/null || echo 0)
    line_count=$(( line_count + 0 ))
    if (( line_count > 500 )); then
      local overage=$(( line_count - 500 ))
      local size_penalty=$(( overage / 50 ))
      (( size_penalty < 1 )) && size_penalty=1
      QUALITY_SCORE=$(( QUALITY_SCORE + size_penalty * 2 ))
      file_findings+="  - ${line_count} lines (${overage} over 500-line threshold)"$'\n'
    fi

    # IX. Dead code — TODO/FIXME/HACK markers
    local todo_count
    todo_count=$(grep -ciE '\b(TODO|FIXME|HACK|XXX)\b' "$full_path" 2>/dev/null || echo 0)
    if (( todo_count > 2 )); then
      QUALITY_SCORE=$(( QUALITY_SCORE + (todo_count - 2) * 2 ))
      file_findings+="  - ${todo_count}x TODO/FIXME markers"$'\n'
    fi

    # The Bestiary — console.log ghosts
    if [[ "$file" =~ \.(ts|tsx|js|jsx)$ ]]; then
      local log_count
      log_count=$(grep -cE 'console\.(log|debug|warn)\(' "$full_path" 2>/dev/null || echo 0)
      if (( log_count > 0 )); then
        QUALITY_SCORE=$(( QUALITY_SCORE + log_count * 2 ))
        file_findings+="  - ${log_count}x console.log/debug/warn calls"$'\n'
      fi
    fi

    # VIII. Naming — utility junk drawer files
    local basename
    basename=$(basename "$file")
    if [[ "$basename" =~ ^(utils|helpers|misc|stuff|common)\.(ts|tsx|js|jsx|py|go)$ ]]; then
      QUALITY_SCORE=$(( QUALITY_SCORE + 5 ))
      file_findings+="  - junk-drawer filename (${basename})"$'\n'
    fi

    if [[ -n "$file_findings" ]]; then
      findings+="$file:"$'\n'"$file_findings"
    fi
  done

  if [[ -n "$findings" ]]; then
    printf '%s' "$findings" > "$findings_file"
  else
    : > "$findings_file"
  fi
}
