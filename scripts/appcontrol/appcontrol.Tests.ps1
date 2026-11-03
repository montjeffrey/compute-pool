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

    # Get-WinEvent's own error when a log has nothing in range.
    function Get-NoEvents {
        [Management.Automation.ErrorRecord]::new([Exception]::new('No events were found that match the specified selection criteria.'),
            'NoMatchingEventsFound', 'ObjectNotFound', $null)
    }
    function New-FakeEvent([int]$Id, [hashtable]$Data) {
        $items = foreach ($k in $Data.Keys) { "<Data Name='$k'>$([Security.SecurityElement]::Escape($Data[$k]))</Data>" }
        $x = "<Event><EventData>$($items -join '')</EventData></Event>"
        [pscustomobject]@{ Id = $Id; TimeCreated = Get-Date; ActivityId = [guid]::NewGuid() } |
            Add-Member ScriptMethod ToXml ([scriptblock]::Create("'$($x -replace "'", "''")'")) -PassThru
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
    }
    It 'exits 0 when no log has events' {
        Mock Get-WinEvent { $PSCmdlet.ThrowTerminatingError((Get-NoEvents)) }
        Invoke-Check | Should -Be 0
    }
    It 'exits 1 on a would-block event for a matching file' {
        Mock Get-WinEvent {
            if ($FilterHashtable.Id -contains 3076) { New-FakeEvent 3076 @{ 'File Name' = '\Device\HarddiskVolume3\bin\pool.exe'; PolicyName = 'compute-pool allow list' } }
            else { $PSCmdlet.ThrowTerminatingError((Get-NoEvents)) }
        }
        Invoke-Check | Should -Be 1
    }
    It 'reads the file name from FileNameBuffer (event 3033)' {
        Mock Get-WinEvent {
            if ($FilterHashtable.Id -contains 3033) { New-FakeEvent 3033 @{ FileNameBuffer = '\Device\HarddiskVolume3\x\pool.exe'; Status = '1' } }
            else { $PSCmdlet.ThrowTerminatingError((Get-NoEvents)) }
        }
        Invoke-Check | Should -Be 1
    }
    It 'ignores files that do not match, and events from another policy' {
        Mock Get-WinEvent {
            if ($FilterHashtable.Id -contains 3076) {
                New-FakeEvent 3076 @{ 'File Name' = '\x\other.exe'; PolicyName = 'compute-pool allow list' }
                New-FakeEvent 3076 @{ 'File Name' = '\x\pool.exe'; PolicyName = 'Some other policy' }
            } else { $PSCmdlet.ThrowTerminatingError((Get-NoEvents)) }
        }
        Invoke-Check @{ Policy = 'compute-pool allow list' } | Should -Be 0
    }
    It 'exits 2 when a log cannot be read, instead of reporting no events' {
        Mock Get-WinEvent { throw [UnauthorizedAccessException]::new('Attempted to perform an unauthorized operation.') }
        Invoke-Check | Should -Be 2
    }
}

Describe 'deploy-policy.ps1' {
    BeforeAll {
        # A copy without "#Requires -RunAsAdministrator", so the gate runs
        # unelevated, and a fake CiTool that only records its arguments.
        $d = Join-Path $TestDrive 'deploy'
        New-Item -ItemType Directory $d | Out-Null
        (Get-Content "$dir\deploy-policy.ps1") -notmatch '^#Requires' | Set-Content "$d\deploy-policy.ps1"
        Copy-Item "$dir\check-audit.ps1" $d
        $calls = Join-Path $TestDrive 'citool-calls.txt'
        $listing = Join-Path $TestDrive 'citool-listing.txt'
        Set-Content "$d\fake-citool.ps1" "Add-Content '$calls' (`$args -join ' '); if (`$args[0] -eq '--list-policies') { Get-Content '$listing' }; exit 0"
        $key = "HKCU:\Software\compute-pool-test-$([guid]::NewGuid().ToString('N').Substring(0, 8))"
        # TestDrive is per Describe block, so this block builds its own policy.
        $policyDir = Join-Path $TestDrive 'policy'
        & "$dir\build-policy.ps1" -CertificatePath $cer -OutDir $policyDir -TemplatePath $template -WarningAction SilentlyContinue | Out-Null
        [xml]$px = Get-Content (Join-Path $policyDir 'compute-pool-policy.xml')
        $policyId = $px.SiPolicy.PolicyID
        $record = Join-Path $key $policyId.Trim('{}')
        $signed = Join-Path $TestDrive 'signed-pool.exe'
        Copy-Item $unsigned $signed
        & "$dir\sign.ps1" $signed -Thumbprint $cert.Thumbprint | Out-Null
        $common = @{ CiTool = "$d\fake-citool.ps1"; StateKey = $key }
        function Deploy { param([string]$File, [hashtable]$More = @{})
            & "$d\deploy-policy.ps1" (Join-Path $policyDir $File) @common @More
        }
    }
    AfterAll { if (Test-Path $key) { Remove-Item $key -Recurse } }
    BeforeEach { Set-Content $listing "Policy ID: $($policyId.Trim('{}'))" }

    It 'refuses enforce before audit' {
        { Deploy 'compute-pool-enforce.cip' @{ Program = $signed } } | Should -Throw '*first*'
    }
    It 'deploys audit, records it, and prints the rollback' {
        $out = Deploy 'compute-pool-audit.cip'
        ($out -join "`n") | Should -BeLike "*To undo: deploy-policy.ps1 -Remove*CiTool --remove-policy $policyId*"
        (Get-ItemProperty $record).Mode | Should -Be 'audit'
        Get-Content $calls | Should -Contain "--update-policy $(Join-Path $policyDir 'compute-pool-audit.cip')"
    }
    It 'refuses enforce without -Program' {
        { Deploy 'compute-pool-enforce.cip' } | Should -Throw '*-Program*'
    }
    It 'refuses enforce for a program the policy does not trust' {
        { Deploy 'compute-pool-enforce.cip' @{ Program = $unsigned } } | Should -Throw "*doesn't trust*"
    }
    It 'refuses enforce when the audit policy is no longer active' {
        Set-Content $listing 'Policy ID: 00000000-0000-0000-0000-000000000000'
        { Deploy 'compute-pool-enforce.cip' @{ Program = $signed } } | Should -Throw "*isn't active*"
    }
    It 'refuses enforce when check-audit found events' {
        Mock Get-WinEvent {
            if ($FilterHashtable.Id -contains 3076) { New-FakeEvent 3076 @{ 'File Name' = '\x\pool.exe'; PolicyName = 'compute-pool allow list' } }
            else { $PSCmdlet.ThrowTerminatingError((Get-NoEvents)) }
        }
        { Deploy 'compute-pool-enforce.cip' @{ Program = $signed } } | Should -Throw '*logged the events*'
    }
    It 'deploys enforce when every check passes' {
        Mock Get-WinEvent { $PSCmdlet.ThrowTerminatingError((Get-NoEvents)) }
        Deploy 'compute-pool-enforce.cip' @{ Program = $signed } | Out-Null
        (Get-ItemProperty $record).Mode | Should -Be 'enforce'
    }
    It 'removes the policy and its record' {
        & "$d\deploy-policy.ps1" (Join-Path $policyDir 'compute-pool-policy.xml') -Remove @common | Out-Null
        Get-Content $calls | Should -Contain "--remove-policy $policyId"
        Test-Path $record | Should -BeFalse
    }
}
