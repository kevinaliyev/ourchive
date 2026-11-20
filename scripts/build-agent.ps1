param(
    [string]$Version = "0.3.0-local",
    [string]$BaseURL = "https://github.com/OWNER/ourchive/releases/download/v0.3.0"
)

$ErrorActionPreference = "Stop"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    if (Test-Path "C:\Program Files\Go\bin\go.exe") {
        $env:PATH = "C:\Program Files\Go\bin;$env:PATH"
    }
}

$outDir = Join-Path $PSScriptRoot "..\dist"
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Ext = ".exe" },
    @{ GOOS = "darwin";  GOARCH = "arm64"; Ext = "" },
    @{ GOOS = "darwin";  GOARCH = "amd64"; Ext = "" },
    @{ GOOS = "linux";   GOARCH = "amd64"; Ext = "" },
    @{ GOOS = "linux";   GOARCH = "arm64"; Ext = "" }
)

$assets = [ordered]@{}
$ldflags = "-s -w -X github.com/ourchive/ourchive/internal/updater.Version=$Version"

foreach ($t in $targets) {
    $name = "ourchive-agent-$($t.GOOS)-$($t.GOARCH)$($t.Ext)"
    $dest = Join-Path $outDir $name
    Write-Host "Building $name..." -ForegroundColor Cyan
    $env:CGO_ENABLED = "0"
    $env:GOOS = $t.GOOS
    $env:GOARCH = $t.GOARCH
    & go build -ldflags $ldflags -o $dest ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw "Build failed for $name" }
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $stream = [System.IO.File]::OpenRead($dest)
    try {
        $hash = ([System.BitConverter]::ToString($sha.ComputeHash($stream)) -replace "-", "").ToLowerInvariant()
    } finally {
        $stream.Dispose()
        $sha.Dispose()
    }
    $assets["$($t.GOOS)/$($t.GOARCH)"] = [ordered]@{
        url    = "$BaseURL/$name"
        sha256 = $hash
    }
}

Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue

$manifest = [ordered]@{
    version      = $Version
    published_at = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    assets       = $assets
}

$manifestPath = Join-Path $outDir "manifest.json"
$manifest | ConvertTo-Json -Depth 5 | Set-Content -Path $manifestPath -Encoding UTF8
Write-Host "Done! Release binaries and manifest.json written to $outDir" -ForegroundColor Green
