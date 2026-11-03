# Signs programs (such as pool.exe) with your code-signing certificate from
# new-cert.ps1, so an App Control policy built with build-policy.ps1 lets
# them run. Sign every build you install; an unsigned or rebuilt-but-unsigned
# pool.exe is blocked like any unknown program.
#
# Each file is copied into a new folder in your own temp directory, and the
# copy is hashed, signed and checked there, then moved over the original.
# -ExpectedSha256 refuses a file whose bytes aren't the ones you meant (the
# hash from SHA256SUMS or your own build), and because the copy is what gets
# hashed and signed, the file can't be swapped in between. -TimestampServer
# (an RFC 3161 URL) keeps the signature valid after the certificate expires.
#
#   pwsh scripts/appcontrol/sign.ps1 bin\pool.exe -Thumbprint <from new-cert.ps1> [-ExpectedSha256 <hex>] [-TimestampServer http://timestamp.digicert.com]
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
    $found = @(Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert | Where-Object Subject -eq $Subject)
    if ($found.Count -gt 1) { throw "$($found.Count) certificates are named '$Subject'; pick one with -Thumbprint" }
    $found[0]
}
if (-not $cert) { throw "no code-signing certificate '$Subject' in your store; run new-cert.ps1 first" }

for ($i = 0; $i -lt $Path.Count; $i++) {
    $p = (Resolve-Path -LiteralPath $Path[$i]).Path
    $tmp = New-Item -ItemType Directory (Join-Path ([IO.Path]::GetTempPath()) "pool-sign-$([guid]::NewGuid().ToString('N'))")
    try {
        $copy = Join-Path $tmp (Split-Path -Leaf $p)
        Copy-Item -LiteralPath $p $copy
        if ($ExpectedSha256) {
            $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $copy).Hash
            if ($actual -ne $ExpectedSha256[$i]) { throw "$p has SHA-256 $actual, not $($ExpectedSha256[$i]); not signing it" }
        }
        $sig = @{ LiteralPath = $copy; Certificate = $cert; HashAlgorithm = 'SHA256' }
        if ($TimestampServer) { $sig.TimestampServer = $TimestampServer }
        $result = Set-AuthenticodeSignature @sig
        # Read the signature back rather than trusting the result of signing.
        # Its status is "UnknownError" because Windows doesn't trust the
        # certificate's root, which is intended: the App Control policy trusts
        # the certificate instead. Any other failure (the file unchanged, a
        # different signer, a hash mismatch) is an error.
        $check = Get-AuthenticodeSignature -LiteralPath $copy
        if ($check.SignerCertificate.Thumbprint -ne $cert.Thumbprint) {
            throw "signing $p failed ($($result.Status): $($result.StatusMessage)); its signer is $($check.SignerCertificate.Subject)"
        }
        if ($check.Status -notin 'Valid', 'UnknownError') { throw "signature on $p is $($check.Status): $($check.StatusMessage)" }
        if ($TimestampServer -and -not $check.TimeStamperCertificate) { throw "signature on $p has no timestamp" }
        # Replace the original instead of writing into it, so a hard link at
        # $p doesn't carry the change to another file.
        Move-Item -LiteralPath $copy -Destination $p -Force
        "signed $p"
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
