#!/bin/bash
# This script builds the Citadel CLI for common server architectures.

set -e

echo "--- Building Citadel CLI..."
VERSION=$(git describe --tags --always --dirty || echo "dev")
OUTPUT_DIR="build"

# Clean previous builds
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

# Build for Linux AMD64 (most common servers)
echo "Building for linux/amd64..."
GOOS=linux GOARCH=amd64 go build -ldflags="-X main.version=$VERSION" -o "$OUTPUT_DIR/citadel-linux-amd64" .

# Build for Linux ARM64 (e.g., AWS Graviton, Raspberry Pi)
echo "Building for linux/arm64..."
GOOS=linux GOARCH=arm64 go build -ldflags="-X main.version=$VERSION" -o "$OUTPUT_DIR/citadel-linux-arm64" .

echo ""
echo "✅ Build complete. Binaries are in the './$OUTPUT_DIR' directory."
ls -l "$OUTPUT_DIR"
