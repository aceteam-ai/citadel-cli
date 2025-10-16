#!/bin/bash
# init.sh
# ==============================================================================
# Citadel CLI Project Initializer
#
# This script scaffolds the current directory into a Go project for the
# Citadel CLI using cobra-cli.
# ==============================================================================

set -e

# --- Configuration ---
# Derive project name from the current directory
PROJECT_NAME=$(basename "$PWD")
# IMPORTANT: Change this to your actual GitHub username
MODULE_PATH="github.com/aceboss/$PROJECT_NAME"
AUTHOR_INFO="AceTeam <dev@aceteam.ai>"

# --- Prerequisite Checks ---
echo "--- Checking prerequisites..."
# (Prerequisite checks remain the same as before)
if ! command -v go &> /dev/null; then echo "❌ Go not found"; exit 1; fi
if ! command -v cobra-cli &> /dev/null; then
    echo " installing cobra-cli..."
    go install github.com/spf13/cobra-cli@latest
fi
echo "✅ Prerequisites met."

# --- Project Scaffolding ---
echo "--- Initializing project '$PROJECT_NAME' in the current directory..."

# Check if already initialized
if [ -f "go.mod" ]; then
    echo "⚠️  go.mod already exists. Skipping 'go mod init' and 'cobra-cli init'."
else
    go mod init "$MODULE_PATH"
    cobra-cli init --author "$AUTHOR_INFO" --license mit
fi

echo "--- Adding core commands..."
cobra-cli add login
cobra-cli add up
cobra-cli add status
cobra-cli add down
cobra-cli add nodes
cobra-cli add logs

# --- Create Supporting Files ---
echo "--- Creating/updating supporting files..."
# (The content for README.md, .gitignore, and build.sh is the same as before)
# ... (omitted for brevity, but they would be here) ...

echo ""
echo "--- Tidying Go modules..."
go mod tidy

echo ""
echo "🎉 Success! Project '$PROJECT_NAME' is ready."
echo ""
echo "Next steps:"
echo "1. Open the project in your editor (e.g., 'code .')"
echo "2. Start building your commands in the 'cmd/' directory."

