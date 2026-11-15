$ErrorActionPreference = 'Stop'

$go = Get-Command go -ErrorAction SilentlyContinue
if ($null -eq $go) {
    throw 'Go 1.22+ not found on PATH.'
}
$goExe = $go.Source

function Invoke-Go {
    param([string[]]$Arguments)
    & $goExe @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "go $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
    }
}

$versionLine = (& $goExe version)
if ($versionLine -notmatch 'go1\.(2[2-9]|[3-9][0-9])') {
    throw "Go 1.22+ required, found: $versionLine"
}

Invoke-Go @('vet', './...')
$gofmt = Join-Path (Split-Path $goExe) 'gofmt.exe'
$unformatted = (& $gofmt -l .)
if ($LASTEXITCODE -ne 0) {
    throw "gofmt failed with exit code $LASTEXITCODE"
}
if ($unformatted) {
    throw "gofmt needed: $unformatted"
}

Invoke-Go @('test', './...')

$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$oldCGO = $env:CGO_ENABLED
try {
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    Invoke-Go @('build', '-trimpath', '-ldflags=-s -w -buildid=', '-o', 'countdown.exe', '.')
} finally {
    $env:GOOS = $oldGOOS
    $env:GOARCH = $oldGOARCH
    $env:CGO_ENABLED = $oldCGO
}

if (-not (Test-Path 'countdown.exe')) {
    throw 'countdown.exe was not produced.'
}

$manifestPath = Join-Path $PWD 'countdown.exe.manifest'
if (-not (Test-Path $manifestPath)) {
    throw 'countdown.exe.manifest is missing.'
}
$mt = Get-Command mt.exe -ErrorAction SilentlyContinue
if ($null -eq $mt) {
    $kits = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\bin'
    if (Test-Path $kits) {
        $mt = Get-ChildItem $kits -Recurse -Filter mt.exe -ErrorAction SilentlyContinue |
            Where-Object { $_.FullName -match '\\x64\\mt\.exe$' } |
            Sort-Object FullName -Descending |
            Select-Object -First 1
    }
}
if ($null -eq $mt) {
    throw 'mt.exe (Windows SDK) is required to embed countdown.exe.manifest.'
}
if ($mt -is [System.Management.Automation.CommandInfo]) {
    $mtPath = $mt.Source
} else {
    $mtPath = $mt.FullName
}
$exePath = (Resolve-Path 'countdown.exe').Path
& $mtPath -nologo -manifest $manifestPath "-outputresource:$exePath;#1"
if ($LASTEXITCODE -ne 0) {
    throw "mt.exe failed with exit code $LASTEXITCODE"
}
$bytes = [IO.File]::ReadAllBytes($exePath)
$ascii = [Text.Encoding]::UTF8.GetString($bytes)
$unicode = [Text.Encoding]::Unicode.GetString($bytes)
if (($ascii -notmatch 'Microsoft.Windows.Common-Controls') -and ($unicode -notmatch 'Microsoft.Windows.Common-Controls')) {
    throw 'embedded manifest does not contain Microsoft.Windows.Common-Controls'
}
Write-Host 'Manifest embedded.'

$oldExe = $env:COUNTDOWN_EXE
try {
    $env:COUNTDOWN_EXE = (Resolve-Path 'countdown.exe').Path
    Invoke-Go @('test', '-run', '^TestProtocolSmokeWithBuiltBinary$', '-v', '.')
} finally {
    $env:COUNTDOWN_EXE = $oldExe
}

Write-Host 'Build OK: countdown.exe'
