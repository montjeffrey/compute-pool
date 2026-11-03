# Signs programs (such as pool.exe) with your code-signing certificate from
# new-cert.ps1, so an App Control policy built with build-policy.ps1 lets
# them run. Sign every build you install; an unsigned or rebuilt-but-unsigned
# pool.exe is blocked like any unknown program.
#
# -ExpectedSha256 refuses to sign a file whose bytes aren't the ones you
# meant (for example the hash from SHA256SUMS or your own build), so a file
# swapped in between building and signing doesn't get your signature.
# -TimestampServer (an RFC 3161 URL) keeps the signature valid after the
# certificate expires.
#
#   pwsh scripts/appcontrol/sign.ps1 bin\pool.exe [-ExpectedSha256 <hex>] [-TimestampServer http://timestamp.digicert.com]
param(
    [Parameter(Mandatory, Position = 0)][string[]]$Path,
    [string]$Subject = "CN=compute-pool code signing ($($env:COMPUTERNAME.ToLower()))",
    [string]$Thumbprint,
    [string[]]$ExpectedSha256,
    [string]$TimestampServer
)
$ErrorActionPreference = 'Stop'
if ($ExpectedSha256 -and $ExpectedSha256.Count -ne $Path.Count) {
    throw "-ExpectedSha256 needs one hash per file ($($Path.Count))"
}
$cert = if ($Thumbprint) {
    Get-Item "Cert:\CurrentUser\My\$Thumbprint"
} else {
    Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert | Where-Object Subject -eq $Subject |
        Sort-Object NotAfter -Descending | Select-Object -First 1
}
if (-not $cert) { throw "no code-signing certificate '$Subject' in your store; run new-cert.ps1 first" }
for ($i = 0; $i -lt $Path.Count; $i++) {
    $p = (Resolve-Path $Path[$i]).Path
    if ($ExpectedSha256) {
        $actual = (Get-FileHash -Algorithm SHA256 $p).Hash
        if ($actual -ne $ExpectedSha256[$i]) { throw "$p has SHA-256 $actual, not $($ExpectedSha256[$i]); not signing it" }
    }
    $sig = @{ FilePath = $p; Certificate = $cert; HashAlgorithm = 'SHA256' }
    if ($TimestampServer) { $sig.TimestampServer = $TimestampServer }
    $result = Set-AuthenticodeSignature @sig
    # Read the signature back rather than trusting the result of signing.
    # Its status is "UnknownError" because Windows doesn't trust the
    # certificate's root, which is intended: the App Control policy trusts
    # the certificate instead. Any other failure (the file unchanged, a
    # different signer, a hash mismatch) is an error.
    $check = Get-AuthenticodeSignature -FilePath $p
    if ($check.SignerCertificate.Thumbprint -ne $cert.Thumbprint) {
        throw "signing $p failed ($($result.Status): $($result.StatusMessage)); its signer is $($check.SignerCertificate.Subject)"
    }
    if ($check.Status -notin 'Valid', 'UnknownError') { throw "signature on $p is $($check.Status): $($check.StatusMessage)" }
    if ($TimestampServer -and -not $check.TimeStamperCertificate) { throw "signature on $p has no timestamp" }
    "signed $p"
}
