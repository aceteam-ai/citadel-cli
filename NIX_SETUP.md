# Nix Development Environment Setup

This repository includes Nix flake configuration for a reproducible development environment.

## Prerequisites

### Install Nix (if not already installed)

On Ubuntu/macOS:
```bash
sh <(curl -L https://nixos.org/nix/install) --daemon
```

### Enable Flakes

Add to `~/.config/nix/nix.conf` (create if it doesn't exist):
```
experimental-features = nix-command flakes
```

Or for multi-user installations, add to `/etc/nix/nix.conf`:
```
experimental-features = nix-command flakes
```

### Install direnv

**On Ubuntu:**
```bash
sudo apt install direnv
```

**On macOS:**
```bash
brew install direnv
```

**Setup shell hook** - Add to your shell config (`~/.bashrc`, `~/.zshrc`, etc.):
```bash
eval "$(direnv hook bash)"  # or zsh, fish, etc.
```

Then restart your shell or run `source ~/.bashrc`.

## Usage

### Quick Start

1. Clone the repository and navigate to it:
```bash
cd /path/to/citadel-cli
```

2. Allow direnv to load the environment:
```bash
direnv allow
```

That's it! The development environment with Go 1.24, Docker, Tailscale, and all build tools will be automatically loaded.

### What Gets Installed

The Nix flake provides:
- **Go 1.24** - Exact version specified in go.mod
- **Go tools** - gopls, go-tools, gotools for development
- **Docker & Docker Compose** - Container management
- **Tailscale** - Network mesh tooling
- **Build utilities** - git, make, tree, curl, wget, jq

### Useful Commands

After `direnv allow`, you can use all the standard commands:

```bash
# Build release binaries
./build.sh

# Quick local build
go build -o citadel .

# Run tests
go test ./...

# Run citadel commands
go run . status
go run . --help
```

### Building the Citadel Package

You can also build the Citadel CLI as a Nix package:

```bash
# Build the package
nix build

# Run the built package
./result/bin/citadel --version
```

### Updating Dependencies

When you update go.mod, you may need to update the `vendorHash` in `flake.nix`. Run:

```bash
nix build 2>&1 | grep "got:" | awk '{print $2}'
```

Then update the `vendorHash` value in `flake.nix` with the printed hash.

## Troubleshooting

### "experimental features not enabled"
Enable flakes in your Nix configuration (see Prerequisites above).

### Docker daemon not running
The Nix environment provides the Docker CLI, but you still need the Docker daemon running:
```bash
sudo systemctl start docker  # On Linux with systemd
```

### direnv not loading automatically
Make sure you've added the direnv hook to your shell config and restarted your shell.

### Go version mismatch
If you see Go version errors, ensure you're in the direnv-managed directory and check:
```bash
which go  # Should point to Nix store path
go version  # Should show go1.24.x
```
