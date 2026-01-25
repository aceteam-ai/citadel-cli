# build.ps1
# PowerShell build script for Citadel CLI on Windows
# This script builds the Citadel CLI for common architectures and packages them for release.
#
# Usage:
#   .\build.ps1              # Build for current platform only
#   .\build.ps1 -All         # Build for all platforms (linux/darwin/windows, amd64/arm64)

param(
    [switch]$All,
    [switch]$Help
)

if ($Help) {
    Write-Host "Usage: .\build.ps1 [OPTIONS]"
    Write-Host ""
    Write-Host "Build the Citadel CLI binary."
    Write-Host ""
    Write-Host "Options:"
    Write-Host "  -All        Build for all platforms (linux/darwin/windows, amd64/arm64)"
    Write-Host "  -Help       Show this help message"
    Write-Host ""
    Write-Host "By default, builds only for the current platform."
    exit 0
}

Write-Host "--- Building and Packaging Citadel CLI..."

# --- Configuration ---
$VERSION = (git describe --tags --always --dirty 2>$null)
if (-not $VERSION) {
    $VERSION = "dev"
}

$BUILD_DIR = "build"
$RELEASE_DIR = "release"
$MODULE_PATH = (go list -m)
$VERSION_VAR_PATH = "${MODULE_PATH}/cmd.version"

# --- Clean Up ---
if (Test-Path $BUILD_DIR) {
    Remove-Item -Recurse -Force $BUILD_DIR
}
if (Test-Path $RELEASE_DIR) {
    Remove-Item -Recurse -Force $RELEASE_DIR
}
New-Item -ItemType Directory -Path $BUILD_DIR | Out-Null
New-Item -ItemType Directory -Path $RELEASE_DIR | Out-Null
Write-Host "--- Cleaned old build and release directories ---"

# --- Detect Current Platform ---
$CURRENT_OS = "windows"  # We're on Windows
$CURRENT_ARCH = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "386" }

# ARM64 detection (Windows 11 on ARM)
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") {
    $CURRENT_ARCH = "arm64"
}

# --- Determine Build Targets ---
if ($All) {
    Write-Host "--- Building for all platforms (--All flag detected) ---"
    $PLATFORMS = @("linux", "darwin", "windows")
    $ARCHS = @("amd64", "arm64")
} else {
    Write-Host "--- Building for current platform only: $CURRENT_OS/$CURRENT_ARCH ---"
    Write-Host "    (Use -All flag to build for all platforms)"
    $PLATFORMS = @($CURRENT_OS)
    $ARCHS = @($CURRENT_ARCH)
}

# --- Build and Package Loop ---
foreach ($OS in $PLATFORMS) {
    foreach ($ARCH in $ARCHS) {
        Write-Host ""
        Write-Host "--- Processing $OS/$ARCH ---"

        # Define paths and names
        $PLATFORM_DIR = Join-Path $BUILD_DIR "${OS}-${ARCH}"

        # Define binary name with .exe for Windows
        $BINARY_NAME = "citadel"
        if ($OS -eq "windows") {
            $BINARY_NAME = "citadel.exe"
        }

        $BINARY_PATH = Join-Path $PLATFORM_DIR $BINARY_NAME

        New-Item -ItemType Directory -Path $PLATFORM_DIR -Force | Out-Null

        # 1. Build the binary
        Write-Host "Building binary..."
        $env:GOOS = $OS
        $env:GOARCH = $ARCH
        $env:CGO_ENABLED = "0"

        $ldflags = "-X '$VERSION_VAR_PATH=$VERSION'"
        go build -ldflags $ldflags -o $BINARY_PATH ./cmd/citadel

        if ($LASTEXITCODE -ne 0) {
            Write-Error "Build failed for $OS/$ARCH"
            exit 1
        }

        # 2. Package (Windows uses .zip, others use .tar.gz)
        if ($OS -eq "windows") {
            $RELEASE_NAME = "citadel_${VERSION}_${OS}_${ARCH}.zip"
            $RELEASE_PATH = Join-Path $RELEASE_DIR $RELEASE_NAME
            Write-Host "Packaging to $RELEASE_NAME..."

            # Use Compress-Archive (PowerShell built-in)
            Compress-Archive -Path $BINARY_PATH -DestinationPath $RELEASE_PATH -Force
        } else {
            $RELEASE_NAME = "citadel_${VERSION}_${OS}_${ARCH}.tar.gz"
            $RELEASE_PATH = Join-Path $RELEASE_DIR $RELEASE_NAME
            Write-Host "Packaging to $RELEASE_NAME..."

            # Use tar (available in Windows 10+ and PowerShell 7+)
            Push-Location $PLATFORM_DIR
            tar -czf (Resolve-Path $RELEASE_PATH).Path citadel
            Pop-Location

            if ($LASTEXITCODE -ne 0) {
                Write-Warning "tar not available - skipping .tar.gz packaging for $OS/$ARCH"
                Write-Warning "Install tar or use WSL/Git Bash for cross-platform builds"
            }
        }
    }
}

# --- Create Symlink for Current Platform ---
$CURRENT_BINARY = Join-Path $BUILD_DIR "${CURRENT_OS}-${CURRENT_ARCH}" "citadel.exe"
if (Test-Path $CURRENT_BINARY) {
    # Remove old symlink/file if exists
    if (Test-Path "citadel.exe") {
        Remove-Item "citadel.exe" -Force
    }

    # Create symlink (requires Admin) or copy as fallback
    try {
        New-Item -ItemType SymbolicLink -Path "citadel.exe" -Target $CURRENT_BINARY -Force -ErrorAction Stop | Out-Null
        Write-Host ""
        Write-Host "--- Created symlink: citadel.exe -> $CURRENT_BINARY ---"
    } catch {
        # Fallback to copy if not running as Admin
        Copy-Item $CURRENT_BINARY "citadel.exe" -Force
        Write-Host ""
        Write-Host "--- Copied binary: citadel.exe (symlink requires Admin) ---"
    }
}

# --- Generate Checksums ---
Write-Host ""
Write-Host "--- Generating Checksums ---"

$checksumFile = Join-Path $RELEASE_DIR "checksums.txt"
$checksums = @()

Get-ChildItem -Path $RELEASE_DIR -Filter "*.tar.gz" -ErrorAction SilentlyContinue | ForEach-Object {
    $hash = (Get-FileHash -Path $_.FullName -Algorithm SHA256).Hash.ToLower()
    $checksums += "$hash  $($_.Name)"
}

Get-ChildItem -Path $RELEASE_DIR -Filter "*.zip" -ErrorAction SilentlyContinue | ForEach-Object {
    $hash = (Get-FileHash -Path $_.FullName -Algorithm SHA256).Hash.ToLower()
    $checksums += "$hash  $($_.Name)"
}

$checksums | Out-File -FilePath $checksumFile -Encoding ASCII

Write-Host "✅ Build and packaging complete."
Write-Host ""
Write-Host "Binaries for local use are in: '.\$BUILD_DIR'"
Get-ChildItem -Path $BUILD_DIR -Recurse -File | ForEach-Object {
    Write-Host "  $($_.FullName.Replace((Get-Location).Path + '\', ''))"
}

Write-Host ""
Write-Host "Release artifacts are in: '.\$RELEASE_DIR'"
Get-ChildItem -Path $RELEASE_DIR -File | ForEach-Object {
    Write-Host "  $($_.Name)"
}

Write-Host ""
Write-Host "📋 SHA256 Checksums (copy this into your release notes):"
Write-Host "----------------------------------------------------"
Get-Content $checksumFile
Write-Host "----------------------------------------------------"
