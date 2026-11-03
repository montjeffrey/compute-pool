# Builds an App Control for Business policy that keeps Smart App Control's
# rules ("Signed and Reputable": Windows, publicly trusted signed code, and
# code Microsoft's cloud judges safe) and also trusts code signed with your
# own certificate, such as pool builds you sign with sign.ps1.
#
# Needs the ConfigCI module (Windows Pro, Enterprise or Education; Home can
# enforce the policy but can't build it). Writes, into -OutDir:
#   compute-pool-policy.xml    the policy source
#   compute-pool-audit.cip     the same policy in audit mode: logs what it
#                              would block (Event Viewer, CodeIntegrity
#                              Operational, event 3076) and blocks nothing
#   compute-pool-enforce.cip   the policy enforced
# Both .cip files share one policy ID, so deploying the enforced one later
# replaces the audit one. Deploy with deploy-policy.ps1.
#
#   pwsh scripts/appcontrol/build-policy.ps1 -CertificatePath pool-signing.cer -OutDir out
param(
    [Parameter(Mandatory)][string]$CertificatePath,
    [string]$OutDir = '.',
    [string]$PolicyName = 'compute-pool allow list'
)
$ErrorActionPreference = 'Stop'
Import-Module ConfigCI

$template = Join-Path $env:windir 'schemas\CodeIntegrity\ExamplePolicies\SmartAppControl.xml'
if (-not (Test-Path $template)) { throw "template not found: $template" }
New-Item -ItemType Directory -Force $OutDir | Out-Null
$xml = Join-Path (Resolve-Path $OutDir) 'compute-pool-policy.xml'
Copy-Item $template $xml -Force

# The template carries one option only Smart App Control itself may use;
# Microsoft says it must be removed from policies built from it.
[xml]$doc = Get-Content $xml
@($doc.SiPolicy.Rules.Rule | Where-Object Option -eq 'Enabled:Conditional Windows Lockdown Policy') |
    ForEach-Object { [void]$_.ParentNode.RemoveChild($_) }
$doc.Save($xml)

# A new policy ID, so this policy sits beside Windows' own instead of
# replacing one of them.
Set-CIPolicyIdInfo -FilePath $xml -PolicyName $PolicyName -ResetPolicyID | Out-Null
Set-CIPolicyVersion -FilePath $xml -Version '1.0.0.0'
# Trust user-mode code signed with the certificate. Only this rule widens
# Smart App Control's circle of trust.
Add-SignerRule -FilePath $xml -CertificatePath (Resolve-Path $CertificatePath) -User
# Unsigned policy (6) that can be updated without a reboot (16).
Set-RuleOption -FilePath $xml -Option 6
Set-RuleOption -FilePath $xml -Option 16

Set-RuleOption -FilePath $xml -Option 3 -Delete
ConvertFrom-CIPolicy -XmlFilePath $xml -BinaryFilePath (Join-Path $OutDir 'compute-pool-enforce.cip') | Out-Null
Set-RuleOption -FilePath $xml -Option 3 # Enabled:Audit Mode
ConvertFrom-CIPolicy -XmlFilePath $xml -BinaryFilePath (Join-Path $OutDir 'compute-pool-audit.cip') | Out-Null

[xml]$p = Get-Content $xml
"Policy ID: $($p.SiPolicy.PolicyID)"
"Options:   " + (($p.SiPolicy.Rules.Rule | ForEach-Object Option) -join ', ')
"Signers:   " + (($p.SiPolicy.Signers.Signer | ForEach-Object Name) -join ', ')
