# Pure decision logic for ci-real-browser.ps1 (citadel-cli#1795). Dot-sourced by
# the step script and by test-ci-real-browser.ps1.

# Decide, from `go test -json` lines of one attempt, whether a retry is safe.
# Retry only when every failing package is fully accounted for by named
# top-level tests that reported Action "fail". Anything else (a package that
# failed without a failing test, a build failure, a panic, a timeout, output
# that is not JSON) means a retry could hide a real failure, so: no retry.
function Get-RetryDecision {
    param([string[]]$Lines)
    $failedTests = @{}   # package -> set of top-level test names
    $pkgFailed = @{}     # packages with a package-level fail event
    $fatal = @()
    foreach ($line in $Lines) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        if (-not $line.StartsWith("{")) { $fatal += "non-JSON output: $line"; continue }
        try { $ev = $line | ConvertFrom-Json } catch { $fatal += "unparseable output: $line"; continue }
        $pkg = [string]$ev.Package
        if ($ev.Output -and ($ev.Output -match '^panic: ' -or $ev.Output -match 'test timed out after' -or $ev.Output -match '\[build failed\]' -or $ev.Output -match '^fatal error: ')) {
            $fatal += "$pkg : $($ev.Output.Trim())"
        }
        if ($ev.Action -eq "fail") {
            if ($ev.Test) {
                if (-not $ev.Test.Contains("/")) {
                    if (-not $failedTests.ContainsKey($pkg)) { $failedTests[$pkg] = @() }
                    $failedTests[$pkg] += [string]$ev.Test
                }
            } else {
                $pkgFailed[$pkg] = $true
            }
        }
    }
    if ($fatal.Count -gt 0) { return [pscustomobject]@{ Retry = $false; Tests = @(); Reason = "fatal output: $($fatal[0])" } }
    if ($pkgFailed.Count -eq 0 -and $failedTests.Count -eq 0) { return [pscustomobject]@{ Retry = $false; Tests = @(); Reason = "no failure recorded" } }
    foreach ($pkg in $pkgFailed.Keys) {
        if (-not $failedTests.ContainsKey($pkg)) { return [pscustomobject]@{ Retry = $false; Tests = @(); Reason = "package $pkg failed with no failing test" } }
    }
    foreach ($pkg in $failedTests.Keys) {
        if (-not $pkgFailed.ContainsKey($pkg)) { return [pscustomobject]@{ Retry = $false; Tests = @(); Reason = "package $pkg has failing tests but no package fail event" } }
    }
    $names = @($failedTests.Values | ForEach-Object { $_ } | Sort-Object -Unique)
    [pscustomobject]@{ Retry = $true; Tests = $names; Reason = "all failures are named top-level tests" }
}
