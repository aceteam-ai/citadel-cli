---
sidebar_position: 5
title: Releasing
---

# Releasing

Citadel releases are managed through the `release.sh` script. Always use this script -- never create tags or GitHub releases manually.

## Creating a Release

### Interactive

```bash
./release.sh
```

The script prompts for the version number and confirms before proceeding.

### Non-Interactive

```bash
./release.sh -v v1.2.0 -y
```

The `-y` flag skips confirmation prompts.

### Dry Run

Preview what will happen without making any changes:

```bash
./release.sh --dry-run -v v1.2.0
```

## What the Release Script Does

1. **Validates the environment** -- checks that the working tree is clean, required tools are installed, and the version tag does not already exist.
2. **Creates and pushes the git tag** -- tags the current commit with the specified version.
3. **Builds for all platforms** -- produces binaries for 6 targets:
   - `linux/amd64`
   - `linux/arm64`
   - `darwin/amd64`
   - `darwin/arm64`
   - `windows/amd64`
   - `windows/arm64`
4. **Creates the GitHub release** -- uploads all platform binaries as release assets.
5. **Updates the Homebrew tap** -- pushes the new formula to `aceteam-ai/homebrew-tap` so users can install via `brew install aceteam-ai/tap/citadel`.

## Version Format

Versions follow [semantic versioning](https://semver.org/):

- **Release**: `vX.Y.Z` (e.g., `v2.3.0`)
- **Pre-release**: `vX.Y.Z-rc1` (e.g., `v2.3.0-rc1`)

The `v` prefix is required.

## Version Injection

The `build.sh` script injects the version into the binary at build time using Go linker flags:

```bash
go build -ldflags="-X 'github.com/aceteam-ai/citadel-cli/cmd.Version=v2.3.0'" -o citadel .
```

The version is stored in the `Version` variable in `cmd/version.go` and displayed by `citadel version`.

## Build Script

For development builds (current platform only):

```bash
./build.sh
```

For release builds (all platforms):

```bash
./build.sh --all
```

Binaries are placed in the `./build/` directory. Linux and macOS builds are packaged as `.tar.gz` archives; Windows builds are packaged as `.zip` files.
