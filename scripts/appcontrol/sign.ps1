# Signs programs (such as pool.exe) with your code-signing certificate from
# new-cert.ps1, so an App Control policy built with build-policy.ps1 lets
# them run. Sign every build you install; an unsigned or rebuilt-but-unsigned
# pool.exe is blocked like any unknown program.
#
#   pwsh scripts/appcontrol/sign.ps1 bin\pool.exe[,more.exe]
param(
    [Parameter(Mandatory, Position = 0)][string[]]$Path,
    [string]$Subject = "CN=compute-pool code signing ($($env:COMPUTERNAME.ToLower()))"
)
$ErrorActionPreference = 'Stop'
$cert = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert | Where-Object Subject -eq $Subject |
    Sort-Object NotAfter -Descending | Select-Object -First 1
if (-not $cert) { throw "no code-signing certificate '$Subject' in your store; run new-cert.ps1 first" }
foreach ($p in $Path) {
    $r = Set-AuthenticodeSignature -FilePath $p -Certificate $cert -HashAlgorithm SHA256
    # "UnknownError" here only means Windows doesn't trust the root, which is
    # intended: the App Control policy trusts the certificate instead.
    if ($r.SignerCertificate.Thumbprint -ne $cert.Thumbprint) { throw "signing $p failed: $($r.StatusMessage)" }
    "signed $p"
}
