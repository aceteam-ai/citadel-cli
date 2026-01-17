#!/bin/bash
# release.sh
# Automates the complete release process for Citadel CLI:
# - Creates and pushes a git tag
# - Builds release artifacts
# - Creates a GitHub release with binaries
#
# Usage:
#   ./release.sh                              # Interactive mode
#   ./release.sh -v v1.2.0 -y                 # Non-interactive with version
#   ./release.sh -v v1.2.0 -y --notes "..."   # With custom release notes
#   ./release.sh --dry-run -v v1.2.0          # Dry run (no git/gh commands)

set -e

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# --- Default Values ---
VERSION=""
AUTO_CONFIRM=false
DRY_RUN=false
CUSTOM_NOTES=""

# --- Parse Arguments ---
print_help() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Automates the complete release process for Citadel CLI."
    echo ""
    echo "Options:"
    echo "  -v, --version VERSION   Version to release (e.g., v1.2.0)"
    echo "  -y, --yes               Auto-confirm without prompting"
    echo "  -n, --notes TEXT        Custom release notes summary (replaces auto-generated)"
    echo "  --dry-run               Show what would be done without executing"
    echo "  -h, --help              Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0                                    # Interactive mode"
    echo "  $0 -v v1.2.0 -y                       # Non-interactive release"
    echo "  $0 -v v1.2.0 -y -n 'Added macOS support and improved performance'"
    echo "  $0 --dry-run -v v1.2.0                # Preview without executing"
    echo ""
    echo "AI/Claude integration:"
    echo "  Generate release notes with AI and pass them via --notes flag:"
    echo "  $0 -v v1.2.0 -y --notes \"\$(claude 'summarize changes for release')\""
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -v|--version)
            VERSION="$2"
            shift 2
            ;;
        -y|--yes)
            AUTO_CONFIRM=true
            shift
            ;;
        -n|--notes)
            CUSTOM_NOTES="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        -h|--help)
            print_help
            exit 0
            ;;
        *)
            echo -e "${RED}Error: Unknown option: $1${NC}"
            print_help
            exit 1
            ;;
    esac
done

# --- Helper Functions ---
run_cmd() {
    if [[ "$DRY_RUN" == true ]]; then
        echo -e "${BLUE}[DRY-RUN] Would execute: $*${NC}"
    else
        "$@"
    fi
}

echo "🚀 Citadel CLI Release Automation"
if [[ "$DRY_RUN" == true ]]; then
    echo -e "${YELLOW}   (DRY RUN MODE - no changes will be made)${NC}"
fi
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
CURRENT_VERSION=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
if [ -z "$VERSION" ]; then
    echo -e "${YELLOW}Current version: $CURRENT_VERSION${NC}"
    echo ""
    read -p "Enter new version (e.g., v1.2.0): " VERSION
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

# --- Generate Change Summary ---
echo ""
echo -e "${GREEN}Analyzing changes since $CURRENT_VERSION...${NC}"

# Get commits since last tag
if [ "$CURRENT_VERSION" != "v0.0.0" ]; then
    COMMIT_LOG=$(git log $CURRENT_VERSION..HEAD --pretty=format:"- %s" --no-merges)
    COMMIT_COUNT=$(git rev-list --count $CURRENT_VERSION..HEAD)
    FILES_CHANGED=$(git diff --stat $CURRENT_VERSION..HEAD | tail -1)

    # Get list of changed files by category
    CHANGED_FILES=$(git diff --name-only $CURRENT_VERSION..HEAD)

    # Categorize changes
    HAS_CMD_CHANGES=$(echo "$CHANGED_FILES" | grep -c "^cmd/" || true)
    HAS_INTERNAL_CHANGES=$(echo "$CHANGED_FILES" | grep -c "^internal/" || true)
    HAS_SERVICE_CHANGES=$(echo "$CHANGED_FILES" | grep -c "^services/" || true)
    HAS_BUILD_CHANGES=$(echo "$CHANGED_FILES" | grep -cE "^(build|release|Makefile|go\.(mod|sum))" || true)
    HAS_DOC_CHANGES=$(echo "$CHANGED_FILES" | grep -cE "\.(md|txt)$" || true)
else
    COMMIT_LOG=$(git log --pretty=format:"- %s" --no-merges -20)
    COMMIT_COUNT="N/A"
    FILES_CHANGED="Initial release"
    HAS_CMD_CHANGES=0
    HAS_INTERNAL_CHANGES=0
    HAS_SERVICE_CHANGES=0
    HAS_BUILD_CHANGES=0
    HAS_DOC_CHANGES=0
fi

# Build a smart summary if no custom notes provided
if [ -z "$CUSTOM_NOTES" ]; then
    CHANGE_CATEGORIES=""
    [[ $HAS_CMD_CHANGES -gt 0 ]] && CHANGE_CATEGORIES="${CHANGE_CATEGORIES}CLI commands, "
    [[ $HAS_INTERNAL_CHANGES -gt 0 ]] && CHANGE_CATEGORIES="${CHANGE_CATEGORIES}core internals, "
    [[ $HAS_SERVICE_CHANGES -gt 0 ]] && CHANGE_CATEGORIES="${CHANGE_CATEGORIES}service definitions, "
    [[ $HAS_BUILD_CHANGES -gt 0 ]] && CHANGE_CATEGORIES="${CHANGE_CATEGORIES}build system, "
    [[ $HAS_DOC_CHANGES -gt 0 ]] && CHANGE_CATEGORIES="${CHANGE_CATEGORIES}documentation, "
    CHANGE_CATEGORIES=${CHANGE_CATEGORIES%, }  # Remove trailing comma

    if [ -n "$CHANGE_CATEGORIES" ]; then
        AUTO_SUMMARY="Changes to: ${CHANGE_CATEGORIES}"
    else
        AUTO_SUMMARY="Various improvements and fixes"
    fi
else
    AUTO_SUMMARY="$CUSTOM_NOTES"
fi

echo ""
echo "📝 Release Summary:"
echo "   Version: $VERSION (from $CURRENT_VERSION)"
echo "   Branch: $(git branch --show-current)"
echo "   Commit: $(git rev-parse --short HEAD)"
echo "   Changes: $COMMIT_COUNT commits, $FILES_CHANGED"
echo ""
echo "   Summary: $AUTO_SUMMARY"
echo ""
echo "   Commits:"
echo "$COMMIT_LOG" | head -10 | sed 's/^/      /'
if [ $(echo "$COMMIT_LOG" | wc -l) -gt 10 ]; then
    echo "      ... and more"
fi
echo ""

if [[ "$AUTO_CONFIRM" != true ]]; then
    read -p "Continue with release? (y/N): " -n 1 -r
    echo ""
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Release cancelled."
        exit 1
    fi
fi

# Step 1: Create and push tag
echo ""
echo -e "${GREEN}Step 1/3: Creating and pushing git tag${NC}"
run_cmd git tag -a "$VERSION" -m "$VERSION"
run_cmd git push origin "$VERSION"
echo "✅ Tag $VERSION created and pushed"

# Step 2: Build artifacts
echo ""
echo -e "${GREEN}Step 2/3: Building release artifacts${NC}"
if [[ "$DRY_RUN" == true ]]; then
    echo -e "${BLUE}[DRY-RUN] Would execute: ./build.sh --all${NC}"
else
    ./build.sh --all
fi
echo "✅ Build complete"

# Step 3: Generate release notes and create release
echo ""
echo -e "${GREEN}Step 3/3: Creating GitHub release${NC}"

# Read checksums (or use placeholder for dry run)
if [[ "$DRY_RUN" == true ]]; then
    CHECKSUMS="<checksums would be generated here>"
else
    CHECKSUMS=$(cat release/checksums.txt)
fi

# Use custom notes or auto-generated summary
if [ -n "$CUSTOM_NOTES" ]; then
    SUMMARY_SECTION="$CUSTOM_NOTES"
else
    SUMMARY_SECTION="$AUTO_SUMMARY

### Commits

$COMMIT_LOG"
fi

# Create release notes
RELEASE_NOTES="## What's New

$SUMMARY_SECTION

## Installation

### One-Line Installer (Recommended)

\`\`\`bash
# User-local install (no sudo required)
curl -fsSL https://get.aceteam.ai/citadel.sh | bash

# Or system-wide install
curl -fsSL https://get.aceteam.ai/citadel.sh | sudo bash
\`\`\`

### Manual Installation

Download the appropriate binary for your platform and architecture:

#### Linux

\`\`\`bash
# For amd64
curl -LO https://github.com/aceteam-ai/citadel-cli/releases/download/$VERSION/citadel_${VERSION}_linux_amd64.tar.gz
tar -xzf citadel_${VERSION}_linux_amd64.tar.gz
mv citadel ~/.local/bin/  # or: sudo mv citadel /usr/local/bin/

# For arm64
curl -LO https://github.com/aceteam-ai/citadel-cli/releases/download/$VERSION/citadel_${VERSION}_linux_arm64.tar.gz
tar -xzf citadel_${VERSION}_linux_arm64.tar.gz
mv citadel ~/.local/bin/  # or: sudo mv citadel /usr/local/bin/
\`\`\`

#### macOS

\`\`\`bash
# For Intel Macs (amd64)
curl -LO https://github.com/aceteam-ai/citadel-cli/releases/download/$VERSION/citadel_${VERSION}_darwin_amd64.tar.gz
tar -xzf citadel_${VERSION}_darwin_amd64.tar.gz
mv citadel ~/.local/bin/  # or: sudo mv citadel /usr/local/bin/

# For Apple Silicon (arm64)
curl -LO https://github.com/aceteam-ai/citadel-cli/releases/download/$VERSION/citadel_${VERSION}_darwin_arm64.tar.gz
tar -xzf citadel_${VERSION}_darwin_arm64.tar.gz
mv citadel ~/.local/bin/  # or: sudo mv citadel /usr/local/bin/
\`\`\`

## SHA256 Checksums

\`\`\`
$CHECKSUMS
\`\`\`"

# Create GitHub release
if [[ "$DRY_RUN" == true ]]; then
    echo -e "${BLUE}[DRY-RUN] Would create GitHub release with:${NC}"
    echo "  Title: $VERSION"
    echo "  Assets: linux_amd64, linux_arm64, darwin_amd64, darwin_arm64, windows_amd64, windows_arm64, checksums.txt"
    echo ""
    echo "Release notes preview:"
    echo "----------------------------------------"
    echo "$RELEASE_NOTES" | head -30
    echo "..."
    echo "----------------------------------------"
else
    gh release create "$VERSION" \
      --title "$VERSION" \
      --notes "$RELEASE_NOTES" \
      release/citadel_${VERSION}_linux_amd64.tar.gz \
      release/citadel_${VERSION}_linux_arm64.tar.gz \
      release/citadel_${VERSION}_darwin_amd64.tar.gz \
      release/citadel_${VERSION}_darwin_arm64.tar.gz \
      release/citadel_${VERSION}_windows_amd64.zip \
      release/citadel_${VERSION}_windows_arm64.zip \
      release/checksums.txt
fi

if [[ "$DRY_RUN" == true ]]; then
    echo ""
    echo -e "${GREEN}✅ Dry run complete - no changes were made${NC}"
    echo ""
    echo "To perform the actual release, run:"
    echo "  $0 -v $VERSION -y"
else
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
fi
