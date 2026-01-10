# Windows Development Setup Guide

This guide helps Windows developers set up their environment to build and develop Citadel CLI.

## Prerequisites

### Required Software

1. **Go 1.24.0 or later**
   - Download from: https://go.dev/dl/
   - Or install via winget: `winget install GoLang.Go`
   - Verify: `go version`

2. **Git**
   - Download from: https://git-scm.com/download/win
   - Or install via winget: `winget install Git.Git`
   - Verify: `git --version`

3. **PowerShell 5.1+ or PowerShell 7+** (comes with Windows)
   - Verify: `$PSVersionTable.PSVersion`
   - For best experience, install PowerShell 7: `winget install Microsoft.PowerShell`

### Optional (for cross-platform builds)

4. **Windows Subsystem for Linux (WSL2)**
   - Install: `wsl --install`
   - Required for: Cross-platform .tar.gz packaging
   - Or use Git Bash as alternative

5. **Docker Desktop** (for testing)
   - Install: `winget install Docker.DockerDesktop`
   - Requires WSL2 backend
   - Used for running/testing Citadel services

## Quick Start

### 1. Clone the Repository

```powershell
git clone https://github.com/aceteam-ai/citadel-cli.git
cd citadel-cli
```

### 2. Build for Windows

Using PowerShell:

```powershell
# Build for current platform (Windows)
.\build.ps1

# Build for all platforms (requires tar for Linux/macOS packages)
.\build.ps1 -All

# Quick development build (no packaging)
go build -o citadel.exe .
```

Using Git Bash (alternative):

```bash
# If you prefer bash-style builds
./build.sh
```

### 3. Run Tests

```powershell
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run platform-specific tests
go test -v ./internal/platform/...
```

### 4. Run Locally

```powershell
# Build first
go build -o citadel.exe .

# Run commands (most require Administrator)
.\citadel.exe --help
.\citadel.exe status
```

## Build Scripts

### PowerShell Build Script (`build.ps1`)

**Native Windows build script** - recommended for Windows developers.

```powershell
# Show help
.\build.ps1 -Help

# Build for Windows only
.\build.ps1

# Build for all platforms
.\build.ps1 -All
```

**Features:**
- ✅ Native PowerShell (no WSL required)
- ✅ Builds Windows .exe binaries
- ✅ Creates .zip packages for Windows
- ✅ SHA256 checksums generation
- ⚠️ Requires tar for cross-platform .tar.gz (optional)

**Output:**
- `build/windows-amd64/citadel.exe` - Windows 64-bit binary
- `release/citadel_VERSION_windows_amd64.zip` - Release package
- `citadel.exe` - Symlink to current platform binary

### Bash Build Script (`build.sh`)

**Unix-style build script** - requires WSL or Git Bash on Windows.

```bash
# Using WSL or Git Bash
./build.sh --all
```

**Requires:**
- WSL (Windows Subsystem for Linux)
- Or Git Bash with Unix tools
- Or MSYS2/Cygwin

## Development Workflow

### Day-to-Day Development

```powershell
# 1. Make changes to code
code .  # Open in VS Code

# 2. Run tests
go test ./internal/platform/...

# 3. Build and test locally
go build -o citadel.exe .
.\citadel.exe --help

# 4. Run full test suite
go test ./...
```

### Building Release Artifacts

```powershell
# Build for all platforms
.\build.ps1 -All

# Check release directory
ls .\release\

# Verify checksums
cat .\release\checksums.txt
```

## Common Issues & Solutions

### Issue: "tar is not recognized"

**Problem:** Cross-platform builds fail when creating .tar.gz packages.

**Solution:**
- Option 1: Install tar (available in Windows 10 1803+)
  ```powershell
  # Tar should be available by default
  tar --version
  ```
- Option 2: Use WSL for cross-platform builds
  ```powershell
  wsl
  cd /mnt/c/path/to/citadel-cli
  ./build.sh --all
  ```
- Option 3: Build Windows-only packages
  ```powershell
  .\build.ps1  # Only builds for Windows
  ```

### Issue: "Cannot create symbolic link"

**Problem:** Symlink creation fails without Administrator privileges.

**Solution:** The script automatically falls back to copying the binary. Or run PowerShell as Administrator:
```powershell
# Right-click PowerShell and "Run as Administrator"
.\build.ps1
```

### Issue: Go module errors

**Problem:** `go: module not found` or dependency issues.

**Solution:**
```powershell
# Update dependencies
go mod download
go mod tidy

# Clear module cache if needed
go clean -modcache
go mod download
```

### Issue: Tests fail on Windows

**Problem:** Windows-specific tests fail.

**Solution:** Ensure you're running as Administrator for tests that require it:
```powershell
# Run as Administrator
go test ./internal/platform/... -v
```

## IDE Setup

### Visual Studio Code

Recommended extensions:

```powershell
# Install VS Code
winget install Microsoft.VisualStudioCode

# Recommended extensions (install via VS Code)
# - Go (golang.go)
# - PowerShell (ms-vscode.powershell)
# - YAML (redhat.vscode-yaml)
```

Settings for Go development:

```json
{
  "go.useLanguageServer": true,
  "go.toolsManagement.autoUpdate": true,
  "go.lintOnSave": "workspace",
  "go.formatTool": "gofmt",
  "go.testFlags": ["-v"]
}
```

### GoLand / IntelliJ IDEA

1. Open project directory
2. Trust Go modules
3. Set Go SDK to 1.24.0+
4. Enable Go modules in Settings → Go → Go Modules

## Platform-Specific Development

### Windows-Specific Code

Windows platform code is in:
- `internal/platform/platform_windows.go` - Windows Admin detection
- Windows-specific implementations in other platform files

**Build tags:**
```go
//go:build windows
// +build windows

package platform
```

### Testing Windows Features

```powershell
# Test Windows Admin detection
go test -v ./internal/platform -run TestIsRoot

# Test Windows package manager
go test -v ./internal/platform -run TestWinget

# Test Windows Docker manager
go test -v ./internal/platform -run TestWindowsDocker
```

## WSL Integration (Optional)

If you want to use WSL for development:

```powershell
# Enter WSL
wsl

# Your Windows files are at /mnt/c/
cd /mnt/c/Users/YourName/citadel-cli

# Use bash build script
./build.sh --all

# Exit WSL
exit
```

**Benefits:**
- Full Linux environment
- Bash scripts work natively
- Better for cross-platform development

**Downsides:**
- Slower file I/O across WSL boundary
- Extra setup required

## Git Configuration

Recommended Git settings for Windows:

```powershell
# Handle line endings properly
git config --global core.autocrlf true

# Use Windows credential manager
git config --global credential.helper manager-core

# Set default branch
git config --global init.defaultBranch main
```

## Performance Tips

### Speed up Go builds

```powershell
# Enable Go build cache (default on)
go env GOCACHE

# Parallel builds (default is # of CPUs)
go env GOMAXPROCS

# Use SSDs for faster builds
# Store code on C:\ or fast drive
```

### Speed up tests

```powershell
# Run tests in parallel
go test ./... -parallel 8

# Skip slow tests during development
go test ./... -short

# Run specific tests
go test ./internal/platform -run TestIsWindows
```

## Debugging

### Debug with VS Code

Create `.vscode/launch.json`:

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Debug Citadel",
      "type": "go",
      "request": "launch",
      "mode": "debug",
      "program": "${workspaceFolder}",
      "args": ["--help"]
    }
  ]
}
```

### Debug with Delve

```powershell
# Install Delve debugger
go install github.com/go-delve/delve/cmd/dlv@latest

# Debug the application
dlv debug . -- --help
```

## CI/CD Considerations

### GitHub Actions

Windows builds run on `windows-latest` runners:

```yaml
- name: Build for Windows
  run: |
    .\build.ps1 -All
  shell: pwsh
```

### Local Testing

Test your changes work on all platforms before committing:

```powershell
# Build all platforms
.\build.ps1 -All

# Run all tests
go test ./...

# Verify checksums
cat .\release\checksums.txt
```

## Getting Help

### Resources
- **Go Documentation**: https://go.dev/doc/
- **PowerShell Docs**: https://docs.microsoft.com/powershell/
- **Project README**: See `README.md`
- **Platform Code**: See `CLAUDE.md` for architecture details

### Community
- Open issues on GitHub for bugs
- Check existing issues for known problems
- See `CONTRIBUTING.md` for contribution guidelines

## Next Steps

1. ✅ Install prerequisites (Go, Git, PowerShell)
2. ✅ Clone repository
3. ✅ Run `.\build.ps1` to build
4. ✅ Run `go test ./...` to verify
5. 🚀 Start developing!

For running Citadel CLI on Windows as a user (not developer), see the main README.md.
