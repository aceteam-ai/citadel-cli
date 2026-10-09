#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: ./build-dmg.sh [--arch <arm64|aarch64|amd64|x86_64|target-triple>]

Build one unsigned, architecture-specific Citadel Tauri DMG. With no --arch,
the current host architecture is used.

Targets:
  arm64, aarch64, aarch64-apple-darwin  Apple Silicon
  amd64, x86_64, x86_64-apple-darwin   Intel

The legacy --binary and --version options are retired. The Tauri bundle owns
the bundled Citadel helper and application version.

Developer ID signing, Apple notarization, stapling, native installer
acceptance, and publishing remain separate distribution gates.
EOF
}

fail() {
  printf 'Error: %s\n' "$1" >&2
  return 2
}

target_for_arch() {
  case "$1" in
    arm64|aarch64|aarch64-apple-darwin)
      printf '%s\n' aarch64-apple-darwin
      ;;
    amd64|x86_64|x86_64-apple-darwin)
      printf '%s\n' x86_64-apple-darwin
      ;;
    *)
      fail "Unsupported macOS architecture: $1"
      ;;
  esac
}

reject_symlink_components() {
  local root="$1"
  local relative="$2"
  local current="$root"
  local component
  local -a components

  IFS=/ read -r -a components <<< "$relative"
  for component in "${components[@]}"; do
    current="$current/$component"
    if [[ -L "$current" ]]; then
      fail "Refusing symlinked DMG builder path: $current"
      return
    fi
  done
}

resolve_delegate() {
  local repo_root="$1"
  local relative='desktop/scripts/build-macos-dmg.sh'
  local delegate="$repo_root/$relative"

  reject_symlink_components "$repo_root" "$relative" || return
  if [[ ! -f "$delegate" ]]; then
    fail "The checked-in Tauri DMG builder is missing or not a regular file"
    return
  fi
  if [[ ! -x "$delegate" ]]; then
    fail "The checked-in Tauri DMG builder is not executable"
    return
  fi
  printf '%s\n' "$delegate"
}

main() {
  local requested_arch=''
  local target
  local repo_root
  local delegate

  if [[ $# -eq 1 && ( "$1" == -h || "$1" == --help ) ]]; then
    usage
    return 0
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --arch)
        if [[ -n "$requested_arch" ]]; then
          fail 'Only one --arch selection is allowed'
          return
        fi
        if [[ $# -lt 2 ]]; then
          fail 'Missing value for --arch'
          return
        fi
        if [[ -z "$2" ]]; then
          fail 'Empty value for --arch'
          return
        fi
        requested_arch="$2"
        shift 2
        ;;
      --binary|--version)
        fail "$1 is retired; the reviewed Tauri bundle owns helper and version provenance"
        return
        ;;
      -h|--help)
        fail "$1 must be used by itself"
        return
        ;;
      --*)
        fail "Unknown option: $1"
        return
        ;;
      *)
        fail "Unexpected positional argument: $1"
        return
        ;;
    esac
  done

  if [[ -z "$requested_arch" ]]; then
    requested_arch="$(uname -m)"
  fi
  target="$(target_for_arch "$requested_arch")" || return

  repo_root="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)" || return
  delegate="$(resolve_delegate "$repo_root")" || return
  exec "$delegate" "$target"
}

main "$@"
