# Winget Manifest for Citadel CLI

This directory contains the Windows Package Manager (winget) manifest for Citadel CLI.

## Installation (for users)

Once published to the winget repository, users can install Citadel with:

```powershell
winget install AceTeam.Citadel
```

Or upgrade existing installation:

```powershell
winget upgrade AceTeam.Citadel
```

## Publishing to Winget (for maintainers)

### Prerequisites

1. Install winget-create:
   ```powershell
   winget install Microsoft.WingetCreate
   ```

2. Fork the winget-pkgs repository:
   - Go to https://github.com/microsoft/winget-pkgs
   - Click "Fork"

### Automated Submission (Recommended)

The easiest way is to use wingetcreate to automatically submit:

```powershell
# For a new release (replace v1.3.0 with actual version)
wingetcreate update AceTeam.Citadel `
  --version 1.3.0 `
  --urls https://github.com/aceteam-ai/citadel-cli/releases/download/v1.3.0/citadel_v1.3.0_windows_amd64.zip `
         https://github.com/aceteam-ai/citadel-cli/releases/download/v1.3.0/citadel_v1.3.0_windows_arm64.zip `
  --submit
```

This will:
1. Download the installers
2. Calculate SHA256 checksums
3. Update the manifest
4. Create a PR to microsoft/winget-pkgs

### Manual Submission

If you prefer manual control:

1. **Update manifest files** in this directory for new version:
   - Update `PackageVersion` in all three files
   - Update `InstallerUrl` with new version
   - Update SHA256 checksums (get from release checksums.txt)
   - Update `ReleaseDate`

2. **Clone winget-pkgs repository**:
   ```powershell
   git clone https://github.com/YOUR_USERNAME/winget-pkgs
   cd winget-pkgs
   ```

3. **Create version directory**:
   ```powershell
   mkdir -p manifests/a/AceTeam/Citadel/1.3.0
   ```

4. **Copy manifest files**:
   ```powershell
   cp .winget/*.yaml manifests/a/AceTeam/Citadel/1.3.0/
   ```

5. **Validate manifests**:
   ```powershell
   winget validate manifests/a/AceTeam/Citadel/1.3.0
   ```

6. **Commit and create PR**:
   ```bash
   git checkout -b citadel-1.3.0
   git add manifests/a/AceTeam/Citadel/1.3.0
   git commit -m "New version: AceTeam.Citadel version 1.3.0"
   git push origin citadel-1.3.0
   ```

7. Create PR at https://github.com/microsoft/winget-pkgs/pulls

### Getting SHA256 Checksums

After building a release, checksums are in `release/checksums.txt`:

```powershell
# Or calculate manually:
Get-FileHash citadel_v1.3.0_windows_amd64.zip -Algorithm SHA256
```

## First-Time Package Submission

For the first submission of a new package:

1. Use `wingetcreate new`:
   ```powershell
   wingetcreate new https://github.com/aceteam-ai/citadel-cli/releases/download/v1.3.0/citadel_v1.3.0_windows_amd64.zip
   ```

2. Follow the interactive prompts

3. Review and submit the generated manifest

## Manifest Structure

- **AceTeam.Citadel.yaml** - Version metadata
- **AceTeam.Citadel.installer.yaml** - Installer configuration
- **AceTeam.Citadel.locale.en-US.yaml** - Package description and metadata

## Resources

- [Winget Documentation](https://docs.microsoft.com/en-us/windows/package-manager/)
- [Manifest Schema](https://github.com/microsoft/winget-cli/tree/master/schemas/JSON/manifests)
- [Submission Guidelines](https://github.com/microsoft/winget-pkgs/blob/master/CONTRIBUTING.md)
