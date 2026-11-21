# Pure decision logic for ci-real-browser.ps1 (citadel-cli#1795). Dot-sourced by
# the step script and by test-ci-real-browser.ps1.

# Decide, from `go test -json` lines of one attempt, whether a retry is safe.
# Retry only when every failing package is fully accounted for by named
# top-level tests that reported Action "fail". Anything else (a package that
# failed without a failing test, a build failure, a panic, a timeout, output
# that is not JSON) means a retry could hide a real failure, so: no retry.
function Get-RetryDecision {
    param([string[]]$Lines, [int]$ExitCode)
    $failedTests = @{}   # package -> set of top-level test names
    $pkgFailed = @{}     # packages with a package-level fail event
    $fatal = @()
    $pkgSeen = @{}       # packages that emitted any event
    $pkgDone = @{}       # packages with a terminal pass/fail/skip
    $started = @{}       # package|test for top-level tests that began
    $finished = @{}      # package|test with a terminal action
    foreach ($line in $Lines) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        if (-not $line.StartsWith("{")) { $fatal += "non-JSON output: $line"; continue }
        try { $ev = $line | ConvertFrom-Json } catch { $fatal += "unparseable output: $line"; continue }
        $pkg = [string]$ev.Package
        if ($pkg) { $pkgSeen[$pkg] = $true }
        $terminal = $ev.Action -in @("pass", "fail", "skip")
        if ($ev.Test -and -not $ev.Test.Contains("/")) {
            if ($ev.Action -eq "run") { $started["$pkg|$($ev.Test)"] = $true }
            if ($terminal) { $finished["$pkg|$($ev.Test)"] = $true }
        } elseif (-not $ev.Test -and $terminal) { $pkgDone[$pkg] = $true }
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
    # Only a plain test-failure exit (1) is retryable; a kill, signal or tool
    # error exits differently. The stream must also be complete: every package
    # and every started top-level test reached a terminal action.
    if ($ExitCode -ne 1) { return [pscustomobject]@{ Retry = $false; Tests = @(); Reason = "exit code $ExitCode is not a plain test failure (1)" } }
    foreach ($pkg in $pkgSeen.Keys) {
        if (-not $pkgDone.ContainsKey($pkg)) { return [pscustomobject]@{ Retry = $false; Tests = @(); Reason = "package $pkg has no terminal action (interrupted run)" } }
    }
    foreach ($k in $started.Keys) {
        if (-not $finished.ContainsKey($k)) { return [pscustomobject]@{ Retry = $false; Tests = @(); Reason = "test $k started but never finished (interrupted run)" } }
    }
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
