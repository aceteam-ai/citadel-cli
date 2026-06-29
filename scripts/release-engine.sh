#!/bin/bash
# release-engine.sh — Shared release automation engine
#
# Vendored into each repo's release directory. Driven by a per-repo
# release.config.sh that defines hooks for version read/write, build,
# test, and artifact handling.
#
# Features:
#   - Step-based state file for idempotent resume
#   - Durable logging (not /tmp)
#   - --status to check last run
#   - --dry-run preview
#   - --resume to pick up from failure
#   - --clean to start fresh
#
# Config hooks (defined in release.config.sh):
#   RELEASE_PROJECT_NAME     — e.g., "citadel-os", "citadel-cli", "aceteam"
#   RELEASE_TAG_PREFIX       — e.g., "os-v", "v"
#   RELEASE_GITHUB_REPO      — e.g., "aceteam-ai/citadel"
#   RELEASE_HAS_PRODUCTION   — "true" if main→production merge is needed
#   hook_get_version()       — print current version to stdout
#   hook_set_version(v)      — write new version
#   hook_test()              — run tests (optional, default: no-op)
#   hook_build()             — run build (optional, default: no-op)
#   hook_artifact()          — print path to release artifact (optional)
#   hook_post_release()      — run after GitHub release (optional)

set -euo pipefail

# ── Colors ──────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

info()    { echo -e "${BLUE}[INFO]${NC}    $*"; }
ok()      { echo -e "${GREEN}[OK]${NC}      $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}    $*"; }
err()     { echo -e "${RED}[ERROR]${NC}   $*"; }
fatal()   { err "$*"; exit 1; }
step_msg() { echo -e "${CYAN}[STEP]${NC}    $*"; }

# ── State management ───────────────────────────────────────────────────────

RELEASE_STATE_DIR="${RELEASE_STATE_DIR:-.release}"
STATE_FILE="$RELEASE_STATE_DIR/state"
LOG_FILE="$RELEASE_STATE_DIR/release.log"

ensure_state_dir() {
  mkdir -p "$RELEASE_STATE_DIR"
}

read_state() {
  local key=$1
  if [[ -f "$STATE_FILE" ]]; then
    grep "^${key}=" "$STATE_FILE" 2>/dev/null | cut -d= -f2- || true
  fi
}

write_state() {
  local key=$1 value=$2
  ensure_state_dir
  if grep -q "^${key}=" "$STATE_FILE" 2>/dev/null; then
    sed -i "s|^${key}=.*|${key}=${value}|" "$STATE_FILE"
  else
    echo "${key}=${value}" >> "$STATE_FILE"
  fi
}

step_done() {
  local step=$1
  [[ "$(read_state "step_${step}")" == "done" ]]
}

mark_step() {
  local step=$1
  write_state "step_${step}" "done"
  write_state "last_step" "$step"
  write_state "last_step_time" "$(date -Iseconds)"
}

clear_state() {
  rm -rf "$RELEASE_STATE_DIR"
  ok "Release state cleared."
}

# ── Logging ────────────────────────────────────────────────────────────────

log() {
  ensure_state_dir
  echo "[$(date -Iseconds)] $*" >> "$LOG_FILE"
}

log_and_info() {
  log "$*"
  info "$*"
}

# ── Version helpers ────────────────────────────────────────────────────────

bump_version() {
  local current=$1 bump_type=$2
  IFS='.' read -r major minor patch <<< "$current"

  case $bump_type in
    major)  echo "$((major + 1)).0.0" ;;
    minor)  echo "${major}.$((minor + 1)).0" ;;
    patch)  echo "${major}.${minor}.$((patch + 1))" ;;
    *)
      if [[ $bump_type =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "$bump_type"
      else
        fatal "Invalid version format: $bump_type (expected: X.Y.Z, major, minor, or patch)"
      fi
      ;;
  esac
}

# ── Status command ─────────────────────────────────────────────────────────

show_status() {
  echo ""
  echo "  Release Status: ${RELEASE_PROJECT_NAME:-unknown}"
  echo "  ─────────────────────────────────────"

  if [[ ! -f "$STATE_FILE" ]]; then
    echo "  No release in progress."
    echo ""
    echo "  Current version: $(hook_get_version 2>/dev/null || echo 'unknown')"
    local latest_tag
    latest_tag=$(git tag --list "${RELEASE_TAG_PREFIX:-v}*" --sort=-version:refname 2>/dev/null | head -1)
    echo "  Latest tag:      ${latest_tag:-none}"
    echo ""
    return 0
  fi

  local version target last_step last_time
  version=$(read_state "current_version")
  target=$(read_state "target_version")
  last_step=$(read_state "last_step")
  last_time=$(read_state "last_step_time")

  echo "  Current version: ${version:-unknown}"
  echo "  Target version:  ${target:-unknown}"
  echo "  Last step:       ${last_step:-none}"
  echo "  Last step time:  ${last_time:-unknown}"
  echo ""

  local steps=("validate" "bump" "commit" "tag" "build" "push" "release")
  for s in "${steps[@]}"; do
    if step_done "$s"; then
      echo -e "  ${GREEN}✓${NC} $s"
    elif [[ "$s" == "$last_step" ]]; then
      echo -e "  ${YELLOW}▸${NC} $s (current)"
    else
      echo -e "  ${NC}○${NC} $s"
    fi
  done

  echo ""

  if [[ -f "$LOG_FILE" ]]; then
    echo "  Recent log:"
    tail -5 "$LOG_FILE" | sed 's/^/    /'
    echo ""
  fi

  return 0
}

# ── Release journal ────────────────────────────────────────────────────────

append_release_journal() {
  local version=$1
  local tag_prefix="${RELEASE_TAG_PREFIX:-v}"
  local date=$(date +%Y-%m-%d)
  local latest_tag
  latest_tag=$(git tag --list "${tag_prefix}*" --sort=-version:refname 2>/dev/null | head -1)

  local journal_file="RELEASES.md"

  if [[ ! -f "$journal_file" ]]; then
    cat > "$journal_file" << HEADER
# Release Journal — ${RELEASE_PROJECT_NAME:-unknown}

Auto-generated log of every release.

HEADER
  fi

  local commit_count=0
  local lines_added=0
  local files_changed=0
  local commit_list=""

  if [[ -n "$latest_tag" ]]; then
    commit_count=$(git log "${latest_tag}..HEAD" --oneline 2>/dev/null | wc -l | tr -d ' ')
    local diffstat
    diffstat=$(git diff --shortstat "$latest_tag"..HEAD 2>/dev/null || echo "")
    files_changed=$(echo "$diffstat" | grep -o '[0-9]* file' | grep -o '[0-9]*' || echo 0)
    lines_added=$(echo "$diffstat" | grep -o '[0-9]* insertion' | grep -o '[0-9]*' || echo 0)

    while IFS= read -r line; do
      [[ -n "$line" ]] && commit_list="${commit_list}\n  - ${line}"
    done < <(git log "${latest_tag}..HEAD" --oneline --format="%s" 2>/dev/null)
  fi

  local entry=""
  entry+="---\n\n"
  entry+="## ${tag_prefix}${version} — ${date}\n\n"
  entry+="| Metric | Value |\n"
  entry+="|--------|-------|\n"
  entry+="| Commits | ${commit_count} |\n"
  entry+="| Files changed | ${files_changed} |\n"
  entry+="| Lines added | +${lines_added} |\n"

  if [[ -n "$commit_list" ]]; then
    entry+="\n**Changes:**\n${commit_list}\n"
  fi

  entry+="\n"

  echo -e "$entry" >> "$journal_file"
  log "Appended ${tag_prefix}${version} to $journal_file"
}

# ── Default hooks (only set if not already defined by the sourcing script) ──

declare -F hook_test         &>/dev/null || hook_test()         { true; }
declare -F hook_build        &>/dev/null || hook_build()        { true; }
declare -F hook_artifact     &>/dev/null || hook_artifact()     { true; }
declare -F hook_post_release &>/dev/null || hook_post_release() { true; }

# ── Main release flow ─────────────────────────────────────────────────────

run_release() {
  local bump_type="${1:-minor}"
  local dry_run="${DRY_RUN:-false}"
  local allow_dirty="${ALLOW_DIRTY:-false}"

  log "=== Release started: bump=$bump_type dry_run=$dry_run ==="

  # ── Step 1: Validate ──────────────────────────────────────────────────
  if ! step_done "validate"; then
    step_msg "Validating prerequisites..."

    # Clean-tree check: catches untracked, modified, and staged changes.
    # Runs in all modes (including --dry-run) to prevent dirty releases.
    if [[ "$allow_dirty" != "true" ]]; then
      if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
        fatal "Working tree is not clean. Commit, stash, or remove untracked files first.\n       Use --allow-dirty to override."
      fi
    else
      warn "Skipping clean-tree check (--allow-dirty)."
    fi

    local current_branch
    current_branch=$(git branch --show-current)
    if [[ "$current_branch" != "main" ]]; then
      if [[ "$dry_run" == "true" ]]; then
        warn "Not on main branch (on: $current_branch). Dry run continues."
      else
        fatal "Releases must be made from main. Currently on: $current_branch"
      fi
    fi

    if [[ "$dry_run" != "true" ]]; then
      log_and_info "Pulling latest from origin..."
      git pull --ff-only origin main || fatal "Failed to pull. Resolve conflicts first."
    fi

    local current_version
    current_version=$(hook_get_version)
    local new_version
    new_version=$(bump_version "$current_version" "$bump_type")

    write_state "current_version" "$current_version"
    write_state "target_version" "$new_version"
    write_state "bump_type" "$bump_type"

    log "Validated: $current_version -> $new_version"

    if [[ "$dry_run" != "true" ]]; then
      mark_step "validate"
    fi
  fi

  local current_version new_version
  current_version=$(read_state "current_version")
  new_version=$(read_state "target_version")

  info "Version: $current_version → $new_version"
  info "Tag:     ${RELEASE_TAG_PREFIX}${new_version}"

  if [[ "$dry_run" == "true" ]]; then
    echo ""
    ok "Dry run complete."
    echo "  Would bump: $current_version → $new_version"
    echo "  Would tag:  ${RELEASE_TAG_PREFIX}${new_version}"
    echo "  Would build: $(type -t hook_build &>/dev/null && echo yes || echo no)"
    if [[ "${RELEASE_HAS_PRODUCTION:-false}" == "true" ]]; then
      echo "  Would merge: main → production"
    fi
    return 0
  fi

  # ── Step 2: Bump version ──────────────────────────────────────────────
  if ! step_done "bump"; then
    step_msg "Bumping version to $new_version..."
    hook_set_version "$new_version"
    mark_step "bump"
    log "Version bumped to $new_version"
  else
    ok "Version already bumped."
  fi

  # ── Step 3: Test ──────────────────────────────────────────────────────
  if ! step_done "test"; then
    step_msg "Running tests..."
    log "Running tests..."
    local _test_ok
    _test_ok=$(mktemp)
    set +e
    ( set -euo pipefail; hook_test; echo ok > "$_test_ok" ) 2>&1 | tee -a "$LOG_FILE"
    set -e
    if [[ ! -s "$_test_ok" ]]; then
      rm -f "$_test_ok"
      fatal "Tests failed. Fix and re-run to resume."
    fi
    rm -f "$_test_ok"
    mark_step "test"
    log "Tests passed"
  else
    ok "Tests already passed."
  fi

  # ── Step 3b: Release journal ──────────────────────────────────────────
  if ! step_done "journal"; then
    step_msg "Updating release journal..."
    append_release_journal "$new_version"
    mark_step "journal"
  fi

  # ── Step 4: Commit ────────────────────────────────────────────────────
  if ! step_done "commit"; then
    step_msg "Creating release commit..."
    git add -A
    git commit -m "chore(release): ${RELEASE_TAG_PREFIX}${new_version}" || true
    mark_step "commit"
    log "Committed"
  else
    ok "Already committed."
  fi

  # ── Step 5: Tag ───────────────────────────────────────────────────────
  if ! step_done "tag"; then
    step_msg "Creating tag ${RELEASE_TAG_PREFIX}${new_version}..."
    git tag -a "${RELEASE_TAG_PREFIX}${new_version}" -m "Release ${RELEASE_TAG_PREFIX}${new_version}"
    mark_step "tag"
    log "Tagged ${RELEASE_TAG_PREFIX}${new_version}"
  else
    ok "Already tagged."
  fi

  # ── Step 6: Build ─────────────────────────────────────────────────────
  if ! step_done "build"; then
    step_msg "Building..."
    log "Build started"
    local _build_ok
    _build_ok=$(mktemp)
    set +e
    ( set -euo pipefail; hook_build; echo ok > "$_build_ok" ) 2>&1 | tee -a "$LOG_FILE"
    set -e
    if [[ ! -s "$_build_ok" ]]; then
      rm -f "$_build_ok"
      fatal "Build failed. Fix and re-run to resume from build step."
    fi
    rm -f "$_build_ok"
    mark_step "build"
    log "Build completed"
  else
    ok "Already built."
  fi

  # ── Step 7: Push ──────────────────────────────────────────────────────
  if ! step_done "push"; then
    step_msg "Pushing to origin..."
    git push origin main --follow-tags
    log "Pushed main + tags"

    if [[ "${RELEASE_HAS_PRODUCTION:-false}" == "true" ]]; then
      step_msg "Merging main → production..."
      git checkout production
      git pull --ff-only origin production 2>/dev/null || true
      git merge --ff-only main || fatal "Cannot fast-forward production. Resolve divergence."
      git push origin production
      git checkout main
      log "Merged to production"
    fi

    mark_step "push"
  else
    ok "Already pushed."
  fi

  # ── Step 8: GitHub Release ────────────────────────────────────────────
  if ! step_done "release"; then
    if command -v gh &>/dev/null && [[ -n "${RELEASE_GITHUB_REPO:-}" ]]; then
      step_msg "Creating GitHub Release..."

      # If the project defines a release-notes preamble hook (e.g. to stamp a
      # wire-protocol compatibility line), write it to a file and prepend it to
      # the auto-generated notes.
      local notes_args=("--generate-notes")
      if declare -F hook_release_notes_preamble &>/dev/null; then
        local preamble_file
        preamble_file="$(mktemp)"
        hook_release_notes_preamble >"$preamble_file" 2>/dev/null || true
        if [[ -s "$preamble_file" ]]; then
          notes_args=("--notes-file" "$preamble_file" "--generate-notes")
        fi
      fi

      local release_args=(
        "--repo" "$RELEASE_GITHUB_REPO"
        "--title" "${RELEASE_TAG_PREFIX}${new_version}"
        "${notes_args[@]}"
      )

      # Collect artifacts (hook_artifact may return multiple lines)
      local artifacts=()
      while IFS= read -r artifact_path; do
        if [[ -n "$artifact_path" && -f "$artifact_path" ]]; then
          info "Attaching: $(basename "$artifact_path") ($(du -h "$artifact_path" | cut -f1))"
          artifacts+=("$artifact_path")
        fi
      done < <(hook_artifact 2>/dev/null || true)

      gh release create "${RELEASE_TAG_PREFIX}${new_version}" "${release_args[@]}" "${artifacts[@]}"
      log "GitHub Release created"
    else
      warn "Skipping GitHub Release (gh not found or RELEASE_GITHUB_REPO not set)."
    fi

    hook_post_release || true

    mark_step "release"
  else
    ok "Already released."
  fi

  # ── Done ──────────────────────────────────────────────────────────────
  echo ""
  ok "Release ${RELEASE_TAG_PREFIX}${new_version} complete!"
  echo ""
  echo "  ┌─────────────────────────────────┐"
  echo "  │ Previous: $current_version"
  printf "  │ Released: %s\n" "${RELEASE_TAG_PREFIX}${new_version}"
  echo "  │ Tag:      ${RELEASE_TAG_PREFIX}${new_version}"
  if [[ "${RELEASE_HAS_PRODUCTION:-false}" == "true" ]]; then
    echo "  │ Production: updated"
  fi
  echo "  └─────────────────────────────────┘"
  echo ""

  log "=== Release ${RELEASE_TAG_PREFIX}${new_version} completed ==="

  clear_state
}

# ── Entry point (called by repo-specific release.sh) ──────────────────────

engine_main() {
  local bump_type=""

  while [[ $# -gt 0 ]]; do
    case $1 in
      --dry-run)      DRY_RUN=true; shift ;;
      --allow-dirty)  ALLOW_DIRTY=true; shift ;;
      --status)       show_status; exit 0 ;;
      --clean)        clear_state; exit 0 ;;
      --resume)       shift ;; # resume is the default behavior
      -h|--help)
        echo "Usage: $0 [OPTIONS] [BUMP_TYPE]"
        echo ""
        echo "Bump types (default: minor):"
        echo "  patch      Patch version bump"
        echo "  minor      Minor version bump"
        echo "  major      Major version bump"
        echo "  X.Y.Z      Explicit version"
        echo ""
        echo "Options:"
        echo "  --dry-run       Preview changes without modifying anything"
        echo "  --allow-dirty   Allow release from a dirty working tree"
        echo "  --status        Show current release state and last run info"
        echo "  --resume        Resume a failed release (default behavior)"
        echo "  --clean         Clear release state and start fresh"
        echo "  -h, --help      Show this help"
        echo ""
        echo "Project: ${RELEASE_PROJECT_NAME:-unknown}"
        echo "Tag prefix: ${RELEASE_TAG_PREFIX:-v}"
        exit 0
        ;;
      *)
        if [[ -z "$bump_type" ]]; then
          bump_type=$1
        else
          fatal "Unexpected argument: $1"
        fi
        shift
        ;;
    esac
  done

  run_release "${bump_type:-minor}"
}
