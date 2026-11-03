# Creates a self-signed code-signing certificate in your user certificate
# store, for signing your own pool builds (sign.ps1) and trusting them in an
# App Control policy (build-policy.ps1). The certificate is not added to
# trusted roots: only the App Control policy trusts it.
#
# The private key is made in the TPM when there is one, so it can't be
# copied off this device. Without a TPM it falls back to a software key
# marked non-exportable, which code running as this account can still
# extract (-RequireTpm refuses instead). Neither stops the key being *used*:
# anything that runs as this account can sign code the policy will trust.
# So create it under an account that runs nothing else, and never under the
# account the agent runs as.
#
# If a certificate with the subject already exists, it is reused only when
# you name it with -Thumbprint (the one this script printed when it made
# it): nothing can prove where an existing key came from.
#
#   pwsh scripts/appcontrol/new-cert.ps1 -Subject 'CN=Your Name' [-RequireTpm] [-OutFile pool-signing.cer] [-Thumbprint <to reuse>]
param(
    [string]$Subject = "CN=compute-pool code signing ($($env:COMPUTERNAME.ToLower()))",
    [string]$OutFile = (Join-Path $env:LOCALAPPDATA 'compute-pool\signing\pool-signing.cer'),
    [int]$Years = 2,
    [switch]$RequireTpm,
    [string]$Thumbprint
)
$ErrorActionPreference = 'Stop'
$found = @(Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert | Where-Object Subject -eq $Subject)
if ($Thumbprint) {
    $cert = $found | Where-Object Thumbprint -eq $Thumbprint
    if (-not $cert) { throw "no code-signing certificate '$Subject' with thumbprint $Thumbprint in your store" }
    "Reusing the certificate $Thumbprint."
} elseif ($found) {
    throw "a certificate named '$Subject' already exists ($($found.Thumbprint -join ', ')); pass -Thumbprint to reuse one you made, or pick another -Subject"
} else {
    # App Control signer rules need RSA, not ECC. TPMs reliably support RSA
    # 2048; many refuse 3072.
    $common = @{
        Type = 'CodeSigningCert'; Subject = $Subject; CertStoreLocation = 'Cert:\CurrentUser\My'
        KeyAlgorithm = 'RSA'; KeyLength = 2048; HashAlgorithm = 'SHA256'; NotAfter = (Get-Date).AddYears($Years)
    }
    try {
        $cert = New-SelfSignedCertificate @common -Provider 'Microsoft Platform Crypto Provider'
    } catch {
        if ($RequireTpm) { throw "no TPM-backed key could be created: $($_.Exception.Message)" }
        Write-Warning "No TPM-backed key ($($_.Exception.Message)). Using a software key marked non-exportable; code running as this account can still use or extract it."
        $cert = New-SelfSignedCertificate @common -KeyExportPolicy NonExportable
    }
}
$key = [Security.Cryptography.X509Certificates.RSACertificateExtensions]::GetRSAPrivateKey($cert)
$provider = if ($key -is [Security.Cryptography.RSACng]) { $key.Key.Provider.Provider } else { $key.GetType().Name }
$exportable = if ($key -is [Security.Cryptography.RSACng]) { $key.Key.ExportPolicy -ne [Security.Cryptography.CngExportPolicies]::None }
    elseif ($key -is [Security.Cryptography.RSACryptoServiceProvider]) { $key.CspKeyContainerInfo.Exportable }
    else { $true }
if ($exportable) { throw "the key of the certificate for '$Subject' can be exported, so it may exist elsewhere; remove it and run this again" }
if ($RequireTpm -and $provider -ne 'Microsoft Platform Crypto Provider') {
    throw "the certificate for '$Subject' uses '$provider', not the TPM; remove it or pick another -Subject"
}
New-Item -ItemType Directory -Force (Split-Path $OutFile) | Out-Null
Export-Certificate -Cert $cert -FilePath $OutFile | Out-Null
"Certificate: $($cert.Subject)"
"Thumbprint:  $($cert.Thumbprint)"
"Key:         $provider"
"Valid until: $($cert.NotAfter.ToString('yyyy-MM-dd'))"
"Public part: $OutFile"
