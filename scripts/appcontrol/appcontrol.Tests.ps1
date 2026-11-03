# Pester 5 tests for the App Control scripts. They never deploy a policy:
# deploy-policy.ps1 runs against a fake CiTool and a throwaway registry key.
#
#   Invoke-Pester scripts/appcontrol

BeforeAll {
    $dir = $PSScriptRoot
    $subject = "CN=compute-pool test $([guid]::NewGuid().ToString('N').Substring(0, 8))"
    $cer = Join-Path $TestDrive 'test.cer'
    & "$dir\new-cert.ps1" -Subject $subject -OutFile $cer -WarningAction SilentlyContinue | Out-Null
    $cert = Get-ChildItem Cert:\CurrentUser\My | Where-Object Subject -eq $subject

    # A small unsigned program to sign.
    $csc = Join-Path $env:windir 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
    Set-Content (Join-Path $TestDrive 'h.cs') 'class H { static void Main() { System.Console.Write("hi"); } }'
    $unsigned = Join-Path $TestDrive 'hello.exe'
    & $csc -nologo "-out:$unsigned" (Join-Path $TestDrive 'h.cs') | Out-Null

    # Windows Server runners may lack the Smart App Control template; the
    # default Windows template exercises the same code.
    $template = Join-Path $env:windir 'schemas\CodeIntegrity\ExamplePolicies\SmartAppControl.xml'
    if (-not (Test-Path $template)) { $template = Join-Path $env:windir 'schemas\CodeIntegrity\ExamplePolicies\DefaultWindows_Enforced.xml' }

    function New-TestCert([string]$Name) {
        New-SelfSignedCertificate -Type CodeSigningCert -Subject $Name -CertStoreLocation Cert:\CurrentUser\My `
            -KeyAlgorithm RSA -KeyLength 2048 -KeyExportPolicy NonExportable
    }
    # Get-WinEvent's own error when a log has nothing in range.
    function Get-NoEvents {
        [Management.Automation.ErrorRecord]::new([Exception]::new('No events were found that match the specified selection criteria.'),
            'NoMatchingEventsFound', 'ObjectNotFound', $null)
    }
    function New-FakeEvent([int]$Id, [hashtable]$Data, [datetime]$Time = (Get-Date)) {
        $items = foreach ($k in $Data.Keys) { "<Data Name='$k'>$([Security.SecurityElement]::Escape($Data[$k]))</Data>" }
        $x = "<Event><EventData>$($items -join '')</EventData></Event>"
        [pscustomobject]@{ Id = $Id; TimeCreated = $Time; ActivityId = [guid]::NewGuid() } |
            Add-Member ScriptMethod ToXml ([scriptblock]::Create("'$($x -replace "'", "''")'")) -PassThru
    }
    # Stands in for Get-WinEvent: logs that go back years, holding $Events.
    function Get-FakeLog($Oldest, $FilterHashtable, [object[]]$Events = @()) {
        if ($Oldest) { return New-FakeEvent 1 @{} ([datetime]'2000-01-01') }
        $hit = @($Events | Where-Object { $FilterHashtable.Id -contains $_.Id })
        if ($hit) { $hit } else { throw (Get-NoEvents) }
    }
}

AfterAll {
    if ($cert) { Remove-Item "Cert:\CurrentUser\My\$($cert.Thumbprint)" -DeleteKey }
}

Describe 'new-cert.ps1' {
    It 'makes an RSA 2048 code-signing certificate valid for two years' {
        $cert | Should -Not -BeNullOrEmpty
        $cert.PublicKey.GetRSAPublicKey().KeySize | Should -Be 2048
        $cert.EnhancedKeyUsageList.ObjectId | Should -Contain '1.3.6.1.5.5.7.3.3'
        ($cert.NotAfter - (Get-Date)).TotalDays | Should -BeGreaterThan 700
        ($cert.NotAfter - (Get-Date)).TotalDays | Should -BeLessThan 735
        $cer | Should -Exist
    }
    It 'reuses the certificate on a second run' {
        & "$dir\new-cert.ps1" -Subject $subject -OutFile $cer -WarningAction SilentlyContinue | Out-Null
        @(Get-ChildItem Cert:\CurrentUser\My | Where-Object Subject -eq $subject).Count | Should -Be 1
    }
    It 'refuses when two certificates have the subject' {
        $twin = New-TestCert $subject
        try {
            { & "$dir\new-cert.ps1" -Subject $subject -OutFile (Join-Path $TestDrive 'x.cer') } | Should -Throw '*2 certificates*'
        } finally { Remove-Item "Cert:\CurrentUser\My\$($twin.Thumbprint)" -DeleteKey }
    }
    It 'refuses to reuse a certificate whose key can be exported' {
        $name = "CN=compute-pool test exportable $([guid]::NewGuid().ToString('N').Substring(0, 6))"
        $planted = New-SelfSignedCertificate -Type CodeSigningCert -Subject $name -CertStoreLocation Cert:\CurrentUser\My -KeyExportPolicy Exportable
        try {
            { & "$dir\new-cert.ps1" -Subject $name -OutFile (Join-Path $TestDrive 'x.cer') } | Should -Throw '*can be exported*'
        } finally { Remove-Item "Cert:\CurrentUser\My\$($planted.Thumbprint)" -DeleteKey }
    }
    It 'with -RequireTpm, fails instead of falling back to a software key' {
        Mock New-SelfSignedCertificate { throw 'no TPM here' } -ParameterFilter { $Provider }
        $name = "CN=compute-pool test notpm $([guid]::NewGuid().ToString('N').Substring(0, 6))"
        { & "$dir\new-cert.ps1" -Subject $name -OutFile (Join-Path $TestDrive 'x.cer') -RequireTpm } | Should -Throw '*no TPM-backed key*'
        @(Get-ChildItem Cert:\CurrentUser\My | Where-Object Subject -eq $name).Count | Should -Be 0
    }
}

Describe 'sign.ps1' {
    BeforeEach {
        $exe = Join-Path $TestDrive "s$([guid]::NewGuid().ToString('N').Substring(0, 6)).exe"
        Copy-Item $unsigned $exe
    }
    It 'signs with the certificate and the result verifies' {
        & "$dir\sign.ps1" $exe -Thumbprint $cert.Thumbprint -ExpectedSha256 (Get-FileHash $exe).Hash | Out-Null
        $s = Get-AuthenticodeSignature $exe
        $s.SignerCertificate.Thumbprint | Should -Be $cert.Thumbprint
        $s.Status | Should -BeIn 'Valid', 'UnknownError'
    }
    It 'refuses a file whose hash is not the expected one' {
        { & "$dir\sign.ps1" $exe -Thumbprint $cert.Thumbprint -ExpectedSha256 ('0' * 64) } | Should -Throw '*not signing it*'
        (Get-AuthenticodeSignature $exe).Status | Should -Be 'NotSigned'
    }
    It 'needs one expected hash per file' {
        { & "$dir\sign.ps1" $exe, $exe -Thumbprint $cert.Thumbprint -ExpectedSha256 ('0' * 64) } | Should -Throw '*one hash per file*'
    }
    It 'fails when the signature is not there afterwards, whatever signing reported' {
        Mock Set-AuthenticodeSignature { [pscustomobject]@{ Status = 'Valid'; StatusMessage = 'Signature verified.' } }
        { & "$dir\sign.ps1" $exe -Thumbprint $cert.Thumbprint } | Should -Throw '*failed*'
        (Get-AuthenticodeSignature $exe).Status | Should -Be 'NotSigned'
    }
    It 'refuses to guess between two certificates with the same subject' {
        $twin = New-TestCert $subject
        try { { & "$dir\sign.ps1" $exe -Subject $subject } | Should -Throw '*pick one with -Thumbprint*' }
        finally { Remove-Item "Cert:\CurrentUser\My\$($twin.Thumbprint)" -DeleteKey }
    }
    It 'replaces a hard-linked file instead of writing through the link' {
        $other = Join-Path $TestDrive "link$([guid]::NewGuid().ToString('N').Substring(0, 6)).exe"
        New-Item -ItemType HardLink -Path $other -Target $exe | Out-Null
        & "$dir\sign.ps1" $exe -Thumbprint $cert.Thumbprint | Out-Null
        (Get-AuthenticodeSignature $exe).SignerCertificate.Thumbprint | Should -Be $cert.Thumbprint
        (Get-AuthenticodeSignature $other).Status | Should -Be 'NotSigned'
    }
}

Describe 'build-policy.ps1' {
    BeforeAll {
        $out = Join-Path $TestDrive 'policy'
        & "$dir\build-policy.ps1" -CertificatePath $cer -OutDir $out -TemplatePath $template -WarningAction SilentlyContinue | Out-Null
        [xml]$p = Get-Content (Join-Path $out 'compute-pool-policy.xml')
        $options = @($p.SiPolicy.Rules.Rule | ForEach-Object Option)
    }
    It 'writes the XML and both .cip files' {
        foreach ($f in 'compute-pool-policy.xml', 'compute-pool-audit.cip', 'compute-pool-enforce.cip') {
            Join-Path $out $f | Should -Exist
        }
    }
    It 'trusts the certificate for user-mode code' {
        $signer = @($p.SiPolicy.Signers.Signer | Where-Object Name -eq ($subject -replace '^CN='))
        $signer.Count | Should -Be 1
        $umci = $p.SiPolicy.SigningScenarios.SigningScenario | Where-Object Value -eq 12
        $umci.ProductSigners.AllowedSigners.AllowedSigner.SignerId | Should -Contain $signer[0].ID
    }
    It 'is unsigned, updatable without a reboot, and without the Smart App Control-only option' {
        $options | Should -Contain 'Enabled:Unsigned System Integrity Policy'
        $options | Should -Contain 'Enabled:Update Policy No Reboot'
        $options | Should -Not -Contain 'Enabled:Conditional Windows Lockdown Policy'
    }
    It 'leaves the XML in audit mode, and the enforce .cip differs from the audit one' {
        $options | Should -Contain 'Enabled:Audit Mode'
        (Get-FileHash (Join-Path $out 'compute-pool-audit.cip')).Hash |
            Should -Not -Be (Get-FileHash (Join-Path $out 'compute-pool-enforce.cip')).Hash
    }
    It 'has a policy ID of its own' {
        [xml]$t = Get-Content $template
        $p.SiPolicy.PolicyID | Should -Not -Be $t.SiPolicy.PolicyID
    }
    It 'refuses a certificate without code-signing usage' {
        $plain = New-SelfSignedCertificate -Subject 'CN=compute-pool test no eku' -CertStoreLocation Cert:\CurrentUser\My `
            -KeyUsage DigitalSignature -TextExtension @('2.5.29.37={text}1.3.6.1.5.5.7.3.1')
        try {
            $plainCer = Join-Path $TestDrive 'plain.cer'
            Export-Certificate -Cert $plain -FilePath $plainCer | Out-Null
            { & "$dir\build-policy.ps1" -CertificatePath $plainCer -OutDir (Join-Path $TestDrive 'bad') -TemplatePath $template } |
                Should -Throw '*code-signing*'
        } finally { Remove-Item "Cert:\CurrentUser\My\$($plain.Thumbprint)" -DeleteKey }
    }
}

Describe 'check-audit.ps1' {
    BeforeAll {
        function Invoke-Check { param([hashtable]$Params = @{})
            & "$dir\check-audit.ps1" @Params *>&1 | Out-Null
            $LASTEXITCODE
        }
        $ours = [guid]::NewGuid()
    }
    It 'exits 0 when no log has events' {
        Mock Get-WinEvent { Get-FakeLog $Oldest $FilterHashtable }
        Invoke-Check | Should -Be 0
    }
    It 'exits 1 on a would-block event for a matching file' {
        Mock Get-WinEvent { Get-FakeLog $Oldest $FilterHashtable (New-FakeEvent 3076 @{ 'File Name' = '\Device\HarddiskVolume3\bin\pool.exe' }) }
        Invoke-Check | Should -Be 1
    }
    It 'reads the file name from FileNameBuffer (event 3033)' {
        Mock Get-WinEvent { Get-FakeLog $Oldest $FilterHashtable (New-FakeEvent 3033 @{ FileNameBuffer = '\Device\HarddiskVolume3\x\pool.exe' }) }
        Invoke-Check | Should -Be 1
    }
    It 'with -PolicyId, counts any file that policy would block' {
        Mock Get-WinEvent { Get-FakeLog $Oldest $FilterHashtable (New-FakeEvent 3076 @{ 'File Name' = '\x\node.exe'; PolicyGUID = "{$ours}" }) }
        Invoke-Check @{ PolicyId = $ours } | Should -Be 1
    }
    It 'with -PolicyId, ignores other policies and unmatched files without a policy' {
        Mock Get-WinEvent {
            Get-FakeLog $Oldest $FilterHashtable @(
                New-FakeEvent 3076 @{ 'File Name' = '\x\pool.exe'; PolicyGUID = "{$([guid]::NewGuid())}" }
                New-FakeEvent 3033 @{ FileNameBuffer = '\x\chrome.dll' })
        }
        Invoke-Check @{ PolicyId = $ours } | Should -Be 0
    }
    It 'exits 2 when a log cannot be read, instead of reporting no events' {
        Mock Get-WinEvent { throw [UnauthorizedAccessException]::new('Attempted to perform an unauthorized operation.') }
        Invoke-Check | Should -Be 2
    }
    It 'exits 2 when a log no longer goes back to -Since' {
        Mock Get-WinEvent { if ($Oldest) { New-FakeEvent 1 @{} (Get-Date) } else { throw (Get-NoEvents) } }
        Invoke-Check @{ Since = (Get-Date).AddDays(-1) } | Should -Be 2
    }
}

Describe 'deploy-policy.ps1' {
    BeforeAll {
        # A copy without "#Requires -RunAsAdministrator", so the gate runs
        # unelevated, and a fake CiTool that records its arguments, keeps a
        # copy of each policy it is given, and lists $listing as JSON.
        $d = Join-Path $TestDrive 'deploy'
        New-Item -ItemType Directory $d | Out-Null
        (Get-Content "$dir\deploy-policy.ps1") -notmatch '^#Requires -RunAsAdministrator' | Set-Content "$d\deploy-policy.ps1"
        Copy-Item "$dir\check-audit.ps1" $d
        $calls = Join-Path $TestDrive 'citool-calls.txt'
        $listing = Join-Path $TestDrive 'citool-listing.json'
        $deployed = Join-Path $TestDrive 'deployed.cip'
        Set-Content "$d\fake-citool.ps1" @"
Add-Content '$calls' (`$args -join ' ')
if (`$args[0] -eq '--update-policy') { Copy-Item `$args[1] '$deployed' -Force }
if (`$args[0] -eq '--list-policies') { Get-Content '$listing' }
exit 0
"@
        $key = "HKCU:\Software\compute-pool-test-$([guid]::NewGuid().ToString('N').Substring(0, 8))"
        # TestDrive is per Describe block, so this block builds its own policy.
        $policyDir = Join-Path $TestDrive 'policy'
        & "$dir\build-policy.ps1" -CertificatePath $cer -OutDir $policyDir -TemplatePath $template -WarningAction SilentlyContinue | Out-Null
        $policyXml = Join-Path $policyDir 'compute-pool-policy.xml'
        [xml]$px = Get-Content $policyXml
        $policyId = $px.SiPolicy.PolicyID
        $record = Join-Path $key ([guid]$policyId).ToString()
        $signed = Join-Path $TestDrive 'signed-pool.exe'
        Copy-Item $unsigned $signed
        & "$dir\sign.ps1" $signed -Thumbprint $cert.Thumbprint | Out-Null
        $common = @{ CiTool = "$d\fake-citool.ps1"; StateKey = $key; WorkRoot = (Join-Path $TestDrive 'work') }
        function Deploy { param([hashtable]$More = @{}, [string]$Xml = $policyXml)
            & "$d\deploy-policy.ps1" $Xml @common @More
        }
        function Set-Listing([string]$PolicyId, [string]$BaseId = $PolicyId) {
            Set-Content $listing (@{ Policies = @(@{ PolicyID = $PolicyId; BasePolicyID = $BaseId; FriendlyName = 'x' }) } | ConvertTo-Json -Depth 3)
        }
        Mock Get-WinEvent { Get-FakeLog $Oldest $FilterHashtable }
    }
    AfterAll { if (Test-Path $key) { Remove-Item $key -Recurse } }
    BeforeEach { Set-Listing $policyId.Trim('{}') }

    It 'needs -Mode' {
        { Deploy } | Should -Throw '*-Mode*'
    }
    It 'refuses a policy ID that is not a GUID' {
        $bad = Join-Path $TestDrive 'bad-id.xml'
        (Get-Content $policyXml -Raw) -replace [regex]::Escape($policyId), '..\..\SOFTWARE\x' | Set-Content $bad
        { Deploy @{ Mode = 'audit' } $bad } | Should -Throw '*not a GUID*'
    }
    It 'refuses enforce before audit' {
        { Deploy @{ Mode = 'enforce'; Program = $signed } } | Should -Throw '*-Mode audit first*'
    }
    It 'refuses a work folder that is a link' {
        $elsewhere = New-Item -ItemType Directory (Join-Path $TestDrive 'elsewhere')
        $link = Join-Path $TestDrive 'linked-work'
        New-Item -ItemType Junction -Path $link -Target $elsewhere | Out-Null
        { & "$d\deploy-policy.ps1" $policyXml -Mode audit -CiTool $common.CiTool -StateKey $key -WorkRoot $link } | Should -Throw '*is a link*'
    }
    It 'deploys exactly the audit build, records it, and prints the rollback' {
        $out = Deploy @{ Mode = 'audit' }
        ($out -join "`n") | Should -BeLike "*To undo:*-Remove*CiTool --remove-policy $policyId*"
        (Get-ItemProperty $record).Mode | Should -Be 'audit'
        (Get-FileHash $deployed).Hash | Should -Be (Get-FileHash (Join-Path $policyDir 'compute-pool-audit.cip')).Hash
    }
    It 'refuses enforce without -Program' {
        { Deploy @{ Mode = 'enforce' } } | Should -Throw '*-Program*'
    }
    It 'refuses enforce for an unsigned program' {
        { Deploy @{ Mode = 'enforce'; Program = $unsigned } } | Should -Throw "*doesn't trust*"
    }
    It 'refuses enforce for a program signed by another certificate with the same name' {
        $twin = New-TestCert $subject
        try {
            $other = Join-Path $TestDrive 'twin-signed.exe'
            Copy-Item $unsigned $other
            & "$dir\sign.ps1" $other -Thumbprint $twin.Thumbprint | Out-Null
            { Deploy @{ Mode = 'enforce'; Program = $other } } | Should -Throw "*doesn't trust*"
        } finally { Remove-Item "Cert:\CurrentUser\My\$($twin.Thumbprint)" -DeleteKey }
    }
    It 'refuses enforce when CiTool no longer lists the policy' {
        Set-Listing ([guid]::NewGuid().ToString())
        { Deploy @{ Mode = 'enforce'; Program = $signed } } | Should -Throw "*doesn't list*"
    }
    It 'refuses enforce when the policy appears only as the base of another' {
        Set-Listing ([guid]::NewGuid().ToString()) $policyId.Trim('{}')
        { Deploy @{ Mode = 'enforce'; Program = $signed } } | Should -Throw "*doesn't list*"
    }
    It "refuses enforce when CiTool's list can't be read" {
        Set-Content $listing '{"OperationResult":-2147024891}'
        { Deploy @{ Mode = 'enforce'; Program = $signed } } | Should -Throw "*can't read CiTool*"
    }
    It 'refuses enforce when the policy would have blocked something' {
        Mock Get-WinEvent { Get-FakeLog $Oldest $FilterHashtable (New-FakeEvent 3076 @{ 'File Name' = '\x\node.exe'; PolicyGUID = $policyId }) }
        { Deploy @{ Mode = 'enforce'; Program = $signed } } | Should -Throw '*logged the events*'
    }
    It 'deploys exactly the enforce build when every check passes' {
        Deploy @{ Mode = 'enforce'; Program = $signed } | Out-Null
        (Get-ItemProperty $record).Mode | Should -Be 'enforce'
        (Get-FileHash $deployed).Hash | Should -Be (Get-FileHash (Join-Path $policyDir 'compute-pool-enforce.cip')).Hash
    }
    It 'removes the policy and its record' {
        Deploy @{ Remove = $true } | Out-Null
        Get-Content $calls | Should -Contain "--remove-policy $policyId"
        Test-Path $record | Should -BeFalse
    }
    It 'leaves no work folders behind' {
        @(Get-ChildItem (Join-Path $TestDrive 'work')).Count | Should -Be 0
    }
}
