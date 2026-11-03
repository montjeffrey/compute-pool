# Lists App Control decisions about files matching -Match since -Since.
# From Microsoft-Windows-CodeIntegrity/Operational:
#   3076 / 3077  a policy in audit mode would have blocked the program / an
#                enforced policy blocked it
#   3034 / 3033  the same, for a file that didn't meet a required signing
#                level
# From Microsoft-Windows-AppLocker/MSI and Script (App Control runs
# unapproved PowerShell scripts in Constrained Language Mode):
#   8028 / 8029  a script would have been blocked / was blocked
# With the audit policy deployed, a signed pool.exe should get none of these
# from "compute-pool allow list", and an unsigned one should.
#
# Exit code: 0 no matching event, 1 at least one (so it can gate a script),
# 2 a log couldn't be read (that is not the same as "no events").
#
# -Policy keeps only events from that policy (events that name no policy,
# like 3033, are always kept), so another product's policy doesn't count.
#
#   pwsh scripts/appcontrol/check-audit.ps1 [-Match pool] [-Since (Get-Date).AddHours(-1)] [-Policy 'compute-pool allow list']
param(
    [string]$Match = 'pool',
    [datetime]$Since = (Get-Date).AddHours(-1),
    [string]$Policy
)
$ErrorActionPreference = 'Stop'
$logs = @(
    @{ LogName = 'Microsoft-Windows-CodeIntegrity/Operational'; Id = 3076, 3077, 3033, 3034 }
    @{ LogName = 'Microsoft-Windows-AppLocker/MSI and Script'; Id = 8028, 8029 }
)
$labels = @{
    3076 = 'would block (audit)'; 3077 = 'blocked'
    3034 = 'would block, signing level (audit)'; 3033 = 'blocked, signing level'
    8028 = 'script would be blocked (audit)'; 8029 = 'script blocked'
}

# Every name/value pair an event carries, from EventData or UserData.
function Get-EventFields($e) {
    $x = [xml]$e.ToXml()
    $f = [ordered]@{}
    foreach ($d in @($x.Event.EventData.Data)) { if ($d -and $d.Name) { $f[$d.Name] = $d.'#text' } }
    if ($x.Event.UserData) {
        foreach ($n in $x.Event.UserData.SelectNodes('.//*[not(*)]')) { $f[$n.LocalName] = $n.InnerText }
    }
    $f
}

$unreadable = @()
$rows = foreach ($log in $logs) {
    try {
        $events = Get-WinEvent -FilterHashtable (@{ StartTime = $Since } + $log)
    } catch {
        if ($_.FullyQualifiedErrorId -like 'NoMatchingEventsFound*') { continue }
        $unreadable += "$($log.LogName): $($_.Exception.Message)"
        continue
    }
    foreach ($e in $events) {
        $f = Get-EventFields $e
        $file = @('File Name', 'FileNameBuffer', 'FilePath', 'FullFilePath' | ForEach-Object { $f[$_] } | Where-Object { $_ })[0]
        # Without a known file field, match any value the event carries.
        $haystack = if ($file) { $file } else { $f.Values -join ' ' }
        if ($haystack -notmatch $Match) { continue }
        if ($Policy -and $f['PolicyName'] -and $f['PolicyName'] -ne $Policy) { continue }
        [pscustomobject]@{
            Time     = $e.TimeCreated
            Event    = "$($e.Id) $($labels[$e.Id])"
            Policy   = $f['PolicyName']
            File     = $haystack -replace '^\\Device\\HarddiskVolume\d+', ''
            Activity = $e.ActivityId
        }
    }
}
# Event 3089 records each signature Windows found on the file, under the
# same activity ID: shown as the signer, so a signed build that is still
# blocked shows which certificate Windows saw.
$signers = @{}
if ($rows) {
    try {
        foreach ($s in Get-WinEvent -FilterHashtable @{ LogName = $logs[0].LogName; Id = 3089; StartTime = $Since }) {
            $sf = Get-EventFields $s
            if (-not ($s.ActivityId -and $sf['PublisherName'])) { continue }
            $k = "$($s.ActivityId)"
            if (-not $signers.ContainsKey($k)) { $signers[$k] = @() }
            $signers[$k] += $sf['PublisherName']
        }
    } catch {
        if ($_.FullyQualifiedErrorId -notlike 'NoMatchingEventsFound*') { Write-Warning "can't read signature details (3089): $($_.Exception.Message)" }
    }
}
$rows = foreach ($r in $rows) {
    $who = @($signers["$($r.Activity)"] | Select-Object -Unique) -join '; '
    [pscustomobject]@{ Time = $r.Time; Event = $r.Event; Policy = $r.Policy; File = $r.File; Signer = $(if ($who) { $who } else { '(none recorded)' }) }
}
foreach ($u in $unreadable) { Write-Warning "can't read $u" }
if ($rows) {
    $rows | Sort-Object Time | Format-Table -AutoSize -Wrap | Out-String -Width 200
    exit 1
}
if ($unreadable) { exit 2 }
"No App Control events for '$Match' since $Since."
exit 0
