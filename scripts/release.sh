#!/bin/bash
# release.sh — Release automation for Citadel CLI
#
# Builds cross-platform binaries, tags the release, and creates a GitHub
# Release with binaries attached. Idempotent — re-running after a failure
# resumes from the last completed step.
#
# Usage:
#   ./scripts/release.sh              # Minor bump release
#   ./scripts/release.sh patch        # Patch release
#   ./scripts/release.sh --dry-run    # Preview without changes
#   ./scripts/release.sh --status     # Check last release state
#   ./scripts/release.sh --resume     # Resume after failure
#   ./scripts/release.sh --clean      # Clear state and start fresh
#
# Version is derived from git tags (vX.Y.Z). The binary version is set
# at build time via ldflags.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

# ── Project config ─────────────────────────────────────────────────────────

RELEASE_PROJECT_NAME="citadel-cli"
RELEASE_TAG_PREFIX="v"
RELEASE_GITHUB_REPO="aceteam-ai/citadel-cli"
RELEASE_HAS_PRODUCTION="false"
RELEASE_STATE_DIR="$REPO_ROOT/.release"

# ── Config hooks ───────────────────────────────────────────────────────────

hook_get_version() {
  local tag
  tag=$(git tag --list 'v*' --sort=-version:refname 2>/dev/null | head -1)
  if [[ -n "$tag" ]]; then
    echo "${tag#v}"
  else
    echo "0.0.0"
  fi
}

hook_set_version() {
  # Version is set via git tag + ldflags at build time, not a file
  true
}

hook_test() {
  info "Running Go tests..."
  go test ./...
  info "Running Go vet..."
  go vet ./...
}

hook_build() {
  local version
  version=$(read_state "target_version")
  if [[ -z "$version" ]]; then
    fatal "No target version found in state."
  fi

  info "Building citadel-cli v${version} for all platforms..."

  local release_dir="$REPO_ROOT/release"
  rm -rf "$release_dir"
  mkdir -p "$release_dir"

  local ldflags="-X github.com/aceteam-ai/citadel-cli/cmd.version=${version}"
  local platforms=(
    "linux/amd64"
    "linux/arm64"
    "darwin/amd64"
    "darwin/arm64"
    "windows/amd64"
    "windows/arm64"
  )

  for platform in "${platforms[@]}"; do
    local os="${platform%/*}"
    local arch="${platform#*/}"
    local ext=""
    [[ "$os" == "windows" ]] && ext=".exe"

    info "  Building ${os}/${arch}..."
    local build_dir
    build_dir=$(mktemp -d)

    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
      -ldflags "$ldflags" \
      -o "$build_dir/citadel${ext}" \
      ./cmd/citadel

    local archive_name="citadel_v${version}_${os}_${arch}"
    if [[ "$os" == "windows" ]]; then
      (cd "$build_dir" && zip -q "$release_dir/${archive_name}.zip" "citadel${ext}")
    else
      tar -czf "$release_dir/${archive_name}.tar.gz" -C "$build_dir" "citadel${ext}"
    fi

    rm -rf "$build_dir"
  done

  (cd "$release_dir" && sha256sum * > checksums.txt)

  ok "Built $(ls "$release_dir"/*.tar.gz "$release_dir"/*.zip 2>/dev/null | wc -l) archives"
  ls -lh "$release_dir/"
}

hook_artifact() {
  local release_dir="$REPO_ROOT/release"
  if [[ -d "$release_dir" ]]; then
    find "$release_dir" -type f \( -name '*.tar.gz' -o -name '*.zip' -o -name 'checksums.txt' \) | sort
  fi
}

hook_post_release() {
  info "Next steps:"
  echo "  1. Update Homebrew tap with new version + checksums"
  echo "  2. Citadel OS will pick up the new CLI version on next ISO build"
}

# ── Load engine and run ────────────────────────────────────────────────────

source "$SCRIPT_DIR/release-engine.sh"
engine_main "$@"
