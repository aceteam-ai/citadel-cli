#!/usr/bin/env bash
set -euo pipefail

if [[ $# -gt 1 ]]; then
  echo 'Usage: stage-sidecar.sh [<supported-target-triple>]' >&2
  exit 2
fi

if [[ $# -eq 0 ]]; then
  if ! target="$(rustc --print host-tuple)"; then
    echo 'Could not determine the Rust host target' >&2
    exit 1
  fi
else
  target="$1"
fi

output_suffix=''
case "$target" in
  aarch64-apple-darwin) go_os=darwin; go_arch=arm64 ;;
  x86_64-apple-darwin) go_os=darwin; go_arch=amd64 ;;
  x86_64-unknown-linux-gnu) go_os=linux; go_arch=amd64 ;;
  aarch64-unknown-linux-gnu) go_os=linux; go_arch=arm64 ;;
  x86_64-pc-windows-msvc) go_os=windows; go_arch=amd64; output_suffix=.exe ;;
  *) echo "Unsupported Citadel desktop helper target: ${target:-<empty>}" >&2; exit 2 ;;
esac

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! version="$(cd "$repo_dir" && git describe --tags --always)"; then
  echo 'Could not determine the Citadel helper version' >&2
  exit 1
fi
if [[ -z "$version" ]]; then
  echo 'Citadel helper version is empty' >&2
  exit 1
fi

output="$repo_dir/desktop/src-tauri/binaries/citadel-$target$output_suffix"
mkdir -p "$(dirname "$output")"
(
  cd "$repo_dir"
  CGO_ENABLED=0 GOOS="$go_os" GOARCH="$go_arch" go build -buildvcs=false \
    -ldflags="-X github.com/aceteam-ai/citadel-cli/cmd.Version=$version" \
    -o "$output" ./cmd/citadel
)
chmod 755 "$output"
echo "Staged bundled Citadel helper for $target"
