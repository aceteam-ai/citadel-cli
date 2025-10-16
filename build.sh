#!/bin/bash
# build.sh
# This script builds the Citadel CLI for common server architectures.

set -e

echo "--- Building Citadel CLI..."
# Use git to determine version, fall back to "dev"
VERSION=$(git describe --tags --always --dirty || echo "dev")
OUTPUT_DIR="build"

# Clean previous builds
rm -rf "$OUTPUT_DIR"
echo "--- Cleaned old build directory ---"

# --- Build for Linux AMD64 (most common servers) ---
PLATFORM_DIR_AMD64="$OUTPUT_DIR/linux-amd64"
mkdir -p "$PLATFORM_DIR_AMD64"
echo "Building for linux/amd64..."
GOOS=linux GOARCH=amd64 go build -ldflags="-X main.version=$VERSION" -o "$PLATFORM_DIR_AMD64/citadel" .

# --- Build for Linux ARM64 (e.g., AWS Graviton, Raspberry Pi) ---
PLATFORM_DIR_ARM64="$OUTPUT_DIR/linux-arm64"
mkdir -p "$PLATFORM_DIR_ARM64"
echo "Building for linux/arm64..."
GOOS=linux GOARCH=arm64 go build -ldflags="-X main.version=$VERSION" -o "$PLATFORM_DIR_ARM64/citadel" .

echo ""
echo "✅ Build complete. Binaries are in the './$OUTPUT_DIR' directory."
tree "$OUTPUT_DIR"