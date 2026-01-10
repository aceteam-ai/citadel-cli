#!/bin/bash
# build.sh
# This script builds the Citadel CLI for common server architectures
# and packages them for a formal release.
#
# Usage:
#   ./build.sh          # Build for current platform only
#   ./build.sh --all    # Build for all platforms (linux/darwin, amd64/arm64)

set -e

# --- Parse Arguments ---
BUILD_ALL=false
if [[ "$1" == "--help" ]] || [[ "$1" == "-h" ]]; then
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Build the Citadel CLI binary."
    echo ""
    echo "Options:"
    echo "  --all       Build for all platforms (linux/darwin, amd64/arm64)"
    echo "  --help, -h  Show this help message"
    echo ""
    echo "By default, builds only for the current platform."
    exit 0
fi

if [[ "$1" == "--all" ]]; then
    BUILD_ALL=true
fi

echo "--- Building and Packaging Citadel CLI..."

# --- Configuration ---
VERSION=$(git describe --tags --always --dirty || echo "dev")
BUILD_DIR="build"
RELEASE_DIR="release"
MODULE_PATH=$(go list -m)
VERSION_VAR_PATH="${MODULE_PATH}/cmd.Version"

# --- Clean Up ---
rm -rf "$BUILD_DIR" "$RELEASE_DIR"
mkdir -p "$BUILD_DIR" "$RELEASE_DIR"
echo "--- Cleaned old build and release directories ---"

# --- Detect Current Platform ---
CURRENT_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
CURRENT_ARCH=$(uname -m)

# Normalize OS name
case "$CURRENT_OS" in
    linux) CURRENT_OS="linux" ;;
    darwin) CURRENT_OS="darwin" ;;
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
    PLATFORMS=("linux" "darwin")
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
        BINARY_PATH="$PLATFORM_DIR/citadel"
        RELEASE_NAME="citadel_${VERSION}_${OS}_${ARCH}.tar.gz"
        RELEASE_PATH="$RELEASE_DIR/$RELEASE_NAME"

        mkdir -p "$PLATFORM_DIR"

        # 1. Build the binary
        echo "Building binary..."
        GOOS=$OS GOARCH=$ARCH go build -ldflags="-X '${VERSION_VAR_PATH}=${VERSION}'" -o "$BINARY_PATH" .

        # 2. Package into a .tar.gz
        echo "Packaging to $RELEASE_NAME..."
        # The -C flag changes directory before archiving, so we don't get the full path in the tarball.
        tar -C "$PLATFORM_DIR" -czf "$RELEASE_PATH" citadel
    done
done

# --- Create Symlink for Current Platform ---
CURRENT_BINARY="$BUILD_DIR/${CURRENT_OS}-${CURRENT_ARCH}/citadel"
if [[ -f "$CURRENT_BINARY" ]]; then
    ln -sf "$CURRENT_BINARY" citadel
    echo ""
    echo "--- Created symlink: citadel -> $CURRENT_BINARY ---"
fi

# --- Generate Checksums ---
echo ""
echo "--- Generating Checksums ---"
# Use shasum on macOS, sha256sum on Linux
if command -v sha256sum &> /dev/null; then
    (cd "$RELEASE_DIR" && sha256sum *.tar.gz > checksums.txt)
else
    (cd "$RELEASE_DIR" && shasum -a 256 *.tar.gz > checksums.txt)
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
