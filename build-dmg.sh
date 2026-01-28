#!/bin/bash
# build-dmg.sh - Build macOS DMG installer for Citadel
#
# This script creates a macOS .app bundle and packages it into a DMG file
# suitable for distribution to novice users.
#
# Usage:
#   ./build-dmg.sh [--version VERSION] [--binary PATH]
#
# Options:
#   --version VERSION   Version string (default: extracted from build.sh or "dev")
#   --binary PATH       Path to pre-built citadel binary (default: builds for darwin/arm64)
#
# Requirements:
#   - macOS (uses hdiutil which is macOS-only)
#   - Go (if building the binary)
#
# Output:
#   - build/Citadel.app          - The macOS application bundle
#   - build/Citadel-VERSION.dmg  - The distributable DMG file

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Script directory
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BUILD_DIR="$SCRIPT_DIR/build"
PACKAGING_DIR="$SCRIPT_DIR/packaging/macos"

# Default values
VERSION=""
BINARY_PATH=""
ARCH="${GOARCH:-$(uname -m)}"

# Normalize architecture name
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
esac

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --version)
            VERSION="$2"
            shift 2
            ;;
        --binary)
            BINARY_PATH="$2"
            shift 2
            ;;
        --arch)
            ARCH="$2"
            shift 2
            ;;
        -h|--help)
            echo "Usage: $0 [--version VERSION] [--binary PATH] [--arch ARCH]"
            echo ""
            echo "Options:"
            echo "  --version VERSION   Version string (default: from git tag or 'dev')"
            echo "  --binary PATH       Path to pre-built citadel binary"
            echo "  --arch ARCH         Target architecture: amd64 or arm64 (default: current)"
            exit 0
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            exit 1
            ;;
    esac
done

# Get version from git if not specified
if [ -z "$VERSION" ]; then
    VERSION=$(git describe --tags --always 2>/dev/null || echo "dev")
fi

echo -e "${GREEN}Building Citadel DMG${NC}"
echo "  Version: $VERSION"
echo "  Architecture: $ARCH"

# Check if running on macOS
if [[ "$(uname)" != "Darwin" ]]; then
    echo -e "${YELLOW}Warning: DMG creation requires macOS. Building .app bundle only.${NC}"
    CREATE_DMG=false
else
    CREATE_DMG=true
fi

# Create build directory
mkdir -p "$BUILD_DIR"

# Build or locate the citadel binary
if [ -n "$BINARY_PATH" ]; then
    if [ ! -f "$BINARY_PATH" ]; then
        echo -e "${RED}Error: Binary not found at $BINARY_PATH${NC}"
        exit 1
    fi
    CITADEL_BIN="$BINARY_PATH"
    echo "  Using binary: $CITADEL_BIN"
else
    echo "  Building citadel for darwin/$ARCH..."
    CITADEL_BIN="$BUILD_DIR/citadel-darwin-$ARCH"

    # Get module path for version injection
    MODULE_PATH=$(go list -m 2>/dev/null || echo "citadel-cli")

    GOOS=darwin GOARCH="$ARCH" go build \
        -ldflags="-X '${MODULE_PATH}/cmd.Version=${VERSION}'" \
        -o "$CITADEL_BIN" \
        "$SCRIPT_DIR"

    echo -e "${GREEN}  Built: $CITADEL_BIN${NC}"
fi

# Create .app bundle structure
APP_NAME="Citadel.app"
APP_DIR="$BUILD_DIR/$APP_NAME"
CONTENTS_DIR="$APP_DIR/Contents"
MACOS_DIR="$CONTENTS_DIR/MacOS"
RESOURCES_DIR="$CONTENTS_DIR/Resources"

echo "Creating .app bundle..."

# Clean previous build
rm -rf "$APP_DIR"

# Create directory structure
mkdir -p "$MACOS_DIR"
mkdir -p "$RESOURCES_DIR"

# Copy Info.plist and update version
cp "$PACKAGING_DIR/Info.plist" "$CONTENTS_DIR/Info.plist"

# Update version in Info.plist
if command -v sed &> /dev/null; then
    # Use sed to update version strings
    sed -i.bak "s/<string>1.0.0<\/string>/<string>${VERSION}<\/string>/g" "$CONTENTS_DIR/Info.plist"
    rm -f "$CONTENTS_DIR/Info.plist.bak"
fi

# Copy launcher script
cp "$PACKAGING_DIR/citadel-launcher" "$MACOS_DIR/citadel-launcher"
chmod +x "$MACOS_DIR/citadel-launcher"

# Copy citadel binary
cp "$CITADEL_BIN" "$MACOS_DIR/citadel"
chmod +x "$MACOS_DIR/citadel"

# Create a simple placeholder icon if AppIcon.icns doesn't exist
# In production, you would include a proper .icns file
if [ ! -f "$RESOURCES_DIR/AppIcon.icns" ]; then
    echo "  Note: No AppIcon.icns found, app will use default icon"
fi

echo -e "${GREEN}Created: $APP_DIR${NC}"

# Create DMG (macOS only)
if [ "$CREATE_DMG" = true ]; then
    DMG_NAME="Citadel-${VERSION}-${ARCH}.dmg"
    DMG_PATH="$BUILD_DIR/$DMG_NAME"
    DMG_TEMP="$BUILD_DIR/dmg-temp"

    echo "Creating DMG..."

    # Clean previous DMG build
    rm -rf "$DMG_TEMP"
    rm -f "$DMG_PATH"

    # Create temporary directory for DMG contents
    mkdir -p "$DMG_TEMP"

    # Copy app to DMG temp directory
    cp -R "$APP_DIR" "$DMG_TEMP/"

    # Create symlink to Applications folder
    ln -s /Applications "$DMG_TEMP/Applications"

    # Create a README file for the DMG
    cat > "$DMG_TEMP/README.txt" << 'EOF'
Citadel - AceTeam Sovereign Compute Fabric

Installation:
1. Drag Citadel.app to the Applications folder
2. Double-click Citadel.app to open a terminal with Citadel ready to use

First-time setup:
1. Open Citadel.app
2. Run: citadel init
3. Follow the on-screen instructions to connect to the AceTeam Network

For more information, visit: https://aceteam.ai

Note: On first launch, macOS may warn about an unidentified developer.
To open the app: Right-click Citadel.app > Open > Click "Open" in the dialog.
EOF

    # Create DMG using hdiutil
    # -volname: Name shown when mounted
    # -srcfolder: Source folder to include
    # -ov: Overwrite existing DMG
    # -format UDZO: Compressed DMG format
    hdiutil create \
        -volname "Citadel $VERSION" \
        -srcfolder "$DMG_TEMP" \
        -ov \
        -format UDZO \
        "$DMG_PATH"

    # Clean up temp directory
    rm -rf "$DMG_TEMP"

    echo -e "${GREEN}Created: $DMG_PATH${NC}"

    # Print DMG info
    echo ""
    echo "DMG Details:"
    ls -lh "$DMG_PATH"
else
    echo ""
    echo -e "${YELLOW}Skipping DMG creation (not running on macOS)${NC}"
    echo "To create a DMG, run this script on macOS."
fi

echo ""
echo -e "${GREEN}Build complete!${NC}"
echo ""
echo "Output files:"
echo "  App Bundle: $APP_DIR"
if [ "$CREATE_DMG" = true ]; then
    echo "  DMG File:   $DMG_PATH"
fi
