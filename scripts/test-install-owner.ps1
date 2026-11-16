# Installer ownership regression test (citadel-cli#1695). Run from an ELEVATED
# PowerShell on Windows; it downloads the latest release, so it needs network.
#   - per-user installs (both entrypoints) must leave citadel.exe, and a dir the
#     script created, owned by the running user, not Administrators
#   - an existing install dir is handled (the exe is still re-owned)
#   - -MachineWide must stay Administrators-owned with its restricted DACL
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$userSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$adminSid = "S-1-5-32-544"
$usersSid = "S-1-5-32-545"
$failures = @()

function Get-OwnerSid($path) {
    (Get-Acl -Path $path).GetOwner([System.Security.Principal.SecurityIdentifier]).Value
}
function Check($name, $cond) {
    if ($cond) { Write-Host "ok   $name" } else { Write-Host "FAIL $name"; $script:failures += $name }
}
function Install($script, $extra) {
    $psArgs = @("-NoProfile", "-File", $script, "-Force", "-AddToPath:`$false") + $extra
    & pwsh @psArgs
    if ($LASTEXITCODE -ne 0) { throw "$script exited $LASTEXITCODE" }
}

# Elevated runner: a plain mkdir is Administrators-owned, which is the bug.
$elevated = ([System.Security.Principal.WindowsPrincipal][System.Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $elevated) { throw "run this from an elevated PowerShell" }

$tag = [guid]::NewGuid().ToString("N").Substring(0, 8)
$cleanup = @()
try {
    foreach ($entry in @("scripts\citadel.ps1", "install.ps1")) {
        $script = Join-Path $root $entry

        # New dir created by the script.
        $dir = Join-Path $env:LOCALAPPDATA "CitadelTest-$tag-new-$([IO.Path]::GetFileNameWithoutExtension($entry))"
        $cleanup += $dir
        Install $script @("-InstallDir", $dir)
        Check "$entry new: exe owned by user" ((Get-OwnerSid (Join-Path $dir "citadel.exe")) -eq $userSid)
        Check "$entry new: dir owned by user" ((Get-OwnerSid $dir) -eq $userSid)

        # Existing dir (created elevated, so Administrators-owned) plus reinstall.
        $dir2 = Join-Path $env:LOCALAPPDATA "CitadelTest-$tag-old-$([IO.Path]::GetFileNameWithoutExtension($entry))"
        $cleanup += $dir2
        New-Item -ItemType Directory -Path $dir2 | Out-Null
        Install $script @("-InstallDir", $dir2)
        Check "$entry existing dir: exe owned by user" ((Get-OwnerSid (Join-Path $dir2 "citadel.exe")) -eq $userSid)
        Install $script @("-InstallDir", $dir2)
        Check "$entry reinstall: exe owned by user" ((Get-OwnerSid (Join-Path $dir2 "citadel.exe")) -eq $userSid)
    }

    # Machine-wide contract: unchanged.
    $mw = Join-Path $env:ProgramFiles "CitadelTest-$tag-mw"
    $cleanup += $mw
    Install (Join-Path $root "scripts\citadel.ps1") @("-MachineWide", "-InstallDir", $mw)
    $exe = Join-Path $mw "citadel.exe"
    Check "machine-wide: dir owned by Administrators" ((Get-OwnerSid $mw) -eq $adminSid)
    Check "machine-wide: exe not owned by user" ((Get-OwnerSid $exe) -ne $userSid)
    $acl = Get-Acl -Path $mw
    Check "machine-wide: DACL inheritance removed" $acl.AreAccessRulesProtected
    $fsr = [System.Security.AccessControl.FileSystemRights]
    $writeBits = $fsr::WriteData -bor $fsr::AppendData -bor $fsr::WriteAttributes -bor $fsr::WriteExtendedAttributes -bor $fsr::Delete -bor $fsr::DeleteSubdirectoriesAndFiles -bor $fsr::ChangePermissions -bor $fsr::TakeOwnership
    $bad = @($acl.Access | Where-Object {
        $sid = $_.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value
        ($sid -eq $usersSid -or $sid -eq $userSid) -and $_.AccessControlType -eq "Allow" -and
        (($_.FileSystemRights -band $writeBits) -ne 0)
    })
    $bad | ForEach-Object { Write-Host "  offending: $($_.IdentityReference) $($_.FileSystemRights)" }
    Check "machine-wide: no write grant to Users or the runner" ($bad.Count -eq 0)
}
finally {
    foreach ($d in $cleanup) { if (Test-Path $d) { Remove-Item -Recurse -Force $d -ErrorAction SilentlyContinue } }
}
if ($failures.Count -gt 0) { Write-Host "$($failures.Count) check(s) failed"; exit 1 }
Write-Host "all installer ownership checks passed"
