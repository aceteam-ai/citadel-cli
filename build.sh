#!/bin/bash
# build.sh
# This script builds the Citadel CLI for common server architectures
# and packages them for a formal release.
#
# Usage:
#   ./build.sh          # Build for current platform only
#   ./build.sh --all    # Build for all platforms (linux/darwin/windows, amd64/arm64)
#   ./build.sh --dmg    # Also build the signed macOS .app + DMG (darwin only;
#                       # see scripts/macos-sign.sh for signing/notarization env)

set -e

# --- Parse Arguments ---
BUILD_ALL=false
BUILD_DMG=false
for arg in "$@"; do
    case "$arg" in
        --help|-h)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Build the Citadel CLI binary."
            echo ""
            echo "Options:"
            echo "  --all       Build for all platforms (linux/darwin/windows, amd64/arm64)"
            echo "  --dmg       Also build the signed macOS .app + DMG bundle (darwin only)"
            echo "  --help, -h  Show this help message"
            echo ""
            echo "By default, builds only for the current platform."
            exit 0
            ;;
        --all) BUILD_ALL=true ;;
        --dmg) BUILD_DMG=true ;;
    esac
done

echo "--- Building and Packaging Citadel CLI..."

# --- Configuration ---
VERSION=$(git describe --tags --always --dirty || echo "dev")
BUILD_DIR="build"
RELEASE_DIR="release"
MODULE_PATH=$(go list -m)
VERSION_VAR_PATH="${MODULE_PATH}/cmd.version"

# --- Man Page Generation ---
MAN_DIR="docs/man"

# --- Clean Up ---
rm -rf "$BUILD_DIR" "$RELEASE_DIR" "$MAN_DIR"
mkdir -p "$BUILD_DIR" "$RELEASE_DIR"
echo "--- Cleaned old build and release directories ---"

# Generate man pages (for release builds or when --all is specified)
if [[ "$BUILD_ALL" == true ]]; then
    echo "--- Generating man pages ---"
    go run docs/gen-manpage.go
fi

# --- Detect Current Platform ---
CURRENT_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
CURRENT_ARCH=$(uname -m)

# Normalize OS name
case "$CURRENT_OS" in
    linux) CURRENT_OS="linux" ;;
    darwin) CURRENT_OS="darwin" ;;
    mingw*|msys*|cygwin*) CURRENT_OS="windows" ;;
    *) echo "⚠️  Unknown OS: $CURRENT_OS, defaulting to linux"; CURRENT_OS="linux" ;;
esac

# Normalize architecture name
case "$CURRENT_ARCH" in
    x86_64|amd64) CURRENT_ARCH="amd64" ;;
    aarch64|arm64) CURRENT_ARCH="arm64" ;;
    *) echo "⚠️  Unknown architecture: $CURRENT_ARCH, defaulting to amd64"; CURRENT_ARCH="amd64" ;;
esac

# --- Determine Build Targets ---
if [[ "$BUILD_ALL" == true ]]; then
    echo "--- Building for all platforms (--all flag detected) ---"
    PLATFORMS=("linux" "darwin" "windows")
    ARCHS=("amd64" "arm64")
else
    echo "--- Building for current platform only: $CURRENT_OS/$CURRENT_ARCH ---"
    echo "    (Use --all flag to build for all platforms)"
    PLATFORMS=("$CURRENT_OS")
    ARCHS=("$CURRENT_ARCH")
fi

# --- Build and Package Loop ---

for OS in "${PLATFORMS[@]}"; do
    for ARCH in "${ARCHS[@]}"; do
        echo ""
        echo "--- Processing $OS/$ARCH ---"

        # Define paths and names
        PLATFORM_DIR="$BUILD_DIR/${OS}-${ARCH}"

        # Define binary name with .exe for Windows
        BINARY_NAME="citadel"
        if [[ "$OS" == "windows" ]]; then
            BINARY_NAME="citadel.exe"
        fi

        BINARY_PATH="$PLATFORM_DIR/$BINARY_NAME"

        mkdir -p "$PLATFORM_DIR"

        # 1. Build the binary
        echo "Building binary..."
        CGO_ENABLED=0 GOOS=$OS GOARCH=$ARCH go build -ldflags="-X '${VERSION_VAR_PATH}=${VERSION}'" -o "$BINARY_PATH" ./cmd/citadel

        # 2. Copy man page if available (not for Windows)
        if [[ "$OS" != "windows" ]] && [[ -f "$MAN_DIR/citadel.1" ]]; then
            cp "$MAN_DIR/citadel.1" "$PLATFORM_DIR/"
        fi

        # 3. Package (Windows uses .zip, others use .tar.gz)
        if [[ "$OS" == "windows" ]]; then
            RELEASE_NAME="citadel_${VERSION}_${OS}_${ARCH}.zip"
            RELEASE_PATH="$RELEASE_DIR/$RELEASE_NAME"
            echo "Packaging to $RELEASE_NAME..."
            # Use absolute path for zip output
            ABSOLUTE_RELEASE_PATH="$(cd "$(dirname "$RELEASE_PATH")" && pwd)/$(basename "$RELEASE_PATH")"
            (cd "$PLATFORM_DIR" && zip -q "$ABSOLUTE_RELEASE_PATH" "$BINARY_NAME")
        else
            RELEASE_NAME="citadel_${VERSION}_${OS}_${ARCH}.tar.gz"
            RELEASE_PATH="$RELEASE_DIR/$RELEASE_NAME"
            echo "Packaging to $RELEASE_NAME..."
            # Include man page if available
            if [[ -f "$PLATFORM_DIR/citadel.1" ]]; then
                tar -C "$PLATFORM_DIR" -czf "$RELEASE_PATH" citadel citadel.1
            else
                tar -C "$PLATFORM_DIR" -czf "$RELEASE_PATH" citadel
            fi
        fi
    done
done

# --- Create Symlink for Current Platform ---
CURRENT_BINARY="$BUILD_DIR/${CURRENT_OS}-${CURRENT_ARCH}/citadel"
if [[ -f "$CURRENT_BINARY" ]]; then
    ln -sf "$CURRENT_BINARY" citadel
    echo ""
    echo "--- Created symlink: citadel -> $CURRENT_BINARY ---"
fi

# --- Build signed macOS .app + DMG (--dmg only) ---
# Reuses build-dmg.sh (which owns .app assembly + scripts/macos-sign.sh) for
# every darwin/ARCH binary this run actually produced, rather than
# duplicating bundle assembly here. No-ops with a warning off Darwin (hdiutil
# and codesign don't exist elsewhere) and skip-with-warning without signing
# credentials — see scripts/macos-sign.sh; this is a local/dev entry point,
# not the release path (that's scripts/release.sh, which sets
# CITADEL_REQUIRE_SIGNING=1).
if [[ "$BUILD_DMG" == true ]]; then
    if [[ "$(uname -s)" != "Darwin" ]]; then
        echo ""
        echo "⚠️  --dmg requested but not running on macOS; skipping .app/DMG build (hdiutil/codesign are macOS-only)."
    else
        for ARCH in "${ARCHS[@]}"; do
            DARWIN_BIN="$BUILD_DIR/darwin-${ARCH}/citadel"
            if [[ ! -f "$DARWIN_BIN" ]]; then
                echo ""
                echo "⚠️  --dmg: no darwin/${ARCH} binary was built above; skipping."
                continue
            fi
            echo ""
            echo "--- Building signed macOS .app + DMG for darwin/${ARCH} ---"
            ./build-dmg.sh --binary "$DARWIN_BIN" --version "$VERSION" --arch "$ARCH"
            # build-dmg.sh normalizes its DMG name to a leading "v"; VERSION
            # here (git describe --tags) already carries one, so this matches
            # directly without re-deriving it.
            DMG_NAME="citadel_${VERSION}_darwin_${ARCH}.dmg"
            if [[ -f "$BUILD_DIR/$DMG_NAME" ]]; then
                cp "$BUILD_DIR/$DMG_NAME" "$RELEASE_DIR/$DMG_NAME"
            else
                echo "⚠️  Expected $BUILD_DIR/$DMG_NAME after build-dmg.sh but it's missing."
            fi
        done
    fi
fi

# --- Generate Checksums ---
echo ""
echo "--- Generating Checksums ---"
# Use shasum on macOS, sha256sum on Linux
if command -v sha256sum &> /dev/null; then
    (cd "$RELEASE_DIR" && sha256sum *.tar.gz *.zip *.dmg 2>/dev/null > checksums.txt || sha256sum *.tar.gz > checksums.txt)
else
    (cd "$RELEASE_DIR" && shasum -a 256 *.tar.gz *.zip *.dmg 2>/dev/null > checksums.txt || shasum -a 256 *.tar.gz > checksums.txt)
fi

echo "✅ Build and packaging complete."
echo ""
echo "Binaries for local use are in: './$BUILD_DIR'"
tree "$BUILD_DIR"
echo ""
echo "Release artifacts are in: './$RELEASE_DIR'"
tree "$RELEASE_DIR"
echo ""
echo "📋 SHA256 Checksums (copy this into your release notes):"
echo "----------------------------------------------------"
cat "$RELEASE_DIR/checksums.txt"
echo "----------------------------------------------------"
