# Deploys (or removes) an App Control policy built by build-policy.ps1.
# Changes a Windows security setting, so run it yourself from an elevated
# PowerShell. The policy applies right away, without a reboot.
#
#   deploy-policy.ps1 compute-pool-audit.cip     # audit: logs, blocks nothing
#   deploy-policy.ps1 compute-pool-enforce.cip   # enforce: replaces the audit one
#   deploy-policy.ps1 -Remove compute-pool-policy.xml
#
# Enforcing only works with Smart App Control off: Smart App Control can't be
# extended, and a program must pass every enforced policy.
#Requires -RunAsAdministrator
param(
    [Parameter(Mandatory)][string]$PolicyFile,
    [switch]$Remove
)
$ErrorActionPreference = 'Stop'
$citool = Join-Path $env:windir 'System32\CiTool.exe'
if ($Remove) {
    [xml]$p = Get-Content $PolicyFile
    & $citool --remove-policy $p.SiPolicy.PolicyID
} else {
    & $citool --update-policy (Resolve-Path $PolicyFile)
}
if ($LASTEXITCODE) { throw "CiTool failed ($LASTEXITCODE)" }
& $citool --list-policies | Select-String -Context 0,6 'compute-pool allow list'
