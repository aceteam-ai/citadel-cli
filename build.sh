#!/bin/bash
# build.sh
# This script builds the Citadel CLI for common server architectures
# and packages them for a formal release.

set -e

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

# --- Build and Package Loop ---
ARCHS=("amd64" "arm64")
for ARCH in "${ARCHS[@]}"; do
    echo ""
    echo "--- Processing linux/$ARCH ---"
    
    # Define paths and names
    PLATFORM_DIR="$BUILD_DIR/linux-$ARCH"
    BINARY_PATH="$PLATFORM_DIR/citadel"
    RELEASE_NAME="citadel_${VERSION}_linux_${ARCH}.tar.gz"
    RELEASE_PATH="$RELEASE_DIR/$RELEASE_NAME"
    
    mkdir -p "$PLATFORM_DIR"

    # 1. Build the binary
    echo "Building binary..."
    GOOS=linux GOARCH=$ARCH go build -ldflags="-X '${VERSION_VAR_PATH}=${VERSION}'" -o "$BINARY_PATH" .

    # 2. Package into a .tar.gz
    echo "Packaging to $RELEASE_NAME..."
    # The -C flag changes directory before archiving, so we don't get the full path in the tarball.
    tar -C "$PLATFORM_DIR" -czf "$RELEASE_PATH" citadel
done

# --- Generate Checksums ---
echo ""
echo "--- Generating Checksums ---"
(cd "$RELEASE_DIR" && sha256sum *.tar.gz > checksums.txt)

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
