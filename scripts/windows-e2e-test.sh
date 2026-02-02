#!/usr/bin/env bash
#
# Windows E2E Test Runner for citadel-cli
#
# Tests the full first-time user experience on a Windows machine via WinRM:
#   clean → install → init → verify
#
# Prerequisites:
#   - pywinrm: pip install pywinrm
#   - WinRM enabled on the target (see README for setup commands)
#
# Usage:
#   ./scripts/windows-e2e-test.sh --host 192.168.2.207 --user aceteam --password SECRET
#   ./scripts/windows-e2e-test.sh clean --host 192.168.2.207 ...
#   ./scripts/windows-e2e-test.sh --skip-clean --host 192.168.2.207 ...
#   ./scripts/windows-e2e-test.sh --dry-run --host 192.168.2.207 ...

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WINRM_CMD="$SCRIPT_DIR/winrm-cmd.py"

# --- Defaults ---
HOST="${WINRM_HOST:-192.168.2.207}"
USER="${WINRM_USER:-}"
PASSWORD="${WINRM_PASSWORD:-}"
AUTHKEY="${CITADEL_AUTHKEY:-}"
VERSION=""
SKIP_CLEAN=false
DRY_RUN=false
PHASE=""  # empty = run all phases

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

# --- Counters ---
PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0

usage() {
    cat <<EOF
Usage: $(basename "$0") [PHASE] [OPTIONS]

Phases (run individually or omit for all):
  clean       Reset Windows machine to fresh state
  install     Download and install citadel binary
  provision   Run citadel init to bootstrap everything
  verify      Check that everything works

Options:
  --host HOST         WinRM host (default: 192.168.2.207, or \$WINRM_HOST)
  --user USER         WinRM username (or \$WINRM_USER)
  --password PASS     WinRM password (or \$WINRM_PASSWORD)
  --authkey KEY       Nexus authkey for citadel init (or \$CITADEL_AUTHKEY)
  --version VER       Citadel version to install (e.g., v2.3.0)
  --skip-clean        Skip the clean phase when running all phases
  --dry-run           Show commands without executing
  -h, --help          Show this help
EOF
    exit 0
}

# --- Argument parsing ---
while [[ $# -gt 0 ]]; do
    case "$1" in
        clean|install|provision|verify)
            PHASE="$1"; shift ;;
        --host)       HOST="$2"; shift 2 ;;
        --user)       USER="$2"; shift 2 ;;
        --password)   PASSWORD="$2"; shift 2 ;;
        --authkey)    AUTHKEY="$2"; shift 2 ;;
        --version)    VERSION="$2"; shift 2 ;;
        --skip-clean) SKIP_CLEAN=true; shift ;;
        --dry-run)    DRY_RUN=true; shift ;;
        -h|--help)    usage ;;
        *) echo "Unknown option: $1" >&2; usage ;;
    esac
done

# --- Validation ---
if [[ -z "$USER" ]]; then
    echo -e "${RED}Error: --user or \$WINRM_USER is required${NC}" >&2
    exit 1
fi
if [[ -z "$PASSWORD" ]]; then
    echo -e "${RED}Error: --password or \$WINRM_PASSWORD is required${NC}" >&2
    exit 1
fi

# --- Ensure pywinrm is available ---
ensure_pywinrm() {
    if ! python3 -c "import winrm" 2>/dev/null; then
        echo -e "${YELLOW}Installing pywinrm...${NC}"
        pip install pywinrm >/dev/null 2>&1
        if ! python3 -c "import winrm" 2>/dev/null; then
            echo -e "${RED}Failed to install pywinrm. Run: pip install pywinrm${NC}" >&2
            exit 1
        fi
    fi
}

# --- Remote execution helper ---
run_remote() {
    local description="$1"
    local command="$2"
    local allow_fail="${3:-false}"

    if [[ "$DRY_RUN" == "true" ]]; then
        echo -e "  ${BLUE}[DRY RUN]${NC} $description"
        echo "    PS> $command"
        return 0
    fi

    echo -e "  ${BLUE}→${NC} $description"
    local output
    local exit_code=0
    output=$(python3 "$WINRM_CMD" "$HOST" "$USER" "$PASSWORD" "$command" 2>&1) || exit_code=$?

    if [[ -n "$output" ]]; then
        echo "$output" | sed 's/^/    /'
    fi

    if [[ $exit_code -ne 0 && "$allow_fail" != "true" ]]; then
        echo -e "  ${RED}✗ FAILED${NC} (exit code $exit_code)"
        return $exit_code
    fi

    return 0
}

# --- Test assertion ---
check() {
    local description="$1"
    local command="$2"

    if [[ "$DRY_RUN" == "true" ]]; then
        echo -e "  ${BLUE}[DRY RUN]${NC} Check: $description"
        echo "    PS> $command"
        ((SKIP_COUNT++))
        return 0
    fi

    echo -ne "  ${BLUE}→${NC} Check: $description ... "
    local output
    local exit_code=0
    output=$(python3 "$WINRM_CMD" "$HOST" "$USER" "$PASSWORD" "$command" 2>&1) || exit_code=$?

    if [[ $exit_code -eq 0 ]]; then
        echo -e "${GREEN}PASS${NC}"
        if [[ -n "$output" ]]; then
            echo "$output" | head -5 | sed 's/^/    /'
        fi
        ((PASS_COUNT++))
    else
        echo -e "${RED}FAIL${NC}"
        if [[ -n "$output" ]]; then
            echo "$output" | head -10 | sed 's/^/    /'
        fi
        ((FAIL_COUNT++))
    fi
}

# --- Phase implementations ---

phase_clean() {
    echo -e "\n${BOLD}═══ Phase 1: Clean ═══${NC}"
    echo -e "Resetting ${HOST} to fresh state...\n"

    # Uninstall Docker Desktop (if installed)
    run_remote "Uninstall Docker Desktop" \
        'winget uninstall Docker.DockerDesktop --force --silent 2>$null; $true' true

    # Stop Docker Desktop process
    run_remote "Stop Docker Desktop process" \
        'Stop-Process -Name "Docker Desktop" -Force -ErrorAction SilentlyContinue; $true' true

    # Remove Citadel install directory
    run_remote "Remove Citadel install directory" \
        'if (Test-Path "$env:LOCALAPPDATA\Citadel") { Remove-Item -Recurse -Force "$env:LOCALAPPDATA\Citadel" }; Write-Output "Done"' true

    # Remove citadel-node config directory
    run_remote "Remove citadel-node config" \
        'if (Test-Path "$env:USERPROFILE\citadel-node") { Remove-Item -Recurse -Force "$env:USERPROFILE\citadel-node" }; Write-Output "Done"' true

    # Remove .citadel-node network state
    run_remote "Remove citadel network state" \
        'if (Test-Path "$env:USERPROFILE\.citadel-node") { Remove-Item -Recurse -Force "$env:USERPROFILE\.citadel-node" }; Write-Output "Done"' true

    # Remove citadel from user PATH
    run_remote "Remove Citadel from PATH" \
        '
        $citadelPath = "$env:LOCALAPPDATA\Citadel"
        $currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
        if ($currentPath -and $currentPath.Contains($citadelPath)) {
            $newPath = ($currentPath.Split(";") | Where-Object { $_ -ne $citadelPath }) -join ";"
            [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
            Write-Output "Removed $citadelPath from PATH"
        } else {
            Write-Output "Citadel not found in PATH"
        }
        ' true

    # Optionally unregister WSL Ubuntu
    run_remote "Unregister WSL Ubuntu (if present)" \
        '
        $wslList = wsl --list --quiet 2>$null
        if ($wslList -match "Ubuntu") {
            wsl --unregister Ubuntu 2>$null
            Write-Output "Unregistered Ubuntu WSL distribution"
        } else {
            Write-Output "No Ubuntu WSL distribution found"
        }
        ' true

    echo -e "\n${GREEN}Clean phase complete.${NC}"
}

phase_install() {
    echo -e "\n${BOLD}═══ Phase 2: Install ═══${NC}"
    echo -e "Installing citadel binary on ${HOST}...\n"

    local install_cmd
    if [[ -n "$VERSION" ]]; then
        install_cmd="\$env:CITADEL_VERSION='$VERSION'; iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/install.ps1 | iex"
    else
        install_cmd="iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/install.ps1 | iex"
    fi

    run_remote "Download and run install.ps1" "$install_cmd"

    # Refresh PATH and verify installation
    check "citadel binary installed" \
        '
        $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
        $citadel = Get-Command citadel -ErrorAction SilentlyContinue
        if ($citadel) {
            Write-Output "Found: $($citadel.Source)"
            citadel version
        } else {
            # Check common install location directly
            $directPath = "$env:LOCALAPPDATA\Citadel\citadel.exe"
            if (Test-Path $directPath) {
                Write-Output "Found at: $directPath"
                & $directPath version
            } else {
                Write-Error "citadel not found"
                exit 1
            }
        }
        '

    echo -e "\n${GREEN}Install phase complete.${NC}"
}

phase_provision() {
    echo -e "\n${BOLD}═══ Phase 3: Provision ═══${NC}"
    echo -e "Running citadel init on ${HOST}...\n"

    if [[ -z "$AUTHKEY" ]]; then
        echo -e "${YELLOW}Warning: No --authkey provided. citadel init will use device authorization flow.${NC}"
        echo -e "${YELLOW}For non-interactive testing, provide --authkey or set \$CITADEL_AUTHKEY.${NC}"
    fi

    local init_cmd='
        $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
        $citadel = "$env:LOCALAPPDATA\Citadel\citadel.exe"
        if (-not (Test-Path $citadel)) {
            $citadel = "citadel"
        }
    '

    if [[ -n "$AUTHKEY" ]]; then
        init_cmd+="& \$citadel init --authkey '$AUTHKEY'"
    else
        init_cmd+='& $citadel init'
    fi

    # citadel init can take a long time (Docker Desktop install + startup)
    # The WinRM command itself doesn't have a timeout, but we document the expectation
    run_remote "Run citadel init (this may take several minutes)" "$init_cmd"

    echo -e "\n${GREEN}Provision phase complete.${NC}"
}

phase_verify() {
    echo -e "\n${BOLD}═══ Phase 4: Verify ═══${NC}"
    echo -e "Verifying installation on ${HOST}...\n"

    # Helper to ensure PATH is refreshed in each check
    local path_refresh='$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")'

    check "citadel version" \
        "$path_refresh; citadel version"

    check "citadel status" \
        "$path_refresh; citadel status"

    check "Docker is running" \
        "$path_refresh; docker version"

    check "citadel node directory exists" \
        'if (Test-Path "$env:USERPROFILE\citadel-node\citadel.yaml") { Get-Content "$env:USERPROFILE\citadel-node\citadel.yaml" | Select-Object -First 5 } else { Write-Error "citadel.yaml not found"; exit 1 }'

    # This check may fail if authkey wasn't provided or network isn't configured
    check "citadel nodes (network connectivity)" \
        "$path_refresh; citadel nodes --nexus https://nexus.aceteam.ai"

    echo -e "\n${GREEN}Verify phase complete.${NC}"
}

# --- Summary ---
print_summary() {
    echo -e "\n${BOLD}═══ Results ═══${NC}"
    echo -e "  ${GREEN}Passed:${NC}  $PASS_COUNT"
    echo -e "  ${RED}Failed:${NC}  $FAIL_COUNT"
    if [[ $SKIP_COUNT -gt 0 ]]; then
        echo -e "  ${YELLOW}Skipped:${NC} $SKIP_COUNT (dry run)"
    fi
    local total=$((PASS_COUNT + FAIL_COUNT))
    if [[ $total -gt 0 ]]; then
        echo -e "  ${BOLD}Total:${NC}   $total"
    fi

    if [[ $FAIL_COUNT -gt 0 ]]; then
        echo -e "\n${RED}Some checks failed.${NC}"
        return 1
    elif [[ $total -gt 0 ]]; then
        echo -e "\n${GREEN}All checks passed.${NC}"
    fi
    return 0
}

# --- Main ---
main() {
    echo -e "${BOLD}Citadel Windows E2E Test${NC}"
    echo -e "Target: ${HOST} (user: ${USER})"
    if [[ -n "$VERSION" ]]; then
        echo -e "Version: ${VERSION}"
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        echo -e "${YELLOW}(Dry run mode — no commands will be executed)${NC}"
    fi

    ensure_pywinrm

    # Test WinRM connectivity
    echo -e "\n${BLUE}Testing WinRM connectivity...${NC}"
    if [[ "$DRY_RUN" != "true" ]]; then
        if ! python3 "$WINRM_CMD" "$HOST" "$USER" "$PASSWORD" 'Write-Output "WinRM OK: $(hostname)"' 2>/dev/null; then
            echo -e "${RED}Cannot connect to ${HOST} via WinRM. Ensure WinRM is enabled.${NC}" >&2
            exit 1
        fi
        echo -e "${GREEN}Connected.${NC}"
    fi

    if [[ -n "$PHASE" ]]; then
        # Run single phase
        case "$PHASE" in
            clean)     phase_clean ;;
            install)   phase_install ;;
            provision) phase_provision ;;
            verify)    phase_verify ;;
        esac
    else
        # Run all phases
        if [[ "$SKIP_CLEAN" != "true" ]]; then
            phase_clean
        else
            echo -e "\n${YELLOW}Skipping clean phase (--skip-clean)${NC}"
        fi
        phase_install
        phase_provision
        phase_verify
    fi

    print_summary
}

main
