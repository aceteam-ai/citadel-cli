# Citadel CLI - Windows Development Environment Setup
# This script sets up everything you need to develop Citadel CLI on Windows
#
# Usage:
#   .\setup-dev-windows.ps1
#
# Or run directly from GitHub:
#   iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/setup-dev-windows.ps1 | iex

param(
    [switch]$SkipBuild = $false,
    [switch]$SkipTests = $false,
    [switch]$Help = $false
)

$ErrorActionPreference = "Stop"

# Colors for output
function Write-ColorOutput($ForegroundColor, $Message) {
    $fc = $host.UI.RawUI.ForegroundColor
    $host.UI.RawUI.ForegroundColor = $ForegroundColor
    Write-Output $Message
    $host.UI.RawUI.ForegroundColor = $fc
}

function Write-Info($Message) {
    Write-ColorOutput Cyan "==> $Message"
}

function Write-Success($Message) {
    Write-ColorOutput Green "✓ $Message"
}

function Write-Error-Msg($Message) {
    Write-ColorOutput Red "✗ $Message"
}

function Write-Warning-Msg($Message) {
    Write-ColorOutput Yellow "⚠ $Message"
}

function Write-Step($Number, $Total, $Message) {
    Write-ColorOutput Cyan "[$Number/$Total] $Message"
}

if ($Help) {
    Write-Host "Citadel CLI - Windows Development Setup"
    Write-Host ""
    Write-Host "This script will:"
    Write-Host "  1. Check for required tools (Go, Git)"
    Write-Host "  2. Install missing tools via winget"
    Write-Host "  3. Clone repository (if not already cloned)"
    Write-Host "  4. Build the project"
    Write-Host "  5. Run tests"
    Write-Host ""
    Write-Host "Options:"
    Write-Host "  -SkipBuild    Skip the build step"
    Write-Host "  -SkipTests    Skip the test step"
    Write-Host "  -Help         Show this help message"
    Write-Host ""
    exit 0
}

# Banner
Write-Host ""
Write-ColorOutput Cyan "╔════════════════════════════════════════════════╗"
Write-ColorOutput Cyan "║   Citadel CLI - Windows Dev Environment Setup ║"
Write-ColorOutput Cyan "╚════════════════════════════════════════════════╝"
Write-Host ""

$totalSteps = 5
$currentStep = 0

# Step 1: Check for winget
$currentStep++
Write-Step $currentStep $totalSteps "Checking for Windows Package Manager (winget)..."

$hasWinget = $false
try {
    $wingetVersion = winget --version 2>&1
    if ($LASTEXITCODE -eq 0) {
        $hasWinget = $true
        Write-Success "Winget is installed: $wingetVersion"
    }
} catch {
    $hasWinget = $false
}

if (-not $hasWinget) {
    Write-Warning-Msg "Winget is not installed"
    Write-Host ""
    Write-Host "Winget is required to install prerequisites automatically."
    Write-Host "Please install winget by upgrading to Windows 10 1809+ or Windows 11,"
    Write-Host "or manually install Go and Git from:"
    Write-Host "  - Go: https://go.dev/dl/"
    Write-Host "  - Git: https://git-scm.com/download/win"
    Write-Host ""
    exit 1
}

# Step 2: Check and install Go
$currentStep++
Write-Step $currentStep $totalSteps "Checking for Go..."

$hasGo = $false
try {
    $goVersion = go version 2>&1
    if ($LASTEXITCODE -eq 0) {
        $hasGo = $true
        Write-Success "Go is installed: $goVersion"
    }
} catch {
    $hasGo = $false
}

if (-not $hasGo) {
    Write-Info "Go not found. Installing via winget..."
    try {
        winget install --id GoLang.Go --silent --accept-package-agreements --accept-source-agreements
        Write-Success "Go installed successfully"

        # Refresh environment
        Write-Info "Refreshing environment variables..."
        $env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path", "User")

        # Verify installation
        $goVersion = go version 2>&1
        if ($LASTEXITCODE -eq 0) {
            Write-Success "Go verified: $goVersion"
        } else {
            Write-Warning-Msg "Go was installed but not found in PATH. Please restart your terminal."
            exit 1
        }
    } catch {
        Write-Error-Msg "Failed to install Go"
        Write-Host "Please install Go manually from: https://go.dev/dl/"
        exit 1
    }
}

# Step 3: Check and install Git
$currentStep++
Write-Step $currentStep $totalSteps "Checking for Git..."

$hasGit = $false
try {
    $gitVersion = git --version 2>&1
    if ($LASTEXITCODE -eq 0) {
        $hasGit = $true
        Write-Success "Git is installed: $gitVersion"
    }
} catch {
    $hasGit = $false
}

if (-not $hasGit) {
    Write-Info "Git not found. Installing via winget..."
    try {
        winget install --id Git.Git --silent --accept-package-agreements --accept-source-agreements
        Write-Success "Git installed successfully"

        # Refresh environment
        Write-Info "Refreshing environment variables..."
        $env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path", "User")

        # Verify installation
        $gitVersion = git --version 2>&1
        if ($LASTEXITCODE -eq 0) {
            Write-Success "Git verified: $gitVersion"
        } else {
            Write-Warning-Msg "Git was installed but not found in PATH. Please restart your terminal."
            exit 1
        }
    } catch {
        Write-Error-Msg "Failed to install Git"
        Write-Host "Please install Git manually from: https://git-scm.com/download/win"
        exit 1
    }
}

# Step 4: Clone or verify repository
$currentStep++
Write-Step $currentStep $totalSteps "Setting up repository..."

$repoUrl = "https://github.com/aceteam-ai/citadel-cli.git"
$repoDir = "citadel-cli"

if (Test-Path ".git") {
    Write-Success "Already in a Git repository"
    $repoDir = "."
} elseif (Test-Path "citadel-cli\.git") {
    Write-Success "Repository already cloned at .\citadel-cli"
    $repoDir = "citadel-cli"
} else {
    Write-Info "Cloning repository from GitHub..."
    try {
        git clone $repoUrl
        Write-Success "Repository cloned successfully"
    } catch {
        Write-Error-Msg "Failed to clone repository"
        exit 1
    }
}

# Change to repo directory
if ($repoDir -ne ".") {
    Set-Location $repoDir
}

Write-Info "Current directory: $(Get-Location)"

# Verify we're in the right place
if (-not (Test-Path "go.mod")) {
    Write-Error-Msg "go.mod not found. Are you in the citadel-cli directory?"
    exit 1
}

Write-Success "Repository ready"

# Step 5: Download Go dependencies
$currentStep++
Write-Step $currentStep $totalSteps "Downloading Go dependencies..."

try {
    go mod download
    Write-Success "Dependencies downloaded"
} catch {
    Write-Error-Msg "Failed to download dependencies"
    exit 1
}

# Step 6: Build the project
if (-not $SkipBuild) {
    Write-Host ""
    Write-ColorOutput Cyan "╔════════════════════════════════════════════════╗"
    Write-ColorOutput Cyan "║                 Building Project               ║"
    Write-ColorOutput Cyan "╚════════════════════════════════════════════════╝"
    Write-Host ""

    Write-Info "Building for Windows..."
    Write-Host ""

    try {
        .\build.ps1
        Write-Host ""
        Write-Success "Build completed successfully!"
    } catch {
        Write-Error-Msg "Build failed"
        exit 1
    }
} else {
    Write-Info "Skipping build (--SkipBuild flag)"
}

# Step 7: Run tests
if (-not $SkipTests) {
    Write-Host ""
    Write-ColorOutput Cyan "╔════════════════════════════════════════════════╗"
    Write-ColorOutput Cyan "║                  Running Tests                 ║"
    Write-ColorOutput Cyan "╚════════════════════════════════════════════════╝"
    Write-Host ""

    Write-Info "Running test suite..."
    Write-Host ""

    try {
        go test ./... -v
        Write-Host ""
        Write-Success "All tests passed!"
    } catch {
        Write-Warning-Msg "Some tests failed"
        Write-Host "This is normal if you don't have Docker or WSL2 installed."
    }
} else {
    Write-Info "Skipping tests (--SkipTests flag)"
}

# Final summary
Write-Host ""
Write-ColorOutput Cyan "╔════════════════════════════════════════════════╗"
Write-ColorOutput Cyan "║              Setup Complete! 🎉                ║"
Write-ColorOutput Cyan "╚════════════════════════════════════════════════╝"
Write-Host ""

Write-Host "Development environment is ready!"
Write-Host ""
Write-Host "What you can do now:"
Write-Host "  1. Build the project:       .\build.ps1"
Write-Host "  2. Run tests:               go test ./..."
Write-Host "  3. Run citadel locally:     .\citadel.exe --help"
Write-Host "  4. Make changes and rebuild"
Write-Host ""

# Check for built binary
if (Test-Path "citadel.exe") {
    Write-Host "Binary location: $(Resolve-Path citadel.exe)"
    Write-Host ""
    Write-Info "Testing binary..."
    try {
        $version = .\citadel.exe version 2>&1
        Write-Host "  $version"
    } catch {
        Write-Warning-Msg "Binary exists but failed to run"
    }
    Write-Host ""
}

Write-Host "Quick Commands:"
Write-Host "  .\build.ps1              # Build for current platform"
Write-Host "  .\build.ps1 -All         # Build for all platforms"
Write-Host "  go test ./...            # Run all tests"
Write-Host "  go test -v ./internal/platform  # Test platform code"
Write-Host "  .\citadel.exe status     # Test the CLI"
Write-Host ""

Write-Host "Documentation:"
Write-Host "  README.md                # General overview"
Write-Host "  WINDOWS_DEVELOPMENT.md   # Windows-specific guide"
Write-Host "  CLAUDE.md                # Architecture details"
Write-Host ""

Write-Success "Happy coding!"
Write-Host ""
