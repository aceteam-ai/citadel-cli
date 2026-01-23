# Homebrew Tap for Citadel CLI

This directory contains the Homebrew formula template for Citadel CLI.

## Installation (for users)

Install Citadel via our Homebrew tap:

```bash
brew tap aceteam-ai/tap
brew install citadel
```

Or as a one-liner:

```bash
brew install aceteam-ai/tap/citadel
```

Upgrade to the latest version:

```bash
brew upgrade citadel
```

## Publishing to Homebrew Tap (for maintainers)

### Repository Structure

The actual Homebrew tap lives in a separate repository: `aceteam-ai/homebrew-tap`

```
homebrew-tap/
├── README.md           # User-facing tap documentation
└── Formula/
    └── citadel.rb      # Actual formula (copy from template, fill in version/hashes)
```

### Updating the Formula

After creating a new release with `release.sh`, update the Homebrew tap:

1. **Get the SHA256 checksums** from the release output or `release/checksums.txt`:

   ```bash
   # From checksums.txt
   grep darwin release/checksums.txt
   ```

2. **Update the formula** in `aceteam-ai/homebrew-tap`:

   ```ruby
   # In Formula/citadel.rb, update:
   version "X.Y.Z"  # New version (without 'v' prefix)

   # For Apple Silicon (arm64)
   url "https://github.com/aceteam-ai/citadel-cli/releases/download/vX.Y.Z/citadel_vX.Y.Z_darwin_arm64.tar.gz"
   sha256 "ARM64_SHA256_HERE"

   # For Intel Macs (amd64)
   url "https://github.com/aceteam-ai/citadel-cli/releases/download/vX.Y.Z/citadel_vX.Y.Z_darwin_amd64.tar.gz"
   sha256 "AMD64_SHA256_HERE"
   ```

3. **Commit and push** the formula update:

   ```bash
   cd /path/to/homebrew-tap
   git add Formula/citadel.rb
   git commit -m "citadel X.Y.Z"
   git push origin main
   ```

4. **Verify the update**:

   ```bash
   brew update
   brew upgrade citadel
   citadel version
   ```

### First-Time Tap Setup

To create the `aceteam-ai/homebrew-tap` repository:

1. **Create the repository** on GitHub:
   - Repository name: `homebrew-tap` (must start with `homebrew-`)
   - Make it public

2. **Initialize the structure**:

   ```bash
   mkdir -p Formula
   cp /path/to/citadel-cli/.homebrew/citadel.rb.template Formula/citadel.rb
   # Edit Formula/citadel.rb with actual version and SHA256 values
   ```

3. **Add a README.md**:

   ```markdown
   # AceTeam Homebrew Tap

   Homebrew formulae for AceTeam tools.

   ## Installation

   ```bash
   brew tap aceteam-ai/tap
   brew install citadel
   ```

   ## Available Formulae

   - `citadel` - On-premise agent for the AceTeam Sovereign Compute Fabric
   ```

4. **Commit and push**:

   ```bash
   git add .
   git commit -m "Initial tap setup with citadel formula"
   git push origin main
   ```

### Validating the Formula

Before publishing, validate the formula locally:

```bash
# Audit the formula
brew audit --strict Formula/citadel.rb

# Test installation
brew install --build-from-source Formula/citadel.rb
citadel version

# Clean up
brew uninstall citadel
```

## Formula Template

See `citadel.rb.template` in this directory for the formula structure with VERSION placeholders.

## Resources

- [Homebrew Formula Cookbook](https://docs.brew.sh/Formula-Cookbook)
- [Homebrew Tap Documentation](https://docs.brew.sh/Taps)
- [Formula API Documentation](https://rubydoc.brew.sh/Formula)
