# Measures pool speedup: the same CPU-bound tasks on one device vs all devices.
#
#   go build -o bin/pool.exe ./cmd/pool; go build -o bin/poolbench.exe ./cmd/poolbench
#   pwsh scripts/bench.ps1 [-Tasks 48] [-Work 8000000] [-Solo desktop-jmc] [-Repeat 2]
param(
    [int]$Tasks = 48,
    [int]$Work = 8000000,
    [string]$Solo = "desktop-jmc",
    [int]$Repeat = 2
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$pool = Join-Path $root "bin\pool.exe"
$bench = Join-Path $root "bin\poolbench.exe"

function Measure-Map([string]$Label, [string[]]$Extra) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $out = & $pool map --range "1..$Tasks" @Extra --ship $bench -- poolbench.exe -n $Work '{}' 2>&1
    $secs = $sw.Elapsed.TotalSeconds
    $summary = ($out | Select-String '^pool: \d+ tasks').Line
    $perHost = $out | Select-String 'in ([\d.]+)s on (\S+)$' | ForEach-Object { $_.Matches[0] } |
        Group-Object { $_.Groups[2].Value } | ForEach-Object {
            $avg = ($_.Group | ForEach-Object { [double]$_.Groups[1].Value } | Measure-Object -Average).Average
            "{0}: {1} tasks, {2:N2}s compute each" -f $_.Name, $_.Count, $avg
        }
    [pscustomobject]@{ Run = $Label; Seconds = [math]::Round($secs, 1); Summary = $summary; PerHost = ($perHost -join "; ") }
}

$rows = for ($i = 1; $i -le $Repeat; $i++) {
    Measure-Map "$Solo only (#$i)" @("--nodes", $Solo)
    Measure-Map "all devices (#$i)" @()
}
$rows | Format-Table -AutoSize -Wrap | Out-String -Width 200

$solo = ($rows | Where-Object Run -like "$Solo only*" | Measure-Object Seconds -Average).Average
$all = ($rows | Where-Object Run -like "all devices*" | Measure-Object Seconds -Average).Average
"Speedup from pooling: {0:N2}x ({1:N1}s -> {2:N1}s)" -f ($solo / $all), $solo, $all
