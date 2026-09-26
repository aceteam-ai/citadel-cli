#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
target="${1:-$(rustc --print host-tuple)}"
case "$target" in
  aarch64-apple-darwin) go_arch=arm64 ;;
  x86_64-apple-darwin) go_arch=amd64 ;;
  *) echo "This first Citadel desktop build supports macOS targets only: $target" >&2; exit 2 ;;
esac

output="$repo_dir/desktop/src-tauri/binaries/citadel-$target"
mkdir -p "$(dirname "$output")"
(
  cd "$repo_dir"
  CGO_ENABLED=0 GOOS=darwin GOARCH="$go_arch" go build -buildvcs=false \
    -ldflags="-X github.com/aceteam-ai/citadel-cli/cmd.Version=$(git describe --tags --always)" \
    -o "$output" ./cmd/citadel
)
chmod 755 "$output"
echo "Staged bundled Citadel helper for $target"
