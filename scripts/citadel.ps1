# Citadel CLI Installer for Windows
#
# Usage: irm https://get.aceteam.ai/citadel.ps1 | iex
#
# Or download and run:
#   Invoke-WebRequest -Uri https://get.aceteam.ai/citadel.ps1 -OutFile citadel.ps1
#   .\citadel.ps1

# -MachineWide installs to %ProgramFiles%\Citadel with an admin-only ACL
# instead of the default %LOCALAPPDATA%\Citadel, and is what 'citadel up'
# (machine-wide TUN mode, citadel #709) requires: its embedded wintun.dll can
# only be loaded from the directory the running exe lives in, so that
# directory must not be writable by a non-administrator, or a local user
# could plant a DLL that loads into citadel's elevated process. 'citadel
# login' works from EITHER install location -- it needs no driver.
param(
    [string]$Version = "latest",
    [string]$InstallDir = "$env:LOCALAPPDATA\Citadel",
    [switch]$AddToPath = $true,
    [switch]$Force = $false,
    [switch]$MachineWide = $false
)

$ErrorActionPreference = "Stop"

# Only move the default install location for -MachineWide when the caller
# didn't already pass an explicit -InstallDir.
if ($MachineWide -and -not $PSBoundParameters.ContainsKey('InstallDir')) {
    $InstallDir = "$env:ProgramFiles\Citadel"
}

if ($MachineWide) {
    $currentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    if (-not $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Write-Host ""
        Write-Host "-MachineWide needs an elevated (Administrator) PowerShell session:" -ForegroundColor Red
        Write-Host "  it writes to $InstallDir and locks its ACL down to admins only," -ForegroundColor Red
        Write-Host "  which a non-admin session cannot do." -ForegroundColor Red
        Write-Host ""
        Write-Host "Re-run from 'Run as Administrator', or drop -MachineWide to install" -ForegroundColor Yellow
        Write-Host "unprivileged to $env:LOCALAPPDATA\Citadel (citadel login only)." -ForegroundColor Yellow
        exit 1
    }
}

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
    Write-ColorOutput Green "[OK] $Message"
}

function Write-Error($Message) {
    Write-ColorOutput Red "[ERROR] $Message"
}

function Write-Warning($Message) {
    Write-ColorOutput Yellow "[WARN] $Message"
}

# Banner
Write-Host ""
Write-ColorOutput Cyan "+========================================+"
Write-ColorOutput Cyan "|   Citadel CLI Installer for Windows   |"
Write-ColorOutput Cyan "+========================================+"
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
    Write-Host "  irm https://get.aceteam.ai/citadel.ps1 | iex -Force"
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

    # Lock the install directory down to admins-only. This is what makes
    # machine-wide mode's embedded-driver loading safe (citadel #709): the
    # wintun.dll citadel extracts at 'citadel up' time can only be loaded
    # from this same directory, and 'citadel up' refuses to load anything
    # from a directory a non-administrator could write to (see
    # docs/machine-wide-tun.md). icacls SIDs are used instead of names so
    # this works on non-English Windows too.
    if ($MachineWide) {
        Write-Info "Restricting $InstallDir to administrators..."
        # Passed as a plain array element (not manually quoted): PowerShell's
        # native-command argument marshaling already quotes an element
        # containing spaces, and doing it ourselves as well would hand icacls
        # a path with literal quote characters baked in.
        $icaclsArgs = @(
            $InstallDir, "/inheritance:r",
            "/grant:r", "*S-1-5-18:(OI)(CI)F",     # NT AUTHORITY\SYSTEM: Full Control
            "/grant:r", "*S-1-5-32-544:(OI)(CI)F", # BUILTIN\Administrators: Full Control
            "/grant:r", "*S-1-5-32-545:(OI)(CI)RX" # BUILTIN\Users: Read & Execute (citadel login/status stay runnable unprivileged)
        )
        $icaclsResult = & icacls @icaclsArgs 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-Error "Failed to lock down $InstallDir -- machine-wide mode ('citadel up') would refuse to load its driver from here."
            Write-Error "$icaclsResult"
            exit 1
        }

        # /grant above sets the DACL but never reassigns ownership -- an
        # object's owner keeps implicit WRITE_DAC/READ_CONTROL over its own
        # permissions regardless of what the DACL says, so a non-admin owner
        # could still re-DACL this directory later even with the grants
        # above in place. citadel's ACL check now asserts the owner itself
        # is admin-like, independent of the DACL (issue #789,
        # ownerNotAdminLike in internal/network/acl.go) -- set it explicitly
        # so that check passes durably rather than by accident of whichever
        # SID an elevated mkdir happened to assign.
        $ownerResult = & icacls $InstallDir /setowner "*S-1-5-32-544" 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-Error "Failed to set owner of $InstallDir to Administrators -- machine-wide mode ('citadel up') would refuse to load its driver from here."
            Write-Error "$ownerResult"
            exit 1
        }

        Write-Success "Locked $InstallDir to SYSTEM + Administrators (Users: read & execute only)"
    }

    # Verify installation
    $installedVersion = & $targetPath version 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-Success "Verified installation: $installedVersion"
    } else {
        Write-Warning "Installation completed but version check failed"
    }

    # Add to PATH if requested. A machine-wide install goes on the Machine
    # PATH (visible to every user and to services); the default unprivileged
    # install stays on the User PATH, as before.
    if ($AddToPath) {
        $pathScope = if ($MachineWide) { "Machine" } else { "User" }
        $existingPath = [Environment]::GetEnvironmentVariable("Path", $pathScope)

        if ($existingPath -notlike "*$InstallDir*") {
            Write-Info "Adding to $pathScope PATH..."

            $newPath = if ($existingPath) {
                "$existingPath;$InstallDir"
            } else {
                $InstallDir
            }

            [Environment]::SetEnvironmentVariable("Path", $newPath, $pathScope)

            # Update current session
            $env:Path = "$env:Path;$InstallDir"

            Write-Success "Added to $pathScope PATH (restart shell to apply)"
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
if ($MachineWide) {
    Write-Host "This is a machine-wide install: 'citadel up' (puts the whole machine on"
    Write-Host "the network, not just citadel itself) can run from here. From an elevated"
    Write-Host "(Administrator) PowerShell, run: citadel up"
} else {
    Write-Host "This is the unprivileged install: 'citadel login' works as-is. 'citadel up'"
    Write-Host "(machine-wide mode) needs the admin-only install -- re-run this installer"
    Write-Host "with -MachineWide from an elevated PowerShell session to switch, or run"
    Write-Host "'citadel up --check' from here to see exactly what it's missing."
}
Write-Host ""
Write-Host "Documentation: https://github.com/aceteam-ai/citadel-cli"
Write-Host ""
