#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: build-macos-dmg.sh <aarch64-apple-darwin|x86_64-apple-darwin>

Build one unsigned, architecture-specific Citadel DMG with Tauri. Signing,
notarization, and publishing are separate release gates.
EOF
}

target_arch() {
  case "${1:-}" in
    aarch64-apple-darwin) printf '%s\n' arm64 ;;
    x86_64-apple-darwin) printf '%s\n' x86_64 ;;
    *)
      echo "Unsupported macOS target: ${1:-<missing>}" >&2
      return 2
      ;;
  esac
}

validate_macho_arch() {
  local path="$1"
  local expected_arch="$2"
  local label="$3"
  local arch_output
  local -a arches

  if [[ ! -f "$path" || ! -x "$path" ]]; then
    echo "$label is missing or not executable: $path" >&2
    return 1
  fi
  if ! arch_output="$(lipo -archs "$path")"; then
    echo "Could not inspect $label architecture: $path" >&2
    return 1
  fi
  read -r -a arches <<< "$arch_output" || true
  if [[ ${#arches[@]} -ne 1 || "${arches[0]}" != "$expected_arch" ]]; then
    echo "$label must contain only $expected_arch, found: ${arch_output:-<none>}" >&2
    return 1
  fi
}

validate_app_bundle() {
  local app_bundle="$1"
  local target="$2"
  local expected_arch
  expected_arch="$(target_arch "$target")" || return

  if [[ ! -d "$app_bundle" ]]; then
    echo "Tauri app bundle is missing: $app_bundle" >&2
    return 1
  fi
  if [[ ! -f "$app_bundle/Contents/Info.plist" ]]; then
    echo "Tauri app bundle has no Info.plist: $app_bundle" >&2
    return 1
  fi
  validate_macho_arch \
    "$app_bundle/Contents/MacOS/citadel-desktop" \
    "$expected_arch" \
    "Citadel desktop executable" || return
  validate_macho_arch \
    "$app_bundle/Contents/MacOS/citadel" \
    "$expected_arch" \
    "Bundled Citadel helper" || return
}

single_dmg() {
  local dmg_dir="$1"
  local -a dmgs=()
  shopt -s nullglob
  dmgs=("$dmg_dir"/*.dmg)
  shopt -u nullglob

  if [[ ${#dmgs[@]} -ne 1 ]]; then
    echo "Expected exactly one Tauri DMG in $dmg_dir, found ${#dmgs[@]}" >&2
    return 1
  fi
  if [[ ! -s "${dmgs[0]}" ]]; then
    echo "Tauri DMG is empty: ${dmgs[0]}" >&2
    return 1
  fi
  printf '%s\n' "${dmgs[0]}"
}

host_os() {
  uname -s
}

stage_sidecar() {
  local repo_root="$1"
  local target="$2"
  "$repo_root/desktop/scripts/stage-sidecar.sh" "$target"
}

run_tauri_build() {
  local repo_root="$1"
  local target="$2"
  (
    cd "$repo_root/desktop"
    npm run tauri -- build --target "$target" --bundles dmg --no-sign --ci
  )
}

reject_symlink_components() {
  local repo_root="$1"
  local relative_path="$2"
  local current="$repo_root"
  local component
  local -a components

  IFS=/ read -r -a components <<< "$relative_path"
  for component in "${components[@]}"; do
    current="$current/$component"
    if [[ -L "$current" ]]; then
      echo "Refusing symlinked build path: $current" >&2
      return 1
    fi
  done
}

validate_build_paths() {
  local repo_root="$1"
  local target="$2"
  local physical_root

  target_arch "$target" >/dev/null || return
  if [[ "$repo_root" != /* || ! -d "$repo_root" || -L "$repo_root" ]]; then
    echo "Repository root must be an existing physical absolute directory: $repo_root" >&2
    return 1
  fi
  physical_root="$(cd -P "$repo_root" && pwd -P)" || return
  if [[ "$physical_root" != "$repo_root" ]]; then
    echo "Repository root contains a symlink: $repo_root" >&2
    return 1
  fi

  reject_symlink_components "$repo_root" \
    "desktop/scripts/stage-sidecar.sh" || return
  reject_symlink_components "$repo_root" \
    "desktop/src-tauri/binaries/citadel-$target" || return
  reject_symlink_components "$repo_root" \
    "desktop/src-tauri/target/$target/release/bundle/macos/Citadel.app" || return
  reject_symlink_components "$repo_root" \
    "desktop/src-tauri/target/$target/release/bundle/dmg" || return
}

reset_bundle_output() {
  local repo_root="$1"
  local target="$2"
  local bundle_root="$repo_root/desktop/src-tauri/target/$target/release/bundle"

  target_arch "$target" >/dev/null || return
  validate_build_paths "$repo_root" "$target" || return

  # These are Tauri's derived outputs for the already validated target. Remove
  # them so a successful command can never report a stale app or DMG.
  rm -rf "$bundle_root/macos/Citadel.app" "$bundle_root/dmg"
}

build_macos_dmg() {
  local repo_root="$1"
  local target="$2"
  local expected_arch
  local target_root
  local app_bundle
  local dmg_path

  expected_arch="$(target_arch "$target")" || return
  target_root="$repo_root/desktop/src-tauri/target/$target/release/bundle"
  if [[ "$(host_os)" != Darwin ]]; then
    echo 'Citadel DMGs must be built on macOS' >&2
    return 1
  fi
  if ! command -v lipo >/dev/null 2>&1; then
    echo 'lipo is required to validate macOS bundle architecture' >&2
    return 1
  fi
  if ! command -v npm >/dev/null 2>&1; then
    echo 'npm is required to run the Tauri build' >&2
    return 1
  fi
  validate_build_paths "$repo_root" "$target" || return

  stage_sidecar "$repo_root" "$target" || return
  validate_macho_arch \
    "$repo_root/desktop/src-tauri/binaries/citadel-$target" \
    "$expected_arch" \
    "Staged Citadel helper" || return

  reset_bundle_output "$repo_root" "$target" || return
  run_tauri_build "$repo_root" "$target" || return

  app_bundle="$target_root/macos/Citadel.app"
  validate_app_bundle "$app_bundle" "$target" || return
  dmg_path="$(single_dmg "$target_root/dmg")" || return

  printf 'Unsigned Tauri DMG: %s\n' "$dmg_path"
  printf 'Required distribution gates: Developer ID signing and Apple notarization\n'
}

main() {
  if [[ $# -ne 1 ]]; then
    usage
    return 2
  fi
  local repo_root
  repo_root="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
  build_macos_dmg "$repo_root" "$1"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
