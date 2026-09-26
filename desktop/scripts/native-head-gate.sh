#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || ! "$1" =~ ^[0-9]+$ || ! "$2" =~ ^[0-9a-f]{40}$ ]]; then
  echo 'Usage: native-head-gate.sh <same-repository PR number> <reviewed 40-character head SHA>' >&2
  exit 2
fi

pr_number="$1"
expected_sha="$2"
repo="aceteam-ai/citadel-cli"
repo_root="$(git rev-parse --show-toplevel)"
origin="$(git -C "$repo_root" remote get-url origin)"
case "$origin" in
  "git@github.com:$repo.git"|"https://github.com/$repo.git") ;;
  *) echo 'Run this gate from the canonical Citadel checkout' >&2; exit 1 ;;
esac
head_repository="$(gh api "repos/$repo/pulls/$pr_number" --jq '.head.repo.full_name')"
actual_sha="$(gh api "repos/$repo/pulls/$pr_number" --jq '.head.sha')"
test "$head_repository" = "$repo"
test "$actual_sha" = "$expected_sha"

identities="$(security find-identity -v -p codesigning)"
if grep -Eq '[1-9][0-9]* valid identities found' <<< "$identities"; then
  echo 'Native gate host must not expose code-signing identities' >&2
  exit 1
fi

test "$(rustc --version)" = 'rustc 1.95.0 (59807616e 2026-04-14)'
test "$(node --version)" = 'v22.21.1'

git -C "$repo_root" fetch origin "$expected_sha"
test "$(gh api "repos/$repo/pulls/$pr_number" --jq '.head.sha')" = "$expected_sha"
gate_dir="$(mktemp -d "${TMPDIR:-/tmp}/citadel-native-head.XXXXXXXX")"
rmdir "$gate_dir"
git -C "$repo_root" worktree add --detach "$gate_dir" "$expected_sha"
test "$(git -C "$gate_dir" rev-parse HEAD)" = "$expected_sha"

echo "Building reviewed head $expected_sha in $gate_dir"
(
  cd "$gate_dir/desktop"
  npm ci
  npm run build
  ./scripts/stage-sidecar.sh
  (cd src-tauri && cargo test --locked)
  npm run tauri -- build --bundles app --no-sign --ci
)
echo "Native gate complete. Review the unsigned bundle in $gate_dir/desktop/src-tauri/target/release/bundle/macos"
