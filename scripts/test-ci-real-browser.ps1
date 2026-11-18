# Regression test for Get-RetryDecision. Run by ci-real-browser.ps1 before the
# browser tests, so it executes in the Windows co-browse job (#1795).
. (Join-Path $PSScriptRoot "ci-real-browser-lib.ps1")
$fail = @()
function J($o) { $o | ConvertTo-Json -Compress }
function Check($name, $got, $wantRetry) {
    if ($got.Retry -eq $wantRetry) { Write-Host "ok   $name ($($got.Reason))" } else { Write-Host "FAIL $name : got Retry=$($got.Retry) ($($got.Reason))"; $script:fail += $name }
}
function D($lines, $code = 1) { Get-RetryDecision $lines $code }
$namedA = @(
    (J @{Action="run"; Package="p/a"; Test="TestX"}),
    (J @{Action="fail"; Package="p/a"; Test="TestX/sub"}),
    (J @{Action="fail"; Package="p/a"; Test="TestX"}),
    (J @{Action="fail"; Package="p/a"})
)
$d = D $namedA
Check "named failure only retries" $d $true
if (($d.Tests -join ",") -ne "TestX") { Write-Host "FAIL subtest leaked into retry set: $($d.Tests -join ',')"; $fail += "retry set" }

$timeoutB = @(
    (J @{Action="output"; Package="p/b"; Output="panic: test timed out after 10m0s`n"}),
    (J @{Action="fail"; Package="p/b"})
)
Check "named failure plus another package timing out must not retry" (D ($namedA + $timeoutB)) $false

$hungNoText = @((J @{Action="fail"; Package="p/c"}))
Check "package fail without a failing test must not retry" (D ($namedA + $hungNoText)) $false
Check "panic must not retry" (D ($namedA + @((J @{Action="output"; Package="p/d"; Output="panic: boom"})))) $false
Check "build failure text must not retry" (D ($namedA + @("# p/e", "p/e/x.go:1:1: syntax error"))) $false
Check "no failure recorded must not retry" (D @((J @{Action="pass"; Package="p/a"}))) $false
$interrupted = @(
    (J @{Action="run"; Package="p/a"; Test="TestX"}),
    (J @{Action="fail"; Package="p/a"; Test="TestX"}),
    (J @{Action="fail"; Package="p/a"}),
    (J @{Action="run"; Package="p/b"; Test="TestKilled"}),
    (J @{Action="output"; Package="p/b"; Test="TestKilled"; Output="=== RUN   TestKilled`n"})
)
Check "named failure plus an interrupted package must not retry" (D $interrupted) $false
Check "unexpected exit code must not retry" (D $namedA 2) $false
Check "killed exit code must not retry" (D $namedA -1) $false
Check "started test that never finished must not retry" (D ($namedA + @((J @{Action="run"; Package="p/f"; Test="TestHang"}), (J @{Action="pass"; Package="p/f"})))) $false
if ($fail.Count -gt 0) { Write-Host "self-test FAILED: $($fail -join '; ')"; exit 1 }
exit 0
