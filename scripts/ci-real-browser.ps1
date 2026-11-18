# CI helper for the Windows real-browser steps (citadel-cli#1795).
#   - kills browser processes left over from an earlier step
#   - warms the browser once (first-run work, cold disk) with a throwaway profile
#   - runs the packages one at a time (-p 1): the five packages used to launch
#     browsers concurrently on a small runner, which starved the cold launches
#   - retries ONLY the top-level tests that failed, once, and prints both
#     attempts. A second failure fails the step; a build error or a panic
#     (no named failing test) is never retried.
param([Parameter(Mandatory)][string]$Pattern)
$ErrorActionPreference = "Continue"
$pkgs = @("./internal/platform/", "./internal/localcobrowse/", "./internal/localviewer/", "./internal/cobrowsestream/", "./tests/localgate/")

function Stop-Browsers {
    Get-Process -Name chrome, msedge, msedgewebview2, MicrosoftEdgeUpdate -ErrorAction SilentlyContinue |
        ForEach-Object { Write-Host "stopping stale $($_.ProcessName) pid=$($_.Id)"; Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Seconds 2
}

function Invoke-WarmUp {
    $exe = $env:CITADEL_COBROWSE_BROWSER
    if (-not $exe -or -not (Test-Path $exe)) { Write-Host "warm-up skipped: no browser path"; return }
    $profile = Join-Path $env:TEMP "warmup-$([guid]::NewGuid().ToString('N'))"
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $p = Start-Process -FilePath $exe -PassThru -ArgumentList @("--headless=new", "--no-first-run", "--no-default-browser-check", "--user-data-dir=$profile", "--dump-dom", "about:blank")
    if (-not $p.WaitForExit(60000)) { Write-Host "warm-up: browser still running after 60s, killing"; Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
    Write-Host "warm-up finished in $([int]$sw.Elapsed.TotalSeconds)s"
    Stop-Browsers
    Remove-Item -Recurse -Force $profile -ErrorAction SilentlyContinue
}

function Invoke-Attempt($label, $run) {
    Write-Host "::group::$label"
    $log = Join-Path $env:TEMP "gotest-$([guid]::NewGuid().ToString('N')).log"
    & go test @pkgs -p 1 -count=1 -v -run $run 2>&1 | Tee-Object -FilePath $log
    $code = $LASTEXITCODE
    Write-Host "::endgroup::"
    Write-Host "$label exit code: $code"
    $failed = @(Select-String -Path $log -Pattern '^--- FAIL: (\S+)' | ForEach-Object { $_.Matches[0].Groups[1].Value } | Sort-Object -Unique)
    [pscustomobject]@{ Code = $code; Failed = $failed }
}

Stop-Browsers
Invoke-WarmUp
$first = Invoke-Attempt "attempt 1" $Pattern
if ($first.Code -eq 0) { exit 0 }
if ($first.Failed.Count -eq 0) { Write-Host "attempt 1 failed without a named failing test (build error, panic or timeout); not retrying"; exit $first.Code }
Write-Host "attempt 1 FAILED: $($first.Failed -join ', ')"
Write-Host "retrying only those tests once (#1795); both attempts are reported"
Stop-Browsers
$second = Invoke-Attempt "attempt 2 (retry of failed tests)" ("^(" + ($first.Failed -join "|") + ")$")
if ($second.Code -eq 0) { Write-Host "attempt 2 PASSED; attempt 1 had failed: $($first.Failed -join ', ')"; exit 0 }
Write-Host "attempt 2 FAILED too: $($second.Failed -join ', ')"
exit $second.Code
