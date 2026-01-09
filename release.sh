#!/bin/bash
# release.sh
# Automates the complete release process for Citadel CLI:
# - Creates and pushes a git tag
# - Builds release artifacts
# - Creates a GitHub release with binaries

set -e

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

echo "🚀 Citadel CLI Release Automation"
echo ""

# Check if gh CLI is installed
if ! command -v gh &> /dev/null; then
    echo -e "${RED}Error: GitHub CLI (gh) is not installed.${NC}"
    echo "Install it from: https://cli.github.com/"
    exit 1
fi

# Check if working directory is clean
if [[ -n $(git status -s) ]]; then
    echo -e "${RED}Error: Working directory is not clean.${NC}"
    echo "Please commit or stash your changes before releasing."
    git status -s
    exit 1
fi

# Get version from argument or prompt
if [ -z "$1" ]; then
    CURRENT_VERSION=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
    echo -e "${YELLOW}Current version: $CURRENT_VERSION${NC}"
    echo ""
    read -p "Enter new version (e.g., v1.2.0): " VERSION
else
    VERSION="$1"
fi

# Validate version format
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9]+)?$ ]]; then
    echo -e "${RED}Error: Invalid version format.${NC}"
    echo "Version must be in format: v1.2.3 or v1.2.3-rc1"
    exit 1
fi

# Check if tag already exists
if git rev-parse "$VERSION" >/dev/null 2>&1; then
    echo -e "${RED}Error: Tag $VERSION already exists.${NC}"
    exit 1
fi

echo ""
echo "📝 Release Summary:"
echo "   Version: $VERSION"
echo "   Branch: $(git branch --show-current)"
echo "   Commit: $(git rev-parse --short HEAD)"
echo ""
read -p "Continue with release? (y/N): " -n 1 -r
echo ""
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Release cancelled."
    exit 1
fi

# Step 1: Create and push tag
echo ""
echo -e "${GREEN}Step 1/3: Creating and pushing git tag${NC}"
git tag "$VERSION"
git push origin "$VERSION"
echo "✅ Tag $VERSION created and pushed"

# Step 2: Build artifacts
echo ""
echo -e "${GREEN}Step 2/3: Building release artifacts${NC}"
./build.sh
echo "✅ Build complete"

# Step 3: Generate release notes from commits
echo ""
echo -e "${GREEN}Step 3/3: Creating GitHub release${NC}"

# Get commits since last tag
PREVIOUS_TAG=$(git describe --tags --abbrev=0 HEAD^ 2>/dev/null || echo "")
if [ -n "$PREVIOUS_TAG" ]; then
    COMMITS=$(git log $PREVIOUS_TAG..HEAD --pretty=format:"- %s" --no-merges)
else
    COMMITS=$(git log --pretty=format:"- %s" --no-merges -10)
fi

# Read checksums
CHECKSUMS=$(cat release/checksums.txt)

# Create release notes
RELEASE_NOTES="## What's New

$COMMITS

## Installation

Download the appropriate binary for your architecture:

\`\`\`bash
# For amd64
curl -LO https://github.com/aceteam-ai/citadel-cli/releases/download/$VERSION/citadel_${VERSION}_linux_amd64.tar.gz
tar -xzf citadel_${VERSION}_linux_amd64.tar.gz
sudo mv citadel /usr/local/bin/

# For arm64
curl -LO https://github.com/aceteam-ai/citadel-cli/releases/download/$VERSION/citadel_${VERSION}_linux_arm64.tar.gz
tar -xzf citadel_${VERSION}_linux_arm64.tar.gz
sudo mv citadel /usr/local/bin/
\`\`\`

## SHA256 Checksums

\`\`\`
$CHECKSUMS
\`\`\`"

# Create GitHub release
gh release create "$VERSION" \
  --title "$VERSION" \
  --notes "$RELEASE_NOTES" \
  release/citadel_${VERSION}_linux_amd64.tar.gz \
  release/citadel_${VERSION}_linux_arm64.tar.gz \
  release/checksums.txt

RELEASE_URL=$(gh release view "$VERSION" --json url -q .url)

echo ""
echo -e "${GREEN}✅ Release $VERSION published successfully!${NC}"
echo ""
echo "📦 Release URL: $RELEASE_URL"
echo ""
echo "Next steps:"
echo "  1. Review the release notes and edit if needed"
echo "  2. Announce the release to your team"
echo "  3. Update any documentation that references version numbers"
