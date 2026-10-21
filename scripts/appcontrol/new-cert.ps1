# Creates a self-signed code-signing certificate in your user certificate
# store, for signing your own pool builds (sign.ps1) and trusting them in an
# App Control policy (build-policy.ps1). The private key can't be exported,
# so only this account on this device can sign with it. The certificate is
# not added to trusted roots: only the App Control policy trusts it.
#
#   pwsh scripts/appcontrol/new-cert.ps1 [-OutFile pool-signing.cer]
param(
    [string]$Subject = "CN=compute-pool code signing ($($env:COMPUTERNAME.ToLower()))",
    [string]$OutFile = (Join-Path $env:LOCALAPPDATA 'compute-pool\signing\pool-signing.cer')
)
$ErrorActionPreference = 'Stop'
$cert = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert | Where-Object Subject -eq $Subject |
    Sort-Object NotAfter -Descending | Select-Object -First 1
if (-not $cert) {
    # App Control signer rules need RSA (up to 4096 bits), not ECC.
    $cert = New-SelfSignedCertificate -Type CodeSigningCert -Subject $Subject -CertStoreLocation Cert:\CurrentUser\My `
        -KeyAlgorithm RSA -KeyLength 3072 -HashAlgorithm SHA256 -KeyExportPolicy NonExportable -NotAfter (Get-Date).AddYears(10)
}
New-Item -ItemType Directory -Force (Split-Path $OutFile) | Out-Null
Export-Certificate -Cert $cert -FilePath $OutFile | Out-Null
"Certificate: $($cert.Subject) ($($cert.Thumbprint)), valid until $($cert.NotAfter.ToString('yyyy-MM-dd'))"
"Public part: $OutFile"
