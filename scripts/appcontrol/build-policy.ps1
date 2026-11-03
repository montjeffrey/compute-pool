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
    [string]$PolicyName = 'compute-pool allow list',
    [string]$TemplatePath = (Join-Path $env:windir 'schemas\CodeIntegrity\ExamplePolicies\SmartAppControl.xml')
)
$ErrorActionPreference = 'Stop'
Import-Module ConfigCI

$cerFile = (Resolve-Path $CertificatePath).Path
$cert = [Security.Cryptography.X509Certificates.X509Certificate2]::new($cerFile)
# App Control signer rules work with RSA keys only, and the certificate must
# be one Windows will accept for code signing.
if ($cert.PublicKey.Oid.Value -ne '1.2.840.113549.1.1.1') { throw "$cerFile is not an RSA certificate; App Control signer rules need RSA" }
$ekus = @($cert.Extensions | Where-Object { $_ -is [Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension] } |
    ForEach-Object { $_.EnhancedKeyUsages } | ForEach-Object Value)
if ($ekus -notcontains '1.3.6.1.5.5.7.3.3') { throw "$cerFile has no code-signing usage (1.3.6.1.5.5.7.3.3)" }
if ($cert.NotAfter -lt (Get-Date)) { throw "$cerFile expired on $($cert.NotAfter.ToString('yyyy-MM-dd'))" }

if (-not (Test-Path $TemplatePath)) { throw "template not found: $TemplatePath" }
New-Item -ItemType Directory -Force $OutDir | Out-Null
$xml = Join-Path (Resolve-Path $OutDir) 'compute-pool-policy.xml'
Copy-Item $TemplatePath $xml -Force

# The template carries one option only Smart App Control itself may use;
# Microsoft says it must be removed from policies built from it.
[xml]$doc = Get-Content $xml
@($doc.SiPolicy.Rules.Rule | Where-Object Option -eq 'Enabled:Conditional Windows Lockdown Policy') |
    ForEach-Object { [void]$_.ParentNode.RemoveChild($_) }
$doc.Save($xml)

# A new policy ID makes this a base policy of its own. It doesn't extend
# Smart App Control: Windows runs a program only if every enforced policy
# allows it, so while Smart App Control is on it still blocks what it
# blocked before. Turned off, this policy takes its place.
Set-CIPolicyIdInfo -FilePath $xml -PolicyName $PolicyName -ResetPolicyID | Out-Null
Set-CIPolicyVersion -FilePath $xml -Version '1.0.0.0'
# Trust user-mode code signed with the certificate. Only this rule widens
# Smart App Control's circle of trust.
Add-SignerRule -FilePath $xml -CertificatePath $cerFile -User
# Unsigned policy (6) that can be updated without a reboot (16).
Set-RuleOption -FilePath $xml -Option 6
Set-RuleOption -FilePath $xml -Option 16

Set-RuleOption -FilePath $xml -Option 3 -Delete
ConvertFrom-CIPolicy -XmlFilePath $xml -BinaryFilePath (Join-Path $OutDir 'compute-pool-enforce.cip') | Out-Null
Set-RuleOption -FilePath $xml -Option 3 # Enabled:Audit Mode
ConvertFrom-CIPolicy -XmlFilePath $xml -BinaryFilePath (Join-Path $OutDir 'compute-pool-audit.cip') | Out-Null

[xml]$p = Get-Content $xml
$name = $cert.GetNameInfo([Security.Cryptography.X509Certificates.X509NameType]::SimpleName, $false)
$ours = @($p.SiPolicy.Signers.Signer | Where-Object Name -eq $name)
if ($ours.Count -ne 1) { throw "expected one signer named '$name' in $xml, found $($ours.Count)" }
"Policy ID: $($p.SiPolicy.PolicyID)"
"Options:   " + (($p.SiPolicy.Rules.Rule | ForEach-Object Option) -join ', ')
""
"Trusted, besides what the template allows: user-mode code signed by"
"  $($cert.Subject) (thumbprint $($cert.Thumbprint), until $($cert.NotAfter.ToString('yyyy-MM-dd')))"
"Not trusted: anything else that is unsigned and unknown to Microsoft's"
"  cloud, including programs jobs ship with --ship unless you sign them too."
"The policy is unsigned, so any administrator can remove it."
