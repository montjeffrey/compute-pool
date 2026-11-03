# Deploys (or removes) an App Control policy built by build-policy.ps1.
# Changes a Windows security setting, so run it yourself from an elevated
# PowerShell; pool never runs it. The policy applies right away, without a
# reboot. Do it in this order:
#
#   deploy-policy.ps1 out\compute-pool-audit.cip      # 1. audit: logs, blocks nothing
#   (run your signed pool.exe, and anything else jobs will run)
#   check-audit.ps1 -Since <time printed in step 1>   # 2. no would-block events
#   deploy-policy.ps1 out\compute-pool-enforce.cip -Program bin\pool.exe
#                                                     # 3. enforce: replaces the audit one
#   deploy-policy.ps1 -Remove out\compute-pool-policy.xml
#
# Step 3 refuses unless the same policy was deployed in audit mode by this
# script (recorded under -StateKey, which only administrators can write)
# and is still active, -Program is signed by the policy's signer,
# and check-audit.ps1 finds no event for -Match since the audit deployment.
# -Force skips those checks. No event can also mean the program didn't run,
# so run the signed build at least once in audit mode.
#
# A policy can only add restrictions: while Smart App Control is on, it still
# blocks whatever it blocked before. The policy takes Smart App Control's
# place once you turn that off.
#Requires -RunAsAdministrator
param(
    [Parameter(Mandatory, Position = 0)][string]$PolicyFile,
    [switch]$Remove,
    [string]$Program,
    [string]$Match = 'pool',
    [switch]$Force,
    [string]$StateKey = 'HKLM:\SOFTWARE\compute-pool\appcontrol',
    [string]$CiTool = (Join-Path $env:windir 'System32\CiTool.exe')
)
$ErrorActionPreference = 'Stop'

# build-policy.ps1 writes the XML next to both .cip files.
$xmlFile = if ($PolicyFile -like '*.xml') { $PolicyFile } else { Join-Path (Split-Path -Parent (Resolve-Path $PolicyFile)) 'compute-pool-policy.xml' }
if (-not (Test-Path $xmlFile)) { throw "no policy XML at $xmlFile; keep the files build-policy.ps1 wrote together" }
[xml]$policy = Get-Content $xmlFile
$id = $policy.SiPolicy.PolicyID
$name = @($policy.SiPolicy.Settings.Setting | Where-Object ValueName -eq 'Name')[0].Value.String
$state = Join-Path $StateKey $id.Trim('{}')

function Invoke-CiTool {
    $out = & $CiTool @args 2>&1
    if ($LASTEXITCODE) { throw "CiTool $($args -join ' ') failed ($LASTEXITCODE): $out" }
    $out
}
$rollback = "deploy-policy.ps1 -Remove `"$((Resolve-Path $xmlFile).Path)`"   (or: CiTool --remove-policy $id)"

if ($Remove) {
    Invoke-CiTool --remove-policy $id | Out-Null
    if (Test-Path $state) { Remove-Item $state -Recurse }
    "Removed $name $id."
    return
}

$mode = if ($PolicyFile -like '*-audit.cip') { 'audit' } elseif ($PolicyFile -like '*-enforce.cip') { 'enforce' } else {
    throw "expected compute-pool-audit.cip or compute-pool-enforce.cip, not $PolicyFile"
}
if ($mode -eq 'enforce' -and -not $Force) {
    if (-not (Test-Path $state)) { throw "deploy compute-pool-audit.cip from the same folder first (no audit record for $id); -Force skips this" }
    $audit = Get-ItemProperty $state
    if ($audit.Mode -ne 'audit') { throw "policy $id is already $($audit.Mode); -Force to redeploy" }
    if (-not ((Invoke-CiTool --list-policies) -match [regex]::Escape($id.Trim('{}')))) {
        throw "policy $id isn't active any more; deploy compute-pool-audit.cip again"
    }
    if (-not $Program) { throw "-Program <your signed pool.exe> is required, to check it is signed by this policy's signer" }
    $sig = Get-AuthenticodeSignature -FilePath $Program
    $signer = if ($sig.SignerCertificate) { $sig.SignerCertificate.GetNameInfo([Security.Cryptography.X509Certificates.X509NameType]::SimpleName, $false) }
    if ($sig.Status -notin 'Valid', 'UnknownError' -or $signer -notin @($policy.SiPolicy.Signers.Signer | ForEach-Object Name)) {
        throw "$Program is $($sig.Status), signed by '$signer', which this policy doesn't trust"
    }
    $since = [datetime]::Parse($audit.DeployedAt, [Globalization.CultureInfo]::InvariantCulture, 'RoundtripKind').ToLocalTime()
    & (Join-Path $PSScriptRoot 'check-audit.ps1') -Match $Match -Since $since -Policy $name
    switch ($LASTEXITCODE) {
        0 { }
        1 { throw "App Control logged the events above since the audit policy went in at $since; fix them or -Force" }
        default { throw "check-audit.ps1 couldn't read the logs (exit $LASTEXITCODE); -Force to deploy anyway" }
    }
}

$now = (Get-Date).ToUniversalTime()
Invoke-CiTool --update-policy (Resolve-Path $PolicyFile).Path | Out-Null
New-Item -Force $state | Out-Null
Set-ItemProperty $state -Name Mode -Value $mode
Set-ItemProperty $state -Name DeployedAt -Value $now.ToString('o')
"Deployed $name $id in $mode mode at $($now.ToLocalTime())."
if ($mode -eq 'audit') { "Next: run your signed pool.exe, then check-audit.ps1 -Since '$($now.ToLocalTime())'." }
"To undo: $rollback"
