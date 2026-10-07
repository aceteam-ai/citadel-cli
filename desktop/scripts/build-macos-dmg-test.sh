#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=build-macos-dmg.sh
source "$script_dir/build-macos-dmg.sh"

fixture_root="$(mktemp -d "${TMPDIR:-/tmp}/citadel-dmg-test.XXXXXXXX")"
fixture_root="$(cd -P "$fixture_root" && pwd -P)"
trap 'rm -rf "$fixture_root"' EXIT

tests_run=0

assert_fails() {
  local message="$1"
  shift
  tests_run=$((tests_run + 1))
  if "$@" >/dev/null 2>&1; then
    echo "Expected failure: $message" >&2
    exit 1
  fi
}

assert_equals() {
  local expected="$1"
  local actual="$2"
  local message="$3"
  tests_run=$((tests_run + 1))
  if [[ "$expected" != "$actual" ]]; then
    echo "$message: expected '$expected', got '$actual'" >&2
    exit 1
  fi
}

make_executable() {
  local path="$1"
  local arch="$2"
  mkdir -p "$(dirname "$path")"
  printf 'arch=%s\n' "$arch" > "$path"
  chmod 755 "$path"
}

make_bundle() {
  local root="$1"
  local main_arch="$2"
  local helper_arch="$3"
  mkdir -p "$root/Contents/MacOS"
  printf '<plist/>\n' > "$root/Contents/Info.plist"
  make_executable "$root/Contents/MacOS/citadel-desktop" "$main_arch"
  make_executable "$root/Contents/MacOS/citadel" "$helper_arch"
}

lipo() {
  if [[ "$1" != -archs || ! -f "$2" ]]; then
    return 1
  fi
  sed -n 's/^arch=//p' "$2"
}

assert_equals arm64 "$(target_arch aarch64-apple-darwin)" 'Apple Silicon target mapping'
assert_equals x86_64 "$(target_arch x86_64-apple-darwin)" 'Intel target mapping'
assert_fails 'unsupported targets are rejected' target_arch universal-apple-darwin

arm_app="$fixture_root/arm/Citadel.app"
make_bundle "$arm_app" arm64 arm64
validate_app_bundle "$arm_app" aarch64-apple-darwin
tests_run=$((tests_run + 1))

intel_app="$fixture_root/intel/Citadel.app"
make_bundle "$intel_app" x86_64 x86_64
validate_app_bundle "$intel_app" x86_64-apple-darwin
tests_run=$((tests_run + 1))

wrong_main="$fixture_root/wrong-main/Citadel.app"
make_bundle "$wrong_main" x86_64 arm64
assert_fails 'wrong main executable architecture is rejected' \
  validate_app_bundle "$wrong_main" aarch64-apple-darwin

universal_helper="$fixture_root/universal-helper/Citadel.app"
make_bundle "$universal_helper" arm64 'arm64 x86_64'
assert_fails 'universal helper is rejected' \
  validate_app_bundle "$universal_helper" aarch64-apple-darwin

missing_helper="$fixture_root/missing-helper/Citadel.app"
make_bundle "$missing_helper" arm64 arm64
rm "$missing_helper/Contents/MacOS/citadel"
assert_fails 'missing helper is rejected' \
  validate_app_bundle "$missing_helper" aarch64-apple-darwin

dmg_dir="$fixture_root/dmgs"
mkdir -p "$dmg_dir"
assert_fails 'missing DMG is rejected' single_dmg "$dmg_dir"
: > "$dmg_dir/Citadel_empty.dmg"
assert_fails 'empty DMG is rejected' single_dmg "$dmg_dir"
rm "$dmg_dir/Citadel_empty.dmg"
printf 'dmg\n' > "$dmg_dir/Citadel_arm64.dmg"
assert_equals "$dmg_dir/Citadel_arm64.dmg" "$(single_dmg "$dmg_dir")" 'single DMG selection'
printf 'dmg\n' > "$dmg_dir/Citadel_other.dmg"
assert_fails 'ambiguous DMG output is rejected' single_dmg "$dmg_dir"

fake_repo="$fixture_root/repo"
target=aarch64-apple-darwin
target_root="$fake_repo/desktop/src-tauri/target/$target/release/bundle"
mkdir -p "$fake_repo/desktop/src-tauri/binaries" "$target_root/dmg"
printf 'stale\n' > "$target_root/dmg/stale.dmg"

host_os() { printf '%s\n' Darwin; }
stage_sidecar() {
  make_executable "$1/desktop/src-tauri/binaries/citadel-$2" arm64
}
run_tauri_build() {
  make_bundle "$1/desktop/src-tauri/target/$2/release/bundle/macos/Citadel.app" arm64 arm64
  mkdir -p "$1/desktop/src-tauri/target/$2/release/bundle/dmg"
  printf 'dmg\n' > "$1/desktop/src-tauri/target/$2/release/bundle/dmg/Citadel_0.1.0_aarch64.dmg"
}
command() { return 0; }

build_output="$(build_macos_dmg "$fake_repo" "$target")"
assert_equals \
  "Unsigned Tauri DMG: $target_root/dmg/Citadel_0.1.0_aarch64.dmg
Required distribution gates: Developer ID signing and Apple notarization" \
  "$build_output" \
  'entrypoint reports the exact unsigned artifact and remaining gates'

stage_sidecar() {
  make_executable "$1/desktop/src-tauri/binaries/citadel-$2" x86_64
}
run_tauri_build() {
  : > "$fixture_root/tauri-called"
}
assert_fails 'wrong staged sidecar architecture stops before Tauri' \
  build_macos_dmg "$fake_repo" "$target"
assert_fails 'Tauri was not called after sidecar validation failed' \
  test -e "$fixture_root/tauri-called"

symlink_repo="$fixture_root/symlink-repo"
escape_dir="$fixture_root/escape"
symlink_parent="$symlink_repo/desktop/src-tauri/target/$target/release"
mkdir -p "$symlink_repo/desktop/scripts" \
  "$symlink_repo/desktop/src-tauri/binaries" \
  "$symlink_parent" \
  "$escape_dir/macos/Citadel.app" \
  "$escape_dir/dmg"
printf '#!/usr/bin/env bash\n' > "$symlink_repo/desktop/scripts/stage-sidecar.sh"
printf 'keep\n' > "$escape_dir/dmg/outside.dmg"
ln -s "$escape_dir" "$symlink_parent/bundle"
assert_fails 'symlinked output parent is rejected' \
  reset_bundle_output "$symlink_repo" "$target"
assert_equals keep "$(sed -n '1p' "$escape_dir/dmg/outside.dmg")" \
  'symlink rejection preserves outside artifacts'

: > "$fixture_root/stage-called"
stage_sidecar() {
  : > "$fixture_root/stage-called"
}
run_tauri_build() {
  : > "$fixture_root/tauri-called"
}
rm -f "$fixture_root/stage-called" "$fixture_root/tauri-called"
assert_fails 'output-parent symlink fails before staging' \
  build_macos_dmg "$symlink_repo" "$target"
assert_fails 'stage was not called for an escaping output path' \
  test -e "$fixture_root/stage-called"
assert_fails 'Tauri was not called for an escaping output path' \
  test -e "$fixture_root/tauri-called"

sidecar_link_repo="$fixture_root/sidecar-link-repo"
sidecar_escape="$fixture_root/sidecar-escape"
mkdir -p "$sidecar_link_repo/desktop/scripts" \
  "$sidecar_link_repo/desktop/src-tauri" \
  "$sidecar_escape"
printf '#!/usr/bin/env bash\n' > "$sidecar_link_repo/desktop/scripts/stage-sidecar.sh"
printf 'keep\n' > "$sidecar_escape/outside-helper"
ln -s "$sidecar_escape" "$sidecar_link_repo/desktop/src-tauri/binaries"
rm -f "$fixture_root/stage-called" "$fixture_root/tauri-called"
assert_fails 'sidecar-parent symlink fails before staging' \
  build_macos_dmg "$sidecar_link_repo" "$target"
assert_fails 'stage was not called for an escaping sidecar path' \
  test -e "$fixture_root/stage-called"
assert_fails 'Tauri was not called for an escaping sidecar path' \
  test -e "$fixture_root/tauri-called"
assert_equals keep "$(sed -n '1p' "$sidecar_escape/outside-helper")" \
  'sidecar symlink rejection preserves outside artifacts'

rm "$sidecar_link_repo/desktop/src-tauri/binaries"
mkdir -p "$sidecar_link_repo/desktop/src-tauri/binaries"
ln -s "$sidecar_escape/outside-helper" \
  "$sidecar_link_repo/desktop/src-tauri/binaries/citadel-$target"
rm -f "$fixture_root/stage-called" "$fixture_root/tauri-called"
assert_fails 'final sidecar symlink fails before staging' \
  build_macos_dmg "$sidecar_link_repo" "$target"
assert_fails 'stage was not called for a symlinked final sidecar' \
  test -e "$fixture_root/stage-called"
assert_fails 'Tauri was not called for a symlinked final sidecar' \
  test -e "$fixture_root/tauri-called"
assert_equals keep "$(sed -n '1p' "$sidecar_escape/outside-helper")" \
  'final sidecar symlink rejection preserves outside artifact'

assert_fails 'reset rejects unsupported target independently' \
  reset_bundle_output "$fake_repo" universal-apple-darwin

echo "macOS DMG entrypoint tests passed ($tests_run assertions)"
