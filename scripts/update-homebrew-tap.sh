#!/bin/bash
# update-homebrew-tap.sh — Sync the aceteam-ai/homebrew-tap Citadel formula to a
# published release: rewrites the formula version and the four per-platform
# SHA256s, then commits and pushes.
#
# Invoked automatically by release.sh (hook_post_release). Can also be run
# standalone to (re)sync the tap to any already-published GitHub release — handy
# for backfilling a version the tap missed.
#
# Usage:
#   ./scripts/update-homebrew-tap.sh            # version = latest vX.Y.Z git tag
#   ./scripts/update-homebrew-tap.sh 2.73.0     # explicit version (no leading v)
#
# Env:
#   CHECKSUMS_FILE  If set, read checksums from this file instead of downloading
#                   them from the GitHub release (release.sh passes its freshly
#                   built release/checksums.txt).
#
# Requires: gh (authenticated with push access to the tap), git.

set -euo pipefail

SOURCE_REPO="aceteam-ai/citadel-cli"
TAP_REPO="aceteam-ai/homebrew-tap"
FORMULA_PATH="Formula/citadel.rb"
TAG_PREFIX="v"

# Homebrew ships macOS + Linux only; the formula has no Windows bottles.
PLATFORMS=(darwin_arm64 darwin_amd64 linux_arm64 linux_amd64)

info()  { echo "[tap] $*"; }
fatal() { echo "[tap] ERROR: $*" >&2; exit 1; }

command -v gh  >/dev/null || fatal "gh is required (authenticated with tap push access)."
command -v git >/dev/null || fatal "git is required."

# ── Resolve version ─────────────────────────────────────────────────────────
VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  VERSION=$(git tag --list "${TAG_PREFIX}*" --sort=-version:refname 2>/dev/null | head -1)
  VERSION="${VERSION#"$TAG_PREFIX"}"
fi
[[ -n "$VERSION" ]] || fatal "Could not determine version (pass it explicitly, e.g. 2.73.0)."
TAG="${TAG_PREFIX}${VERSION}"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# ── Obtain checksums ────────────────────────────────────────────────────────
CHECKSUMS="${CHECKSUMS_FILE:-}"
if [[ -n "$CHECKSUMS" ]]; then
  [[ -f "$CHECKSUMS" ]] || fatal "CHECKSUMS_FILE=$CHECKSUMS does not exist."
  info "Using local checksums: $CHECKSUMS"
else
  info "Downloading checksums.txt from $SOURCE_REPO release $TAG..."
  gh release download "$TAG" --repo "$SOURCE_REPO" --pattern checksums.txt --dir "$WORK" \
    || fatal "Could not download checksums.txt for $TAG (is the release published?)."
  CHECKSUMS="$WORK/checksums.txt"
fi

# ── Extract per-platform SHA256s ────────────────────────────────────────────
declare -A SHA
for plat in "${PLATFORMS[@]}"; do
  archive="citadel_v${VERSION}_${plat}.tar.gz"
  sum=$(awk -v a="$archive" '$2==a {print $1}' "$CHECKSUMS")
  [[ -n "$sum" ]] || fatal "No checksum for $archive in $CHECKSUMS."
  SHA[$plat]="$sum"
done

# ── Clone tap and rewrite the formula ───────────────────────────────────────
info "Cloning $TAP_REPO..."
gh repo clone "$TAP_REPO" "$WORK/tap" -- --depth 1 --quiet \
  || fatal "Could not clone $TAP_REPO."
cd "$WORK/tap"
[[ -f "$FORMULA_PATH" ]] || fatal "$FORMULA_PATH not found in tap."

# awk keys each sha256 line off the platform token in the url line above it, so
# the four bottles are matched positionally-independent and never mixed up. The
# url lines interpolate #{version}, so only the version line + sha256s change.
awk \
  -v ver="$VERSION" \
  -v da="${SHA[darwin_arm64]}" -v di="${SHA[darwin_amd64]}" \
  -v la="${SHA[linux_arm64]}"  -v li="${SHA[linux_amd64]}" '
  /^[[:space:]]*version "/            { sub(/version "[^"]*"/, "version \"" ver "\""); print; next }
  /url ".*_darwin_arm64\./           { plat="da"; print; next }
  /url ".*_darwin_amd64\./           { plat="di"; print; next }
  /url ".*_linux_arm64\./            { plat="la"; print; next }
  /url ".*_linux_amd64\./            { plat="li"; print; next }
  /^[[:space:]]*sha256 "/ {
    if      (plat=="da") sub(/sha256 "[^"]*"/, "sha256 \"" da "\"")
    else if (plat=="di") sub(/sha256 "[^"]*"/, "sha256 \"" di "\"")
    else if (plat=="la") sub(/sha256 "[^"]*"/, "sha256 \"" la "\"")
    else if (plat=="li") sub(/sha256 "[^"]*"/, "sha256 \"" li "\"")
    plat=""; print; next
  }
  { print }
' "$FORMULA_PATH" > "$FORMULA_PATH.tmp"
mv "$FORMULA_PATH.tmp" "$FORMULA_PATH"

if git diff --quiet -- "$FORMULA_PATH"; then
  info "Formula already at $VERSION with matching checksums — nothing to push."
  exit 0
fi

info "Formula updated to $VERSION:"
git --no-pager diff -- "$FORMULA_PATH"

git add "$FORMULA_PATH"
git commit -m "citadel ${VERSION}" --quiet
git push origin HEAD --quiet
info "Pushed formula update to $TAP_REPO (citadel ${VERSION})."
