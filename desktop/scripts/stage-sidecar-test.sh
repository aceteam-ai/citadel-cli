#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source_script="$script_dir/stage-sidecar.sh"
fixture_root="$(mktemp -d "${TMPDIR:-/tmp}/citadel-sidecar-test.XXXXXXXX")"
fixture_root="$(cd -P "$fixture_root" && pwd -P)"
trap 'rm -rf "$fixture_root"' EXIT

real_chmod="$(command -v chmod)"
shim_dir="$fixture_root/shims"
mkdir -p "$shim_dir"
tests_run=0
case_number=0

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

# These tools only record construction and create inert text. No compiler,
# repository query, Rust toolchain or Tauri/Mac operation is invoked.
cat > "$shim_dir/go" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
record="${FIXTURE_RECORD:?}"
printf 'called\n' >> "$record/go.calls"
printf '%s\n' "$PWD" > "$record/go.cwd"
printf '%s\n' "$#" > "$record/go.argc"
printf '%s\n' "$@" > "$record/go.argv"
printf 'CGO_ENABLED=%s\nGOOS=%s\nGOARCH=%s\n' \
  "${CGO_ENABLED-}" "${GOOS-}" "${GOARCH-}" > "$record/go.env"
output=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == -o ]]; then
    shift
    output="${1-}"
    break
  fi
  shift
done
if [[ -n "$output" ]]; then
  umask 077
  printf 'inert helper data\n' > "$output"
fi
exit "${FAKE_GO_EXIT:-0}"
SHIM

cat > "$shim_dir/git" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
record="${FIXTURE_RECORD:?}"
printf 'called\n' >> "$record/git.calls"
printf '%s\n' "$PWD" > "$record/git.cwd"
printf '%s\n' "$#" > "$record/git.argc"
printf '%s\n' "$@" > "$record/git.argv"
if [[ "${FAKE_GIT_EXIT:-0}" -ne 0 ]]; then
  exit "$FAKE_GIT_EXIT"
fi
printf '%s\n' "${FAKE_GIT_VERSION-v9.8.7-6-gabcdef0}"
SHIM

cat > "$shim_dir/rustc" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
record="${FIXTURE_RECORD:?}"
printf 'called\n' >> "$record/rustc.calls"
printf '%s\n' "$#" > "$record/rustc.argc"
printf '%s\n' "$@" > "$record/rustc.argv"
if [[ "${FAKE_RUSTC_EXIT:-0}" -ne 0 ]]; then
  exit "$FAKE_RUSTC_EXIT"
fi
printf '%s\n' "${FAKE_RUST_TARGET-aarch64-apple-darwin}"
SHIM

cat > "$shim_dir/chmod" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
record="${FIXTURE_RECORD:?}"
printf 'called\n' >> "$record/chmod.calls"
printf '%s\n' "$#" > "$record/chmod.argc"
printf '%s\n' "$@" > "$record/chmod.argv"
if [[ "${FAKE_CHMOD_EXIT:-0}" -ne 0 ]]; then
  exit "$FAKE_CHMOD_EXIT"
fi
"${REAL_CHMOD:?}" "$@"
SHIM

"$real_chmod" 755 "$shim_dir/go" "$shim_dir/git" "$shim_dir/rustc" "$shim_dir/chmod"

prepare_case() {
  case_number=$((case_number + 1))
  case_dir="$fixture_root/case-$case_number"
  fake_repo="$case_dir/repo with space"
  record="$case_dir/record"
  mkdir -p "$fake_repo/desktop/scripts" "$record"
  cp "$source_script" "$fake_repo/desktop/scripts/stage-sidecar.sh"
  fake_go_exit=0
  fake_git_exit=0
  fake_rustc_exit=0
  fake_chmod_exit=0
  fake_git_version=v9.8.7-6-gabcdef0
  fake_rust_target=aarch64-apple-darwin
}

run_stage() {
  local expected="$1"
  shift
  local actual
  if env PATH="$shim_dir:$PATH" FIXTURE_RECORD="$record" REAL_CHMOD="$real_chmod" \
    CGO_ENABLED=1 GOOS=wrong GOARCH=wrong \
    CITADEL_SIDECAR_OUTPUT="$case_dir/forbidden-helper" \
    FAKE_GO_EXIT="$fake_go_exit" FAKE_GIT_EXIT="$fake_git_exit" \
    FAKE_GIT_VERSION="$fake_git_version" FAKE_RUSTC_EXIT="$fake_rustc_exit" \
    FAKE_RUST_TARGET="$fake_rust_target" FAKE_CHMOD_EXIT="$fake_chmod_exit" \
    "$fake_repo/desktop/scripts/stage-sidecar.sh" "$@" \
    > "$case_dir/stdout" 2> "$case_dir/stderr"; then
    actual=0
  else
    actual=$?
  fi
  assert_equals "$expected" "$actual" 'stage exit status'
}

assert_no_build() {
  assert_absent "$record/go.calls" 'no Go build was invoked'
  assert_absent "$record/chmod.calls" 'no helper chmod was invoked'
  assert_absent "$fake_repo/desktop/src-tauri" 'no output namespace was created'
  assert_absent "$case_dir/forbidden-helper" 'no output override was used'
  assert_equals '' "$(cat "$case_dir/stdout")" 'failure reports no success'
}

assert_success() {
  local target="$1"
  local go_os="$2"
  local go_arch="$3"
  local suffix="$4"
  local output="$fake_repo/desktop/src-tauri/binaries/citadel-$target$suffix"
  assert_equals called "$(cat "$record/go.calls")" 'one Go invocation'
  assert_equals "$fake_repo" "$(cat "$record/go.cwd")" 'Go build uses repository cwd'
  assert_equals 6 "$(cat "$record/go.argc")" 'Go argv retains one ldflags argument'
  assert_equals "$(printf '%s\n' build -buildvcs=false \
    "-ldflags=-X github.com/aceteam-ai/citadel-cli/cmd.Version=$fake_git_version" \
    -o "$output" ./cmd/citadel)" \
    "$(cat "$record/go.argv")" 'exact Go build argv and fixed output'
  assert_equals "$(printf 'CGO_ENABLED=0\nGOOS=%s\nGOARCH=%s\n' "$go_os" "$go_arch")" \
    "$(cat "$record/go.env")" 'explicit environment overrides inherited cross-build values'
  assert_equals called "$(cat "$record/git.calls")" 'one provenance query'
  assert_equals "$fake_repo" "$(cat "$record/git.cwd")" 'Git query uses repository cwd'
  assert_equals 3 "$(cat "$record/git.argc")" 'Git describe argc'
  assert_equals "$(printf '%s\n' describe --tags --always)" \
    "$(cat "$record/git.argv")" 'checked provenance command'
  assert_equals called "$(cat "$record/chmod.calls")" 'one helper chmod'
  assert_equals 2 "$(cat "$record/chmod.argc")" 'chmod argc'
  assert_equals "$(printf '%s\n' 755 "$output")" \
    "$(cat "$record/chmod.argv")" 'only the fixed helper is made executable'
  tests_run=$((tests_run + 1))
  if [[ ! -x "$output" ]]; then
    echo 'Successful staging did not make the fake helper executable' >&2
    exit 1
  fi
  assert_equals 'inert helper data' "$(cat "$output")" 'output comes from inert Go shim'
  assert_absent "$output.exe" 'no extra executable suffix is appended'
  if [[ -n "$suffix" ]]; then
    assert_absent "$fake_repo/desktop/src-tauri/binaries/citadel-$target" \
      'Windows helper has no suffixless twin'
  fi
  assert_absent "$case_dir/forbidden-helper" 'environment cannot redirect helper output'
  assert_equals "Staged bundled Citadel helper for $target" \
    "$(cat "$case_dir/stdout")" 'success is announced after build and chmod'
}

for target in aarch64-apple-darwin x86_64-apple-darwin \
  x86_64-unknown-linux-gnu aarch64-unknown-linux-gnu x86_64-pc-windows-msvc; do
  expected_suffix=''
  case "$target" in
    aarch64-apple-darwin) expected_os=darwin; expected_arch=arm64 ;;
    x86_64-apple-darwin) expected_os=darwin; expected_arch=amd64 ;;
    x86_64-unknown-linux-gnu) expected_os=linux; expected_arch=amd64 ;;
    aarch64-unknown-linux-gnu) expected_os=linux; expected_arch=arm64 ;;
    x86_64-pc-windows-msvc) expected_os=windows; expected_arch=amd64; expected_suffix=.exe ;;
  esac
  prepare_case
  run_stage 0 "$target"
  assert_success "$target" "$expected_os" "$expected_arch" "$expected_suffix"
  assert_absent "$record/rustc.calls" 'explicit target does not probe Rust host'

  prepare_case
  fake_rust_target="$target"
  run_stage 0
  assert_success "$target" "$expected_os" "$expected_arch" "$expected_suffix"
  assert_equals called "$(cat "$record/rustc.calls")" 'default target probes Rust once'
  assert_equals 2 "$(cat "$record/rustc.argc")" 'Rust host probe argc'
  assert_equals "$(printf '%s\n' --print host-tuple)" \
    "$(cat "$record/rustc.argv")" 'Rust host probe argv'
done

for target in '' x86_64-unknown-linux-musl x86_64-pc-windows-gnu \
  aarch64-pc-windows-msvc i686-pc-windows-msvc \
  universal-apple-darwin amd64 --output '../escape'; do
  prepare_case
  run_stage 2 "$target"
  assert_no_build
  assert_absent "$record/rustc.calls" 'invalid explicit target does not fall back to Rust'
  assert_absent "$record/git.calls" 'invalid target refuses before provenance query'
done

prepare_case
run_stage 2 aarch64-unknown-linux-gnu extra
assert_no_build
assert_absent "$record/rustc.calls" 'extra args refuse before host lookup'
assert_absent "$record/git.calls" 'extra args refuse before provenance query'

prepare_case
run_stage 2 '' x86_64-apple-darwin
assert_no_build
assert_absent "$record/rustc.calls" 'empty plus extra args do not become a host default'
assert_absent "$record/git.calls" 'empty plus extra args refuse before provenance query'

for mode in failed empty unsupported; do
  prepare_case
  expected=2
  case "$mode" in
    failed) fake_rustc_exit=17; expected=1 ;;
    empty) fake_rust_target='' ;;
    unsupported) fake_rust_target=x86_64-unknown-linux-musl ;;
  esac
  run_stage "$expected"
  assert_no_build
  assert_equals called "$(cat "$record/rustc.calls")" 'host refusal uses the Rust probe'
  assert_absent "$record/git.calls" 'invalid host refuses before Git/build'
done

for mode in failed empty; do
  prepare_case
  case "$mode" in
    failed) fake_git_exit=19 ;;
    empty) fake_git_version='' ;;
  esac
  run_stage 1 aarch64-unknown-linux-gnu
  assert_no_build
  assert_equals called "$(cat "$record/git.calls")" 'provenance failure was exercised'
  assert_absent "$record/rustc.calls" 'explicit target needs no Rust despite Git failure'
done

prepare_case
fake_go_exit=23
run_stage 23 x86_64-unknown-linux-gnu
assert_equals called "$(cat "$record/go.calls")" 'build failure was exercised'
assert_absent "$record/chmod.calls" 'failed build is never made executable'
assert_equals '' "$(cat "$case_dir/stdout")" 'failed build is not announced as staged'
tests_run=$((tests_run + 1))
if [[ -x "$fake_repo/desktop/src-tauri/binaries/citadel-x86_64-unknown-linux-gnu" ]]; then
  echo 'Failed build left an executable fake output' >&2
  exit 1
fi

prepare_case
fake_chmod_exit=31
run_stage 31 aarch64-unknown-linux-gnu
assert_equals called "$(cat "$record/go.calls")" 'chmod failure follows a build'
assert_equals called "$(cat "$record/chmod.calls")" 'chmod failure was exercised'
assert_equals '' "$(cat "$case_dir/stdout")" 'chmod failure is not announced as staged'

prepare_case
run_stage 2 x86_64-pc-windows-msvc extra
assert_no_build
assert_absent "$record/rustc.calls" 'Windows extra args refuse before host lookup'
assert_absent "$record/git.calls" 'Windows extra args refuse before provenance query'

for mode in failed empty; do
  prepare_case
  case "$mode" in
    failed) fake_git_exit=19 ;;
    empty) fake_git_version='' ;;
  esac
  run_stage 1 x86_64-pc-windows-msvc
  assert_no_build
  assert_equals called "$(cat "$record/git.calls")" 'Windows provenance failure was exercised'
  assert_absent "$record/rustc.calls" 'explicit Windows target needs no Rust'
done

prepare_case
fake_go_exit=23
run_stage 23 x86_64-pc-windows-msvc
assert_equals called "$(cat "$record/go.calls")" 'Windows build failure was exercised'
assert_absent "$record/chmod.calls" 'failed Windows build is never made executable'
assert_absent "$fake_repo/desktop/src-tauri/binaries/citadel-x86_64-pc-windows-msvc" \
  'failed Windows build has no suffixless output'
assert_absent "$fake_repo/desktop/src-tauri/binaries/citadel-x86_64-pc-windows-msvc.exe.exe" \
  'failed Windows build has no doubled suffix'
assert_equals '' "$(cat "$case_dir/stdout")" 'failed Windows build is not announced as staged'
tests_run=$((tests_run + 1))
if [[ -x "$fake_repo/desktop/src-tauri/binaries/citadel-x86_64-pc-windows-msvc.exe" ]]; then
  echo 'Failed Windows build left an executable fake output' >&2
  exit 1
fi

prepare_case
fake_chmod_exit=31
run_stage 31 x86_64-pc-windows-msvc
assert_equals called "$(cat "$record/go.calls")" 'Windows chmod failure follows a build'
assert_equals called "$(cat "$record/chmod.calls")" 'Windows chmod failure was exercised'
assert_equals "$(printf '%s\n' 755 \
  "$fake_repo/desktop/src-tauri/binaries/citadel-x86_64-pc-windows-msvc.exe")" \
  "$(cat "$record/chmod.argv")" 'Windows chmod selects the suffixed output'
assert_absent "$fake_repo/desktop/src-tauri/binaries/citadel-x86_64-pc-windows-msvc" \
  'Windows chmod failure has no suffixless output'
assert_absent "$fake_repo/desktop/src-tauri/binaries/citadel-x86_64-pc-windows-msvc.exe.exe" \
  'Windows chmod failure has no doubled suffix'
assert_equals '' "$(cat "$case_dir/stdout")" 'Windows chmod failure is not announced as staged'

echo "Sidecar staging source tests passed ($tests_run assertions)"
