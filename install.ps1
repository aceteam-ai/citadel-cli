# Citadel CLI Installer for Windows
# Usage: iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/install.ps1 | iex
#
# Or download and run:
#   Invoke-WebRequest -Uri https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/install.ps1 -OutFile install.ps1
#   .\install.ps1

param(
    [string]$Version = "latest",
    [string]$InstallDir = "$env:LOCALAPPDATA\Citadel",
    [switch]$AddToPath = $true,
    [switch]$Force = $false
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

function Write-Error($Message) {
    Write-ColorOutput Red "✗ $Message"
}

function Write-Warning($Message) {
    Write-ColorOutput Yellow "⚠ $Message"
}

# Banner
Write-Host ""
Write-ColorOutput Cyan "╔════════════════════════════════════════╗"
Write-ColorOutput Cyan "║   Citadel CLI Installer for Windows   ║"
Write-ColorOutput Cyan "╔════════════════════════════════════════╝"
Write-Host ""

# Detect architecture
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") {
    "arm64"
} elseif ([Environment]::Is64BitOperatingSystem) {
    "amd64"
} else {
    Write-Error "32-bit Windows is not supported"
    exit 1
}

Write-Info "Detected architecture: windows_$arch"

# Check for existing installation
$existingCitadel = Get-Command citadel -ErrorAction SilentlyContinue
if ($existingCitadel -and -not $Force) {
    Write-Warning "Citadel is already installed at: $($existingCitadel.Source)"
    Write-Host ""
    Write-Host "To reinstall, run with -Force flag:"
    Write-Host "  iwr -useb <url> | iex -Force"
    Write-Host ""
    $continue = Read-Host "Continue anyway? (y/N)"
    if ($continue -ne 'y' -and $continue -ne 'Y') {
        Write-Info "Installation cancelled"
        exit 0
    }
}

# Determine version to install
if ($Version -eq "latest") {
    Write-Info "Fetching latest release version..."
    try {
        $latestRelease = Invoke-RestMethod -Uri "https://api.github.com/repos/aceteam-ai/citadel-cli/releases/latest"
        $Version = $latestRelease.tag_name
        Write-Success "Latest version: $Version"
    } catch {
        Write-Error "Failed to fetch latest release version"
        Write-Error $_.Exception.Message
        exit 1
    }
} else {
    Write-Info "Installing version: $Version"
}

# Construct download URL
$downloadUrl = "https://github.com/aceteam-ai/citadel-cli/releases/download/$Version/citadel_${Version}_windows_${arch}.zip"
Write-Info "Download URL: $downloadUrl"

# Create temporary directory
$tempDir = Join-Path $env:TEMP "citadel-install-$(Get-Random)"
New-Item -ItemType Directory -Path $tempDir | Out-Null
Write-Info "Using temp directory: $tempDir"

try {
    # Download release
    $zipPath = Join-Path $tempDir "citadel.zip"
    Write-Info "Downloading Citadel CLI..."

    try {
        Invoke-WebRequest -Uri $downloadUrl -OutFile $zipPath -UseBasicParsing
        Write-Success "Downloaded successfully"
    } catch {
        Write-Error "Failed to download from $downloadUrl"
        Write-Error $_.Exception.Message
        Write-Host ""
        Write-Host "Possible issues:"
        Write-Host "  - Version $Version does not exist"
        Write-Host "  - Network connection problem"
        Write-Host "  - GitHub is unreachable"
        Write-Host ""
        Write-Host "Check available releases at:"
        Write-Host "  https://github.com/aceteam-ai/citadel-cli/releases"
        exit 1
    }

    # Extract archive
    Write-Info "Extracting archive..."
    $extractPath = Join-Path $tempDir "extracted"
    Expand-Archive -Path $zipPath -DestinationPath $extractPath -Force

    # Find the citadel.exe in extracted files
    $citadelExe = Get-ChildItem -Path $extractPath -Filter "citadel.exe" -Recurse | Select-Object -First 1

    if (-not $citadelExe) {
        Write-Error "citadel.exe not found in downloaded archive"
        exit 1
    }

    Write-Success "Extracted successfully"

    # Create install directory
    Write-Info "Installing to: $InstallDir"
    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    # Copy binary
    $targetPath = Join-Path $InstallDir "citadel.exe"
    Copy-Item -Path $citadelExe.FullName -Destination $targetPath -Force
    Write-Success "Installed citadel.exe"

    # Verify installation
    $installedVersion = & $targetPath version 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-Success "Verified installation: $installedVersion"
    } else {
        Write-Warning "Installation completed but version check failed"
    }

    # Add to PATH if requested
    if ($AddToPath) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")

        if ($userPath -notlike "*$InstallDir*") {
            Write-Info "Adding to user PATH..."

            $newPath = if ($userPath) {
                "$userPath;$InstallDir"
            } else {
                $InstallDir
            }

            [Environment]::SetEnvironmentVariable("Path", $newPath, "User")

            # Update current session
            $env:Path = "$env:Path;$InstallDir"

            Write-Success "Added to PATH (restart shell to apply)"
        } else {
            Write-Info "Already in PATH"
        }
    }

} finally {
    # Cleanup
    Write-Info "Cleaning up temporary files..."
    Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}

# Success message
Write-Host ""
Write-Success "Citadel CLI installation complete!"
Write-Host ""
Write-Host "Installation location: $InstallDir"
Write-Host ""
Write-Host "Next steps:"
Write-Host "  1. Restart your terminal (or run: refreshenv)"
Write-Host "  2. Verify installation: citadel version"
Write-Host "  3. Get help: citadel --help"
Write-Host ""
Write-Host "To provision a new node:"
Write-Host "  - Open PowerShell as Administrator"
Write-Host "  - Run: citadel init"
Write-Host ""
Write-Host "Documentation: https://github.com/aceteam-ai/citadel-cli"
Write-Host ""
