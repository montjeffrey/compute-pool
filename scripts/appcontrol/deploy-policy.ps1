# Deploys (or removes) the App Control policy that build-policy.ps1 wrote.
# Changes a Windows security setting, so run it yourself from an elevated
# PowerShell 7, from a copy of these scripts that only administrators can
# change; pool never runs it. The policy applies right away, without a
# reboot. Do it in this order:
#
#   deploy-policy.ps1 out\compute-pool-policy.xml -Mode audit     # 1. logs, blocks nothing
#   (run your signed pool.exe, and the programs your jobs use)
#   check-audit.ps1 -Since <time printed in step 1>               # 2. nothing would be blocked
#   deploy-policy.ps1 out\compute-pool-policy.xml -Mode enforce -Program bin\pool.exe
#                                                                 # 3. replaces the audit one
#   deploy-policy.ps1 out\compute-pool-policy.xml -Remove        # only a policy this script deployed
#
# The audit deployment records the SHA-256 of the XML, and enforce refuses if
# it changed since, so the policy can't gain rules in between. Every
# deployment prints the policy's options and the signers it adds.
#
# It copies the XML into a new folder only administrators can write, checks
# it there, builds the .cip from that copy in the mode you asked for, and
# deploys that file, so what was checked is what gets deployed.
#
# Step 3 refuses unless this script deployed the same policy in audit mode
# (recorded under -StateKey, which only administrators can write), CiTool
# still lists it, -Program is signed by the certificate the policy trusts,
# and check-audit.ps1 finds nothing that policy would block (and nothing
# matching -Match from a check that names no policy) since the audit
# deployment. -Force skips those checks. No events can also mean the program
# never ran, so run it in step 1.
#
# A policy can only add restrictions: while Smart App Control is on, it still
# blocks whatever it blocked before. The policy takes Smart App Control's
# place once you turn that off.
#Requires -RunAsAdministrator
#Requires -Version 7
param(
    [Parameter(Mandatory, Position = 0)][string]$PolicyXml,
    [ValidateSet('audit', 'enforce')][string]$Mode,
    [switch]$Remove,
    [string]$Program,
    [string]$Match = 'pool',
    [switch]$Force,
    [string]$StateKey = 'HKLM:\SOFTWARE\compute-pool\appcontrol',
    [string]$WorkRoot = (Join-Path ([Environment]::GetFolderPath('CommonApplicationData')) 'compute-pool-appcontrol'),
    [string]$CiTool = (Join-Path ([Environment]::SystemDirectory) 'CiTool.exe')
)
$ErrorActionPreference = 'Stop'
if (-not $Remove -and -not $Mode) { throw '-Mode audit or -Mode enforce is required (or -Remove)' }

# The SHA-256 of a certificate's to-be-signed part: what an App Control
# signer rule (CertRoot Type="TBS") pins. Same as in build-policy.ps1.
function Get-TbsHash([Security.Cryptography.X509Certificates.X509Certificate2]$Cert) {
    $tbs = [Formats.Asn1.AsnReader]::new($Cert.RawData, 'DER').ReadSequence().ReadEncodedValue().ToArray()
    [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($tbs))
}

# A new folder that only SYSTEM and Administrators can reach, owned by
# Administrators. Your own unelevated programs run as you without
# Administrators, so they can't reach it either. The root must not be a link,
# and must not belong to anyone else.
$workOwners = 'S-1-5-18', 'S-1-5-32-544'
$workSddl = 'O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)'
$workSections = 'Owner, Access'
function New-WorkDir {
    if (Test-Path -LiteralPath $WorkRoot) {
        $root = Get-Item -LiteralPath $WorkRoot -Force
        if ($root.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "$WorkRoot is a link; remove it" }
        $owner = (Get-Acl -LiteralPath $WorkRoot).GetOwner([Security.Principal.SecurityIdentifier]).Value
        if ($owner -notin $workOwners) { throw "$WorkRoot belongs to $owner; remove it" }
    } else {
        New-Item -ItemType Directory $WorkRoot | Out-Null
    }
    # Owner and access list only: Set-Acl would also write the audit list,
    # which needs a privilege administrators don't hold by default.
    $folder = [IO.DirectoryInfo]::new($WorkRoot)
    $acl = [IO.FileSystemAclExtensions]::GetAccessControl($folder, [Security.AccessControl.AccessControlSections]::Access)
    $acl.SetSecurityDescriptorSddlForm($workSddl, $workSections)
    [IO.FileSystemAclExtensions]::SetAccessControl($folder, $acl)
    New-Item -ItemType Directory (Join-Path $WorkRoot ([guid]::NewGuid().ToString('N')))
}

function Invoke-CiTool {
    $out = & $CiTool @args 2>&1
    if ($LASTEXITCODE) { throw "CiTool $($args -join ' ') failed ($LASTEXITCODE): $out" }
    $out
}

$work = New-WorkDir
try {
    $xml = Join-Path $work 'policy.xml'
    $bytes = [IO.File]::ReadAllBytes((Resolve-Path -LiteralPath $PolicyXml).Path)
    [IO.File]::WriteAllBytes($xml, $bytes)
    # The audit deployment records this hash, and enforce requires the same
    # one, so the policy can't gain rules between the two.
    $policyHash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes))
    [xml]$policy = Get-Content -LiteralPath $xml
    $g = [guid]::Empty
    if (-not [guid]::TryParse($policy.SiPolicy.PolicyID, [ref]$g)) { throw "PolicyID '$($policy.SiPolicy.PolicyID)' is not a GUID" }
    if ($policy.SiPolicy.BasePolicyID -ne $policy.SiPolicy.PolicyID) { throw 'this is a supplemental policy; build-policy.ps1 makes base policies' }
    $id = "{$g}"
    $name = @($policy.SiPolicy.Settings.Setting | Where-Object ValueName -eq 'Name')[0].Value.String
    $record = Join-Path $StateKey $g.ToString()
    $rollback = "deploy-policy.ps1 `"$((Resolve-Path -LiteralPath $PolicyXml).Path)`" -Remove   (or: CiTool --remove-policy $id)"

    if ($Remove) {
        if (-not (Test-Path $record) -and -not $Force) { throw "this script didn't deploy policy $id; -Force removes it anyway" }
        Invoke-CiTool --remove-policy $id | Out-Null
        if (Test-Path $record) { Remove-Item $record -Recurse }
        "Removed $name $id."
        return
    }

    if ($Mode -eq 'enforce' -and -not $Force) {
        if (-not (Test-Path $record)) { throw "deploy this policy with -Mode audit first (no audit record for $id); -Force skips this" }
        $audit = Get-ItemProperty $record
        if ($audit.Mode -ne 'audit') { throw "policy $id is already $($audit.Mode); -Force to redeploy" }
        if ($audit.PolicySha256 -ne $policyHash) { throw "$PolicyXml changed since it was deployed in audit mode; deploy it with -Mode audit again" }
        $listed = (Invoke-CiTool --list-policies --json) -join "`n" | ConvertFrom-Json
        if (-not $listed.PSObject.Properties['Policies']) { throw "can't read CiTool's policy list; -Force skips this check" }
        if (-not @($listed.Policies | Where-Object { ("$($_.PolicyID)" -as [guid]) -eq $g })) {
            throw "CiTool doesn't list policy $id any more; deploy it with -Mode audit again"
        }
        if (-not $Program) { throw "-Program <your signed pool.exe> is required, to check it is signed by the certificate this policy trusts" }
        $sig = Get-AuthenticodeSignature -LiteralPath $Program
        $trusted = @($policy.SiPolicy.Signers.Signer | ForEach-Object { $_.CertRoot } | Where-Object Type -eq 'TBS' | ForEach-Object Value)
        if ($sig.Status -notin 'Valid', 'UnknownError' -or -not $sig.SignerCertificate -or (Get-TbsHash $sig.SignerCertificate) -notin $trusted) {
            throw "$Program is $($sig.Status), signed by '$($sig.SignerCertificate.Subject)', a certificate this policy doesn't trust"
        }
        $since = [datetime]::Parse($audit.DeployedAt, [Globalization.CultureInfo]::InvariantCulture, 'RoundtripKind').ToLocalTime()
        & (Join-Path $PSScriptRoot 'check-audit.ps1') -Match $Match -Since $since -PolicyId $g
        switch ($LASTEXITCODE) {
            0 { }
            1 { throw "App Control logged the events above since the audit policy went in at $since; fix them or -Force" }
            default { throw "check-audit.ps1 couldn't confirm there were none (exit $LASTEXITCODE); -Force to deploy anyway" }
        }
    }

    Import-Module ConfigCI
    if ($Mode -eq 'audit') { Set-RuleOption -FilePath $xml -Option 3 } else { Set-RuleOption -FilePath $xml -Option 3 -Delete }
    $cip = Join-Path $work "$g.cip"
    ConvertFrom-CIPolicy -XmlFilePath $xml -BinaryFilePath $cip | Out-Null
    $now = (Get-Date).ToUniversalTime()
    Invoke-CiTool --update-policy $cip | Out-Null
    New-Item -Force $record | Out-Null
    Set-ItemProperty $record -Name Mode -Value $Mode
    Set-ItemProperty $record -Name DeployedAt -Value $now.ToString('o')
    Set-ItemProperty $record -Name PolicySha256 -Value $policyHash
    "Deployed $name $id in $Mode mode at $($now.ToLocalTime())."
    [xml]$deployedXml = Get-Content -LiteralPath $xml
    "Options: " + (($deployedXml.SiPolicy.Rules.Rule | ForEach-Object Option) -join ', ')
    "Signers it trusts beyond the template:"
    foreach ($s in $policy.SiPolicy.Signers.Signer | Where-Object { $_.ID -like 'ID_SIGNER_S_*' }) { "  $($s.Name) (TBS $($s.CertRoot.Value))" }
    if ($Mode -eq 'audit') { "Next: run your signed pool.exe, then check-audit.ps1 -Since '$($now.ToLocalTime())'." }
    "To undo: $rollback"
} finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
