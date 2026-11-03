# Lists App Control decisions about programs matching -Match since -Since,
# from Event Viewer (Microsoft-Windows-CodeIntegrity/Operational):
#   3076  a policy in audit mode would have blocked the program
#   3077  an enforced policy blocked it
# With the audit policy deployed, a signed pool.exe should get no 3076 from
# "compute-pool allow list", and an unsigned one should.
#
#   pwsh scripts/appcontrol/check-audit.ps1 [-Match pool] [-Since (Get-Date).AddHours(-1)]
param(
    [string]$Match = 'pool',
    [datetime]$Since = (Get-Date).AddHours(-1)
)
$events = Get-WinEvent -ErrorAction SilentlyContinue -FilterHashtable @{
    LogName = 'Microsoft-Windows-CodeIntegrity/Operational'; Id = 3076, 3077; StartTime = $Since }
$rows = foreach ($e in $events) {
    $d = @{}
    ([xml]$e.ToXml()).Event.EventData.Data | ForEach-Object { $d[$_.Name] = $_.'#text' }
    if ($d['File Name'] -notmatch $Match) { continue }
    [pscustomobject]@{
        Time   = $e.TimeCreated
        Event  = if ($e.Id -eq 3076) { 'would block (audit)' } else { 'blocked' }
        Policy = $d['PolicyName']
        File   = $d['File Name'] -replace '^\\Device\\HarddiskVolume\d+', ''
    }
}
if ($rows) { $rows | Sort-Object Time | Format-Table -AutoSize -Wrap } else { "No App Control events for '$Match' since $Since." }
