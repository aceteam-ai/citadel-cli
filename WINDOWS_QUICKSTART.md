# Windows Development - Quick Start Guide

Get started developing Citadel CLI on Windows in **under 5 minutes**.

## One-Command Setup

Open PowerShell and run:

```powershell
iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/setup-dev-windows.ps1 | iex
```

This will automatically:
- ✅ Check for Go and Git (install if missing via winget)
- ✅ Clone the repository
- ✅ Download dependencies
- ✅ Build the project
- ✅ Run tests

**Done!** You're ready to start coding.

## What Just Happened?

The setup script:

1. **Checked for winget** (Windows Package Manager)
2. **Installed Go** (if not already installed)
3. **Installed Git** (if not already installed)
4. **Cloned repository** to `.\citadel-cli\`
5. **Downloaded Go dependencies** with `go mod download`
6. **Built the project** with `.\build.ps1`
7. **Ran tests** with `go test ./...`

## Next Steps

### Basic Development Workflow

```powershell
# Navigate to repo (if you ran from elsewhere)
cd citadel-cli

# Make your changes in your editor
code .  # Or use your preferred editor

# Build
.\build.ps1

# Test
go test ./...

# Run locally
.\citadel.exe --help
```

### Common Commands

```powershell
# Build for Windows only
.\build.ps1

# Build for all platforms (Linux, macOS, Windows)
.\build.ps1 -All

# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run specific package tests
go test -v ./internal/platform

# Quick development build (no packaging)
go build -o citadel.exe .

# Check version
.\citadel.exe version
```

## Development Tips

### IDE Setup

**VS Code (Recommended)**
```powershell
# Install VS Code
winget install Microsoft.VisualStudioCode

# Open project
code .

# Install Go extension
# Search for "Go" in Extensions (Ctrl+Shift+X)
```

**GoLand**
- Full-featured Go IDE
- Just open the folder and it auto-detects everything

### File Watching and Auto-Rebuild

For rapid development, you can use a file watcher:

```powershell
# Install Air (live reload for Go)
go install github.com/cosmtrek/air@latest

# Run with auto-reload
air
```

### Debugging

**VS Code Debugging:**

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
      "args": ["status"]
    }
  ]
}
```

Press F5 to start debugging!

### Testing Windows-Specific Code

```powershell
# Test platform abstractions
go test -v ./internal/platform

# Test Windows-specific functions
go test -v ./internal/platform -run TestWindows
```

## Troubleshooting

### "winget not found"

Upgrade to Windows 10 1809+ or Windows 11, or install manually:
- Go: https://go.dev/dl/
- Git: https://git-scm.com/download/win

### "go not found" after installation

Restart PowerShell to reload PATH:
```powershell
# Or refresh environment
$env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path", "User")
```

### Tests fail

Some tests require Docker/WSL2 and will skip if not available. This is normal.

To run only basic tests:
```powershell
go test -short ./...
```

### Build fails

```powershell
# Clean and retry
go clean -cache
go mod download
go mod tidy
.\build.ps1
```

## Manual Setup (Alternative)

If you prefer manual setup:

```powershell
# 1. Install prerequisites
winget install GoLang.Go
winget install Git.Git

# 2. Clone repository
git clone https://github.com/aceteam-ai/citadel-cli.git
cd citadel-cli

# 3. Download dependencies
go mod download

# 4. Build
.\build.ps1

# 5. Test
go test ./...
```

## What to Work On?

Check out:
- [Good First Issues](https://github.com/aceteam-ai/citadel-cli/labels/good%20first%20issue)
- [CONTRIBUTING.md](CONTRIBUTING.md) - Contribution guidelines
- [CLAUDE.md](CLAUDE.md) - Architecture overview

## Need More Details?

See the complete Windows development guide:
- [WINDOWS_DEVELOPMENT.md](WINDOWS_DEVELOPMENT.md) - Full setup guide
- [README.md](README.md) - Project overview

---

**Questions?** Open an issue or discussion on GitHub!
