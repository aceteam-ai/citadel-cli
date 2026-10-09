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

assert_absent() {
  local path="$1"
  local message="$2"
  tests_run=$((tests_run + 1))
  if [[ -e "$path" || -L "$path" ]]; then
    echo "$message: unexpected path exists: $path" >&2
    exit 1
  fi
}

assert_contains() {
  local value="$1"
  local expected="$2"
  local message="$3"
  tests_run=$((tests_run + 1))
  if [[ "$value" != *"$expected"* ]]; then
    echo "$message: missing '$expected'" >&2
    exit 1
  fi
}

assert_status() {
  local expected="$1"
  local message="$2"
  shift 2
  local actual
  tests_run=$((tests_run + 1))
  set +e
  "$@" >/dev/null 2>&1
  actual=$?
  set -e
  if [[ "$actual" -ne "$expected" ]]; then
    echo "$message: expected status $expected, got $actual" >&2
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

repo_root="$(cd -P "$script_dir/../.." && pwd -P)"
dispatcher_source="$repo_root/build-dmg.sh"
dispatcher_root="$fixture_root/dispatcher"
dispatcher="$dispatcher_root/build-dmg.sh"
delegate="$dispatcher_root/desktop/scripts/build-macos-dmg.sh"
record="$dispatcher_root/delegate.record"

make_dispatcher_root() {
  local root="$1"
  mkdir -p "$root"
  cp "$dispatcher_source" "$root/build-dmg.sh"
  chmod 755 "$root/build-dmg.sh"
}

make_recording_delegate() {
  local path="$1"
  mkdir -p "$(dirname "$path")"
  printf '%s\n' \
    '#!/usr/bin/env bash' \
    'printf "%s\n" "$@" >> "${RECORD_FILE:?}"' \
    'exit "${DELEGATE_EXIT_CODE:-0}"' > "$path"
  chmod 755 "$path"
}

assert_dispatches() {
  local arch="$1"
  local expected="$2"
  rm -f "$record"
  RECORD_FILE="$record" "$dispatcher" --arch "$arch"
  assert_equals "$expected" "$(sed -n '1p' "$record")" \
    "dispatcher maps $arch"
  assert_equals 1 "$(wc -l < "$record" | tr -d ' ')" \
    "dispatcher invokes the delegate exactly once for $arch"
}

assert_refused_without_delegate() {
  local message="$1"
  shift
  rm -f "$record"
  assert_status 2 "$message" env RECORD_FILE="$record" "$dispatcher" "$@"
  assert_absent "$record" "$message does not invoke the delegate"
}

make_dispatcher_root "$dispatcher_root"
make_recording_delegate "$delegate"

assert_dispatches arm64 aarch64-apple-darwin
assert_dispatches aarch64 aarch64-apple-darwin
assert_dispatches aarch64-apple-darwin aarch64-apple-darwin
assert_dispatches amd64 x86_64-apple-darwin
assert_dispatches x86_64 x86_64-apple-darwin
assert_dispatches x86_64-apple-darwin x86_64-apple-darwin

fake_bin="$fixture_root/fake-bin"
mkdir -p "$fake_bin"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'if [[ "$1" != -m ]]; then exit 64; fi' \
  'printf "%s\n" "${FAKE_UNAME_ARCH:?}"' > "$fake_bin/uname"
chmod 755 "$fake_bin/uname"

rm -f "$record"
PATH="$fake_bin:$PATH" FAKE_UNAME_ARCH=arm64 RECORD_FILE="$record" "$dispatcher"
assert_equals aarch64-apple-darwin "$(sed -n '1p' "$record")" \
  'no-argument dispatcher maps an Apple Silicon host'
rm -f "$record"
PATH="$fake_bin:$PATH" FAKE_UNAME_ARCH=x86_64 RECORD_FILE="$record" "$dispatcher"
assert_equals x86_64-apple-darwin "$(sed -n '1p' "$record")" \
  'no-argument dispatcher maps an Intel host'

rm -f "$record"
help_output="$(RECORD_FILE="$record" "$dispatcher" --help)"
assert_contains "$help_output" 'unsigned, architecture-specific Citadel Tauri DMG' \
  'help identifies the unsigned Tauri artifact'
assert_contains "$help_output" 'The legacy --binary and --version options are retired' \
  'help explains retired provenance overrides'
assert_contains "$help_output" 'Developer ID signing, Apple notarization, stapling' \
  'help preserves distribution gates'
assert_contains "$help_output" 'native installer' \
  'help preserves native acceptance gate'
assert_contains "$help_output" 'publishing remain separate distribution gates' \
  'help preserves publishing gate'
assert_absent "$record" 'help does not invoke the delegate'

assert_refused_without_delegate 'legacy version injection is rejected' --version v9.9.9
assert_refused_without_delegate 'legacy binary injection is rejected' --binary /tmp/citadel
assert_refused_without_delegate 'missing architecture value is rejected' --arch
assert_refused_without_delegate 'empty architecture value is rejected' --arch ''
assert_refused_without_delegate 'duplicate architecture selection is rejected' \
  --arch arm64 --arch amd64
assert_refused_without_delegate 'unsupported architecture is rejected' --arch universal
assert_refused_without_delegate 'unknown option is rejected' --unknown
assert_refused_without_delegate 'equals-form option is rejected' --arch=arm64
assert_refused_without_delegate 'positional argument is rejected' arm64
assert_refused_without_delegate 'help combined with another argument is rejected' --help extra
assert_refused_without_delegate 'architecture plus positional extra is rejected' \
  --arch arm64 extra

rm -f "$record"
assert_status 73 'delegate exit status is preserved' env \
  RECORD_FILE="$record" DELEGATE_EXIT_CODE=73 "$dispatcher" --arch arm64
assert_equals aarch64-apple-darwin "$(sed -n '1p' "$record")" \
  'failing delegate still receives only the canonical target'

missing_root="$fixture_root/missing-delegate"
make_dispatcher_root "$missing_root"
assert_status 2 'missing exact delegate is rejected' \
  "$missing_root/build-dmg.sh" --arch arm64

directory_root="$fixture_root/directory-delegate"
make_dispatcher_root "$directory_root"
mkdir -p "$directory_root/desktop/scripts/build-macos-dmg.sh"
assert_status 2 'directory delegate is rejected' \
  "$directory_root/build-dmg.sh" --arch arm64

nonexec_root="$fixture_root/nonexec-delegate"
make_dispatcher_root "$nonexec_root"
mkdir -p "$nonexec_root/desktop/scripts"
printf '#!/usr/bin/env bash\nexit 0\n' > \
  "$nonexec_root/desktop/scripts/build-macos-dmg.sh"
chmod 644 "$nonexec_root/desktop/scripts/build-macos-dmg.sh"
assert_status 2 'non-executable delegate is rejected' \
  "$nonexec_root/build-dmg.sh" --arch arm64

outside_delegate="$fixture_root/outside-delegate"
outside_record="$fixture_root/outside.record"
make_recording_delegate "$outside_delegate"

final_link_root="$fixture_root/final-link-delegate"
make_dispatcher_root "$final_link_root"
mkdir -p "$final_link_root/desktop/scripts"
ln -s "$outside_delegate" \
  "$final_link_root/desktop/scripts/build-macos-dmg.sh"
assert_status 2 'symlinked final delegate is rejected' env \
  RECORD_FILE="$outside_record" "$final_link_root/build-dmg.sh" --arch arm64
assert_absent "$outside_record" 'symlinked final delegate is not invoked'

ancestor_target="$fixture_root/ancestor-target"
make_recording_delegate "$ancestor_target/scripts/build-macos-dmg.sh"
ancestor_link_root="$fixture_root/ancestor-link-delegate"
make_dispatcher_root "$ancestor_link_root"
ln -s "$ancestor_target" "$ancestor_link_root/desktop"
assert_status 2 'symlinked desktop ancestor is rejected' env \
  RECORD_FILE="$outside_record" "$ancestor_link_root/build-dmg.sh" --arch arm64
assert_absent "$outside_record" 'delegate below symlinked desktop is not invoked'

scripts_target="$fixture_root/scripts-target"
make_recording_delegate "$scripts_target/build-macos-dmg.sh"
scripts_link_root="$fixture_root/scripts-link-delegate"
make_dispatcher_root "$scripts_link_root"
mkdir -p "$scripts_link_root/desktop"
ln -s "$scripts_target" "$scripts_link_root/desktop/scripts"
assert_status 2 'symlinked scripts ancestor is rejected' env \
  RECORD_FILE="$outside_record" "$scripts_link_root/build-dmg.sh" --arch arm64
assert_absent "$outside_record" 'delegate below symlinked scripts is not invoked'

wrong_path_root="$fixture_root/wrong-path-delegate"
make_dispatcher_root "$wrong_path_root"
path_bin="$fixture_root/path-bin"
path_record="$fixture_root/path.record"
make_recording_delegate "$path_bin/build-macos-dmg.sh"
assert_status 2 'PATH delegate is not searched' env \
  PATH="$path_bin:$PATH" RECORD_FILE="$path_record" \
  "$wrong_path_root/build-dmg.sh" --arch arm64
assert_absent "$path_record" 'same-named PATH delegate is not invoked'

override_root="$fixture_root/environment-override"
make_dispatcher_root "$override_root"
override_record="$fixture_root/override.record"
expected_record="$fixture_root/expected.record"
make_recording_delegate "$override_root/desktop/scripts/build-macos-dmg.sh"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  "printf 'called\\n' > '$override_record'" > "$outside_delegate"
chmod 755 "$outside_delegate"
assert_status 0 'environment override cannot replace checked-in delegate' env \
  CITADEL_DMG_BUILDER="$outside_delegate" RECORD_FILE="$expected_record" \
  "$override_root/build-dmg.sh" --arch arm64
assert_equals aarch64-apple-darwin "$(sed -n '1p' "$expected_record")" \
  'checked-in delegate wins over environment override'
assert_absent "$override_record" 'environment-selected delegate is not invoked'

assert_absent "$repo_root/packaging/macos/Info.plist" \
  'legacy Info.plist is removed'
assert_absent "$repo_root/packaging/macos/citadel-launcher" \
  'legacy Terminal launcher is removed'

tests_run=$((tests_run + 1))
if grep -Eq \
  'citadel-launcher|packaging/macos|hdiutil|go[[:space:]]+build|Citadel\.app|CFBundleExecutable|codesign|notarytool|gh[[:space:]]+release' \
  "$dispatcher_source"; then
  echo 'Root dispatcher regained independent packaging, compilation, signing, or publishing logic' >&2
  exit 1
fi

readme="$(cat "$repo_root/desktop/README.md")"
assert_contains "$readme" \
  'From the repository root, `./build-dmg.sh` is a compatibility dispatcher to that exact Tauri builder.' \
  'README names the single desktop packaging path'
assert_contains "$readme" \
  'The retired `--binary` and `--version` options fail closed' \
  'README records the provenance compatibility decision'
assert_contains "$readme" \
  'There is no legacy Terminal-launcher packaging path.' \
  'README records legacy path retirement'
assert_contains "$readme" \
  'The repository-root `build-dmg.sh` builds only through the reviewed Tauri entrypoint.' \
  'README headline rejects a second builder'

echo "macOS DMG entrypoint tests passed ($tests_run assertions)"
