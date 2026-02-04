---
sidebar_position: 1
title: Contributing
---

# Contributing

This guide covers how to set up a development environment, build the project, and submit changes to the Citadel CLI codebase.

## Prerequisites

- **Go 1.23+** -- the project uses modern Go features and module conventions
- **Docker** -- required for integration tests and running services locally
- **git** -- for version control

A Nix flake is available for a reproducible development environment. If you use Nix, `nix develop` will provide all required tools.

## Clone and Build

```bash
git clone https://github.com/aceteam-ai/citadel-cli.git
cd citadel-cli
./build.sh
```

The `build.sh` script builds for the current platform and places the binary in `./build/`.

For a quick development build without packaging:

```bash
go build -o citadel .
```

## Running Tests

```bash
# All tests
go test ./...

# Verbose output
go test -v ./...

# Specific test
go test -v ./cmd -run TestReadManifest
```

See the [Testing](./testing.md) page for integration and E2E test details.

## Git Workflow

**Never push directly to main.** This is a hard rule with no exceptions.

### Starting new work

If you are on the `main` branch, create a feature branch first:

```bash
git checkout -b feat/my-new-feature
# or
git checkout -b fix/describe-the-bug
```

### Working on a feature branch

If you are already on a feature branch, commit and push directly to that branch:

```bash
git add <files>
git commit -m "description of change"
git push
```

Do not create additional branches off of a feature branch.

### Pull requests

Only create a PR when explicitly asked. After pushing your branch, inform the reviewer and wait for instructions.

```bash
# Only when requested:
gh pr create --title "feat: description" --body "..."
```

## Code Style

- Run `gofmt` before committing. The codebase follows standard Go formatting.
- Handle errors explicitly. Do not discard errors with `_` unless there is a documented reason.
- Add comments to all exported functions and types.
- Use the naming conventions established in the existing code (e.g., `internal/` packages follow the standard Go project layout).

## Documentation

- Update the README for user-facing changes (new commands, changed flags, new behavior).
- Update CLAUDE.md when you discover or introduce new architecture patterns, important implementation details, or development conventions.

## Future Work

Create GitHub issues for planned improvements instead of leaving TODO comments in the code. This keeps all planned work visible, trackable, and prioritizable.

```bash
gh issue create --title "feat: description of improvement" --body "Context and details..."
```
