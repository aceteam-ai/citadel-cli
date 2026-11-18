# CI helper for the Windows real-browser steps (citadel-cli#1795).
#   - kills browser processes left over from an earlier step
#   - warms the browser once (first-run work, cold disk) with a throwaway profile
#   - runs the packages one at a time (-p 1): the five packages used to launch
#     browsers concurrently on a small runner, which starved the cold launches
#   - retries ONLY the top-level tests that failed, once, and prints both
#     attempts. Get-RetryDecision (ci-real-browser-lib.ps1, self-tested by
#     test-ci-real-browser.ps1 at the start of every run) refuses to retry when
#     any package failed without a named failing test, or on a build error,
#     panic or timeout. A second failure fails the step.
param([Parameter(Mandatory)][string]$Pattern)
$ErrorActionPreference = "Continue"
. (Join-Path $PSScriptRoot "ci-real-browser-lib.ps1")
& (Join-Path $PSScriptRoot "test-ci-real-browser.ps1")
if ($LASTEXITCODE -ne 0) { exit 1 }
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
    $lines = New-Object System.Collections.Generic.List[string]
    # -json for the retry decision; print the Output fields so the Actions log
    # stays human readable. Everything goes to the host, only the result object
    # is returned.
    & go test @pkgs -p 1 -count=1 -json -run $run 2>&1 | ForEach-Object {
        $line = [string]$_
        $lines.Add($line)
        if ($line.StartsWith("{")) {
            try { $ev = $line | ConvertFrom-Json; if ($ev.Output) { Write-Host -NoNewline $ev.Output } } catch { Write-Host $line }
        } else { Write-Host $line }
    }
    $code = $LASTEXITCODE
    Write-Host "::endgroup::"
    Write-Host "$label exit code: $code"
    [pscustomobject]@{ Code = $code; Lines = $lines.ToArray() }
}

Stop-Browsers
Invoke-WarmUp
$first = Invoke-Attempt "attempt 1" $Pattern
if ($first.Code -eq 0) { exit 0 }
$decision = Get-RetryDecision $first.Lines $first.Code
if (-not $decision.Retry) { Write-Host "attempt 1 failed and is not retryable: $($decision.Reason)"; exit $first.Code }
Write-Host "attempt 1 FAILED: $($decision.Tests -join ', ')"
Write-Host "retrying only those tests once (#1795); both attempts are reported"
Stop-Browsers
$second = Invoke-Attempt "attempt 2 (retry of failed tests)" ("^(" + ($decision.Tests -join "|") + ")$")
if ($second.Code -eq 0) {
    Write-Host "::warning::real-browser test(s) passed only on retry: $($decision.Tests -join ', ')"
    exit 0
}
Write-Host "attempt 2 FAILED too"
exit $second.Code
